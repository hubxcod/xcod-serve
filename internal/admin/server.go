// Package admin is the web panel (/admin), organized as MVC:
//
//   - models: config.Store (channels, keys, settings), stream.Hub (live state), stats.Meter
//     (history) and auth (credentials and sessions);
//   - views: templates in views/ and static files in static/;
//   - controllers: one *_controller.go file per panel area.
package admin

import (
	"embed"
	"net/http"
	"time"

	"xcod/internal/auth"
	"xcod/internal/config"
	"xcod/internal/stats"
	"xcod/internal/stream"
)

//go:embed views/*.html
var viewsFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	store *config.Store
	hub   *stream.Hub
	meter *stats.Meter
	creds *auth.Credentials
	sess  *auth.Sessions
	lim   *auth.Limiter
	views *views
	net   Network
}

func New(store *config.Store, hub *stream.Hub, meter *stats.Meter, creds *auth.Credentials, net Network) (*Server, error) {
	v, err := loadViews()
	if err != nil {
		return nil, err
	}
	return &Server{store: store, hub: hub, meter: meter, creds: creds, sess: auth.NewSessions(),
		lim: auth.NewLimiter(5, 15*time.Minute), views: v, net: net}, nil
}

// Routes registers the panel on mux. Everything goes through guard (security headers, CDN
// block and IP allowlist); inner pages also require a session and, on POST, the CSRF token.
func (s *Server) Routes(mux *http.ServeMux) {
	public := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.guard(h)) }
	private := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.guard(s.requireAuth(h))) }
	// on a repeater, the content of these pages comes from the primary
	primary := func(pattern string, h http.HandlerFunc) { private(pattern, s.primaryOnly(h)) }

	public("GET /admin/static/", s.static)
	public("GET /admin/login", s.loginForm)
	public("POST /admin/login", s.login)
	public("GET /admin/lang", s.setLang)
	private("POST /admin/logout", s.logout)

	private("GET /admin", s.dashboard)
	private("GET /admin/{$}", s.dashboard)
	private("GET /admin/api/live", s.liveJSON)
	private("POST /admin/api/kick", s.kick)
	private("GET /admin/preview/{file}", s.preview)

	primary("GET /admin/channels", s.channelList)
	primary("GET /admin/channels/new", s.channelNew)
	primary("POST /admin/channels", s.channelCreate)
	primary("GET /admin/channels/{id}", s.channelEdit)
	primary("POST /admin/channels/{id}", s.channelUpdate)
	primary("POST /admin/channels/{id}/delete", s.channelDelete)

	primary("GET /admin/access", s.accessList)
	primary("POST /admin/access", s.accessCreate)
	primary("POST /admin/access/toggle", s.accessToggle)
	primary("POST /admin/access/delete", s.accessDelete)
	primary("POST /admin/access/require", s.accessRequire)
	primary("POST /admin/access/app", s.accessApp)

	private("GET /admin/network", s.networkPage)
	private("GET /admin/api/network", s.networkJSON)
	private("POST /admin/network/settings", s.networkSettings)
	primary("POST /admin/network/repeaters", s.repeaterCreate)
	primary("POST /admin/network/repeaters/toggle", s.repeaterToggle)
	primary("POST /admin/network/repeaters/token", s.repeaterRotate)
	primary("POST /admin/network/repeaters/delete", s.repeaterDelete)
	primary("POST /admin/network/connect", s.networkConnect)
	private("POST /admin/network/disconnect", s.networkDisconnect)
	primary("POST /admin/network/update", s.updateRepeaters)

	private("GET /admin/server", s.serverPage)
	private("GET /admin/api/server", s.serverJSON)
	private("POST /admin/server/domain", s.serverDomain)
	private("POST /admin/server/check", s.serverCheck)
	private("POST /admin/server/upload", s.updateUpload)
	primary("POST /admin/server/update", s.updateGitHub)

	private("GET /admin/stats", s.statsPage)
	private("GET /admin/api/stats", s.statsJSON)
	private("GET /admin/stats.csv", s.statsCSV)

	primary("GET /admin/settings", s.settingsForm)
	primary("POST /admin/settings", s.settingsSave)

	private("GET /admin/security", s.securityPage)
	private("POST /admin/security/password", s.securityPassword)
	private("POST /admin/security/ips", s.securityAllow)
	private("POST /admin/security/address", s.securityAddress)
	private("POST /admin/security/sessions/others", s.securityLogoutOthers)
	private("POST /admin/security/sessions/{id}/end", s.securityLogoutOne)
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	// the URL carries ?v=fingerprint (see asset): new version = new URL, so caching can be long
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.StripPrefix("/admin/", http.FileServerFS(staticFS)).ServeHTTP(w, r)
}
