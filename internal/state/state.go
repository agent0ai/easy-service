package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type State struct {
	Revision, Digest, ImageRef, Prepared, Bundle string
	PID                                          int
}
type Store struct{ Data string }

func (s Store) Save(v State) error {
	d := filepath.Join(s.Data, "state")
	if e := os.MkdirAll(d, 0700); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	tmp := filepath.Join(d, "active.json.tmp")
	dst := filepath.Join(d, "active.json")
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	if e = os.Rename(tmp, dst); e != nil {
		return e
	}
	f, e := os.Open(d)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s Store) Load() (State, error) {
	var v State
	b, e := os.ReadFile(filepath.Join(s.Data, "state", "active.json"))
	if e != nil {
		return v, e
	}
	e = json.Unmarshal(b, &v)
	return v, e
}
func (s Store) Clear() error {
	e := os.Remove(filepath.Join(s.Data, "state", "active.json"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	return e
}
func (s Store) Reconcile() error {
	v, e := s.Load()
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if e == nil && v.PID > 1 && within(s.Data, v.Bundle) && ownedRuntime(v.PID) {
		_ = syscall.Kill(-v.PID, syscall.SIGTERM)
	}
	for _, name := range []string{"instances", "runsc-root", "deployments"} {
		path := filepath.Join(s.Data, name)
		if !within(s.Data, path) {
			return errors.New("unsafe cleanup path")
		}
		if e = os.RemoveAll(path); e != nil {
			return e
		}
	}
	return s.Clear()
}
func within(root, p string) bool {
	r, e := filepath.Abs(root)
	if e != nil {
		return false
	}
	x, e := filepath.Abs(p)
	return e == nil && x != r && strings.HasPrefix(x, r+string(filepath.Separator))
}

func ownedRuntime(pid int) bool {
	b, e := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "cmdline"))
	return e == nil && strings.Contains(string(b), "rootlesskit")
}
