package supervisor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/logs"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/state"
)

type Instance interface {
	Exited() bool
	Done() <-chan error
	Stop(context.Context) error
	Kill(context.Context) error
	Endpoint() string
	BundlePath() string
	RootPath() string
	MemoryUsage() (uint64, error)
}
type Runtime interface {
	Prepare(context.Context, string, string, string, string, []string) (string, error)
	ValidatePrepared(string) error
	// Start separates filesystem-operation cancellation from process lifetime.
	Start(context.Context, context.Context, string, string, string, []string) (Instance, error)
	Restart(context.Context, string, string, string, []string) (Instance, error)
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
	cancelWatch                context.CancelFunc
	failed                     chan struct{}
}

type retirement struct {
	instance Instance
	ctx      context.Context
	cancel   context.CancelFunc
}

// Deployment is immutable while a candidate is prepared. Existing watchers keep
// their own committed snapshot until publication retires them.
type Deployment struct {
	Cfg            config.Config
	Overrides      map[string]string
	Runtime        Runtime
	Checkout       Checkout
	Health         Health
	Rootfs, Digest string
}

type Engine struct {
	Cfg            config.Config
	Overrides      map[string]string
	Logs           *logs.Manager
	CollectCaches  func(Deployment)
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
	draining       map[uint64]*retirement
	seq            uint64
	Failures       chan uint64
	MemoryExceeded chan uint64
	MemoryInterval time.Duration
}

func (e *Engine) nextID() (string, error) {
	for {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("instance identity: %w", err)
		}
		id := hex.EncodeToString(b[:])
		if e.Logs != nil && e.Logs.HasInstance(id) {
			continue
		}
		available := true
		for _, dir := range []string{"instances", "runsc-root", "deployments"} {
			if _, err := os.Lstat(filepath.Join(e.Cfg.DataDir, dir, id)); err == nil {
				available = false
				break
			} else if !os.IsNotExist(err) {
				return "", err
			}
		}
		if available {
			return id, nil
		}
	}
}

func (e *Engine) snapshot() Deployment {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Deployment{e.Cfg, e.Overrides, e.Runtime, e.Checkout, e.Health, e.Rootfs, e.Digest}
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
	if a.Instance.Exited() {
		return fmt.Errorf("active deployment exited before replacement")
	}
	return e.deploy(ctx, ctx, revision.Selection{SHA: a.Revision}, &generation)
}

func (e *Engine) deploy(ctx, watchCtx context.Context, sel revision.Selection, expected *uint64) error {
	return e.deployWith(ctx, watchCtx, sel, expected, e.snapshot())
}

func (e *Engine) deployWith(ctx, watchCtx context.Context, sel revision.Selection, expected *uint64, plan Deployment) error {
	if !revision.ValidSHA(sel.SHA) {
		return fmt.Errorf("refusing non-exact deployment SHA")
	}
	var prepared string
	committed := false
	defer func() {
		if !committed {
			e.collectFailed(plan, prepared)
		}
	}()
	e.mu.Lock()
	previous := e.active
	serving := previous != nil && e.Proxy.Current() == previous.Backend && !previous.Instance.Exited()
	e.mu.Unlock()
	if serving {
		opCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		ctx = opCtx
		go func() {
			select {
			case <-previous.Instance.Done():
				cancel()
			case <-previous.failed:
				cancel()
			case <-opCtx.Done():
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := e.nextID()
	if err != nil {
		return err
	}
	log.Printf("preparing candidate instance=%s revision=%s", id, sel.SHA)
	prepared, err = e.prepareWith(ctx, sel.SHA, id, plan)
	if err != nil {
		return err
	}
	// Runtime.Start binds the sandbox process lifetime to its context. Operations
	// and health watches may be cancelled before graceful proxy drain completes,
	// so active processes use the engine-owned runtime context instead.
	e.registerLog(id, id, sel.SHA, plan)
	log.Printf("starting candidate instance=%s revision=%s", id, sel.SHA)
	candidate, err := plan.Runtime.Start(ctx, e.runtimeContext(watchCtx), id, prepared, plan.Cfg.RunCommand, plan.Cfg.AppEnv)
	if e.Logs != nil {
		e.Logs.Forget(id)
	}
	if err != nil {
		e.invalidatePrepared(prepared)
		return fmt.Errorf("start candidate: %w", err)
	}
	if err = plan.Health.Ready(ctx, candidate.Endpoint(), candidate, plan.Cfg.StartupTimeout); err != nil {
		e.cleanup(candidate, true)
		return fmt.Errorf("candidate rejected: %w", err)
	}
	a := &Active{Revision: sel.SHA, Digest: plan.Digest, Prepared: prepared, Instance: candidate}
	old, err := e.publishWith(ctx, watchCtx, a, expected, plan)
	if err != nil {
		e.cleanup(candidate, true)
		return err
	}
	committed = true
	log.Printf("candidate healthy; cut over instance=%s revision=%s", id, sel.SHA)
	if old != nil {
		// Once routing has changed, retiring the old backend is committed
		// lifecycle work. The operation context may be cancelled by SIGTERM,
		// but main waits for the controller before shutting the engine down, so
		// preserve in-flight requests through the fixed drain deadline.
		log.Printf("draining previous deployment revision=%s", old.Revision)
		drained := e.drain(old, drainTimeout(plan.Cfg, e.Drain))
		log.Printf("old deployment drain complete=%t", drained)
		e.cleanup(old.Instance, true)
		e.mu.Lock()
		if old.Instance.Exited() {
			if r := e.draining[old.generation]; r != nil {
				r.cancel()
				delete(e.draining, old.generation)
			}
		}
		e.mu.Unlock()
		if old.Prepared != prepared {
			e.invalidatePrepared(old.Prepared)
		}
	}
	e.removeOtherPrepared(prepared)
	if e.CollectCaches != nil {
		e.CollectCaches(plan)
	}
	return nil
}

func (e *Engine) publish(ctx, watchCtx context.Context, a *Active, expected *uint64) (*Active, error) {
	return e.publishWith(ctx, watchCtx, a, expected, e.snapshot())
}
func (e *Engine) publishWith(ctx, watchCtx context.Context, a *Active, expected *uint64, plan Deployment) (*Active, error) {
	backend, err := proxy.NewBackend(a.Instance.Endpoint())
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.active
	if ctx.Err() != nil || a.Instance.Exited() || (expected != nil && (old == nil || old.generation != *expected || old.Instance.Exited())) {
		return nil, fmt.Errorf("candidate cancelled or deployment exited before cutover")
	}
	e.seq++
	a.Backend, a.generation = backend, e.seq
	if err = e.Store.Save(state.State{Revision: a.Revision, Digest: a.Digest, GitURL: plan.Cfg.GitURL, ImageRef: plan.Cfg.RuntimeImage, Overrides: plan.Overrides}); err != nil {
		return nil, err
	}
	e.Cfg, e.Overrides, e.Runtime, e.Checkout, e.Health, e.Rootfs, e.Digest = plan.Cfg, plan.Overrides, plan.Runtime, plan.Checkout, plan.Health, plan.Rootfs, plan.Digest
	e.active = a
	if old != nil && !old.Instance.Exited() {
		retiredCtx, cancel := context.WithCancel(context.Background())
		if e.draining == nil {
			e.draining = make(map[uint64]*retirement)
		}
		e.draining[old.generation] = &retirement{old.Instance, retiredCtx, cancel}
	}
	e.Proxy.Set(backend)
	e.watch(watchCtx, a)
	if old != nil && old.cancelWatch != nil {
		old.cancelWatch()
	}
	return old, nil
}

func (e *Engine) drain(old *Active, timeout time.Duration) bool {
	e.mu.Lock()
	r := e.draining[old.generation]
	e.mu.Unlock()
	if r == nil {
		return old.Backend.Drain(context.Background(), timeout)
	}
	return old.Backend.Drain(r.ctx, timeout)
}

// KillDraining bypasses serialized deployment work: a deployment can be waiting
// inside Drain. Publication registers ownership before traffic changes.
func (e *Engine) KillDraining(ctx context.Context) (int, error) {
	e.mu.Lock()
	retired := make([]*retirement, 0, len(e.draining))
	for _, r := range e.draining {
		retired = append(retired, r)
		r.cancel()
	}
	e.mu.Unlock()
	log.Printf("force-stopping draining instances count=%d", len(retired))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	for _, r := range retired {
		wg.Add(1)
		go func(r *retirement) {
			defer wg.Done()
			if err := r.instance.Kill(ctx); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
				return
			}
			e.cleanup(r.instance, true)
			e.mu.Lock()
			for generation, entry := range e.draining {
				if entry == r && r.instance.Exited() {
					delete(e.draining, generation)
				}
			}
			e.mu.Unlock()
		}(r)
	}
	wg.Wait()
	if err := errors.Join(failures...); err != nil {
		return len(retired) - len(failures), err
	}
	return len(retired), nil
}

func (e *Engine) ensurePrepared(ctx context.Context, sha, id string) (string, error) {
	return e.prepareWith(ctx, sha, id, e.snapshot())
}
func (e *Engine) prepareWith(ctx context.Context, sha, id string, plan Deployment) (string, error) {
	prepID := preparedID(sha, plan.Digest, plan.Cfg)
	prepared := filepath.Join(plan.Cfg.DataDir, "prepared", prepID)
	if plan.Runtime.ValidatePrepared(prepared) == nil {
		return prepared, nil
	}
	e.invalidatePrepared(prepared)
	deployment := filepath.Join(plan.Cfg.DataDir, "deployments", id)
	defer os.RemoveAll(deployment)
	checkout := filepath.Join(deployment, "checkout")
	if err := plan.Checkout.Checkout(ctx, sha, checkout); err != nil {
		return "", fmt.Errorf("checkout candidate: %w", err)
	}
	if plan.Cfg.SetupCommand != "" {
		e.registerLog("setup-"+prepID, id, sha, plan)
		if e.Logs != nil {
			defer e.Logs.Forget("setup-" + prepID)
		}
	}
	log.Printf("running setup instance=%s revision=%s", id, sha)
	result, err := plan.Runtime.Prepare(ctx, prepID, plan.Rootfs, checkout, plan.Cfg.SetupCommand, plan.Cfg.AppEnv)
	if err != nil {
		return "", fmt.Errorf("prepare candidate: %w", err)
	}
	if err = plan.Runtime.ValidatePrepared(result); err != nil {
		e.invalidatePrepared(result)
		return "", fmt.Errorf("prepared candidate validation: %w", err)
	}
	return result, nil
}
func (e *Engine) collectFailed(plan Deployment, prepared string) {
	current := e.snapshot()
	e.mu.Lock()
	active := e.active
	e.mu.Unlock()
	keep := []string{prepared}
	if active != nil {
		keep = append(keep, active.Prepared)
	}
	e.removeOtherPrepared(keep...)
	if current.Rootfs == "" {
		current = plan
	}
	if current.Rootfs != "" && e.CollectCaches != nil {
		e.CollectCaches(current)
	}
}

func (e *Engine) registerLog(key, id, sha string, plan Deployment) {
	if e.Logs != nil {
		e.Logs.Register(key, logs.Info{ID: id, Commit: sha, GitURL: plan.Cfg.GitURL, Image: plan.Cfg.RuntimeImage, Digest: plan.Digest, DeployedAt: time.Now().UTC()})
	}
}
func drainTimeout(cfg config.Config, fallback time.Duration) time.Duration {
	if cfg.DrainTimeout > 0 {
		return cfg.DrainTimeout
	}
	return fallback
}

func (e *Engine) invalidatePrepared(path string) {
	root := filepath.Join(e.Cfg.DataDir, "prepared")
	if state.Within(root, path) {
		_ = os.RemoveAll(path)
	}
}
func (e *Engine) removeOtherPrepared(keep ...string) {
	root := filepath.Join(e.Cfg.DataDir, "prepared")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if !slices.Contains(keep, path) && state.Within(root, path) {
			_ = os.RemoveAll(path)
		}
	}
}
func preparedID(sha, digest string, cfg config.Config) string {
	inputs := struct {
		Version            int
		SHA, Digest, Setup string
		Env                []string
	}{2, sha, digest, cfg.SetupCommand, cfg.AppEnv}
	b, _ := json.Marshal(inputs)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (e *Engine) Recover(ctx context.Context, generation uint64) error {
	return e.recover(ctx, ctx, generation, false)
}

func (e *Engine) recover(ctx, watchCtx context.Context, generation uint64, forceRestart bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	a := e.active
	if a == nil || a.generation != generation {
		e.mu.Unlock()
		return nil
	}
	e.Proxy.Set(nil)
	if a.cancelWatch != nil {
		a.cancelWatch()
	}
	restarted := forceRestart || !a.Restarted
	e.mu.Unlock()
	old := a.Instance
	if err := stop(old); err != nil {
		return err
	}
	e.mu.Lock()
	a.Restarted = true
	e.mu.Unlock()
	id, identityErr := e.nextID()
	if identityErr != nil {
		return identityErr
	}
	var inst Instance
	var err error
	prepared := a.Prepared
	if restarted {
		e.removeInstancePath(filepath.Join(e.Cfg.DataDir, "runsc-root"), old.RootPath())
		e.registerLog(id, id, a.Revision, e.snapshot())
		inst, err = e.Runtime.Restart(e.runtimeContext(watchCtx), id, old.BundlePath(), e.Cfg.RunCommand, e.Cfg.AppEnv)
		log.Printf("restarting revision %s using existing writable installation", a.Revision)
	} else {
		var prepErr error
		prepared, prepErr = e.ensurePrepared(ctx, a.Revision, id)
		if prepErr != nil {
			return prepErr
		}
		e.registerLog(id, id, a.Revision, e.snapshot())
		inst, err = e.Runtime.Start(ctx, e.runtimeContext(watchCtx), id, prepared, e.Cfg.RunCommand, e.Cfg.AppEnv)
		log.Printf("redeploying revision %s with fresh writable filesystem", a.Revision)
	}
	if e.Logs != nil {
		e.Logs.Forget(id)
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
	replacement := &Active{Revision: a.Revision, Digest: a.Digest, Prepared: prepared, Instance: inst, Restarted: restarted}
	if _, err = e.publish(ctx, watchCtx, replacement, nil); err != nil {
		e.cleanup(inst, !restarted)
		return err
	}
	if !restarted {
		e.cleanup(old, true)
	}
	e.removeOtherPrepared(replacement.Prepared)
	return nil
}
func (e *Engine) MarkUnhealthy(generation uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != nil && e.active.generation == generation {
		e.Proxy.Set(nil)
		return true
	}
	return false
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
	ctx, a.cancelWatch = context.WithCancel(ctx)
	a.failed = make(chan struct{})
	instance, endpoint, generation := a.Instance, a.Instance.Endpoint(), a.generation
	cfg, checker := e.Cfg, e.Health
	go func() {
		r := checker.UntilFailure(ctx, endpoint, instance, cfg.HealthFailures)
		if ctx.Err() == nil && r != health.Healthy {
			log.Printf("deployment failed revision=%s health_result=%d", a.Revision, r)
			close(a.failed)
			e.MarkUnhealthy(generation)
			select {
			case e.Failures <- generation:
			case <-ctx.Done():
			}
		}
	}()
	if cfg.ServiceMemoryLimit > 0 && e.MemoryExceeded != nil {
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
					if used > cfg.ServiceMemoryLimit {
						log.Printf("memory threshold exceeded revision=%s used=%d limit=%d", a.Revision, used, cfg.ServiceMemoryLimit)
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
		if a.cancelWatch != nil {
			a.cancelWatch()
		}
	}
	e.mu.Unlock()
	if a != nil {
		drained := a.Backend.Drain(ctx, drainTimeout(e.Cfg, e.Drain))
		log.Printf("active deployment shutdown drain complete=%t", drained)
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := stopContext(stopCtx, a.Instance); err != nil {
			log.Printf("sandbox shutdown failed: %v", err)
		}
	}
}
func stop(i Instance) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return stopContext(ctx, i)
}
func stopContext(ctx context.Context, i Instance) error {
	err := i.Stop(ctx)
	if !i.Exited() {
		return fmt.Errorf("sandbox is still running after stop: %v", err)
	}
	return nil
}
func (e *Engine) cleanup(i Instance, bundle bool) {
	log.Printf("stopping retired/private instance bundle=%s", filepath.Base(i.BundlePath()))
	if err := stop(i); err != nil {
		log.Printf("sandbox cleanup deferred: %v", err)
		return
	}
	e.mu.Lock()
	data := e.Cfg.DataDir
	e.mu.Unlock()
	e.removeInstancePath(filepath.Join(data, "runsc-root"), i.RootPath())
	if bundle {
		e.removeInstancePath(filepath.Join(data, "instances"), i.BundlePath())
	}
}
func (e *Engine) removeInstancePath(root, path string) {
	if state.Within(root, path) {
		_ = os.RemoveAll(path)
	}
}

type Selector interface {
	Select(context.Context, string, string, string) (revision.Selection, error)
}
type Controller struct {
	Cfg       config.Config
	Settings  *config.Settings
	Configure func(context.Context, config.Config) (Deployment, Selector, error)
	Selector  Selector
	Engine    *Engine
	Initial   *revision.Selection
	commands  chan command
}

func (c *Controller) Run(ctx context.Context) {
	updates := make(chan revision.Selection, 1)
	refresh := make(chan selectionRequest)
	var stopPoll context.CancelFunc
	var pollDone chan struct{}
	startPoll := func(immediate bool) {
		pollCtx, cancel := context.WithCancel(ctx)
		stopPoll = cancel
		pollDone = make(chan struct{})
		cfg, selector, done := c.Cfg, c.Selector, pollDone
		go func() { defer close(done); c.poll(pollCtx, cfg, selector, immediate, updates, refresh) }()
	}
	startPoll(true)
	defer func() { stopPoll(); <-pollDone }()
	selected := c.Engine.Selected()
	pending := c.Initial
	pendingFailed := false
	var recovery *uint64
	var retry <-chan time.Time
	retryTimer := time.NewTimer(time.Hour)
	retryTimer.Stop()
	defer retryTimer.Stop()
	scheduleRetry := func(delay time.Duration) {
		if !retryTimer.Stop() {
			select {
			case <-retryTimer.C:
			default:
			}
		}
		retryTimer.Reset(delay)
		retry = retryTimer.C
	}
	if pending != nil {
		scheduleRetry(0)
	}
	acceptUpdate := func(s revision.Selection) bool {
		if s.SHA == selected {
			// The newest selection owns desired state even when it moves back to
			// the active revision. Cancel any older failed candidate.
			changed := pending != nil
			pending = nil
			pendingFailed = false
			return changed
		}
		if pending != nil && pending.SHA == s.SHA {
			return false
		}
		pending = &s
		pendingFailed = false
		return true
	}
	for {
		attempt := false
		select {
		case <-ctx.Done():
			return
		case request := <-c.commands:
			if request.action == "apply" {
				stopPoll()
				<-pollDone
				select {
				case <-updates:
				default:
				}
			}
			op, cancel := context.WithCancel(ctx)
			stopCancel := context.AfterFunc(request.ctx, cancel)
			if request.ctx.Err() != nil {
				cancel()
			}
			sha, err := c.execute(op, ctx, request.action, refresh, updates)
			if request.action == "apply" {
				startPoll(false)
			}
			stopCancel()
			cancel()
			if err == nil {
				recovery = nil
				if sha != "" {
					selected, pending, pendingFailed = sha, nil, false
					retryTimer.Stop()
					retry = nil
				}
			} else if c.Engine.Proxy.Current() == nil {
				if generation := c.Engine.activeGeneration(); generation != 0 {
					recovery = &generation
					scheduleRetry(c.Cfg.HealthInterval)
				}
			}
			request.result <- err
			continue
		case s := <-updates:
			attempt = acceptUpdate(s)
		case gen := <-c.Engine.Failures:
			if !c.Engine.MarkUnhealthy(gen) {
				continue
			}
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
		// A candidate that already failed must not block recovery of the serving
		// revision. A genuinely newer selection still gets the first attempt.
		if recovery != nil && (pending == nil || pendingFailed) {
			if err := c.Engine.Recover(ctx, *recovery); err != nil {
				log.Printf("recovery failed; retrying after health interval: %v", err)
				scheduleRetry(c.Cfg.HealthInterval)
				continue
			}
			recovery = nil
			if pending != nil {
				scheduleRetry(c.Cfg.HealthInterval)
			}
			continue
		}
		if pending != nil {
			s := *pending
			if err := c.deploySelection(ctx, ctx, s); err != nil {
				pendingFailed = true
				log.Printf("deployment %s failed; retrying after health interval: %v", s.SHA, err)
				scheduleRetry(c.Cfg.HealthInterval)
			} else {
				selected, pending, recovery = s.SHA, nil, nil
				continue
			}
		}
		if recovery != nil {
			if err := c.Engine.Recover(ctx, *recovery); err != nil {
				log.Printf("recovery failed; retrying after health interval: %v", err)
				scheduleRetry(c.Cfg.HealthInterval)
				continue
			}
			recovery = nil
		}
	}
}
func (c *Controller) poll(ctx context.Context, cfg config.Config, selector Selector, immediate bool, out chan revision.Selection, refresh <-chan selectionRequest) {
	if selector == nil {
		<-ctx.Done()
		return
	}
	check := func() {
		x, e := selector.Select(ctx, cfg.UpdateMethod, cfg.GitBranch, cfg.UpdatePattern)
		if e != nil {
			log.Printf("revision check failed: %v", e)
			return
		}
		select {
		case out <- x:
		default:
			select {
			case <-out:
			default:
			}
			select {
			case out <- x:
			case <-ctx.Done():
			}
		}
	}
	if immediate {
		check()
	}
	t := time.NewTicker(cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		case request := <-refresh:
			s, err := selector.Select(request.ctx, cfg.UpdateMethod, cfg.GitBranch, cfg.UpdatePattern)
			request.result <- selectionResult{s, err}
		}
	}
}
func Serve(ctx, shutdownCtx context.Context, h http.Handler) error {
	s := &http.Server{Addr: ":80", Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		err := s.Shutdown(shutdownCtx)
		if err != nil {
			_ = s.Close()
		}
		shutdownDone <- err
	}()
	e := s.Serve(listener)
	if e == http.ErrServerClosed {
		return <-shutdownDone
	}
	return e
}
