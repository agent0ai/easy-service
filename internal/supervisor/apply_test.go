package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/sandbox"
	"github.com/example/easy-service/internal/state"
)

type applyFixture struct {
	t       *testing.T
	base    map[string]string
	c       *Controller
	runtime *settingsRuntime
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	origin := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	commitVersion(t, origin, "one")
	base := maps.Clone(config.Defaults)
	base["GIT_URL"], base["RUN_COMMAND"] = origin, "serve"
	base["DATA_DIR"], base["CONFIG_DIR"], base["LOG_DIR"] = t.TempDir(), t.TempDir(), t.TempDir()
	base["POLL_INTERVAL"], base["HEALTH_INTERVAL"], base["HEALTH_PATH"], base["DRAIN_TIMEOUT"] = "3600", "0.2", "/health", "10"
	store := state.Store{Data: base["DATA_DIR"], ConfigDir: base["CONFIG_DIR"]}
	settings, _, err := config.Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(base, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &settingsRuntime{httpRuntime: &httpRuntime{Runtime: sandbox.Runtime{Data: cfg.DataDir}}}
	git := revision.New(origin, "", cfg.DataDir)
	e := &Engine{Cfg: cfg, Runtime: rt, Checkout: git, Proxy: proxy.New(), Store: store, Health: health.New(cfg.HealthInterval, cfg.HealthPath), Rootfs: t.TempDir(), Digest: "sha256:fixture", RuntimeContext: ctx, Drain: 10 * time.Second, Failures: make(chan uint64, 2)}
	c := &Controller{Cfg: cfg, Settings: settings, Engine: e, Selector: git}
	f := &applyFixture{t: t, base: base, c: c, runtime: rt, ctx: ctx, cancel: cancel}
	t.Cleanup(cancel)
	return f
}

func (f *applyFixture) start() {
	f.t.Helper()
	closeControl, err := f.c.StartControl(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	f.client = controlClient(f.t, f.c.Cfg.DataDir)
	done := make(chan struct{})
	go func() { f.c.Run(f.ctx); close(done) }()
	f.t.Cleanup(func() { f.cancel(); <-done; closeControl(); f.c.Engine.Shutdown(context.Background()) })
}

func (f *applyFixture) request(method, path string, body any, want int) []byte {
	f.t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	req, err := http.NewRequest(method, "http://local"+path, bytes.NewReader(data))
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	data, err = io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want {
		f.t.Fatalf("%s %s: status=%d body=%s err=%v", method, path, res.StatusCode, data, err)
	}
	return data
}

func (f *applyFixture) stage(values map[string]string) {
	f.t.Helper()
	f.request("POST", "/config", map[string]any{"values": values}, http.StatusOK)
}

func (f *applyFixture) submit() uint64 {
	f.t.Helper()
	var accepted config.ApplyResult
	if err := json.Unmarshal(f.request("POST", "/apply", nil, http.StatusAccepted), &accepted); err != nil || accepted.ID == 0 || accepted.State != "accepted" {
		f.t.Fatal("submission missing", accepted, err)
	}
	return accepted.ID
}

func TestQueuedApplyCoalescesDuringDrain(t *testing.T) {
	f := newApplyFixture(t)
	f.start()
	e, settings := f.c.Engine, f.c.Settings
	waitFor(t, func() bool { return e.Status().State == "running" })
	old := e.Status().Instance
	front := httptest.NewServer(e.Proxy)
	defer front.Close()
	stream, err := front.Client().Get(front.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if _, err := io.ReadFull(stream.Body, make([]byte, len("data: one\n\n"))); err != nil {
		t.Fatal(err)
	}
	f.stage(map[string]string{"APP_FIRST": "one"})
	first := f.submit()
	waitFor(t, func() bool { return e.Status().Instance != old })
	if settings.ApplyStatus().Running != first {
		t.Fatal("apply did not remain running during drain")
	}
	// A queued waiter can disconnect while the previous apply is draining.
	f.stage(map[string]string{"APP_SECOND": "two"})
	waitCtx, stopWait := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	waitReq, _ := http.NewRequestWithContext(waitCtx, "POST", "http://local/apply?wait=true", nil)
	go func() {
		res, err := f.client.Do(waitReq)
		if res != nil {
			res.Body.Close()
		}
		waitDone <- err
	}()
	waitFor(t, func() bool { return settings.ApplyStatus().Queued != 0 })
	second := settings.ApplyStatus().Queued
	stopWait()
	if err := <-waitDone; err == nil {
		t.Fatal("waiter unexpectedly completed while draining")
	}
	// This waiter observes supersession, not completion of some later request.
	replaced, err := settings.SubmitApply()
	if err != nil || replaced.ID != second {
		t.Fatal("duplicate pending submission", err)
	}
	f.stage(map[string]string{"APP_SECOND": "three"})
	third := f.submit()
	result, err := replaced.Wait(f.ctx)
	if err != nil || result.State != "superseded" {
		t.Fatal("replaced waiter", result, err)
	}
	f.stage(map[string]string{"APP_DRAFT": "not-submitted"})
	var status Status
	if err := json.Unmarshal(f.request("GET", "/status", nil, 200), &status); err != nil || status.Apply.Running != first || status.Apply.Queued != third || !status.ConfigPending {
		t.Fatal("queue not exposed in status", status.Apply, err)
	}
	if f.runtime.count() != 2 || len(status.Instances) != 2 {
		t.Fatal("queued candidates started during drain", f.runtime.count(), status.Instances)
	}
	// The healthy candidate is already serving while its predecessor drains.
	res, err := front.Client().Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || string(body) != "one" {
		t.Fatal("cutover stopped serving", err)
	}
	stream.Body.Close()
	waitFor(t, func() bool { s := settings.ApplyStatus(); return s.Last != nil && s.Last.ID == third && s.Running == 0 })
	if f.runtime.count() != 3 || settings.Applied()["APP_FIRST"] != "one" || settings.Applied()["APP_SECOND"] != "three" || settings.Applied()["APP_DRAFT"] != "" || settings.Pending()["APP_DRAFT"] != "not-submitted" {
		t.Fatal("coalescing lost a submitted setting or included a draft", f.runtime.count())
	}
}

func TestApplyWaitDisconnectAndQueuedRepair(t *testing.T) {
	f := newApplyFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.c.Configure = func(ctx context.Context, cfg config.Config) (Deployment, Selector, error) {
		if cfg.RunCommand == "blocked-failure" {
			close(entered)
			select {
			case <-release:
				return Deployment{}, nil, errors.New("candidate rejected")
			case <-ctx.Done():
				return Deployment{}, nil, ctx.Err()
			}
		}
		plan := f.c.Engine.snapshot()
		plan.Cfg = cfg
		return plan, f.c.Selector, nil
	}
	f.start()
	waitFor(t, func() bool { return f.c.Engine.Status().State == "running" })
	old := f.c.Engine.Status().Instance
	f.stage(map[string]string{"RUN_COMMAND": "blocked-failure"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://local/apply?wait=true", nil)
	waitDone := make(chan error, 1)
	go func() {
		res, err := f.client.Do(req)
		if res != nil {
			res.Body.Close()
		}
		waitDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wait submission not started")
	}
	failed, err := f.c.Settings.SubmitApply()
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-waitDone; err == nil {
		t.Fatal("wait request did not disconnect")
	}
	if f.c.Settings.ApplyStatus().Running != failed.ID || f.c.Engine.Status().Instance != old {
		t.Fatal("detaching waiter stopped deployment or current app")
	}
	f.stage(map[string]string{"RUN_COMMAND": "serve", "APP_REPAIR": "yes"})
	queued := f.submit()
	close(release)
	result, err := failed.Wait(f.ctx)
	if err != nil || result.State != "failed" || !strings.Contains(result.Error, "candidate rejected") {
		t.Fatal("detached apply was cancelled or lost its failure", result, err)
	}
	waitFor(t, func() bool {
		s := f.c.Settings.ApplyStatus()
		return s.Last != nil && s.Last.ID == queued && s.Last.State == "completed"
	})
	if f.c.Engine.Status().Instance == old || f.c.Settings.Applied()["APP_REPAIR"] != "yes" || f.runtime.count() != 2 {
		t.Fatal("queued repair failed")
	}
	// Explicit waiting still returns failure; no automatic retry of this snapshot.
	f.stage(map[string]string{"RUN_COMMAND": "fail-start"})
	if body := f.request("POST", "/apply?wait=true", nil, 500); !strings.Contains(string(body), "configured command failed") {
		t.Fatal("apply failure reason missing")
	}
}

func TestControllerResumesSubmittedApplyWithoutDraft(t *testing.T) {
	f := newApplyFixture(t)
	if err := f.c.Settings.Stage(map[string]string{"APP_VALUE": "submitted"}, nil); err != nil {
		t.Fatal(err)
	}
	submitted, err := f.c.Settings.SubmitApply()
	if err != nil {
		t.Fatal(err)
	}
	f.c.Settings.TakeApply()
	if err := f.c.Settings.Stage(map[string]string{"APP_VALUE": "draft"}, nil); err != nil {
		t.Fatal(err)
	}
	f.c.Settings, _, err = config.Open(f.base, f.c.Engine.Store)
	if err != nil {
		t.Fatal(err)
	}
	// Start unconfigured, as on a restart after the first apply was interrupted.
	f.c.Selector = nil
	f.c.Configure = func(_ context.Context, cfg config.Config) (Deployment, Selector, error) {
		plan := f.c.Engine.snapshot()
		plan.Cfg = cfg
		return plan, f.c.Engine.Checkout.(Selector), nil
	}
	f.start()
	waitFor(t, func() bool {
		s := f.c.Settings.ApplyStatus()
		return s.Last != nil && s.Last.ID == submitted.ID && s.Last.State == "completed"
	})
	if f.runtime.count() != 1 || f.c.Settings.Applied()["APP_VALUE"] != "submitted" || f.c.Settings.Pending()["APP_VALUE"] != "draft" {
		t.Fatal("restart lost submission or activated unsubmitted draft")
	}
}

func TestQueuedApplyProgressesAfterFailedInitialRecovery(t *testing.T) {
	f := newApplyFixture(t)
	bad := commitVersion(t, f.base["GIT_URL"], "bad")
	if _, err := f.c.Selector.Select(f.ctx, "commit", "main", "*"); err != nil {
		t.Fatal(err)
	}
	f.c.Initial = &revision.Selection{SHA: bad}
	f.c.Cfg.StartupTimeout = 100 * time.Millisecond
	f.c.Engine.Cfg.StartupTimeout = 100 * time.Millisecond
	if err := f.c.Settings.Stage(map[string]string{"HEALTH_PATH": "/"}, nil); err != nil {
		t.Fatal(err)
	}
	submitted, err := f.c.Settings.SubmitApply()
	if err != nil {
		t.Fatal(err)
	}
	f.start()
	waitFor(t, func() bool {
		s := f.c.Settings.ApplyStatus()
		return s.Last != nil && s.Last.ID == submitted.ID && s.Last.State == "completed"
	})
	if f.runtime.count() != 2 || f.c.Engine.Selected() != bad || f.c.Engine.Status().State != "running" {
		t.Fatal("failed initial recovery blocked queued repair or was skipped")
	}
}

func TestSupervisorShutdownPreservesRunningSubmission(t *testing.T) {
	f := newApplyFixture(t)
	entered := make(chan struct{})
	f.c.Configure = func(ctx context.Context, _ config.Config) (Deployment, Selector, error) {
		close(entered)
		<-ctx.Done()
		return Deployment{}, nil, ctx.Err()
	}
	f.c.Selector = nil
	if err := f.c.Settings.Stage(map[string]string{"APP_VALUE": "submitted"}, nil); err != nil {
		t.Fatal(err)
	}
	submitted, err := f.c.Settings.SubmitApply()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { f.c.Run(f.ctx); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("apply not started")
	}
	f.cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not stop")
	}
	reopened, _, err := config.Open(f.base, f.c.Engine.Store)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.TakeApply(); got == nil || got.ID != submitted.ID || got.Overrides["APP_VALUE"] != "submitted" || reopened.ApplyStatus().Last != nil {
		t.Fatal("shutdown converted interrupted apply into permanent failure")
	}
}
