package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fake struct{ calls []string }

var sourceDigest = "sha256:" + strings.Repeat("a", 64)
var copiedDigest = "sha256:" + strings.Repeat("b", 64)

func (f *fake) Run(_ context.Context, _ time.Duration, n string, a ...string) (string, error) {
	f.calls = append(f.calls, n+" "+strings.Join(a, " "))
	if n == "skopeo" && a[0] == "inspect" {
		if strings.HasPrefix(a[len(a)-1], "oci:") {
			return copiedDigest + "\n", nil
		}
		return sourceDigest + "\n", nil
	}
	if n == "skopeo" && a[0] == "copy" {
		for i, arg := range a {
			if arg == "--digestfile" {
				return "", os.WriteFile(a[i+1], []byte(copiedDigest), 0600)
			}
		}
		return "", fmt.Errorf("copy must record its result digest")
	}
	if n == "umoci" {
		if !strings.Contains(strings.Join(a, " "), "--rootless") {
			return "", fmt.Errorf("rootless unpack required")
		}
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
	if p.Digest != sourceDigest || len(f.calls) != 4 {
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
	if !strings.Contains(f.calls[1], "docker://example/app@"+sourceDigest) {
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

type runnerFunc func(context.Context, time.Duration, string, ...string) (string, error)

func (f runnerFunc) Run(ctx context.Context, limit time.Duration, n string, a ...string) (string, error) {
	return f(ctx, limit, n, a...)
}

func TestCopiedDigestMismatchAndUnpackFailureRemovePartialCache(t *testing.T) {
	for _, phase := range []string{"digest", "unpack"} {
		t.Run(phase, func(t *testing.T) {
			base := &fake{}
			m := Manager{Data: t.TempDir(), Runner: runnerFunc(func(ctx context.Context, limit time.Duration, n string, a ...string) (string, error) {
				if phase == "digest" && n == "skopeo" && a[0] == "inspect" && strings.HasPrefix(a[len(a)-1], "oci:") {
					return "sha256:" + strings.Repeat("c", 64), nil
				}
				if phase == "unpack" && n == "umoci" {
					return "", errors.New("unpack failed")
				}
				return base.Run(ctx, limit, n, a...)
			})}
			if _, err := m.Prepare(context.Background(), "registry.invalid:5000/app:tag"); err == nil {
				t.Fatal("broken cache accepted")
			}
			entries, err := os.ReadDir(filepath.Join(m.Data, "images"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("partial cache retained: %d %v", len(entries), err)
			}
			if !strings.Contains(base.calls[1], "docker://registry.invalid:5000/app@"+sourceDigest) {
				t.Fatal("digest pin damaged registry port or retained image tag")
			}
		})
	}
}

func TestCachedRootfsMustBelongToItsImage(t *testing.T) {
	m := Manager{Data: t.TempDir(), Runner: &fake{}}
	p, err := m.Prepare(context.Background(), "example/app:tag")
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(filepath.Dir(filepath.Dir(p.Rootfs)), "ready.json")
	p.Rootfs = t.TempDir()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cached("example/app:tag", p.Digest); err == nil {
		t.Fatal("unrelated rootfs accepted from cache metadata")
	}
}
