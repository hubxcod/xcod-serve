package stream

import (
	"crypto/hmac"
	"net/http"
	"strings"

	"xcod/internal/config"
)

// RepeaterHeader carries the repeater's credential when it pulls a channel from the primary.
// The secret is never sent: only "ID:signature".
const RepeaterHeader = "X-Xcod-Repeater"

// RepeaterPullAuth builds the RepeaterHeader value from the "ID.secret" token.
func RepeaterPullAuth(token string) string {
	id, secret, _ := config.SplitToken(token)
	return id + ":" + sign(secret, "pull", id)
}

// repeaterFrom identifies the repeater behind a request (primary only).
func repeaterFrom(r *http.Request, c *config.Config) (config.Repeater, bool) {
	id, sig, ok := strings.Cut(r.Header.Get(RepeaterHeader), ":")
	if !ok || c.IsRepeater() {
		return config.Repeater{}, false
	}
	rep, ok := c.FindRepeater(id)
	if !ok || !hmac.Equal([]byte(sig), []byte(sign(rep.Secret, "pull", id))) {
		return config.Repeater{}, false
	}
	return rep, true
}
