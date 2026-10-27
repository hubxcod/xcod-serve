package admin

import (
	"errors"
	"net/http"
	"strings"

	"xcod/internal/config"
	"xcod/internal/i18n"
	"xcod/internal/stream"
)

type settingsView struct {
	C       config.Config
	Presets []string
	FFmpeg  string // executable found ("" = not installed)
	Error   string
}

func (s *Server) settingsView(c config.Config, errMsg string) settingsView {
	return settingsView{C: c, Presets: config.EncoderPresets, FFmpeg: stream.FFmpegBinary(c.FFmpegPath), Error: errMsg}
}

func (s *Server) settingsForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "settings", "Settings", s.settingsView(s.store.Get(), ""))
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	var form config.Config
	err := s.store.Update(func(c *config.Config) error {
		var errs []error
		num := func(dst *int, name string) {
			n, err := intField(r, name)
			errs = append(errs, err)
			*dst = n
		}
		num(&c.MaxClients, "max_clients")
		num(&c.MaxUpstreams, "max_upstreams")
		num(&c.HLSSegment, "hls_segment_seconds")
		num(&c.HLSWindow, "hls_window")
		num(&c.BacklogKB, "backlog_kb")
		text := func(name string) string { return strings.TrimSpace(r.PostFormValue(name)) }
		c.UserAgent = text("user_agent")
		c.IdleTimeout = text("idle_timeout")
		c.StallTimeout = text("stall_timeout")
		c.EncoderPreset = text("encoder_preset")
		c.FFmpegPath = text("ffmpeg_path")
		c.CDNURL = text("cdn_url")
		c.CDNSecret = text("cdn_secret")
		c.TrustProxy = r.PostFormValue("trust_proxy") == "on"
		form = *c
		return errors.Join(errs...)
	})
	if err != nil {
		s.render(w, r, http.StatusBadRequest, "settings", "Settings", s.settingsView(form, i18n.Text(s.lang(r), err)))
		return
	}
	s.audit(r, "saved the settings")
	s.redirect(w, r, "/admin/settings", "ok", "Settings saved and applied.")
}
