package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"xcod/internal/auth"
	"xcod/internal/i18n"
)

// views holds one template per page, each combined with the layout.
type views struct {
	pages map[string]*template.Template
}

// staticVersion is a fingerprint of the files in static/: it changes with every panel
// version, so the browser fetches the new CSS/JS right away (and may cache it otherwise).
var staticVersion = func() string {
	h := sha256.New()
	fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := staticFS.ReadFile(path)
			h.Write([]byte(path))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

var funcs = template.FuncMap{
	"asset": func(name string) string { return "/admin/static/" + name + "?v=" + staticVersion },
	"bytes": formatBytes,
	"date":  func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	// replaced on every render by langFuncs (the page's language)
	"t":   func(s string) string { return s },
	"tf":  func(format string, args ...any) string { return fmt.Sprintf(format, args...) },
	"ago": func(t time.Time) string { return "" },
	"initial": func(s string) string { // first letter, for the menu avatar
		for _, r := range s {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"mask": func(s string) string { // shows only the start of a secret
		if len(s) <= 6 {
			return "••••"
		}
		return s[:6] + "…"
	},
}

func loadViews() (*views, error) {
	v := &views{pages: map[string]*template.Template{}}
	files, err := fs.Glob(viewsFS, "views/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(f, "views/"), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(viewsFS, "views/layout.html", f)
		if err != nil {
			return nil, fmt.Errorf("view %s: %v", name, err)
		}
		v.pages[name] = t
	}
	return v, nil
}

// langFuncs are the template functions that depend on the page's language.
func langFuncs(lang string) template.FuncMap {
	return template.FuncMap{
		"t":  func(s string) string { return i18n.T(lang, s) },
		"tf": func(format string, args ...any) string { return i18n.Sprintf(lang, format, args...) },
		"ago": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return i18n.T(lang, "just now")
			case d < time.Hour:
				return i18n.Sprintf(lang, "%d min ago", int(d.Minutes()))
			default:
				return i18n.Sprintf(lang, "%d h ago", int(d.Hours()))
			}
		},
	}
}

// langCookie remembers the panel language chosen by the viewer.
const langCookie = "xcod_lang"

// lang is the request's panel language (chosen in the menu; English by default).
func (s *Server) lang(r *http.Request) string {
	if c, err := r.Cookie(langCookie); err == nil {
		return i18n.Supported(c.Value)
	}
	return i18n.Default
}

// tr translates a format for the request's language.
func (s *Server) tr(r *http.Request, format string, args ...any) string {
	return i18n.Sprintf(s.lang(r), format, args...)
}

// setLang saves the chosen language and goes back to the page the viewer was on.
func (s *Server) setLang(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: langCookie, Value: i18n.Supported(r.URL.Query().Get("l")), Path: s.cookiePath(),
		MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.https(r)})
	next := r.URL.Query().Get("next")
	if p := s.to("/admin"); next != p && !strings.HasPrefix(next, p+"/") { // only back into the panel
		next = p
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// page is what every view receives.
type page struct {
	Title string
	Nav   string // active menu item
	User  string
	CSRF  string
	Flash *auth.Flash
	Data  any

	Repeater bool   // repeater server: the menu hides what comes from the primary
	NewVer   string // new version on GitHub (primary with updates installed only)
	Version  string // running version (menu footer)
	P        string // secret path in front of /admin (see gate.go)
	Lang     string // page language (i18n code)
	Langs    []i18n.Lang
	Here     string            // current address, to come back after switching language
	JSText   map[string]string // translations for the panel's JavaScript (nil = English)
}

// render draws the view with the layout. It renders into a buffer first, so a template error
// does not leave the page half drawn.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	lang := s.lang(r)
	p := page{Title: i18n.T(lang, title), Nav: name, Data: data, Repeater: s.store.Get().IsRepeater(), Version: s.net.Version,
		P: s.prefix(), Lang: lang, Langs: i18n.Langs, Here: s.to(r.URL.Path)}
	if lang != i18n.Default {
		p.JSText = i18n.Catalog(lang)
	}
	if s.net.Releases != nil && s.net.Updater.Enabled() && !p.Repeater && name != "server" {
		if rel := s.net.Releases.Cached(); rel.Newer(s.net.Version) {
			p.NewVer = rel.Tag
		}
	}
	if sess := session(r); sess != nil {
		p.User, p.CSRF, p.Flash = sess.User, sess.CSRF, s.sess.TakeFlash(sess)
	}
	t := s.views.pages[name]
	if t == nil {
		http.Error(w, "page not found", http.StatusInternalServerError)
		return
	}
	t, err := t.Clone()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	t.Funcs(langFuncs(lang))
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		log.Printf("admin: error rendering %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// redirect goes back to a page with a translated message (POST → redirect → GET).
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to, kind, format string, args ...any) {
	if sess := session(r); sess != nil && format != "" {
		s.sess.SetFlash(sess, kind, s.tr(r, format, args...))
	}
	http.Redirect(w, r, s.to(to), http.StatusSeeOther)
}

// fail goes back to a page showing err, translated.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, to string, err error) {
	if sess := session(r); sess != nil {
		s.sess.SetFlash(sess, "err", i18n.Text(s.lang(r), err))
	}
	http.Redirect(w, r, s.to(to), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func formatBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// baseURL is the server's address as the browser sees it.
func (s *Server) baseURL(r *http.Request) string {
	if s.https(r) {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}
