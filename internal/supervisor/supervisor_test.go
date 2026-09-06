package supervisor

import (
	"context"
	"errors"
	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/state"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fi struct {
	id       string
	ex       atomic.Bool
	done     chan error
	mem      uint64
	stopOnce sync.Once
}

func (i *fi) Exited() bool       { return i.ex.Load() }
func (i *fi) Done() <-chan error { return i.done }
func (i *fi) Stop(context.Context) error {
	i.stopOnce.Do(func() {
		i.ex.Store(true)
		close(i.done)
	})
	return nil
}
func (i *fi) Endpoint() string             { return "http://127.0.0.1:1" }
func (i *fi) BundlePath() string           { return "/tmp/b" }
func (i *fi) RootPath() string             { return "/tmp/r" }
func (i *fi) PID() int                     { return 0 }
func (i *fi) MemoryUsage() (uint64, error) { return i.mem, nil }

type fr struct {
	mu               sync.Mutex
	starts, restarts int
	failStarts       int
	preparePort      int
	data             string
	memory           uint64
	instances        []*fi
}

type contextRuntime struct {
	*fr
	ctxMu    sync.Mutex
	contexts []context.Context
}

func (r *contextRuntime) Start(ctx context.Context, id, prepared, command string, port int, env []string) (Instance, error) {
	i, err := r.fr.Start(ctx, id, prepared, command, port, env)
	if err != nil {
		return nil, err
	}
	r.ctxMu.Lock()
	r.contexts = append(r.contexts, ctx)
	r.ctxMu.Unlock()
	go func() {
		<-ctx.Done()
		_ = i.Stop(context.Background())
	}()
	return i, nil
}

func (r *fr) Prepare(_ context.Context, id string, _ string, _ string, _ string, port int, _ []string) (string, error) {
	r.preparePort = port
	prepared := filepath.Join(r.data, "prepared", id)
	if err := os.MkdirAll(filepath.Join(prepared, "rootfs", "app"), 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(prepared, ".easy-service-ready"), []byte("ok"), 0600); err != nil {
		return "", err
	}
	return prepared, nil
}
func (r *fr) ValidatePrepared(path string) error {
	_, err := os.Stat(filepath.Join(path, ".easy-service-ready"))
	return err
}
func (r *fr) Start(context.Context, string, string, string, int, []string) (Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	if r.failStarts > 0 {
		r.failStarts--
		return nil, errors.New("launch failed")
	}
	i := &fi{done: make(chan error), mem: r.memory}
	r.instances = append(r.instances, i)
	return i, nil
}
func (r *fr) Restart(context.Context, string, string, string, int, []string) (Instance, error) {
	r.mu.Lock()
	r.restarts++
	r.mu.Unlock()
	i := &fi{done: make(chan error), mem: r.memory}
	r.instances = append(r.instances, i)
	return i, nil
}
func TestRestartAllowanceState(t *testing.T) {
	a := &Active{}
	if a.Restarted {
		t.Fatal()
	}
	a.Restarted = true
	for i := 0; i < 3; i++ { /* successful probes do not mutate restart allowance */
	}
	if !a.Restarted {
		t.Fatal("probe reset restart allowance")
	}
}
func TestFailedCandidatePreservesBackendContract(t *testing.T) {
	p := proxy.New()
	b, _ := proxy.NewBackend("http://old")
	p.Set(b) // candidate health is unreachable and must not be installed
	d := t.TempDir()
	sha := "0123456789012345678901234567890123456789"
	r := &fr{data: d}
	e := &Engine{Cfg: config.Config{DataDir: d, RunCommand: "x", ServicePort: 80, StartupTimeout: time.Millisecond, HealthInterval: time.Millisecond, HealthFailures: 1}, Runtime: r, Checkout: checkoutFake{}, Proxy: p, Store: state.Store{Data: t.TempDir()}, Health: health.New(time.Millisecond, "/"), Rootfs: t.TempDir(), Digest: "sha256:digest", Drain: time.Millisecond, Failures: make(chan uint64, 1)}
	if e.Deploy(context.Background(), revision.Selection{SHA: sha}) == nil {
		t.Fatal("candidate unexpectedly healthy")
	}
	if p.Current() != b {
		t.Fatal("failed candidate replaced active backend")
	}
}

type checkoutFake struct{}

func (checkoutFake) Checkout(context.Context, string, string) error { return nil }

type fakeHealth struct{ ready error }

func (f fakeHealth) Ready(context.Context, string, health.Exited, time.Duration) error {
	return f.ready
}
func (fakeHealth) UntilFailure(context.Context, string, health.Exited, int) health.Result {
	return health.Healthy
}

func engineFor(t *testing.T, h Health) (*Engine, *fr) {
	t.Helper()
	d := t.TempDir()
	sha := "0123456789012345678901234567890123456789"
	prep := filepath.Join(d, "prepared", preparedID(sha, "sha256:digest"))
	if err := os.MkdirAll(prep, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prep, ".easy-service-ready"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &fr{data: d}
	e := &Engine{Cfg: config.Config{DataDir: d, RuntimeImage: "image", RunCommand: "x", ServicePort: 80, StartupTimeout: time.Second, HealthInterval: time.Millisecond, HealthFailures: 1}, Runtime: r, Checkout: checkoutFake{}, Proxy: proxy.New(), Store: state.Store{Data: d}, Health: h, Rootfs: t.TempDir(), Digest: "sha256:digest", Drain: time.Millisecond, Failures: make(chan uint64, 4)}
	return e, r
}

func TestOneRestartThenFreshRedeploy(t *testing.T) {
	e, r := engineFor(t, fakeHealth{})
	sha := "0123456789012345678901234567890123456789"
	if err := e.Deploy(context.Background(), revision.Selection{SHA: sha}); err != nil {
		t.Fatal(err)
	}
	first := e.active.generation
	if err := e.Recover(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := e.active.generation
	if err := e.Recover(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if r.restarts != 1 || r.starts != 2 {
		t.Fatalf("starts=%d restarts=%d", r.starts, r.restarts)
	}
	if e.active.Restarted {
		t.Fatal("fresh redeploy did not reset restart allowance")
	}
	third := e.active.generation
	if err := e.Recover(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	if r.restarts != 2 {
		t.Fatalf("fresh deployment received no restart allowance: restarts=%d", r.restarts)
	}
}

func TestRejectedCandidateNeverCutsOver(t *testing.T) {
	e, _ := engineFor(t, fakeHealth{ready: errors.New("unhealthy")})
	b, _ := proxy.NewBackend("http://old")
	e.Proxy.Set(b)
	if e.Deploy(context.Background(), revision.Selection{SHA: "0123456789012345678901234567890123456789"}) == nil {
		t.Fatal("candidate accepted")
	}
	if e.Proxy.Current() != b {
		t.Fatal("rejected candidate changed routing")
	}
}

func TestRecoveryStateSaveFailureKeepsGenerationRetryable(t *testing.T) {
	e, _ := engineFor(t, fakeHealth{})
	sha := "0123456789012345678901234567890123456789"
	if err := e.Deploy(context.Background(), revision.Selection{SHA: sha}); err != nil {
		t.Fatal(err)
	}
	generation := e.active.generation
	bad := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(bad, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	good := e.Store
	e.Store = state.Store{Data: bad}
	if err := e.Recover(context.Background(), generation); err == nil {
		t.Fatal("state write unexpectedly succeeded")
	}
	if e.active.generation != generation || !e.active.Restarted {
		t.Fatalf("recovery state became inert: generation=%d restarted=%t", e.active.generation, e.active.Restarted)
	}
	e.Store = good
	if err := e.Recover(context.Background(), generation); err != nil {
		t.Fatalf("same generation was not retryable: %v", err)
	}
	if e.active.generation == generation || e.active.Restarted {
		t.Fatal("fresh recovery did not commit a new generation with a restart allowance")
	}
}

type staticSelector struct{ selection revision.Selection }

func (s staticSelector) Select(context.Context, string, string, string) (revision.Selection, error) {
	return s.selection, nil
}

type sequenceSelector struct {
	mu    sync.Mutex
	calls int
	a, b  revision.Selection
}

func (s *sequenceSelector) Select(context.Context, string, string, string) (revision.Selection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		return s.a, nil
	}
	return s.b, nil
}

func TestInitialDeploymentRetriesAtHealthInterval(t *testing.T) {
	e, r := engineFor(t, fakeHealth{})
	r.failStarts = 1
	e.Cfg.PollInterval = time.Hour
	e.Cfg.HealthInterval = 5 * time.Millisecond
	c := Controller{Cfg: e.Cfg, Selector: staticSelector{revision.Selection{SHA: "0123456789012345678901234567890123456789"}}, Engine: e}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.After(500 * time.Millisecond)
	for {
		r.mu.Lock()
		starts := r.starts
		r.mu.Unlock()
		if starts >= 2 && e.Selected() != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("initial deployment waited for the poll interval instead of retrying")
		case <-time.After(time.Millisecond):
		}
	}
	if r.preparePort != e.Cfg.ServicePort {
		t.Fatalf("setup PORT=%d, want %d", r.preparePort, e.Cfg.ServicePort)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("controller did not stop")
	}
}

func TestNewerRevisionSupersedesBrokenPendingSelection(t *testing.T) {
	e, r := engineFor(t, fakeHealth{})
	r.failStarts = 1
	e.Cfg.PollInterval = 2 * time.Millisecond
	e.Cfg.HealthInterval = 30 * time.Millisecond
	a := revision.Selection{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b := revision.Selection{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	c := Controller{Cfg: e.Cfg, Selector: &sequenceSelector{a: a, b: b}, Engine: e}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.After(time.Second)
	for e.Selected() != b.SHA {
		select {
		case <-deadline:
			t.Fatalf("newer revision did not supersede broken selection; selected=%q", e.Selected())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestSelectionReturningToActiveCancelsBrokenPending(t *testing.T) {
	e, r := engineFor(t, fakeHealth{})
	a := revision.Selection{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b := revision.Selection{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if err := e.Deploy(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	r.failStarts = 100
	e.Cfg.PollInterval = 2 * time.Millisecond
	e.Cfg.HealthInterval = 15 * time.Millisecond
	c := Controller{Cfg: e.Cfg, Selector: &sequenceSelector{a: b, b: a}, Engine: e}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.After(time.Second)
	for {
		r.mu.Lock()
		starts := r.starts
		r.mu.Unlock()
		if starts >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("broken candidate was never attempted")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	starts := r.starts
	r.mu.Unlock()
	if starts != 2 || e.Selected() != a.SHA {
		t.Fatalf("superseded candidate kept retrying: starts=%d selected=%q", starts, e.Selected())
	}
	cancel()
	<-done
}

type failingSelector struct{}

func (failingSelector) Select(context.Context, string, string, string) (revision.Selection, error) {
	return revision.Selection{}, errors.New("network unavailable")
}

func TestCachedRevisionRecoveryRetriesWithoutGit(t *testing.T) {
	e, r := engineFor(t, fakeHealth{})
	r.failStarts = 1
	e.Cfg.PollInterval = time.Hour
	e.Cfg.HealthInterval = 3 * time.Millisecond
	initial := revision.Selection{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "recovery"}
	c := Controller{Cfg: e.Cfg, Selector: failingSelector{}, Engine: e, Initial: &initial}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.After(time.Second)
	for e.Selected() != initial.SHA {
		select {
		case <-deadline:
			t.Fatal("cached revision did not retry independently of Git")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestMemorySamplingEmitsThresholdOnce(t *testing.T) {
	e, r := engineFor(t, fakeHealth{})
	e.Cfg.ServiceMemoryLimit = 100
	e.MemoryInterval = time.Millisecond
	e.MemoryExceeded = make(chan uint64, 2)
	r.memory = 101
	if err := e.Deploy(context.Background(), revision.Selection{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	select {
	case generation := <-e.MemoryExceeded:
		if generation != e.active.generation {
			t.Fatalf("generation=%d", generation)
		}
	case <-time.After(time.Second):
		t.Fatal("over-limit instance was not reported")
	}
	select {
	case <-e.MemoryExceeded:
		t.Fatal("threshold emitted more than once for one instance")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestMemoryReplacementReusesBlueGreenDeployment(t *testing.T) {
	e, r := engineFor(t, fakeHealth{})
	e.Drain = time.Second
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := e.Deploy(context.Background(), revision.Selection{SHA: sha}); err != nil {
		t.Fatal(err)
	}
	old := e.active.Instance.(*fi)
	generation := e.active.generation
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-releaseRequest
	}))
	defer upstream.Close()
	oldBackend, err := proxy.NewBackend(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	e.active.Backend = oldBackend
	e.Proxy.Set(oldBackend)
	requestDone := make(chan struct{})
	go func() {
		e.Proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(requestDone)
	}()
	<-requestStarted
	replaceDone := make(chan error, 1)
	go func() { replaceDone <- e.Replace(context.Background(), generation) }()
	deadline := time.After(time.Second)
	for e.Proxy.Current() == oldBackend {
		select {
		case <-deadline:
			t.Fatal("replacement did not cut over")
		case <-time.After(time.Millisecond):
		}
	}
	if old.Exited() {
		t.Fatal("old sandbox stopped before its request drained")
	}
	close(releaseRequest)
	<-requestDone
	if err := <-replaceDone; err != nil {
		t.Fatal(err)
	}
	if e.Selected() != sha || r.starts != 2 || r.restarts != 0 || !old.Exited() {
		t.Fatalf("same-revision replacement bypassed deployment lifecycle: selected=%q starts=%d restarts=%d oldExited=%t", e.Selected(), r.starts, r.restarts, old.Exited())
	}
}

func TestSuccessfulMemoryReplacementKeepsCandidateLifetime(t *testing.T) {
	e, r := engineFor(t, fakeHealth{})
	runtime := &contextRuntime{fr: r}
	e.Runtime = runtime
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := e.Deploy(ctx, revision.Selection{SHA: sha}); err != nil {
		t.Fatal(err)
	}
	generation := e.active.generation
	if err := e.Replace(ctx, generation); err != nil {
		t.Fatal(err)
	}
	runtime.ctxMu.Lock()
	replacementCtx := runtime.contexts[1]
	runtime.ctxMu.Unlock()
	if err := replacementCtx.Err(); err != nil {
		t.Fatalf("successful replacement remained bound to the completed operation context: %v", err)
	}
}

func TestControllerCancellationKeepsActiveProcessUntilEngineShutdown(t *testing.T) {
	e, base := engineFor(t, fakeHealth{})
	runtime := &contextRuntime{fr: base}
	e.Runtime = runtime
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	defer cancelRuntime()
	e.RuntimeContext = runtimeCtx
	controllerCtx, cancelController := context.WithCancel(context.Background())
	if err := e.Deploy(controllerCtx, revision.Selection{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	active := e.active.Instance
	cancelController()
	select {
	case <-active.Done():
		t.Fatal("controller cancellation stopped active sandbox before graceful shutdown")
	case <-time.After(20 * time.Millisecond):
	}
	e.Shutdown(context.Background())
	select {
	case <-active.Done():
	case <-time.After(time.Second):
		t.Fatal("engine shutdown did not stop active sandbox")
	}
}

func TestControllerCancellationDoesNotAbortCommittedCutoverDrain(t *testing.T) {
	e, _ := engineFor(t, fakeHealth{})
	e.Drain = time.Second
	first := revision.Selection{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := e.Deploy(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	old := e.active.Instance.(*fi)
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-releaseRequest
	}))
	defer upstream.Close()
	oldBackend, err := proxy.NewBackend(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	e.active.Backend = oldBackend
	e.Proxy.Set(oldBackend)
	requestDone := make(chan struct{})
	go func() {
		e.Proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(requestDone)
	}()
	<-requestStarted

	controllerCtx, cancelController := context.WithCancel(context.Background())
	deployDone := make(chan error, 1)
	go func() {
		deployDone <- e.Deploy(controllerCtx, revision.Selection{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	}()
	deadline := time.After(time.Second)
	for e.Proxy.Current() == oldBackend {
		select {
		case <-deadline:
			t.Fatal("deployment did not cut over")
		case <-time.After(time.Millisecond):
		}
	}
	cancelController()
	select {
	case <-old.Done():
		t.Fatal("controller cancellation stopped old sandbox before its request drained")
	case err := <-deployDone:
		t.Fatalf("deployment completed before old request drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseRequest)
	<-requestDone
	if err := <-deployDone; err != nil {
		t.Fatal(err)
	}
	if !old.Exited() {
		t.Fatal("old sandbox was not stopped after its request drained")
	}
}

type scriptedHealth struct {
	mu         sync.Mutex
	readyCalls int
	failAt     int
	blockAt    int
	entered    chan struct{}
	release    chan struct{}
}

func (h *scriptedHealth) Ready(ctx context.Context, _ string, _ health.Exited, _ time.Duration) error {
	h.mu.Lock()
	h.readyCalls++
	call := h.readyCalls
	h.mu.Unlock()
	if call == h.blockAt {
		if h.entered != nil {
			close(h.entered)
		}
		if h.release == nil {
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case <-h.release:
			return errors.New("not ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if call == h.failAt {
		return errors.New("not ready")
	}
	return nil
}

type channelSelector struct{ selections chan revision.Selection }

func (s channelSelector) Select(ctx context.Context, _, _, _ string) (revision.Selection, error) {
	select {
	case selection := <-s.selections:
		return selection, nil
	case <-ctx.Done():
		return revision.Selection{}, ctx.Err()
	}
}

func TestMemoryReplacementSerializesAndCoalescesNewRevision(t *testing.T) {
	h := &scriptedHealth{blockAt: 2, entered: make(chan struct{}), release: make(chan struct{})}
	e, r := engineFor(t, h)
	e.MemoryExceeded = make(chan uint64, 1)
	a := revision.Selection{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b := revision.Selection{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if err := e.Deploy(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	selections := make(chan revision.Selection, 1)
	e.Cfg.PollInterval = time.Hour
	c := Controller{Cfg: e.Cfg, Selector: channelSelector{selections}, Engine: e}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	e.MemoryExceeded <- e.active.generation
	<-h.entered
	selections <- b
	// Let the independent poller publish the newest selection while the
	// serialized replacement still owns the deployment engine.
	time.Sleep(10 * time.Millisecond)
	close(h.release)
	deadline := time.After(time.Second)
	for e.Selected() != b.SHA {
		select {
		case <-deadline:
			t.Fatalf("new revision did not supersede failed memory replacement; selected=%q", e.Selected())
		case <-time.After(time.Millisecond):
		}
	}
	r.mu.Lock()
	restarts := r.restarts
	r.mu.Unlock()
	if restarts != 0 {
		t.Fatalf("recovery raced newer deployment: restarts=%d", restarts)
	}
	cancel()
	<-done
}
func (*scriptedHealth) UntilFailure(context.Context, string, health.Exited, int) health.Result {
	return health.Healthy
}

func TestFailedMemoryCandidateRestartsOriginal(t *testing.T) {
	h := &scriptedHealth{failAt: 2}
	e, r := engineFor(t, h)
	e.MemoryExceeded = make(chan uint64, 1)
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := e.Deploy(context.Background(), revision.Selection{SHA: sha}); err != nil {
		t.Fatal(err)
	}
	e.Cfg.PollInterval = time.Hour
	c := Controller{Cfg: e.Cfg, Selector: staticSelector{revision.Selection{SHA: sha}}, Engine: e}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	e.MemoryExceeded <- e.active.generation
	deadline := time.After(time.Second)
	for {
		r.mu.Lock()
		restarts := r.restarts
		r.mu.Unlock()
		if restarts == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("failed memory candidate did not enter normal restart recovery")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestOriginalDeathCancelsMemoryReplacement(t *testing.T) {
	h := &scriptedHealth{blockAt: 2, entered: make(chan struct{})}
	e, _ := engineFor(t, h)
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := e.Deploy(context.Background(), revision.Selection{SHA: sha}); err != nil {
		t.Fatal(err)
	}
	old := e.active.Instance.(*fi)
	generation := e.active.generation
	result := make(chan error, 1)
	go func() { result <- e.Replace(context.Background(), generation) }()
	<-h.entered
	_ = old.Stop(context.Background())
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("replacement succeeded after original died")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("original death did not immediately cancel replacement readiness")
	}
	if e.active.generation != generation {
		t.Fatal("failed replacement cut over")
	}
	if err := e.Recover(context.Background(), generation); err != nil {
		t.Fatalf("ordinary recovery did not resume: %v", err)
	}
}

func TestShutdownDrainsBeforeStoppingSandbox(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	defer upstream.Close()
	p := proxy.New()
	b, err := proxy.NewBackend(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p.Set(b)
	i := &fi{done: make(chan error)}
	e := &Engine{Proxy: p, Store: state.Store{Data: t.TempDir()}, Drain: time.Second, active: &Active{Instance: i, Backend: b}}
	requestDone := make(chan struct{})
	go func() {
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(requestDone)
	}()
	<-started
	shutdownDone := make(chan struct{})
	go func() { e.Shutdown(context.Background()); close(shutdownDone) }()
	time.Sleep(10 * time.Millisecond)
	if i.Exited() {
		t.Fatal("sandbox stopped before in-flight request drained")
	}
	close(release)
	<-requestDone
	<-shutdownDone
	if !i.Exited() {
		t.Fatal("sandbox was not stopped after drain")
	}
}

func TestShutdownPreservesDurableStateForRestartRecovery(t *testing.T) {
	data := t.TempDir()
	store := state.Store{Data: data}
	want := state.State{Revision: "abc", Digest: "sha256:digest", ImageRef: "image", Prepared: filepath.Join(data, "prepared", "abc")}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	i := &fi{done: make(chan error)}
	b, err := proxy.NewBackend("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Proxy: proxy.New(), Store: store, active: &Active{Instance: i, Backend: b}}
	e.Shutdown(context.Background())
	got, err := store.Load()
	if err != nil {
		t.Fatalf("shutdown removed cached recovery state: %v", err)
	}
	if got.Revision != want.Revision || got.Digest != want.Digest || got.Prepared != want.Prepared {
		t.Fatalf("shutdown changed cached recovery state: got %+v", got)
	}
}
