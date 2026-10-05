package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type Backend struct {
	URL      *url.URL
	in       int
	zero     chan struct{}
	mu       sync.Mutex
	draining bool
	reverse  *httputil.ReverseProxy
	bind     sync.Once
}

func NewBackend(raw string) (*Backend, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return nil, e
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("backend must be an HTTP endpoint without credentials")
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	director := rp.Director
	rp.Director = nil
	rp.Rewrite = func(r *httputil.ProxyRequest) {
		director(r.Out)
		// Clone snapshots Trailer before the upload finishes. Share the live
		// map so Transport sees values populated when Body reaches EOF.
		r.Out.Trailer = r.In.Trailer
		// Preserve the existing forwarding contract for trusted TLS upstreams.
		r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
		r.SetXForwarded()
		for _, name := range []string{"Forwarded", "X-Forwarded-Host", "X-Forwarded-Proto"} {
			r.Out.Header[name] = r.In.Header[name]
		}
	}
	// Preserve streaming latency even when the application knows the total size.
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
	return &Backend{URL: u, zero: make(chan struct{}), reverse: rp}, nil
}
func (b *Backend) enter() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.in++
	if b.in == 1 && b.draining {
		b.zero = make(chan struct{})
	}
}
func (b *Backend) leave() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.in--
	if b.in == 0 && b.draining {
		close(b.zero)
	}
}
func (b *Backend) Drain(ctx context.Context, deadline time.Duration) bool {
	b.mu.Lock()
	b.draining = true
	zero := b.zero
	idle := b.in == 0
	b.mu.Unlock()
	if idle {
		return true
	}
	t := time.NewTimer(deadline)
	defer t.Stop()
	select {
	case <-zero:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

type Proxy struct {
	current   *Backend
	transport http.RoundTripper
	selection sync.RWMutex
	buffers   bufferPool
}

type bufferPool struct{ pool sync.Pool }

func (p *bufferPool) Get() []byte {
	if b := p.pool.Get(); b != nil {
		return b.([]byte)
	}
	return make([]byte, 32<<10)
}
func (p *bufferPool) Put(b []byte) { p.pool.Put(b) }

func New() *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// The client default retains only two connections per host and churns TCP
	// connections under concurrent proxy traffic. Keep the existing global cap.
	transport.MaxIdleConnsPerHost = transport.MaxIdleConns
	return &Proxy{transport: transport}
}
func (p *Proxy) Set(b *Backend) *Backend {
	p.selection.Lock()
	defer p.selection.Unlock()
	if b != nil {
		b.bind.Do(func() {
			b.reverse.Transport = p.transport
			b.reverse.BufferPool = &p.buffers
		})
	}
	old := p.current
	p.current = b
	return old
}
func (p *Proxy) Current() *Backend {
	p.selection.RLock()
	defer p.selection.RUnlock()
	return p.current
}
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.selection.RLock()
	b := p.current
	if b != nil {
		b.enter()
	}
	p.selection.RUnlock()
	if b == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer b.leave()
	// HTTP/1 otherwise consumes the remaining upload before flushing a response,
	// which deadlocks applications that reply while the client is still sending.
	// HTTP/2 already allows duplex I/O; non-server writers may not support this.
	_ = http.NewResponseController(w).EnableFullDuplex()
	b.reverse.ServeHTTP(w, r)
}
