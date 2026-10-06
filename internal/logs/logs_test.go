package logs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDailyPagesRetentionAndRestart(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, Policy{Days: 30, FileSize: 1024, TotalSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 23, 59, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	info := Info{ID: "abc123def456", Commit: strings.Repeat("a", 40), Image: "node:22", DeployedAt: now}
	m.Register(info.ID, info)
	out, stderr, id := m.Output(info.ID, "run", io.Discard, io.Discard)
	if id != info.ID {
		t.Fatal("instance identity changed")
	}
	if n, err := out.Write([]byte(strings.Repeat("output\n", 700))); err != nil || n != 4900 {
		t.Fatalf("output: %d %v", n, err)
	}
	check := func() []string {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var size int64
		var names []string
		for _, entry := range entries {
			if !ownedName.MatchString(entry.Name()) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if len(b) > 1024 || !bytes.HasPrefix(b, []byte("---\ninstance: \"abc123def456\"\n")) || !bytes.Contains(b, []byte("commit: \""+info.Commit+"\"")) {
				t.Fatalf("page lacks bounds/frontmatter: %s", entry.Name())
			}
			size += int64(len(b))
			names = append(names, entry.Name())
		}
		if size > 4096 {
			t.Fatalf("total cap exceeded: %d", size)
		}
		return names
	}
	first := check()
	if len(first) < 2 {
		t.Fatal("file pagination did not occur")
	}
	now = now.Add(2 * time.Minute)
	if _, err := stderr.Write([]byte("next-day\n")); err != nil {
		t.Fatal(err)
	}
	names := check()
	found := false
	for _, name := range names {
		if strings.Contains(name, "2026-10-07") {
			found = true
		}
	}
	if !found {
		t.Fatal("UTC day did not rotate")
	}
	m.Release(info.ID)
	m.Close()
	reopened, err := New(dir, Policy{Days: 30, FileSize: 1024, TotalSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.pages) == 0 {
		t.Fatal("instance retirement/restart lost logs")
	}
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("leave alone"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "fff123fff123-2020-01-01-000001.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	reopened.now = func() time.Time { return now.AddDate(0, 0, 31) }
	if err := reopened.Trim(); err != nil {
		t.Fatal(err)
	}
	if len(reopened.pages) != 0 {
		t.Fatal("expired logs survived collection")
	}
	for _, path := range []string{unrelated, outside, link} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("collector removed unrelated file/symlink:", err)
		}
	}
}

func TestConcurrentCapturePolicyChangesAndFileFailure(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir, Policy{Days: 30, FileSize: 2048, TotalSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Register("abc123abc123", Info{ID: "abc123abc123", Commit: "commit", DeployedAt: time.Now()})
	stdout, stderr, _ := m.Output("abc123abc123", "run", io.Discard, io.Discard)
	var wg sync.WaitGroup
	for _, out := range []io.Writer{stdout, stderr} {
		wg.Add(1)
		go func(out io.Writer) {
			defer wg.Done()
			for range 100 {
				if _, err := out.Write([]byte("a log line\n")); err != nil {
					t.Error(err)
				}
			}
		}(out)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 10 {
			if err := m.Configure(Policy{Days: 30, FileSize: 1024, TotalSize: 4096}); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	if err := m.Trim(); err != nil {
		t.Fatal(err)
	}
	if m.total > 4096 {
		t.Fatal("live policy update exceeded total cap")
	}
	var console bytes.Buffer
	m.Register("def123def123", Info{ID: "def123def123"})
	out, _, _ := m.Output("def123def123", "run", &console, io.Discard)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if n, err := out.Write([]byte("survives\n")); n != 9 || err != nil || !strings.Contains(console.String(), "survives") {
		t.Fatalf("file failure interrupted console/application output: %d %v %q", n, err, console.String())
	}
}
