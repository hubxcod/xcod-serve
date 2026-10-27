package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	CookieName  = "xcod_session"
	idleTimeout = 2 * time.Hour  // unused for this long: signed out
	maxLifetime = 12 * time.Hour // even when in use, asks for login again
)

// Session is an active panel login. It lives only in memory: restarting xcod requires a new
// login.
type Session struct {
	ID       string // public identifier (hash of the token), used to list and end sessions
	token    string
	User     string
	CSRF     string
	IP       string
	Agent    string
	Created  time.Time
	LastSeen time.Time
	flash    *Flash
}

// Flash is a message shown once on the next page.
type Flash struct {
	Kind string // "ok" or "err"
	Text string
}

type Sessions struct {
	mu   sync.Mutex
	byID map[string]*Session // token -> session
}

func NewSessions() *Sessions { return &Sessions{byID: map[string]*Session{}} }

// Create opens a session and sets the cookie.
func (s *Sessions) Create(w http.ResponseWriter, user, ip, agent string, secure bool, path string) *Session {
	token := randomToken(32)
	sum := sha256.Sum256([]byte(token))
	now := time.Now()
	sess := &Session{ID: hex.EncodeToString(sum[:8]), token: token, User: user, CSRF: randomToken(32),
		IP: ip, Agent: agent, Created: now, LastSeen: now}
	s.mu.Lock()
	s.byID[token] = sess
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: token, Path: path, HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: int(maxLifetime.Seconds())})
	return sess
}

// Get returns the request's session (and refreshes its last use), or nil.
func (s *Sessions) Get(r *http.Request) *Session {
	ck, err := r.Cookie(CookieName)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.byID[ck.Value]
	if sess == nil {
		return nil
	}
	now := time.Now()
	if now.Sub(sess.LastSeen) > idleTimeout || now.Sub(sess.Created) > maxLifetime {
		delete(s.byID, ck.Value)
		return nil
	}
	sess.LastSeen = now
	return sess
}

// Destroy ends the request's session and deletes the cookie.
func (s *Sessions) Destroy(w http.ResponseWriter, r *http.Request, path string) {
	if ck, err := r.Cookie(CookieName); err == nil {
		s.mu.Lock()
		delete(s.byID, ck.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: path, MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode})
}

// DestroyOthers ends every session except keep (password change, "sign out other devices").
func (s *Sessions) DestroyOthers(keep *Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for t, sess := range s.byID {
		if sess != keep {
			delete(s.byID, t)
			n++
		}
	}
	return n
}

// DestroyID ends the session with this public ID.
func (s *Sessions) DestroyID(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, sess := range s.byID {
		if sess.ID == id {
			delete(s.byID, t)
			return true
		}
	}
	return false
}

// List returns copies of the active sessions, most recent first.
func (s *Sessions) List() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := []Session{}
	for t, sess := range s.byID {
		if now.Sub(sess.LastSeen) > idleTimeout || now.Sub(sess.Created) > maxLifetime {
			delete(s.byID, t)
			continue
		}
		cp := *sess
		cp.token, cp.CSRF, cp.flash = "", "", nil
		out = append(out, cp)
	}
	slices.SortFunc(out, func(a, b Session) int { return b.LastSeen.Compare(a.LastSeen) })
	return out
}

// Rename changes the user shown for the session (after the panel user is renamed).
func (s *Sessions) Rename(sess *Session, user string) {
	s.mu.Lock()
	sess.User = user
	s.mu.Unlock()
}

// SetFlash stores a message for the next page.
func (s *Sessions) SetFlash(sess *Session, kind, text string) {
	s.mu.Lock()
	sess.flash = &Flash{Kind: kind, Text: text}
	s.mu.Unlock()
}

// TakeFlash returns and clears the pending message.
func (s *Sessions) TakeFlash(sess *Session) *Flash {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.flash
	sess.flash = nil
	return f
}
