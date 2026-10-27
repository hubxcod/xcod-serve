package cluster

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xcod/internal/config"
	"xcod/internal/stats"
	"xcod/internal/stream"
)

const (
	appSecret = "0123456789abcdef0123456789abcdef"
	repID     = "0a1b2c3d"
	repSecret = "repeater-secret-with-more-than-32-characters"
)

// fakeProvider sends MPEG-TS packets forever.
func fakeProvider() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pkt := make([]byte, 188*50)
		for i := 0; i < len(pkt); i += 188 {
			pkt[i] = 0x47
		}
		w.Header().Set("Content-Type", "video/mp2t")
		for r.Context().Err() == nil {
			if _, err := w.Write(pkt); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
}

type server struct {
	store *config.Store
	hub   *stream.Hub
	reg   *Registry
	srv   *httptest.Server
}

func newServer(t *testing.T, setup func(c *config.Config)) *server {
	dir := t.TempDir()
	store, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error { setup(c); return nil }); err != nil {
		t.Fatal(err)
	}
	hub := stream.NewHub(store, stats.New(filepath.Join(dir, "stats.json")))
	reg := NewRegistry(store, hub, "test")
	hub.SetBalancer(reg)
	mux := http.NewServeMux()
	hub.Routes(mux)
	reg.Routes(mux)
	s := &server{store: store, hub: hub, reg: reg, srv: httptest.NewServer(mux)}
	t.Cleanup(func() { s.srv.CloseClientConnections(); s.srv.Close() })
	return s
}

func TestFullNetwork(t *testing.T) {
	up := fakeProvider()
	t.Cleanup(func() { up.CloseClientConnections(); up.Close() })

	primary := newServer(t, func(c *config.Config) {
		c.AppSecret = appSecret
		c.Channels["c1"] = config.Channel{Name: "Channel 1", URL: up.URL, Quality: "original"}
		c.Repeaters = []config.Repeater{{ID: repID, Name: "rep-1", Secret: repSecret, Enabled: true}}
	})
	rep := newServer(t, func(c *config.Config) {})
	token := repID + "." + repSecret
	if err := Connect(rep.store, primary.srv.URL, token); err != nil {
		t.Fatal(err)
	}
	agent := NewAgent(rep.store, rep.hub, "8090", "test", func() string { return rep.srv.URL }, nil)

	// 1. heartbeat: the repeater gets channels and secret; the primary sees it online
	if err := agent.exchange(); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	c := rep.store.Get()
	if ch := c.Channels["c1"]; ch.URL != primary.srv.URL+"/live/c1.ts" || ch.Name != "Channel 1" || c.AppSecret != appSecret {
		t.Fatalf("configuration not synced: %+v secret=%q", c.Channels, c.AppSecret)
	}
	nodes := primary.reg.Nodes()
	if len(nodes) != 1 || !nodes[0].Online || nodes[0].PublicURL != rep.srv.URL {
		t.Fatalf("repeater not online: %+v", nodes)
	}

	// 2. video flows: provider -> primary -> repeater -> viewer
	resp, err := http.Get(rep.srv.URL + "/live/c1.ts")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 188*20)
	_, err = io.ReadFull(resp.Body, buf)
	resp.Body.Close()
	if err != nil || buf[0] != 0x47 || buf[188] != 0x47 {
		t.Fatalf("video did not arrive through the repeater: %v", err)
	}
	if st := primary.hub.Status(false); st[0].Reps != 1 || st[0].TS != 0 {
		t.Errorf("primary should see 1 repeater and 0 TS viewers: %+v", st[0])
	}

	// 3. with "distribute only", the app start link already points to the repeater
	primary.store.Update(func(c *config.Config) error { c.OriginNoServe = true; return nil })
	resp, err = http.Get(primary.srv.URL + "/live/c1.m3u8?" + stream.SignStartLink(appSecret, "c1", time.Now().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), rep.srv.URL+"/live/c1.m3u8?se=") {
		t.Fatalf("master playlist does not point to the repeater:\n%s", body)
	}

	// 4. without credentials or with a wrong token, the primary refuses
	req, _ := http.NewRequest(http.MethodGet, primary.srv.URL+"/live/c1.ts", nil)
	req.Header.Set(stream.RepeaterHeader, repID+":forged-signature")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("pull with a forged signature should give 403: %v %v", err, resp.Status)
	}
	wrong := NewAgent(rep.store, rep.hub, "8090", "test", func() string { return "" }, nil)
	rep.store.Update(func(c *config.Config) error {
		c.OriginToken = repID + ".some-other-secret-with-more-than-32-letters"
		return nil
	})
	if err := wrong.exchange(); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a wrong token should be refused: %v", err)
	}
}

func TestPickFavorsMoreBandwidth(t *testing.T) {
	s := newServer(t, func(c *config.Config) {
		c.CapacityMbps = 100
		c.Repeaters = []config.Repeater{{ID: repID, Name: "big", Secret: repSecret, Enabled: true}}
	})
	s.reg.nodes[repID] = &Node{ID: repID, PublicURL: "http://big", Capacity: 900, LastSeen: time.Now()}
	n := map[string]int{}
	for range 10000 {
		n[s.reg.Pick()]++
	}
	// free bandwidth 100 vs 900: ~10% here, ~90% on the repeater
	if here := n[""]; here < 700 || here > 1300 {
		t.Errorf("wrong split: %v", n)
	}

	s.reg.nodes[repID].LastSeen = time.Now().Add(-time.Minute) // offline
	for range 100 {
		if got := s.reg.Pick(); got != "" {
			t.Fatalf("offline repeater got viewers: %s", got)
		}
	}
}

func TestSignatureMatchesInstaller(t *testing.T) {
	// install.sh computes: printf 'dl|ID|TS' | openssl dgst -sha256 -hmac SECRET
	if got := sign("secret", "dl", "0a1b2c3d", "1761350400"); got != "15af0540670c21818d6247ceca7dc46b78f7259f05c4e6673a1a831cb5639d53" {
		t.Fatalf("signature differs from openssl: %s", got)
	}
}
