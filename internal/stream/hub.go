// Package stream is the xcod engine: one provider connection per channel, shared with every
// viewer as plain MPEG-TS or HLS.
package stream

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"xcod/internal/config"
	"xcod/internal/stats"
)

type Hub struct {
	store *config.Store
	meter *stats.Meter
	conf  atomic.Pointer[config.Config] // copy of the configuration in use (cheap reads)

	mu       sync.Mutex // serializes viewer admission and channel changes
	channels map[string]*channel
	kicks    map[string]int64 // channel -> when the admin kicked it (passed on to repeaters)

	balancer atomic.Pointer[Balancer]
}

// Balancer splits the audience across the network's servers (internal/cluster).
type Balancer interface {
	// Pick returns the public address of the server that will serve a new viewer ("" = this one).
	Pick() string
}

// SetBalancer turns on splitting the audience across servers.
func (h *Hub) SetBalancer(b Balancer) { h.balancer.Store(&b) }

func (h *Hub) pick() string {
	if b := h.balancer.Load(); b != nil {
		return (*b).Pick()
	}
	return ""
}

func NewHub(store *config.Store, meter *stats.Meter) *Hub {
	h := &Hub{store: store, meter: meter, channels: map[string]*channel{}, kicks: map[string]int64{}}
	h.Apply(store.Get())
	store.OnChange(h.Apply)
	return h
}

// cfg returns the configuration in use. Do not modify the result.
func (h *Hub) cfg() *config.Config { return h.conf.Load() }

func (h *Hub) get(id string) *channel {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.channels[id]
}

// IDs lists the channels in order.
func (h *Hub) IDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.channels))
	for id := range h.channels {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Apply swaps the running configuration. Removed channels, or channels with a new URL, are
// dropped; the others keep running without interruption.
func (h *Hub) Apply(c config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conf.Store(&c) // before reconfigure: the new session already uses the new preset/ffmpeg
	for id, ch := range h.channels {
		cc, ok := c.Channels[id]
		old := ch.getConf()
		switch {
		case !ok || cc.URL != old.URL:
			delete(h.channels, id)
			go ch.shutdown()
			log.Printf("[%s] channel removed/changed", id)
		case cc != old:
			ch.reconfigure(cc)
		}
	}
	for id, cc := range c.Channels {
		if _, ok := h.channels[id]; !ok {
			h.channels[id] = newChannel(h, id, cc)
		}
	}
}

// viewer is whoever is asking to watch.
type viewer struct {
	ip, ua string
	key    string // access key used ("" = no key, "app" = signed app link)
	appExp int64  // expiry of the signed link (only when key == "app")
	appNet string // IP range of the app session ("" = legacy link, not bound to a network)
	rep    string // name of the repeater pulling the channel ("" = viewer)
}

func (v viewer) hlsID() string { return v.key + "|" + v.ip + "|" + v.ua }

var errBanned = errors.New("disconnected by the administrator")

// admit adds the viewer to the channel within the limits. For HLS pass c nil; for TS pass c.
// Active channels without viewers are closed to free a slot (channel switching); channels
// with viewers are never dropped.
func (h *Hub) admit(ch *channel, v viewer, c *client) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.channels[ch.id] != ch {
		return errors.New("channel unavailable")
	}
	if v.rep != "" { // repeater: outside the audience limits (its viewers are counted there)
		ch.addClient(c)
		return nil
	}
	if ch.isBanned(v.ip) {
		return errBanned
	}
	if c == nil && ch.touchHLS(v.hlsID()) {
		return nil // HLS viewer already counted
	}
	cfg := h.cfg()

	if v.key != "" {
		if k, ok := cfg.FindKey(v.key); ok && k.MaxScreens > 0 && h.screensLocked(v.key) >= k.MaxScreens {
			return fmt.Errorf("limit of %d screen(s) for this key reached", k.MaxScreens)
		}
	}

	if cfg.MaxClients > 0 {
		total := 0
		for _, other := range h.channels {
			_, n := other.state()
			total += n
		}
		if total >= cfg.MaxClients {
			return fmt.Errorf("server full (%d viewers)", total)
		}
	}

	if running, _ := ch.state(); !running && cfg.MaxUpstreams > 0 {
		var active, idle []*channel
		for _, other := range h.channels {
			if running, n := other.state(); running {
				active = append(active, other)
				if n == 0 {
					idle = append(idle, other)
				}
			}
		}
		need := len(active) - cfg.MaxUpstreams + 1
		if need > len(idle) {
			busy := "?"
			for _, a := range active {
				if _, n := a.state(); n > 0 {
					busy = a.id
					break
				}
			}
			return fmt.Errorf("provider connection limit reached: channel %s in use", busy)
		}
		for _, other := range idle[:max(need, 0)] {
			log.Printf("[%s] freeing the connection for channel %s", other.id, ch.id)
			other.forceStop(5 * time.Second)
		}
	}
	if c != nil {
		ch.addClient(c)
	} else {
		ch.addHLS(v)
	}
	return nil
}

// screensLocked counts the screens open with the key across all channels. Requires h.mu.
func (h *Hub) screensLocked(key string) int {
	n := 0
	for _, ch := range h.channels {
		n += ch.screens(key)
	}
	return n
}

// Screens counts the open screens per access key.
func (h *Hub) Screens() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int{}
	for _, ch := range h.channels {
		for k, n := range ch.screensByKey() {
			out[k] += n
		}
	}
	return out
}

// Kick drops the channel's viewers and bans them for one minute.
func (h *Hub) Kick(id string) bool {
	ch := h.get(id)
	if ch == nil {
		return false
	}
	h.mu.Lock()
	h.kicks[id] = time.Now().UnixNano()
	h.mu.Unlock()
	ch.kick(true)
	if !ch.hasRepeaters() { // with repeaters pulling, the channel stays up for them
		ch.forceStop(5 * time.Second) // frees the provider connection right away
	}
	return true
}

// Kicks reports when each channel was kicked (repeaters kick their viewers too).
func (h *Hub) Kicks() map[string]int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int64, len(h.kicks))
	for k, v := range h.kicks {
		out[k] = v
	}
	return out
}

// Viewers counts each channel's audience (without repeaters).
func (h *Hub) Viewers() map[string]int {
	out := map[string]int{}
	for _, s := range h.Status(false) {
		if n := s.TS + s.HLS; n > 0 {
			out[s.ID] = n
		}
	}
	return out
}

// BytesOut adds up everything sent to viewers and repeaters.
func (h *Hub) BytesOut() int64 {
	var n int64
	for _, s := range h.Status(false) {
		n += s.BytesOut
	}
	return n
}

// DropKey disconnects whoever watches with the key (disabled or deleted) and, with key "",
// whoever watches without a key (when keys become required).
func (h *Hub) DropKey(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.channels {
		ch.dropKey(key)
	}
}

// ServePreview serves the panel preview: HLS without requiring an access key (the panel is
// already authenticated).
func (h *Hub) ServePreview(w http.ResponseWriter, r *http.Request, id string) {
	streamHeaders(w)
	ch := h.get(id)
	if ch == nil {
		http.NotFound(w, r)
		return
	}
	h.serveHLS(w, r, ch, viewer{ip: ClientIP(r, h.cfg()), ua: r.UserAgent(), key: "panel"})
}

// ---------- state for the panel and the statistics ----------

type Viewer struct {
	Kind string `json:"kind"` // "TS", "HLS" or "REP"
	IP   string `json:"ip"`
	Key  string `json:"key,omitempty"` // access key name
}

type ChannelStatus struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Quality  string   `json:"quality"`
	Running  bool     `json:"running"`
	TS       int      `json:"ts"`
	HLS      int      `json:"hls"`
	Reps     int      `json:"repeaters"` // repeaters pulling the channel
	BytesSrc int64    `json:"bytes_src"` // received from the provider
	BytesIn  int64    `json:"bytes_in"`  // delivered signal (after transcoding)
	BytesOut int64    `json:"bytes_out"` // sent to viewers (actual upload)
	Uptime   string   `json:"uptime,omitempty"`
	Segments int      `json:"segments"`
	Viewers  []Viewer `json:"viewers,omitempty"`
}

// Status describes the channels in ID order; detail includes the viewers.
func (h *Hub) Status(detail bool) []ChannelStatus {
	cfg := h.cfg()
	names := map[string]string{}
	for _, k := range cfg.AccessKeys {
		names[k.Key] = k.Name
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ChannelStatus, 0, len(h.channels))
	for id, ch := range h.channels {
		ch.mu.Lock()
		s := ChannelStatus{ID: id, Name: ch.conf.Name, Quality: ch.conf.Quality, Running: ch.running,
			HLS: ch.pruneHLSLocked(), BytesSrc: ch.cnt.Src.Load(), BytesIn: ch.bytesIn, BytesOut: ch.cnt.Out.Load()}
		for c := range ch.clients {
			if c.rep != "" {
				s.Reps++
			} else {
				s.TS++
			}
		}
		if s.Name == "" {
			s.Name = id
		}
		if ch.running {
			s.Uptime = time.Since(ch.started).Truncate(time.Second).String()
		}
		if ch.seg != nil {
			s.Segments = len(ch.seg.segs)
		}
		if detail {
			for c := range ch.clients {
				if c.rep != "" {
					s.Viewers = append(s.Viewers, Viewer{Kind: "REP", IP: c.ip, Key: "repeater " + c.rep})
				} else {
					s.Viewers = append(s.Viewers, Viewer{Kind: "TS", IP: c.ip, Key: names[c.key]})
				}
			}
			for _, v := range ch.hlsViewers {
				s.Viewers = append(s.Viewers, Viewer{Kind: "HLS", IP: v.ip, Key: names[v.key]})
			}
			slices.SortFunc(s.Viewers, func(a, b Viewer) int {
				if a.IP != b.IP {
					if a.IP < b.IP {
						return -1
					}
					return 1
				}
				return 0
			})
		}
		ch.mu.Unlock()
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b ChannelStatus) int {
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return out
}

// Load feeds the statistics.
func (h *Hub) Load() map[string]stats.ChannelLoad {
	out := map[string]stats.ChannelLoad{}
	for _, s := range h.Status(false) {
		out[s.ID] = stats.ChannelLoad{Viewers: s.TS + s.HLS, Running: s.Running}
	}
	return out
}
