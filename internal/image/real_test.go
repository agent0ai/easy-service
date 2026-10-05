package image

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/easy-service/internal/process"
)

var realOCI = flag.Bool("real-oci", false, "test actual OCI pull and unpack as an unprivileged user")

func TestRealRootlessOCI(t *testing.T) {
	if !*realOCI {
		t.Skip("enable with -args -real-oci")
	}
	if os.Geteuid() == 0 {
		t.Fatal("run this test as an unprivileged user")
	}
	for _, command := range []string{"skopeo", "umoci"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// A real multi-platform index: the copied architecture manifest has a
	// different digest, and rootless unpack must preserve a usable filesystem.
	ref := "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	m := Manager{Data: t.TempDir(), Runner: process.Runner{}}
	p, err := m.Prepare(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(p.Rootfs, "etc", "alpine-release")); err != nil {
		t.Fatal(err)
	}
	cached, err := m.Cached(ref, p.Digest)
	if err != nil || cached != p {
		t.Fatalf("cache mismatch: %+v %v", cached, err)
	}
}
