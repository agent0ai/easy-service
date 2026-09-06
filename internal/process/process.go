package process

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

type Runner struct{}

func (Runner) Run(ctx context.Context, limit time.Duration, n string, a ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	c := exec.CommandContext(cctx, n, a...)
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b
	e := c.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s timed out after %s", n, limit)
	}
	if e != nil {
		return "", fmt.Errorf("%s failed: %w: %s", n, e, b.String())
	}
	return b.String(), nil
}
