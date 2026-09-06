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
	"github.com/example/easy-service/internal/process"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/sandbox"
	"github.com/example/easy-service/internal/state"
	"github.com/example/easy-service/internal/supervisor"
)

type runtimeAdapter struct{ sandbox.Runtime }

func (r runtimeAdapter) Start(c context.Context, a, b, d string, p int, e []string) (supervisor.Instance, error) {
	return r.Runtime.Start(c, a, b, d, p, e)
}
func (r runtimeAdapter) Restart(c context.Context, a, b, d string, p int, e []string) (supervisor.Instance, error) {
	return r.Runtime.Restart(c, a, b, d, p, e)
}
func main() {
	cfg, err := config.Load()
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
	serverDrained := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		err := run(ctx, cfg, p, serverDrained)
		result <- err
		if err != nil {
			cancel()
		}
	}()
	if err = supervisor.Serve(ctx, p); err != nil && ctx.Err() == nil {
		log.Printf("HTTP server: %v", err)
	}
	cancel()
	close(serverDrained)
	select {
	case err = <-result:
	case <-time.After(45 * time.Second):
		log.Fatal("supervisor shutdown deadline expired")
	}
	if err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cfg config.Config, p *proxy.Proxy, serverDrained <-chan struct{}) error {
	runtimeCtx, cancelRuntime := context.WithCancel(context.Background())
	defer cancelRuntime()
	store := state.Store{Data: cfg.DataDir}
	prior, _ := store.Load()
	if err := store.Reconcile(); err != nil {
		return fmt.Errorf("restart reconciliation: %w", err)
	}
	images := imagepkg.Manager{Data: cfg.DataDir, Runner: process.Runner{}}
	var prepared imagepkg.Prepared
	for {
		var err error
		prepared, err = images.Prepare(ctx, cfg.RuntimeImage)
		if err != nil && prior.ImageRef == cfg.RuntimeImage && prior.Digest != "" {
			prepared, err = images.Cached(cfg.RuntimeImage, prior.Digest)
		}
		if err == nil {
			break
		}
		log.Printf("runtime image preparation failed; retrying: %v", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.HealthInterval):
		}
	}
	git := revision.New(cfg.GitURL, cfg.GitToken, cfg.DataDir)
	rt := runtimeAdapter{sandbox.Runtime{Data: cfg.DataDir, Stdout: os.Stdout, Stderr: os.Stderr}}
	engine := &supervisor.Engine{Cfg: cfg, Runtime: rt, Checkout: git, Proxy: p, Store: store, Health: health.New(cfg.HealthInterval, cfg.HealthPath), Rootfs: prepared.Rootfs, Digest: prepared.Digest, Drain: 30 * time.Second, RuntimeContext: runtimeCtx, Failures: make(chan uint64, 1), MemoryExceeded: make(chan uint64, 1)}
	var initial *revision.Selection
	if prior.ImageRef == cfg.RuntimeImage && prior.Revision != "" {
		initial = &revision.Selection{SHA: prior.Revision, Name: "recovery"}
	}
	controller := &supervisor.Controller{Cfg: cfg, Selector: git, Engine: engine, Initial: initial}
	controller.Run(ctx)
	<-serverDrained
	shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	engine.Shutdown(shutdown)
	return nil
}
