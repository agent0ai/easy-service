package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/proxy"
)

func TestReconciliationFailurePropagatesToProcessOwner(t *testing.T) {
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "state", "active.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), config.Config{DataDir: data}, proxy.New(), make(chan struct{}))
	if err == nil || !strings.Contains(err.Error(), "restart reconciliation") {
		t.Fatalf("reconciliation error did not reach process owner: %v", err)
	}
}
