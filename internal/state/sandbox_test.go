package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func ownedSandbox(t *testing.T, data, area, id string) RuntimeOwner {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("sandbox ownership requires root")
	}
	bundle := filepath.Join(data, area, id)
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	o, reused, err := AcquireSandboxOwner(data, id, bundle)
	if err != nil || reused {
		t.Fatal(o, reused, err)
	}
	if err := os.Chown(filepath.Join(bundle, "rootfs"), int(o.UID), int(o.UID)); err != nil {
		t.Fatal(err)
	}
	if err := o.SaveSandbox(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := RemoveSandbox(data, bundle); err != nil {
			t.Error(err)
		}
	})
	return o
}

func TestReconcileKeepsOnlyAcceptedSandboxAndReservedUsers(t *testing.T) {
	data := t.TempDir()
	keep := ownedSandbox(t, data, "instances", "accepted")
	old := ownedSandbox(t, data, "instances", "retired")
	prepared := ownedSandbox(t, data, "prepared", "cached")
	s := Store{Data: data, ConfigDir: t.TempDir()}
	if err := s.Save(State{Sandbox: "accepted"}); err != nil {
		t.Fatal(err)
	}
	// Runtime leases can disappear when persistent /data moves to a new container.
	if err := keep.Release(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, o := range []RuntimeOwner{keep, prepared} {
		if _, err := os.Stat(o.Bundle); err != nil {
			t.Fatal("reusable filesystem removed:", err)
		}
		if _, err := os.Stat(o.claim()); err != nil {
			t.Fatal("user reservation removed:", err)
		}
	}
	for _, p := range []string{old.Bundle, old.claim()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("retired sandbox retained:", p, err)
		}
	}
	resumed, reused, err := AcquireSandboxOwner(data, "new-process", keep.Bundle)
	if err != nil || !reused || resumed.UID != keep.UID {
		t.Fatal("cannot resume accepted sandbox", resumed, reused, err)
	}
	if err := resumed.SaveSandbox(); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxLeaseCannotBeStolenAndCanBeRecreated(t *testing.T) {
	data := t.TempDir()
	o := ownedSandbox(t, data, "instances", "accepted")
	peer := o
	peer.Bundle = filepath.Join(data, "instances", "peer")
	b, _ := json.Marshal(peer)
	if err := AtomicWrite(filepath.Join(o.claim(), "owner.json"), b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := AcquireSandboxOwner(data, "new", o.Bundle); err == nil {
		t.Fatal("stole peer's user")
	}
	if err := o.Release(); err == nil {
		t.Fatal("released peer's user")
	}
	if err := o.saveClaim(); err != nil {
		t.Fatal(err)
	}
	if err := o.Release(); err != nil {
		t.Fatal(err)
	}
	resumed, reused, err := AcquireSandboxOwner(data, "new", o.Bundle)
	if err != nil || !reused || resumed.UID != o.UID {
		t.Fatal("lost filesystem identity after runtime leases were cleared", err)
	}
	if err := resumed.SaveSandbox(); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxRejectsUntrustedOwnershipAndPaths(t *testing.T) {
	data := t.TempDir()
	o := ownedSandbox(t, data, "instances", "accepted")
	path := filepath.Join(o.Bundle, sandboxOwnerFile)
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := AcquireSandboxOwner(data, "new", o.Bundle); err == nil {
		t.Fatal("trusted writable ownership metadata")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(data, "instances", "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := AcquireSandboxOwner(data, "new", link); err == nil {
		t.Fatal("followed sandbox symlink")
	}
	if err := RemoveSandbox(data, outside); err == nil {
		t.Fatal("accepted outside cleanup path")
	}
}

func TestDamagedSandboxMetadataIsDiscardedWithoutLeakingItsUser(t *testing.T) {
	data := t.TempDir()
	o := ownedSandbox(t, data, "instances", "accepted")
	s := Store{Data: data, ConfigDir: t.TempDir()}
	if err := s.Save(State{Sandbox: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.Bundle, sandboxOwnerFile), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{o.Bundle, o.claim()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("damaged sandbox retained", p, err)
		}
	}
}
