package revision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Selection struct {
	SHA, Name string
	Time      time.Time
}
type Manager struct {
	URL, Token, Mirror string
	Client             *http.Client
	Run                func(context.Context, string, ...string) (string, error)
}

func New(raw, token, data string) *Manager {
	m := &Manager{URL: raw, Token: token, Mirror: filepath.Join(data, "git-mirror"), Client: &http.Client{Timeout: 30 * time.Second}}
	m.Run = m.run
	return m
}
func (m *Manager) gitEnv(cmd *exec.Cmd) {
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1"}
	if m.Token != "" {
		a := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + m.Token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+a)
	}
}
func (m *Manager) run(ctx context.Context, dir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	m.gitEnv(c)
	b, e := c.CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("git operation failed: %w: %s", e, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}
func (m *Manager) Fetch(ctx context.Context) error {
	if _, e := os.Stat(m.Mirror); os.IsNotExist(e) {
		if e := os.MkdirAll(filepath.Dir(m.Mirror), 0700); e != nil {
			return e
		}
		if _, e = m.Run(ctx, "", "init", "--bare", m.Mirror); e != nil {
			return e
		}
		if _, e = m.Run(ctx, m.Mirror, "remote", "add", "origin", m.URL); e != nil {
			return e
		}
	}
	_, e := m.Run(ctx, m.Mirror, "fetch", "--prune", "--force", "origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*")
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
		return Selection{SHA: s, Name: branch}, e
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
	out, e := m.Run(ctx, m.Mirror, "for-each-ref", "--format=%(refname:strip=2)%09%(creatordate:unix)%09%(*objectname)%09%(objectname)", "refs/tags")
	if e != nil {
		return Selection{}, e
	}
	var xs []Selection
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 4 || !Match(p, f[0]) {
			continue
		}
		ts, _ := strconv.ParseInt(f[1], 10, 64)
		sha := f[2]
		if sha == "" {
			sha = f[3]
		}
		resolved, e := m.Run(ctx, m.Mirror, "rev-parse", sha+"^{commit}")
		if e != nil {
			continue
		}
		xs = append(xs, Selection{resolved, f[0], time.Unix(ts, 0)})
	}
	if len(xs) == 0 {
		return Selection{}, fmt.Errorf("no matching tags")
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].Time.Equal(xs[j].Time) {
			return xs[i].Name > xs[j].Name
		}
		return xs[i].Time.After(xs[j].Time)
	})
	return xs[0], nil
}

type release struct {
	Tag               string    `json:"tag_name"`
	Published         time.Time `json:"published_at"`
	Draft, Prerelease bool
}

func (m *Manager) release(ctx context.Context, p string) (Selection, error) {
	u, _ := url.Parse(m.URL)
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"), "/")
	if len(parts) != 2 {
		return Selection{}, fmt.Errorf("repository URL must identify owner/repo")
	}
	next := "https://api.github.com/repos/" + parts[0] + "/" + parts[1] + "/releases?per_page=100"
	var xs []release
	for next != "" {
		req, _ := http.NewRequestWithContext(ctx, "GET", next, nil)
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
		e = json.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		if e != nil {
			return Selection{}, e
		}
		for _, r := range page {
			if !r.Draft && !r.Prerelease && Match(p, r.Tag) {
				xs = append(xs, r)
			}
		}
		next = parseNext(res.Header.Get("Link"))
	}
	if len(xs) == 0 {
		return Selection{}, fmt.Errorf("no matching published releases")
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].Published.Equal(xs[j].Published) {
			return xs[i].Tag > xs[j].Tag
		}
		return xs[i].Published.After(xs[j].Published)
	})
	sha, e := m.Run(ctx, m.Mirror, "rev-parse", "refs/tags/"+xs[0].Tag+"^{commit}")
	return Selection{sha, xs[0].Tag, xs[0].Published}, e
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
	if len(sha) != 40 {
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
