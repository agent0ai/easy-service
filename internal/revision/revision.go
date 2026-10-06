package revision

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/easy-service/internal/process"
	"github.com/example/easy-service/internal/state"
)

type Selection struct{ SHA string }

func ValidSHA(sha string) bool {
	if len(sha) != 40 || sha != strings.ToLower(sha) {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

type Manager struct {
	URL, Token, Mirror string
	Client             *http.Client
	Run                func(context.Context, string, ...string) (string, error)
}

func New(raw, token, data string) *Manager {
	identity := sha256.Sum256([]byte(raw))
	m := &Manager{URL: raw, Token: token, Mirror: filepath.Join(data, "git-mirrors", hex.EncodeToString(identity[:12])), Client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.Scheme != "https" || req.URL.Host != "api.github.com" {
			return fmt.Errorf("refusing unsafe GitHub API redirect")
		}
		return nil
	}}}
	m.Run = m.run
	return m
}
func (m *Manager) gitEnv(cmd *exec.Cmd) {
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1"}
	if !filepath.IsAbs(m.URL) && m.Token != "" {
		a := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + m.Token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+a)
	}
}

// Prune retires other repositories only after a successful deployment, while
// the controller owns selection and no old-source polling worker can use them.
func (m *Manager) Prune() error {
	root := filepath.Dir(m.Mirror)
	if filepath.Base(root) != "git-mirrors" || !state.Within(root, m.Mirror) {
		return fmt.Errorf("unsafe Git cache cleanup path")
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == filepath.Base(m.Mirror) || !entry.IsDir() || len(entry.Name()) != 24 {
			continue
		}
		if _, err := hex.DecodeString(entry.Name()); err != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
func (m *Manager) run(ctx context.Context, dir string, args ...string) (string, error) {
	c := process.Command(ctx, "git", args...)
	c.Dir = dir
	m.gitEnv(c)
	b, e := process.Output(c)
	if e != nil {
		if m.Token != "" {
			b = strings.ReplaceAll(b, m.Token, "[redacted]")
			b = strings.ReplaceAll(b, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+m.Token)), "[redacted]")
		}
		return "", fmt.Errorf("git operation failed: %w: %s", e, strings.TrimSpace(b))
	}
	return strings.TrimSpace(b), nil
}
func (m *Manager) Fetch(ctx context.Context) error {
	if _, e := os.Stat(filepath.Join(m.Mirror, "HEAD")); os.IsNotExist(e) {
		if e := os.MkdirAll(filepath.Dir(m.Mirror), 0700); e != nil {
			return e
		}
		if _, e = m.Run(ctx, "", "init", "--bare", m.Mirror); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	// Also repairs an interrupted remote setup and follows configuration changes.
	if _, e := m.Run(ctx, m.Mirror, "config", "remote.origin.url", m.URL); e != nil {
		return e
	}
	args := []string{"fetch", "--prune", "--force"}
	if filepath.IsAbs(m.URL) {
		// Git strips command-scope configuration before starting upload-pack.
		// Explicit local sources may be mounted from a different UID; pass
		// exact trust to that helper, never to unrelated repositories.
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
		args = append(args, "--upload-pack=git -c "+quote("safe.directory="+m.URL)+" -c "+quote("safe.directory="+filepath.Join(m.URL, ".git"))+" upload-pack")
	}
	args = append(args, "origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*")
	_, e := m.Run(ctx, m.Mirror, args...)
	return e
}
func (m *Manager) Select(ctx context.Context, method, branch, pattern string) (Selection, error) {
	op, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if e := m.Fetch(op); e != nil {
		return Selection{}, e
	}
	switch method {
	case "commit":
		s, e := m.Run(op, m.Mirror, "rev-parse", "refs/remotes/origin/"+branch+"^{commit}")
		return Selection{SHA: s}, e
	case "tag":
		return m.tag(op, pattern)
	case "release":
		return m.release(op, pattern)
	}
	return Selection{}, fmt.Errorf("unknown method")
}
func Match(pattern, name string) bool { // whole-string wildcard
	pi, ni, star, mark := 0, 0, -1, 0
	for ni < len(name) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == name[ni]) {
			pi++
			ni++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			mark = ni
			pi++
		} else if star >= 0 {
			pi = star + 1
			mark++
			ni = mark
		} else {
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
func (m *Manager) tag(ctx context.Context, p string) (Selection, error) {
	out, e := m.Run(ctx, m.Mirror, "for-each-ref", "--sort=-refname", "--sort=-creatordate", "--format=%(refname:strip=2)", "refs/tags")
	if e != nil {
		return Selection{}, e
	}
	for _, name := range strings.Split(out, "\n") {
		if name == "" || !Match(p, name) {
			continue
		}
		resolved, e := m.Run(ctx, m.Mirror, "rev-parse", "refs/tags/"+name+"^{commit}")
		if e != nil {
			continue
		}
		return Selection{SHA: resolved}, nil
	}
	return Selection{}, fmt.Errorf("no matching tags")
}

type release struct {
	Tag               string    `json:"tag_name"`
	Published         time.Time `json:"published_at"`
	Draft, Prerelease bool
}

func (m *Manager) release(ctx context.Context, p string) (Selection, error) {
	u, err := url.Parse(m.URL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return Selection{}, fmt.Errorf("release mode requires a GitHub repository")
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"), "/")
	if len(parts) != 2 {
		return Selection{}, fmt.Errorf("repository URL must identify owner/repo")
	}
	next := "https://api.github.com/repos/" + parts[0] + "/" + parts[1] + "/releases?per_page=100"
	var best *release
	seen := make(map[string]bool)
	for next != "" {
		if seen[next] {
			return Selection{}, fmt.Errorf("GitHub release pagination repeated a page")
		}
		seen[next] = true
		req, err := http.NewRequestWithContext(ctx, "GET", next, nil)
		if err != nil || req.URL.Scheme != "https" || req.URL.Host != "api.github.com" || req.URL.User != nil {
			return Selection{}, fmt.Errorf("invalid GitHub release pagination URL")
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if m.Token != "" {
			req.Header.Set("Authorization", "Bearer "+m.Token)
		}
		res, e := m.Client.Do(req)
		if e != nil {
			return Selection{}, e
		}
		if res.StatusCode/100 != 2 {
			res.Body.Close()
			return Selection{}, fmt.Errorf("GitHub releases: %s", res.Status)
		}
		var page []release
		e = json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&page)
		res.Body.Close()
		if e != nil {
			return Selection{}, e
		}
		for _, r := range page {
			if !r.Draft && !r.Prerelease && Match(p, r.Tag) && (best == nil || r.Published.After(best.Published) || (r.Published.Equal(best.Published) && r.Tag > best.Tag)) {
				candidate := r
				best = &candidate
			}
		}
		next = parseNext(res.Header.Get("Link"))
	}
	if best == nil {
		return Selection{}, fmt.Errorf("no matching published releases")
	}
	sha, e := m.Run(ctx, m.Mirror, "rev-parse", "refs/tags/"+best.Tag+"^{commit}")
	return Selection{SHA: sha}, e
}
func parseNext(h string) string {
	for _, p := range strings.Split(h, ",") {
		if strings.Contains(p, "rel=\"next\"") {
			a := strings.Index(p, "<")
			b := strings.Index(p, ">")
			if a >= 0 && b > a {
				return p[a+1 : b]
			}
		}
	}
	return ""
}
func (m *Manager) Checkout(ctx context.Context, sha, dst string) error {
	op, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !ValidSHA(sha) {
		return fmt.Errorf("refusing non-exact SHA")
	}
	if e := os.MkdirAll(dst, 0700); e != nil {
		return e
	}
	if _, e := m.Run(op, dst, "init"); e != nil {
		return e
	}
	if _, e := m.Run(op, dst, "fetch", "--depth=1", m.Mirror, sha); e != nil {
		return e
	}
	if _, e := m.Run(op, dst, "checkout", "--detach", sha); e != nil {
		return e
	}
	got, e := m.Run(op, dst, "rev-parse", "HEAD")
	if e != nil || got != sha {
		return fmt.Errorf("checkout SHA mismatch")
	}
	return os.RemoveAll(filepath.Join(dst, ".git"))
}
