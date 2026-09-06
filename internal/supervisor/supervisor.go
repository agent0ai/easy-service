package supervisor

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/state"
)

type Instance interface {
	Exited() bool
	Done() <-chan error
	Stop(context.Context) error
	Endpoint() string
	BundlePath() string
	RootPath() string
	PID() int
	MemoryUsage() (uint64, error)
}
type Runtime interface {
	Prepare(context.Context, string, string, string, string, int, []string) (string, error)
	ValidatePrepared(string) error
	Start(context.Context, string, string, string, int, []string) (Instance, error)
	Restart(context.Context, string, string, string, int, []string) (Instance, error)
}
type Checkout interface {
	Checkout(context.Context, string, string) error
}
type Health interface {
	Ready(context.Context, string, health.Exited, time.Duration) error
	UntilFailure(context.Context, string, health.Exited, int) health.Result
}
type Active struct {
	Revision, Digest, Prepared string
	Instance                   Instance
	Backend                    *proxy.Backend
	Restarted                  bool
	generation                 uint64
}
type Engine struct {
	Cfg            config.Config
	Runtime        Runtime
	Checkout       Checkout
	Proxy          *proxy.Proxy
	Store          state.Store
	Health         Health
	Rootfs, Digest string
	Drain          time.Duration
	// RuntimeContext owns sandbox process lifetime independently from polling,
	// health-watch, and signal contexts. Production cancels it only after the
	// proxy and active backend have drained during Engine.Shutdown.
	RuntimeContext context.Context
	mu             sync.Mutex
	active         *Active
	seq            uint64
	Failures       chan uint64
	MemoryExceeded chan uint64
	MemoryInterval time.Duration
}

func (e *Engine) nextID(sha string) string {
	e.seq++
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("%s-%d-%d", short, time.Now().UnixNano(), e.seq)
}
func (e *Engine) Deploy(ctx context.Context, sel revision.Selection) error {
	return e.deploy(ctx, ctx, sel, nil)
}

func (e *Engine) runtimeContext(fallback context.Context) context.Context {
	if e.RuntimeContext != nil {
		return e.RuntimeContext
	}
	return fallback
}

func (e *Engine) Replace(ctx context.Context, generation uint64) error {
	e.mu.Lock()
	a := e.active
	e.mu.Unlock()
	if a == nil || a.generation != generation {
		return nil
	}
	replaceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-a.Instance.Done():
			cancel()
		case <-replaceCtx.Done():
		}
	}()
	return e.deploy(replaceCtx, ctx, revision.Selection{SHA: a.Revision, Name: "memory replacement"}, &generation)
}

func (e *Engine) deploy(ctx, watchCtx context.Context, sel revision.Selection, expected *uint64) error {
	id := e.nextID(sel.SHA)
	prepared, err := e.ensurePrepared(ctx, sel.SHA, id)
	if err != nil {
		return err
	}
	// Runtime.Start binds the sandbox process lifetime to its context. Operations
	// and health watches may be cancelled before graceful proxy drain completes,
	// so active processes use the engine-owned runtime context instead.
	candidate, err := e.Runtime.Start(e.runtimeContext(watchCtx), id, prepared, e.Cfg.RunCommand, e.Cfg.ServicePort, e.Cfg.AppEnv)
	if err != nil {
		e.invalidatePrepared(prepared)
		return fmt.Errorf("start candidate: %w", err)
	}
	if err = e.Health.Ready(ctx, candidate.Endpoint(), candidate, e.Cfg.StartupTimeout); err != nil {
		e.cleanup(candidate, true)
		return fmt.Errorf("candidate rejected: %w", err)
	}
	backend, err := proxy.NewBackend(candidate.Endpoint())
	if err != nil {
		e.cleanup(candidate, true)
		return err
	}
	e.mu.Lock()
	old := e.active
	if expected != nil && (old == nil || old.generation != *expected || old.Instance.Exited() || ctx.Err() != nil) {
		e.mu.Unlock()
		e.cleanup(candidate, true)
		return fmt.Errorf("active deployment failed during replacement")
	}
	e.seq++
	a := &Active{sel.SHA, e.Digest, prepared, candidate, backend, false, e.seq}
	e.active = a
	if err = e.Store.Save(state.State{Revision: sel.SHA, Digest: e.Digest, ImageRef: e.Cfg.RuntimeImage, Prepared: prepared, Bundle: candidate.BundlePath(), PID: candidate.PID()}); err != nil {
		e.active = old
		e.mu.Unlock()
		e.cleanup(candidate, true)
		return err
	}
	e.Proxy.Set(backend)
	e.mu.Unlock()
	log.Printf("cut over to revision %s", sel.SHA)
	e.watch(watchCtx, a)
	if old != nil {
		// Once routing has changed, retiring the old backend is committed
		// lifecycle work. The operation context may be cancelled by SIGTERM,
		// but main waits for the controller before shutting the engine down, so
		// preserve in-flight requests through the fixed drain deadline.
		drained := old.Backend.Drain(context.Background(), e.Drain)
		log.Printf("old deployment drain complete=%t", drained)
		e.cleanup(old.Instance, true)
		if old.Prepared != prepared {
			e.invalidatePrepared(old.Prepared)
		}
	}
	e.removeOtherPrepared(prepared)
	return nil
}
func (e *Engine) ensurePrepared(ctx context.Context, sha, id string) (string, error) {
	prepID := preparedID(sha, e.Digest)
	prepared := filepath.Join(e.Cfg.DataDir, "prepared", prepID)
	if e.Runtime.ValidatePrepared(prepared) == nil {
		return prepared, nil
	}
	e.invalidatePrepared(prepared)
	deployment := filepath.Join(e.Cfg.DataDir, "deployments", id)
	defer os.RemoveAll(deployment)
	checkout := filepath.Join(deployment, "checkout")
	if err := e.Checkout.Checkout(ctx, sha, checkout); err != nil {
		return "", fmt.Errorf("checkout candidate: %w", err)
	}
	result, err := e.Runtime.Prepare(ctx, prepID, e.Rootfs, checkout, e.Cfg.SetupCommand, e.Cfg.ServicePort, e.Cfg.AppEnv)
	if err != nil {
		return "", fmt.Errorf("prepare candidate: %w", err)
	}
	if err = e.Runtime.ValidatePrepared(result); err != nil {
		e.invalidatePrepared(result)
		return "", fmt.Errorf("prepared candidate validation: %w", err)
	}
	return result, nil
}
func (e *Engine) invalidatePrepared(path string) {
	root := filepath.Join(e.Cfg.DataDir, "prepared")
	if within(root, path) {
		_ = os.RemoveAll(path)
	}
}
func (e *Engine) removeOtherPrepared(keep string) {
	root := filepath.Join(e.Cfg.DataDir, "prepared")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if path != keep && within(root, path) {
			_ = os.RemoveAll(path)
		}
	}
}
func preparedID(sha, digest string) string {
	if len(sha) > 16 {
		sha = sha[:16]
	}
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return sha + "-" + digest
}
func (e *Engine) Recover(ctx context.Context, generation uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.active
	if a == nil || a.generation != generation {
		return nil
	}
	e.Proxy.Set(nil)
	old := a.Instance
	stop(old)
	id := e.nextID(a.Revision)
	var inst Instance
	var err error
	restarted := false
	prepared := a.Prepared
	if !a.Restarted {
		a.Restarted = true
		restarted = true
		e.removeInstancePath(filepath.Join(e.Cfg.DataDir, "runsc-root"), old.RootPath())
		inst, err = e.Runtime.Restart(e.runtimeContext(ctx), id, old.BundlePath(), e.Cfg.RunCommand, e.Cfg.ServicePort, e.Cfg.AppEnv)
		log.Printf("restarting revision %s using existing writable installation", a.Revision)
	} else {
		var prepErr error
		prepared, prepErr = e.ensurePrepared(ctx, a.Revision, id)
		if prepErr != nil {
			return prepErr
		}
		inst, err = e.Runtime.Start(e.runtimeContext(ctx), id, prepared, e.Cfg.RunCommand, e.Cfg.ServicePort, e.Cfg.AppEnv)
		log.Printf("redeploying revision %s with fresh writable filesystem", a.Revision)
	}
	if err != nil {
		if !restarted {
			e.invalidatePrepared(prepared)
		}
		return err
	}
	if err = e.Health.Ready(ctx, inst.Endpoint(), inst, e.Cfg.StartupTimeout); err != nil {
		e.cleanup(inst, !restarted)
		return err
	}
	b, err := proxy.NewBackend(inst.Endpoint())
	if err != nil {
		e.cleanup(inst, !restarted)
		return err
	}
	e.seq++
	replacement := &Active{Revision: a.Revision, Digest: a.Digest, Prepared: prepared, Instance: inst, Backend: b, Restarted: restarted, generation: e.seq}
	if err = e.Store.Save(state.State{Revision: replacement.Revision, Digest: replacement.Digest, ImageRef: e.Cfg.RuntimeImage, Prepared: replacement.Prepared, Bundle: inst.BundlePath(), PID: inst.PID()}); err != nil {
		e.cleanup(inst, !restarted)
		return err
	}
	e.active = replacement
	e.Proxy.Set(b)
	e.watch(ctx, replacement)
	if !restarted {
		e.cleanup(old, true)
	}
	e.removeOtherPrepared(replacement.Prepared)
	return nil
}
func (e *Engine) MarkUnhealthy(generation uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != nil && e.active.generation == generation {
		e.Proxy.Set(nil)
	}
}
func (e *Engine) Selected() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil {
		return ""
	}
	return e.active.Revision
}
func (e *Engine) watch(ctx context.Context, a *Active) {
	instance, endpoint, generation := a.Instance, a.Instance.Endpoint(), a.generation
	go func() {
		r := e.Health.UntilFailure(ctx, endpoint, instance, e.Cfg.HealthFailures)
		if ctx.Err() == nil && r != health.Healthy {
			e.MarkUnhealthy(generation)
			e.Failures <- generation
		}
	}()
	if e.Cfg.ServiceMemoryLimit > 0 && e.MemoryExceeded != nil {
		interval := e.MemoryInterval
		if interval <= 0 {
			interval = time.Second
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-instance.Done():
					return
				case <-ticker.C:
					used, err := instance.MemoryUsage()
					if err != nil {
						log.Printf("deployment memory sample failed: %v", err)
						continue
					}
					if used > e.Cfg.ServiceMemoryLimit {
						select {
						case e.MemoryExceeded <- generation:
						case <-ctx.Done():
						}
						return
					}
				}
			}
		}()
	}
}
func (e *Engine) Shutdown(ctx context.Context) {
	e.mu.Lock()
	a := e.active
	e.active = nil
	if a != nil {
		e.Proxy.Set(nil)
	}
	e.mu.Unlock()
	if a != nil {
		drained := a.Backend.Drain(ctx, e.Drain)
		log.Printf("active deployment shutdown drain complete=%t", drained)
		stopContext(ctx, a.Instance)
	}
}
func stop(i Instance) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = i.Stop(ctx)
}
func stopContext(ctx context.Context, i Instance) { _ = i.Stop(ctx) }
func (e *Engine) cleanup(i Instance, bundle bool) {
	stop(i)
	e.removeInstancePath(filepath.Join(e.Cfg.DataDir, "runsc-root"), i.RootPath())
	if bundle {
		e.removeInstancePath(filepath.Join(e.Cfg.DataDir, "instances"), i.BundlePath())
	}
}
func (e *Engine) removeInstancePath(root, path string) {
	if within(root, path) {
		_ = os.RemoveAll(path)
	}
}
func within(root, path string) bool {
	r, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	p, err := filepath.Abs(path)
	return err == nil && p != r && strings.HasPrefix(p, r+string(filepath.Separator))
}
func backendOf(a *Active) *proxy.Backend {
	if a == nil {
		return nil
	}
	return a.Backend
}

type Selector interface {
	Select(context.Context, string, string, string) (revision.Selection, error)
}
type Controller struct {
	Cfg      config.Config
	Selector Selector
	Engine   *Engine
	Initial  *revision.Selection
}

func (c *Controller) Run(ctx context.Context) {
	updates := make(chan revision.Selection, 1)
	go c.poll(ctx, updates)
	selected := c.Engine.Selected()
	pending := c.Initial
	var recovery *uint64
	var retry <-chan time.Time
	if pending != nil {
		retry = time.After(0)
	}
	acceptUpdate := func(s revision.Selection) {
		if s.SHA == selected {
			// The newest selection owns desired state even when it moves back to
			// the active revision. Cancel any older failed candidate.
			pending = nil
			return
		}
		pending = &s
	}
	for {
		attempt := false
		select {
		case <-ctx.Done():
			return
		case s := <-updates:
			acceptUpdate(s)
			attempt = true
		case gen := <-c.Engine.Failures:
			c.Engine.MarkUnhealthy(gen)
			recovery = &gen
			attempt = true
		case gen := <-c.Engine.MemoryExceeded:
			if err := c.Engine.Replace(ctx, gen); err != nil && ctx.Err() == nil {
				log.Printf("memory replacement failed; restarting active deployment: %v", err)
				recovery = &gen
			}
			attempt = true
		case <-retry:
			retry = nil
			attempt = true
		}
		if !attempt {
			continue
		}
		for {
			select {
			case s := <-updates:
				acceptUpdate(s)
			default:
				goto drained
			}
		}
	drained:
		if pending != nil {
			s := *pending
			if err := c.Engine.Deploy(ctx, s); err != nil {
				log.Printf("deployment %s failed; retrying after health interval: %v", s.SHA, err)
				retry = time.After(c.Cfg.HealthInterval)
				continue
			}
			selected, pending, recovery = s.SHA, nil, nil
			continue
		}
		if recovery != nil {
			if err := c.Engine.Recover(ctx, *recovery); err != nil {
				log.Printf("recovery failed; retrying after health interval: %v", err)
				retry = time.After(c.Cfg.HealthInterval)
				continue
			}
			recovery = nil
		}
	}
}
func (c *Controller) poll(ctx context.Context, out chan revision.Selection) {
	check := func() {
		x, e := c.Selector.Select(ctx, c.Cfg.UpdateMethod, c.Cfg.GitBranch, c.Cfg.UpdatePattern)
		if e != nil {
			log.Printf("revision check failed: %v", e)
			return
		}
		select {
		case out <- x:
		default:
			<-out
			out <- x
		}
	}
	check()
	t := time.NewTicker(c.Cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
func Serve(ctx context.Context, h http.Handler) error {
	s := &http.Server{Addr: ":80", Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		d, c := context.WithTimeout(context.Background(), 35*time.Second)
		defer c()
		shutdownDone <- s.Shutdown(d)
	}()
	e := s.Serve(listener)
	if e == http.ErrServerClosed {
		return <-shutdownDone
	}
	return e
}
