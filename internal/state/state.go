package state

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type State struct {
	Revision, Digest, GitURL, ImageRef string
	Sandbox, ResumeKey                 string            `json:",omitempty"`
	Overrides                          map[string]string `json:"overrides,omitempty"`
}

func (v State) Recoverable(gitURL, imageRef string) bool {
	return v.GitURL != "" && v.GitURL == gitURL && v.ImageRef == imageRef && v.Revision != ""
}

type Store struct{ Data, ConfigDir string }

func (s Store) directory() string {
	if s.ConfigDir != "" {
		return s.ConfigDir
	}
	return filepath.Join(s.Data, "state")
}

func (s Store) Save(v State) error {
	d := s.directory()
	if e := os.MkdirAll(d, 0700); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	dst := filepath.Join(d, "active.json")
	return AtomicWrite(dst, b)
}

// AtomicWrite makes the file contents durable before publishing the rename.
func AtomicWrite(dst string, b []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s Store) Load() (State, error) {
	var v State
	b, e := os.ReadFile(filepath.Join(s.directory(), "active.json"))
	if e != nil {
		return v, e
	}
	e = json.Unmarshal(b, &v)
	return v, e
}
func (s Store) Clear() error {
	e := os.Remove(filepath.Join(s.directory(), "active.json"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	return syncDir(s.directory())
}
func (s Store) Reconcile() error {
	v, e := s.Load()
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if err := reconcileRuntimeOwners(s.Data); err != nil {
		return err
	}
	instances := filepath.Join(s.Data, "instances")
	keep := ""
	if v.Sandbox != "" && filepath.Base(v.Sandbox) == v.Sandbox {
		bundle := filepath.Join(instances, v.Sandbox)
		if _, err := SandboxOwner(s.Data, bundle); err == nil {
			keep = v.Sandbox
		}
	}
	entries, err := os.ReadDir(instances)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == keep {
			continue
		}
		path := filepath.Join(instances, entry.Name())
		if entry.IsDir() {
			if err := RemoveSandbox(s.Data, path); err != nil {
				return err
			}
		} else if err := os.Remove(path); err != nil {
			return err
		}
	}
	if keep == "" {
		_ = os.Remove(instances)
	}
	// /run can be lost while /data survives; reserve retained filesystem users
	// before any new setup or candidate can allocate them.
	for _, area := range []string{"instances", "prepared"} {
		entries, err := os.ReadDir(filepath.Join(s.Data, area))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, entry := range entries {
			bundle := filepath.Join(s.Data, area, entry.Name())
			owner, err := SandboxOwner(s.Data, bundle)
			if os.IsNotExist(err) {
				continue
			} // installation from an older supervisor
			if err == nil {
				_, _, err = AcquireSandboxOwner(s.Data, filepath.Base(owner.Root), bundle)
			}
			if err != nil {
				log.Printf("discarding unusable saved sandbox=%s: %v", entry.Name(), err)
				if err := RemoveSandbox(s.Data, bundle); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range []string{"runtime", "deployments"} {
		path := filepath.Join(s.Data, name)
		if !Within(s.Data, path) {
			return errors.New("unsafe cleanup path")
		}
		if e = os.RemoveAll(path); e != nil {
			return e
		}
	}
	// Applied configuration and its last healthy revision survive reconciliation.
	if s.ConfigDir != "" {
		entries, err := os.ReadDir(s.ConfigDir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			for _, name := range []string{"active.json", "pending.json", "apply.json"} {
				prefix := name + ".tmp-"
				if !strings.HasPrefix(entry.Name(), prefix) {
					continue
				}
				if _, err := strconv.ParseUint(strings.TrimPrefix(entry.Name(), prefix), 10, 32); err != nil {
					continue
				}
				if err := os.Remove(filepath.Join(s.ConfigDir, entry.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if v.Sandbox != "" {
		return nil
	}
	return s.Clear()
}

// Within reports whether p is lexically below root, excluding root itself.
func Within(root, p string) bool {
	r, e := filepath.Abs(root)
	if e != nil {
		return false
	}
	x, e := filepath.Abs(p)
	return e == nil && x != r && strings.HasPrefix(x, r+string(filepath.Separator))
}
