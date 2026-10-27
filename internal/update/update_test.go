package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeBinary pretends to be an executable (the content does not matter: the version comes
// from the fake check).
var fakeBinary = bytes.Repeat([]byte("x"), 2<<20)

func newTest(t *testing.T) *Updater {
	u := New(t.TempDir(), "v1.0.0")
	u.check = func(string) (string, error) { return "v1.1.0", nil }
	return u
}

func shaOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestStage(t *testing.T) {
	u := newTest(t)
	v, err := u.Stage(bytes.NewReader(fakeBinary), shaOf(fakeBinary))
	if err != nil || v != "v1.1.0" {
		t.Fatalf("stage: %v %v", v, err)
	}
	for _, f := range []string{"xcod.new", "ready"} {
		if _, err := os.Stat(filepath.Join(u.dir, f)); err != nil {
			t.Errorf("%s was not created", f)
		}
	}
	if u.Pending() != "v1.1.0" {
		t.Error("should stay pending until the installer answers")
	}
	if _, err := u.Stage(bytes.NewReader(fakeBinary), ""); err == nil {
		t.Error("a second update in progress should be refused")
	}
	// the installer answered without restarting (it refused): the pending state ends
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(u.dir, "result"), []byte("error: test\n"), 0o600)
	if u.Pending() != "" {
		t.Error("pending state should end with the installer's result")
	}
	if msg, _ := u.Result(); msg != "error: test" {
		t.Errorf("result: %q", msg)
	}
}

func TestStageRejects(t *testing.T) {
	cases := map[string]struct {
		data []byte
		sha  string
	}{
		"different SHA": {fakeBinary, strings.Repeat("0", 64)},
		"too small":     {[]byte("hi"), ""},
	}
	for name, c := range cases {
		u := newTest(t)
		if _, err := u.Stage(bytes.NewReader(c.data), c.sha); err == nil {
			t.Errorf("%s: should be refused", name)
		}
		if _, err := os.Stat(filepath.Join(u.dir, "ready")); err == nil {
			t.Errorf("%s: should not trigger the installer", name)
		}
	}
	u := newTest(t)
	u.check = func(string) (string, error) { return "", fmt.Errorf("not xcod") }
	if _, err := u.Stage(bytes.NewReader(fakeBinary), ""); err == nil {
		t.Error("a file that does not run should be refused")
	}
	if _, err := New("", "v1").Stage(bytes.NewReader(fakeBinary), ""); err == nil {
		t.Error("without an update directory it should refuse")
	}
}

func TestGitHub(t *testing.T) {
	asset := "xcod-linux-" + runtime.GOARCH
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/xcod/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.1.0","name":"v1.1.0","body":"what's new","html_url":"https://github.com/owner/xcod/releases/v1.1.0",
				"published_at":"2026-10-27T12:00:00Z","assets":[{"name":%q,"browser_download_url":"%s/bin"},{"name":"SHA256SUMS","browser_download_url":"%s/sums"}]}`,
				asset, srv.URL, srv.URL)
		case "/sums":
			fmt.Fprintf(w, "%s  %s\n%s  xcod-linux-other\n", shaOf(fakeBinary), asset, strings.Repeat("a", 64))
		case "/bin":
			w.Write(fakeBinary)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	g := NewReleases("owner/xcod")
	g.api = srv.URL
	rel, _, err := g.Latest(context.Background(), time.Hour)
	if err != nil || rel.Tag != "v1.1.0" || rel.sha != shaOf(fakeBinary) {
		t.Fatalf("latest: %+v %v", rel, err)
	}
	if !rel.Newer("v1.0.0") || rel.Newer("v1.1.0") || rel.Newer("dev") {
		t.Error("wrong version comparison")
	}
	u := newTest(t)
	if v, err := u.Install(context.Background(), rel); err != nil || v != "v1.1.0" {
		t.Fatalf("install: %v %v", v, err)
	}

	g2 := NewReleases("owner/none")
	g2.api = srv.URL
	if _, _, err := g2.Latest(context.Background(), time.Hour); err == nil || !strings.Contains(err.Error(), "no release") {
		t.Errorf("repository without releases: %v", err)
	}
}
