package config

import (
	"os"
	"testing"
)

func validEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_URL", "https://github.com/acme/api.git")
	t.Setenv("RUNTIME_IMAGE", "alpine@sha256:abc")
	t.Setenv("RUN_COMMAND", "serve")
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
		os.Clearenv()
		t.Setenv("RUNTIME_IMAGE", "x")
		t.Setenv("RUN_COMMAND", "x")
		t.Setenv(tc.k, tc.v)
		if _, e := Load(); e == nil {
			t.Fatalf("accepted %s", tc.v)
		}
	}
}
