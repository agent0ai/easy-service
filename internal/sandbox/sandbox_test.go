package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/process"
)

func TestPrivateWritableCopies(t *testing.T) {
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, "x"), []byte("base"), 0600)
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	if e := CopyTree(context.Background(), base, a); e != nil {
		t.Fatal(e)
	}
	if e := CopyTree(context.Background(), base, b); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(a, "x"), []byte("changed"), 0600)
	got, _ := os.ReadFile(filepath.Join(b, "x"))
	if string(got) != "base" {
		t.Fatal("instances share writable data")
	}
}

func TestStrictWorkloadEnvironment(t *testing.T) {
	got := workloadEnv([]string{"SECRET=app", "PORT=wrong", "PATH=wrong", "GIT_TOKEN=explicit-app-value"}, 8080)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "SECRET=app") || !strings.Contains(joined, "PORT=8080") || strings.Contains(joined, "PORT=wrong") || strings.Contains(joined, "PATH=wrong") {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(joined, "GIT_TOKEN=explicit-app-value") {
		t.Fatal("an explicitly provided APP_GIT_TOKEN must forward as GIT_TOKEN")
	}
}

func TestMissingIsolationPrerequisitesFailClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if e := Validate(); e == nil || !strings.Contains(e.Error(), "mandatory rootless gVisor prerequisite") {
		t.Fatalf("unexpected validation result: %v", e)
	}
}

func TestPreparedValidationRequiresCompleteInstallation(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, ".easy-service-ready"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (Runtime{}).ValidatePrepared(d); err == nil {
		t.Fatal("marker-only prepared installation was accepted")
	}
	if err := os.MkdirAll(filepath.Join(d, "rootfs", "app"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := (Runtime{}).ValidatePrepared(d); err != nil {
		t.Fatalf("complete prepared installation rejected: %v", err)
	}
}

func TestProcessTreeMemorySampling(t *testing.T) {
	used, err := processTreeRSS(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if used == 0 {
		t.Fatal("running process tree reported no resident memory")
	}
}

func TestImageAppSymlinkCannotEscapePreparation(t *testing.T) {
	base, checkout, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "app")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "source"), []byte("committed"), 0600); err != nil {
		t.Fatal(err)
	}
	r := Runtime{Data: t.TempDir()}
	prepared, err := r.Prepare(context.Background(), "test", base, checkout, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "source")); !os.IsNotExist(err) {
		t.Fatal("preparation followed the image /app symlink into the host")
	}
	if err := r.ValidatePrepared(prepared); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkProcessTreeMemorySampling(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := processTreeRSS(os.Getpid()); err != nil {
			b.Fatal(err)
		}
	}
}

func TestExitObservationPreservesWaitError(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "runsc"), []byte("#!/bin/sh\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r := Runtime{Data: t.TempDir()}
	i, err := r.start(context.Background(), "failed", t.TempDir(), "ignored", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-i.Done()
	if !i.Exited() {
		t.Fatal("finished process was not reported exited")
	}
	for n := 0; n < 3; n++ {
		if err := i.Wait(context.Background()); err == nil {
			t.Fatal("exit observation consumed the setup failure")
		}
	}
}

func TestForcedStopReapsBeforeReturning(t *testing.T) {
	bin, ready := t.TempDir(), filepath.Join(t.TempDir(), "ready")
	helper := "#!/bin/sh\ntrap '' TERM\n: > \"$EASY_SERVICE_STOP_READY\"\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(bin, "runsc"), []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("EASY_SERVICE_STOP_READY", ready)
	i, err := (Runtime{Data: t.TempDir()}).start(context.Background(), "ignore-term", t.TempDir(), "ignored", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = i.Stop(ctx)
	})
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_ = i.Stop(ctx)
	if !i.Exited() {
		t.Fatal("forced stop returned before process-group completion")
	}
}

func TestExitedInstanceDoesNotSignalStalePID(t *testing.T) {
	cmd := process.Command(context.Background(), "sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = process.Wait(cmd); close(done) }()
	t.Cleanup(func() { _ = process.KillGroup(cmd); <-done })
	// Model a completed instance whose PID was reused by an unrelated group.
	i := &Instance{cmd: cmd, done: make(chan error)}
	close(i.done)
	if err := i.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("an exited instance signalled the reused process group")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestDirectRuntimeUsesDistinctPortsAndDockerDNS(t *testing.T) {
	bin := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Only runsc execution is replaced; allocation, OCI spec, DNS and HTTP are real.
	helper := "#!/bin/sh\nexec \"$EASY_SERVICE_RUNSC_BINARY\" -test.run=^TestDirectRunscHelper$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "runsc"), []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("EASY_SERVICE_RUNSC_BINARY", executable)
	t.Setenv("EASY_SERVICE_RUNSC_HELPER", "1")
	r := Runtime{Data: t.TempDir()}
	resolver, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	ports := make(map[int]bool)
	for _, id := range []string{"old", "candidate"} {
		bundle := filepath.Join(r.Data, "instances", id)
		i, err := r.start(context.Background(), id, bundle, id, []string{"PORT=wrong"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = i.Stop(ctx)
		})
		if err := health.New(10*time.Millisecond, "/").Ready(context.Background(), i.Endpoint(), i, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if ports[i.Port] || i.Port < 1024 {
			t.Fatalf("overlapping deployments received invalid/shared port %d", i.Port)
		}
		ports[i.Port] = true
		res, err := http.Get(i.Endpoint())
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || string(body) != id {
			t.Fatalf("deployment endpoint reached wrong workload: %q %v", body, err)
		}
		b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var spec Spec
		if err := json.Unmarshal(b, &spec); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, mount := range spec.Mounts {
			if mount.Destination == "/etc/resolv.conf" {
				contents, err := os.ReadFile(mount.Source)
				if err != nil || string(contents) != string(resolver) || !filepath.IsAbs(mount.Source) || !strings.Contains(strings.Join(mount.Options, ","), "ro") {
					t.Fatalf("owned read-only Docker DNS mount: %+v %v", mount, err)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("runtime did not provide Docker DNS")
		}
	}
}

func TestDirectRunscHelper(t *testing.T) {
	if os.Getenv("EASY_SERVICE_RUNSC_HELPER") != "1" {
		return
	}
	args := strings.Join(os.Args, " ")
	for _, required := range []string{"--rootless=true", "--platform=systrap", "--directfs=false", "--overlay2=none", "--network=host"} {
		if !strings.Contains(args, required) {
			t.Fatal("missing direct gVisor argument:", required)
		}
	}
	var bundle string
	for n, arg := range os.Args {
		if arg == "--bundle" && n+1 < len(os.Args) {
			bundle = os.Args[n+1]
		}
	}
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.Process.NoNewPrivileges || spec.Process.Cwd != "/app" {
		t.Fatal("workload restrictions changed")
	}
	var port int
	for _, item := range spec.Process.Env {
		if strings.HasPrefix(item, "PORT=") {
			port, err = strconv.Atoi(strings.TrimPrefix(item, "PORT="))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, spec.Process.Args[2])
	})); err != nil {
		t.Fatal(err)
	}
}

func TestProcessTreeMemoryIncludesChildFromAnotherThread(t *testing.T) {
	// Keeping the launching thread occupied forces the fork onto another OS
	// thread, whose /proc children file must also be walked.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		baseline, err := processTreeRSS(os.Getpid())
		if err != nil {
			result <- err
			return
		}
		executable, err := os.Executable()
		if err != nil {
			result <- err
			return
		}
		cmd := exec.Command(executable, "-test.run=^TestMemoryAllocationHelper$")
		cmd.Env = append(os.Environ(), "EASY_SERVICE_MEMORY_HELPER=1")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			result <- err
			return
		}
		if err := cmd.Start(); err != nil {
			result <- err
			return
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
			result <- err
			return
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			var tree, child uint64
			tree, err = processTreeRSS(os.Getpid())
			if err != nil {
				break
			}
			child, err = processTreeRSS(cmd.Process.Pid)
			if err == nil && child >= 32<<20 && tree > baseline+child/2 {
				result <- nil
				return
			}
			time.Sleep(time.Millisecond)
		}
		result <- fmt.Errorf("child memory not sampled: %v", err)
	}()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestMemoryAllocationHelper(t *testing.T) {
	if os.Getenv("EASY_SERVICE_MEMORY_HELPER") != "1" {
		return
	}
	b := make([]byte, 64<<20)
	for i := 0; i < len(b); i += os.Getpagesize() {
		b[i] = 1
	}
	fmt.Println("ready")
	time.Sleep(10 * time.Second)
	runtime.KeepAlive(b)
}
