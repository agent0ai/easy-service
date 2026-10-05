package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/example/easy-service/internal/process"
)

type Spec struct {
	OCIVersion string  `json:"ociVersion"`
	Process    Process `json:"process"`
	Root       Root    `json:"root"`
	Mounts     []Mount `json:"mounts"`
	Linux      Linux   `json:"linux"`
}
type Process struct {
	Terminal        bool     `json:"terminal"`
	User            User     `json:"user"`
	Args            []string `json:"args"`
	Env             []string `json:"env"`
	Cwd             string   `json:"cwd"`
	NoNewPrivileges bool     `json:"noNewPrivileges"`
}
type User struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}
type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}
type Mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}
type Linux struct {
	Namespaces []Namespace `json:"namespaces"`
}
type Namespace struct {
	Type string `json:"type"`
}
type Instance struct {
	ID, Bundle, Root string
	Port             int
	cmd              *exec.Cmd
	done             chan error
	waitErr          error
	once             sync.Once
}
type Runtime struct {
	Data           string
	Stdout, Stderr io.Writer
}

func Validate() error {
	for _, n := range []string{"runsc", "rootlesskit", "slirp4netns"} {
		if _, e := exec.LookPath(n); e != nil {
			return fmt.Errorf("mandatory rootless gVisor prerequisite %s not found", n)
		}
	}
	if _, e := os.Stat("/proc/self/ns/user"); e != nil {
		return fmt.Errorf("user namespaces unavailable: %w", e)
	}
	if st, e := os.Stat("/dev/net/tun"); e != nil || st.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("rootless sandbox networking requires /dev/net/tun")
	}
	if _, e := os.Stat("/sys/fs/cgroup/cgroup.controllers"); e != nil {
		return fmt.Errorf("cgroup v2 is required: %w", e)
	}
	b, e := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if e == nil && strings.TrimSpace(string(b)) == "0" {
		return fmt.Errorf("unprivileged user namespaces are disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := process.Command(ctx, "rootlesskit", "--net=slirp4netns", "--disable-host-loopback", "--port-driver=builtin", "--", "true")
	if b, e := process.Output(cmd); e != nil {
		return fmt.Errorf("nested rootless networking preflight failed: %w: %s", e, strings.TrimSpace(string(b)))
	}
	return nil
}
func CopyTree(ctx context.Context, src, dst string) error {
	if e := os.MkdirAll(dst, 0700); e != nil {
		return e
	}
	c := process.Command(ctx, "cp", "-a", "--reflink=auto", filepath.Clean(src)+"/.", dst)
	if b, e := process.Output(c); e != nil {
		return fmt.Errorf("copy filesystem: %w: %s", e, b)
	}
	return nil
}
func (r Runtime) Prepare(ctx context.Context, id, base, checkout, setup string, servicePort int, env []string) (result string, err error) {
	dir := filepath.Join(r.Data, "prepared", id)
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	if e := os.RemoveAll(dir); e != nil {
		return "", e
	}
	if e := CopyTree(ctx, base, filepath.Join(dir, "rootfs")); e != nil {
		return "", e
	}
	app := filepath.Join(dir, "rootfs", "app")
	// An image's /app symlink is relative to the container filesystem, not the
	// supervisor. Replace it before any host copy can follow it out of rootfs.
	if e := os.RemoveAll(app); e != nil {
		return "", e
	}
	if e := CopyTree(ctx, checkout, app); e != nil {
		return "", e
	}
	if setup != "" {
		op, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		i, e := r.start(op, "setup-"+id, dir, setup, servicePort, env)
		if e != nil {
			return "", e
		}
		e = i.Wait(op)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = i.Stop(stopCtx)
		stopCancel()
		_ = os.RemoveAll(i.RootPath())
		if e != nil {
			return "", fmt.Errorf("setup failed: %w", e)
		}
	}
	if e := os.WriteFile(filepath.Join(dir, ".easy-service-ready"), []byte("ok\n"), 0600); e != nil {
		return "", e
	}
	return dir, nil
}
func (r Runtime) ValidatePrepared(dir string) error {
	for _, path := range []string{filepath.Join(dir, ".easy-service-ready"), filepath.Join(dir, "rootfs"), filepath.Join(dir, "rootfs", "app")} {
		if st, err := os.Stat(path); err != nil || (path != filepath.Join(dir, ".easy-service-ready") && !st.IsDir()) {
			return fmt.Errorf("prepared installation invalid at %s", filepath.Base(path))
		}
	}
	return nil
}
func (r Runtime) Start(ctx, lifetime context.Context, id, prepared, command string, servicePort int, env []string) (*Instance, error) {
	bundle := filepath.Join(r.Data, "instances", id)
	root := filepath.Join(r.Data, "runsc-root", id)
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(bundle)
			_ = os.RemoveAll(root)
		}
	}()
	if e := os.RemoveAll(bundle); e != nil {
		return nil, e
	}
	if e := CopyTree(ctx, filepath.Join(prepared, "rootfs"), filepath.Join(bundle, "rootfs")); e != nil {
		return nil, e
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	i, err := r.start(lifetime, id, bundle, command, servicePort, env)
	ok = err == nil
	return i, err
}
func (r Runtime) Restart(ctx context.Context, id, bundle, command string, servicePort int, env []string) (*Instance, error) {
	root := filepath.Join(r.Data, "runsc-root", id)
	i, err := r.start(ctx, id, bundle, command, servicePort, env)
	if err != nil {
		_ = os.RemoveAll(root)
	}
	return i, err
}
func (i *Instance) PID() int {
	if i.cmd == nil || i.cmd.Process == nil {
		return 0
	}
	return i.cmd.Process.Pid
}
func (i *Instance) MemoryUsage() (uint64, error) { return processTreeRSS(i.PID()) }

func processTreeRSS(root int) (uint64, error) {
	if root <= 1 {
		return 0, fmt.Errorf("sandbox process is not running")
	}
	type child struct{ pid, parent int }
	pending := []child{{root, 0}}
	seen := make(map[int]bool)
	var total uint64
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[next.pid] {
			continue
		}
		seen[next.pid] = true
		dir := filepath.Join("/proc", strconv.Itoa(next.pid))
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			if next.pid == root {
				return 0, err
			}
			continue
		}
		closeParen := strings.LastIndexByte(string(stat), ')')
		if closeParen < 0 {
			continue
		}
		fields := strings.Fields(string(stat[closeParen+1:]))
		if len(fields) < 22 {
			continue
		}
		parent, _ := strconv.Atoi(fields[1])
		if next.parent != 0 && parent != next.parent {
			continue // a short-lived child PID was reused outside our tree
		}
		pages, err := strconv.ParseUint(fields[21], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid sandbox RSS: %w", err)
		}
		total += pages * uint64(os.Getpagesize())
		// A process can fork from any thread. The leader's children file alone
		// would miss subprocesses started by other runsc/rootlesskit threads.
		tasks, err := os.ReadDir(filepath.Join(dir, "task"))
		if err != nil {
			continue
		}
		for _, task := range tasks {
			children, err := os.ReadFile(filepath.Join(dir, "task", task.Name(), "children"))
			if err != nil {
				continue
			}
			for _, raw := range strings.Fields(string(children)) {
				pid, err := strconv.Atoi(raw)
				if err == nil && pid > 1 {
					pending = append(pending, child{pid, next.pid})
				}
			}
		}
	}
	return total, nil
}
func (i *Instance) Endpoint() string   { return fmt.Sprintf("http://127.0.0.1:%d", i.Port) }
func (i *Instance) BundlePath() string { return i.Bundle }
func (i *Instance) RootPath() string   { return i.Root }
func (i *Instance) Done() <-chan error { return i.done }
func (r Runtime) start(ctx context.Context, id, bundle, command string, servicePort int, env []string) (*Instance, error) {
	if e := os.MkdirAll(bundle, 0700); e != nil {
		return nil, e
	}
	port := 0
	if servicePort > 0 {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			return nil, e
		}
		port = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	spec := Spec{OCIVersion: "1.0.2", Process: Process{User: User{0, 0}, Args: []string{"/bin/sh", "-c", command}, Env: workloadEnv(env, servicePort), Cwd: "/app", NoNewPrivileges: true}, Root: Root{"rootfs", false}, Mounts: []Mount{{"/proc", "proc", "proc", nil}, {"/dev", "tmpfs", "tmpfs", []string{"nosuid", "strictatime", "mode=755", "size=65536k"}}, {"/tmp", "tmpfs", "tmpfs", []string{"nosuid", "nodev", "mode=1777", "size=64m"}}}, Linux: Linux{[]Namespace{{"pid"}, {"ipc"}, {"uts"}, {"mount"}}}}
	// OCI images need not contain resolv.conf. Slirp's default private DNS
	// endpoint belongs to this network, independent of the supervisor resolver.
	dns, e := filepath.Abs(filepath.Join(bundle, "resolv.conf"))
	if e != nil {
		return nil, e
	}
	if e := os.WriteFile(dns, []byte("nameserver 10.0.2.3\n"), 0600); e != nil {
		return nil, e
	}
	spec.Mounts = append(spec.Mounts, Mount{"/etc/resolv.conf", "bind", dns, []string{"bind", "ro", "nosuid", "nodev", "noexec"}})
	b, _ := json.Marshal(spec)
	if e := os.WriteFile(filepath.Join(bundle, "config.json"), b, 0600); e != nil {
		return nil, e
	}
	args := []string{"--net=slirp4netns", "--disable-host-loopback", "--port-driver=builtin"}
	if port > 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d/tcp", port, servicePort))
	}
	root := filepath.Join(r.Data, "runsc-root", id)
	args = append(args, "--", "runsc", "--rootless=true", "--network=host", "--file-access=exclusive", "--root", root, "run", "--bundle", bundle, id)
	cmd := process.Command(ctx, "rootlesskit", args...)
	phase, logID := "run", id
	if strings.HasPrefix(id, "setup-") {
		phase, logID = "setup", strings.TrimPrefix(id, "setup-")
	}
	cmd.Stdout = &prefixWriter{r.Stdout, "[" + logID + "][" + phase + "][stdout] "}
	cmd.Stderr = &prefixWriter{r.Stderr, "[" + logID + "][" + phase + "][stderr] "}
	if e := cmd.Start(); e != nil {
		return nil, e
	}
	i := &Instance{ID: id, Bundle: bundle, Root: root, Port: port, cmd: cmd, done: make(chan error)}
	go func() { i.waitErr = process.Wait(cmd); close(i.done) }()
	return i, nil
}
func workloadEnv(app []string, servicePort int) []string {
	values := map[string]string{}
	for _, item := range app {
		if at := strings.IndexByte(item, '='); at > 0 {
			values[item[:at]] = item[at+1:]
		}
	}
	values["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	values["HOME"] = "/tmp"
	values["PORT"] = strconv.Itoa(servicePort)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}
func (i *Instance) Wait(ctx context.Context) error {
	select {
	case <-i.done:
		return i.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (i *Instance) Exited() bool {
	select {
	case <-i.done:
		return true
	default:
		return false
	}
}
func (i *Instance) Stop(ctx context.Context) error {
	if i.Exited() {
		return nil // a stale PID may now belong to a different process group
	}
	i.once.Do(func() {
		if i.cmd.Process != nil {
			_ = syscall.Kill(-i.cmd.Process.Pid, syscall.SIGTERM)
		}
	})
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		if i.cmd.Process != nil {
			_ = syscall.Kill(-i.cmd.Process.Pid, syscall.SIGKILL)
		}
		// Killing is asynchronous. Reap before callers reuse or remove the
		// installation, allowing for Command's one-second output-pipe deadline.
		t := time.NewTimer(2 * time.Second)
		defer t.Stop()
		select {
		case <-i.done:
			return nil
		case <-t.C:
			return fmt.Errorf("sandbox did not finish after forced stop: %w", ctx.Err())
		}
	}
}

type prefixWriter struct {
	w io.Writer
	p string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if p.w == nil {
		return len(b), nil
	}
	for _, part := range strings.SplitAfter(string(b), "\n") {
		if part != "" {
			if _, e := fmt.Fprintf(p.w, "%s%s", p.p, part); e != nil {
				return 0, e
			}
		}
	}
	return len(b), nil
}
