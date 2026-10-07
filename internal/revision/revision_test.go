package revision

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/example/easy-service/internal/process"
)

func TestMatch(t *testing.T) {
	for _, x := range []struct {
		p, n string
		w    bool
	}{{"v1.*", "v1.2", true}, {"v1.?", "v1.22", false}, {"*", "x", true}, {"a", "ba", false}} {
		if Match(x.p, x.n) != x.w {
			t.Fatal(x)
		}
	}
}
func TestPaginatedReleaseSelection(t *testing.T) {
	var srv *httptest.Server
	calls := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Link", "<https://api.github.com/repos/a/b/releases?per_page=100&page=2>; rel=\"next\"")
			fmt.Fprint(w, `[{"tag_name":"v1","published_at":"2024-01-01T00:00:00Z"}]`)
		} else {
			fmt.Fprint(w, `[{"tag_name":"v2","published_at":"2025-01-01T00:00:00Z"},{"tag_name":"v3","published_at":"2026-01-01T00:00:00Z","prerelease":true}]`)
		}
	}))
	defer srv.Close()
	m := &Manager{URL: "https://github.com/a/b.git", Client: srv.Client(), Mirror: t.TempDir()}
	m.Run = func(_ context.Context, _ string, a ...string) (string, error) {
		if a[0] == "rev-parse" {
			if a[1] != "refs/tags/v2^{commit}" {
				t.Fatal("wrong published release selected:", a)
			}
			return strings.Repeat("a", 40), nil
		}
		return "", nil
	} // rewrite API through transport
	m.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		r.URL, _ = r.URL.Parse(srv.URL)
		return http.DefaultTransport.RoundTrip(r)
	})
	s, e := m.release(context.Background(), "v*")
	if e != nil || s.SHA != strings.Repeat("a", 40) {
		t.Fatalf("%+v %v", s, e)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCredentialOnlyInMinimalChildEnvironment(t *testing.T) {
	m := &Manager{Token: "secret"}
	cmd := exec.Command("git", "fetch", "https://github.com/a/b")
	m.gitEnv(cmd)
	if strings.Contains(strings.Join(cmd.Args, " "), "secret") {
		t.Fatal("credential in command arguments")
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "GIT_CONFIG_VALUE_0=Authorization: Basic ") || strings.Contains(env, "UNRELATED=") {
		t.Fatalf("bad isolated env: %s", env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	read := process.Command(ctx, "git", "config", "--get", "http.extraHeader")
	m.gitEnv(read)
	got, err := process.Output(read)
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:secret"))
	if err != nil || strings.TrimSpace(got) != want {
		t.Fatalf("native Git did not receive scoped authentication: %v", err)
	}
}

func TestLocalTrustDoesNotForwardTokenOrTrustOtherRepositories(t *testing.T) {
	source := t.TempDir()
	m := New(source, "supervisor-only-test-token", t.TempDir())
	cmd := exec.Command("git", "fetch")
	m.gitEnv(cmd)
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "Authorization") || strings.Contains(env, "supervisor-only-test-token") {
		t.Fatal("local Git unnecessarily received supervisor authentication")
	}
	if strings.Contains(env, "safe.directory") {
		t.Fatal("local trust leaked beyond the configured upload-pack operation")
	}
}

func TestCommitTagAndExactCleanCheckout(t *testing.T) {
	root := t.TempDir()
	origin, work := filepath.Join(root, "origin.git"), filepath.Join(root, "work")
	gitTest(t, "", "init", "--bare", origin)
	gitTest(t, "", "init", work)
	gitTest(t, work, "config", "user.email", "test@example.invalid")
	gitTest(t, work, "config", "user.name", "Test")
	if e := os.WriteFile(filepath.Join(work, "version"), []byte("one"), 0600); e != nil {
		t.Fatal(e)
	}
	gitTest(t, work, "add", "version")
	gitTest(t, work, "commit", "-m", "one")
	gitTest(t, work, "tag", "v1-light")
	if e := os.WriteFile(filepath.Join(work, "version"), []byte("two"), 0600); e != nil {
		t.Fatal(e)
	}
	gitTest(t, work, "commit", "-am", "two")
	gitTest(t, work, "tag", "-a", "v2-annotated", "-m", "two")
	gitTest(t, work, "tag", "-a", "v3-blob", "-m", "not a commit", "HEAD:version")
	gitTest(t, work, "branch", "-M", "main")
	gitTest(t, work, "remote", "add", "origin", origin)
	gitTest(t, work, "push", "origin", "main", "--tags")
	m := New(origin, "", filepath.Join(root, "data"))
	commit, e := m.Select(context.Background(), "commit", "main", "*")
	if e != nil || len(commit.SHA) != 40 {
		t.Fatalf("commit=%+v err=%v", commit, e)
	}
	resolveCalls := 0
	run := m.Run
	m.Run = func(ctx context.Context, dir string, args ...string) (string, error) {
		if args[0] == "rev-parse" {
			resolveCalls++
		}
		return run(ctx, dir, args...)
	}
	tag, e := m.Select(context.Background(), "tag", "", "v*")
	if e != nil || tag.SHA != commit.SHA {
		t.Fatalf("tag=%+v err=%v", tag, e)
	}
	if resolveCalls != 2 {
		t.Fatalf("tag selection resolved %d objects instead of skipping the invalid newest and resolving the winner", resolveCalls)
	}
	dst := filepath.Join(root, "checkout")
	if e = m.Checkout(context.Background(), commit.SHA, dst); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(e) {
		t.Fatal("checkout retained Git metadata")
	}
	b, e := os.ReadFile(filepath.Join(dst, "version"))
	if e != nil || string(b) != "two" {
		t.Fatalf("content=%q err=%v", b, e)
	}
}

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	(&Manager{}).gitEnv(c)
	c.Env = append(c.Env, "GIT_AUTHOR_DATE=2025-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2025-01-01T00:00:00Z")
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git %v: %v: %s", args, e, b)
	}
}

func localRepository(t *testing.T, version string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repository with spaces")
	gitTest(t, "", "init", "-b", "main", dir)
	gitTest(t, dir, "config", "user.email", "test@example.invalid")
	gitTest(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "version")
	gitTest(t, dir, "commit", "-m", version)
	return dir
}

func TestMirrorRecoversInterruptedInitializationAndSourceChange(t *testing.T) {
	data := t.TempDir()
	a, b := localRepository(t, "a"), localRepository(t, "b")
	m := New(a, "", data)
	if err := os.MkdirAll(m.Mirror, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := m.Select(context.Background(), "commit", "main", "*")
	if err != nil {
		t.Fatalf("interrupted empty mirror did not recover: %v", err)
	}
	m = New(b, "", data)
	second, err := m.Select(context.Background(), "commit", "main", "*")
	if err != nil || second.SHA == first.SHA {
		t.Fatalf("source change kept old origin: first=%s second=%s err=%v", first.SHA, second.SHA, err)
	}
	if err := os.WriteFile(filepath.Join(b, "version"), []byte("uncommitted"), 0600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "checkout")
	if err := m.Checkout(context.Background(), second.SHA, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "version"))
	if err != nil || string(got) != "b" {
		t.Fatalf("local checkout included uncommitted content: %q %v", got, err)
	}
}

func TestCheckoutRejectsMalformedExactSHA(t *testing.T) {
	m := New("", "", t.TempDir())
	m.Run = func(context.Context, string, ...string) (string, error) {
		t.Fatal("malformed SHA reached git")
		return "", nil
	}
	if err := m.Checkout(context.Background(), strings.Repeat("z", 40), t.TempDir()); err == nil {
		t.Fatal("non-hexadecimal SHA accepted")
	}
}

func TestReleaseRejectsUnsafePagination(t *testing.T) {
	for _, next := range []string{"://[", "https://untrusted.invalid/steal", "https://api.github.com/repos/a/b/releases?per_page=100"} {
		t.Run(next, func(t *testing.T) {
			defer func() {
				if err := recover(); err != nil {
					t.Fatalf("pagination panicked: %v", err)
				}
			}()
			calls := 0
			m := New("https://github.com/a/b", "test-only-token", t.TempDir())
			m.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				header := make(http.Header)
				if calls == 1 {
					header.Set("Link", "<"+next+">; rel=\"next\"")
				}
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader("[]"))}, nil
			})
			if _, err := m.release(context.Background(), "*"); err == nil {
				t.Fatal("unsafe pagination accepted")
			}
			if calls != 1 {
				t.Fatalf("unsafe/repeated pagination sent %d HTTP requests", calls)
			}
		})
	}
}

func TestLocalRepositoryAcrossUIDs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("cross-UID mount check requires root to create an unprivileged test process")
	}
	public, err := os.MkdirTemp("", "easy-service-local-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(public) })
	work, bare, linked := filepath.Join(public, "work ' $(touch escaped)"), filepath.Join(public, "bare.git"), filepath.Join(public, "linked-worktree")
	gitTest(t, "", "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "version"), []byte("committed"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, work, "add", "version")
	gitTest(t, work, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "committed")
	gitTest(t, "", "clone", "--bare", work, bare)
	gitTest(t, work, "worktree", "add", "-b", "linked", linked)
	// Model readable host repositories mounted into a different container UID.
	if err := filepath.Walk(public, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if info.IsDir() {
			mode = 0755
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(public, "test-helper")
	if err := os.Link(exe, helper); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{work, bare, linked} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := process.Command(ctx, helper, "-test.run=^TestLocalRepositoryForeignUIDHelper$", "-test.v")
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 65534, Gid: 65534}
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "EASY_SERVICE_LOCAL_SOURCE=" + source}
		out, err := process.Output(cmd)
		cancel()
		if err != nil {
			t.Fatalf("foreign-UID local source %s: %v: %s", source, err, out)
		}
	}
}

func TestLocalRepositoryForeignUIDHelper(t *testing.T) {
	source := os.Getenv("EASY_SERVICE_LOCAL_SOURCE")
	if source == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := New(source, "", t.TempDir())
	sel, err := m.Select(ctx, "commit", "main", "*")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "checkout")
	if err := m.Checkout(ctx, sel.SHA, dst); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(dst, "version"))
	if err != nil || string(contents) != "committed" {
		t.Fatalf("foreign-UID checkout: %q %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatal("checkout retained Git metadata")
	}
}

func TestMirrorSurvivesSupervisorUIDChange(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to change cache ownership")
	}
	source := localRepository(t, "cache")
	m := New(source, "", t.TempDir())
	before, err := m.Select(context.Background(), "commit", "main", "*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(m.Mirror, 10000, 10000); err != nil {
		t.Fatal(err)
	}
	after, err := m.Select(context.Background(), "commit", "main", "*")
	if err != nil || before != after {
		t.Fatalf("persisted mirror rejected after UID change: %v %v", after, err)
	}
	dst := filepath.Join(t.TempDir(), "checkout")
	if err := m.Checkout(context.Background(), after.SHA, dst); err != nil {
		t.Fatal("persisted mirror checkout rejected after UID change:", err)
	}
	if contents, err := os.ReadFile(filepath.Join(dst, "version")); err != nil || string(contents) != "cache" {
		t.Fatalf("persisted mirror checkout: %q %v", contents, err)
	}
}
