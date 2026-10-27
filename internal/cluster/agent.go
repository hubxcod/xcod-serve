package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"reflect"
	"runtime"
	"sync"
	"time"

	"xcod/internal/config"
	"xcod/internal/i18n"
	"xcod/internal/stream"
	"xcod/internal/update"
)

// Agent is the repeater's side: it sends the heartbeat and applies the primary's configuration.
type Agent struct {
	store     *config.Store
	hub       *stream.Hub
	port      string // this server's HTTP port (sent in the report)
	version   string
	publicURL func() string // public address (HTTPS domain, if any)
	client    *http.Client
	updater   *update.Updater

	mu        sync.Mutex
	status    AgentStatus
	kicks     map[string]int64
	out       rate
	failed    map[string]time.Time // SHA-256 that failed -> when (no retry for 1h)
	lastForce int64
	updating  bool
}

// AgentStatus is the state of the link to the primary (shown in the repeater's panel).
type AgentStatus struct {
	Name     string    `json:"name"` // name given on the primary
	LastOK   time.Time `json:"last_ok"`
	LastErr  string    `json:"last_err"`
	Channels int       `json:"channels"`
	Update   string    `json:"update"` // automatic update status
	errMsg   error     // LastErr, translatable
	updMsg   error     // Update, translatable
}

// Localize returns the status with its messages in the given language.
func (s AgentStatus) Localize(lang string) AgentStatus {
	if s.errMsg != nil {
		s.LastErr = i18n.Text(lang, s.errMsg)
	}
	if s.updMsg != nil {
		s.Update = i18n.Text(lang, s.updMsg)
	}
	return s
}

func NewAgent(store *config.Store, hub *stream.Hub, port, version string, publicURL func() string, updater *update.Updater) *Agent {
	return &Agent{store: store, hub: hub, port: port, version: version, publicURL: publicURL, updater: updater,
		client: &http.Client{Timeout: 15 * time.Second}, failed: map[string]time.Time{}}
}

func (a *Agent) Status() AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// Run sends a heartbeat every heartbeatEvery while the server is a repeater.
func (a *Agent) Run(stop <-chan struct{}) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		if a.store.Get().IsRepeater() {
			a.beat()
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// Now sends an out-of-schedule heartbeat (when connecting from the panel).
func (a *Agent) Now() { go a.beat() }

func (a *Agent) beat() {
	err := a.exchange()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		if a.status.LastErr != err.Error() {
			log.Printf("cluster: primary: %v", err)
		}
		a.status.LastErr, a.status.errMsg = err.Error(), err
		return
	}
	a.status.LastErr, a.status.errMsg = "", nil
	a.status.LastOK = time.Now()
}

func (a *Agent) exchange() error {
	cfg := a.store.Get()
	a.mu.Lock()
	mbps := a.out.sample(a.hub.BytesOut(), time.Now())
	upd := a.status.Update
	a.mu.Unlock()
	body, _ := json.Marshal(Report{PublicURL: a.publicURL(), Port: a.port, Capacity: cfg.Capacity(),
		OutMbps: mbps, Viewers: a.hub.Viewers(), Version: a.version, SHA256: update.SelfSHA(), Update: upd})

	req, err := http.NewRequest(http.MethodPost, cfg.Origin+"/cluster/api/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authHeader(cfg.OriginToken, "hb", body, time.Now()))
	resp, err := a.client.Do(req)
	if err != nil {
		return i18n.Errorf("cannot reach %s", cfg.Origin)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return i18n.Errorf("primary refused (%d): %s", resp.StatusCode, bytes.TrimSpace(data))
	}
	_, secret, _ := config.SplitToken(cfg.OriginToken)
	plain, err := open(secret, data)
	if err != nil {
		return err
	}
	var s Sync
	if err := json.Unmarshal(plain, &s); err != nil {
		return i18n.Errorf("invalid response: %v", err)
	}
	if err := a.apply(s); err != nil {
		return i18n.Errorf("primary configuration: %v", err)
	}
	return nil
}

var errUnchanged = errors.New("unchanged")

// apply copies the primary's configuration. Channels point to the primary's .ts, already at
// the final quality (the primary does the transcoding).
func (a *Agent) apply(s Sync) error {
	err := a.store.Update(func(c *config.Config) error {
		if !c.IsRepeater() {
			return nil
		}
		next := *c
		next.AppOnly, next.AppSecret, next.RequireKey, next.AccessKeys = s.AppOnly, s.AppSecret, s.RequireKey, s.AccessKeys
		next.IdleTimeout, next.StallTimeout = s.IdleTimeout, s.StallTimeout
		next.BacklogKB, next.HLSSegment, next.HLSWindow = s.BacklogKB, s.HLSSegment, s.HLSWindow
		next.MaxUpstreams = 0 // connections to the primary, not to the provider
		next.Channels = map[string]config.Channel{}
		for _, ch := range s.Channels {
			next.Channels[ch.ID] = config.Channel{Name: ch.Name, URL: c.Origin + "/live/" + ch.ID + ".ts", Quality: "original"}
		}
		if reflect.DeepEqual(next, *c) {
			return errUnchanged
		}
		*c = next
		return nil
	})
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	if err != nil {
		return err
	}

	a.mu.Lock()
	first := a.kicks == nil
	a.status.Name, a.status.Channels = s.Name, len(s.Channels)
	var kick []string
	for id, t := range s.Kicks {
		if !first && t > a.kicks[id] {
			kick = append(kick, id)
		}
	}
	a.kicks = s.Kicks // the first time only records them: old kicks do not apply
	a.mu.Unlock()
	for _, id := range kick {
		a.hub.Kick(id)
	}
	a.maybeUpdate(s.Update)
	return nil
}

// maybeUpdate downloads the primary's binary when it differs from ours. With viewers
// watching, it waits until the server is idle (restarting drops whoever watches here), unless
// the primary's panel asks to "update now".
func (a *Agent) maybeUpdate(u UpdateInfo) {
	set := func(format string, args ...any) {
		a.mu.Lock()
		a.status.Update, a.status.updMsg = "", nil
		if format != "" {
			a.status.updMsg = i18n.Errorf(format, args...)
			a.status.Update = a.status.updMsg.Error()
		}
		a.mu.Unlock()
	}
	switch {
	case a.updater == nil || !a.updater.Enabled():
		set("")
		return
	case u.SHA256 == "" || u.SHA256 == update.SelfSHA():
		set("")
		return
	case u.OS != runtime.GOOS || u.Arch != runtime.GOARCH:
		set("primary runs on %s/%s: update this server with the installer", u.OS, u.Arch)
		return
	case a.updater.Pending() != "":
		set("installing %s…", a.updater.Pending())
		return
	}
	a.mu.Lock()
	failedAt, failed := a.failed[u.SHA256]
	force := u.Force > a.lastForce
	busy := a.updating
	a.mu.Unlock()
	if busy || (failed && time.Since(failedAt) < time.Hour && !force) {
		return
	}
	viewers := 0
	for _, n := range a.hub.Viewers() {
		viewers += n
	}
	if viewers > 0 && !force {
		set("version %s available: updates when idle (%d watching)", u.Version, viewers)
		return
	}
	a.mu.Lock()
	a.updating, a.lastForce = true, u.Force
	a.mu.Unlock()
	set("downloading %s from the primary…", u.Version)
	go func() {
		err := a.download(u)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.updating = false
		if err != nil {
			a.failed[u.SHA256] = time.Now()
			a.status.updMsg = i18n.Errorf("update to %s failed: %v", u.Version, err)
			a.status.Update = a.status.updMsg.Error()
			log.Printf("cluster: %s", a.status.Update)
			return
		}
		a.status.updMsg = i18n.Errorf("installing %s…", u.Version)
		a.status.Update = a.status.updMsg.Error()
		log.Printf("cluster: version %s downloaded from the primary; restarting to install", u.Version)
	}()
}

func (a *Agent) download(u UpdateInfo) error {
	cfg := a.store.Get()
	req, err := http.NewRequest(http.MethodGet, cfg.Origin+"/cluster/xcod/"+runtime.GOARCH, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authHeader(cfg.OriginToken, "dl", nil, time.Now()))
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return i18n.Errorf("cannot reach the primary")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return i18n.Errorf("primary refused (%d): %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	// the SHA-256 came in the encrypted heartbeat response: a file swapped in transit is rejected
	_, err = a.updater.Stage(resp.Body, u.SHA256)
	return err
}

// Connect turns this server into a repeater of the primary (from the panel or the installer).
func Connect(store *config.Store, origin, token string) error {
	return store.Update(func(c *config.Config) error {
		c.Mode, c.Origin, c.OriginToken = config.ModeRepeater, origin, token
		c.Channels = map[string]config.Channel{} // channels now come from the primary
		c.Repeaters = nil
		return nil
	})
}

// Disconnect turns the server back into a primary, without channels.
func Disconnect(store *config.Store) error {
	return store.Update(func(c *config.Config) error {
		c.Mode, c.Origin, c.OriginToken = "", "", ""
		c.Channels = map[string]config.Channel{}
		c.MaxUpstreams = config.Default().MaxUpstreams
		return nil
	})
}
