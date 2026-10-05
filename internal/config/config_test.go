package config

import (
	"path/filepath"
	"testing"
)

func validEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_URL", "https://github.com/acme/api.git")
	t.Setenv("RUNTIME_IMAGE", "alpine@sha256:abc")
	t.Setenv("RUN_COMMAND", "serve")
}

func TestDefaultRuntimeImage(t *testing.T) {
	validEnvironment(t)
	t.Setenv("RUNTIME_IMAGE", "")
	c, err := Load()
	if err != nil || c.RuntimeImage != "debian:bookworm-slim" {
		t.Fatalf("default runtime image=%q err=%v", c.RuntimeImage, err)
	}
	t.Setenv("RUNTIME_IMAGE", "node:22-bookworm-slim")
	c, err = Load()
	if err != nil || c.RuntimeImage != "node:22-bookworm-slim" {
		t.Fatalf("shorthand runtime image=%q err=%v", c.RuntimeImage, err)
	}
}

func TestLoadAndEnvironmentIsolation(t *testing.T) {
	t.Setenv("GIT_URL", "https://github.com/acme/api.git")
	t.Setenv("RUNTIME_IMAGE", "alpine@sha256:abc")
	t.Setenv("RUN_COMMAND", "serve")
	t.Setenv("APP_SECRET", "workload")
	t.Setenv("GIT_TOKEN", "supervisor")
	t.Setenv("UNRELATED", "no")
	c, e := Load()
	if e != nil {
		t.Fatal(e)
	}
	got := map[string]bool{}
	for _, v := range c.AppEnv {
		got[v] = true
	}
	if !got["SECRET=workload"] || got["GIT_TOKEN=supervisor"] || got["UNRELATED=no"] {
		t.Fatalf("bad app env: %v", c.AppEnv)
	}
}

func TestServiceMemoryLimitSemantics(t *testing.T) {
	validEnvironment(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ServiceMemoryLimit != 512<<20 {
		t.Fatalf("default memory limit=%d", c.ServiceMemoryLimit)
	}
	for _, value := range []string{"", "0"} {
		t.Run("disabled-"+value, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv("SERVICE_MEMORY_LIMIT", value)
			c, err := Load()
			if err != nil || c.ServiceMemoryLimit != 0 {
				t.Fatalf("limit=%d err=%v", c.ServiceMemoryLimit, err)
			}
		})
	}
	t.Setenv("SERVICE_MEMORY_LIMIT", "2G")
	c, err = Load()
	if err != nil || c.ServiceMemoryLimit != 2<<30 {
		t.Fatalf("limit=%d err=%v", c.ServiceMemoryLimit, err)
	}
	t.Setenv("SERVICE_MEMORY_LIMIT", "-1")
	if _, err = Load(); err == nil {
		t.Fatal("negative memory limit accepted")
	}
}
func TestInvalid(t *testing.T) {
	for _, tc := range []struct{ k, v string }{{"GIT_URL", "http://github.com/a/b"}, {"GIT_URL", "https://x:y@github.com/a/b"}} {
		validEnvironment(t)
		t.Setenv("RUNTIME_IMAGE", "x")
		t.Setenv("RUN_COMMAND", "x")
		t.Setenv(tc.k, tc.v)
		if _, e := Load(); e == nil {
			t.Fatalf("accepted %s", tc.v)
		}
	}
}

func TestLocalRepositoryConfiguration(t *testing.T) {
	validEnvironment(t)
	path := t.TempDir()
	t.Setenv("GIT_URL", path)
	c, err := Load()
	if err != nil || c.GitURL != path {
		t.Fatalf("local path rejected: url=%q err=%v", c.GitURL, err)
	}
	t.Setenv("UPDATE_METHOD", "release")
	if _, err := Load(); err == nil {
		t.Fatal("local repository accepted GitHub release mode")
	}
	t.Setenv("UPDATE_METHOD", "commit")
	t.Setenv("GIT_URL", filepath.Join(path, "missing"))
	if _, err := Load(); err == nil {
		t.Fatal("missing local repository accepted")
	}
}

func TestDurationMustFitPositiveNanoseconds(t *testing.T) {
	validEnvironment(t)
	for _, value := range []string{"0.0000000001", "9223372036.854776", "NaN", "Inf", "-1"} {
		t.Setenv("POLL_INTERVAL", value)
		if _, err := Load(); err == nil {
			t.Fatalf("accepted duration that cannot drive a ticker: %q", value)
		}
	}
}
