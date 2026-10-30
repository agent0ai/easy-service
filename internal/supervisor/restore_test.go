package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/health"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/revision"
	"github.com/example/easy-service/internal/sandbox"
	"github.com/example/easy-service/internal/state"
)

func TestSavedSandboxResumeAndFreshFallback(t *testing.T) {
	for _, mode := range []string{"resume", "bad-health", "missing", "changed-environment", "changed-command", "changed-image", "failed-publish"} {
		t.Run(mode, func(t *testing.T) {
			origin := t.TempDir()
			if b, err := exec.Command("git", "init", "-b", "main", origin).CombinedOutput(); err != nil {
				t.Fatal(string(b), err)
			}
			sha := commitVersion(t, origin, "one")
			data := t.TempDir()
			cfg := config.Config{DataDir: data, GitURL: origin, RuntimeImage: "image", HealthPath: "/health", StartupTimeout: time.Second, HealthInterval: 100 * time.Millisecond, HealthFailures: 3}
			git := revision.New(origin, "", data)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rt := &httpRuntime{Runtime: sandbox.Runtime{Data: data}}
			e := &Engine{Cfg: cfg, Runtime: rt, Checkout: git, Proxy: proxy.New(), Store: state.Store{Data: data, ConfigDir: t.TempDir()}, Health: health.New(cfg.HealthInterval, "/health"), Rootfs: t.TempDir(), Digest: "digest", RuntimeContext: ctx, Failures: make(chan uint64, 1)}
			t.Cleanup(func() { e.Shutdown(context.Background()) })
			if _, err := git.Select(ctx, "commit", "main", "*"); err != nil {
				t.Fatal(err)
			}
			if err := e.Deploy(ctx, revision.Selection{SHA: sha}); err != nil {
				t.Fatal(err)
			}
			oldBundle := e.active.Instance.BundlePath()
			marker := filepath.Join(oldBundle, "rootfs", "app", "marker")
			if err := os.WriteFile(marker, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			// A policy-only update must retain the same resume metadata.
			if err := e.commitPolicy(ctx, ctx, e.snapshot()); err != nil {
				t.Fatal(err)
			}
			saved, err := e.Store.Load()
			if err != nil || saved.Sandbox != filepath.Base(oldBundle) || saved.ResumeKey == "" {
				t.Fatal("resume identity missing", saved, err)
			}
			e.Shutdown(context.Background())
			e.Restore = &saved
			switch mode {
			case "bad-health":
				if err := os.WriteFile(filepath.Join(oldBundle, "rootfs", "app", "version"), []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.RemoveAll(oldBundle); err != nil {
					t.Fatal(err)
				}
			case "changed-environment":
				e.Cfg.AppEnv = []string{"MODE=changed"}
			case "changed-command":
				e.Cfg.RunCommand = "changed"
			case "changed-image":
				e.Digest = "other-digest"
			case "failed-publish":
				blocked := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
				e.Store.ConfigDir = blocked
			}
			err = e.Deploy(ctx, revision.Selection{SHA: sha})
			if mode == "failed-publish" {
				if err == nil {
					t.Fatal("failed state write accepted")
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal("failed publication deleted accepted files:", err)
				}
				if e.Proxy.Current() != nil || len(rt.List()) != 0 {
					t.Fatal("failed publication left a serving process")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "resume" {
				if rt.starts != 1 || rt.restarts != 1 || e.active.Instance.BundlePath() != oldBundle {
					t.Fatal("resume created a new installation")
				}
				if body, err := os.ReadFile(marker); err != nil || string(body) != "preserved" {
					t.Fatal("writable state lost", err)
				}
			} else {
				if rt.starts != 2 || e.active.Instance.BundlePath() == oldBundle {
					t.Fatal("did not replace unusable/incompatible sandbox")
				}
				if mode == "bad-health" && rt.restarts != 1 {
					t.Fatal("bad saved sandbox was not tried once")
				}
				if mode != "bad-health" && rt.restarts != 0 {
					t.Fatal("attempted incompatible saved sandbox")
				}
			}
		})
	}
}
