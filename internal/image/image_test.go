package image

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fake struct{ calls []string }

func (f *fake) Run(_ context.Context, _ time.Duration, n string, a ...string) (string, error) {
	f.calls = append(f.calls, n+" "+strings.Join(a, " "))
	if n == "skopeo" && a[0] == "inspect" {
		return "sha256:abc\n", nil
	}
	if n == "umoci" {
		for _, x := range a {
			if strings.HasSuffix(x, "/bundle") {
				os.MkdirAll(filepath.Join(x, "rootfs"), 0700)
			}
		}
	}
	return "", nil
}
func TestDigestArchitectureCacheAndPreparation(t *testing.T) {
	f := &fake{}
	m := Manager{t.TempDir(), f}
	p, e := m.Prepare(context.Background(), "example/app:latest")
	if e != nil {
		t.Fatal(e)
	}
	if p.Digest != "sha256:abc" || len(f.calls) != 4 {
		t.Fatalf("%+v %v", p, f.calls)
	}
	if _, e = m.Prepare(context.Background(), "example/app:latest"); e != nil {
		t.Fatal(e)
	}
	if len(f.calls) != 5 {
		t.Fatalf("cache missed: %v", f.calls)
	}
	if !strings.Contains(f.calls[0], "--override-arch") {
		t.Fatal("architecture not selected")
	}
	if !strings.Contains(f.calls[1], "docker://example/app:latest@sha256:abc") {
		t.Fatalf("copy did not use immutable digest: %v", f.calls)
	}
}

func TestPinnedDigestMismatchFailsBeforePull(t *testing.T) {
	f := &fake{}
	m := Manager{t.TempDir(), f}
	if _, e := m.Prepare(context.Background(), "example/app@sha256:different"); e == nil {
		t.Fatal("accepted mismatched pinned digest")
	}
	if len(f.calls) != 1 {
		t.Fatalf("image was pulled after mismatch: %v", f.calls)
	}
}
