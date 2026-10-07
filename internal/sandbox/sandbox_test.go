package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/logs"
	"github.com/example/easy-service/internal/process"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var heldMemory []byte

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__fake-runtime" {
		fakeRuntime()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "__daemon" {
		signal.Ignore(syscall.SIGTERM)
		heldMemory = make([]byte, 64<<20)
		for i := range heldMemory {
			heldMemory[i] = 1
		}
		for {
			for i := range heldMemory {
				heldMemory[i]++
			}
		}
	}
	if handled, err := Child(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}
func fakeRuntime() {
	var config launchConfig
	if err := json.NewDecoder(os.NewFile(3, "configuration")).Decode(&config); err != nil {
		panic(err)
	}
	if os.Geteuid() != int(config.UID) {
		panic("real UID was not changed")
	}
	status, _ := os.ReadFile("/proc/self/status")
	if !strings.Contains(string(status), "NoNewPrivs:\t1") {
		panic("privilege escalation was not disabled")
	}
	if os.Getenv("SUPERVISOR_ONLY_TEST") != "" {
		panic("supervisor environment leaked")
	}
	if config.Command == "fail" {
		fmt.Fprintln(os.Stdout, "setup-output")
		fmt.Fprintln(os.Stderr, "runtime-start-failure")
		os.Exit(23)
	}
	if config.Command == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	values := map[string]string{}
	for _, line := range config.Env {
		key, value, _ := strings.Cut(line, "=")
		values[key] = value
	}
	handler := http.NewServeMux()
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, values["MESSAGE"]) })
	handler.HandleFunc("/spawn", func(w http.ResponseWriter, r *http.Request) {
		cmd := exec.Command(os.Args[0], "__daemon")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		go cmd.Wait()
		fmt.Fprint(w, cmd.Process.Pid)
	})
	if err := http.ListenAndServe("127.0.0.1:"+values["PORT"], handler); err != nil {
		panic(err)
	}
}
func fakeNative(t *testing.T) Runtime {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("Linux UID isolation requires root")
	}
	root := t.TempDir()
	for p := root; strings.HasPrefix(p, "/tmp/"); p = filepath.Dir(p) {
		if err := os.Chmod(p, 0711); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(bin, "helper")
	bytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copy, bytes, 0755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(copy, "'", "'\"'\"'") + "' __fake-runtime\n"
	if err := os.WriteFile(filepath.Join(bin, "proot"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SUPERVISOR_ONLY_TEST", "must-stay-outside")
	return Runtime{Data: filepath.Join(root, "data")}
}
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

func TestWorkloadEnvironmentPrecedenceAndIsolation(t *testing.T) {
	t.Setenv("SUPERVISOR_ONLY_TEST", "must-stay-outside")
	image := []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "HOME=/image-home", "IMAGE_DEFAULT=present", "PORT=old"}
	got := workloadEnv(image, []string{"SECRET=app", "PORT=wrong", "PATH=/app/bin:/bin", "HOME=/app/home", "GIT_TOKEN=explicit-app-value"}, 8080)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"SECRET=app", "PORT=8080", "PATH=/app/bin:/bin", "HOME=/app/home", "IMAGE_DEFAULT=present", "GIT_TOKEN=explicit-app-value"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
	if strings.Contains(joined, "PORT=wrong") || strings.Contains(joined, "PORT=old") || strings.Contains(joined, "SUPERVISOR_ONLY_TEST") {
		t.Fatal("reserved port or supervisor environment leaked:", got)
	}
	defaults := strings.Join(workloadEnv(image, nil, 8080), "\n")
	if !strings.Contains(defaults, "PATH=/usr/local/go/bin:/usr/bin:/bin") || !strings.Contains(defaults, "HOME=/image-home") {
		t.Fatal("image defaults discarded:", defaults)
	}
	if !strings.Contains(strings.Join(workloadEnv(nil, nil, 8080), "\n"), "HOME=/root") {
		t.Fatal("default HOME is not on the private filesystem")
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

func TestMissingRuntimePrerequisitesFailClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := Validate(); err == nil {
		t.Fatal("missing runtime accepted")
	}
}

func TestLiveSetupFilesystemCannotBeReusedOrRemoved(t *testing.T) {
	r := fakeNative(t)
	bundle := filepath.Join(r.Data, "prepared", "busy")
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs", "app"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(bundle, ".easy-service-ready")
	if err := os.WriteFile(marker, []byte("ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	i, err := r.start(context.Background(), "setup-busy", bundle, "ignore-term", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := i.Kill(ctx); err != nil {
			t.Error(err)
		}
	}()
	if err := r.ValidatePrepared(bundle); err == nil {
		t.Fatal("accepted a filesystem still owned by live setup")
	}
	if _, err := r.Prepare(context.Background(), "busy", "", "", "", nil); err == nil {
		t.Fatal("reused a live setup filesystem")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("removed a live setup filesystem:", err)
	}
}
func TestNativeRuntimeOwnsAllProcessesAndFiles(t *testing.T) {
	r := fakeNative(t)
	var instances []*Instance
	for _, id := range []string{"old", "candidate"} {
		bundle := filepath.Join(r.Data, "instances", id)
		if err := os.MkdirAll(filepath.Join(bundle, "rootfs", "app"), 0700); err != nil {
			t.Fatal(err)
		}
		i, err := r.start(context.Background(), id, bundle, "ignore-term", []string{"MESSAGE=" + id, "PORT=wrong"})
		if err != nil {
			t.Fatal(err)
		}
		instances = append(instances, i)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = i.Kill(ctx)
		})
		if err := health.New(time.Millisecond, "/").Ready(context.Background(), i.Endpoint(), i, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(filepath.Join(bundle, "rootfs"))
		if err != nil || st.Sys().(*syscall.Stat_t).Uid != i.owner.UID || st.Mode().Perm() != 0700 {
			t.Fatalf("private filesystem owner: %v %v", st, err)
		}
		dns, err := os.ReadFile(filepath.Join(i.Root, "resolv.conf"))
		expected, _ := os.ReadFile("/etc/resolv.conf")
		if err != nil || string(dns) != string(expected) {
			t.Fatal("Docker DNS was not copied")
		}
	}
	old, candidate := instances[0], instances[1]
	if old.Port == candidate.Port || old.owner.UID == candidate.owner.UID || len(r.List()) != 2 {
		t.Fatal("overlapping instances share identity")
	}
	response, err := http.Get(old.Endpoint() + "/spawn")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		usage, err := old.Usage()
		if err == nil && usage.MemoryBytes >= 64<<20 && usage.CPUPercent > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CPU/RAM not observed: %+v %v", usage, err)
		}
		time.Sleep(220 * time.Millisecond)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat)[strings.LastIndexByte(string(stat), ')')+1:])
	if fields[2] != strconv.Itoa(pid) {
		t.Fatal("worker did not leave the parent's group")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := old.Stop(ctx); err != nil || !old.Exited() {
		t.Fatalf("whole-instance forced stop: %v", err)
	}
	sample, err := process.SampleUser(old.owner.UID)
	if err != nil || len(sample.PIDs) != 0 {
		t.Fatal("detached descendants survived", sample, err)
	}
	if err := old.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	response, err = http.Get(candidate.Endpoint())
	if err != nil {
		t.Fatal("peer instance stopped", err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "candidate" {
		t.Fatal("wrong peer response")
	}
}
func TestRuntimeFailurePreservesOutputAndWaitResult(t *testing.T) {
	r := fakeNative(t)
	var stdout, stderr bytes.Buffer
	r.Stdout, r.Stderr = &stdout, &stderr
	directory := t.TempDir()
	manager, err := logs.New(directory, logs.Policy{Days: 30, FileSize: 10 << 20, TotalSize: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	r.Logs = manager
	manager.Register("setup-abcdef123456", logs.Info{ID: "abcdef123456", Commit: "fixture", DeployedAt: time.Now()})
	bundle := filepath.Join(r.Data, "prepared", "fixture")
	i, err := r.start(context.Background(), "setup-abcdef123456", bundle, "fail", nil)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		err := i.Wait(context.Background())
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
			t.Fatalf("wait consumed error: %v", err)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("logs missing: %v %v", entries, err)
	}
	retained, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"setup-output", "runtime-start-failure"} {
		if !strings.Contains(stdout.String()+stderr.String(), message) || !strings.Contains(string(retained), message) {
			t.Fatal("console/retained output lost", message)
		}
	}
}
func TestExitedInstanceDoesNotSignalStalePID(t *testing.T) {
	cmd := process.Command(context.Background(), "sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	i := &Instance{cmd: cmd, done: make(chan error)}
	close(i.done)
	if err := i.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("completed instance signalled another process")
	}
}
