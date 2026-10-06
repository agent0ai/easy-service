package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
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

type runtimeAdapter struct{ sandbox.Runtime }

func (r runtimeAdapter) Start(c, lifetime context.Context, a, b, d string, e []string) (supervisor.Instance, error) {
	return r.Runtime.Start(c, lifetime, a, b, d, e)
}
func (r runtimeAdapter) Restart(c context.Context, a, b, d string, e []string) (supervisor.Instance, error) {
	return r.Runtime.Restart(c, a, b, d, e)
}
func main() {
	if len(os.Args) > 1 {
		if err := cli(os.Args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	defaults := config.Environment()
	paths := make(map[string]string)
	for name, value := range config.Defaults {
		paths[name] = value
	}
	for _, name := range []string{"DATA_DIR", "CONFIG_DIR", "LOG_DIR"} {
		paths[name] = defaults[name]
	}
	bootstrap, err := config.Parse(paths, false)
	if err != nil {
		log.Fatal(err)
	}
	settings, _, err := config.Open(defaults, state.Store{Data: bootstrap.DataDir, ConfigDir: bootstrap.ConfigDir})
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := config.Parse(settings.Values(false), false)
	if err != nil {
		log.Fatal(err)
	}
	if err = os.MkdirAll(cfg.DataDir, 0700); err != nil {
		log.Fatal(err)
	}
	if err = sandbox.Validate(); err != nil {
		log.Fatalf("gVisor isolation unavailable: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	p := proxy.New()
	shutdownCtx, finishShutdown := context.WithCancel(context.Background())
	defer finishShutdown()
	result := make(chan error, 1)
	go func() {
		err := run(ctx, cfg, p, settings)
		result <- err
		finishShutdown()
		if err != nil {
			cancel()
		}
	}()
	if err = supervisor.Serve(ctx, shutdownCtx, p); err != nil && ctx.Err() == nil {
		log.Printf("HTTP server: %v", err)
	}
	cancel()
	err = <-result
	if err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cfg config.Config, p *proxy.Proxy, settings *config.Settings) error {
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	defer cancelRuntime()
	store := state.Store{Data: cfg.DataDir, ConfigDir: cfg.ConfigDir}
	git := revision.New(cfg.GitURL, cfg.GitToken, cfg.DataDir)
	rt := &runtimeAdapter{sandbox.Runtime{Data: cfg.DataDir, Stdout: os.Stdout, Stderr: os.Stderr}}
	engine := &supervisor.Engine{Cfg: cfg, Runtime: rt, Checkout: git, Proxy: p, Store: store, Health: health.New(cfg.HealthInterval, cfg.HealthPath), Drain: 30 * time.Second, RuntimeContext: runtimeCtx, Failures: make(chan uint64, 1), MemoryExceeded: make(chan uint64, 1)}
	controller := &supervisor.Controller{Cfg: cfg, Selector: git, Engine: engine, Settings: settings}
	if cfg.GitURL == "" || cfg.RunCommand == "" {
		controller.Selector = nil
	}
	closeControl, err := controller.StartControl(ctx)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	defer closeControl()
	prior, _ := store.Load()
	if err := store.Reconcile(); err != nil {
		return fmt.Errorf("restart reconciliation: %w", err)
	}
	var capture *logs.Manager
	if cfg.LogDir != "" {
		capture, err = logs.New(cfg.LogDir, logs.Policy{Days: cfg.LogRetentionDays, FileSize: cfg.LogMaxFileSize, TotalSize: cfg.LogMaxTotalSize})
		if err != nil {
			return fmt.Errorf("application logs: %w", err)
		}
		defer capture.Close()
		go capture.Run(runtimeCtx)
		engine.Logs = capture
	}
	if settings != nil {
		engine.Overrides = prior.Overrides
	}
	images := imagepkg.Manager{Data: cfg.DataDir, Runner: process.Runner{}}
	controller.Configure = func(op context.Context, next config.Config) (supervisor.Deployment, supervisor.Selector, error) {
		var prepared imagepkg.Prepared
		var err error
		if engine.Rootfs != "" && next.RuntimeImage == engine.Cfg.RuntimeImage {
			prepared, err = images.Cached(next.RuntimeImage, engine.Digest)
		} else {
			log.Printf("preparing runtime image image=%s", next.RuntimeImage)
			prepared, err = images.Prepare(op, next.RuntimeImage)
			if err != nil && engine.Rootfs == "" && prior.ImageRef == next.RuntimeImage && prior.Digest != "" {
				log.Printf("using cached runtime image after registry failure")
				prepared, err = images.Cached(next.RuntimeImage, prior.Digest)
			}
		}
		if err != nil {
			return supervisor.Deployment{}, nil, err
		}
		git := revision.New(next.GitURL, next.GitToken, next.DataDir)
		runtime := &runtimeAdapter{sandbox.Runtime{Data: next.DataDir, ImageEnv: prepared.Env, Logs: capture, SetupTimeout: next.SetupTimeout, Stdout: os.Stdout, Stderr: os.Stderr}}
		return supervisor.Deployment{Cfg: next, Runtime: runtime, Checkout: git, Health: health.New(next.HealthInterval, next.HealthPath), Rootfs: prepared.Rootfs, Digest: prepared.Digest}, git, nil
	}
	engine.CollectCaches = func(plan supervisor.Deployment) {
		prepared, err := images.Cached(plan.Cfg.RuntimeImage, plan.Digest)
		if err == nil {
			err = images.Prune(prepared)
		}
		if err != nil {
			log.Printf("image cache cleanup failed: %v", err)
		}
		if err := revision.New(plan.Cfg.GitURL, plan.Cfg.GitToken, plan.Cfg.DataDir).Prune(); err != nil {
			log.Printf("Git cache cleanup failed: %v", err)
		}
	}
	if cfg.GitURL != "" && cfg.RunCommand != "" && prior.Recoverable(cfg.GitURL, cfg.RuntimeImage) {
		controller.Initial = &revision.Selection{SHA: prior.Revision}
	}
	if controller.Selector == nil {
		log.Printf("waiting for configuration: set GIT_URL and RUN_COMMAND, then run config apply")
	}
	controller.Run(ctx)
	shutdown, cancel := context.WithTimeout(context.Background(), engine.Cfg.DrainTimeout+15*time.Second)
	defer cancel()
	engine.Shutdown(shutdown)
	return nil
}
