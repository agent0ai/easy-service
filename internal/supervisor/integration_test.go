package supervisor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/sandbox"
	"github.com/example/easy-service/internal/state"
)

// Only execution is replaced: Git, private filesystem copies, preparation,
// readiness/liveness, proxy routing, controller ordering and state are real.
type httpRuntime struct {
	sandbox.Runtime
	mu               sync.Mutex
	starts, restarts int
}

type httpInstance struct {
	server       *httptest.Server
	bundle, root string
	done         chan error
	once         sync.Once
	cancel       context.CancelFunc
}

func (i *httpInstance) Exited() bool {
	select {
	case <-i.done:
		return true
	default:
		return false
	}
}
func (i *httpInstance) Done() <-chan error           { return i.done }
func (i *httpInstance) Endpoint() string             { return i.server.URL }
func (i *httpInstance) BundlePath() string           { return i.bundle }
func (i *httpInstance) RootPath() string             { return i.root }
func (i *httpInstance) MemoryUsage() (uint64, error) { return 0, nil }
func (i *httpInstance) Stop(context.Context) error {
	i.once.Do(func() {
		i.cancel()
		i.server.CloseClientConnections()
		i.server.Close()
		close(i.done)
	})
	return nil
}
func (i *httpInstance) Kill(ctx context.Context) error { return i.Stop(ctx) }
func (r *httpRuntime) Start(op, lifetime context.Context, id, prepared, _ string, _ []string) (Instance, error) {
	bundle := filepath.Join(r.Data, "instances", id)
	if err := sandbox.CopyTree(op, filepath.Join(prepared, "rootfs"), filepath.Join(bundle, "rootfs")); err != nil {
		_ = os.RemoveAll(bundle)
		return nil, err
	}
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	return r.launch(lifetime, id, bundle)
}
func (r *httpRuntime) Restart(lifetime context.Context, id, bundle, _ string, _ []string) (Instance, error) {
	r.mu.Lock()
	r.restarts++
	r.mu.Unlock()
	return r.launch(lifetime, id, bundle)
}
func (r *httpRuntime) launch(lifetime context.Context, id, bundle string) (Instance, error) {
	b, err := os.ReadFile(filepath.Join(bundle, "rootfs", "app", "version"))
	if err != nil {
		return nil, err
	}
	version := string(b)
	ctx, cancel := context.WithCancel(lifetime)
	i := &httpInstance{bundle: bundle, root: filepath.Join(r.Data, "runsc-root", id), done: make(chan error), cancel: cancel}
	i.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			switch version {
			case "bad":
				w.WriteHeader(500)
				return
			case "stall":
				<-req.Context().Done()
				return
			}
		}
		if req.URL.Path == "/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", version)
			w.(http.Flusher).Flush()
			<-req.Context().Done()
			return
		}
		fmt.Fprint(w, version)
	}))
	i.server.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	i.server.Start()
	go func() {
		select {
		case <-ctx.Done():
			_ = i.Stop(context.Background())
		case <-i.Done():
		}
	}()
	return i, nil
}

func TestStreamingDeploymentCutoverAndDrain(t *testing.T) {
	for _, disconnect := range []bool{true, false} {
		t.Run(fmt.Sprintf("client_disconnect=%t", disconnect), func(t *testing.T) {
			origin := t.TempDir()
			if b, err := exec.Command("git", "init", "-b", "main", origin).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, b)
			}
			first := commitVersion(t, origin, "one")
			data := t.TempDir()
			cfg := config.Config{DataDir: data, GitURL: origin, RuntimeImage: "test-image", StartupTimeout: time.Second, HealthInterval: 100 * time.Millisecond, HealthFailures: 3}
			git := revision.New(origin, "", data)
			ctx, cancel := context.WithCancel(context.Background())
			e := &Engine{Cfg: cfg, Runtime: &httpRuntime{Runtime: sandbox.Runtime{Data: data}}, Checkout: git, Proxy: proxy.New(), Store: state.Store{Data: data}, Health: health.New(cfg.HealthInterval, "/health"), Rootfs: t.TempDir(), Digest: "sha256:test", Drain: 300 * time.Millisecond, RuntimeContext: ctx, Failures: make(chan uint64, 1)}
			t.Cleanup(func() { e.Shutdown(context.Background()); cancel() })
			if _, err := git.Select(ctx, "commit", "main", "*"); err != nil {
				t.Fatal(err)
			}
			if err := e.Deploy(ctx, revision.Selection{SHA: first}); err != nil {
				t.Fatal(err)
			}
			old := e.active
			front := httptest.NewServer(e.Proxy)
			defer front.Close()
			front.Client().Timeout = 2 * time.Second
			stream, err := front.Client().Get(front.URL + "/stream")
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			firstEvent := make([]byte, len("data: one\n\n"))
			if _, err := io.ReadFull(stream.Body, firstEvent); err != nil || string(firstEvent) != "data: one\n\n" {
				t.Fatalf("initial event: %q %v", firstEvent, err)
			}
			second := commitVersion(t, origin, "two")
			if _, err := git.Select(ctx, "commit", "main", "*"); err != nil {
				t.Fatal(err)
			}
			// A durable-state failure must leave existing traffic and its
			// watcher/process serving, even after the candidate became ready.
			goodStore := e.Store
			blockedState := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blockedState, []byte("blocked"), 0600); err != nil {
				t.Fatal(err)
			}
			e.Store = state.Store{Data: blockedState}
			rejected := e.Deploy(ctx, revision.Selection{SHA: second})
			e.Store = goodStore
			if rejected == nil || e.active != old || e.Proxy.Current() != old.Backend || old.Instance.Exited() {
				t.Fatal("state publication failure replaced or stopped the serving deployment")
			}
			saved, err := goodStore.Load()
			if err != nil || saved.Revision != first {
				t.Fatalf("failed cutover changed durable revision: state=%+v err=%v", saved, err)
			}
			res, err := front.Client().Get(front.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil || string(body) != "one" {
				t.Fatalf("failed cutover interrupted serving: body=%q err=%v", body, err)
			}
			deployed := make(chan error, 1)
			go func() { deployed <- e.Deploy(ctx, revision.Selection{SHA: second}) }()
			waitFor(t, func() bool { return e.Selected() == second })
			if old.Instance.Exited() {
				t.Fatal("cutover terminated the old stream before drain")
			}
			res, err = front.Client().Get(front.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil || string(body) != "two" {
				t.Fatalf("new request did not cut over: %q %v", body, err)
			}
			if disconnect {
				_ = stream.Body.Close()
			}
			select {
			case err := <-deployed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stream blocked deployment beyond the drain deadline")
			}
			if !old.Instance.Exited() {
				t.Fatal("old backend survived drain completion/deadline")
			}
			if _, err := os.Stat(old.Instance.BundlePath()); !os.IsNotExist(err) {
				t.Fatal("old writable filesystem retained after drain")
			}
			if !disconnect {
				// SIGTERM may let the application finish with a normal EOF or
				// terminate its socket abruptly; both must end the stream promptly.
				if _, err := io.ReadAll(stream.Body); err != nil {
					if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
						t.Fatal("stream remained open after drain expiry:", err)
					}
				}
			}
		})
	}
}

func commitVersion(t *testing.T, dir, version string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, b)
		}
		return string(b)
	}
	git("add", "version")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", version)
	return strings.TrimSpace(git("rev-parse", "HEAD"))
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLocalGitBadCommitsStallsCrashesAndFreshRecovery(t *testing.T) {
	origin := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main", origin)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, b)
	}
	first := commitVersion(t, origin, "one")
	t.Setenv("GIT_URL", origin)
	t.Setenv("RUNTIME_IMAGE", "test-image")
	t.Setenv("RUN_COMMAND", "test-boundary")
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("POLL_INTERVAL", "0.01")
	t.Setenv("HEALTH_INTERVAL", "0.01")
	t.Setenv("STARTUP_TIMEOUT", "0.1")
	t.Setenv("HEALTH_FAILURES", "2")
	t.Setenv("SERVICE_MEMORY_LIMIT", "0")
	t.Setenv("HEALTH_PATH", "/health")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	rt := &httpRuntime{Runtime: sandbox.Runtime{Data: cfg.DataDir}}
	git := revision.New(cfg.GitURL, "", cfg.DataDir)
	e := &Engine{Cfg: cfg, Runtime: rt, Checkout: git, Proxy: proxy.New(), Store: state.Store{Data: cfg.DataDir}, Health: health.New(cfg.HealthInterval, cfg.HealthPath), Rootfs: t.TempDir(), Digest: "sha256:test", Drain: 100 * time.Millisecond, Failures: make(chan uint64, 1)}
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	e.RuntimeContext = runtimeCtx
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c := Controller{Cfg: cfg, Selector: git, Engine: e}
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; e.Shutdown(context.Background()); cancelRuntime() })
	front := httptest.NewServer(e.Proxy)
	defer front.Close()
	front.Client().Timeout = time.Second
	request := func() string {
		res, err := front.Client().Get(front.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("serving interrupted: status=%d err=%v", res.StatusCode, err)
		}
		return string(b)
	}
	waitFor(t, func() bool { return e.Selected() == first })
	for _, version := range []string{"bad", "stall"} {
		rt.mu.Lock()
		before := rt.starts
		rt.mu.Unlock()
		commitVersion(t, origin, version)
		waitFor(t, func() bool { rt.mu.Lock(); defer rt.mu.Unlock(); return rt.starts > before })
		for n := 0; n < 20; n++ {
			if got := request(); got != "one" {
				t.Fatalf("rejected commit was routed: %q", got)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	second := commitVersion(t, origin, "two")
	waitFor(t, func() bool { return e.Selected() == second })
	if got := request(); got != "two" {
		t.Fatal(got)
	}
	for n := 0; n < 2; n++ {
		e.mu.Lock()
		active := e.active
		e.mu.Unlock()
		marker := filepath.Join(active.Instance.BundlePath(), "rootfs", "app", "ephemeral")
		if n == 0 {
			if err := os.WriteFile(marker, []byte("writable"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		_ = active.Instance.Stop(context.Background())
		waitFor(t, func() bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.active.generation != active.generation && e.Proxy.Current() != nil
		})
		e.mu.Lock()
		recovered := e.active
		e.mu.Unlock()
		_, err := os.Stat(filepath.Join(recovered.Instance.BundlePath(), "rootfs", "app", "ephemeral"))
		if n == 0 && err != nil {
			t.Fatal("restart discarded writable installation")
		}
		if n == 1 && !os.IsNotExist(err) {
			t.Fatal("fresh redeploy retained ephemeral application writes")
		}
		if got := request(); got != "two" {
			t.Fatal(got)
		}
	}
	rt.mu.Lock()
	restarts := rt.restarts
	rt.mu.Unlock()
	if restarts != 1 {
		t.Fatalf("restart allowance was not preserved: %d", restarts)
	}
	saved, err := e.Store.Load()
	if err != nil || saved.Revision != second || !saved.Recoverable(cfg.GitURL, cfg.RuntimeImage) || saved.Recoverable(origin+"-different", cfg.RuntimeImage) {
		t.Fatalf("deployment/recovery did not preserve exact source identity: state=%+v err=%v", saved, err)
	}
	for _, name := range []string{"prepared", "instances"} {
		entries, err := os.ReadDir(filepath.Join(cfg.DataDir, name))
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s retained failed/retired data: count=%d err=%v", name, len(entries), err)
		}
	}
}
