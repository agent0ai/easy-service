package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

func controlClient(t *testing.T, data string) *http.Client {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", ControlPath(data))
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func TestControlActionsWithLocalGit(t *testing.T) {
	origin := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	first := commitVersion(t, origin, "one")
	cfg := config.Config{DataDir: t.TempDir(), GitURL: origin, GitToken: "supervisor-secret", RuntimeImage: "node:22-bookworm-slim", UpdateMethod: "commit", GitBranch: "main", UpdatePattern: "*", PollInterval: time.Hour, StartupTimeout: 100 * time.Millisecond, HealthInterval: 10 * time.Millisecond, HealthFailures: 2}
	git := revision.New(origin, "", cfg.DataDir)
	rt := &httpRuntime{Runtime: sandbox.Runtime{Data: cfg.DataDir}}
	e := &Engine{Cfg: cfg, Runtime: rt, Checkout: git, Proxy: proxy.New(), Store: state.Store{Data: cfg.DataDir}, Health: health.New(cfg.HealthInterval, "/health"), Rootfs: t.TempDir(), Digest: "sha256:test", Drain: 500 * time.Millisecond, Failures: make(chan uint64, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	e.RuntimeContext = ctx
	c := &Controller{Cfg: cfg, Selector: git, Engine: e}
	closeControl, err := c.StartControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; closeControl(); e.Shutdown(context.Background()) })
	client := controlClient(t, cfg.DataDir)
	active := func() Instance {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.active == nil {
			return nil
		}
		return e.active.Instance
	}
	waitFor(t, func() bool { return e.Status().State == "running" && e.Selected() == first })
	request := func(method, action string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://local/"+action, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, string(body)
	}
	code, body := request("GET", "status")
	var status Status
	if err := json.Unmarshal([]byte(body), &status); err != nil || code != 200 || status.Revision != first || status.State != "running" || strings.Contains(body, cfg.GitToken) {
		t.Fatalf("status: %d %s %v", code, body, err)
	}
	if code, _ := request("GET", "restart"); code != 405 {
		t.Fatal("GET triggered an action")
	}
	if code, _ := request("POST", "unknown"); code != 404 {
		t.Fatal("unknown action accepted")
	}
	old := active()
	marker := filepath.Join(old.BundlePath(), "rootfs", "app", "writable-marker")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, body := request("POST", "restart"); code != 200 {
			t.Fatalf("restart: %d %s", code, body)
		}
		if active().BundlePath() != old.BundlePath() {
			t.Fatal("explicit restart replaced the writable installation")
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("restart discarded application writes:", err)
		}
	}
	// Returning a command response cancels its HTTP context; the newly published
	// health watcher must remain owned by the long-lived controller context.
	crashed := active()
	_ = crashed.Stop(context.Background())
	waitFor(t, func() bool { return active() != crashed && e.Status().State == "running" })
	old = active()
	front := httptest.NewServer(e.Proxy)
	defer front.Close()
	front.Client().Timeout = 2 * time.Second
	stream, err := front.Client().Get(front.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if _, err := io.ReadFull(stream.Body, make([]byte, len("data: one\n\n"))); err != nil {
		t.Fatal(err)
	}
	redeployed := make(chan error, 1)
	go func() {
		res, err := client.Post("http://local/redeploy", "", nil)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				err = io.ErrUnexpectedEOF
			}
		}
		redeployed <- err
	}()
	waitFor(t, func() bool { return active() != old })
	if old.Exited() {
		t.Fatal("same-revision redeploy skipped stream drain")
	}
	if code, _ := request("POST", "restart"); code != 409 {
		t.Fatal("simultaneous action was not rejected")
	}
	if code, body := request("GET", "status"); code != 200 || !strings.Contains(body, first) {
		t.Fatal("status blocked behind stream drain")
	}
	if code, body := request("POST", "kill-draining"); code != 200 || !strings.Contains(body, `"killed":1`) {
		t.Fatalf("urgent kill blocked behind redeploy: %d %s", code, body)
	}
	if !old.Exited() || active().Exited() {
		t.Fatal("urgent kill stopped the current instance or left the retired one alive")
	}
	if _, err := io.ReadFull(stream.Body, make([]byte, 1)); err == nil {
		t.Fatal("retired stream survived urgent kill")
	}
	if code, body := request("POST", "kill-draining"); code != 200 || !strings.Contains(body, `"killed":0`) {
		t.Fatalf("empty draining set: %d %s", code, body)
	}
	stream.Body.Close()
	if err := <-redeployed; err != nil {
		t.Fatal(err)
	}
	if active().BundlePath() == old.BundlePath() || e.Selected() != first {
		t.Fatal("unchanged revision did not get a fresh writable installation")
	}
	crashed = active()
	_ = crashed.Stop(context.Background())
	waitFor(t, func() bool { return active() != crashed && e.Status().State == "running" })
	// A failed CLI candidate must leave routing on the healthy old revision.
	commitVersion(t, origin, "bad")
	old = active()
	if code, _ := request("POST", "redeploy"); code != 500 {
		t.Fatal("broken candidate accepted")
	}
	if active() != old || old.Exited() || e.Status().State != "running" {
		t.Fatal("failed manual deployment interrupted serving")
	}
	second := commitVersion(t, origin, "two")
	if code, body := request("POST", "redeploy"); code != 200 || !strings.Contains(body, second) {
		t.Fatalf("immediate Git selection failed: %d %s", code, body)
	}
	res, err := front.Client().Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(response) != "two" {
		t.Fatalf("manual cutover did not reach workload: %q", response)
	}
	// Explicit restart failure enters existing automatic recovery, even after
	// its original HTTP request has ended.
	if err := os.WriteFile(filepath.Join(active().BundlePath(), "rootfs", "app", "version"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _ := request("POST", "restart"); code != 500 {
		t.Fatal("unhealthy restart accepted")
	}
	waitFor(t, func() bool { return e.Status().State == "running" })
}

func TestControlSocketOwnershipAndCancellation(t *testing.T) {
	e, rt := engineFor(t, fakeHealth{})
	e.Cfg.PollInterval = time.Hour
	sha := "0123456789012345678901234567890123456789"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Controller{Cfg: e.Cfg, Selector: staticSelector{revision.Selection{SHA: sha}}, Engine: e}
	path := ControlPath(e.Cfg.DataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	closeControl, err := c.StartControl(ctx)
	if err != nil {
		t.Fatal("stale socket not recovered:", err)
	}
	defer closeControl()
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Dir(path), 0700}, {path, 0600}} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("control permissions: %v %v", info, err)
		}
	}
	other := &Controller{Cfg: c.Cfg, Engine: e}
	if closeOther, err := other.StartControl(ctx); err == nil {
		closeOther()
		t.Fatal("replaced a live control socket")
	}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return e.Selected() == sha })
	aborted, abort := context.WithCancel(ctx)
	abort()
	result := make(chan error, 1)
	c.commands <- command{aborted, "restart", result}
	if err := <-result; err == nil {
		t.Fatal("cancelled action succeeded")
	}
	rt.mu.Lock()
	restarts := rt.restarts
	rt.mu.Unlock()
	if restarts != 0 || e.Status().State != "running" {
		t.Fatal("cancelled action stopped the active workload")
	}
	cancel()
	<-done
	waitFor(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
}
