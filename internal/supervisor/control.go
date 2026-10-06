package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/example/easy-service/internal/revision"
)

type command struct {
	ctx    context.Context
	action string
	result chan error
}

type selectionRequest struct {
	ctx    context.Context
	result chan selectionResult
}

type selectionResult struct {
	selection revision.Selection
	err       error
}

func (c *Controller) execute(ctx, watchCtx context.Context, action string, refresh chan<- selectionRequest, updates <-chan revision.Selection) (string, error) {
	if action == "apply" {
		return c.apply(ctx, watchCtx)
	}
	if action == "restart" {
		generation := c.Engine.activeGeneration()
		if generation == 0 {
			return "", fmt.Errorf("no active deployment to restart")
		}
		return "", c.Engine.recover(ctx, watchCtx, generation, true)
	}
	if action != "redeploy" {
		return "", fmt.Errorf("unknown action %q", action)
	}
	if c.Selector == nil {
		return "", fmt.Errorf("set GIT_URL and RUN_COMMAND, then run config apply")
	}
	result := make(chan selectionResult, 1)
	select {
	case refresh <- selectionRequest{ctx, result}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	var selected selectionResult
	select {
	case selected = <-result:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if selected.err != nil {
		return "", selected.err
	}
	// The polling worker completed all older samples before this request.
	// Discard them before forcing deployment of the freshly selected revision.
	select {
	case <-updates:
	default:
	}
	return selected.selection.SHA, c.deploySelection(ctx, watchCtx, selected.selection)
}

type Status struct {
	Instance      string `json:"instance,omitempty"`
	ConfigPending bool   `json:"config_pending"`
	State         string `json:"state"`
	Revision      string `json:"revision,omitempty"`
	RuntimeImage  string `json:"runtime_image"`
	Digest        string `json:"digest,omitempty"`
	MemoryBytes   uint64 `json:"memory_bytes"`
	MemoryLimit   uint64 `json:"memory_limit"`
	MemoryError   string `json:"memory_error,omitempty"`
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	s := Status{State: "waiting", RuntimeImage: e.Cfg.RuntimeImage, MemoryLimit: e.Cfg.ServiceMemoryLimit}
	a := e.active
	if a != nil {
		s.Instance = filepath.Base(a.Instance.RootPath())
		s.Revision, s.Digest = a.Revision, a.Digest
		s.State = "unhealthy"
		if !a.Instance.Exited() && e.Proxy.Current() == a.Backend {
			s.State = "running"
		}
	}
	e.mu.Unlock()
	if a != nil {
		var err error
		s.MemoryBytes, err = a.Instance.MemoryUsage()
		if err != nil {
			s.MemoryError = err.Error()
		}
	}
	return s
}

func (e *Engine) activeGeneration() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == nil {
		return 0
	}
	return e.active.generation
}

func ControlPath(data string) string { return filepath.Join(data, "control", "service.sock") }

// StartControl exposes only a private Unix socket; workload HTTP stays on port 80.
// Call before Run so the command channel has one stable owner.
func (c *Controller) StartControl(ctx context.Context) (func(), error) {
	path := ControlPath(c.Cfg.DataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("control path is not a socket")
		}
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("control socket is already in use")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	c.commands = make(chan command)
	var action sync.Mutex
	writeStatus := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := c.Engine.Status()
		if c.Settings != nil {
			status.ConfigPending = !maps.Equal(c.Settings.Pending(), c.Settings.Applied())
		}
		_ = json.NewEncoder(w).Encode(status)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", writeStatus)
	mux.HandleFunc("POST /kill-draining", func(w http.ResponseWriter, r *http.Request) {
		// An urgent force-stop must be available while another action drains.
		if r.Context().Err() != nil {
			return
		}
		killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		killed, err := c.Engine.KillDraining(killCtx)
		if err != nil {
			log.Printf("force-stop draining failed stopped=%d error=%v", killed, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Killed int `json:"killed"`
		}{killed})
	})
	for _, name := range []string{"redeploy", "restart", "apply"} {
		mux.HandleFunc("POST /"+name, func(w http.ResponseWriter, r *http.Request) {
			if !action.TryLock() {
				http.Error(w, "another action is in progress", http.StatusConflict)
				return
			}
			defer action.Unlock()
			request := command{r.Context(), name, make(chan error, 1)}
			select {
			case c.commands <- request:
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				http.Error(w, "supervisor is stopping", http.StatusServiceUnavailable)
				return
			}
			select {
			case err := <-request.result:
				if err != nil {
					log.Printf("control action failed action=%s error=%v", name, err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				http.Error(w, "supervisor is stopping", http.StatusServiceUnavailable)
				return
			}
			writeStatus(w, r)
		})
	}
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		if c.Settings == nil {
			http.Error(w, "runtime configuration is unavailable", http.StatusServiceUnavailable)
			return
		}
		values := c.Settings.Values(true)
		if names := r.URL.Query()["name"]; len(names) > 0 {
			filtered := make(map[string]string, len(names))
			for _, name := range names {
				value, exists := values[name]
				if !exists {
					http.Error(w, "unknown configuration variable "+name, http.StatusBadRequest)
					return
				}
				filtered[name] = value
			}
			values = filtered
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(values)
	})
	mux.HandleFunc("POST /config", func(w http.ResponseWriter, r *http.Request) {
		if c.Settings == nil {
			http.Error(w, "runtime configuration is unavailable", http.StatusServiceUnavailable)
			return
		}
		if !action.TryLock() {
			http.Error(w, "another action is in progress", http.StatusConflict)
			return
		}
		defer action.Unlock()
		var patch struct {
			Values map[string]string `json:"values"`
			Unset  []string          `json:"unset"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&patch); err != nil {
			http.Error(w, "invalid configuration request", http.StatusBadRequest)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			http.Error(w, "invalid configuration request", http.StatusBadRequest)
			return
		}
		if len(patch.Values)+len(patch.Unset) == 0 {
			http.Error(w, "no configuration changes supplied", http.StatusBadRequest)
			return
		}
		if err := c.Settings.Stage(patch.Values, patch.Unset); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		names := make([]string, 0, len(patch.Values)+len(patch.Unset))
		for name := range patch.Values {
			names = append(names, name)
		}
		names = append(names, patch.Unset...)
		sort.Strings(names)
		log.Printf("configuration staged variables=%s; run config apply to activate", strings.Join(names, ","))
		writeStatus(w, r)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() { server.Close() })
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("control server: %v", err)
		}
	}()
	return func() { stopClose(); server.Close(); <-done }, nil
}
