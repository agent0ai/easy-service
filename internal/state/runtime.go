package state

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/example/easy-service/internal/process"
)

const runtimeOwners = "/run/easy-service/users"

// RuntimeOwner is a root-owned UID lease. Workloads cannot create, change or
// release leases; a UID is never reused while its processes remain alive.
type RuntimeOwner struct {
	UID    uint32
	Root   string
	Bundle string `json:",omitempty"`
}

func NewRuntimeOwner(data, id string) (RuntimeOwner, error) {
	return newRuntimeOwner(data, id, "")
}

func newRuntimeOwner(data, id, bundle string) (RuntimeOwner, error) {
	owner := RuntimeOwner{Bundle: bundle}
	if os.Geteuid() != 0 || id == "." || id == ".." || id == "/" || id == "" || filepath.Base(id) != id {
		return owner, fmt.Errorf("invalid runtime owner")
	}
	data, err := filepath.Abs(data)
	if err != nil {
		return owner, err
	}
	owner.Root = filepath.Join(data, "runtime", id)
	if err := os.MkdirAll(owner.Root, 0711); err != nil {
		return owner, err
	}
	if err := os.MkdirAll(runtimeOwners, 0700); err != nil {
		return owner, err
	}
	st, err := os.Lstat(runtimeOwners)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 || st.Sys().(*syscall.Stat_t).Uid != 0 {
		return owner, fmt.Errorf("runtime owner directory must be private and root-owned")
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return owner, err
	}
	// Stay within the ordinary 65536-UID mapping used by Docker remapping.
	const slots = 40000
	first := binary.LittleEndian.Uint32(random[:]) % slots
	for offset := uint32(0); offset < slots; offset++ {
		owner.UID = process.MinWorkloadUID + (first+offset)%slots
		claim := owner.claim()
		if err := os.Mkdir(claim, 0700); os.IsExist(err) {
			continue
		} else if err != nil {
			return owner, err
		}
		sample, err := process.SampleUser(owner.UID)
		if err != nil || len(sample.PIDs) != 0 {
			_ = os.Remove(claim)
			if err != nil {
				return owner, err
			}
			continue
		}
		body, _ := json.Marshal(owner)
		if err := AtomicWrite(filepath.Join(claim, "owner.json"), body); err != nil {
			_ = os.RemoveAll(claim)
			return owner, err
		}
		return owner, nil
	}
	return owner, fmt.Errorf("no unused workload UID available")
}

func (o RuntimeOwner) claim() string {
	return filepath.Join(runtimeOwners, strconv.FormatUint(uint64(o.UID), 10))
}

func (o RuntimeOwner) Release() error {
	body, err := os.ReadFile(filepath.Join(o.claim(), "owner.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var current RuntimeOwner
	if json.Unmarshal(body, &current) != nil || current != o {
		return fmt.Errorf("runtime UID lease changed")
	}
	sample, err := process.SampleUser(o.UID)
	if err != nil {
		return err
	}
	if len(sample.PIDs) != 0 {
		return fmt.Errorf("cannot release a running instance UID")
	}
	return os.RemoveAll(o.claim())
}

func ownersForData(data string) ([]RuntimeOwner, error) {
	data, err := filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(runtimeOwners)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var owners []RuntimeOwner
	for _, entry := range entries {
		uid, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil || uid < process.MinWorkloadUID || uid >= 60000 || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("invalid runtime UID lease")
		}
		claim := filepath.Join(runtimeOwners, entry.Name())
		st, err := os.Lstat(claim)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || st.Mode().Perm() != 0700 || st.Sys().(*syscall.Stat_t).Uid != 0 {
			return nil, fmt.Errorf("untrusted runtime UID lease")
		}
		body, err := os.ReadFile(filepath.Join(claim, "owner.json"))
		if os.IsNotExist(err) {
			continue // allocation interrupted before metadata: no workload could start
		}
		if err != nil {
			return nil, err
		}
		var owner RuntimeOwner
		if err := json.Unmarshal(body, &owner); err != nil || uint64(owner.UID) != uid {
			return nil, fmt.Errorf("invalid runtime owner metadata")
		}
		if filepath.Dir(owner.Root) != filepath.Join(data, "runtime") {
			continue // another supervisor owns this lease
		}
		owners = append(owners, owner)
	}
	return owners, nil
}

func reconcileRuntimeOwners(data string) error {
	owners, err := ownersForData(data)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = process.KillUser(ctx, owner.UID)
		cancel()
		if err != nil {
			return fmt.Errorf("stop orphan instance: %w", err)
		}
		// Writable sandboxes reserve their UID even while stopped.
		if owner.Bundle != "" {
			if saved, err := SandboxOwner(data, owner.Bundle); err == nil && saved.UID == owner.UID {
				continue
			}
			if SandboxPath(data, owner.Bundle) {
				if err := RemoveSandbox(data, owner.Bundle); err != nil {
					return err
				}
			}
		}
		if err := owner.Release(); err != nil {
			return err
		}
	}
	return nil
}
