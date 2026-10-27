// Package config holds the xcod configuration (config.json in the data directory): channels,
// limits, CDN, access keys and panel rules.
package config

import (
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"xcod/internal/i18n"
)

type Config struct {
	UserAgent     string `json:"user_agent"`
	IdleTimeout   string `json:"idle_timeout"`        // close the upstream after X without viewers
	StallTimeout  string `json:"stall_timeout"`       // reconnect if the upstream sends nothing for X
	BacklogKB     int    `json:"backlog_kb"`          // buffer sent to a TS client when it joins
	MaxUpstreams  int    `json:"max_upstreams"`       // simultaneous provider connections (0 = unlimited)
	MaxClients    int    `json:"max_clients"`         // simultaneous viewers in total (0 = unlimited)
	HLSSegment    int    `json:"hls_segment_seconds"` // target HLS segment duration
	HLSWindow     int    `json:"hls_window"`          // segments listed in the HLS playlist
	FFmpegPath    string `json:"ffmpeg_path"`         // empty = look it up in PATH
	EncoderPreset string `json:"encoder_preset"`      // x264 preset (CPU vs quality)

	CDNURL     string `json:"cdn_url"`     // public CDN address (empty = no CDN)
	CDNSecret  string `json:"cdn_secret"`  // value of the X-CDN-Secret header sent by the CDN
	TrustProxy bool   `json:"trust_proxy"` // accept X-Real-IP from a private network (reverse proxy)

	RequireKey bool        `json:"require_key"` // channel links require an access key
	AccessKeys []AccessKey `json:"access_keys"`

	// App access: the app backend issues expiring signed links (HMAC) with AppSecret. With
	// AppOnly on, only those links open the channels.
	AppOnly    bool     `json:"app_only"`
	AppSecret  string   `json:"app_secret"`
	AdminAllow []string `json:"admin_allow"` // IPs/networks allowed to open the panel (empty = all)
	PanelToken string   `json:"panel_token"` // secret path segment: the panel lives at /<token>/admin (empty = /admin)

	// Server network (see internal/cluster). The primary connects to the provider and shares
	// the audience with the repeaters; a repeater pulls the channels from the primary.
	Mode          string     `json:"mode"`            // "" = primary, "repeater" = repeater
	Origin        string     `json:"origin"`          // repeater: primary's address
	OriginToken   string     `json:"origin_token"`    // repeater: "ID.secret" issued by the primary
	PublicURL     string     `json:"public_url"`      // this server's public address (empty = automatic)
	CapacityMbps  int        `json:"capacity_mbps"`   // this server's outbound bandwidth (0 = DefaultCapacity)
	OriginNoServe bool       `json:"origin_no_serve"` // primary: only distribute, serve no viewers
	Repeaters     []Repeater `json:"repeaters"`
	Domain        string     `json:"domain"` // domain with automatic HTTPS (Let's Encrypt)

	Channels map[string]Channel `json:"channels"`

	idle, stall time.Duration
	adminNets   []*net.IPNet
}

type Channel struct {
	Name      string `json:"name,omitempty"` // display name (empty = the ID)
	URL       string `json:"url"`
	Quality   string `json:"quality"`              // profile name (see profiles.go)
	FPS       int    `json:"fps,omitempty"`        // 0 = keep the source's
	VideoKbps int    `json:"video_kbps,omitempty"` // 0 = the profile's ceiling
}

// UnmarshalJSON also accepts the old format: "id": "url".
func (ch *Channel) UnmarshalJSON(b []byte) error {
	var url string
	if json.Unmarshal(b, &url) == nil {
		*ch = Channel{URL: url, Quality: "original"}
		return nil
	}
	type plain Channel
	return json.Unmarshal(b, (*plain)(ch))
}

// AccessKey unlocks the channels for one client when RequireKey is on.
type AccessKey struct {
	Name       string `json:"name"`
	Key        string `json:"key"`
	Enabled    bool   `json:"enabled"`
	Expires    string `json:"expires,omitempty"` // YYYY-MM-DD, valid until the end of that day
	MaxScreens int    `json:"max_screens"`       // simultaneous screens (0 = unlimited)
}

// Repeater is a repeater server registered on the primary.
type Repeater struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Secret  string `json:"secret"` // never sent over the network: it only signs requests
	Enabled bool   `json:"enabled"`
}

// Token is what gets pasted on the repeater ("ID.secret").
func (r Repeater) Token() string { return r.ID + "." + r.Secret }

// ModeRepeater marks the server as a repeater.
const ModeRepeater = "repeater"

// DefaultCapacity is the outbound bandwidth assumed when none is set (a 1 Gbps port with headroom).
const DefaultCapacity = 800

func (c Config) IsRepeater() bool { return c.Mode == ModeRepeater }

// Capacity is this server's outbound bandwidth in Mbps.
func (c Config) Capacity() int {
	if c.CapacityMbps > 0 {
		return c.CapacityMbps
	}
	return DefaultCapacity
}

// FindRepeater returns the enabled repeater with this ID.
func (c Config) FindRepeater(id string) (Repeater, bool) {
	for _, r := range c.Repeaters {
		if r.ID == id && r.Enabled {
			return r, true
		}
	}
	return Repeater{}, false
}

// SplitToken splits "ID.secret".
func SplitToken(t string) (id, secret string, ok bool) {
	id, secret, ok = strings.Cut(strings.TrimSpace(t), ".")
	return id, secret, ok && repIDRe.MatchString(id) && len(secret) >= 32
}

// Valid reports whether the key can be used now.
func (k AccessKey) Valid(now time.Time) bool {
	if !k.Enabled {
		return false
	}
	if k.Expires == "" {
		return true
	}
	end, err := time.ParseInLocation("2006-01-02", k.Expires, time.Local)
	return err == nil && now.Before(end.AddDate(0, 0, 1))
}

var (
	idRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	keyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{12,64}$`)

	repIDRe  = regexp.MustCompile(`^[a-f0-9]{8}$`)
	panelRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

// ValidID reports whether the text can be a channel ID (it becomes part of the URL).
func ValidID(id string) bool { return idRe.MatchString(id) }

func Default() Config {
	c := Config{
		UserAgent: "VLC/3.0.20 LibVLC/3.0.20", IdleTimeout: "10s", StallTimeout: "15s",
		BacklogKB: 1024, MaxUpstreams: 1, MaxClients: 1100, HLSSegment: 4, HLSWindow: 5,
		EncoderPreset: "veryfast", Channels: map[string]Channel{},
	}
	c.Validate()
	return c
}

func (c Config) Idle() time.Duration  { return c.idle }
func (c Config) Stall() time.Duration { return c.stall }

// AdminAllowed reports whether the IP may open the panel.
func (c Config) AdminAllowed(ip net.IP) bool {
	if len(c.adminNets) == 0 {
		return true
	}
	for _, n := range c.adminNets {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// FindKey returns the access key with this value.
func (c Config) FindKey(key string) (AccessKey, bool) {
	for _, k := range c.AccessKeys {
		if k.Key == key {
			return k, true
		}
	}
	return AccessKey{}, false
}

// Validate checks the configuration and computes the derived fields.
func (c *Config) Validate() error {
	var err error
	if c.idle, err = time.ParseDuration(c.IdleTimeout); err != nil || c.idle < time.Second {
		return i18n.Errorf("invalid idle timeout: %q (e.g. 10s)", c.IdleTimeout)
	}
	if c.stall, err = time.ParseDuration(c.StallTimeout); err != nil || c.stall < time.Second {
		return i18n.Errorf("invalid stall timeout: %q (e.g. 15s)", c.StallTimeout)
	}
	switch {
	case c.BacklogKB < 0 || c.BacklogKB > 16384:
		return i18n.Errorf("initial buffer must be between 0 and 16384 KB")
	case c.MaxUpstreams < 0 || c.MaxClients < 0:
		return i18n.Errorf("limits cannot be negative")
	case c.HLSSegment < 1 || c.HLSSegment > 10:
		return i18n.Errorf("HLS segment must be between 1 and 10 seconds")
	case c.HLSWindow < 3 || c.HLSWindow > 20:
		return i18n.Errorf("the HLS playlist must have between 3 and 20 segments")
	case !slices.Contains(EncoderPresets, c.EncoderPreset):
		return i18n.Errorf("invalid encoder preset: %q", c.EncoderPreset)
	}

	c.CDNURL = strings.TrimRight(strings.TrimSpace(c.CDNURL), "/")
	if c.CDNURL != "" && !isHTTP(c.CDNURL) {
		return i18n.Errorf("CDN address must start with http:// or https://")
	}
	c.CDNSecret = strings.TrimSpace(c.CDNSecret)
	if c.CDNSecret != "" && len(c.CDNSecret) < 16 {
		return i18n.Errorf("the CDN secret must have at least 16 characters")
	}

	c.AppSecret = strings.TrimSpace(c.AppSecret)
	switch {
	case c.AppSecret != "" && len(c.AppSecret) < 32:
		return i18n.Errorf("the app secret must have 32 or more characters")
	case c.AppOnly && c.AppSecret == "":
		return i18n.Errorf("set the app secret before making the channels app-only")
	}

	seen := map[string]bool{}
	for i, k := range c.AccessKeys {
		k.Name = strings.TrimSpace(k.Name)
		switch {
		case k.Name == "":
			return i18n.Errorf("every access key needs a name")
		case !keyRe.MatchString(k.Key):
			return i18n.Errorf("invalid key for %s (12 to 64 letters, digits, - or _)", k.Name)
		case seen[k.Key]:
			return i18n.Errorf("duplicate key in %s", k.Name)
		case k.MaxScreens < 0:
			return i18n.Errorf("screen limit for %s cannot be negative", k.Name)
		}
		if k.Expires != "" {
			if _, err := time.Parse("2006-01-02", k.Expires); err != nil {
				return i18n.Errorf("invalid expiry for %s: use YYYY-MM-DD", k.Name)
			}
		}
		seen[k.Key] = true
		c.AccessKeys[i] = k
	}

	if err := c.validateNetwork(); err != nil {
		return err
	}

	c.PanelToken = strings.TrimSpace(c.PanelToken)
	if c.PanelToken != "" && !panelRe.MatchString(c.PanelToken) {
		return i18n.Errorf("the panel token must have 16 to 64 letters, digits, - or _")
	}

	c.adminNets = nil
	for _, s := range c.AdminAllow {
		n, err := parseNet(s)
		if err != nil {
			return i18n.Errorf("invalid panel IP or network: %q", s)
		}
		c.adminNets = append(c.adminNets, n)
	}

	if c.Channels == nil {
		c.Channels = map[string]Channel{}
	}
	for id, ch := range c.Channels {
		if err := ch.validate(id); err != nil {
			return err
		}
		if ch.Quality == "" {
			ch.Quality = "original"
		}
		ch.Name = strings.TrimSpace(ch.Name)
		if strings.ContainsAny(ch.Name, "\r\n,\"") || len(ch.Name) > 80 {
			return i18n.Errorf("channel %s: the name cannot contain commas, quotes or line breaks (max. 80)", id)
		}
		c.Channels[id] = ch
	}
	return nil
}

// validateNetwork checks the mode, the repeaters and the domain.
func (c *Config) validateNetwork() error {
	c.Origin = strings.TrimRight(strings.TrimSpace(c.Origin), "/")
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	c.Domain = strings.ToLower(strings.TrimSpace(c.Domain))
	switch {
	case c.Mode != "" && c.Mode != ModeRepeater:
		return i18n.Errorf("invalid mode: %q", c.Mode)
	case c.IsRepeater() && !isHTTP(c.Origin):
		return i18n.Errorf("the primary's address must start with http:// or https://")
	case c.PublicURL != "" && !isHTTP(c.PublicURL):
		return i18n.Errorf("the public address must start with http:// or https://")
	case c.CapacityMbps < 0 || c.CapacityMbps > 100000:
		return i18n.Errorf("capacity must be between 0 and 100000 Mbps")
	case c.Domain != "" && !domainRe.MatchString(c.Domain):
		return i18n.Errorf("invalid domain: %q (e.g. video.example.com)", c.Domain)
	}
	if c.IsRepeater() {
		if _, _, ok := SplitToken(c.OriginToken); !ok {
			return i18n.Errorf("invalid primary token (copy it from the primary's Network page)")
		}
	}
	ids := map[string]bool{}
	for i, r := range c.Repeaters {
		r.Name = strings.TrimSpace(r.Name)
		switch {
		case r.Name == "" || len(r.Name) > 64:
			return i18n.Errorf("every repeater needs a name (up to 64 characters)")
		case !repIDRe.MatchString(r.ID) || ids[r.ID]:
			return i18n.Errorf("invalid ID for repeater %s", r.Name)
		case len(r.Secret) < 32:
			return i18n.Errorf("secret of repeater %s is too short", r.Name)
		}
		ids[r.ID] = true
		c.Repeaters[i] = r
	}
	return nil
}

func (ch Channel) validate(id string) error {
	switch {
	case !ValidID(id):
		return i18n.Errorf("invalid channel ID: %q (use letters, digits, - and _)", id)
	case !isHTTP(ch.URL):
		return i18n.Errorf("channel %s: the URL must start with http:// or https://", id)
	case ch.FPS < 0 || ch.FPS > 60:
		return i18n.Errorf("channel %s: FPS must be between 0 and 60", id)
	case ch.VideoKbps < 0 || ch.VideoKbps > 20000:
		return i18n.Errorf("channel %s: video ceiling must be between 0 and 20000 kbps", id)
	}
	if _, ok := FindQuality(ch.Quality); !ok {
		return i18n.Errorf("channel %s: invalid quality %q", id, ch.Quality)
	}
	return nil
}

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// parseNet accepts "1.2.3.4", "1.2.3.0/24" or IPv6.
func parseNet(s string) (*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, errors.New("invalid IP")
		}
		bits := 128
		if ip.To4() != nil {
			ip, bits = ip.To4(), 32
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
	}
	_, n, err := net.ParseCIDR(s)
	return n, err
}

// clone copies maps and slices so that edits do not change the configuration in use.
func (c Config) clone() Config {
	c.Channels = maps(c.Channels)
	c.AccessKeys = slices.Clone(c.AccessKeys)
	c.AdminAllow = slices.Clone(c.AdminAllow)
	c.Repeaters = slices.Clone(c.Repeaters)
	return c
}

func maps(m map[string]Channel) map[string]Channel {
	out := make(map[string]Channel, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
