package cluster

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"xcod/deploy"
	"xcod/internal/config"
	"xcod/internal/stream"
	"xcod/internal/update"
)

// Registry is the primary's side: it receives the heartbeats and picks the server for each
// new viewer.
type Registry struct {
	store   *config.Store
	hub     *stream.Hub
	version string

	mu    sync.Mutex
	nodes map[string]*Node // by repeater ID
	local rate
	force int64 // last "update repeaters now"
}

// Node is the last known state of a repeater.
type Node struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	PublicURL string         `json:"public_url"`
	IP        string         `json:"ip"`
	Version   string         `json:"version"`
	Capacity  int            `json:"capacity_mbps"`
	OutMbps   float64        `json:"out_mbps"`
	Viewers   map[string]int `json:"viewers"`
	LastSeen  time.Time      `json:"last_seen"`
	Online    bool           `json:"online"`
	SHA256    string         `json:"-"`
	Outdated  bool           `json:"outdated"` // runs a different binary than the primary
	Update    string         `json:"update"`
}

// rate measures outbound bandwidth from the bytes sent.
type rate struct {
	bytes int64
	at    time.Time
	mbps  float64
}

func (r *rate) sample(total int64, now time.Time) float64 {
	if !r.at.IsZero() && now.After(r.at) && total >= r.bytes {
		r.mbps = float64(total-r.bytes) * 8 / now.Sub(r.at).Seconds() / 1e6
	}
	r.bytes, r.at = total, now
	return r.mbps
}

func NewRegistry(store *config.Store, hub *stream.Hub, version string) *Registry {
	return &Registry{store: store, hub: hub, version: version, nodes: map[string]*Node{}}
}

// ForceUpdate asks outdated repeaters to update right away, even with viewers.
func (g *Registry) ForceUpdate() {
	g.mu.Lock()
	g.force = time.Now().Unix()
	g.mu.Unlock()
}

// Run measures this server's bandwidth (used to split the audience).
func (g *Registry) Run(stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			total := g.hub.BytesOut()
			g.mu.Lock()
			g.local.sample(total, now)
			g.mu.Unlock()
		}
	}
}

// Routes registers the endpoints used by repeaters.
func (g *Registry) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /cluster/api/heartbeat", g.heartbeat)
	mux.HandleFunc("GET /cluster/install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(deploy.InstallScript)
	})
	mux.HandleFunc("GET /cluster/xcod/{arch}", g.download)
}

func (g *Registry) heartbeat(w http.ResponseWriter, r *http.Request) {
	cfg := g.store.Get()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
		return
	}
	ip := stream.ClientIP(r, &cfg)
	rep, err := checkAuth(r, &cfg, "hb", body, time.Now())
	if err == nil && cfg.IsRepeater() {
		err = errChained
	}
	if err != nil {
		log.Printf("cluster: heartbeat rejected from %s: %v", ip, err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	var rp Report
	if err := json.Unmarshal(body, &rp); err != nil {
		http.Error(w, "invalid report", http.StatusBadRequest)
		return
	}

	pub := strings.TrimRight(rp.PublicURL, "/")
	if !strings.HasPrefix(pub, "http://") && !strings.HasPrefix(pub, "https://") {
		pub = "http://" + net.JoinHostPort(ip, cmp.Or(rp.Port, "8090"))
	}
	g.mu.Lock()
	n := g.nodes[rep.ID]
	if n == nil {
		log.Printf("cluster: repeater %s connected (%s)", rep.Name, pub)
		n = &Node{ID: rep.ID}
		g.nodes[rep.ID] = n
	}
	n.Name, n.PublicURL, n.IP, n.Version = rep.Name, pub, ip, rp.Version
	n.Capacity, n.OutMbps, n.Viewers, n.LastSeen = rp.Capacity, rp.OutMbps, rp.Viewers, time.Now()
	n.SHA256, n.Update = rp.SHA256, rp.Update
	g.mu.Unlock()

	plain, _ := json.Marshal(g.syncFor(rep, cfg))
	out, err := seal(rep.Secret, plain)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out)
}

var errChained = errors.New("this server is a repeater: connect to the primary")

func (g *Registry) syncFor(rep config.Repeater, c config.Config) Sync {
	s := Sync{Name: rep.Name, AppOnly: c.AppOnly, AppSecret: c.AppSecret, RequireKey: c.RequireKey,
		AccessKeys: c.AccessKeys, IdleTimeout: c.IdleTimeout, StallTimeout: c.StallTimeout,
		BacklogKB: c.BacklogKB, HLSSegment: c.HLSSegment, HLSWindow: c.HLSWindow, Kicks: g.hub.Kicks()}
	g.mu.Lock()
	s.Update = UpdateInfo{Version: g.version, SHA256: update.SelfSHA(), OS: runtime.GOOS, Arch: runtime.GOARCH, Force: g.force}
	g.mu.Unlock()
	for id, ch := range c.Channels {
		s.Channels = append(s.Channels, SyncChannel{ID: id, Name: ch.Name})
	}
	slices.SortFunc(s.Channels, func(a, b SyncChannel) int { return strings.Compare(a.ID, b.ID) })
	return s
}

// download serves this very binary to the repeater installer.
func (g *Registry) download(w http.ResponseWriter, r *http.Request) {
	cfg := g.store.Get()
	if _, err := checkAuth(r, &cfg, "dl", nil, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if runtime.GOOS != "linux" || r.PathValue("arch") != runtime.GOARCH {
		http.Error(w, "the primary runs on "+runtime.GOOS+"/"+runtime.GOARCH+
			": download xcod for this architecture and use --binary", http.StatusNotFound)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		http.Error(w, "binary not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, exe)
}

// Nodes lists the registered repeaters with their last state.
func (g *Registry) Nodes() []Node {
	cfg := g.store.Get()
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Node, 0, len(cfg.Repeaters))
	for _, rep := range cfg.Repeaters {
		n := Node{ID: rep.ID, Name: rep.Name}
		if got := g.nodes[rep.ID]; got != nil {
			n = *got
			n.Name = rep.Name
		}
		n.Online = rep.Enabled && time.Since(n.LastSeen) < offlineAfter
		n.Outdated = n.SHA256 != "" && n.SHA256 != update.SelfSHA()
		out = append(out, n)
	}
	return out
}

// LocalMbps is this server's outbound bandwidth.
func (g *Registry) LocalMbps() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.local.mbps
}

// Viewers adds up the viewers of online repeaters, per channel (fed into the statistics).
func (g *Registry) Viewers() map[string]int {
	out := map[string]int{}
	for _, n := range g.Nodes() {
		if n.Online {
			for id, v := range n.Viewers {
				out[id] += v
			}
		}
	}
	return out
}

// Pick chooses the server for a new viewer with a random draw weighted by each server's free
// bandwidth: bigger and emptier servers get more viewers. "" = this server.
func (g *Registry) Pick() string {
	cfg := g.store.Get()
	if cfg.IsRepeater() || len(cfg.Repeaters) == 0 {
		return ""
	}
	type cand struct {
		url    string
		weight float64
	}
	var cs []cand
	weight := func(capacity int, out float64) float64 {
		c := float64(capacity)
		if c <= 0 {
			c = config.DefaultCapacity
		}
		return max(c-out, c*0.02) // a full server still gets a little: the measurement lags
	}
	if !cfg.OriginNoServe {
		cs = append(cs, cand{"", weight(cfg.Capacity(), g.LocalMbps())})
	}
	for _, n := range g.Nodes() {
		if n.Online && n.PublicURL != "" {
			cs = append(cs, cand{n.PublicURL, weight(n.Capacity, n.OutMbps)})
		}
	}
	total := 0.0
	for _, c := range cs {
		total += c.weight
	}
	if total <= 0 {
		return "" // no repeater online: serve here
	}
	x := rand.Float64() * total
	for _, c := range cs {
		if x -= c.weight; x < 0 {
			return c.url
		}
	}
	return cs[len(cs)-1].url
}

// Forget deletes the state of a deleted repeater.
func (g *Registry) Forget(id string) {
	g.mu.Lock()
	delete(g.nodes, id)
	g.mu.Unlock()
}
