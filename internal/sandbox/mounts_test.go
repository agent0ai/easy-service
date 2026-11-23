package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example/easy-service/internal/config"
)

func TestMountSourceAndDestinationValidation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("setting the shared group requires root")
	}
	root, source := t.TempDir(), t.TempDir()
	mounts := []config.Mount{{Source: source, Destination: "/persist/db"}}
	if _, err := launchMounts(root, mounts); err == nil {
		t.Fatal("accepted a source with incorrect group permissions")
	}
	if err := os.Chown(source, 0, dataGroup); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, os.ModeSetgid|0770); err != nil {
		t.Fatal(err)
	}
	if bindings, err := launchMounts(root, mounts); err != nil || len(bindings) != 1 || bindings[0].Inode == 0 {
		t.Fatalf("valid directory failed: %v, %v", bindings, err)
	}
	if err := os.Symlink(source, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := mountSource(filepath.Join(root, "alias")); err == nil {
		t.Fatal("accepted a source symlink")
	}
	mounts[0].Destination = "/alias/db"
	if _, err := launchMounts(root, mounts); err == nil {
		t.Fatal("accepted a symlink in the destination path")
	}
	cfg := config.Config{DataDir: filepath.Join(root, "alias"), SandboxMounts: mounts}
	if err := ValidateMounts(cfg); err == nil {
		t.Fatal("accepted a source aliased by a protected directory")
	}
}
