package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/example/easy-service/internal/logs"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/sandbox"
	"github.com/example/easy-service/internal/state"
)

type settingsRuntime struct {
	*httpRuntime
	mu     sync.Mutex
	starts int
	env    []string
}

func (r *settingsRuntime) Start(op, lifetime context.Context, id, prepared, command string, env []string) (Instance, error) {
	r.mu.Lock()
	r.starts++
	r.env = append([]string(nil), env...)
	r.mu.Unlock()
	if command == "fail-start" {
		return nil, fmt.Errorf("configured command failed")
	}
	return r.httpRuntime.Start(op, lifetime, id, prepared, command, env)
}
func (r *settingsRuntime) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.starts }

func TestStagedApplyStreamingRollbackAndPolicy(t *testing.T) {
	origin := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	first := commitVersion(t, origin, "one")
	base := map[string]string{}
	for k, v := range config.Defaults {
		base[k] = v
	}
	base["GIT_URL"], base["RUN_COMMAND"], base["APP_MODE"] = origin, "serve", "docker"
	base["DATA_DIR"], base["CONFIG_DIR"], base["LOG_DIR"] = t.TempDir(), t.TempDir(), t.TempDir()
	base["POLL_INTERVAL"], base["HEALTH_INTERVAL"], base["HEALTH_PATH"], base["STARTUP_TIMEOUT"], base["DRAIN_TIMEOUT"] = "3600", "0.01", "/health", "0.3", "1"
	store := state.Store{Data: base["DATA_DIR"], ConfigDir: base["CONFIG_DIR"]}
	settings, _, err := config.Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(settings.Values(false), true)
	if err != nil {
		t.Fatal(err)
	}
	git := revision.New(origin, "", cfg.DataDir)
	rt := &settingsRuntime{httpRuntime: &httpRuntime{Runtime: sandbox.Runtime{Data: cfg.DataDir}}}
	capture, err := logs.New(cfg.LogDir, logs.Policy{Days: 30, FileSize: 10 << 20, TotalSize: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	e := &Engine{Cfg: cfg, Runtime: rt, Checkout: git, Proxy: proxy.New(), Store: store, Health: health.New(cfg.HealthInterval, cfg.HealthPath), Rootfs: t.TempDir(), Digest: "sha256:first", Drain: time.Second, Logs: capture, Failures: make(chan uint64, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	e.RuntimeContext = ctx
	c := &Controller{Cfg: cfg, Settings: settings, Engine: e, Selector: git}
	c.Configure = func(_ context.Context, next config.Config) (Deployment, Selector, error) {
		git := revision.New(next.GitURL, next.GitToken, next.DataDir)
		plan := e.snapshot()
		plan.Cfg, plan.Checkout, plan.Health = next, git, health.New(next.HealthInterval, next.HealthPath)
		if next.RuntimeImage != e.Cfg.RuntimeImage {
			plan.Digest = "sha256:changed"
		}
		return plan, git, nil
	}
	closeControl, err := c.StartControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; closeControl(); e.Shutdown(context.Background()) })
	client := controlClient(t, cfg.DataDir)
	request := func(method, path string, patch any) (int, []byte) {
		t.Helper()
		var body io.Reader
		if patch != nil {
			b, err := json.Marshal(patch)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, "http://local"+path, body)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, b
	}
	stage := func(values map[string]string) {
		t.Helper()
		if code, body := request("POST", "/config", map[string]any{"values": values}); code != 200 {
			t.Fatalf("stage: %d %s", code, body)
		}
	}
	active := func() Instance {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.active == nil {
			return nil
		}
		return e.active.Instance
	}
	waitFor(t, func() bool { return e.Status().State == "running" && e.Selected() == first })
	old := active()
	starts := rt.count()
	stage(map[string]string{"APP_MODE": "docker"})
	if code, body := request("POST", "/apply", nil); code != 200 || active() != old || rt.count() != starts {
		t.Fatalf("same-value override was not saved without redeploy: %d %s", code, body)
	}
	if settings.Applied()["APP_MODE"] != "docker" {
		t.Fatal("explicit override matching Docker default was discarded")
	}
	stage(map[string]string{"APP_MODE": "pending", "APP_EMPTY": ""})
	if code, body := request("GET", "/config?name=APP_MODE&name=APP_EMPTY", nil); code != 200 || !bytes.Contains(body, []byte("pending")) || bytes.Contains(body, []byte("GIT_URL")) {
		t.Fatalf("filtered show: %d %s", code, body)
	}
	if code, _ := request("GET", "/config?name=APP_MISSING", nil); code != 400 {
		t.Fatal("unknown show name accepted")
	}
	if active() != old || rt.count() != starts || settings.Values(false)["APP_MODE"] != "docker" {
		t.Fatal("set redeployed or applied changes")
	}
	reopened, _, err := config.Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Values(true)["APP_MODE"] != "pending" || reopened.Values(false)["APP_MODE"] != "docker" {
		t.Fatal("restart activated or lost staged settings")
	}
	// New code exists, but an app-only apply must preserve the exact running SHA.
	second := commitVersion(t, origin, "two")
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
	applied := make(chan error, 1)
	go func() {
		res, err := client.Post("http://local/apply", "", nil)
		if err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				err = fmt.Errorf("%s", b)
			}
		}
		applied <- err
	}()
	waitFor(t, func() bool { return active() != old })
	if old.Exited() || e.Selected() != first {
		t.Fatal("app-only apply lost the old stream or advanced Git")
	}
	stream.Body.Close()
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if settings.Values(false)["APP_MODE"] != "pending" {
		t.Fatal("healthy apply was not accepted")
	}
	rt.mu.Lock()
	gotEnv := strings.Join(rt.env, "\n")
	rt.mu.Unlock()
	if !strings.Contains(gotEnv, "MODE=pending") || !strings.Contains(gotEnv, "EMPTY=") {
		t.Fatal("candidate did not receive staged app variables")
	}
	old = active()
	stage(map[string]string{"RUN_COMMAND": "fail-start", "APP_MODE": "bad"})
	if code, _ := request("POST", "/apply", nil); code != 500 {
		t.Fatal("failed candidate accepted")
	}
	prior, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if active() != old || old.Exited() || prior.Overrides["APP_MODE"] != "pending" || settings.Values(false)["APP_MODE"] != "pending" {
		t.Fatal("failed apply changed active settings/revision/routing")
	}
	stage(map[string]string{"RUN_COMMAND": "serve", "APP_MODE": "pending", "POLL_INTERVAL": "0"})
	if code, _ := request("POST", "/apply", nil); code != 500 || active() != old {
		t.Fatal("invalid batch affected the application")
	}
	stage(map[string]string{"POLL_INTERVAL": "3600", "SERVICE_MEMORY_LIMIT": "0", "HEALTH_FAILURES": "4", "LOG_MAX_FILE_SIZE": "1K", "LOG_MAX_TOTAL_SIZE": "2K"})
	starts = rt.count()
	if code, body := request("POST", "/apply", nil); code != 200 {
		t.Fatalf("policy apply: %d %s", code, body)
	}
	if active() != old || rt.count() != starts || e.Status().MemoryLimit != 0 {
		t.Fatal("policy-only changes replaced the application")
	}
	if code, body := request("POST", "/apply", nil); code != 200 || rt.count() != starts {
		t.Fatalf("no-op apply replaced application: %d %s", code, body)
	}
	// Simulate a durable-write failure without changing the accepted state file.
	stage(map[string]string{"APP_MODE": "not-durable"})
	blocked := filepath.Join(cfg.ConfigDir, "blocked")
	if err := os.WriteFile(blocked, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	e.Store.ConfigDir = blocked
	if code, _ := request("POST", "/apply", nil); code != 500 {
		t.Fatal("failed persistence accepted")
	}
	e.Store = store
	if active() != old || old.Exited() || settings.Values(false)["APP_MODE"] != "pending" {
		t.Fatal("persistence failure switched traffic/settings")
	}
	stage(map[string]string{"APP_MODE": "pending", "RUNTIME_IMAGE": "node:22-bookworm-slim"})
	if code, body := request("POST", "/apply", nil); code != 200 || e.Status().Digest != "sha256:changed" || e.Selected() != first {
		t.Fatalf("image apply: %d %s", code, body)
	}
	// Explicit redeploy still checks Git immediately under the committed config.
	if code, body := request("POST", "/redeploy", nil); code != 200 || e.Selected() != second {
		t.Fatalf("redeploy: %d %s", code, body)
	}
	origin2 := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", origin2).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	third := commitVersion(t, origin2, "three")
	stage(map[string]string{"GIT_URL": origin2})
	if code, body := request("POST", "/apply", nil); code != 200 || e.Selected() != third {
		t.Fatalf("source apply: %d %s", code, body)
	}
	base["APP_MODE"] = "changed-docker-default"
	reopened, prior, err = config.Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Values(false)["APP_MODE"] != "pending" || prior.Revision != third || prior.GitURL != origin2 {
		t.Fatal("accepted configuration did not survive restart with matching source identity")
	}
	crashed := active()
	_ = crashed.Stop(context.Background())
	waitFor(t, func() bool { return active() != crashed && e.Status().State == "running" })
}

func TestUnconfiguredSupervisorAcceptsCLIConfiguration(t *testing.T) {
	base := map[string]string{}
	for k, v := range config.Defaults {
		base[k] = v
	}
	base["DATA_DIR"], base["CONFIG_DIR"], base["LOG_DIR"] = t.TempDir(), t.TempDir(), t.TempDir()
	base["HEALTH_PATH"], base["HEALTH_INTERVAL"] = "/health", "0.01"
	store := state.Store{Data: base["DATA_DIR"], ConfigDir: base["CONFIG_DIR"]}
	settings, _, err := config.Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(settings.Values(false), false)
	if err != nil {
		t.Fatal(err)
	}
	rt := &httpRuntime{Runtime: sandbox.Runtime{Data: cfg.DataDir}}
	e := &Engine{Cfg: cfg, Runtime: rt, Proxy: proxy.New(), Store: store, Failures: make(chan uint64, 1)}
	c := &Controller{Cfg: cfg, Settings: settings, Engine: e}
	c.Configure = func(_ context.Context, cfg config.Config) (Deployment, Selector, error) {
		git := revision.New(cfg.GitURL, cfg.GitToken, cfg.DataDir)
		return Deployment{Cfg: cfg, Runtime: rt, Checkout: git, Health: health.New(cfg.HealthInterval, cfg.HealthPath), Rootfs: t.TempDir(), Digest: "sha256:initial"}, git, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.RuntimeContext = ctx
	closeControl, err := c.StartControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done; closeControl(); e.Shutdown(context.Background()) }()
	origin := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	sha := commitVersion(t, origin, "one")
	if err := settings.Stage(map[string]string{"GIT_URL": origin, "RUN_COMMAND": "serve"}, nil); err != nil {
		t.Fatal(err)
	}
	if e.Status().State != "waiting" {
		t.Fatal("unconfigured supervisor deployed pending settings")
	}
	client := controlClient(t, cfg.DataDir)
	res, err := client.Post("http://local/redeploy", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 500 {
		t.Fatal("unconfigured redeploy did not report missing configuration")
	}
	res, err = client.Post("http://local/apply", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || e.Selected() != sha {
		t.Fatalf("configuration-only launch failed: %d %s", res.StatusCode, b)
	}
}
