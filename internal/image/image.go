package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Runner interface {
	Run(context.Context, time.Duration, string, ...string) (string, error)
}
type Manager struct {
	Data   string
	Runner Runner
}
type Prepared struct{ Digest, Rootfs string }

func (m Manager) Cached(ref, digest string) (Prepared, error) {
	h := sha256.Sum256([]byte(ref + "\x00" + digest + "\x00" + runtime.GOARCH))
	dir := filepath.Join(m.Data, "images", hex.EncodeToString(h[:12]))
	b, e := os.ReadFile(filepath.Join(dir, "ready.json"))
	if e != nil {
		return Prepared{}, e
	}
	var p Prepared
	if e = json.Unmarshal(b, &p); e != nil || p.Digest != digest {
		return Prepared{}, fmt.Errorf("cached image metadata mismatch")
	}
	if st, e := os.Stat(p.Rootfs); e != nil || !st.IsDir() {
		return Prepared{}, fmt.Errorf("cached image rootfs unavailable")
	}
	return p, nil
}

func (m Manager) Prepare(ctx context.Context, ref string) (Prepared, error) {
	digest, e := m.Runner.Run(ctx, 10*time.Minute, "skopeo", "inspect", "--override-arch", runtime.GOARCH, "--format", "{{.Digest}}", "docker://"+ref)
	if e != nil {
		return Prepared{}, fmt.Errorf("resolve runtime image: %w", e)
	}
	digest = strings.TrimSpace(digest)
	if !strings.HasPrefix(digest, "sha256:") {
		return Prepared{}, fmt.Errorf("registry returned invalid digest")
	}
	if at := strings.LastIndex(ref, "@sha256:"); at >= 0 && ref[at+1:] != digest {
		return Prepared{}, fmt.Errorf("resolved digest does not match pinned RUNTIME_IMAGE")
	}
	h := sha256.Sum256([]byte(ref + "\x00" + digest + "\x00" + runtime.GOARCH))
	dir := filepath.Join(m.Data, "images", hex.EncodeToString(h[:12]))
	root := filepath.Join(dir, "bundle", "rootfs")
	meta := filepath.Join(dir, "ready.json")
	if b, e := os.ReadFile(meta); e == nil {
		var p Prepared
		if json.Unmarshal(b, &p) == nil && p.Digest == digest {
			if st, e := os.Stat(p.Rootfs); e == nil && st.IsDir() {
				return p, nil
			}
		}
	}
	if e := os.RemoveAll(dir); e != nil {
		return Prepared{}, e
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return Prepared{}, e
	}
	layout := filepath.Join(dir, "oci")
	bundle := filepath.Join(dir, "bundle")
	immutable := ref
	if at := strings.LastIndexByte(immutable, '@'); at >= 0 {
		immutable = immutable[:at]
	}
	immutable += "@" + digest
	if _, e = m.Runner.Run(ctx, 10*time.Minute, "skopeo", "copy", "--override-arch", runtime.GOARCH, "docker://"+immutable, "oci:"+layout+":runtime"); e != nil {
		os.RemoveAll(dir)
		return Prepared{}, fmt.Errorf("pull runtime image: %w", e)
	}
	copied, inspectErr := m.Runner.Run(ctx, 10*time.Minute, "skopeo", "inspect", "--format", "{{.Digest}}", "oci:"+layout+":runtime")
	if inspectErr != nil || strings.TrimSpace(copied) != digest {
		os.RemoveAll(dir)
		return Prepared{}, fmt.Errorf("copied runtime image digest verification failed")
	}
	if _, e = m.Runner.Run(ctx, 10*time.Minute, "umoci", "unpack", "--image", layout+":runtime", bundle); e != nil {
		os.RemoveAll(dir)
		return Prepared{}, fmt.Errorf("unpack runtime image: %w", e)
	}
	p := Prepared{digest, root}
	b, _ := json.Marshal(p)
	if e := atomicWrite(meta, b); e != nil {
		return Prepared{}, e
	}
	return p, nil
}
func atomicWrite(path string, b []byte) error {
	tmp := path + ".tmp"
	if e := os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	if e := os.Rename(tmp, path); e != nil {
		return e
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	d, e := os.Open(path)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
