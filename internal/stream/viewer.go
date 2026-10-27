package stream

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"xcod/internal/config"
)

// FromCDN reports whether the request came from the CDN: it sends X-CDN-Secret (set in a CDN
// rule) with the value of cdn_secret.
func FromCDN(r *http.Request, c *config.Config) bool {
	got := r.Header.Get("X-CDN-Secret")
	return c.CDNSecret != "" && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(c.CDNSecret)) == 1
}

// ClientIP is the IP of whoever made the request, taking a reverse proxy or CDN into account.
// IP headers are only accepted from those sources; from anywhere else they could be forged to
// get around bans.
func ClientIP(r *http.Request, c *config.Config) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	peer := net.ParseIP(ip)
	trusted := peer.IsLoopback() || FromCDN(r, c) || (c.TrustProxy && peer.IsPrivate())
	if !trusted {
		return ip
	}
	for _, h := range []string{"X-Real-IP", "CF-Connecting-IP", "True-Client-IP"} {
		if v := strings.TrimSpace(r.Header.Get(h)); net.ParseIP(v) != nil {
			return v
		}
	}
	return ip
}

// IsHTTPS reports whether the request arrived over HTTPS (directly or through a trusted proxy).
func IsHTTPS(r *http.Request, c *config.Config) bool {
	if r.TLS != nil {
		return true
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	peer := net.ParseIP(ip)
	trusted := peer.IsLoopback() || FromCDN(r, c) || (c.TrustProxy && peer.IsPrivate())
	return trusted && r.Header.Get("X-Forwarded-Proto") == "https"
}
