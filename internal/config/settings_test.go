package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example/easy-service/internal/state"
)

func TestStagingAndAppliedOverridesSurviveRestart(t *testing.T) {
	base := Environment()
	base["DATA_DIR"], base["CONFIG_DIR"], base["LOG_DIR"] = t.TempDir(), t.TempDir(), t.TempDir()
	base["GIT_URL"], base["RUN_COMMAND"], base["APP_KEY"] = "https://github.com/acme/api", "serve", "docker-default"
	store := state.Store{Data: base["DATA_DIR"], ConfigDir: base["CONFIG_DIR"]}
	s, _, err := Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stage(map[string]string{"APP_KEY": "saved", "APP_EMPTY": "", "POLL_INTERVAL": "invalid"}, nil); err != nil {
		t.Fatal(err)
	}
	if s.Values(false)["APP_KEY"] != "docker-default" {
		t.Fatal("set activated pending settings")
	}
	if _, err := Parse(s.Values(true), true); err == nil {
		t.Fatal("invalid staged value passed apply validation")
	}
	s, _, err = Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	if s.Values(true)["APP_KEY"] != "saved" || s.Values(false)["APP_KEY"] != "docker-default" {
		t.Fatal("restart applied or lost pending settings")
	}
	if err := s.Stage(map[string]string{"POLL_INTERVAL": "5"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state.State{Revision: "healthy", Overrides: s.Pending()}); err != nil {
		t.Fatal(err)
	}
	s.Accept(s.Pending())
	base["APP_KEY"], base["HEALTH_FAILURES"] = "new-docker-default", "7"
	s, prior, err := Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	if prior.Revision != "healthy" || s.Values(false)["APP_KEY"] != "saved" || s.Values(false)["HEALTH_FAILURES"] != "7" {
		t.Fatal("Docker defaults did not stay beneath saved overrides")
	}
	if err := store.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatal("runtime cleanup removed applied configuration:", err)
	}
	if err := s.Stage(nil, []string{"APP_KEY"}); err != nil {
		t.Fatal(err)
	}
	if s.Values(true)["APP_KEY"] != "new-docker-default" {
		t.Fatal("unset did not restore Docker default")
	}
	for _, file := range []string{"active.json", "pending.json"} {
		info, err := os.Stat(filepath.Join(base["CONFIG_DIR"], file))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("configuration permissions: %v %v", info, err)
		}
	}
	before := s.Values(true)["APP_KEY"]
	for _, patch := range []map[string]string{{"UNKNOWN": "x"}, {"APP_": "x"}, {"APP_BAD-NAME": "x"}, {"APP_KEY": "\x00"}, {"DATA_DIR": t.TempDir()}} {
		patch["APP_SENTINEL"] = "must-not-partially-save"
		if err := s.Stage(patch, nil); err == nil {
			t.Fatal("invalid batch accepted")
		}
		if s.Values(true)["APP_KEY"] != before || s.Values(true)["APP_SENTINEL"] != "" {
			t.Fatal("invalid batch partially changed configuration")
		}
	}
	if err := os.WriteFile(filepath.Join(base["CONFIG_DIR"], "pending.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(base, store); err == nil {
		t.Fatal("corrupt configuration silently reset")
	}
}

func TestLogPolicyAndBootstrapDefaults(t *testing.T) {
	c, err := Parse(nil, false)
	if err != nil || c.LogRetentionDays != 30 || c.LogMaxFileSize != 10<<20 || c.LogMaxTotalSize != 1<<30 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if _, err := Parse(nil, true); err == nil {
		t.Fatal("required application settings were optional on apply")
	}
	for name, value := range map[string]string{"LOG_RETENTION_DAYS": "999999999999", "LOG_MAX_FILE_SIZE": "1", "LOG_MAX_TOTAL_SIZE": "1M", "CONFIG_DIR": "/data/config", "LOG_DIR": "/"} {
		if _, err := Parse(map[string]string{name: value}, false); err == nil {
			t.Fatalf("invalid policy/path accepted: %s", name)
		}
	}
}
