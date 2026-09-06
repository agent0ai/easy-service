package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type proc struct {
	exited atomic.Bool
	done   chan error
}

func newProc() *proc               { return &proc{done: make(chan error)} }
func (p *proc) Exited() bool       { return p.exited.Load() }
func (p *proc) Done() <-chan error { return p.done }
func (p *proc) exit()              { p.exited.Store(true); close(p.done) }
func TestTimeoutCalculation(t *testing.T) {
	if ProbeTimeout(10*time.Second) != 3*time.Second || ProbeTimeout(2*time.Second) != time.Second {
		t.Fatal()
	}
}
func TestRedirectFailsAndSuccessResetsFailures(t *testing.T) {
	var n atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := n.Add(1)
		if x == 2 {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(500)
		}
	}))
	defer s.Close()
	c := New(20*time.Millisecond, "/")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got := c.UntilFailure(ctx, s.URL, newProc(), 2); got != ThresholdFailed || n.Load() != 4 {
		t.Fatalf("result=%v probes=%d", got, n.Load())
	}
}
func TestExitImmediate(t *testing.T) {
	p := newProc()
	p.exit()
	c := New(time.Hour, "/")
	if c.Ready(context.Background(), "http://127.0.0.1:1", p, time.Hour) == nil {
		t.Fatal()
	}
}

func TestExitInterruptsLivenessWait(t *testing.T) {
	p := newProc()
	c := New(time.Hour, "/")
	result := make(chan Result, 1)
	go func() { result <- c.UntilFailure(context.Background(), "http://127.0.0.1:1", p, 3) }()
	time.Sleep(10 * time.Millisecond)
	p.exit()
	select {
	case got := <-result:
		if got != ProcessExited {
			t.Fatalf("result=%v", got)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("process exit did not interrupt health wait")
	}
}
