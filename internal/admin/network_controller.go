package admin

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"

	"xcod/internal/auth"
	"xcod/internal/certs"
	"xcod/internal/cluster"
	"xcod/internal/config"
	"xcod/internal/i18n"
	"xcod/internal/update"
)

// Network is what the panel uses from the server network, HTTPS and updates.
type Network struct {
	Registry *cluster.Registry
	Agent    *cluster.Agent
	Certs    *certs.Manager
	Updater  *update.Updater
	Releases *update.Releases // versions on GitHub (nil = no checks)
	Version  string
}

type networkView struct {
	C         config.Config
	Repeater  bool
	Nodes     []nodeRow
	Agent     cluster.AgentStatus
	LocalMbps float64
	Base      string // this primary's address for repeaters
	Insecure  bool   // primary over plain http: the install command carries the token without HTTPS
	Version   string
}

type nodeRow struct {
	cluster.Node
	Enabled bool
	Command string // install command (carries the token)
}

// primaryBase is the address repeaters use to reach this server.
func (s *Server) primaryBase(r *http.Request) string {
	return cmp.Or(s.store.Get().PublicURL, s.net.Certs.URL(), s.baseURL(r))
}

func installCommand(base string, rep config.Repeater) string {
	return "curl -fsSL " + base + "/cluster/install.sh | bash -s -- --origin " + base + " --token " + rep.Token()
}

func (s *Server) networkPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	base := s.primaryBase(r)
	v := networkView{C: cfg, Repeater: cfg.IsRepeater(), Agent: s.net.Agent.Status().Localize(s.lang(r)), LocalMbps: s.net.Registry.LocalMbps(),
		Base: base, Insecure: strings.HasPrefix(base, "http://"), Version: s.net.Version}
	enabled := map[string]config.Repeater{}
	for _, rep := range cfg.Repeaters {
		enabled[rep.ID] = rep
	}
	for _, n := range s.net.Registry.Nodes() {
		rep := enabled[n.ID]
		v.Nodes = append(v.Nodes, nodeRow{Node: n, Enabled: rep.Enabled, Command: installCommand(base, rep)})
	}
	s.render(w, r, http.StatusOK, "network", "Network", v)
}

// networkJSON refreshes the repeater table every few seconds.
func (s *Server) networkJSON(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	writeJSON(w, http.StatusOK, map[string]any{"nodes": s.net.Registry.Nodes(), "local_mbps": s.net.Registry.LocalMbps(),
		"local_capacity": cfg.Capacity(), "local_serves": !cfg.OriginNoServe, "agent": s.net.Agent.Status().Localize(s.lang(r))})
}

// networkSettings saves the capacity and the public address (in both modes).
func (s *Server) networkSettings(w http.ResponseWriter, r *http.Request) {
	capacity, err := intField(r, "capacity_mbps")
	if err == nil {
		err = s.store.Update(func(c *config.Config) error {
			c.CapacityMbps = capacity
			c.PublicURL = strings.TrimSpace(r.PostFormValue("public_url"))
			if !c.IsRepeater() {
				c.OriginNoServe = r.PostFormValue("origin_no_serve") == "on"
			}
			return nil
		})
	}
	if err != nil {
		s.fail(w, r, "/admin/network", err)
		return
	}
	s.audit(r, "saved the network settings (capacity %d Mbps)", capacity)
	s.redirect(w, r, "/admin/network", "ok", "Network settings saved.")
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) repeaterCreate(w http.ResponseWriter, r *http.Request) {
	rep := config.Repeater{ID: randomHex(4), Name: strings.TrimSpace(r.PostFormValue("name")), Secret: auth.RandomKey() + randomHex(8), Enabled: true}
	err := s.store.Update(func(c *config.Config) error {
		if c.IsRepeater() {
			return i18n.Errorf("this server is a repeater: register repeaters on the primary")
		}
		c.Repeaters = append(c.Repeaters, rep)
		return nil
	})
	if err != nil {
		s.fail(w, r, "/admin/network", err)
		return
	}
	s.audit(r, "registered repeater %q", rep.Name)
	s.redirect(w, r, "/admin/network", "ok", "Repeater %s registered. Copy the install command and run it on the new server.", rep.Name)
}

// editRepeater applies fn to the repeater sent in the form.
func (s *Server) editRepeater(r *http.Request, fn func(c *config.Config, i int)) (config.Repeater, error) {
	id := r.PostFormValue("id")
	var found config.Repeater
	err := s.store.Update(func(c *config.Config) error {
		i := slices.IndexFunc(c.Repeaters, func(x config.Repeater) bool { return x.ID == id })
		if i < 0 {
			return i18n.Errorf("repeater not found")
		}
		found = c.Repeaters[i]
		fn(c, i)
		return nil
	})
	return found, err
}

func (s *Server) repeaterToggle(w http.ResponseWriter, r *http.Request) {
	rep, err := s.editRepeater(r, func(c *config.Config, i int) { c.Repeaters[i].Enabled = !c.Repeaters[i].Enabled })
	if err != nil {
		s.fail(w, r, "/admin/network", err)
		return
	}
	msg := "Repeater %s enabled again."
	if rep.Enabled {
		msg = "Repeater %s disabled: it no longer receives viewers or channels."
	}
	s.audit(r, "toggled repeater %q", rep.Name)
	s.redirect(w, r, "/admin/network", "ok", msg, rep.Name)
}

// repeaterRotate changes the secret: the old command stops working.
func (s *Server) repeaterRotate(w http.ResponseWriter, r *http.Request) {
	rep, err := s.editRepeater(r, func(c *config.Config, i int) { c.Repeaters[i].Secret = auth.RandomKey() + randomHex(8) })
	if err != nil {
		s.fail(w, r, "/admin/network", err)
		return
	}
	s.audit(r, "rotated the token of repeater %q", rep.Name)
	s.redirect(w, r, "/admin/network", "ok", "Token for %s rotated. Run the new command on the repeater to reconnect it.", rep.Name)
}

func (s *Server) repeaterDelete(w http.ResponseWriter, r *http.Request) {
	rep, err := s.editRepeater(r, func(c *config.Config, i int) { c.Repeaters = slices.Delete(c.Repeaters, i, i+1) })
	if err != nil {
		s.fail(w, r, "/admin/network", err)
		return
	}
	s.net.Registry.Forget(rep.ID)
	s.audit(r, "deleted repeater %q", rep.Name)
	s.redirect(w, r, "/admin/network", "ok", "Repeater %s deleted.", rep.Name)
}

// networkConnect turns this server into a repeater (deletes the local channels).
func (s *Server) networkConnect(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimRight(strings.TrimSpace(r.PostFormValue("origin")), "/")
	if err := cluster.Connect(s.store, origin, strings.TrimSpace(r.PostFormValue("token"))); err != nil {
		s.fail(w, r, "/admin/network", err)
		return
	}
	s.net.Agent.Now()
	s.audit(r, "connected this server as a repeater of %s", origin)
	s.redirect(w, r, "/admin/network", "ok", "Connecting to the primary… the status shows up below in a few seconds.")
}

func (s *Server) networkDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := cluster.Disconnect(s.store); err != nil {
		s.fail(w, r, "/admin/network", err)
		return
	}
	s.audit(r, "disconnected this server from the primary")
	s.redirect(w, r, "/admin/network", "ok", "Disconnected: this server is a primary again, with no channels.")
}

// primaryOnly blocks, on a repeater, the pages whose content comes from the primary.
func (s *Server) primaryOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.store.Get().IsRepeater() {
			s.redirect(w, r, "/admin/network", "err", "This server is a repeater: channels, access and settings come from the primary.")
			return
		}
		next(w, r)
	}
}

// ---------- server (domain and HTTPS) ----------

type serverView struct {
	Domain  string
	Status  certs.Status
	Enabled bool
	MyHost  string
	Now     time.Time
	Update  updateView
}

func (s *Server) serverPage(w http.ResponseWriter, r *http.Request) {
	v := serverView{Domain: s.store.Get().Domain, Status: s.net.Certs.Status().Localize(s.lang(r)), Enabled: s.net.Certs.Enabled(),
		MyHost: r.Host, Now: time.Now(), Update: s.updateView(r)}
	s.render(w, r, http.StatusOK, "server", "Server", v)
}

func (s *Server) serverDomain(w http.ResponseWriter, r *http.Request) {
	d := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(r.PostFormValue("domain"), "https://"), "http://"))
	err := s.store.Update(func(c *config.Config) error {
		c.Domain = strings.TrimRight(d, "/")
		return nil
	})
	if err != nil {
		s.fail(w, r, "/admin/server", err)
		return
	}
	s.net.Certs.Check()
	s.audit(r, "set the domain to %q", d)
	msg := "Domain removed."
	if d != "" {
		msg = "Domain saved. Issuing the certificate (this can take up to a minute)…"
	}
	s.redirect(w, r, "/admin/server", "ok", msg)
}

func (s *Server) serverCheck(w http.ResponseWriter, r *http.Request) {
	s.net.Certs.Check()
	s.redirect(w, r, "/admin/server", "ok", "Checking the certificate…")
}

func (s *Server) serverJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.net.Certs.Status().Localize(s.lang(r)))
}

// ---------- updates ----------

type updateView struct {
	Enabled   bool
	Version   string
	Repeater  bool
	Pending   string
	Result    string
	ResultAt  time.Time
	Repo      string
	Latest    update.Release
	Newer     bool
	CheckErr  string
	CheckedAt time.Time
}

func (s *Server) updateView(r *http.Request) updateView {
	u := s.net.Updater
	v := updateView{Enabled: u.Enabled(), Version: s.net.Version, Repeater: s.store.Get().IsRepeater(), Pending: u.Pending()}
	v.Result, v.ResultAt = u.Result()
	if s.net.Releases != nil && !v.Repeater {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		rel, at, err := s.net.Releases.Latest(ctx, 10*time.Minute)
		v.Repo, v.Latest, v.CheckedAt, v.Newer = s.net.Releases.Repo(), rel, at, rel.Newer(s.net.Version)
		if err != nil {
			v.CheckErr = i18n.Text(s.lang(r), err)
		}
	}
	return v
}

// updateUpload receives a binary uploaded through the panel (no GitHub needed).
func (s *Server) updateUpload(w http.ResponseWriter, r *http.Request) {
	f, _, err := r.FormFile("file")
	if err != nil {
		s.redirect(w, r, "/admin/server", "err", "Choose the xcod-linux-amd64 (or arm64) file.")
		return
	}
	defer f.Close()
	v, err := s.net.Updater.Stage(f, "")
	if err != nil {
		s.fail(w, r, "/admin/server", err)
		return
	}
	s.audit(r, "uploaded version %s to install", v)
	s.redirect(w, r, "/admin/server", "ok", "Version %s received. Installing: xcod restarts in a few seconds (viewers on this server reconnect).", v)
}

// updateGitHub installs the latest version published on GitHub.
func (s *Server) updateGitHub(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	rel, _, err := s.net.Releases.Latest(ctx, time.Minute)
	if err == nil && !rel.Newer(s.net.Version) {
		err = i18n.Errorf("this server is already on version %s", s.net.Version)
	}
	if err == nil {
		_, err = s.net.Updater.Install(ctx, rel)
	}
	if err != nil {
		s.fail(w, r, "/admin/server", err)
		return
	}
	s.audit(r, "updated from %s to %s via GitHub", s.net.Version, rel.Tag)
	s.redirect(w, r, "/admin/server", "ok", "Version %s downloaded and verified. Installing: xcod restarts in a few seconds. Repeaters follow on their own.", rel.Tag)
}

// updateRepeaters asks outdated repeaters to update right away, even with viewers.
func (s *Server) updateRepeaters(w http.ResponseWriter, r *http.Request) {
	s.net.Registry.ForceUpdate()
	s.audit(r, "asked the repeaters to update now")
	s.redirect(w, r, "/admin/network", "ok", "Request sent: outdated repeaters update on their next contact (up to 10 s).")
}
