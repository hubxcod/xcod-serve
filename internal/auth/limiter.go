package auth

import (
	"sync"
	"time"
)

// Limiter locks out an IP after too many wrong passwords in a row.
type Limiter struct {
	max    int
	window time.Duration

	mu    sync.Mutex
	fails map[string]*attempts
}

type attempts struct {
	n     int
	first time.Time
	until time.Time // locked out until
}

// NewLimiter: max failures within window lock the IP out for window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, fails: map[string]*attempts{}}
}

// Blocked reports whether the IP is locked out and for how much longer.
func (l *Limiter) Blocked(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.fails[ip]
	if a == nil {
		return false, 0
	}
	if left := time.Until(a.until); left > 0 {
		return true, left
	}
	return false, 0
}

// Fail records a wrong password; it returns true if the IP has just been locked out.
func (l *Limiter) Fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, a := range l.fails { // cleanup
		if now.Sub(a.first) > l.window && now.After(a.until) {
			delete(l.fails, k)
		}
	}
	a := l.fails[ip]
	if a == nil || now.Sub(a.first) > l.window {
		a = &attempts{first: now}
		l.fails[ip] = a
	}
	a.n++
	if a.n >= l.max {
		a.until = now.Add(l.window)
		a.n, a.first = 0, now
		return true
	}
	return false
}

// Success clears the IP's failures.
func (l *Limiter) Success(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}
