package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/example/easy-service/internal/state"
)

type ApplyResult struct {
	ID    uint64 `json:"id"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// A submitted snapshot is immutable; only completion is published through done.
type ApplyRequest struct {
	ID        uint64            `json:"id"`
	Overrides map[string]string `json:"overrides"`
	done      chan struct{}
	result    ApplyResult
}

func (r *ApplyRequest) Wait(ctx context.Context) (ApplyResult, error) {
	select {
	case <-r.done:
		return r.result, nil
	case <-ctx.Done():
		return ApplyResult{}, ctx.Err()
	}
}

type ApplyStatus struct {
	Running uint64       `json:"running,omitempty"`
	Queued  uint64       `json:"queued,omitempty"`
	Last    *ApplyResult `json:"last,omitempty"`
}

type applyQueue struct {
	Sequence uint64        `json:"sequence"`
	Running  *ApplyRequest `json:"running,omitempty"`
	Pending  *ApplyRequest `json:"pending,omitempty"`
	Last     *ApplyResult  `json:"last,omitempty"`
}

func (s *Settings) applyPath() string { return filepath.Join(s.store.ConfigDir, "apply.json") }

func (s *Settings) loadApply() error {
	s.applyReady = make(chan struct{}, 1)
	f, err := os.Open(s.applyPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, (3<<20)+1))
	if err != nil {
		return err
	}
	if len(body) > 3<<20 {
		return errors.New("submitted configuration exceeds 3 MiB")
	}
	if err := json.Unmarshal(body, &s.applies); err != nil {
		return err
	}
	q := &s.applies
	for _, r := range []*ApplyRequest{q.Running, q.Pending} {
		if r == nil {
			continue
		}
		if r.ID == 0 || r.ID > q.Sequence || r.Overrides == nil {
			return errors.New("invalid submitted configuration")
		}
		for name, value := range r.Overrides {
			if err := validateOverride(name, value, s.base); err != nil {
				return err
			}
		}
		r.done = make(chan struct{})
	}
	if q.Running != nil && q.Pending != nil && q.Pending.ID <= q.Running.ID {
		return errors.New("invalid apply order")
	}
	if q.Last != nil && (q.Last.ID == 0 || q.Last.ID > q.Sequence || (q.Last.State != "completed" && q.Last.State != "failed")) {
		return errors.New("invalid apply result")
	}
	// Resume only the newest submitted intent; staged edits remain separate.
	if q.Pending == nil {
		q.Pending = q.Running
	}
	q.Running = nil
	if q.Pending != nil {
		s.signalApply()
	}
	return nil
}

func (s *Settings) saveApply(q applyQueue) error {
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if len(b) > 3<<20 {
		return errors.New("submitted configuration exceeds 3 MiB")
	}
	if err := os.MkdirAll(s.store.ConfigDir, 0700); err != nil {
		return err
	}
	return state.AtomicWrite(s.applyPath(), b)
}

func (s *Settings) signalApply() {
	select {
	case s.applyReady <- struct{}{}:
	default:
	}
}
func (s *Settings) ApplyReady() <-chan struct{} { return s.applyReady }

// SubmitApply freezes and persists the staged overrides before acknowledging them.
func (s *Settings) SubmitApply() (*ApplyRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := maps.Clone(s.base)
	maps.Copy(values, s.pending)
	if _, err := Parse(values, true); err != nil {
		return nil, err
	}
	q := s.applies
	if q.Pending != nil && maps.Equal(q.Pending.Overrides, s.pending) {
		s.signalApply()
		return q.Pending, nil
	}
	if q.Pending == nil && q.Running != nil && maps.Equal(q.Running.Overrides, s.pending) {
		return q.Running, nil
	}
	if q.Sequence == ^uint64(0) {
		return nil, errors.New("apply sequence exhausted")
	}
	q.Sequence++
	r := &ApplyRequest{ID: q.Sequence, Overrides: maps.Clone(s.pending), done: make(chan struct{})}
	previous := q.Pending
	q.Pending = r
	if err := s.saveApply(q); err != nil {
		return nil, err
	}
	s.applies = q
	if previous != nil {
		previous.result = ApplyResult{ID: previous.ID, State: "superseded", Error: fmt.Sprintf("superseded by apply %d", r.ID)}
		close(previous.done)
	}
	s.signalApply()
	return r, nil
}

// TakeApply is called only by the serialized controller, never by a CLI waiter.
func (s *Settings) TakeApply() *ApplyRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applies.Running != nil {
		return nil
	}
	// The durable pending snapshot remains replayable until completion.
	r := s.applies.Pending
	s.applies.Pending, s.applies.Running = nil, r
	return r
}

func (s *Settings) FinishApply(r *ApplyRequest, failure error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := ApplyResult{ID: r.ID, State: "completed"}
	if failure != nil {
		result.State, result.Error = "failed", failure.Error()
		values := maps.Clone(s.base)
		maps.Copy(values, r.Overrides)
		var private []string
		for name, value := range values {
			if value != "" && (name == "GIT_TOKEN" || strings.HasPrefix(name, "APP_")) {
				private = append(private, value)
			}
		}
		// Match whole secrets before overlapping values; never rewrite the mask.
		slices.SortFunc(private, func(a, b string) int { return len(b) - len(a) })
		var replacements []string
		for _, value := range private {
			replacements = append(replacements, value, "[redacted]")
		}
		result.Error = strings.NewReplacer(replacements...).Replace(result.Error)
		if len(result.Error) > 4096 {
			result.Error = strings.ToValidUTF8(result.Error[:4096], "") + "…"
		}
	}
	q := s.applies
	q.Running, q.Last = nil, &result
	if err := s.saveApply(q); err != nil {
		result.State = "failed"
		result.Error = "could not persist apply result; check configuration storage and status before retrying"
	}
	s.applies = q
	r.result = result
	close(r.done)
	if q.Pending != nil {
		s.signalApply()
	}
}

func (s *Settings) ApplyStatus() ApplyStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := ApplyStatus{}
	if s.applies.Running != nil {
		status.Running = s.applies.Running.ID
	}
	if s.applies.Pending != nil {
		status.Queued = s.applies.Pending.ID
	}
	if s.applies.Last != nil {
		last := *s.applies.Last
		status.Last = &last
	}
	return status
}

func (s *Settings) WithOverrides(overrides map[string]string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := maps.Clone(s.base)
	maps.Copy(values, overrides)
	return values
}
