// Package certs issues and renews the HTTPS certificate for the domain set in the panel
// (Let's Encrypt, via ACME). The challenge is answered on port 80 (HTTP-01) or on 443 itself
// (TLS-ALPN-01).
package certs

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"xcod/internal/config"
	"xcod/internal/i18n"
)

type Manager struct {
	store   *config.Store
	m       *autocert.Manager
	httpsOn string // port 443 address ("" = HTTPS off)

	mu     sync.Mutex
	status Status
}

// Status is the certificate state shown in the panel.
type Status struct {
	Domain   string    `json:"domain"`
	OK       bool      `json:"ok"`
	Expires  time.Time `json:"expires"`
	Err      string    `json:"err"`
	Checking bool      `json:"checking"`
	DNS      []string  `json:"dns"` // where the domain points
	err      error     // Err, translatable
}

// Localize returns the status with the error message in the given language.
func (s Status) Localize(lang string) Status {
	if s.err != nil {
		s.Err = i18n.Text(lang, s.err)
	}
	return s
}

// New sets up the issuer. An empty httpsListen means no port 443 (no certificate).
func New(store *config.Store, cacheDir, httpsListen string) *Manager {
	m := &Manager{store: store, httpsOn: httpsListen}
	m.m = &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Cache:  autocert.DirCache(cacheDir),
		// only issue for the panel's domain: nobody can make the server request certificates
		// for other names
		HostPolicy: func(_ context.Context, host string) error {
			if d := store.Get().Domain; d != "" && host == d {
				return nil
			}
			return fmt.Errorf("domain %q not configured", host)
		},
	}
	return m
}

// Enabled reports whether this server serves HTTPS (ports 80/443 configured).
func (m *Manager) Enabled() bool { return m.httpsOn != "" }

// HTTPHandler answers the ACME challenge on port 80 and passes everything else to next.
func (m *Manager) HTTPHandler(next http.Handler) http.Handler { return m.m.HTTPHandler(next) }

// TLSConfig is the configuration for port 443.
func (m *Manager) TLSConfig() *tls.Config {
	c := m.m.TLSConfig()
	c.MinVersion = tls.VersionTLS12
	return c
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.DNS = append([]string(nil), s.DNS...)
	return s
}

// Ready reports whether the domain already has a valid certificate (the panel then requires
// HTTPS).
func (m *Manager) Ready() bool {
	s := m.Status()
	d := m.store.Get().Domain
	return d != "" && s.OK && s.Domain == d && time.Now().Before(s.Expires)
}

// URL is this server's HTTPS address, once ready.
func (m *Manager) URL() string {
	if m.Ready() {
		return "https://" + m.store.Get().Domain
	}
	return ""
}

// Check issues (or checks) the certificate for the configured domain, in the background.
func (m *Manager) Check() {
	d := m.store.Get().Domain
	m.mu.Lock()
	if m.status.Checking {
		m.mu.Unlock()
		return
	}
	m.status = Status{Domain: d, Checking: d != ""}
	m.mu.Unlock()
	if d == "" {
		return
	}
	go func() {
		ips, _ := net.LookupHost(d)
		exp, err := m.issue(d)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.status = Status{Domain: d, DNS: ips, OK: err == nil, Expires: exp}
		if err != nil {
			m.status.Err, m.status.err = err.Error(), err
			log.Printf("https: %s: %v", d, err)
			return
		}
		log.Printf("https: certificate for %s valid until %s", d, exp.Format("2006-01-02"))
	}()
}

// issue asks the issuer for the certificate the way a browser would: it is issued the first
// time (up to a few minutes) and read from the cache afterwards. Returns the expiry.
func (m *Manager) issue(domain string) (time.Time, error) {
	if !m.Enabled() {
		return time.Time{}, i18n.Errorf("HTTPS is off on this server (install without --no-https and open ports 80 and 443)")
	}
	cert, err := m.m.GetCertificate(&tls.ClientHelloInfo{ServerName: domain,
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}}) // ask for the ECDSA certificate (the lightest)
	if err != nil {
		return time.Time{}, i18n.Errorf("could not issue the certificate: check that the DNS for %s points to this server and that ports 80 and 443 are open (%v)", domain, err)
	}
	if cert.Leaf == nil {
		return time.Time{}, i18n.Errorf("empty certificate")
	}
	return cert.Leaf.NotAfter, nil
}

// Watch re-checks the certificate every day (autocert renews it 30 days before expiry).
func (m *Manager) Watch(stop <-chan struct{}) {
	m.Check()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.Check()
		}
	}
}
