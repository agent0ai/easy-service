package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Workload UIDs are reserved independently of ordinary system accounts.
const MinWorkloadUID = 20000

type Usage struct {
	MemoryBytes uint64  `json:"memory_bytes"`
	CPUPercent  float64 `json:"cpu_percent"`
}

type UserSample struct {
	MemoryBytes, CPUTicks uint64
	PIDs                  []int
}

// SampleUser includes descendants that changed their session or were reparented.
func SampleUser(uid uint32) (UserSample, error) {
	if uid < MinWorkloadUID {
		return UserSample{}, fmt.Errorf("invalid workload UID")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return UserSample{}, err
	}
	var sample UserSample
	// ponytail: scan the container's processes per sample; share scans if many instances overlap.
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		status, err := os.ReadFile(filepath.Join(dir, "status"))
		if err != nil {
			continue // the process exited during observation
		}
		owned := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) == 5 {
					actual, _ := strconv.ParseUint(fields[1], 10, 32)
					owned = uint32(actual) == uid
				}
				break
			}
		}
		if !owned {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		end := strings.LastIndexByte(string(stat), ')')
		if end < 0 {
			return sample, fmt.Errorf("invalid process stat")
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 22 {
			return sample, fmt.Errorf("incomplete process stat")
		}
		if fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		pages, err := strconv.ParseUint(fields[21], 10, 64)
		if err != nil {
			return sample, err
		}
		sample.MemoryBytes += pages * uint64(os.Getpagesize())
		// Linux stat includes CPU consumed by children already reaped by this
		// process, preserving their work after ordinary subprocess completion.
		for _, at := range []int{11, 12, 13, 14} {
			ticks, err := strconv.ParseUint(fields[at], 10, 64)
			if err != nil {
				return sample, err
			}
			sample.CPUTicks += ticks
		}
		sample.PIDs = append(sample.PIDs, pid)
	}
	return sample, nil
}

// SignalUser asks the kernel to signal every process owned by this UID in the
// container's PID namespace. A fresh, unprivileged helper cannot signal peers.
// ponytail: serialize short signal helpers; use per-UID locks if bulk shutdown throughput matters.
var userSignals sync.Mutex

func SignalUser(ctx context.Context, uid uint32, signal syscall.Signal) error {
	if uid < MinWorkloadUID || (signal != syscall.SIGTERM && signal != syscall.SIGKILL) {
		return fmt.Errorf("invalid workload signal")
	}
	// Helpers share their target UID. Concurrent broadcasts could otherwise
	// kill another helper while normal drain and urgent force-stop overlap.
	userSignals.Lock()
	defer userSignals.Unlock()
	sample, err := SampleUser(uid)
	if err != nil || len(sample.PIDs) == 0 {
		return err
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "kill -"+strconv.Itoa(int(signal))+" -1")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: &syscall.Credential{Uid: uid, Gid: uid}}
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if err != nil {
		// Every target may exit between the sample and kill(-1).
		remaining, sampleErr := SampleUser(uid)
		if sampleErr == nil && len(remaining.PIDs) == 0 {
			return nil
		}
	}
	return err
}

func KillUser(ctx context.Context, uid uint32) error {
	for {
		if err := SignalUser(ctx, uid, syscall.SIGKILL); err != nil {
			return err
		}
		sample, err := SampleUser(uid)
		if err != nil {
			return err
		}
		if len(sample.PIDs) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("workload processes did not stop: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
