package main

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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	imagepkg "github.com/example/easy-service/internal/image"
	"github.com/example/easy-service/internal/logs"
	"github.com/example/easy-service/internal/process"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/sandbox"
	"github.com/example/easy-service/internal/state"
	"github.com/example/easy-service/internal/supervisor"
)

var realRuntime = flag.Bool("real-runtime", false, "test the real PRoot/Linux UID/OCI/Git/HTTP lifecycle as an unprivileged user")

func TestRealConfigurationCLIAndRestart(t *testing.T) {
	if !*realRuntime {
		t.Skip("enable on a supported host with -real-runtime")
	}
	binary := os.Getenv("EASY_SERVICE_REAL_BINARY")
	if binary == "" {
		t.Skip("set EASY_SERVICE_REAL_BINARY to the built supervisor (able to bind port 80)")
	}
	if os.Geteuid() != 0 {
		t.Fatal("run as root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := realDirectory(t)
	data, cfgDir, logDir := filepath.Join(root, "data"), filepath.Join(root, "config"), filepath.Join(root, "logs")
	env := []string{"PATH=" + os.Getenv("PATH"), "DATA_DIR=" + data, "CONFIG_DIR=" + cfgDir, "LOG_DIR=" + logDir, "SUPERVISOR_ONLY_TEST=must-stay-outside"}
	consolePath := filepath.Join(root, "console")
	console, err := os.OpenFile(consolePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer console.Close()
	var child *exec.Cmd
	var exited chan error
	start := func() {
		t.Helper()
		child = exec.Command(binary)
		child.Env = env
		child.Stdout, child.Stderr = console, console
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		exited = make(chan error, 1)
		go func(cmd *exec.Cmd, done chan error) { done <- cmd.Wait() }(child, exited)
	}
	stop := func() {
		t.Helper()
		if child == nil {
			return
		}
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			if err != nil {
				b, _ := os.ReadFile(consolePath)
				t.Fatalf("supervisor shutdown: %v\n%s", err, b)
			}
		case <-time.After(15 * time.Second):
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			t.Fatal("supervisor shutdown stalled")
		}
		child = nil
	}
	defer func() {
		if child != nil {
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			<-exited
		}
	}()
	cli := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		if err != nil {
			consoleOutput, _ := os.ReadFile(consolePath)
			t.Fatalf("CLI %v: %v %s\n%s", args, err, b, consoleOutput)
		}
		return string(b)
	}
	wait := func(ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for !ready() {
			if ctx.Err() != nil || time.Now().After(deadline) {
				b, _ := os.ReadFile(consolePath)
				t.Fatalf("real supervisor timed out\n%s", b)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	client := &http.Client{Timeout: time.Second}
	served := func(want string) bool {
		res, err := client.Get("http://127.0.0.1:80/")
		if err != nil {
			return false
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		return err == nil && res.StatusCode == 200 && string(b) == want
	}
	start()
	wait(func() bool { _, err := os.Stat(supervisor.ControlPath(data)); return err == nil })
	if status := cli("status"); !strings.Contains(status, "State: waiting") || !strings.Contains(status, "App memory: not running") {
		t.Fatalf("empty container status: %s", status)
	}
	origin := t.TempDir()
	git := revision.New(origin, "", data)
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture"}} {
		if _, err := git.Run(ctx, origin, args...); err != nil {
			t.Fatal(err)
		}
	}
	sha, err := git.Run(ctx, origin, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	run := `test -z "${SUPERVISOR_ONLY_TEST+x}" && echo "stdout $MESSAGE" && echo "stderr $MESSAGE" >&2 && exec /bin/busybox httpd -f -p "$PORT" -h /app`
	setup := `printf '%s' "$MESSAGE" > index.html && mkdir -p cgi-bin && printf '%s\n' '#!/bin/sh' 'printf "Content-Type: text/event-stream\r\n\r\n"' 'while :; do printf "data: tick\n\n"; sleep 1; done' > cgi-bin/stream && chmod +x cgi-bin/stream`
	cli("config", "set", "GIT_URL="+origin, "RUN_COMMAND="+run, "SETUP_COMMAND="+setup, "RUNTIME_IMAGE=busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092", "APP_MESSAGE=one", "POLL_INTERVAL=3600", "HEALTH_INTERVAL=0.2", "DRAIN_TIMEOUT=20")
	if served("one") {
		t.Fatal("set launched an application")
	}
	cli("config", "apply")
	wait(func() bool { return served("one") })
	exported := cli("config", "show")
	imported := exec.CommandContext(ctx, binary, "config", "set")
	imported.Env, imported.Stdin = env, strings.NewReader(exported)
	if b, err := imported.CombinedOutput(); err != nil || string(b) != exported {
		t.Fatalf("real CLI copy/paste did not preserve settings: %v %s", err, b)
	}
	if !served("one") {
		t.Fatal("import deployed pending settings")
	}
	oldFiles, err := os.ReadDir(logDir)
	if err != nil || len(oldFiles) == 0 {
		t.Fatalf("no app output captured: %v", err)
	}
	joined := ""
	for _, f := range oldFiles {
		b, _ := os.ReadFile(filepath.Join(logDir, f.Name()))
		joined += string(b)
	}
	if !strings.Contains(joined, "stdout one") || !strings.Contains(joined, "stderr one") || !strings.Contains(joined, "commit: \""+sha+"\"") || !strings.HasPrefix(joined, "---\n") {
		t.Fatalf("missing output/frontmatter: %s", joined)
	}
	if strings.Contains(joined, "must-stay-outside") {
		t.Fatal("supervisor environment exposed in retained logs")
	}
	cli("config", "set", "APP_MESSAGE=two")
	if !served("one") {
		t.Fatal("pending setting interrupted service")
	}
	stop()
	start()
	wait(func() bool { return served("one") })
	if cli("config", "show", "APP_MESSAGE") != "APP_MESSAGE=two\n" {
		t.Fatal("restart lost pending settings")
	}
	for _, f := range oldFiles {
		if _, err := os.Stat(filepath.Join(logDir, f.Name())); err != nil {
			t.Fatal("retired instance logs disappeared:", err)
		}
	}
	streamRequest, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:80/cgi-bin/stream", nil)
	stream, err := (&http.Client{}).Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if _, err := io.ReadFull(stream.Body, make([]byte, len("data: tick\n\n"))); err != nil {
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() {
		cmd := exec.CommandContext(ctx, binary, "config", "apply")
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v %s", err, b)
		}
		applied <- err
	}()
	wait(func() bool { return served("two") })
	if killed := cli("kill-draining"); killed != "Killed 1 draining instances\n" {
		t.Fatalf("urgent real kill: %q", killed)
	}
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("urgent kill did not unblock real drain")
	}
	stream.Body.Close()
	if killed := cli("kill-draining"); killed != "Killed 0 draining instances\n" || !served("two") {
		t.Fatal("empty force-stop touched the current app")
	}
	cli("config", "set", "RUN_COMMAND=false")
	bad := exec.CommandContext(ctx, binary, "config", "apply")
	bad.Env = env
	if b, err := bad.CombinedOutput(); err == nil {
		t.Fatalf("bad command accepted: %s", b)
	}
	if !served("two") {
		t.Fatal("failed real candidate interrupted serving")
	}
	cli("config", "set", "RUN_COMMAND="+run)
	cli("config", "apply")
	before := cli("status")
	cli("config", "set", "LOG_RETENTION_DAYS=1", "LOG_MAX_FILE_SIZE=1K", "LOG_MAX_TOTAL_SIZE=1K")
	cli("config", "apply")
	after := cli("status")
	instance := func(status string) string {
		for _, line := range strings.Split(status, "\n") {
			if strings.HasPrefix(line, "Instance: ") {
				return line
			}
		}
		return ""
	}
	if instance(before) == "" || instance(before) != instance(after) || !served("two") {
		t.Fatal("log policy replaced the real app instance")
	}
	files, _ := os.ReadDir(logDir)
	var total int64
	for _, f := range files {
		info, err := f.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 1024 {
			t.Fatal("live file limit exceeded")
		}
		total += info.Size()
	}
	if total > 1024 {
		t.Fatalf("live total log cap exceeded: %d", total)
	}
	stop()
	b, _ := os.ReadFile(consolePath)
	for _, event := range []string{"configuration staged", "configuration applied", "candidate healthy", "draining previous", "instance exited", "log collector removed"} {
		if !strings.Contains(string(b), event) {
			t.Fatal(fmt.Sprintf("missing console lifecycle event %q", event))
		}
	}
}

func TestRealRuntimeLifecycle(t *testing.T) {
	if !*realRuntime {
		t.Skip("enable with -args -real-runtime on a supported host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("run the real runtime lifecycle test as root")
	}
	if err := sandbox.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	data, origin := realDirectory(t), t.TempDir()
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
	capture, err := logs.New(t.TempDir(), logs.Policy{Days: 30, FileSize: 10 << 20, TotalSize: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	t.Setenv("SUPERVISOR_ONLY_TEST", "must-stay-outside")
	cfg := config.Config{DataDir: data, GitURL: origin, RuntimeImage: ref, SetupCommand: `/bin/busybox nslookup example.com >/dev/null && mkdir -p "$HOME" && printf prepared > "$HOME/setup-marker" && /bin/busybox dd if=/dev/zero of=/tmp/setup-data bs=1M count=70`, RunCommand: `echo app-stdout; echo app-stderr >&2; test "$EXPECTED" = workload-visible && test -z "${SUPERVISOR_ONLY_TEST+x}" && test ! -e .git && test -f "$HOME/setup-marker" && test "$(wc -c </tmp/setup-data)" -eq 73400320 && test "$(cat index.html)" != bad && exec /bin/busybox httpd -f -p "$PORT" -h /app`, AppEnv: []string{"EXPECTED=workload-visible"}, StartupTimeout: 30 * time.Second, HealthInterval: time.Second, HealthFailures: 3}
	e := &supervisor.Engine{Cfg: cfg, Runtime: runtimeAdapter{sandbox.Runtime{Data: data, ImageEnv: image.Env, Logs: capture, Stdout: os.Stdout, Stderr: os.Stderr}}, Checkout: git, Proxy: proxy.New(), Store: state.Store{Data: data}, Health: health.New(cfg.HealthInterval, "/index.html"), Rootfs: image.Rootfs, Digest: image.Digest, Logs: capture, Drain: time.Second, RuntimeContext: ctx, Failures: make(chan uint64, 1)}
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
	if status.ResourceError != "" || status.MemoryBytes == 0 {
		t.Fatalf("native runtime memory sample: bytes=%d error=%s", status.MemoryBytes, status.ResourceError)
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
		t.Fatalf("native runtime orphan reconciliation: %v", err)
	}
	res, err := front.Client().Get(front.URL + "/index.html")
	if err == nil {
		_ = res.Body.Close()
		if res.StatusCode == 200 {
			t.Fatal("reconciliation left the native runtime workload serving")
		}
	}
}

func TestRealGoImageEnvironmentAndSetup(t *testing.T) {
	if !*realRuntime {
		t.Skip("enable with -args -real-runtime on a supported host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("run the real runtime lifecycle test as root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	images := imagepkg.Manager{Data: realDirectory(t), Runner: process.Runner{}}
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

func TestMain(m *testing.M) {
	if handled, err := sandbox.Child(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}
func realDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for p := dir; strings.HasPrefix(p, "/tmp/"); p = filepath.Dir(p) {
		if err := os.Chmod(p, 0711); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
