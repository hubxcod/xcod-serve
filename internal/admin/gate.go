package admin

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// prefix is the secret path segment in front of /admin ("" when no panel token is set).
func (s *Server) prefix() string {
	if t := s.store.Get().PanelToken; t != "" {
		return "/" + t
	}
	return ""
}

// Gate hides the panel behind its secret address: /<token>/admin/... is served as
// /admin/..., and /admin/... itself answers 404, as if there were no panel. Everything else
// (streams, cluster) passes through untouched.
func (s *Server) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.store.Get().PanelToken
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		if isPanelPath(p) {
			http.NotFound(w, r)
			return
		}
		first, rest, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
		// constant-time comparison: response timing does not leak the token
		if subtle.ConstantTimeCompare([]byte(first), []byte(token)) == 1 && isPanelPath("/"+rest) {
			r2 := r.Clone(r.Context())
			r2.URL.Path, r2.URL.RawPath = "/"+rest, ""
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPanelPath(p string) bool { return p == "/admin" || strings.HasPrefix(p, "/admin/") }

// to is a panel path ("/admin/...") as the browser must request it.
func (s *Server) to(path string) string { return s.prefix() + path }

// cookiePath limits the session cookie to the panel's secret address.
func (s *Server) cookiePath() string { return s.prefix() + "/admin" }
