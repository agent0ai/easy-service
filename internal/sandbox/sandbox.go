package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

	"github.com/example/easy-service/internal/logs"
	"github.com/example/easy-service/internal/process"
	"github.com/example/easy-service/internal/state"
)

type Instance struct {
	ID, Bundle, Root string
	Port             int
	cmd              *exec.Cmd
	done             chan error
	waitErr          error
	once             sync.Once
	owner            state.RuntimeOwner
	sampleMu         sync.Mutex
	sampledAt        time.Time
	startedAt        time.Time
	sampledTicks     uint64
	usage            process.Usage
}
type Runtime struct {
	Data           string
	ImageEnv       []string
	Logs           *logs.Manager
	SetupTimeout   time.Duration
	Stdout, Stderr io.Writer
}

func Validate() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("the supervisor must run as root; workloads use separate unprivileged users")
	}
	if _, err := exec.LookPath("proot"); err != nil {
		return fmt.Errorf("mandatory runtime prerequisite proot not found")
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
func (r Runtime) Prepare(ctx context.Context, id, base, checkout, setup string, env []string) (result string, err error) {
	dir := filepath.Join(r.Data, "prepared", id)
	if r.inUse(dir) {
		return "", fmt.Errorf("prepared filesystem is still in use")
	}
	defer func() {
		if err != nil && !r.inUse(dir) {
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
		timeout := r.SetupTimeout
		if timeout <= 0 {
			timeout = 15 * time.Minute
		}
		op, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		i, e := r.start(op, "setup-"+id, dir, setup, env)
		if e != nil {
			return "", e
		}
		e = i.Wait(op)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopErr := i.Stop(stopCtx)
		stopCancel()
		if stopErr != nil {
			return "", errors.Join(e, fmt.Errorf("stop setup: %w", stopErr))
		}
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
	if r.inUse(dir) {
		return fmt.Errorf("prepared filesystem is still in use")
	}
	for _, path := range []string{filepath.Join(dir, ".easy-service-ready"), filepath.Join(dir, "rootfs"), filepath.Join(dir, "rootfs", "app")} {
		if st, err := os.Stat(path); err != nil || (path != filepath.Join(dir, ".easy-service-ready") && !st.IsDir()) {
			return fmt.Errorf("prepared installation invalid at %s", filepath.Base(path))
		}
	}
	return nil
}

func (r Runtime) inUse(bundle string) bool {
	for _, instance := range r.List() {
		if instance.BundlePath() == bundle {
			return true
		}
	}
	return false
}
func (r Runtime) Start(ctx, lifetime context.Context, id, prepared, command string, env []string) (*Instance, error) {
	bundle := filepath.Join(r.Data, "instances", id)
	root := filepath.Join(r.Data, "runtime", id)
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
	i, err := r.start(lifetime, id, bundle, command, env)
	ok = err == nil
	return i, err
}
func (r Runtime) Restart(ctx context.Context, id, bundle, command string, env []string) (*Instance, error) {
	root := filepath.Join(r.Data, "runtime", id)
	i, err := r.start(ctx, id, bundle, command, env)
	if err != nil {
		_ = os.RemoveAll(root)
	}
	return i, err
}
func (i *Instance) Identity() string { return i.ID }

func (i *Instance) PID() int {
	if i.cmd == nil || i.cmd.Process == nil {
		return 0
	}
	return i.cmd.Process.Pid
}
func (i *Instance) Usage() (process.Usage, error) {
	i.sampleMu.Lock()
	defer i.sampleMu.Unlock()
	now := time.Now()
	if !i.sampledAt.IsZero() && now.Sub(i.sampledAt) < 200*time.Millisecond {
		return i.usage, nil
	}
	sample, err := process.SampleUser(i.owner.UID)
	if err != nil {
		return process.Usage{}, err
	}
	if len(sample.PIDs) == 0 {
		return process.Usage{}, fmt.Errorf("instance processes are not running")
	}
	usage := process.Usage{MemoryBytes: sample.MemoryBytes}
	previous := i.sampledAt
	if previous.IsZero() {
		previous = i.startedAt
	}
	if !previous.IsZero() && sample.CPUTicks >= i.sampledTicks {
		// Linux amd64 and arm64 expose /proc CPU time in USER_HZ (100 ticks/sec).
		usage.CPUPercent = float64(sample.CPUTicks-i.sampledTicks) / now.Sub(previous).Seconds()
	}
	i.sampledAt, i.sampledTicks, i.usage = now, sample.CPUTicks, usage
	return usage, nil
}

var live sync.Map

func (r Runtime) List() []*Instance {
	data, _ := filepath.Abs(r.Data)
	var instances []*Instance
	live.Range(func(_, value any) bool {
		i := value.(*Instance)
		if filepath.Dir(i.Root) == filepath.Join(data, "runtime") && !i.Exited() {
			instances = append(instances, i)
		}
		return true
	})
	return instances
}
func (i *Instance) Endpoint() string   { return fmt.Sprintf("http://127.0.0.1:%d", i.Port) }
func (i *Instance) BundlePath() string { return i.Bundle }
func (i *Instance) RootPath() string   { return i.Root }
func (i *Instance) Done() <-chan error { return i.done }
func (r Runtime) start(ctx context.Context, id, bundle, command string, env []string) (*Instance, error) {
	if err := Validate(); err != nil {
		return nil, err
	}
	data, err := filepath.Abs(r.Data)
	if err != nil {
		return nil, err
	}
	for parent := filepath.Dir(data); ; parent = filepath.Dir(parent) {
		st, err := os.Stat(parent)
		if err != nil || st.Mode().Perm()&0001 == 0 {
			return nil, fmt.Errorf("DATA_DIR parent must permit directory traversal: %s", parent)
		}
		if parent == "/" {
			break
		}
	}
	for _, path := range []string{data, filepath.Join(data, "instances"), filepath.Join(data, "prepared"), filepath.Join(data, "runtime")} {
		if err := os.MkdirAll(path, 0711); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0711); err != nil {
			return nil, err
		}
	}
	bundle, err = filepath.Abs(bundle)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(bundle, 0711); err != nil {
		return nil, err
	}
	if err := os.Chmod(bundle, 0711); err != nil {
		return nil, err
	}
	owner, err := state.NewRuntimeOwner(data, id)
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			_ = owner.Release()
			_ = os.RemoveAll(owner.Root)
		}
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	// ponytail: release before the app binds; use socket activation if atomic handoff is needed.
	if err := listener.Close(); err != nil {
		return nil, err
	}
	rootfs := filepath.Join(bundle, "rootfs")
	tmp := filepath.Join(rootfs, "tmp")
	if st, err := os.Lstat(tmp); err == nil && st.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(tmp); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, 0777|os.ModeSticky); err != nil {
		return nil, err
	}
	traceTmp := filepath.Join(owner.Root, "tmp")
	if err := os.Mkdir(traceTmp, 0700); err != nil {
		return nil, err
	}
	uid := strconv.FormatUint(uint64(owner.UID), 10)
	if b, err := process.Output(process.Command(ctx, "chown", "-hR", uid+":"+uid, "--", rootfs, traceTmp)); err != nil {
		return nil, fmt.Errorf("own instance filesystem: %w: %s", err, b)
	}
	if err := os.Chmod(rootfs, 0700); err != nil {
		return nil, err
	}
	resolver, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	dns := filepath.Join(owner.Root, "resolv.conf")
	if err := os.WriteFile(dns, resolver, 0444); err != nil {
		return nil, err
	}
	proot, err := exec.LookPath("proot")
	if err != nil {
		return nil, err
	}
	proot, err = filepath.Abs(proot)
	if err != nil {
		return nil, err
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	launch := launchConfig{UID: owner.UID, Rootfs: rootfs, DNS: dns, Temp: traceTmp, Proot: proot, Binary: binary, Command: command, Env: workloadEnv(r.ImageEnv, env, port)}
	body, err := json.Marshal(launch)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(owner.Root, "launch.json")
	if err := state.AtomicWrite(path, body); err != nil {
		return nil, err
	}
	descriptor, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer descriptor.Close()
	cmd := process.Command(ctx, binary, "__runtime", "launch")
	cmd.Cancel = func() error {
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		uidErr := process.SignalUser(stop, owner.UID, syscall.SIGKILL)
		return errors.Join(uidErr, cmd.Process.Kill())
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.ExtraFiles = []*os.File{descriptor}
	cmd.SysProcAttr.Setpgid = false
	cmd.SysProcAttr.Setsid = true
	phase, logID := "run", id
	if strings.HasPrefix(id, "setup-") {
		phase, logID = "setup", strings.TrimPrefix(id, "setup-")
	}
	if r.Logs != nil {
		cmd.Stdout, cmd.Stderr, logID = r.Logs.Output(id, phase, r.Stdout, r.Stderr)
	} else {
		cmd.Stdout = &prefixWriter{r.Stdout, "[" + logID + "][" + phase + "][stdout] "}
		cmd.Stderr = &prefixWriter{r.Stderr, "[" + logID + "][" + phase + "][stderr] "}
	}
	if err := cmd.Start(); err != nil {
		if r.Logs != nil {
			r.Logs.Release(logID)
		}
		return nil, err
	}
	started = true
	i := &Instance{ID: logID, Bundle: bundle, Root: owner.Root, Port: port, cmd: cmd, done: make(chan error), owner: owner, startedAt: time.Now()}
	live.Store(owner.Root, i)
	go func() {
		i.waitErr = cmd.Wait()
		for {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := process.KillUser(cleanup, owner.UID)
			cancel()
			if err == nil {
				break
			}
			log.Printf("instance process cleanup blocked instance=%s error=%v", logID, err)
			time.Sleep(time.Second)
		}
		if err := owner.Release(); err != nil {
			log.Printf("instance UID release failed instance=%s error=%v", logID, err)
		}
		close(i.done)
		live.Delete(owner.Root)
		if r.Logs != nil {
			r.Logs.Release(logID)
		}
		log.Printf("instance exited instance=%s phase=%s error=%v", logID, phase, i.waitErr)
	}()
	return i, nil
}
func workloadEnv(image, app []string, servicePort int) []string {
	values := map[string]string{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/root"}
	for _, source := range [][]string{image, app} {
		for _, item := range source {
			if at := strings.IndexByte(item, '='); at > 0 {
				values[item[:at]] = item[at+1:]
			}
		}
	}
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
func (i *Instance) signal(ctx context.Context, sig syscall.Signal) error {
	if i.Exited() {
		return nil
	}
	// os.Process uses a pidfd on supported Linux kernels, including Hostinger's.
	// This also covers the short root-owned launcher phase before UID switching.
	var parentErr error
	uidErr := process.SignalUser(ctx, i.owner.UID, sig)
	if i.cmd != nil && i.cmd.Process != nil {
		parentErr = i.cmd.Process.Signal(sig)
	}
	if errors.Is(parentErr, os.ErrProcessDone) {
		parentErr = nil
	}
	return errors.Join(parentErr, uidErr)
}
func (i *Instance) Stop(ctx context.Context) error {
	if i.Exited() {
		return nil
	}
	var err error
	i.once.Do(func() { err = i.signal(ctx, syscall.SIGTERM) })
	if err != nil && ctx.Err() == nil {
		return err
	}
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		forced, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return i.Kill(forced)
	}
}
func (i *Instance) Kill(ctx context.Context) error {
	if i.Exited() {
		return nil
	}
	if err := i.signal(ctx, syscall.SIGKILL); err != nil {
		return err
	}
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("instance did not finish after kill: %w", ctx.Err())
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
