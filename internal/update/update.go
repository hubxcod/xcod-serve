// Package update prepares the swap of the xcod binary.
//
// The xcod service cannot change its own binary (/opt/xcod is read-only for it). It only
// leaves the verified new version in XCOD_UPDATE_DIR and creates the "ready" file; the
// xcod-update service (systemd, root, installed by install.sh) installs it, restarts and, if
// the new version does not answer, rolls back to the previous one. The outcome is written to
// "result".
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xcod/internal/i18n"
)

// MaxSize limits the received binary.
const MaxSize = 100 << 20

type Updater struct {
	dir     string
	version string
	check   func(path string) (string, error) // runs "-version" on the new binary

	mu       sync.Mutex
	pending  string // staged version, waiting for the installer
	stagedAt time.Time
}

// New sets up the updater. An empty dir turns panel updates off (running outside the
// installed service, e.g. in Docker or during development).
func New(dir, version string) *Updater {
	return &Updater{dir: dir, version: version, check: versionOf}
}

func (u *Updater) Enabled() bool { return u.dir != "" }

// Version is the running version.
func (u *Updater) Version() string { return u.version }

func versionOf(path string) (string, error) {
	out, err := exec.Command(path, "-version").Output()
	if err != nil {
		return "", i18n.Errorf("the file is not an xcod binary for this server (%v)", err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" || len(v) > 64 || strings.ContainsAny(v, " \n") {
		return "", i18n.Errorf("the file did not report a valid version")
	}
	return v, nil
}

// Stage writes the received binary, verifies it (hash, if given, and version) and leaves it
// ready for the installer. Returns the new version.
func (u *Updater) Stage(r io.Reader, wantSHA string) (string, error) {
	if !u.Enabled() {
		return "", i18n.Errorf("updates unavailable: xcod was not installed with install.sh")
	}
	if p := u.Pending(); p != "" {
		return "", i18n.Errorf("an update to %s is already in progress", p)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pending != "" {
		return "", i18n.Errorf("an update to %s is already in progress", u.pending)
	}
	tmp := filepath.Join(u.dir, "xcod.tmp")
	dst := filepath.Join(u.dir, "xcod.new")
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, MaxSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	fail := func(e error) (string, error) { os.Remove(tmp); return "", e }
	switch {
	case err != nil:
		return fail(i18n.Errorf("could not receive the file: %v", err))
	case n > MaxSize:
		return fail(i18n.Errorf("file too large"))
	case n < 1<<20:
		return fail(i18n.Errorf("file too small to be xcod"))
	case wantSHA != "" && hex.EncodeToString(h.Sum(nil)) != wantSHA:
		return fail(i18n.Errorf("the downloaded file does not match the expected SHA-256"))
	}
	v, err := u.check(tmp)
	if err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fail(err)
	}
	// "ready" goes last: it is what triggers the installer
	if err := os.WriteFile(filepath.Join(u.dir, "ready"), []byte(v+"\n"), 0o600); err != nil {
		os.Remove(dst)
		return "", err
	}
	u.pending, u.stagedAt = v, time.Now()
	return v, nil
}

// Pending is the staged version not installed yet ("" = none). If the installer already
// answered (and did not restart this process, i.e. it refused), the pending state ends.
func (u *Updater) Pending() string {
	u.mu.Lock()
	p, at := u.pending, u.stagedAt
	u.mu.Unlock()
	if p == "" {
		return ""
	}
	if _, done := u.Result(); done.After(at) {
		u.mu.Lock()
		u.pending = ""
		u.mu.Unlock()
		return ""
	}
	return p
}

// Result is what the installer recorded about the last update ("" = none).
func (u *Updater) Result() (msg string, at time.Time) {
	if !u.Enabled() {
		return "", time.Time{}
	}
	path := filepath.Join(u.dir, "result")
	st, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}
	}
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b)), st.ModTime()
}

var (
	selfOnce sync.Once
	selfSHA  string
)

// SelfSHA is the SHA-256 of the running binary (repeaters compare it with the primary's).
func SelfSHA() string {
	selfOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			selfSHA = hex.EncodeToString(h.Sum(nil))
		}
	})
	return selfSHA
}
