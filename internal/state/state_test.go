package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicStateRecoveryAndCleanup(t *testing.T) {
	d := t.TempDir()
	s := Store{d}
	v := State{Revision: "abc", Prepared: filepath.Join(d, "prepared", "x")}
	if e := s.Save(v); e != nil {
		t.Fatal(e)
	}
	got, e := s.Load()
	if e != nil || got.Revision != "abc" {
		t.Fatalf("%+v %v", got, e)
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
