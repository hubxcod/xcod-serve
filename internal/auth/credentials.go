// Package auth handles the panel login: hashed credentials, sessions and login rate limiting.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"xcod/internal/i18n"
)

const (
	iterations = 310_000 // PBKDF2-SHA256 (OWASP recommendation)
	MinPassLen = 10
)

// Credentials is the panel user. The password is stored only as a hash in admin.json.
type Credentials struct {
	path string

	mu   sync.RWMutex
	data credFile
}

type credFile struct {
	User    string    `json:"user"`
	Salt    string    `json:"salt"`
	Hash    string    `json:"hash"`
	Iter    int       `json:"iter"`
	Changed time.Time `json:"changed"`
}

// LoadCredentials reads admin.json. If it does not exist (or on reset), it is created from
// user/pass (environment variables); without a password, a random one is generated and
// returned in generated, to be shown once in the log.
func LoadCredentials(path, user, pass string, reset bool) (c *Credentials, generated string, err error) {
	c = &Credentials{path: path}
	if !reset {
		raw, err := os.ReadFile(path)
		if err == nil {
			if err := json.Unmarshal(raw, &c.data); err != nil || c.data.Hash == "" {
				return nil, "", errors.New("invalid " + path + " (delete it or use XCOD_RESET_ADMIN=1)")
			}
			return c, "", nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
	}
	if user == "" {
		user = "admin"
	}
	if pass == "" {
		generated = randomToken(12)
		pass = generated
	}
	if err := c.Set(user, pass); err != nil {
		return nil, "", err
	}
	return c, generated, nil
}

func (c *Credentials) Username() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data.User
}

func (c *Credentials) Changed() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data.Changed
}

// Verify checks user and password in constant time.
func (c *Credentials) Verify(user, pass string) bool {
	c.mu.RLock()
	d := c.data
	c.mu.RUnlock()
	salt, err1 := base64.RawStdEncoding.DecodeString(d.Salt)
	want, err2 := base64.RawStdEncoding.DecodeString(d.Hash)
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pass, salt, d.Iter, len(want))
	if err != nil {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(d.User))
	return subtle.ConstantTimeCompare(got, want)&userOK == 1
}

// Set changes user and password and writes admin.json (readable only by the owner).
func (c *Credentials) Set(user, pass string) error {
	if utf8.RuneCountInString(pass) < MinPassLen {
		return i18n.Errorf("the password must have at least 10 characters")
	}
	if user == "" || len(user) > 64 {
		return i18n.Errorf("invalid username")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	hash, err := pbkdf2.Key(sha256.New, pass, salt, iterations, 32)
	if err != nil {
		return err
	}
	d := credFile{User: user, Salt: base64.RawStdEncoding.EncodeToString(salt),
		Hash: base64.RawStdEncoding.EncodeToString(hash), Iter: iterations, Changed: time.Now()}
	raw, _ := json.MarshalIndent(d, "", "  ")
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return err
	}
	c.mu.Lock()
	c.data = d
	c.mu.Unlock()
	return nil
}

// randomToken returns a URL-safe random string with n bytes of entropy.
func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomKey returns an access key or secret (24 bytes of entropy).
func RandomKey() string { return randomToken(24) }
