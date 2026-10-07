package state

import (
	"context"
	"encoding/json"
	"github.com/example/easy-service/internal/process"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestAtomicStateRecoveryAndCleanup(t *testing.T) {
	d := t.TempDir()
	s := Store{Data: d}
	v := State{Revision: "abc", GitURL: "/source/one", ImageRef: "image"}
	if e := s.Save(v); e != nil {
		t.Fatal(e)
	}
	got, e := s.Load()
	if e != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("%+v %v", got, e)
	}
	if !got.Recoverable(v.GitURL, v.ImageRef) || got.Recoverable("/source/two", v.ImageRef) || got.Recoverable(v.GitURL, "changed-image") {
		t.Fatal("saved recovery revision was not restricted to its source and image")
	}
	legacy := got
	legacy.GitURL = ""
	if legacy.Recoverable(v.GitURL, v.ImageRef) {
		t.Fatal("legacy state without source identity was accepted for recovery")
	}
	for _, name := range []string{"instances", "runtime", "deployments"} {
		os.MkdirAll(filepath.Join(d, name, "orphan"), 0700)
	}
	if e = s.Reconcile(); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"instances", "runtime", "deployments"} {
		if _, e = os.Stat(filepath.Join(d, name)); !os.IsNotExist(e) {
			t.Fatalf("orphan %s retained", name)
		}
	}
	if _, e = s.Load(); !os.IsNotExist(e) {
		t.Fatal("stale state retained")
	}
}

func TestReconcileHelperProcess(t *testing.T) {
	if os.Getenv("EASY_SERVICE_RECONCILE_HELPER") != "1" {
		return
	}
	if os.Getenv("EASY_SERVICE_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	if err := os.WriteFile(os.Getenv("EASY_SERVICE_READY"), []byte("ready"), 0600); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func reconcileProcess(t *testing.T, data, name, area string, ignoreTerm bool) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	bundle := filepath.Join(data, area, "orphan")
	if err := os.MkdirAll(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), name)
	if err := os.Link(exe, helper); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(bundle, "running")
	cmd := exec.Command(helper, "-test.run=TestReconcileHelperProcess", "--", "--rootless=true", "--network=host", "--file-access=exclusive", "--root", filepath.Join(data, "runtime", "orphan"), "run", "--bundle", bundle, "orphan")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{"EASY_SERVICE_RECONCILE_HELPER=1", "EASY_SERVICE_READY=" + ready}
	if ignoreTerm {
		cmd.Env = append(cmd.Env, "EASY_SERVICE_IGNORE_TERM=1")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start")
		}
		time.Sleep(time.Millisecond)
	}
	return cmd, done
}

func TestReconcileStopsOnlyOwnedUIDs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("UID isolation requires root")
	}
	data, other := t.TempDir(), t.TempDir()
	var owners []RuntimeOwner
	var commands []*exec.Cmd
	for _, dir := range []string{data, other} {
		owner, err := NewRuntimeOwner(dir, "orphan")
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
		cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 600 & wait")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: &syscall.Credential{Uid: owner.UID, Gid: owner.UID}}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = process.KillUser(ctx, owner.UID)
			_ = cmd.Wait()
			_ = owner.Release()
		})
		deadline := time.Now().Add(time.Second)
		for {
			s, err := process.SampleUser(owner.UID)
			if err == nil && len(s.PIDs) >= 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("helper did not fork")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := (Store{Data: data}).Reconcile(); err != nil {
		t.Fatal(err)
	}
	s, err := process.SampleUser(owners[0].UID)
	if err != nil || len(s.PIDs) != 0 {
		t.Fatal("owned orphan survived", s, err)
	}
	s, err = process.SampleUser(owners[1].UID)
	if err != nil || len(s.PIDs) == 0 {
		t.Fatal("unrelated supervisor was stopped", s, err)
	}
	_ = commands
}

func TestReconcileDoesNotSignalUnrelatedPID(t *testing.T) {
	for _, name := range []string{"runsc", "unrelated-runsc"} {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			actualData := data
			if name == "runsc" {
				actualData = t.TempDir()
			}
			cmd, done := reconcileProcess(t, actualData, name, "instances", false)
			s := Store{Data: data}
			if err := s.Save(State{}); err != nil {
				t.Fatal(err)
			}
			legacy, err := json.Marshal(map[string]any{"Bundle": filepath.Join(data, "instances", "recorded"), "PID": cmd.Process.Pid})
			if err != nil {
				t.Fatal(err)
			}
			if err := AtomicWrite(filepath.Join(data, "state", "active.json"), legacy); err != nil {
				t.Fatal(err)
			}
			if err := s.Reconcile(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				t.Fatal("stale state killed an unrelated process")
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestWithinExcludesRootPeersAndTraversal(t *testing.T) {
	root := t.TempDir()
	for path, want := range map[string]bool{
		root:                             false,
		filepath.Join(root, "owned"):     true,
		root + "-peer/owned":             false,
		filepath.Join(root, "..", "out"): false,
	} {
		if got := Within(root, path); got != want {
			t.Errorf("Within(%q, %q) = %t, want %t", root, path, got, want)
		}
	}
}

func TestAtomicWriteUsesPrivateTempAndReconcileRetainsConfig(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "active.json")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dst+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(dst, []byte(`{"overrides":{"APP_KEY":"fixture"}}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(outside)
	if err != nil || string(b) != "untouched" {
		t.Fatal("atomic write followed a preexisting temp symlink")
	}
	info, err := os.Stat(dst)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("published credentials were not private")
	}
	for _, name := range []string{"active.json.tmp-1234", "pending.json.tmp-5678", "pending.json.tmp-not-ours"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := (Store{Data: t.TempDir(), ConfigDir: dir}).Reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"active.json.tmp-1234", "pending.json.tmp-5678"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("crashed atomic write was not collected")
		}
	}
	for _, path := range []string{dst, dst + ".tmp", filepath.Join(dir, "pending.json.tmp-not-ours"), outside} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("reconciliation removed configuration/unowned data:", err)
		}
	}
}
