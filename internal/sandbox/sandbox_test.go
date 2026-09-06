package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateWritableCopies(t *testing.T) {
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, "x"), []byte("base"), 0600)
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	if e := CopyTree(context.Background(), base, a); e != nil {
		t.Fatal(e)
	}
	if e := CopyTree(context.Background(), base, b); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(a, "x"), []byte("changed"), 0600)
	got, _ := os.ReadFile(filepath.Join(b, "x"))
	if string(got) != "base" {
		t.Fatal("instances share writable data")
	}
}

func TestStrictWorkloadEnvironment(t *testing.T) {
	got := workloadEnv([]string{"SECRET=app", "PORT=wrong", "PATH=wrong", "GIT_TOKEN=explicit-app-value"}, 8080)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "SECRET=app") || !strings.Contains(joined, "PORT=8080") || strings.Contains(joined, "PORT=wrong") || strings.Contains(joined, "PATH=wrong") {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(joined, "GIT_TOKEN=explicit-app-value") {
		t.Fatal("an explicitly provided APP_GIT_TOKEN must forward as GIT_TOKEN")
	}
}

func TestMissingIsolationPrerequisitesFailClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if e := Validate(); e == nil || !strings.Contains(e.Error(), "mandatory rootless gVisor prerequisite") {
		t.Fatalf("unexpected validation result: %v", e)
	}
}

func TestPreparedValidationRequiresCompleteInstallation(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, ".easy-service-ready"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (Runtime{}).ValidatePrepared(d); err == nil {
		t.Fatal("marker-only prepared installation was accepted")
	}
	if err := os.MkdirAll(filepath.Join(d, "rootfs", "app"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := (Runtime{}).ValidatePrepared(d); err != nil {
		t.Fatalf("complete prepared installation rejected: %v", err)
	}
}

func TestProcessTreeMemorySampling(t *testing.T) {
	used, err := processTreeRSS(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if used == 0 {
		t.Fatal("running process tree reported no resident memory")
	}
}
