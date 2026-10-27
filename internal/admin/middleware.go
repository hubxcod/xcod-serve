package admin

import (
	"context"
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"net/url"

	"xcod/internal/auth"
	"xcod/internal/stream"
	"xcod/internal/update"
)

type ctxKey struct{}

// session returns the session placed in the context by requireAuth.
func session(r *http.Request) *auth.Session {
	s, _ := r.Context().Value(ctxKey{}).(*auth.Session)
	return s
}

// clientIP is the real IP of whoever opens the panel.
func (s *Server) clientIP(r *http.Request) string {
	cfg := s.store.Get()
	return stream.ClientIP(r, &cfg)
}

func (s *Server) https(r *http.Request) bool {
	cfg := s.store.Get()
	return stream.IsHTTPS(r, &cfg)
}

// guard applies to the whole panel: security headers, never through the CDN and only for the
// allowed IPs (when configured). Whoever cannot see the panel gets a 404, as if it did not
// exist.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://cdn.jsdelivr.net; "+
			"style-src 'self'; img-src 'self' data:; media-src 'self' blob:; connect-src 'self'; "+
			"worker-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: with no-referrer the browser sends "Origin: null" on
		// every POST and the origin check would reject the panel itself
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if s.https(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}

		cfg := s.store.Get()
		if stream.FromCDN(r, &cfg) || !cfg.AdminAllowed(net.ParseIP(s.clientIP(r))) {
			http.NotFound(w, r)
			return
		}
		// once the certificate is ready, the panel opened through the domain is HTTPS only
		// (password and session encrypted)
		if r.TLS == nil && s.net.Certs != nil && s.net.Certs.Ready() && hostOnly(r.Host) == cfg.Domain && !s.https(r) {
			http.Redirect(w, r, "https://"+cfg.Domain+r.URL.RequestURI(), http.StatusMovedPermanently)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			log.Printf("admin: cross-origin request refused (%s, Origin %q)", s.clientIP(r), r.Header.Get("Origin"))
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		limit := int64(1 << 20)
		if r.URL.Path == "/admin/server/upload" { // new binary upload
			limit = update.MaxSize + 1<<20
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// sameOrigin rejects forms submitted by other sites (CSRF), including on login, where there
// is no token yet.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		ref := r.Header.Get("Referer")
		if ref == "" {
			return true // clients without Origin/Referer (e.g. curl); the CSRF token still applies
		}
		origin = ref
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// requireAuth requires a session; on state-changing methods it requires the CSRF token (form
// field "csrf" or the X-CSRF-Token header).
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := s.sess.Get(r)
		if sess == nil {
			if r.Method == http.MethodGet && !isAPI(r) {
				http.Redirect(w, r, s.to("/admin/login"), http.StatusSeeOther)
				return
			}
			http.Error(w, "session expired, sign in again", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) != 1 {
				log.Printf("admin: invalid CSRF token from %s", s.clientIP(r))
				http.Error(w, "form expired, reload the page", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}

func isAPI(r *http.Request) bool {
	return len(r.URL.Path) > len("/admin/api/") && r.URL.Path[:len("/admin/api/")] == "/admin/api/"
}

// audit logs who made each change.
func (s *Server) audit(r *http.Request, format string, args ...any) {
	user := "?"
	if sess := session(r); sess != nil {
		user = sess.User
	}
	log.Printf("admin: %s (%s) "+format, append([]any{user, s.clientIP(r)}, args...)...)
}
