package admin

import (
	"net"
	"net/http"
	"strings"
	"time"

	"xcod/internal/auth"
	"xcod/internal/config"
	"xcod/internal/i18n"
)

type securityView struct {
	User      string
	Changed   time.Time
	Sessions  []auth.Session
	CurrentID string
	Allow     string // one IP/network per line
	MyIP      string
	MinLen    int
	PanelURL  string // the panel's full address (with the secret path)
	Secret    bool   // the panel has a secret path
}

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	v := securityView{User: s.creds.Username(), Changed: s.creds.Changed(), Sessions: s.sess.List(),
		CurrentID: session(r).ID, Allow: strings.Join(cfg.AdminAllow, "\n"), MyIP: s.clientIP(r), MinLen: auth.MinPassLen,
		PanelURL: s.baseURL(r) + s.to("/admin"), Secret: cfg.PanelToken != ""}
	s.render(w, r, http.StatusOK, "security", "Security", v)
}

func (s *Server) securityPassword(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSpace(r.PostFormValue("user"))
	current, next, confirm := r.PostFormValue("current"), r.PostFormValue("new"), r.PostFormValue("confirm")
	switch {
	case !s.creds.Verify(s.creds.Username(), current):
		s.lim.Fail(s.clientIP(r)) // counts as a password attempt
		s.redirect(w, r, "/admin/security", "err", "Wrong current password.")
		return
	case next != confirm:
		s.redirect(w, r, "/admin/security", "err", "The confirmation does not match the new password.")
		return
	}
	if user == "" {
		user = s.creds.Username()
	}
	if err := s.creds.Set(user, next); err != nil {
		s.fail(w, r, "/admin/security", err)
		return
	}
	sess := session(r)
	s.sess.Rename(sess, user)
	n := s.sess.DestroyOthers(sess)
	s.audit(r, "changed the panel password (%d other session(s) ended)", n)
	s.redirect(w, r, "/admin/security", "ok", "Password changed. Other devices need to sign in again.")
}

func (s *Server) securityAllow(w http.ResponseWriter, r *http.Request) {
	var list []string
	for _, line := range strings.Split(r.PostFormValue("allow"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			list = append(list, line)
		}
	}
	me := net.ParseIP(s.clientIP(r))
	err := s.store.Update(func(c *config.Config) error {
		c.AdminAllow = list
		if err := c.Validate(); err != nil {
			return err
		}
		if !c.AdminAllowed(me) { // do not let the admin lock themselves out
			return i18n.Errorf("the list does not include your current IP (%s): you would lose access", me)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, "/admin/security", err)
		return
	}
	s.audit(r, "changed the panel's allowed IPs: %v", list)
	if len(list) == 0 {
		s.redirect(w, r, "/admin/security", "ok", "Panel open to any IP.")
		return
	}
	s.redirect(w, r, "/admin/security", "ok", "Panel restricted to %d IP(s)/network(s).", len(list))
}

// securityAddress gives the panel a new secret address. The old one stops working at once and
// every session ends (the session cookie belongs to the old address).
func (s *Server) securityAddress(w http.ResponseWriter, r *http.Request) {
	token := auth.RandomKey()
	if err := s.store.Update(func(c *config.Config) error { c.PanelToken = token; return nil }); err != nil {
		s.fail(w, r, "/admin/security", err)
		return
	}
	s.audit(r, "changed the panel address")
	s.sess.DestroyOthers(nil)
	http.Redirect(w, r, s.to("/admin/login"), http.StatusSeeOther)
}

func (s *Server) securityLogoutOthers(w http.ResponseWriter, r *http.Request) {
	n := s.sess.DestroyOthers(session(r))
	s.audit(r, "ended %d other session(s)", n)
	s.redirect(w, r, "/admin/security", "ok", "%d session(s) ended.", n)
}

func (s *Server) securityLogoutOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == session(r).ID {
		s.redirect(w, r, "/admin/security", "err", "To end this session, use the Sign out button.")
		return
	}
	if !s.sess.DestroyID(id) {
		s.redirect(w, r, "/admin/security", "err", "Session not found.")
		return
	}
	s.audit(r, "ended session %s", id)
	s.redirect(w, r, "/admin/security", "ok", "Session ended.")
}
