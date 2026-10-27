package admin

import (
	"log"
	"math"
	"net/http"
)

type loginView struct {
	User  string
	Error string
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if s.sess.Get(r) != nil {
		http.Redirect(w, r, s.to("/admin"), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Sign in", loginView{})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	user := r.PostFormValue("user")
	if blocked, left := s.lim.Blocked(ip); blocked {
		msg := s.tr(r, "Too many attempts. Try again in %d min.", int(math.Ceil(left.Minutes())))
		s.render(w, r, http.StatusTooManyRequests, "login", "Sign in", loginView{User: user, Error: msg})
		return
	}
	if !s.creds.Verify(user, r.PostFormValue("password")) {
		if s.lim.Fail(ip) {
			log.Printf("admin: IP %s locked out for 15 min after several wrong passwords", ip)
		} else {
			log.Printf("admin: invalid login from %s (user %q)", ip, user)
		}
		s.render(w, r, http.StatusUnauthorized, "login", "Sign in", loginView{User: user, Error: s.tr(r, "Wrong username or password.")})
		return
	}
	s.lim.Success(ip)
	s.sess.Create(w, user, ip, r.UserAgent(), s.https(r), s.cookiePath())
	log.Printf("admin: %s signed in from %s", user, ip)
	http.Redirect(w, r, s.to("/admin"), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.audit(r, "signed out")
	s.sess.Destroy(w, r, s.cookiePath())
	http.Redirect(w, r, s.to("/admin/login"), http.StatusSeeOther)
}
