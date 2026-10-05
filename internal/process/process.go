package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type Runner struct{}

// Command owns a process group. Cancellation and completion must not leave
// descendants alive, and inherited output pipes must not hold Wait forever.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return KillGroup(c) }
	c.WaitDelay = time.Second
	return c
}

func KillGroup(c *exec.Cmd) error {
	if c.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return os.ErrProcessDone
	}
	return err
}

func Wait(c *exec.Cmd) error {
	err := c.Wait()
	_ = KillGroup(c)
	return err
}

type outputBuffer struct {
	b         bytes.Buffer
	truncated bool
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	const limit = 1 << 20
	n := len(p)
	remaining := limit - b.b.Len()
	if n > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.b.Write(p)
	return n, nil
}

func Output(c *exec.Cmd) (string, error) {
	var b outputBuffer
	c.Stdout, c.Stderr = &b, &b
	if err := c.Start(); err != nil {
		return "", err
	}
	err := Wait(c)
	if b.truncated {
		err = errors.Join(err, fmt.Errorf("command output exceeded 1 MiB (truncated)"))
	}
	return b.b.String(), err
}

func (Runner) Run(ctx context.Context, limit time.Duration, n string, a ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	b, e := Output(Command(cctx, n, a...))
	if cctx.Err() != nil {
		return "", fmt.Errorf("%s stopped: %w", n, cctx.Err())
	}
	if e != nil {
		return "", fmt.Errorf("%s failed: %w: %s", n, e, b)
	}
	return b, nil
}
