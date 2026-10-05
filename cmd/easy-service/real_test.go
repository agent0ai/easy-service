package main

import (
	"context"
	"flag"
	"io"
	"net/http"
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
	ref := "busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092"
	image, err := (imagepkg.Manager{Data: data, Runner: process.Runner{}}).Prepare(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUPERVISOR_ONLY_TEST", "must-stay-outside")
	cfg := config.Config{DataDir: data, GitURL: origin, RuntimeImage: ref, SetupCommand: `/bin/busybox nslookup example.com >/dev/null && mkdir -p "$HOME" && printf prepared > "$HOME/setup-marker" && /bin/busybox dd if=/dev/zero of=/tmp/setup-data bs=1M count=70`, RunCommand: `test "$EXPECTED" = workload-visible && test -z "${SUPERVISOR_ONLY_TEST+x}" && test ! -e .git && test -f "$HOME/setup-marker" && test "$(wc -c </tmp/setup-data)" -eq 73400320 && test "$(cat index.html)" != bad && exec /bin/busybox httpd -f -p "$PORT" -h /app`, AppEnv: []string{"EXPECTED=workload-visible"}, StartupTimeout: 30 * time.Second, HealthInterval: time.Second, HealthFailures: 3}
	e := &supervisor.Engine{Cfg: cfg, Runtime: runtimeAdapter{sandbox.Runtime{Data: data, ImageEnv: image.Env, Stdout: os.Stdout, Stderr: os.Stderr}}, Checkout: git, Proxy: proxy.New(), Store: state.Store{Data: data}, Health: health.New(cfg.HealthInterval, "/index.html"), Rootfs: image.Rootfs, Digest: image.Digest, Drain: time.Second, RuntimeContext: ctx, Failures: make(chan uint64, 1)}
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
	status := e.Status()
	if status.MemoryError != "" || status.MemoryBytes == 0 {
		t.Fatalf("direct runsc memory sample: bytes=%d error=%s", status.MemoryBytes, status.MemoryError)
	}
	if err := e.Deploy(ctx, commit("bad")); err == nil {
		t.Fatal("broken real sandbox candidate was accepted")
	}
	assertServed("one")
	if err := e.Deploy(ctx, commit("two")); err != nil {
		t.Fatal(err)
	}
	assertServed("two")
	if err := e.Store.Reconcile(); err != nil {
		t.Fatalf("direct runsc orphan reconciliation: %v", err)
	}
	res, err := front.Client().Get(front.URL + "/index.html")
	if err == nil {
		_ = res.Body.Close()
		if res.StatusCode == 200 {
			t.Fatal("reconciliation left the direct runsc workload serving")
		}
	}
}

func TestRealGoImageEnvironmentAndSetup(t *testing.T) {
	if !*realGVisor {
		t.Skip("enable with -args -real-gvisor on a supported host")
	}
	if os.Geteuid() == 0 {
		t.Fatal("run the real rootless lifecycle test as an unprivileged user")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	images := imagepkg.Manager{Data: t.TempDir(), Runner: process.Runner{}}
	image, err := images.Prepare(ctx, "golang:1.27.1-bookworm")
	if err != nil {
		t.Fatal(err)
	}
	if err := images.Prune(image); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	source := `package main
import ("fmt"; "net/http"; "os")
func main() {
 if _, err := os.Stat(os.Getenv("HOME")+"/install-marker"); err != nil { panic(err) }
 if os.Getenv("SUPERVISOR_ONLY_TEST") != "" { panic("supervisor environment leaked") }
 http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, os.Getenv("MESSAGE")) })
 panic(http.ListenAndServe(":"+os.Getenv("PORT"), nil))
}
`
	for name, contents := range map[string]string{"go.mod": "module example.test/service\n\ngo 1.23\n", "main.go": source} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SUPERVISOR_ONLY_TEST", "must-stay-outside")
	rt := sandbox.Runtime{Data: images.Data, ImageEnv: image.Env, Stdout: os.Stdout, Stderr: os.Stderr}
	env := []string{"HOME=/app/home", "MESSAGE=go-ready"}
	prepared, err := rt.Prepare(ctx, "go-image", image.Rootfs, checkout, `mkdir -p bin && go build -o bin/server . && printf installed > "$HOME/install-marker"`, env)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := rt.Start(ctx, ctx, "go-server", prepared, "exec ./bin/server", env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := instance.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	if err := health.New(time.Second, "/").Ready(ctx, instance.Endpoint(), instance, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(instance.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "go-ready" {
		t.Fatalf("real Go service response: %q %v", body, err)
	}
}
