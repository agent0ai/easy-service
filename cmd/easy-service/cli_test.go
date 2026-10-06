package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/example/easy-service/internal/config"
	"github.com/example/easy-service/internal/proxy"
	"github.com/example/easy-service/internal/state"
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
		if r.URL.Path == "/kill-draining" {
			json.NewEncoder(w).Encode(struct {
				Killed int `json:"killed"`
			}{2})
			return
		}
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
		if err := cli([]string{action}, strings.NewReader(""), &out); err != nil {
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
		if err := cli(args, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
	if err := cli([]string{"help"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	close(fail)
	if err := cli([]string{"redeploy"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "candidate rejected") {
		t.Fatalf("action failure was not reported: %v", err)
	}
	<-requests
	var killed bytes.Buffer
	if err := cli([]string{"kill-draining"}, strings.NewReader(""), &killed); err != nil || killed.String() != "Killed 2 draining instances\n" {
		t.Fatalf("kill-draining CLI: %q %v", killed.String(), err)
	}
	if got := <-requests; got != "POST /kill-draining" {
		t.Fatal(got)
	}
	server.Shutdown(context.Background())
	if err := cli([]string{"status"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("missing supervisor reported success")
	}
}

func TestConfigCLIArgumentsAndFilteredShow(t *testing.T) {
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	path := supervisor.ControlPath(data)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 10)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		if r.Method == "GET" {
			values := map[string]string{"APP_KEY": "fixture-key", "APP_EMPTY": "", "RUN_COMMAND": "serve"}
			if names := r.URL.Query()["name"]; len(names) > 0 {
				filtered := map[string]string{}
				for _, name := range names {
					filtered[name] = values[name]
				}
				values = filtered
			}
			json.NewEncoder(w).Encode(values)
			return
		}
		if r.URL.Path == "/config" {
			var patch struct {
				Values map[string]string
				Unset  []string
			}
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Error(err)
			}
			if len(patch.Values) > 0 && (patch.Values["APP_KEY"] != "value=with=equals" || patch.Values["APP_EMPTY"] != "" || len(patch.Values) != 2) {
				t.Errorf("wrong patch: %+v", patch)
			}
			if len(patch.Unset) > 0 && strings.Join(patch.Unset, ",") != "APP_KEY,APP_EMPTY" {
				t.Errorf("wrong unset: %+v", patch)
			}
		}
		json.NewEncoder(w).Encode(supervisor.Status{State: "waiting", ConfigPending: true})
	})}
	go server.Serve(listener)
	defer server.Close()
	var out bytes.Buffer
	if err := cli([]string{"config", "show"}, strings.NewReader(""), &out); err != nil || out.String() != "APP_EMPTY=\nAPP_KEY=fixture-key\nRUN_COMMAND=serve\n" {
		t.Fatalf("show all: %q %v", out.String(), err)
	}
	if request := <-requests; request != "GET /config" {
		t.Fatal(request)
	}
	out.Reset()
	if err := cli([]string{"config", "show", "RUN_COMMAND", "APP_EMPTY"}, strings.NewReader(""), &out); err != nil || out.String() != "APP_EMPTY=\nRUN_COMMAND=serve\n" {
		t.Fatalf("show selected: %q %v", out.String(), err)
	}
	<-requests
	for _, args := range [][]string{{"config", "set", "APP_KEY=value=with=equals", "APP_EMPTY="}, {"config", "unset", "APP_KEY", "APP_EMPTY"}, {"config", "apply"}} {
		out.Reset()
		if err := cli(args, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		want := "POST /config"
		if args[1] == "apply" {
			want = "POST /apply"
		}
		if got := <-requests; got != want {
			t.Fatalf("request=%q want=%q", got, want)
		}
		if args[1] == "set" && out.String() != "APP_EMPTY=\nAPP_KEY=value=with=equals\n" {
			t.Fatalf("set output cannot be copied: %q", out.String())
		}
	}
	for _, args := range [][]string{{"config"}, {"config", "bad"}, {"config", "set"}, {"config", "set", "APP_KEY"}, {"config", "set", "APP_KEY", "value=with=equals"}, {"config", "set", "APP_KEY=value=with=equals", "APP_EMPTY", ""}, {"config", "set", "APP_KEY=a", "APP_KEY=b"}, {"config", "unset"}, {"config", "apply", "extra"}} {
		if err := cli(args, strings.NewReader(""), io.Discard); err == nil {
			t.Fatalf("invalid CLI arguments accepted: %v", args)
		}
		select {
		case request := <-requests:
			t.Fatalf("invalid arguments sent a request: %v: %s", args, request)
		default:
		}
	}
}

func TestConfigTextRoundTrip(t *testing.T) {
	values := map[string]string{
		"APP_EMPTY": "", "APP_KEY": "nice-key", "APP_UNICODE": "héllo🌍",
		"APP_SPACES": "  keep these spaces  ", "APP_QUOTES": `literal "quotes" and C:\path`,
		"APP_LINES": "one\ntwo\r\nthree\ttab", "APP_LARGE": strings.Repeat("x", 70<<10),
		"APP_CONTROLS": "vertical\vform\fescape\x1b", "APP_SEPARATOR": "unicode\u2028separator",
		"RUN_COMMAND": `exec ./bin/gateway -env ''`, "APP_LITERAL": `$HOME ${KEY} $(command) ` + "`command`",
	}
	base := maps.Clone(config.Defaults)
	maps.Copy(base, values)
	base["DATA_DIR"], base["CONFIG_DIR"], base["LOG_DIR"] = t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("DATA_DIR", base["DATA_DIR"])
	store := state.Store{Data: base["DATA_DIR"], ConfigDir: base["CONFIG_DIR"]}
	settings, _, err := config.Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(base, false)
	if err != nil {
		t.Fatal(err)
	}
	c := &supervisor.Controller{Cfg: cfg, Settings: settings, Engine: &supervisor.Engine{Cfg: cfg, Proxy: proxy.New()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closeControl, err := c.StartControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	var exported bytes.Buffer
	if err := cli([]string{"config", "show"}, strings.NewReader(""), &exported); err != nil {
		t.Fatal(err)
	}
	if strings.Count(exported.String(), "\n") != len(base) || !strings.Contains(exported.String(), "APP_KEY=nice-key\n") || !strings.Contains(exported.String(), "RUN_COMMAND=\"exec ./bin/gateway -env ''\"\n") {
		t.Fatal("show is not one readable assignment per line")
	}
	if err := settings.Stage(map[string]string{"APP_KEY": "different", "RUN_COMMAND": "different"}, nil); err != nil {
		t.Fatal(err)
	}
	input := "\r\n" + strings.ReplaceAll(exported.String(), "\n", "\r\n") + "\r\n"
	var imported bytes.Buffer
	if err := cli([]string{"config", "set"}, strings.NewReader(input), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.String() != exported.String() || !maps.Equal(settings.Values(true), base) {
		t.Fatal("copy/paste changed a value or the output format")
	}
	if !maps.Equal(settings.Values(false), base) {
		t.Fatal("import activated settings")
	}
	reopened, _, err := config.Open(base, store)
	if err != nil || !maps.Equal(reopened.Values(true), base) {
		t.Fatalf("import did not persist: %v", err)
	}
	before := settings.Pending()
	for _, bad := range []io.Reader{
		strings.NewReader("APP_KEY=changed\nmissing-equals\n"),
		strings.NewReader("APP_KEY=changed\nAPP_KEY=duplicate\n"),
		strings.NewReader("APP_KEY=changed\nAPP_QUOTES=\"unfinished\n"),
		strings.NewReader("APP_KEY=changed\nAPP_QUOTES=\"value\"trailing\n"),
		strings.NewReader(strings.Repeat("x", (1<<20)+1)),
		iotest.ErrReader(errors.New("fixture read failure")),
	} {
		var out bytes.Buffer
		if err := cli([]string{"config", "set"}, bad, &out); err == nil {
			t.Fatal("invalid input accepted")
		}
		if out.Len() != 0 || !maps.Equal(settings.Pending(), before) {
			t.Fatal("failed input printed output or partially saved a batch")
		}
	}
}
