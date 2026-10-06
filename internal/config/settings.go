package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/example/easy-service/internal/state"
)

// Settings stores only overrides. Docker environment values remain defaults.
// Accepted overrides share the active revision's atomic state record; pending
// overrides are a separate file and never participate in automatic deployment.
type Settings struct {
	mu                     sync.RWMutex
	base, applied, pending map[string]string
	store                  state.Store
}

func Open(base map[string]string, store state.Store) (*Settings, state.State, error) {
	prior, err := store.Load()
	if err != nil && !os.IsNotExist(err) {
		return nil, prior, fmt.Errorf("load applied configuration: %w", err)
	}
	s := &Settings{base: maps.Clone(base), applied: maps.Clone(prior.Overrides), pending: maps.Clone(prior.Overrides), store: store}
	b, err := os.ReadFile(s.pendingPath())
	if err == nil {
		err = json.Unmarshal(b, &s.pending)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, prior, fmt.Errorf("load pending configuration: %w", err)
	}
	if s.applied == nil {
		s.applied = map[string]string{}
	}
	if s.pending == nil {
		s.pending = map[string]string{}
	}
	for _, overrides := range []map[string]string{s.applied, s.pending} {
		for name, value := range overrides {
			if err := validateOverride(name, value, s.base); err != nil {
				return nil, prior, err
			}
		}
	}
	return s, prior, nil
}

func (s *Settings) pendingPath() string { return filepath.Join(s.store.ConfigDir, "pending.json") }

func validateOverride(name, value string, base map[string]string) error {
	if !ValidName(name) {
		return fmt.Errorf("unknown configuration variable %q", name)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s contains a NUL byte", name)
	}
	if name == "DATA_DIR" || name == "CONFIG_DIR" || name == "LOG_DIR" {
		if value != base[name] {
			return fmt.Errorf("%s is selected when the container starts; set it in Docker", name)
		}
	}
	return nil
}

func (s *Settings) Values(pending bool) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := maps.Clone(s.base)
	overrides := s.applied
	if pending {
		overrides = s.pending
	}
	maps.Copy(values, overrides)
	return values
}

func (s *Settings) Pending() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.pending)
}

func (s *Settings) Applied() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.applied)
}

func (s *Settings) Accept(overrides map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = maps.Clone(overrides)
}

// Stage validates names and persists the whole batch before changing memory.
// Values are validated together on apply, so incomplete configurations can be staged.
func (s *Settings) Stage(values map[string]string, unset []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := maps.Clone(s.pending)
	for name, value := range values {
		if err := validateOverride(name, value, s.base); err != nil {
			return err
		}
		next[name] = value
	}
	for _, name := range unset {
		if !ValidName(name) {
			return fmt.Errorf("unknown configuration variable %q", name)
		}
		delete(next, name)
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return fmt.Errorf("saved configuration exceeds 1 MiB")
	}
	if err = os.MkdirAll(s.store.ConfigDir, 0700); err != nil {
		return err
	}
	if err = state.AtomicWrite(s.pendingPath(), b); err != nil {
		return err
	}
	s.pending = next
	return nil
}

func Changed(old, next map[string]string) []string {
	names := map[string]bool{}
	for name, value := range old {
		v, exists := next[name]
		if !exists || v != value {
			names[name] = true
		}
	}
	for name, value := range next {
		v, exists := old[name]
		if !exists || v != value {
			names[name] = true
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
