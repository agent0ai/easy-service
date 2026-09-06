package proxy

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	URL  *url.URL
	in   atomic.Int64
	zero chan struct{}
	once sync.Once
}

func NewBackend(raw string) (*Backend, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return nil, e
	}
	return &Backend{URL: u, zero: make(chan struct{})}, nil
}
func (b *Backend) enter() { b.in.Add(1) }
func (b *Backend) leave() {
	if b.in.Add(-1) == 0 {
		b.once.Do(func() { close(b.zero) })
	}
}
func (b *Backend) Drain(ctx context.Context, deadline time.Duration) bool {
	if b.in.Load() == 0 {
		return true
	}
	t := time.NewTimer(deadline)
	defer t.Stop()
	select {
	case <-b.zero:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

type Proxy struct {
	current   atomic.Pointer[Backend]
	transport http.RoundTripper
	selection sync.RWMutex
}

func New() *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Proxy{transport: transport}
}
func (p *Proxy) Set(b *Backend) *Backend {
	p.selection.Lock()
	defer p.selection.Unlock()
	return p.current.Swap(b)
}
func (p *Proxy) Current() *Backend { return p.current.Load() }
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.selection.RLock()
	b := p.current.Load()
	if b != nil {
		b.enter()
	}
	p.selection.RUnlock()
	if b == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer b.leave()
	rp := httputil.NewSingleHostReverseProxy(b.URL)
	rp.Transport = p.transport
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}
