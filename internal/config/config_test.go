package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOldChannelFormat(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"channels":{"846":"http://p.test/846"}}`), &c); err != nil {
		t.Fatal(err)
	}
	if ch := c.Channels["846"]; ch.URL != "http://p.test/846" || ch.Quality != "original" {
		t.Fatalf("old format not converted: %+v", ch)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Config){
		"invalid id":        func(c *Config) { c.Channels["a/b"] = Channel{URL: "http://x/1"} },
		"url without http":  func(c *Config) { c.Channels["a"] = Channel{URL: "ftp://x/1"} },
		"quality":           func(c *Config) { c.Channels["a"] = Channel{URL: "http://x/1", Quality: "8k"} },
		"name with newline": func(c *Config) { c.Channels["a"] = Channel{URL: "http://x/1", Name: "a\nb"} },
		"short secret":      func(c *Config) { c.CDNSecret = "short" },
		"short key":         func(c *Config) { c.AccessKeys = []AccessKey{{Name: "x", Key: "abc"}} },
		"duplicate key": func(c *Config) {
			c.AccessKeys = []AccessKey{{Name: "a", Key: "abcdefghijklmn"}, {Name: "b", Key: "abcdefghijklmn"}}
		},
		"invalid network": func(c *Config) { c.AdminAllow = []string{"300.1.1.1"} },
	}
	for name, mut := range cases {
		c := Default()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	c := Default()
	c.Channels["ok"] = Channel{URL: "https://x/1.ts"}
	if err := c.Validate(); err != nil || c.Channels["ok"].Quality != "original" {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestKeyValidity(t *testing.T) {
	now := time.Date(2026, 10, 21, 23, 0, 0, 0, time.Local)
	if !(AccessKey{Enabled: true, Expires: "2026-10-21"}).Valid(now) {
		t.Error("key should be valid until the end of its expiry day")
	}
	if (AccessKey{Enabled: true, Expires: "2026-10-20"}).Valid(now) {
		t.Error("expired key accepted")
	}
	if (AccessKey{Enabled: false}).Valid(now) {
		t.Error("disabled key accepted")
	}
}

func TestAdminAllowed(t *testing.T) {
	c := Default()
	c.AdminAllow = []string{"10.0.0.0/8", "203.0.113.5"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{"10.1.2.3": true, "203.0.113.5": true, "203.0.113.6": false} {
		if got := c.AdminAllowed(parseIP(ip)); got != want {
			t.Errorf("%s: %v, want %v", ip, got, want)
		}
	}
}

func TestStoreUpdateIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("config.json readable by others: %v", fi.Mode())
	}
	err = s.Update(func(c *Config) error { c.Channels["x"] = Channel{URL: "invalid"}; return nil })
	if err == nil || len(s.Get().Channels) != 0 {
		t.Fatal("an invalid change should not be applied")
	}
	// the copy returned by Get does not change the configuration in use
	c := s.Get()
	c.Channels["y"] = Channel{URL: "http://x"}
	if len(s.Get().Channels) != 0 {
		t.Fatal("Get should return a copy")
	}
}

func parseIP(s string) []byte {
	n, _ := parseNet(s)
	return n.IP
}
