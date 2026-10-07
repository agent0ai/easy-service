package process

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestConcurrentUserSignals(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to launch an unprivileged process")
	}
	// Stay outside the production lease range, within Docker's usual UID map.
	uid := uint32(60000 + os.Getpid()%5000)
	sample, err := SampleUser(uid)
	if err != nil || len(sample.PIDs) != 0 {
		t.Fatalf("test UID already in use: %v", err)
	}
	child := exec.Command("/bin/sh", "-c", "trap '' TERM; echo ready; exec sleep 30")
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("test child not ready: %q %v", ready, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start, results := make(chan struct{}), make(chan error, 32)
	for range 32 {
		go func() {
			<-start
			results <- SignalUser(ctx, uid, syscall.SIGTERM)
		}()
	}
	close(start)
	for range 32 {
		if err := <-results; err != nil {
			t.Error("concurrent signal helpers interfered:", err)
		}
	}
}
