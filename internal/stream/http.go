package stream

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Routes registers the public endpoints (viewers and health check).
//
//	/live/{id}.ts | .m3u8             without a key (when keys are not required)
//	/live/{id}.m3u8?st=|se=|exp=&sig= app links: start, session and legacy (see applink.go)
//	/live/{key}/{id}.ts | .m3u8       with an access key
//	/hls/{id}/{seq}.ts                HLS segments (CDN-cacheable)
//	/playlist.m3u | .m3u8             channel list without a key
//	/playlist/{key}.m3u | .m3u8       channel list with a key
//	/healthz                          process health
func (h *Hub) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /live/{file}", func(w http.ResponseWriter, r *http.Request) {
		h.serveLive(w, r, "", r.PathValue("file"))
	})
	mux.HandleFunc("GET /live/{key}/{file}", func(w http.ResponseWriter, r *http.Request) {
		h.serveLive(w, r, r.PathValue("key"), r.PathValue("file"))
	})
	mux.HandleFunc("GET /hls/{id}/{seg}", h.serveSegment)
	mux.HandleFunc("GET /playlist.m3u", func(w http.ResponseWriter, r *http.Request) { h.servePlaylist(w, r, "", ".ts") })
	mux.HandleFunc("GET /playlist.m3u8", func(w http.ResponseWriter, r *http.Request) { h.servePlaylist(w, r, "", ".m3u8") })
	mux.HandleFunc("GET /playlist/{file}", func(w http.ResponseWriter, r *http.Request) {
		file := r.PathValue("file")
		switch {
		case strings.HasSuffix(file, ".m3u8"):
			h.servePlaylist(w, r, strings.TrimSuffix(file, ".m3u8"), ".m3u8")
		case strings.HasSuffix(file, ".m3u"):
			h.servePlaylist(w, r, strings.TrimSuffix(file, ".m3u"), ".ts")
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintln(w, "ok")
	})
}

// streamHeaders: by default nothing is cacheable (playlists and errors); only segments allow
// caching. CORS is open for web players.
func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache, no-store, private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// checkKey validates the access key. Without a key, it only passes if keys are not required.
func (h *Hub) checkKey(w http.ResponseWriter, r *http.Request, key string) bool {
	cfg := h.cfg()
	if key == "" {
		if cfg.RequireKey {
			http.Error(w, "access key required", http.StatusForbidden)
			return false
		}
		return true
	}
	if k, ok := cfg.FindKey(key); ok && k.Valid(time.Now()) {
		return true
	}
	log.Printf("invalid or expired access key from %s", ClientIP(r, cfg))
	http.Error(w, "invalid or expired access key", http.StatusForbidden)
	return false
}

func (h *Hub) serveLive(w http.ResponseWriter, r *http.Request, key, file string) {
	streamHeaders(w)
	cfg := h.cfg()
	ext := ".m3u8"
	if strings.HasSuffix(file, ".ts") {
		ext = ".ts"
	}
	id := strings.TrimSuffix(file, ext)
	var v viewer
	switch {
	case r.Header.Get(RepeaterHeader) != "":
		rep, ok := repeaterFrom(r, cfg)
		if !ok || ext != ".ts" {
			log.Printf("repeater rejected from %s", ClientIP(r, cfg))
			http.Error(w, "repeater not authorized", http.StatusForbidden)
			return
		}
		v = viewer{ip: ClientIP(r, cfg), ua: r.UserAgent(), key: "rep:" + rep.ID, rep: rep.Name}
	case key == "" && isAppRequest(r.URL.Query()):
		var handled bool
		if v, handled = h.appLink(w, r, id, ext); handled {
			return
		}
	case cfg.AppOnly:
		http.Error(w, "this channel is app-only", http.StatusForbidden)
		return
	case !h.checkKey(w, r, key):
		return
	default:
		v = viewer{ip: ClientIP(r, cfg), ua: r.UserAgent(), key: key}
	}
	switch {
	case strings.HasSuffix(file, ".m3u8"):
		if ch := h.get(id); ch != nil {
			h.serveHLS(w, r, ch, v)
			return
		}
	case strings.HasSuffix(file, ".ts"):
		if ch := h.get(strings.TrimSuffix(file, ".ts")); ch != nil {
			h.serveTS(w, r, ch, v)
			return
		}
	}
	http.NotFound(w, r)
}

func (h *Hub) serveTS(w http.ResponseWriter, r *http.Request, ch *channel, v viewer) {
	c := &client{ch: make(chan []byte, 512), closed: make(chan struct{}), addr: r.RemoteAddr, ip: v.ip, key: v.key, rep: v.rep}
	if err := h.admit(ch, v, c); err != nil {
		log.Printf("[%s] ts rejected %s: %v", ch.id, v.ip, err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer ch.removeClient(c)

	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	rc := http.NewResponseController(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-c.closed:
			return
		case data := <-c.ch:
			rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
			n, err := w.Write(data)
			ch.cnt.Out.Add(int64(n))
			if err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (h *Hub) serveHLS(w http.ResponseWriter, r *http.Request, ch *channel, v viewer) {
	if err := h.admit(ch, v, nil); err != nil {
		log.Printf("[%s] hls rejected %s: %v", ch.id, v.ip, err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	pl, err := ch.waitPlaylist(30 * time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// the playlist is never cached: every viewer comes through here (that is why counting,
	// limits, keys and kicks work with a CDN); the segments, which are almost all of the
	// bandwidth, come from the CDN cache.
	if v.key == appKey {
		pl = signSegments(pl, h.cfg().AppSecret, ch.id, v.appExp, v.appNet)
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	n, _ := fmt.Fprint(w, pl)
	ch.cnt.Out.Add(int64(n))
}

func (h *Hub) serveSegment(w http.ResponseWriter, r *http.Request) {
	streamHeaders(w)
	ch := h.get(r.PathValue("id"))
	seq, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("seg"), ".ts"))
	if ch == nil || err != nil {
		http.NotFound(w, r)
		return
	}
	cfg := h.cfg()
	if ch.isBanned(ClientIP(r, cfg)) {
		http.Error(w, errBanned.Error(), http.StatusForbidden)
		return
	}
	// App-only: the segment needs the signature that came with the playlist. The CDN passes
	// straight through (it fetches with X-CDN-Secret and serves from cache; the playlist stays
	// protected).
	if cfg.AppOnly && !FromCDN(r, cfg) {
		if !validSegment(r.URL.Query(), cfg.AppSecret, ch.id, ClientIP(r, cfg), time.Now()) {
			http.Error(w, "segment without a valid signature", http.StatusForbidden)
			return
		}
	}
	data := ch.segment(seq)
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "public, max-age=300, immutable") // the number never repeats
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(60 * time.Second))
	n, _ := w.Write(data)
	ch.cnt.Out.Add(int64(n))
}

// servePlaylist lists the channels. HLS points to the CDN when one is configured; TS is a
// continuous stream that a CDN does not cache, so it stays direct.
func (h *Hub) servePlaylist(w http.ResponseWriter, r *http.Request, key, ext string) {
	streamHeaders(w)
	cfg := h.cfg()
	if cfg.AppOnly {
		http.Error(w, "channels are app-only", http.StatusForbidden)
		return
	}
	if !h.checkKey(w, r, key) {
		return
	}
	base := "http://" + r.Host
	if IsHTTPS(r, cfg) {
		base = "https://" + r.Host
	}
	if cfg.CDNURL != "" && ext == ".m3u8" {
		base = cfg.CDNURL
	}
	prefix := base + "/live/"
	if key != "" {
		prefix += key + "/"
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl")
	fmt.Fprintln(w, "#EXTM3U")
	for _, id := range h.IDs() {
		name := id
		if c, ok := cfg.Channels[id]; ok && c.Name != "" {
			name = c.Name
		}
		fmt.Fprintf(w, "#EXTINF:-1 tvg-id=%q,%s\n%s%s%s\n", id, name, prefix, id, ext)
	}
}
