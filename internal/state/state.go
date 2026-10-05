package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type State struct{ Revision, Digest, GitURL, ImageRef string }

func (v State) Recoverable(gitURL, imageRef string) bool {
	return v.GitURL != "" && v.GitURL == gitURL && v.ImageRef == imageRef && v.Revision != ""
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
	dst := filepath.Join(d, "active.json")
	return AtomicWrite(dst, b)
}

// AtomicWrite makes the file contents durable before publishing the rename.
func AtomicWrite(dst string, b []byte) (err error) {
	tmp := dst + ".tmp"
	defer os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
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
	if e != nil {
		return e
	}
	return syncDir(filepath.Join(s.Data, "state"))
}
func (s Store) Reconcile() error {
	_, e := s.Load()
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	// A crash may leave a setup/candidate process that was never saved. Scan
	// once at startup, proving ownership from the executable and runtime paths.
	entries, e := os.ReadDir("/proc")
	if e != nil {
		return e
	}
	var processes []runtimeProcess
	groups := make(map[int]runtimeProcess)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		p, err := readProcess(pid)
		if err != nil {
			continue
		}
		processes = append(processes, p)
		if p.group == pid && ownedRuntime(s.Data, pid) {
			groups[pid] = p
		}
	}
	for group, original := range groups {
		if p, err := readProcess(group); err == nil && p.start == original.start && p.group == group {
			if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return fmt.Errorf("stop orphan runtime: %w", err)
			}
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		alive := false
		for _, original := range processes {
			if _, ok := groups[original.group]; !ok {
				continue
			}
			p, err := readProcess(original.pid)
			if err == nil && p.start == original.start && p.status != "Z" && p.status != "X" {
				alive = true
				break
			}
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("orphan runtime did not stop before filesystem cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, name := range []string{"instances", "runsc-root", "deployments"} {
		path := filepath.Join(s.Data, name)
		if !Within(s.Data, path) {
			return errors.New("unsafe cleanup path")
		}
		if e = os.RemoveAll(path); e != nil {
			return e
		}
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

type runtimeProcess struct {
	pid, group    int
	start, status string
}

func readProcess(pid int) (runtimeProcess, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return runtimeProcess{}, err
	}
	end := strings.LastIndexByte(string(b), ')')
	fields := strings.Fields(string(b[end+1:]))
	if end < 0 || len(fields) < 20 {
		return runtimeProcess{}, fmt.Errorf("invalid process stat")
	}
	group, err := strconv.Atoi(fields[2])
	return runtimeProcess{pid, group, fields[19], fields[0]}, err
}

func ownedRuntime(data string, pid int) bool {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	st, err := os.Stat(dir)
	if err != nil || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return false
	}
	exe, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil || filepath.Base(strings.TrimSuffix(exe, " (deleted)")) != "runsc" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return false
	}
	args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	if len(args) == 0 || filepath.Base(args[0]) != "runsc" {
		return false
	}
	cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		return false
	}
	var root, bundle string
	for n := 1; n+1 < len(args); n++ {
		value := args[n+1]
		if !filepath.IsAbs(value) {
			value = filepath.Join(cwd, value)
		}
		switch args[n] {
		case "--root":
			root = filepath.Clean(value)
		case "--bundle":
			bundle = filepath.Clean(value)
		}
	}
	data, err = filepath.Abs(data)
	return err == nil && root != "" && bundle != "" && filepath.Dir(root) == filepath.Join(data, "runsc-root") &&
		(filepath.Dir(bundle) == filepath.Join(data, "instances") || filepath.Dir(bundle) == filepath.Join(data, "prepared"))
}
