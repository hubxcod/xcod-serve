package stream

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xcod/internal/config"
	"xcod/internal/stats"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// Fixed vectors: the app backend has the same tests, which guarantees both sides sign alike.
func TestSignatureFixedVector(t *testing.T) {
	if got := appSig(testSecret, "live", "846", 1761350400); got != "vizTmoKZcD-iDgTQ0LKHQlgPRFtDWfGrE0UoZQKnSkA" {
		t.Fatalf("legacy signature changed: %s", got)
	}
	if got := appSig(testSecret, "start", "846", 1761350400); got != "354dP9Y5JrnKiXTR9MT6EBVjS1WKjloA0S_ytFEH_3M" {
		t.Fatalf("start signature changed: %s", got)
	}
}

func TestNetworkOf(t *testing.T) {
	cases := map[string]string{
		"189.40.12.77":             "189.40.12",
		"189.40.12.200":            "189.40.12",
		"2804:14c:5b80:1234::1":    "2804:14c:5b80::",
		"2804:14c:5b80:ffff:1::99": "2804:14c:5b80::",
	}
	for ip, want := range cases {
		if got := networkOf(ip); got != want {
			t.Errorf("networkOf(%s) = %s, want %s", ip, got, want)
		}
	}
}

func TestLegacyAppQuery(t *testing.T) {
	now := time.Unix(1761300000, 0)
	q, _ := url.ParseQuery(SignAppLink(testSecret, "846", time.Unix(1761350400, 0)))
	if _, ok := appQuery(q, testSecret, "live", "846", now); !ok {
		t.Fatal("valid link rejected")
	}
	if _, ok := appQuery(q, testSecret, "live", "999", now); ok {
		t.Error("another channel accepted")
	}
	if _, ok := appQuery(q, testSecret, "live", "846", time.Unix(1761350401, 0)); ok {
		t.Error("expired link accepted")
	}
	q2, _ := url.ParseQuery(q.Encode())
	q2.Set("exp", "1861350400")
	if _, ok := appQuery(q2, testSecret, "live", "846", now); ok {
		t.Error("tampered expiry accepted")
	}
	if _, ok := appQuery(q, "", "live", "846", now); ok {
		t.Error("with no secret configured, no link should be valid")
	}
}

func TestSignSegments(t *testing.T) {
	pl := "#EXTM3U\n#EXTINF:4.000,\n/hls/846/10.ts\n#EXTINF:4.000,\n/hls/846/11.ts\n"
	now := time.Unix(1761300000, 0)

	legacy := signSegments(pl, testSecret, "846", 1761350400, "")
	session := signSegments(pl, testSecret, "846", 1761350400, "189.40.12")
	for name, out := range map[string]string{"legacy": legacy, "session": session} {
		if strings.Count(out, "sig=") != 2 || !strings.HasPrefix(out, "#EXTM3U\n#EXTINF:4.000,\n/hls/846/10.ts?") {
			t.Fatalf("%s: badly signed playlist:\n%s", name, out)
		}
	}
	segQuery := func(out string) url.Values {
		line := strings.Split(out, "\n")[2]
		q, _ := url.ParseQuery(line[strings.Index(line, "?")+1:])
		return q
	}
	if !validSegment(segQuery(legacy), testSecret, "846", "8.8.8.8", now) {
		t.Error("legacy segment rejected")
	}
	if !validSegment(segQuery(session), testSecret, "846", "189.40.12.99", now) {
		t.Error("session segment rejected on the same network")
	}
	if validSegment(segQuery(session), testSecret, "846", "177.10.1.1", now) {
		t.Error("session segment accepted on another network")
	}
	if validSegment(segQuery(session), testSecret, "847", "189.40.12.99", now) {
		t.Error("segment accepted for another channel")
	}
}

// Routes with app-only mode on. Channel 846 does not exist: passing authorization gives 404,
// being blocked gives 403.
func TestAppRoutes(t *testing.T) {
	dir := t.TempDir()
	store, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error {
		c.AppOnly, c.AppSecret = true, testSecret
		c.AccessKeys = []config.AccessKey{{Name: "client", Key: "client-key-12345", Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, stats.New(filepath.Join(dir, "stats.json")))
	mux := http.NewServeMux()
	hub.Routes(mux)

	get := func(path, ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// 1. Start link -> master playlist pointing to the session.
	start := "/live/846.m3u8?" + SignStartLink(testSecret, "846", time.Now().Add(time.Minute))
	w := get(start, "189.40.12.77")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "#EXT-X-STREAM-INF") {
		t.Fatalf("start: %d %q", w.Code, w.Body.String())
	}
	var session string
	for _, l := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(l, "/live/846.m3u8?se=") {
			session = l
		}
	}
	if session == "" {
		t.Fatalf("master playlist without a session:\n%s", w.Body.String())
	}

	// 2. .ts start link -> redirects to the session.
	w = get("/live/846.ts?"+SignStartLink(testSecret, "846", time.Now().Add(time.Minute)), "189.40.12.77")
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/live/846.ts?se=") {
		t.Fatalf("ts start: %d %s", w.Code, w.Header().Get("Location"))
	}

	cases := []struct {
		name, path, ip string
		status         int
	}{
		{"session on the same network", session, "189.40.12.200", http.StatusNotFound},
		{"session on another network", session, "177.10.1.1", http.StatusForbidden},
		{"session for another channel", strings.Replace(session, "846.m3u8", "847.m3u8", 1), "189.40.12.77", http.StatusForbidden},
		{"expired start link", "/live/846.m3u8?" + SignStartLink(testSecret, "846", time.Now().Add(-time.Second)), "189.40.12.77", http.StatusForbidden},
		{"start link for another channel", strings.Replace(start, "846.m3u8", "847.m3u8", 1), "189.40.12.77", http.StatusForbidden},
		{"valid legacy link", "/live/846.m3u8?" + SignAppLink(testSecret, "846", time.Now().Add(time.Hour)), "177.10.1.1", http.StatusNotFound},
		{"no key", "/live/846.m3u8", "189.40.12.77", http.StatusForbidden},
		{"with a client key", "/live/client-key-12345/846.m3u8", "189.40.12.77", http.StatusForbidden},
		{"channel list", "/playlist.m3u8", "189.40.12.77", http.StatusForbidden},
	}
	for _, c := range cases {
		if w := get(c.path, c.ip); w.Code != c.status {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.status, strings.TrimSpace(w.Body.String()))
		}
	}
}
