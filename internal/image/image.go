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

	"github.com/example/easy-service/internal/state"
)

type Runner interface {
	Run(context.Context, time.Duration, string, ...string) (string, error)
}
type Manager struct {
	Data   string
	Runner Runner
}
type Prepared struct {
	Digest, Rootfs string
	Env            []string `json:"-"`
}

func (m Manager) Cached(ref, digest string) (Prepared, error) {
	if !validDigest(digest) {
		return Prepared{}, fmt.Errorf("cached image digest invalid")
	}
	h := sha256.Sum256([]byte(ref + "\x00" + digest + "\x00" + runtime.GOARCH))
	dir := filepath.Join(m.Data, "images", hex.EncodeToString(h[:12]))
	b, e := os.ReadFile(filepath.Join(dir, "ready.json"))
	if e != nil {
		return Prepared{}, e
	}
	var p Prepared
	if e = json.Unmarshal(b, &p); e != nil || p.Digest != digest || p.Rootfs != filepath.Join(dir, "bundle", "rootfs") {
		return Prepared{}, fmt.Errorf("cached image metadata mismatch")
	}
	if st, e := os.Stat(p.Rootfs); e != nil || !st.IsDir() {
		return Prepared{}, fmt.Errorf("cached image rootfs unavailable")
	}
	b, e = os.ReadFile(filepath.Join(dir, "bundle", "config.json"))
	if e != nil {
		return Prepared{}, fmt.Errorf("read runtime image configuration: %w", e)
	}
	var spec struct {
		Process struct{ Env []string }
	}
	if e = json.Unmarshal(b, &spec); e != nil {
		return Prepared{}, fmt.Errorf("parse runtime image configuration: %w", e)
	}
	p.Env = spec.Process.Env
	return p, nil
}

// Prune runs only after a usable deployment is selected or a failed update
// returns to the previously selected image, including offline recovery.
func (m Manager) Prune(keep Prepared) error {
	root := filepath.Join(m.Data, "images")
	dir := filepath.Dir(filepath.Dir(keep.Rootfs))
	if !state.Within(root, dir) || filepath.Dir(dir) != root || keep.Rootfs != filepath.Join(dir, "bundle", "rootfs") {
		return fmt.Errorf("unsafe image cache cleanup path")
	}
	if st, e := os.Stat(keep.Rootfs); e != nil || !st.IsDir() {
		return fmt.Errorf("selected image rootfs unavailable")
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if path != dir {
			if e := os.RemoveAll(path); e != nil {
				return e
			}
		}
	}
	for _, name := range []string{"oci", "copied-digest"} {
		if e := os.RemoveAll(filepath.Join(dir, name)); e != nil {
			return e
		}
	}
	return nil
}

func (m Manager) Prepare(ctx context.Context, ref string) (p Prepared, err error) {
	digest, e := m.Runner.Run(ctx, 10*time.Minute, "skopeo", "inspect", "--no-tags", "--override-os", "linux", "--override-arch", runtime.GOARCH, "--format", "{{.Digest}}", "docker://"+ref)
	if e != nil {
		return Prepared{}, fmt.Errorf("resolve runtime image: %w", e)
	}
	digest = strings.TrimSpace(digest)
	if !validDigest(digest) {
		return Prepared{}, fmt.Errorf("registry returned invalid digest")
	}
	if at := strings.LastIndex(ref, "@sha256:"); at >= 0 && ref[at+1:] != digest {
		return Prepared{}, fmt.Errorf("resolved digest does not match pinned RUNTIME_IMAGE")
	}
	h := sha256.Sum256([]byte(ref + "\x00" + digest + "\x00" + runtime.GOARCH))
	dir := filepath.Join(m.Data, "images", hex.EncodeToString(h[:12]))
	root := filepath.Join(dir, "bundle", "rootfs")
	meta := filepath.Join(dir, "ready.json")
	if p, e := m.Cached(ref, digest); e == nil {
		return p, nil
	}
	if e := os.RemoveAll(dir); e != nil {
		return Prepared{}, e
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return Prepared{}, e
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	layout := filepath.Join(dir, "oci")
	bundle := filepath.Join(dir, "bundle")
	immutable := ref
	if at := strings.LastIndexByte(immutable, '@'); at >= 0 {
		immutable = immutable[:at]
	}
	if at := strings.LastIndexByte(immutable, ':'); at > strings.LastIndexByte(immutable, '/') {
		immutable = immutable[:at] // preserve registry ports, remove image tags
	}
	immutable += "@" + digest
	copiedDigestFile := filepath.Join(dir, "copied-digest")
	if _, e = m.Runner.Run(ctx, 10*time.Minute, "skopeo", "copy", "--digestfile", copiedDigestFile, "--override-os", "linux", "--override-arch", runtime.GOARCH, "docker://"+immutable, "oci:"+layout+":runtime"); e != nil {
		return Prepared{}, fmt.Errorf("pull runtime image: %w", e)
	}
	copied, inspectErr := m.Runner.Run(ctx, 10*time.Minute, "skopeo", "inspect", "--format", "{{.Digest}}", "oci:"+layout+":runtime")
	// The source may be a multi-platform index or converted to OCI. Skopeo
	// records the copied manifest's digest; it need not equal the source index.
	expected, digestErr := os.ReadFile(copiedDigestFile)
	if inspectErr != nil || digestErr != nil || !validDigest(strings.TrimSpace(string(expected))) || strings.TrimSpace(copied) != strings.TrimSpace(string(expected)) {
		return Prepared{}, fmt.Errorf("copied runtime image digest verification failed")
	}
	if _, e = m.Runner.Run(ctx, 10*time.Minute, "umoci", "unpack", "--rootless", "--image", layout+":runtime", bundle); e != nil {
		return Prepared{}, fmt.Errorf("unpack runtime image: %w", e)
	}
	p = Prepared{Digest: digest, Rootfs: root}
	b, _ := json.Marshal(p)
	if e := state.AtomicWrite(meta, b); e != nil {
		return Prepared{}, e
	}
	return m.Cached(ref, digest)
}

func validDigest(digest string) bool {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return false
	}
	_, err := hex.DecodeString(digest[7:])
	return err == nil
}
