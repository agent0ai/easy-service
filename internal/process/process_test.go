package process

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTimeoutKillsDescendantsHoldingOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	result := make(chan error, 1)
	go func() {
		_, err := (Runner{}).Run(context.Background(), 100*time.Millisecond, "/bin/sh", "-c", `sleep 10 & echo $! > "$1"; wait`, "sh", pidFile)
		result <- err
	}()
	defer func() {
		b, _ := os.ReadFile(pidFile)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stalled command succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout left an inherited output pipe open")
	}
}

func TestCommandOutputIsBounded(t *testing.T) {
	out, err := (Runner{}).Run(context.Background(), time.Second, "head", "-c", "2097152", "/dev/zero")
	if err == nil || len(out) > 1<<20 {
		t.Fatalf("unbounded output: bytes=%d err=%v", len(out), err)
	}
}
