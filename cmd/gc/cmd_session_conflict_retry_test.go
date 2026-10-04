package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
	beadslib "github.com/steveyegge/beads"
)

// conflictSessionStore fails a selected lifecycle write before applying it.
// It records reads after the failure so a second command round must fetch the
// session state again instead of replaying a patch from round one.
type conflictSessionStore struct {
	beads.Store
	writeKind          string
	failure            error
	alwaysFail         bool
	writes             int
	readsAfterConflict int
}

func (s *conflictSessionStore) Get(id string) (beads.Bead, error) {
	if s.writes > 0 {
		s.readsAfterConflict++
	}
	return s.Store.Get(id)
}

func (s *conflictSessionStore) resultFor(kind string) error {
	if kind != s.writeKind {
		return nil
	}
	s.writes++
	if s.alwaysFail || s.writes == 1 {
		return s.failure
	}
	return nil
}

func (s *conflictSessionStore) SetMetadataBatch(id string, patch map[string]string) error {
	if err := s.resultFor("metadata"); err != nil {
		return err
	}
	return s.Store.SetMetadataBatch(id, patch)
}

func (s *conflictSessionStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.resultFor("update"); err != nil {
		return err
	}
	return s.Store.Update(id, opts)
}

func (s *conflictSessionStore) Close(id string) error {
	if err := s.resultFor("close"); err != nil {
		return err
	}
	return s.Store.Close(id)
}

func newConflictSessionStore(t *testing.T, kind string, failure error, always bool) (*conflictSessionStore, string) {
	t.Helper()
	store := &conflictSessionStore{Store: beads.NewMemStore(), writeKind: kind, failure: failure, alwaysFail: always}
	b, err := store.Create(beads.Bead{
		Title: "contention fixture", Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "test-session", "template": "worker", "state": "suspended"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return store, b.ID
}

func TestDoSessionWakeConflictRetry(t *testing.T) {
	conflict := errors.New("Error 1213 (40001): serialization failure")
	for _, tt := range []struct {
		name       string
		failure    error
		alwaysFail bool
		wantCode   int
		wantWrites int
		busy       bool
	}{
		{"recovers on second round", conflict, false, 0, 2, false},
		{"exhausts after one retry", conflict, true, 1, 2, true},
		{"permanent error", errors.New("permission denied"), true, 1, 1, false},
		{"bare version mismatch", beadslib.ErrVersionMismatch, true, 1, 1, false},
		{"ambiguous connection result", errors.New("write commit result indeterminate after connection loss (not retried to avoid double-apply)"), true, 1, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, id := newConflictSessionStore(t, "metadata", tt.failure, tt.alwaysFail)
			deps := sessionWakeDeps{store: store, now: func() time.Time { return time.Date(2026, 10, 3, 23, 0, 0, 0, time.UTC) }}
			var out, errOut bytes.Buffer
			code := doSessionWake(id, &out, &errOut, false, deps)
			assertSessionConflictCommandResult(t, "wake", store, tt.failure, code, errOut.String(), tt.wantCode, tt.wantWrites, tt.busy)
		})
	}
}

func TestDoSessionSuspendConflictRetry(t *testing.T) {
	testSessionCommandConflictRetry(t, "suspend", "update", func(t *testing.T, store *conflictSessionStore, id string) (int, string) {
		t.Helper()
		if err := store.Store.SetMetadataBatch(id, map[string]string{"state": "active"}); err != nil {
			t.Fatalf("seed active state: %v", err)
		}
		var out, errOut bytes.Buffer
		code := doSessionSuspend(id, &out, &errOut, false, sessionSuspendDeps{
			store: store, provider: runtime.NewFake(),
		})
		return code, errOut.String()
	})
}

func TestDoSessionSuspendManagedConflictRetry(t *testing.T) {
	for _, alwaysFail := range []bool{false, true} {
		name := "recovers"
		wantCode := 0
		if alwaysFail {
			name, wantCode = "exhausts", 1
		}
		t.Run(name, func(t *testing.T) {
			conflict := errors.New("Error 1213 (40001): serialization failure")
			store, id := newConflictSessionStore(t, "metadata", conflict, alwaysFail)
			if err := store.Store.SetMetadataBatch(id, map[string]string{"state": "active"}); err != nil {
				t.Fatalf("seed active state: %v", err)
			}
			var out, errOut bytes.Buffer
			code := doSessionSuspend(id, &out, &errOut, false, sessionSuspendDeps{
				store: store, cityPath: "/city", cityResolved: true,
				cityUsesManagedReconciler: func(string) bool { return true },
				pokeController:            func(string) error { return nil },
				now:                       func() time.Time { return time.Date(2026, 10, 3, 23, 0, 0, 0, time.UTC) },
			})
			assertSessionConflictCommandResult(t, "suspend", store, conflict, code, errOut.String(), wantCode, 2, alwaysFail)
		})
	}
}

func TestDoSessionCloseConflictRetry(t *testing.T) {
	testSessionCommandConflictRetry(t, "close", "metadata", func(t *testing.T, store *conflictSessionStore, id string) (int, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := doSessionClose(id, &out, &errOut, false, sessionCloseDeps{
			store: store, provider: runtime.NewFake(),
		})
		return code, errOut.String()
	})
}

func testSessionCommandConflictRetry(t *testing.T, verb, writeKind string, run func(*testing.T, *conflictSessionStore, string) (int, string)) {
	t.Helper()
	conflict := errors.New("Error 1213 (40001): serialization failure")
	for _, tt := range []struct {
		name       string
		failure    error
		alwaysFail bool
		wantCode   int
		wantWrites int
		busy       bool
	}{
		{"recovers on second round", conflict, false, 0, 2, false},
		{"exhausts after one retry", conflict, true, 1, 2, true},
		{"permanent error", errors.New("permission denied"), true, 1, 1, false},
		{"bare version mismatch", beadslib.ErrVersionMismatch, true, 1, 1, false},
		{"ambiguous connection result", errors.New("write commit result indeterminate after connection loss (not retried to avoid double-apply)"), true, 1, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, id := newConflictSessionStore(t, writeKind, tt.failure, tt.alwaysFail)
			code, errOut := run(t, store, id)
			assertSessionConflictCommandResult(t, verb, store, tt.failure, code, errOut, tt.wantCode, tt.wantWrites, tt.busy)
		})
	}
}

func assertSessionConflictCommandResult(t *testing.T, verb string, store *conflictSessionStore, failure error, code int, stderr string, wantCode, wantWrites int, busy bool) {
	t.Helper()
	if code != wantCode || store.writes != wantWrites {
		t.Errorf("%s result = (exit %d, %d writes), want (exit %d, %d writes); stderr=%q", verb, code, store.writes, wantCode, wantWrites, stderr)
	}
	if wantWrites == 2 && store.readsAfterConflict == 0 {
		t.Errorf("%s did not re-read lifecycle state in the second round", verb)
	}
	if busy {
		prefix := fmt.Sprintf("gc session %s: session store busy (concurrent update, retried once): ", verb)
		if !strings.HasPrefix(stderr, prefix) || !strings.Contains(stderr, failure.Error()) || !strings.HasSuffix(stderr, "; run the command again\n") {
			t.Errorf("%s stderr = %q, want stable busy message with original error", verb, stderr)
		}
	} else if wantCode != 0 {
		if !strings.Contains(stderr, failure.Error()) || strings.Contains(stderr, "session store busy") {
			t.Errorf("%s stderr = %q, want original non-retryable error without busy message", verb, stderr)
		}
	}
}

// This one boundary test invokes Cobra's public command path. The fake-store
// cases above own the retry matrix; here the relevant risk is forgetting to
// route one of the three CLI verbs through its tested command body.
func TestSessionLifecycleCobraCommandWiring(t *testing.T) {
	for _, tt := range []struct {
		verb       string
		initial    string
		wantState  string
		newCommand func(*bytes.Buffer, *bytes.Buffer) *cobra.Command
	}{
		{"wake", "suspended", "asleep", func(out, errOut *bytes.Buffer) *cobra.Command { return newSessionWakeCmd(out, errOut) }},
		{"suspend", "active", "suspended", func(out, errOut *bytes.Buffer) *cobra.Command { return newSessionSuspendCmd(out, errOut) }},
		{"close", "active", "closed", func(out, errOut *bytes.Buffer) *cobra.Command { return newSessionCloseCmd(out, errOut) }},
	} {
		t.Run(tt.verb, func(t *testing.T) {
			cityDir := t.TempDir()
			writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_DIR", t.TempDir())
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "fake")
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatalf("open city store: %v", err)
			}
			b, err := store.Create(beads.Bead{Title: "CLI fixture", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
				"session_name": "worker-cli", "template": "worker", "state": tt.initial,
			}})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			var out, errOut bytes.Buffer
			cmd := tt.newCommand(&out, &errOut)
			cmd.SetArgs([]string{b.ID})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("gc session %s: %v; stderr=%q", tt.verb, err, errOut.String())
			}
			updated, err := store.Get(b.ID)
			if err != nil {
				t.Fatalf("read session after %s: %v", tt.verb, err)
			}
			gotState := updated.Metadata["state"]
			if tt.verb == "close" {
				gotState = updated.Status
			}
			if gotState != tt.wantState {
				t.Fatalf("gc session %s state = %q, want %q; stdout=%q stderr=%q", tt.verb, gotState, tt.wantState, out.String(), errOut.String())
			}
		})
	}
}
