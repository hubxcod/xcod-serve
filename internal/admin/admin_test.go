package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"xcod/internal/auth"
	"xcod/internal/certs"
	"xcod/internal/cluster"
	"xcod/internal/config"
	"xcod/internal/stats"
	"xcod/internal/stream"
	"xcod/internal/update"
)

const testPass = "test-password-123"

type env struct {
	t     *testing.T
	mux   http.Handler
	store *config.Store
	cook  *http.Cookie
	csrf  string
}

func newEnv(t *testing.T) *env {
	dir := t.TempDir()
	store, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	creds, _, err := auth.LoadCredentials(filepath.Join(dir, "admin.json"), "admin", testPass, false)
	if err != nil {
		t.Fatal(err)
	}
	meter := stats.New(filepath.Join(dir, "stats.json"))
	hub := stream.NewHub(store, meter)
	reg := cluster.NewRegistry(store, hub, "test")
	hub.SetBalancer(reg)
	net := Network{Registry: reg, Agent: cluster.NewAgent(store, hub, "8090", "test", func() string { return "" }, nil),
		Certs: certs.New(store, filepath.Join(dir, "certs"), ""), Updater: update.New("", "test"), Version: "test"}
	srv, err := New(store, hub, meter, creds, net)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	hub.Routes(mux)
	reg.Routes(mux)
	srv.Routes(mux)
	return &env{t: t, mux: srv.Gate(mux), store: store}
}

// do makes a request as if it came from ip (with the session, if any).
func (e *env) do(method, path, ip string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = ip + ":40000"
	r.Host = "panel.test"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if e.cook != nil {
		r.AddCookie(e.cook)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

var csrfRe = regexp.MustCompile(`name="csrf" content="([^"]+)"`)

func (e *env) login() {
	w := e.do("POST", "/admin/login", "203.0.113.9", url.Values{"user": {"admin"}, "password": {testPass}}, nil)
	if w.Code != http.StatusSeeOther {
		e.t.Fatalf("login: status %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName {
			e.cook = c
		}
	}
	if e.cook == nil || !e.cook.HttpOnly || e.cook.SameSite != http.SameSiteStrictMode {
		e.t.Fatalf("session cookie missing or insecure: %+v", e.cook)
	}
	page := e.do("GET", "/admin", "203.0.113.9", nil, nil)
	m := csrfRe.FindStringSubmatch(page.Body.String())
	if m == nil {
		e.t.Fatal("CSRF token not found on the page")
	}
	e.csrf = m[1]
}

func TestLoginRequiredAndHeaders(t *testing.T) {
	e := newEnv(t)
	w := e.do("GET", "/admin", "203.0.113.9", nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/login" {
		t.Fatalf("without login it should go to /admin/login, got %d %s", w.Code, w.Header().Get("Location"))
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP missing: %q", csp)
	}
	// with no-referrer the browser sends "Origin: null" on POSTs and the panel would refuse itself
	if rp := w.Header().Get("Referrer-Policy"); rp == "no-referrer" || rp == "" {
		t.Fatalf("Referrer-Policy %q breaks the panel forms", rp)
	}
	if w := e.do("GET", "/admin/api/live", "203.0.113.9", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("API without login: status %d", w.Code)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	bad := url.Values{"user": {"admin"}, "password": {"wrong"}}
	for i := 0; i < 5; i++ {
		if w := e.do("POST", "/admin/login", "198.51.100.7", bad, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i, w.Code)
		}
	}
	ok := url.Values{"user": {"admin"}, "password": {testPass}}
	if w := e.do("POST", "/admin/login", "198.51.100.7", ok, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the IP should be locked out even with the right password, status %d", w.Code)
	}
	if w := e.do("POST", "/admin/login", "198.51.100.8", ok, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("another IP should sign in, status %d", w.Code)
	}
}

func TestAllPagesRender(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, p := range []string{"/admin", "/admin/channels", "/admin/channels/new", "/admin/access",
		"/admin/stats", "/admin/settings", "/admin/security", "/admin/api/live", "/admin/api/stats",
		"/admin/stats.csv", "/admin/static/app.css", "/admin/network", "/admin/api/network", "/admin/server",
		"/admin/api/server", "/admin/static/network.js", "/cluster/install.sh"} {
		if w := e.do("GET", p, "203.0.113.9", nil, nil); w.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", p, w.Code, w.Body.String())
		}
	}
	if w := e.do("GET", "/admin/login", "203.0.113.9", nil, nil); w.Code != http.StatusSeeOther {
		t.Errorf("login while signed in should redirect, status %d", w.Code)
	}
}

func TestCSRFAndOrigin(t *testing.T) {
	e := newEnv(t)
	e.login()
	form := url.Values{"id": {"channel1"}, "url": {"http://provider.test/1.ts"}, "quality": {"original"}}
	if w := e.do("POST", "/admin/channels", "203.0.113.9", form, nil); w.Code != http.StatusForbidden {
		t.Fatalf("without CSRF it should be 403, got %d", w.Code)
	}
	form.Set("csrf", e.csrf)
	if w := e.do("POST", "/admin/channels", "203.0.113.9", form, map[string]string{"Origin": "https://evil.test"}); w.Code != http.StatusForbidden {
		t.Fatalf("another origin should be 403, got %d", w.Code)
	}
	if w := e.do("POST", "/admin/channels", "203.0.113.9", form, map[string]string{"Origin": "http://panel.test"}); w.Code != http.StatusSeeOther {
		t.Fatalf("create channel: status %d: %s", w.Code, w.Body.String())
	}
	if _, ok := e.store.Get().Channels["channel1"]; !ok {
		t.Fatal("channel was not saved")
	}
	// an invalid ID returns to the form with an error
	bad := url.Values{"csrf": {e.csrf}, "id": {"../x"}, "url": {"http://p.test/1.ts"}, "quality": {"original"}}
	if w := e.do("POST", "/admin/channels", "203.0.113.9", bad, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("an invalid ID should give 400, got %d", w.Code)
	}
	// edit and delete
	edit := url.Values{"csrf": {e.csrf}, "name": {"Channel One"}, "url": {"http://provider.test/1.ts"}, "quality": {"720p"}}
	if w := e.do("POST", "/admin/channels/channel1", "203.0.113.9", edit, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("edit: status %d", w.Code)
	}
	if ch := e.store.Get().Channels["channel1"]; ch.Quality != "720p" || ch.Name != "Channel One" {
		t.Fatalf("edit not applied: %+v", ch)
	}
	if w := e.do("POST", "/admin/channels/channel1/delete", "203.0.113.9", url.Values{"csrf": {e.csrf}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("delete: status %d", w.Code)
	}
	if len(e.store.Get().Channels) != 0 {
		t.Fatal("channel was not deleted")
	}
}

func TestCDNAndAllowlist(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.store.Update(func(c *config.Config) error { c.CDNSecret = "cdn-secret-1234567"; return nil })
	if w := e.do("GET", "/admin", "203.0.113.9", nil, map[string]string{"X-CDN-Secret": "cdn-secret-1234567"}); w.Code != http.StatusNotFound {
		t.Fatalf("panel through the CDN should be 404, got %d", w.Code)
	}
	// a list without your own IP cannot be saved
	w := e.do("POST", "/admin/security/ips", "203.0.113.9", url.Values{"csrf": {e.csrf}, "allow": {"10.0.0.0/8"}}, nil)
	if w.Code != http.StatusSeeOther || len(e.store.Get().AdminAllow) != 0 {
		t.Fatalf("a list without your own IP should be refused: %d %v", w.Code, e.store.Get().AdminAllow)
	}
	e.do("POST", "/admin/security/ips", "203.0.113.9", url.Values{"csrf": {e.csrf}, "allow": {"203.0.113.0/24"}}, nil)
	if got := e.store.Get().AdminAllow; len(got) != 1 {
		t.Fatalf("list not saved: %v", got)
	}
	if w := e.do("GET", "/admin/login", "198.51.100.1", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("an IP outside the list should see 404, got %d", w.Code)
	}
}

func TestAccessKeys(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.do("POST", "/admin/access", "203.0.113.9", url.Values{"csrf": {e.csrf}, "name": {"Client"}, "max_screens": {"2"}}, nil)
	keys := e.store.Get().AccessKeys
	if len(keys) != 1 || len(keys[0].Key) < 24 || !keys[0].Enabled {
		t.Fatalf("key not created: %+v", keys)
	}
	key := keys[0].Key
	if w := e.do("GET", "/playlist.m3u8", "198.51.100.1", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("without required keys, the list should open: %d", w.Code)
	}
	e.do("POST", "/admin/access/require", "203.0.113.9", url.Values{"csrf": {e.csrf}, "require_key": {"on"}}, nil)
	if w := e.do("GET", "/playlist.m3u8", "198.51.100.1", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("with keys required, a list without a key should be 403: %d", w.Code)
	}
	if w := e.do("GET", "/live/channel.m3u8", "198.51.100.1", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a channel without a key should be 403: %d", w.Code)
	}
	if w := e.do("GET", "/playlist/"+key+".m3u8", "198.51.100.1", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("a list with the key should open: %d", w.Code)
	}
	if w := e.do("GET", "/playlist/fake-key-1234567.m3u8", "198.51.100.1", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a fake key should be 403: %d", w.Code)
	}
	e.do("POST", "/admin/access/toggle", "203.0.113.9", url.Values{"csrf": {e.csrf}, "key": {key}}, nil)
	if w := e.do("GET", "/playlist/"+key+".m3u8", "198.51.100.1", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a disabled key should be 403: %d", w.Code)
	}
}

func TestPasswordChange(t *testing.T) {
	e := newEnv(t)
	e.login()
	form := url.Values{"csrf": {e.csrf}, "user": {"admin"}, "current": {"wrong"}, "new": {"new-strong-password"}, "confirm": {"new-strong-password"}}
	e.do("POST", "/admin/security/password", "203.0.113.9", form, nil)
	if w := e.do("POST", "/admin/login", "203.0.113.50", url.Values{"user": {"admin"}, "password": {"new-strong-password"}}, nil); w.Code != http.StatusUnauthorized {
		t.Fatal("the password should not change with a wrong current password")
	}
	form.Set("current", testPass)
	e.do("POST", "/admin/security/password", "203.0.113.9", form, nil)
	if w := e.do("POST", "/admin/login", "203.0.113.51", url.Values{"user": {"admin"}, "password": {"new-strong-password"}}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("login with the new password failed: %d", w.Code)
	}
}

func TestNetworkPages(t *testing.T) {
	e := newEnv(t)
	e.login()
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", e.csrf)
		return e.do("POST", path, "203.0.113.9", form, nil)
	}

	// registering a repeater shows the install command with the token
	if w := post("/admin/network/repeaters", url.Values{"name": {"US-East 2"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("register repeater: %d", w.Code)
	}
	reps := e.store.Get().Repeaters
	if len(reps) != 1 || reps[0].Name != "US-East 2" || !reps[0].Enabled {
		t.Fatalf("repeater not registered: %+v", reps)
	}
	page := e.do("GET", "/admin/network", "203.0.113.9", nil, nil).Body.String()
	if !strings.Contains(page, "/cluster/install.sh | bash -s -- --origin http://panel.test --token "+reps[0].Token()) {
		t.Fatalf("install command missing:\n%s", page)
	}

	// an invalid domain is refused; a valid one is saved
	if post("/admin/server/domain", url.Values{"domain": {"not a domain"}}); e.store.Get().Domain != "" {
		t.Fatal("invalid domain accepted")
	}
	post("/admin/server/domain", url.Values{"domain": {"https://Video.Example.com/"}})
	if d := e.store.Get().Domain; d != "video.example.com" {
		t.Fatalf("domain saved wrong: %q", d)
	}

	// becoming a repeater hides channels/access and blocks those pages
	w := post("/admin/network/connect", url.Values{"origin": {"https://primary.example.com"}, "token": {reps[0].Token()}})
	if w.Code != http.StatusSeeOther || !e.store.Get().IsRepeater() {
		t.Fatalf("connect as repeater: %d", w.Code)
	}
	for _, p := range []string{"/admin/channels", "/admin/access", "/admin/settings"} {
		if w := e.do("GET", p, "203.0.113.9", nil, nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/network" {
			t.Errorf("%s on a repeater should go to /admin/network: %d", p, w.Code)
		}
	}
	if page := e.do("GET", "/admin", "203.0.113.9", nil, nil).Body.String(); strings.Contains(page, `href="/admin/channels"`) {
		t.Error("the repeater menu should not show Channels")
	}
	if w := e.do("GET", "/admin/network", "203.0.113.9", nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Link to the primary") {
		t.Errorf("repeater Network page: %d", w.Code)
	}
	post("/admin/network/disconnect", url.Values{})
	if e.store.Get().IsRepeater() {
		t.Fatal("disconnect did not turn it back into a primary")
	}
}

func TestSecretPanelAddress(t *testing.T) {
	e := newEnv(t)
	const token = "Secret-Panel-Token-123"
	e.store.Update(func(c *config.Config) error { c.PanelToken = token; return nil })

	// the default address and wrong tokens look like there is no panel
	for _, p := range []string{"/admin", "/admin/login", "/admin/static/app.css", "/wrong-token-123456/admin/login", "/"} {
		if w := e.do("GET", p, "203.0.113.9", nil, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, w.Code)
		}
	}
	// streams keep working at their usual paths
	if w := e.do("GET", "/healthz", "203.0.113.9", nil, nil); w.Code != http.StatusOK {
		t.Errorf("/healthz: %d", w.Code)
	}

	base := "/" + token + "/admin"
	page := e.do("GET", base+"/login", "203.0.113.9", nil, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="`+base+`/login"`) {
		t.Fatalf("login page at the secret address: %d", page.Code)
	}
	w := e.do("POST", base+"/login", "203.0.113.9", url.Values{"user": {"admin"}, "password": {testPass}}, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != base {
		t.Fatalf("login: %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName {
			e.cook = c
		}
	}
	if e.cook == nil || e.cook.Path != base {
		t.Fatalf("the session cookie should be limited to %s: %+v", base, e.cook)
	}
	dash := e.do("GET", base, "203.0.113.9", nil, nil).Body.String()
	if !strings.Contains(dash, `href="`+base+`/channels"`) || strings.Contains(dash, `href="/admin`) {
		t.Fatal("panel links should carry the secret address")
	}
	// rotating the address: the old one stops working
	m := csrfRe.FindStringSubmatch(dash)
	w = e.do("POST", base+"/security/address", "203.0.113.9", url.Values{"csrf": {m[1]}}, nil)
	newToken := e.store.Get().PanelToken
	if w.Code != http.StatusSeeOther || newToken == token || w.Header().Get("Location") != "/"+newToken+"/admin/login" {
		t.Fatalf("rotate address: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := e.do("GET", base+"/login", "203.0.113.9", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("the old address should be gone: %d", w.Code)
	}
}

func TestLanguageSwitch(t *testing.T) {
	e := newEnv(t)
	e.login()
	if page := e.do("GET", "/admin", "203.0.113.9", nil, nil).Body.String(); !strings.Contains(page, ">Dashboard</h1>") {
		t.Fatal("English should be the default")
	}

	w := e.do("GET", "/admin/lang?l=pt-BR&next=/admin/channels", "203.0.113.9", nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/channels" {
		t.Fatalf("switch language: %d %s", w.Code, w.Header().Get("Location"))
	}
	var lang *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == langCookie {
			lang = c
		}
	}
	if lang == nil || lang.Value != "pt-BR" {
		t.Fatalf("language cookie: %+v", lang)
	}
	// do() sends only the session cookie: send both
	get := func(path string) string {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr, r.Host = "203.0.113.9:40000", "panel.test"
		r.AddCookie(e.cook)
		r.AddCookie(lang)
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, r)
		return rec.Body.String()
	}
	page := get("/admin")
	if !strings.Contains(page, `<html lang="pt-BR">`) || !strings.Contains(page, ">Painel</h1>") || !strings.Contains(page, ">Canais</a>") {
		t.Fatal("panel should be in Portuguese")
	}
	if !strings.Contains(page, `id="i18n"`) {
		t.Fatal("the JavaScript translations should be on the page")
	}

	// flash messages and validation errors come translated
	post := func(path string, form url.Values) {
		form.Set("csrf", e.csrf)
		r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		r.RemoteAddr, r.Host = "203.0.113.9:40000", "panel.test"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(e.cook)
		r.AddCookie(lang)
		e.mux.ServeHTTP(httptest.NewRecorder(), r)
	}
	post("/admin/channels", url.Values{"id": {"c1"}, "url": {"http://p.test/1.ts"}, "quality": {"original"}})
	if page := get("/admin/channels"); !strings.Contains(page, "Canal c1 criado.") {
		t.Error("success message should be in Portuguese")
	}
	post("/admin/server/domain", url.Values{"domain": {"not a domain"}})
	if page := get("/admin/server"); !strings.Contains(page, "domínio inválido") {
		t.Error("validation error should be in Portuguese")
	}

	// the language menu never redirects outside the panel
	if w := e.do("GET", "/admin/lang?l=en&next=https://evil.test/", "203.0.113.9", nil, nil); w.Header().Get("Location") != "/admin" {
		t.Errorf("open redirect: %s", w.Header().Get("Location"))
	}
}
