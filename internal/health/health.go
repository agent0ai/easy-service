package health

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type Exited interface {
	Exited() bool
	Done() <-chan error
}
type Checker struct {
	Client   *http.Client
	Interval time.Duration
	Path     string
}

func ProbeTimeout(interval time.Duration) time.Duration {
	d := interval / 2
	if d > 3*time.Second {
		return 3 * time.Second
	}
	return d
}
func New(interval time.Duration, path string) *Checker {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	c := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Checker{c, interval, path}
}
func (c *Checker) Probe(ctx context.Context, base string, remaining time.Duration) error {
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	limit := ProbeTimeout(c.Interval)
	if remaining < limit {
		limit = remaining
	}
	pctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, e := http.NewRequestWithContext(pctx, "GET", base+c.Path, nil)
	if e != nil {
		return e
	}
	res, e := c.Client.Do(req)
	if e != nil {
		return e
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("health status %d", res.StatusCode)
	}
	return nil
}
func (c *Checker) Ready(ctx context.Context, base string, p Exited, startup time.Duration) error {
	deadline := time.Now().Add(startup)
	for {
		if p.Exited() {
			return fmt.Errorf("process exited before readiness")
		}
		remaining := time.Until(deadline)
		if e := c.probeWhileRunning(ctx, base, remaining, p); e == nil && !p.Exited() {
			return nil
		} else if p.Exited() {
			return fmt.Errorf("process exited before readiness")
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("startup health timeout")
		}
		wait := c.Interval
		if wait > remaining {
			wait = remaining
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-p.Done():
			t.Stop()
			return fmt.Errorf("process exited before readiness")
		case <-t.C:
		}
	}
}

type Result int

const (
	Healthy Result = iota
	ProcessExited
	ThresholdFailed
)

func (c *Checker) UntilFailure(ctx context.Context, base string, p Exited, failures int) Result {
	bad := 0
	for {
		if p.Exited() {
			return ProcessExited
		}
		if c.probeWhileRunning(ctx, base, c.Interval, p) == nil {
			bad = 0
		} else if p.Exited() {
			return ProcessExited
		} else {
			bad++
			if bad >= failures {
				return ThresholdFailed
			}
		}
		t := time.NewTimer(c.Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return Healthy
		case <-p.Done():
			t.Stop()
			return ProcessExited
		case <-t.C:
		}
	}
}

func (c *Checker) probeWhileRunning(ctx context.Context, base string, remaining time.Duration, p Exited) error {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan struct{})
	go func() {
		select {
		case <-p.Done():
			cancel()
		case <-finished:
		}
	}()
	err := c.Probe(probeCtx, base, remaining)
	close(finished)
	return err
}
