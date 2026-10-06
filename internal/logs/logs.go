package logs

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Policy struct {
	Days                int
	FileSize, TotalSize uint64
}
type Info struct {
	ID, Commit, GitURL, Image, Digest string
	DeployedAt                        time.Time
}
type page struct {
	name, id, day string
	size          uint64
	created       time.Time
	file          *os.File
}

type Manager struct {
	mu         sync.Mutex
	dir        string
	policy     Policy
	pages      map[string]*page
	current    map[string]*page
	registered map[string]Info
	now        func() time.Time
	lastError  time.Time
	total      uint64
}

var instanceName = regexp.MustCompile(`^[a-z0-9]{12}$`)

var ownedName = regexp.MustCompile(`^([a-z0-9]{12})-(\d{4}-\d{2}-\d{2})-(\d{6})\.log$`)

func New(dir string, policy Policy) (*Manager, error) {
	if err := validate(policy); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, policy: policy, pages: map[string]*page{}, current: map[string]*page{}, registered: map[string]Info{}, now: time.Now}
	if err := m.scan(); err != nil {
		return nil, err
	}
	if err := m.Trim(); err != nil {
		return nil, err
	}
	return m, nil
}

func validate(p Policy) error {
	if p.Days < 1 || p.Days > int((time.Duration(1<<63-1))/(24*time.Hour)) || p.FileSize < 1024 || p.TotalSize < p.FileSize {
		return fmt.Errorf("invalid log retention policy")
	}
	return nil
}

func (m *Manager) scan() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		parts := ownedName.FindStringSubmatch(entry.Name())
		if parts == nil || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if _, err := time.Parse("2006-01-02", parts[2]); err != nil {
			continue
		}
		m.pages[entry.Name()] = &page{name: entry.Name(), id: parts[1], day: parts[2], size: uint64(info.Size()), created: info.ModTime().UTC()}
		m.total += uint64(info.Size())
	}
	return nil
}

func (m *Manager) Register(key string, info Info) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registered[key] = info
}

func (m *Manager) HasInstance(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pages {
		if p.id == id {
			return true
		}
	}
	for _, info := range m.registered {
		if info.ID == id {
			return true
		}
	}
	return false
}

// Output consumes a registration once and returns immutable per-process writers.
func (m *Manager) Output(key, phase string, stdout, stderr io.Writer) (io.Writer, io.Writer, string) {
	m.mu.Lock()
	info, ok := m.registered[key]
	delete(m.registered, key)
	m.mu.Unlock()
	if !ok {
		info = Info{ID: key, DeployedAt: m.now().UTC()}
	}
	return &writer{m, info, phase, "stdout", stdout}, &writer{m, info, phase, "stderr", stderr}, info.ID
}

type writer struct {
	manager       *Manager
	info          Info
	phase, stream string
	console       io.Writer
}

func (w *writer) Write(b []byte) (int, error) {
	// Console and file capture are independent: neither sink can disable the other.
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if line == "" {
			continue
		}
		out := []byte(fmt.Sprintf("[%s][%s][%s] %s", w.info.ID, w.phase, w.stream, line))
		if w.console != nil {
			if _, err := w.console.Write(out); err != nil {
				w.manager.report(err)
			}
		}
		if err := w.manager.write(w.info, out); err != nil {
			w.manager.report(err)
		}
	}
	return len(b), nil
}

func (m *Manager) report(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.lastError.IsZero() || now.Sub(m.lastError) >= time.Minute {
		log.Printf("application log capture failed: %v", err)
		m.lastError = now
	}
}

func (m *Manager) write(info Info, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !instanceName.MatchString(info.ID) {
		return fmt.Errorf("invalid log instance identity")
	}
	now := m.now().UTC()
	day := now.Format("2006-01-02")
	for len(b) > 0 {
		p := m.current[info.ID]
		if p == nil || p.day != day || p.size >= m.policy.FileSize {
			if p != nil && p.file != nil {
				p.file.Close()
				p.file = nil
			}
			var err error
			p, err = m.create(info, day, now)
			if err != nil {
				return err
			}
		}
		if p.file == nil {
			f, err := os.OpenFile(filepath.Join(m.dir, p.name), os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
			if os.IsNotExist(err) {
				delete(m.pages, p.name)
				m.total -= p.size
				delete(m.current, info.ID)
				continue
			}
			if err != nil {
				return err
			}
			p.file = f
		}
		n := uint64(len(b))
		if available := m.policy.FileSize - p.size; n > available {
			n = available
		}
		if m.total+n > m.policy.TotalSize {
			if err := m.trim(now, n); err != nil {
				return err
			}
		}
		if m.current[info.ID] != p {
			continue
		}
		written, err := p.file.Write(b[:int(n)])
		p.size += uint64(written)
		m.total += uint64(written)
		b = b[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (m *Manager) create(info Info, day string, now time.Time) (*page, error) {
	header := []byte(fmt.Sprintf("---\ninstance: %q\ncommit: %q\ngit_url: %q\nruntime_image: %q\nimage_digest: %q\ndeployed_at: %q\nday: %q\n---\n", info.ID, info.Commit, info.GitURL, info.Image, info.Digest, info.DeployedAt.UTC().Format(time.RFC3339Nano), day))
	if uint64(len(header)) >= m.policy.FileSize {
		return nil, fmt.Errorf("log frontmatter exceeds LOG_MAX_FILE_SIZE")
	}
	if err := m.trim(now, uint64(len(header))); err != nil {
		return nil, err
	}
	var f *os.File
	var name string
	for sequence := 1; sequence <= 999999; sequence++ {
		name = fmt.Sprintf("%s-%s-%06d.log", info.ID, day, sequence)
		if _, exists := m.pages[name]; exists {
			continue
		}
		var err error
		f, err = os.OpenFile(filepath.Join(m.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	if f == nil {
		return nil, fmt.Errorf("daily log page limit reached")
	}
	n, err := f.Write(header)
	if err != nil || n != len(header) {
		f.Close()
		os.Remove(filepath.Join(m.dir, name))
		if err == nil {
			err = io.ErrShortWrite
		}
		return nil, err
	}
	p := &page{name: name, id: info.ID, day: day, size: uint64(n), created: now, file: f}
	m.pages[name], m.current[info.ID] = p, p
	m.total += uint64(n)
	log.Printf("application log opened instance=%s file=%s", info.ID, name)
	return p, nil
}

func (m *Manager) trim(now time.Time, reserve uint64) error {
	var total uint64
	pages := make([]*page, 0, len(m.pages))
	for _, p := range m.pages {
		total += p.size
		pages = append(pages, p)
	}
	sort.Slice(pages, func(i, j int) bool {
		if pages[i].created.Equal(pages[j].created) {
			return pages[i].name < pages[j].name
		}
		return pages[i].created.Before(pages[j].created)
	})
	cutoff := now.Add(-time.Duration(m.policy.Days) * 24 * time.Hour).Format("2006-01-02")
	var removed int
	var bytes uint64
	for _, p := range pages {
		if p.day >= cutoff && p.size <= m.policy.FileSize && total+reserve <= m.policy.TotalSize {
			continue
		}
		if p.file != nil {
			p.file.Close()
			p.file = nil
		}
		if err := os.Remove(filepath.Join(m.dir, p.name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		delete(m.pages, p.name)
		if m.current[p.id] == p {
			delete(m.current, p.id)
		}
		total -= p.size
		m.total -= p.size
		removed++
		bytes += p.size
	}
	if removed > 0 {
		log.Printf("application log collector removed=%d bytes=%d", removed, bytes)
	}
	return nil
}

func (m *Manager) Trim() error { m.mu.Lock(); defer m.mu.Unlock(); return m.trim(m.now().UTC(), 0) }
func (m *Manager) Configure(policy Policy) error {
	if err := validate(policy); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policy = policy
	return m.trim(m.now().UTC(), 0)
}

func (m *Manager) Release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.current[id]; p != nil && p.file != nil {
		p.file.Close()
		p.file = nil
	}
}

func (m *Manager) Forget(key string) { m.mu.Lock(); defer m.mu.Unlock(); delete(m.registered, key) }

func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-t.C:
			if err := m.Trim(); err != nil {
				m.report(err)
			}
		}
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pages {
		if p.file != nil {
			p.file.Close()
			p.file = nil
		}
	}
}
