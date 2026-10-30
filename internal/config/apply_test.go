package config

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/easy-service/internal/state"
)

func applySettings(t *testing.T) (*Settings, map[string]string, state.Store) {
	t.Helper()
	base := maps.Clone(Defaults)
	base["GIT_URL"], base["RUN_COMMAND"] = "https://github.com/acme/api", "serve"
	base["DATA_DIR"], base["CONFIG_DIR"], base["LOG_DIR"] = t.TempDir(), t.TempDir(), t.TempDir()
	store := state.Store{Data: base["DATA_DIR"], ConfigDir: base["CONFIG_DIR"]}
	settings, _, err := Open(base, store)
	if err != nil {
		t.Fatal(err)
	}
	return settings, base, store
}

func TestApplySnapshotsCoalesceAndWaitersDetach(t *testing.T) {
	s, _, _ := applySettings(t)
	stage := func(values map[string]string) {
		t.Helper()
		if err := s.Stage(values, nil); err != nil {
			t.Fatal(err)
		}
	}
	submit := func() *ApplyRequest {
		t.Helper()
		r, err := s.SubmitApply()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	stage(map[string]string{"APP_FIRST": "one"})
	first := submit()
	if s.TakeApply() != first {
		t.Fatal("first submission missing")
	}
	if submit() != first {
		t.Fatal("identical running apply duplicated")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := first.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("waiter did not detach", err)
	}
	stage(map[string]string{"APP_SECOND": "two"})
	second := submit()
	if submit() != second {
		t.Fatal("identical queued apply duplicated")
	}
	stage(map[string]string{"APP_SECOND": "three"})
	third := submit()
	result, err := second.Wait(context.Background())
	if err != nil || result.State != "superseded" {
		t.Fatal("superseded waiter", result, err)
	}
	stage(map[string]string{"APP_DRAFT": "not-submitted"})
	if s.TakeApply() != nil {
		t.Fatal("parallel apply started")
	}
	s.FinishApply(first, nil)
	if result, err := first.Wait(context.Background()); err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
	if s.TakeApply() != third || third.Overrides["APP_FIRST"] != "one" || third.Overrides["APP_SECOND"] != "three" || third.Overrides["APP_DRAFT"] != "" {
		t.Fatal("queued snapshot changed", third.ID)
	}
	s.FinishApply(third, errors.New("fixture failed"))
	status := s.ApplyStatus()
	if status.Running != 0 || status.Queued != 0 || status.Last.ID != third.ID || status.Last.State != "failed" || status.Last.Error != "fixture failed" {
		t.Fatal(status)
	}
	if s.TakeApply() != nil {
		t.Fatal("failed apply automatically retried")
	}
}

func TestSubmittedApplyRecoveryKeepsLatestAndExcludesDraft(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "newer queued"}[queued], func(t *testing.T) {
			s, base, store := applySettings(t)
			if err := s.Stage(map[string]string{"APP_VALUE": "first"}, nil); err != nil {
				t.Fatal(err)
			}
			first, err := s.SubmitApply()
			if err != nil {
				t.Fatal(err)
			}
			s.TakeApply()
			want := first
			if queued {
				if err := s.Stage(map[string]string{"APP_VALUE": "latest"}, nil); err != nil {
					t.Fatal(err)
				}
				want, err = s.SubmitApply()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Stage(map[string]string{"APP_VALUE": "draft"}, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.Reconcile(); err != nil {
				t.Fatal(err)
			}
			reopened, _, err := Open(base, store)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-reopened.ApplyReady():
			case <-time.After(time.Second):
				t.Fatal("recovered apply not scheduled")
			}
			got := reopened.TakeApply()
			if got == nil || got.ID != want.ID || !maps.Equal(got.Overrides, want.Overrides) || reopened.Values(true)["APP_VALUE"] != "draft" {
				t.Fatal("wrong recovered intent")
			}
			reopened.FinishApply(got, nil)
			again, _, err := Open(base, store)
			if err != nil || again.TakeApply() != nil || again.ApplyStatus().Last.State != "completed" {
				t.Fatal("completed apply replayed", err)
			}
			info, err := os.Stat(filepath.Join(base["CONFIG_DIR"], "apply.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("apply file permissions", err)
			}
		})
	}
}

func TestApplyValidationAndPersistenceFailures(t *testing.T) {
	s, base, _ := applySettings(t)
	if err := s.Stage(map[string]string{"APP_PRIVATE": "private-synthetic-value"}, nil); err != nil {
		t.Fatal(err)
	}
	original, err := s.SubmitApply()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stage(map[string]string{"POLL_INTERVAL": "invalid"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitApply(); err == nil {
		t.Fatal("invalid configuration submitted")
	}
	if s.ApplyStatus().Queued != original.ID {
		t.Fatal("invalid submission replaced queue")
	}
	if err := s.Stage(map[string]string{"POLL_INTERVAL": "300", "APP_OTHER": "value"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.applyPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.applyPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitApply(); err == nil || s.ApplyStatus().Queued != original.ID {
		t.Fatal("failed durable submission accepted")
	}
	if err := os.Remove(s.applyPath()); err != nil {
		t.Fatal(err)
	}
	s.TakeApply()
	s.FinishApply(original, errors.New("setup failed: private-synthetic-value"))
	result, err := original.Wait(context.Background())
	if err != nil || strings.Contains(result.Error, "private-synthetic-value") || !strings.Contains(result.Error, "[redacted]") {
		t.Fatal("private value in result")
	}
	reopened, _, err := Open(base, s.store)
	if err != nil || reopened.ApplyStatus().Last.State != "failed" || reopened.TakeApply() != nil {
		t.Fatal("failure result not retained", err)
	}
	if err := os.WriteFile(s.applyPath(), []byte(`{"sequence":1,"pending":{"id":2,"overrides":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(base, s.store); err == nil {
		t.Fatal("invalid persisted queue accepted")
	}
}

func TestApplyCompletionPersistenceFailureRemainsVisible(t *testing.T) {
	s, _, _ := applySettings(t)
	r, err := s.SubmitApply()
	if err != nil {
		t.Fatal(err)
	}
	s.TakeApply()
	if err := os.Remove(s.applyPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.applyPath(), 0700); err != nil {
		t.Fatal(err)
	}
	s.FinishApply(r, nil)
	result, err := r.Wait(context.Background())
	if err != nil || result.State != "failed" || !strings.Contains(result.Error, "persist") || s.ApplyStatus().Last.State != "failed" {
		t.Fatal("result persistence failure hidden", result, err)
	}
}

func TestApplyFailureRedactsOverlappingValuesOnce(t *testing.T) {
	s, _, _ := applySettings(t)
	if err := s.Stage(map[string]string{"APP_KEY": "private-synthetic-value", "APP_PREFIX": "private", "APP_LETTER": "e"}, nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.SubmitApply()
	if err != nil {
		t.Fatal(err)
	}
	s.TakeApply()
	s.FinishApply(r, errors.New("private-synthetic-value"))
	result, err := r.Wait(context.Background())
	if err != nil || result.Error != "[redacted]" {
		t.Fatal("failure log exposed a credential fragment or rewrote its mask")
	}
}
