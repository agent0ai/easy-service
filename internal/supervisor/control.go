package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	return selected.selection.SHA, c.Engine.deploy(ctx, watchCtx, selected.selection, nil)
}

type Status struct {
	State        string `json:"state"`
	Revision     string `json:"revision,omitempty"`
	RuntimeImage string `json:"runtime_image"`
	Digest       string `json:"digest,omitempty"`
	MemoryBytes  uint64 `json:"memory_bytes"`
	MemoryLimit  uint64 `json:"memory_limit"`
	MemoryError  string `json:"memory_error,omitempty"`
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	s := Status{State: "waiting", RuntimeImage: e.Cfg.RuntimeImage, MemoryLimit: e.Cfg.ServiceMemoryLimit}
	a := e.active
	if a != nil {
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
		_ = json.NewEncoder(w).Encode(c.Engine.Status())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", writeStatus)
	for _, name := range []string{"redeploy", "restart"} {
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
