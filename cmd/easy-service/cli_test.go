package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/easy-service/internal/supervisor"
)

func TestCLICommandsOverUnixSocket(t *testing.T) {
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	t.Setenv("GIT_URL", "")
	t.Setenv("RUN_COMMAND", "")
	path := supervisor.ControlPath(data)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 4)
	fail := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		select {
		case <-fail:
			http.Error(w, "candidate rejected", http.StatusInternalServerError)
			return
		default:
		}
		json.NewEncoder(w).Encode(supervisor.Status{State: "running", Revision: "commit", RuntimeImage: "node:22-bookworm-slim", Digest: "sha256:digest", MemoryBytes: 2 << 20, MemoryLimit: 512 << 20})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	for _, action := range []string{"status", "redeploy", "restart"} {
		var out bytes.Buffer
		if err := cli([]string{action}, &out); err != nil {
			t.Fatal(err)
		}
		method := "POST"
		if action == "status" {
			method = "GET"
		}
		if request := <-requests; request != method+" /"+action {
			t.Fatalf("request: %s", request)
		}
		want := "State: running\nRevision: commit\nImage: node:22-bookworm-slim\nDigest: sha256:digest\nMemory: 2.0 MiB\nMemory limit: 512.0 MiB (soft)\n"
		if action != "status" {
			want = action + " completed\n" + want
		}
		if out.String() != want {
			t.Fatalf("CLI output changed: %q", out.String())
		}
	}
	for _, args := range [][]string{{"unknown"}, {"status", "extra"}} {
		if err := cli(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
	if err := cli([]string{"help"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	close(fail)
	if err := cli([]string{"redeploy"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "candidate rejected") {
		t.Fatalf("action failure was not reported: %v", err)
	}
	<-requests
	server.Shutdown(context.Background())
	if err := cli([]string{"status"}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing supervisor reported success")
	}
}
