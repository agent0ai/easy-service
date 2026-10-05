package state

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAtomicStateRecoveryAndCleanup(t *testing.T) {
	d := t.TempDir()
	s := Store{d}
	v := State{Revision: "abc", GitURL: "/source/one", ImageRef: "image"}
	if e := s.Save(v); e != nil {
		t.Fatal(e)
	}
	got, e := s.Load()
	if e != nil || got != v {
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
	for _, name := range []string{"instances", "runsc-root", "deployments"} {
		os.MkdirAll(filepath.Join(d, name, "orphan"), 0700)
	}
	if e = s.Reconcile(); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"instances", "runsc-root", "deployments"} {
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
	cmd := exec.Command(helper, "-test.run=TestReconcileHelperProcess", "--", "--rootless=true", "--network=host", "--file-access=exclusive", "--root", filepath.Join(data, "runsc-root", "orphan"), "run", "--bundle", bundle, "orphan")
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

func TestReconcileStopsUnrecordedAndUncooperativeRuntime(t *testing.T) {
	for _, area := range []string{"instances", "prepared"} {
		t.Run(area, func(t *testing.T) {
			data := t.TempDir()
			_, done := reconcileProcess(t, data, "runsc", area, true)
			if err := (Store{data}).Reconcile(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("reconciliation left the orphan runtime running")
			}
		})
	}
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
			s := Store{data}
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
