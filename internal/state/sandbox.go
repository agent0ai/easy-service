package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/example/easy-service/internal/process"
)

const sandboxOwnerFile = ".easy-service-owner.json"

// SandboxPath accepts only direct children of supervisor-owned filesystem areas.
func SandboxPath(data, bundle string) bool {
	data, err := filepath.Abs(data)
	if err != nil || !filepath.IsAbs(bundle) || filepath.Clean(bundle) != bundle {
		return false
	}
	parent := filepath.Dir(bundle)
	if parent != filepath.Join(data, "instances") && parent != filepath.Join(data, "prepared") {
		return false
	}
	for _, path := range []string{parent, bundle} {
		st, err := os.Lstat(path)
		if err != nil || !st.IsDir() || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0022 != 0 {
			return false
		}
	}
	return true
}

func SandboxOwner(data, bundle string) (RuntimeOwner, error) {
	var owner RuntimeOwner
	if !SandboxPath(data, bundle) {
		return owner, fmt.Errorf("invalid sandbox directory")
	}
	path := filepath.Join(bundle, sandboxOwnerFile)
	st, err := os.Lstat(path)
	if err != nil {
		return owner, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != 0 {
		return owner, fmt.Errorf("untrusted sandbox ownership")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return owner, err
	}
	data, _ = filepath.Abs(data)
	if json.Unmarshal(body, &owner) != nil || owner.Bundle != bundle || owner.UID < process.MinWorkloadUID || owner.UID >= 60000 || filepath.Dir(owner.Root) != filepath.Join(data, "runtime") {
		return RuntimeOwner{}, fmt.Errorf("invalid sandbox ownership")
	}
	return owner, nil
}

// AcquireSandboxOwner reclaims only a stopped sandbox's protected UID lease.
func AcquireSandboxOwner(data, id, bundle string) (RuntimeOwner, bool, error) {
	if os.Geteuid() != 0 || id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return RuntimeOwner{}, false, fmt.Errorf("invalid runtime owner")
	}
	owner, err := SandboxOwner(data, bundle)
	if os.IsNotExist(err) {
		owner, err = newRuntimeOwner(data, id, bundle)
		return owner, false, err
	}
	if err != nil {
		return owner, false, err
	}
	st, err := os.Lstat(filepath.Join(bundle, "rootfs"))
	if err != nil || !st.IsDir() || st.Sys().(*syscall.Stat_t).Uid != owner.UID {
		return owner, false, fmt.Errorf("sandbox filesystem ownership changed")
	}
	sample, err := process.SampleUser(owner.UID)
	if err != nil {
		return owner, false, err
	}
	if len(sample.PIDs) != 0 {
		return owner, false, fmt.Errorf("sandbox user is still running")
	}
	if err = os.MkdirAll(runtimeOwners, 0700); err != nil {
		return owner, false, err
	}
	if st, err := os.Lstat(runtimeOwners); err != nil || !st.IsDir() || st.Mode().Perm() != 0700 || st.Sys().(*syscall.Stat_t).Uid != 0 {
		return owner, false, fmt.Errorf("untrusted runtime owner directory")
	}
	if err = os.Mkdir(owner.claim(), 0700); os.IsExist(err) {
		if st, err := os.Lstat(owner.claim()); err != nil || !st.IsDir() || st.Mode().Perm() != 0700 || st.Sys().(*syscall.Stat_t).Uid != 0 {
			return owner, false, fmt.Errorf("untrusted runtime UID lease")
		}
		var current RuntimeOwner
		body, readErr := os.ReadFile(filepath.Join(owner.claim(), "owner.json"))
		if readErr != nil || json.Unmarshal(body, &current) != nil || current.UID != owner.UID || current.Bundle != bundle {
			return owner, false, fmt.Errorf("sandbox user is reserved by another installation")
		}
	} else if err != nil {
		return owner, false, err
	}
	data, _ = filepath.Abs(data)
	owner.Root = filepath.Join(data, "runtime", id)
	if err = os.MkdirAll(owner.Root, 0711); err != nil {
		return owner, false, err
	}
	if err = owner.saveClaim(); err != nil {
		return owner, false, err
	}
	return owner, true, nil
}

func (o RuntimeOwner) saveClaim() error {
	body, _ := json.Marshal(o)
	return AtomicWrite(filepath.Join(o.claim(), "owner.json"), body)
}

// SaveSandbox is called only after the first complete ownership walk.
func (o RuntimeOwner) SaveSandbox() error {
	body, _ := json.Marshal(o)
	return AtomicWrite(filepath.Join(o.Bundle, sandboxOwnerFile), body)
}

// RemoveSandbox keeps the user reserved until its last private files are gone.
func RemoveSandbox(data, bundle string) error {
	if _, err := os.Lstat(bundle); os.IsNotExist(err) {
		return nil
	}
	if !SandboxPath(data, bundle) {
		return fmt.Errorf("unsafe sandbox cleanup path")
	}
	owners, err := ownersForData(data)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if owner.Bundle != bundle {
			continue
		}
		sample, err := process.SampleUser(owner.UID)
		if err != nil {
			return err
		}
		if len(sample.PIDs) != 0 {
			return fmt.Errorf("cannot remove a running sandbox")
		}
	}
	if err := os.RemoveAll(bundle); err != nil {
		return err
	}
	for _, owner := range owners {
		if owner.Bundle == bundle {
			if err := owner.Release(); err != nil {
				return err
			}
		}
	}
	return nil
}
