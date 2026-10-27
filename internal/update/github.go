package update

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"xcod/internal/i18n"
)

// DefaultRepo is where xcod releases are published (GitHub Releases, built by release.yml).
const DefaultRepo = "hubxcod/xcod-serve"

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Release is a published version.
type Release struct {
	Tag       string    `json:"tag"`
	Name      string    `json:"name"`
	Notes     string    `json:"notes"`
	URL       string    `json:"url"` // release page
	Published time.Time `json:"published"`
	asset     string    // download URL of the binary for this architecture
	sha       string    // its SHA-256, read from the release's SHA256SUMS
}

// Releases queries the versions on GitHub (cached, to stay within the API rate limit).
type Releases struct {
	repo   string
	client *http.Client
	api    string // https://api.github.com (replaced in tests)

	mu      sync.Mutex
	latest  Release
	checked time.Time
	err     error
}

func NewReleases(repo string) *Releases {
	if repo == "" {
		repo = DefaultRepo
	}
	return &Releases{repo: repo, client: &http.Client{Timeout: 30 * time.Second}, api: "https://api.github.com"}
}

func (g *Releases) Repo() string { return g.repo }

// Cached returns the last result without querying GitHub.
func (g *Releases) Cached() Release {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.latest
}

// Watch checks GitHub every 6 hours (the panel shows a notice when a new version is out).
func (g *Releases) Watch(stop <-chan struct{}, current string) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if rel, _, err := g.Latest(ctx, time.Minute); err == nil && rel.Newer(current) {
			log.Printf("update: version %s available (running %s): update it under Server", rel.Tag, current)
		}
		cancel()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// Latest returns the latest version; it queries GitHub at most once every maxAge.
func (g *Releases) Latest(ctx context.Context, maxAge time.Duration) (Release, time.Time, error) {
	g.mu.Lock()
	if !g.checked.IsZero() && time.Since(g.checked) < maxAge {
		defer g.mu.Unlock()
		return g.latest, g.checked, g.err
	}
	g.mu.Unlock()

	rel, err := g.fetch(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checked, g.err = time.Now(), err
	if err == nil {
		g.latest = rel
	}
	return g.latest, g.checked, g.err
}

func (g *Releases) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "xcod-updater")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, i18n.Errorf("cannot reach GitHub: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, i18n.Errorf("no release published yet at github.com/%s", g.repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, i18n.Errorf("GitHub answered %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (g *Releases) fetch(ctx context.Context) (Release, error) {
	if !repoRe.MatchString(g.repo) {
		return Release{}, i18n.Errorf("invalid repository: %q", g.repo)
	}
	raw, err := g.get(ctx, g.api+"/repos/"+g.repo+"/releases/latest", 1<<20)
	if err != nil {
		return Release{}, err
	}
	var r struct {
		Tag       string    `json:"tag_name"`
		Name      string    `json:"name"`
		Body      string    `json:"body"`
		HTMLURL   string    `json:"html_url"`
		Published time.Time `json:"published_at"`
		Assets    []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Release{}, i18n.Errorf("invalid GitHub response: %v", err)
	}
	rel := Release{Tag: r.Tag, Name: r.Name, Notes: r.Body, URL: r.HTMLURL, Published: r.Published}
	want, sums := "xcod-linux-"+runtime.GOARCH, ""
	for _, a := range r.Assets {
		switch a.Name {
		case want:
			rel.asset = a.URL
		case "SHA256SUMS":
			sums = a.URL
		}
	}
	if rel.asset == "" || sums == "" {
		return rel, i18n.Errorf("release %s does not have %s and SHA256SUMS", r.Tag, want)
	}
	list, err := g.get(ctx, sums, 64<<10)
	if err != nil {
		return rel, err
	}
	sc := bufio.NewScanner(strings.NewReader(string(list)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == want && len(f[0]) == 64 {
			rel.sha = strings.ToLower(f[0])
		}
	}
	if rel.sha == "" {
		return rel, i18n.Errorf("SHA256SUMS of release %s does not list %s", r.Tag, want)
	}
	return rel, nil
}

// Newer reports whether the published version differs from the running one (development
// builds are never offered for automatic replacement).
func (r Release) Newer(current string) bool {
	return r.Tag != "" && r.Tag != current && current != "dev"
}

// Install downloads the version, verifies its SHA-256 and leaves it ready for the system
// installer.
func (u *Updater) Install(ctx context.Context, rel Release) (string, error) {
	if rel.asset == "" || rel.sha == "" {
		return "", i18n.Errorf("release has no binary for this server")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.asset, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "xcod-updater")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", i18n.Errorf("cannot reach GitHub: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", i18n.Errorf("download answered %s", resp.Status)
	}
	return u.Stage(resp.Body, rel.sha)
}
