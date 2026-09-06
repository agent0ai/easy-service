package revision

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
func TestTagTieAndAnnotatedResolution(t *testing.T) {
	m := &Manager{}
	m.Run = func(_ context.Context, _ string, a ...string) (string, error) {
		if a[0] == "fetch" {
			return "", nil
		}
		if a[0] == "for-each-ref" {
			return "v1\t10\tpeeled1\ttagobj\nv2\t10\t\tcommit2", nil
		}
		if a[len(a)-1] == "peeled1^{commit}" {
			return "111", nil
		}
		if a[len(a)-1] == "commit2^{commit}" {
			return "222", nil
		}
		return "", fmt.Errorf("bad %v", a)
	}
	m.Mirror = t.TempDir()
	s, e := m.tag(context.Background(), "v*")
	if e != nil || s.Name != "v2" || s.SHA != "222" {
		t.Fatalf("%+v %v", s, e)
	}
}
func TestPaginatedReleaseSelection(t *testing.T) {
	var srv *httptest.Server
	calls := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Link", "<"+srv.URL+">; rel=\"next\"")
			fmt.Fprint(w, `[{"tag_name":"v1","published_at":"2024-01-01T00:00:00Z"}]`)
		} else {
			fmt.Fprint(w, `[{"tag_name":"v2","published_at":"2025-01-01T00:00:00Z"},{"tag_name":"v3","published_at":"2026-01-01T00:00:00Z","prerelease":true}]`)
		}
	}))
	defer srv.Close()
	m := &Manager{URL: "https://github.com/a/b.git", Client: srv.Client(), Mirror: t.TempDir()}
	m.Run = func(_ context.Context, _ string, a ...string) (string, error) {
		if a[0] == "rev-parse" {
			return "sha2", nil
		}
		return "", nil
	} // rewrite API through transport
	m.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		r.URL, _ = r.URL.Parse(srv.URL)
		return http.DefaultTransport.RoundTrip(r)
	})
	s, e := m.release(context.Background(), "v*")
	if e != nil || s.Name != "v2" || !s.Time.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
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
	gitTest(t, work, "branch", "-M", "main")
	gitTest(t, work, "remote", "add", "origin", origin)
	gitTest(t, work, "push", "origin", "main", "--tags")
	m := New(origin, "", filepath.Join(root, "data"))
	commit, e := m.Select(context.Background(), "commit", "main", "*")
	if e != nil || len(commit.SHA) != 40 {
		t.Fatalf("commit=%+v err=%v", commit, e)
	}
	tag, e := m.Select(context.Background(), "tag", "", "v2-*")
	if e != nil || tag.Name != "v2-annotated" || tag.SHA != commit.SHA {
		t.Fatalf("tag=%+v err=%v", tag, e)
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
	c.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2025-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2025-01-01T00:00:00Z")
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git %v: %v: %s", args, e, b)
	}
}
