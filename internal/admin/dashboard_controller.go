package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"xcod/internal/stats"
	"xcod/internal/stream"
)

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "dashboard", "Dashboard", nil)
}

type liveView struct {
	Channels   []stream.ChannelStatus `json:"channels"`
	Usage      stats.Usage            `json:"usage"`
	RequireKey bool                   `json:"require_key"`
	Base       string                 `json:"base"` // address for .ts links
	HLSBase    string                 `json:"hls_base"`
}

// liveJSON feeds the live dashboard (refreshed every 2s by the browser).
func (s *Server) liveJSON(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	v := liveView{Channels: s.hub.Status(true), Usage: s.meter.Usage(), RequireKey: cfg.RequireKey,
		Base: s.baseURL(r), HLSBase: s.baseURL(r)}
	if cfg.CDNURL != "" {
		v.HLSBase = cfg.CDNURL
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) kick(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !s.hub.Kick(body.Channel) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	s.audit(r, "kicked the viewers of channel %s", body.Channel)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// preview plays the channel inside the panel, even when an access key is required.
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".m3u8")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.hub.ServePreview(w, r, id)
}
