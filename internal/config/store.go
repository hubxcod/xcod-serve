package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"xcod/internal/i18n"
)

// Store keeps the configuration in use and writes every change to disk.
type Store struct {
	path string

	mu  sync.RWMutex
	cur Config

	updMu    sync.Mutex // serializes Update (write + notify in the right order)
	onChange []func(Config)
}

// Load reads the file; if it does not exist (first run), it writes the defaults.
func Load(path string) (*Store, error) {
	s := &Store{path: path, cur: Default()}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.save(s.cur)
	}
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	c := Default()
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("invalid %s: %v", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	s.cur = c
	return s, nil
}

// Get returns a copy of the configuration in use.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.clone()
}

// OnChange registers a function that must know about new configurations.
func (s *Store) OnChange(fn func(Config)) {
	s.updMu.Lock()
	defer s.updMu.Unlock()
	s.onChange = append(s.onChange, fn)
}

// Update applies fn to a copy, validates it, writes it and only then swaps the configuration
// in use. If anything fails, nothing changes.
func (s *Store) Update(fn func(*Config) error) error {
	s.updMu.Lock()
	defer s.updMu.Unlock()
	c := s.Get()
	if err := fn(&c); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if err := s.save(c); err != nil {
		return i18n.Errorf("could not save the configuration: %v", err)
	}
	s.mu.Lock()
	s.cur = c
	s.mu.Unlock()
	for _, f := range s.onChange {
		f(c.clone())
	}
	return nil
}

// save writes atomically (temporary file + rename), readable only by the owner: the file
// holds provider URLs and access keys.
func (s *Store) save(c Config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
