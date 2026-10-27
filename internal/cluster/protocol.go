// Package cluster joins several xcod servers into a network: the primary connects to the
// provider and splits the audience; each repeater pulls the channels from the primary (one
// connection per channel) and serves its own viewers.
//
// The repeater talks to the primary every heartbeatEvery:
//
//	POST /cluster/api/heartbeat   (signed with the repeater's secret)
//	  -> report: public address, capacity, bandwidth and viewers per channel
//	  <- configuration: channels, app secret, keys, HLS settings and kicks
//
// The secret never travels: requests are signed (HMAC-SHA256) and the response, which carries
// the app secret, is encrypted (AES-256-GCM). The network is safe even without HTTPS.
package cluster

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xcod/internal/config"
	"xcod/internal/i18n"
)

const (
	heartbeatEvery = 10 * time.Second
	offlineAfter   = 35 * time.Second // no heartbeat for this long = offline
	clockSkew      = 2 * time.Minute  // accepted clock difference between servers
)

// Report is what the repeater tells the primary.
type Report struct {
	PublicURL string         `json:"public_url"` // empty = the primary uses http://IP:port
	Port      string         `json:"port"`
	Capacity  int            `json:"capacity_mbps"`
	OutMbps   float64        `json:"out_mbps"`
	Viewers   map[string]int `json:"viewers"`
	Version   string         `json:"version"`
	SHA256    string         `json:"sha256"` // of the running binary
	Update    string         `json:"update"` // automatic update status
}

// Sync is what the primary sends to the repeater.
type Sync struct {
	Name         string             `json:"name"`
	AppOnly      bool               `json:"app_only"`
	AppSecret    string             `json:"app_secret"`
	RequireKey   bool               `json:"require_key"`
	AccessKeys   []config.AccessKey `json:"access_keys"`
	IdleTimeout  string             `json:"idle_timeout"`
	StallTimeout string             `json:"stall_timeout"`
	BacklogKB    int                `json:"backlog_kb"`
	HLSSegment   int                `json:"hls_segment_seconds"`
	HLSWindow    int                `json:"hls_window"`
	Channels     []SyncChannel      `json:"channels"`
	Kicks        map[string]int64   `json:"kicks"` // channel -> when it was kicked
	Update       UpdateInfo         `json:"update"`
}

// UpdateInfo describes the primary's binary: a repeater with a different SHA-256 downloads it.
type UpdateInfo struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Force   int64  `json:"force"` // "update now" requested in the panel (otherwise waits until idle)
}

type SyncChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// sign is the hex HMAC-SHA256 of the parts joined with "|" (the installer does the same with
// openssl).
func sign(secret string, parts ...string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(m.Sum(nil))
}

func bodyHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// authHeader builds "XCOD ID:unix:signature" for a request of the given scope.
func authHeader(token, scope string, body []byte, now time.Time) string {
	id, secret, _ := config.SplitToken(token)
	ts := strconv.FormatInt(now.Unix(), 10)
	parts := []string{scope, id, ts}
	if body != nil {
		parts = append(parts, bodyHash(body))
	}
	return "XCOD " + id + ":" + ts + ":" + sign(secret, parts...)
}

// checkAuth verifies the header and returns the repeater that signed it.
func checkAuth(r *http.Request, c *config.Config, scope string, body []byte, now time.Time) (config.Repeater, error) {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "XCOD ")
	f := strings.Split(v, ":")
	if !ok || len(f) != 3 {
		return config.Repeater{}, i18n.Errorf("missing repeater credentials")
	}
	rep, ok := c.FindRepeater(f[0])
	if !ok {
		return config.Repeater{}, i18n.Errorf("unknown or disabled repeater")
	}
	ts, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || now.Sub(time.Unix(ts, 0)).Abs() > clockSkew {
		return config.Repeater{}, i18n.Errorf("repeater clock is off (enable NTP)")
	}
	parts := []string{scope, f[0], f[1]}
	if body != nil {
		parts = append(parts, bodyHash(body))
	}
	if !hmac.Equal([]byte(f[2]), []byte(sign(rep.Secret, parts...))) {
		return config.Repeater{}, i18n.Errorf("invalid signature")
	}
	return rep, nil
}

func aead(secret string) (cipher.AEAD, error) {
	key := sha256.Sum256([]byte("xcod-sync|" + secret))
	b, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// seal encrypts the response: nonce + ciphertext.
func seal(secret string, plain []byte) ([]byte, error) {
	g, err := aead(secret)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return g.Seal(nonce, nonce, plain, nil), nil
}

func open(secret string, data []byte) ([]byte, error) {
	g, err := aead(secret)
	if err != nil {
		return nil, err
	}
	if len(data) < g.NonceSize() {
		return nil, i18n.Errorf("response too short")
	}
	plain, err := g.Open(nil, data[:g.NonceSize()], data[g.NonceSize():], nil)
	if err != nil {
		return nil, i18n.Errorf("response does not match (wrong token?): %v", err)
	}
	return plain, nil
}
