package main

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// failOnceCloseStore is a FileStore whose first atomic terminal close fails
// and whose later closes succeed. It models a one-shot flush error inside
// rollbackPendingCreateClears: the failed-create patch and the closed status
// commit together or not at all, so a failed close leaves the creating row
// untouched, claim and in-flight lease included.
type failOnceCloseStore struct {
	*beads.FileStore
	mu    sync.Mutex
	fails int
}

func (s *failOnceCloseStore) CloseWithMetadataIfMatch(id string, rev int64, metadata map[string]string) (beads.Bead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails > 0 {
		s.fails--
		return beads.Bead{}, errors.New("injected file store close failure")
	}
	return s.FileStore.CloseWithMetadataIfMatch(id, rev, metadata)
}

func startCallCount(sp *runtime.Fake, name string) int {
	n := 0
	for _, call := range sp.Calls {
		if call.Method == "Start" && call.Name == name {
			n++
		}
	}
	return n
}

// TestConfiguredNamedSessionRetriesStartAfterFailedPendingCreate pins the
// retry of a configured always-mode session whose pending-create start fails:
// it is kept open (it must not be closed) and the pre-wake last_woke_at lease
// must not outlive the finished attempt. One second later, not after the
// lease timeout, the session must be started again.
func TestConfiguredNamedSessionRetriesStartAfterFailedPendingCreate(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "helper",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{Template: "helper", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "helper")
	env.sp.StartErrors = map[string]error{sessionName: errors.New("start failed")}
	env.desiredState[sessionName] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  sessionName,
		TemplateName: "helper",
	}
	session := env.createSessionBead(sessionName, "helper")
	env.setSessionMetadata(&session, map[string]string{
		"session_name_explicit":      "true",
		"pending_create_claim":       "true",
		"state":                      "creating",
		"continuation_epoch":         "1",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "helper",
		namedSessionModeMetadata:     "always",
	})

	if woken := env.reconcile([]beads.Bead{session}); woken != 0 {
		t.Fatalf("woken = %d, want 0 on the failing start", woken)
	}
	afterFail, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if afterFail.Status != "open" {
		t.Fatalf("status = %q, want open (configured named session survives a transient start failure)", afterFail.Status)
	}
	if got := afterFail.Metadata["session_name"]; got != sessionName {
		t.Fatalf("session_name = %q, want %q", got, sessionName)
	}
	if got := afterFail.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true (the create episode is preserved for retry)", got)
	}
	if got := afterFail.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want empty so the finished attempt is not an in-flight lease", got)
	}
	if startCallCount(env.sp, sessionName) != 1 {
		t.Fatalf("Start calls after the failed tick = %d, want 1", startCallCount(env.sp, sessionName))
	}

	delete(env.sp.StartErrors, sessionName)
	env.clk.Time = env.clk.Time.Add(time.Second)
	if woken := env.reconcile([]beads.Bead{afterFail}); woken != 1 {
		final, _ := env.store.Get(session.ID)
		t.Fatalf("woken = %d, want 1 on the next tick (state=%q last_woke_at=%q claim=%q stderr=%s)",
			woken, final.Metadata["state"], final.Metadata["last_woke_at"], final.Metadata["pending_create_claim"], env.stderr.String())
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("configured session was not started on the tick after the failed pending create")
	}
}

// TestFileStoreFailedRollbackRetriesOnNextTick covers a failed rollback close.
// An unconfigured pending create rolls back on start failure. On FileStore
// that rollback is one atomic close; when the close fails, nothing lands and
// the in-flight lease stays. The next tick, still inside the startup window,
// must retry the rollback rather than wait out the lease.
func TestFileStoreFailedRollbackRetriesOnNextTick(t *testing.T) {
	beadsPath := filepath.Join(t.TempDir(), ".gc", "beads.json")
	fs, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	store := &failOnceCloseStore{FileStore: fs, fails: 1}

	env := newReconcilerTestEnv(t)
	env.store = store
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	env.addDesired("worker", "worker", false)
	env.sp.StartErrors = map[string]error{"worker": errors.New("start failed")}
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim":      "true",
		"pending_create_started_at": env.clk.Now().UTC().Format(time.RFC3339),
		"state":                     "creating",
		"continuation_epoch":        "1",
	})

	if woken := env.reconcile([]beads.Bead{session}); woken != 0 {
		t.Fatalf("woken = %d, want 0 on the failing start", woken)
	}
	afterFail, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if afterFail.Status != "open" {
		t.Fatalf("status = %q, want open after the injected close failure", afterFail.Status)
	}
	if got := afterFail.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true (the atomic close wrote nothing)", got)
	}
	if pendingCreateStartInFlightInfo(env.sessionInfo(afterFail.ID), env.clk, 0) {
		t.Fatalf("failed rollback left an in-flight lease (last_woke_at=%q)", afterFail.Metadata["last_woke_at"])
	}

	env.clk.Time = env.clk.Time.Add(time.Second)
	env.reconcile([]beads.Bead{afterFail})
	final, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Status != "closed" {
		t.Fatalf("status = %q, want closed on the next tick (state=%q claim=%q last_woke_at=%q started_at=%q stderr=%s)",
			final.Status, final.Metadata["state"], final.Metadata["pending_create_claim"], final.Metadata["last_woke_at"], final.Metadata["pending_create_started_at"], env.stderr.String())
	}
	if got := final.Metadata["state"]; got != "failed-create" {
		t.Fatalf("state = %q, want failed-create", got)
	}
}
