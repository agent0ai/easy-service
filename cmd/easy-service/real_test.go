package main

import (
	"context"
	"flag"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	imagepkg "github.com/example/easy-service/internal/image"
	"github.com/example/easy-service/internal/process"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/sandbox"
	"github.com/example/easy-service/internal/state"
	"github.com/example/easy-service/internal/supervisor"
)

var realGVisor = flag.Bool("real-gvisor", false, "test the real rootless gVisor/OCI/Git/HTTP lifecycle as an unprivileged user")

func TestRealGVisorLifecycle(t *testing.T) {
	if !*realGVisor {
		t.Skip("enable with -args -real-gvisor on a supported host")
	}
	if os.Geteuid() == 0 {
		t.Fatal("run the real rootless lifecycle test as an unprivileged user")
	}
	if err := sandbox.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	data, origin := t.TempDir(), t.TempDir()
	git := revision.New(origin, "", data)
	runGit := func(args ...string) string {
		t.Helper()
		out, err := git.Run(ctx, origin, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	runGit("init", "-b", "main")
	commit := func(version string) revision.Selection {
		t.Helper()
		if err := os.WriteFile(filepath.Join(origin, "index.html"), []byte(version), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "index.html")
		runGit("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", version)
		sel, err := git.Select(ctx, "commit", "main", "*")
		if err != nil {
			t.Fatal(err)
		}
		return sel
	}
	first := commit("one")
	ref := "docker.io/library/alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	image, err := (imagepkg.Manager{Data: data, Runner: process.Runner{}}).Prepare(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPERVISOR_ONLY_TEST", "must-stay-outside")
	cfg := config.Config{DataDir: data, GitURL: origin, RuntimeImage: ref, ServicePort: 80, SetupCommand: "/bin/busybox nslookup example.com >/dev/null && printf prepared > setup-marker", RunCommand: `test "$EXPECTED" = workload-visible && test -z "${SUPERVISOR_ONLY_TEST+x}" && test ! -e .git && test -f setup-marker && test "$(cat index.html)" != bad && exec /bin/busybox httpd -f -p "$PORT" -h /app`, AppEnv: []string{"EXPECTED=workload-visible"}, StartupTimeout: 30 * time.Second, HealthInterval: time.Second, HealthFailures: 3}
	e := &supervisor.Engine{Cfg: cfg, Runtime: runtimeAdapter{sandbox.Runtime{Data: data, Stdout: os.Stdout, Stderr: os.Stderr}}, Checkout: git, Proxy: proxy.New(), Store: state.Store{Data: data}, Health: health.New(cfg.HealthInterval, "/index.html"), Rootfs: image.Rootfs, Digest: image.Digest, Drain: time.Second, RuntimeContext: ctx, Failures: make(chan uint64, 1)}
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	front := httptest.NewServer(e.Proxy)
	defer front.Close()
	front.Client().Timeout = 3 * time.Second
	assertServed := func(want string) {
		t.Helper()
		res, err := front.Client().Get(front.URL + "/index.html")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != 200 || string(body) != want {
			t.Fatalf("real HTTP response: status=%d body=%q err=%v", res.StatusCode, body, err)
		}
	}
	if err := e.Deploy(ctx, first); err != nil {
		t.Fatal(err)
	}
	assertServed("one")
	if err := e.Deploy(ctx, commit("bad")); err == nil {
		t.Fatal("broken real sandbox candidate was accepted")
	}
	assertServed("one")
	if err := e.Deploy(ctx, commit("two")); err != nil {
		t.Fatal(err)
	}
	assertServed("two")
}
