package supervisor

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/sandbox"
	"github.com/example/easy-service/internal/state"
)

var soak = flag.Duration("soak", 0, "duration of real-Git/HTTP deployment and leak stress test")

type soakHealthTransport struct {
	http.RoundTripper
	proxy *proxy.Proxy
	t     *testing.T
}

func (s soakHealthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	res, err := s.RoundTripper.RoundTrip(req)
	if b := s.proxy.Current(); err != nil && b != nil && req.URL.Host == b.URL.Host && req.Context().Err() != context.Canceled {
		s.t.Logf("serving health probe failed after %s: %v", time.Since(started), err)
	}
	return res, err
}

func TestDeploymentSoak(t *testing.T) {
	if *soak <= 0 {
		t.Skip("enable with -args -soak=3m")
	}
	origin := t.TempDir()
	if b, err := exec.Command("git", "init", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, b)
	}
	sha := commitVersion(t, origin, "good-initial")
	data := t.TempDir()
	// Leave scheduling headroom for concurrent traffic and the race detector.
	// Short deadline and stalled-probe behavior have separate focused tests.
	cfg := config.Config{DataDir: data, GitURL: origin, RuntimeImage: "test-image", RunCommand: "owned-http-boundary", ServicePort: 80, StartupTimeout: time.Second, HealthInterval: 250 * time.Millisecond, HealthFailures: 3}
	rt := &httpRuntime{Runtime: sandbox.Runtime{Data: data}}
	git := revision.New(origin, "", data)
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{Cfg: cfg, Runtime: rt, Checkout: git, Proxy: proxy.New(), Store: state.Store{Data: data}, Health: health.New(cfg.HealthInterval, "/health"), Rootfs: t.TempDir(), Digest: "sha256:test", Drain: 2 * time.Second, RuntimeContext: ctx, Failures: make(chan uint64, 1)}
	checker := e.Health.(*health.Checker)
	checker.Client.Transport = soakHealthTransport{checker.Client.Transport, e.Proxy, t}
	t.Cleanup(func() { e.Shutdown(context.Background()); cancel() })
	if _, err := git.Select(ctx, "commit", "main", "*"); err != nil {
		t.Fatal(err)
	}
	if err := e.Deploy(ctx, revision.Selection{SHA: sha}); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(e.Proxy)
	defer front.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = 16
	transport.MaxConnsPerHost = 16
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	trafficCtx, stopTraffic := context.WithCancel(ctx)
	var workers sync.WaitGroup
	var requests, outages, epoch atomic.Uint64
	failures := make(chan string, 1)
	for n := 0; n < 8; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for trafficCtx.Err() == nil {
				before := epoch.Load()
				req, _ := http.NewRequestWithContext(trafficCtx, "GET", front.URL, nil)
				res, err := client.Do(req)
				body := ""
				status := 0
				if err == nil {
					b, readErr := io.ReadAll(res.Body)
					_ = res.Body.Close()
					body, status, err = string(b), res.StatusCode, readErr
				}
				if trafficCtx.Err() != nil {
					return
				}
				if err == nil && status == 200 && strings.HasPrefix(body, "good-") {
					requests.Add(1)
					continue
				}
				if before%2 == 1 || before != epoch.Load() {
					outages.Add(1)
					continue
				}
				select {
				case failures <- fmt.Sprintf("unexpected serving failure: status=%d body=%q err=%v", status, body, err):
				default:
				}
				return
			}
		}()
	}
	defer func() { stopTraffic(); workers.Wait() }()
	started := time.Now()
	end := started.Add(*soak)
	var baseline runtime.MemStats
	baseGoroutines := 0
	cycles := 0
	for time.Now().Before(end) {
		cycles++
		version := fmt.Sprintf("good-%d", cycles)
		if cycles%10 == 0 {
			version = "bad"
		}
		if cycles%10 == 5 {
			version = "stall"
		}
		commitVersion(t, origin, version)
		sel, err := git.Select(ctx, "commit", "main", "*")
		if err != nil {
			t.Fatal(err)
		}
		err = e.Deploy(ctx, sel)
		if strings.HasPrefix(version, "good-") != (err == nil) {
			t.Fatalf("candidate %q: %v", version, err)
		}
		if cycles%8 == 0 {
			e.mu.Lock()
			generation := e.active.generation
			e.mu.Unlock()
			if err := e.Replace(ctx, generation); err != nil {
				t.Fatal(err)
			}
		}
		if cycles%12 == 0 {
			for failure := 0; failure < 2; failure++ {
				epoch.Add(1)
				e.mu.Lock()
				active := e.active
				e.mu.Unlock()
				_ = active.Instance.Stop(ctx)
				if err := e.Recover(ctx, active.generation); err != nil {
					t.Fatal(err)
				}
				epoch.Add(1)
			}
		}
		select {
		case err := <-failures:
			t.Fatal(err)
		default:
		}
		for _, name := range []string{"instances", "prepared", "deployments"} {
			entries, err := os.ReadDir(filepath.Join(data, name))
			// A rejected candidate may retain one prepared cache for retry.
			limit := 1
			if name == "prepared" {
				limit = 3
			}
			if name == "deployments" {
				limit = 0
			}
			if err != nil || len(entries) > limit {
				t.Fatalf("%s grows without cleanup: %d %v", name, len(entries), err)
			}
		}
		if cycles%20 == 0 {
			runtime.GC()
			var current runtime.MemStats
			runtime.ReadMemStats(&current)
			goroutines := runtime.NumGoroutine()
			if baseGoroutines == 0 {
				baseline = current
				baseGoroutines = goroutines
			}
			if current.HeapAlloc > baseline.HeapAlloc+16<<20 || goroutines > baseGoroutines+64 {
				t.Fatalf("resources grow across deployments: heap=%d baseline=%d goroutines=%d baseline=%d", current.HeapAlloc, baseline.HeapAlloc, goroutines, baseGoroutines)
			}
			t.Logf("elapsed=%s cycles=%d successful_requests=%d expected_outage_requests=%d heap_bytes=%d goroutines=%d", time.Since(started).Round(time.Second), cycles, requests.Load(), outages.Load(), current.HeapAlloc, goroutines)
		}
	}
	stopTraffic()
	workers.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	t.Logf("completed duration=%s cycles=%d successful_requests=%d expected_outage_requests=%d", time.Since(started), cycles, requests.Load(), outages.Load())
	if cycles < 20 || requests.Load() < 1000 {
		t.Fatal("soak did not exercise enough deployments and traffic")
	}
}
