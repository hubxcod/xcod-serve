package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"xcod/internal/config"
	"xcod/internal/i18n"
)

type channelRow struct {
	ID, Name, Host, Quality string
	Running                 bool
	Viewers                 int
}

type channelForm struct {
	ID, Name, URL, Quality string
	FPS, VideoKbps         int
	Editing                bool
	Error                  string
	Qualities              []config.Quality
	FPSOptions             []int
}

func (s *Server) channelList(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	live := map[string][2]int{} // id -> running, viewers
	for _, st := range s.hub.Status(false) {
		running := 0
		if st.Running {
			running = 1
		}
		live[st.ID] = [2]int{running, st.TS + st.HLS}
	}
	var rows []channelRow
	for _, id := range sortedKeys(cfg.Channels) {
		ch := cfg.Channels[id]
		q, _ := config.FindQuality(ch.Quality)
		host := ch.URL
		if u, err := url.Parse(ch.URL); err == nil {
			host = u.Host // the full URL often carries the provider's user/password
		}
		rows = append(rows, channelRow{ID: id, Name: ch.Name, Host: host, Quality: q.Label,
			Running: live[id][0] == 1, Viewers: live[id][1]})
	}
	s.render(w, r, http.StatusOK, "channels", "Channels", rows)
}

func (s *Server) newForm(f channelForm) channelForm {
	f.Qualities = config.Qualities
	f.FPSOptions = []int{0, 30, 25, 24, 15}
	if f.Quality == "" {
		f.Quality = "original"
	}
	return f
}

func (s *Server) channelNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "channel_form", "New channel", s.newForm(channelForm{}))
}

func (s *Server) channelEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, ok := s.store.Get().Channels[id]
	if !ok {
		s.redirect(w, r, "/admin/channels", "err", "Channel not found.")
		return
	}
	f := channelForm{ID: id, Name: ch.Name, URL: ch.URL, Quality: ch.Quality, FPS: ch.FPS,
		VideoKbps: ch.VideoKbps, Editing: true}
	s.render(w, r, http.StatusOK, "channel_form", s.tr(r, "Edit channel %s", id), s.newForm(f))
}

// readChannel reads the channel form.
func readChannel(r *http.Request) (channelForm, config.Channel, error) {
	f := channelForm{
		ID: strings.TrimSpace(r.PostFormValue("id")), Name: strings.TrimSpace(r.PostFormValue("name")),
		URL: strings.TrimSpace(r.PostFormValue("url")), Quality: r.PostFormValue("quality"),
	}
	var err1, err2 error
	f.FPS, err1 = intField(r, "fps")
	f.VideoKbps, err2 = intField(r, "video_kbps")
	ch := config.Channel{Name: f.Name, URL: f.URL, Quality: f.Quality, FPS: f.FPS, VideoKbps: f.VideoKbps}
	if err := errors.Join(err1, err2); err != nil {
		return f, ch, err
	}
	return f, ch, nil
}

func (s *Server) channelCreate(w http.ResponseWriter, r *http.Request) {
	f, ch, err := readChannel(r)
	if err == nil {
		err = s.store.Update(func(c *config.Config) error {
			if !config.ValidID(f.ID) {
				return i18n.Errorf("invalid ID: use letters, digits, - and _ (it becomes the channel link)")
			}
			if _, exists := c.Channels[f.ID]; exists {
				return i18n.Errorf("a channel with ID %s already exists", f.ID)
			}
			c.Channels[f.ID] = ch
			return nil
		})
	}
	if err != nil {
		f.Error = i18n.Text(s.lang(r), err)
		s.render(w, r, http.StatusBadRequest, "channel_form", "New channel", s.newForm(f))
		return
	}
	s.audit(r, "created channel %s", f.ID)
	s.redirect(w, r, "/admin/channels", "ok", "Channel %s created.", f.ID)
}

func (s *Server) channelUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, ch, err := readChannel(r)
	f.ID, f.Editing = id, true
	if err == nil {
		err = s.store.Update(func(c *config.Config) error {
			if _, ok := c.Channels[id]; !ok {
				return i18n.Errorf("channel not found")
			}
			c.Channels[id] = ch
			return nil
		})
	}
	if err != nil {
		f.Error = i18n.Text(s.lang(r), err)
		s.render(w, r, http.StatusBadRequest, "channel_form", s.tr(r, "Edit channel %s", id), s.newForm(f))
		return
	}
	s.audit(r, "changed channel %s", id)
	s.redirect(w, r, "/admin/channels", "ok", "Channel %s saved.", id)
}

func (s *Server) channelDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Update(func(c *config.Config) error {
		if _, ok := c.Channels[id]; !ok {
			return i18n.Errorf("channel not found")
		}
		delete(c.Channels, id)
		return nil
	})
	if err != nil {
		s.fail(w, r, "/admin/channels", err)
		return
	}
	s.audit(r, "deleted channel %s", id)
	s.redirect(w, r, "/admin/channels", "ok", "Channel %s deleted.", id)
}

// intField reads a number from the form; empty means 0.
func intField(r *http.Request, name string) (int, error) {
	v := strings.TrimSpace(r.PostFormValue(name))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, i18n.Errorf("invalid value in %s: %q", name, v)
	}
	return n, nil
}
