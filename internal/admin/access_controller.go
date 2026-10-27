package admin

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"xcod/internal/auth"
	"xcod/internal/config"
	"xcod/internal/i18n"
)

type accessRow struct {
	config.AccessKey
	InUse    int
	Valid    bool
	Playlist string // HLS list with the key
	TSList   string // TS list with the key
}

type accessView struct {
	RequireKey bool
	AppOnly    bool
	AppSecret  string
	Keys       []accessRow
	Today      string
}

func (s *Server) accessList(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	screens := s.hub.Screens()
	base := s.baseURL(r)
	v := accessView{RequireKey: cfg.RequireKey, AppOnly: cfg.AppOnly, AppSecret: cfg.AppSecret, Today: time.Now().Format("2006-01-02")}
	for _, k := range cfg.AccessKeys {
		v.Keys = append(v.Keys, accessRow{AccessKey: k, InUse: screens[k.Key], Valid: k.Valid(time.Now()),
			Playlist: base + "/playlist/" + k.Key + ".m3u8", TSList: base + "/playlist/" + k.Key + ".m3u"})
	}
	s.render(w, r, http.StatusOK, "access", "Access", v)
}

func (s *Server) accessCreate(w http.ResponseWriter, r *http.Request) {
	screens, err := intField(r, "max_screens")
	k := config.AccessKey{Name: strings.TrimSpace(r.PostFormValue("name")), Key: auth.RandomKey(),
		Enabled: true, Expires: strings.TrimSpace(r.PostFormValue("expires")), MaxScreens: screens}
	if err == nil {
		err = s.store.Update(func(c *config.Config) error {
			c.AccessKeys = append(c.AccessKeys, k)
			return nil
		})
	}
	if err != nil {
		s.fail(w, r, "/admin/access", err)
		return
	}
	s.audit(r, "created access key %q", k.Name)
	s.redirect(w, r, "/admin/access", "ok", "Key for %s created.", k.Name)
}

// editKey applies fn to the key sent in the form.
func (s *Server) editKey(r *http.Request, fn func(c *config.Config, i int)) (config.AccessKey, error) {
	key := r.PostFormValue("key")
	var found config.AccessKey
	err := s.store.Update(func(c *config.Config) error {
		i := slices.IndexFunc(c.AccessKeys, func(k config.AccessKey) bool { return k.Key == key })
		if i < 0 {
			return i18n.Errorf("key not found")
		}
		found = c.AccessKeys[i]
		fn(c, i)
		return nil
	})
	return found, err
}

func (s *Server) accessToggle(w http.ResponseWriter, r *http.Request) {
	k, err := s.editKey(r, func(c *config.Config, i int) { c.AccessKeys[i].Enabled = !c.AccessKeys[i].Enabled })
	if err != nil {
		s.fail(w, r, "/admin/access", err)
		return
	}
	state := "disabled"
	if !k.Enabled {
		state = "enabled"
	} else {
		s.hub.DropKey(k.Key)
	}
	s.audit(r, "%s key %q", state, k.Name)
	s.redirect(w, r, "/admin/access", "ok", "Key for %s updated.", k.Name)
}

func (s *Server) accessDelete(w http.ResponseWriter, r *http.Request) {
	k, err := s.editKey(r, func(c *config.Config, i int) { c.AccessKeys = slices.Delete(c.AccessKeys, i, i+1) })
	if err != nil {
		s.fail(w, r, "/admin/access", err)
		return
	}
	s.hub.DropKey(k.Key)
	s.audit(r, "deleted key %q", k.Name)
	s.redirect(w, r, "/admin/access", "ok", "Key for %s deleted.", k.Name)
}

// accessApp saves the app secret and the app-only mode.
func (s *Server) accessApp(w http.ResponseWriter, r *http.Request) {
	on := r.PostFormValue("app_only") == "on"
	var keys []string
	err := s.store.Update(func(c *config.Config) error {
		c.AppOnly = on
		c.AppSecret = strings.TrimSpace(r.PostFormValue("app_secret"))
		for _, k := range c.AccessKeys {
			keys = append(keys, k.Key)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, "/admin/access", err)
		return
	}
	msg := "App access saved. Keys and keyless links keep working."
	if on {
		// Whoever watched with a key or without one is dropped; only signed app links remain.
		s.hub.DropKey("")
		for _, k := range keys {
			s.hub.DropKey(k)
		}
		msg = "Channels now open only through signed app links."
	}
	s.audit(r, "app access: app-only = %v", on) // the secret never goes to the log
	s.redirect(w, r, "/admin/access", "ok", msg)
}

func (s *Server) accessRequire(w http.ResponseWriter, r *http.Request) {
	on := r.PostFormValue("require_key") == "on"
	err := s.store.Update(func(c *config.Config) error {
		c.RequireKey = on
		return nil
	})
	if err != nil {
		s.fail(w, r, "/admin/access", err)
		return
	}
	msg := "Keyless links allowed."
	if on {
		s.hub.DropKey("") // whoever watched without a key is dropped
		msg = "Channels now open only with an access key."
	}
	s.audit(r, "require key = %v", on)
	s.redirect(w, r, "/admin/access", "ok", msg)
}
