package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/supervisor"
)

func TestReconciliationFailurePropagatesToProcessOwner(t *testing.T) {
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "state", "active.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), config.Config{DataDir: data}, proxy.New(), nil)
	if err == nil || !strings.Contains(err.Error(), "restart reconciliation") {
		t.Fatalf("reconciliation error did not reach process owner: %v", err)
	}
}

func TestExistingControlOwnerPreventsReconciliation(t *testing.T) {
	data := t.TempDir()
	cfg := config.Config{DataDir: data, RuntimeImage: "debian:bookworm-slim"}
	c := &supervisor.Controller{Cfg: cfg, Engine: &supervisor.Engine{Cfg: cfg, Proxy: proxy.New()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closeControl, err := c.StartControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	if err := os.MkdirAll(filepath.Join(data, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "state", "active.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	err = run(ctx, cfg, proxy.New(), nil)
	if err == nil || !strings.Contains(err.Error(), "control socket is already in use") {
		t.Fatalf("second supervisor reached state reconciliation: %v", err)
	}
	t.Setenv("DATA_DIR", data)
	if err := cli([]string{"status"}, io.Discard); err != nil {
		t.Fatal("second supervisor replaced the original control socket:", err)
	}
}
