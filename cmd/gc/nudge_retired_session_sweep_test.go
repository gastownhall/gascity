package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// retiredNudgeTestCity builds a city dir with one worker session created,
// suspended and slept unheld (so a nudge would queue behind a managed wake),
// and returns its manager and store. The managed-reconciler and poke seams are
// pinned to the dir, as the triage repro does.
func retiredNudgeTestCity(t *testing.T) (string, *session.Manager, *runtime.Fake, session.Info) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sleepUnheld(t, store, info.ID)
	prevManaged := nudgeCityUsesManagedReconciler
	prevPoke := nudgePokeController
	nudgeCityUsesManagedReconciler = func(cityPath string) bool { return cityPath == dir }
	nudgePokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() {
		nudgeCityUsesManagedReconciler = prevManaged
		nudgePokeController = prevPoke
	})
	return dir, mgr, fake, info
}

// sweepQueueBuckets reads the queue's raw state so a test can assert bucket
// membership without the maintaining list's own sweep running first.
func sweepQueueBuckets(t *testing.T, dir string) (pending, inFlight, dead []nudgequeue.Item) {
	t.Helper()
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return state.Pending, state.InFlight, state.Dead
}

// enqueueSweepItem queues one hand-keyed item fenced to sessionID, so a test
// can choose the queue key it is served by independently of the target that
// would have nudged it.
func enqueueSweepItem(t *testing.T, dir string, store beads.NudgesStore, key, sessionID, id string) {
	t.Helper()
	item := newQueuedNudgeWithOptions(key, "sweep me", "session", time.Now(), queuedNudgeOptions{ID: id, SessionID: sessionID})
	if err := enqueueQueuedNudgeWithStore(dir, store, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}
}

func assertShadowTerminated(t *testing.T, store beads.NudgesStore, item nudgequeue.Item) {
	t.Helper()
	if item.BeadID == "" {
		t.Fatalf("item %s has no shadow bead", item.ID)
	}
	shadow, err := store.Get(item.BeadID)
	if err != nil {
		t.Fatalf("reading shadow bead %s: %v", item.BeadID, err)
	}
	if shadow.Status != "closed" || shadow.Metadata["state"] != "failed" || shadow.Metadata["terminal_reason"] != "session-terminated" {
		t.Fatalf("shadow bead = status %q state %q terminal_reason %q, want closed/failed/session-terminated",
			shadow.Status, shadow.Metadata["state"], shadow.Metadata["terminal_reason"])
	}
}

// TestRetiredSessionNudgeSweepDeadLettersOrphans covers AC-4 of
// gastownhall/gascity#4534: a pending item fenced to a retired session moves
// to Dead with LastError session-terminated and its gc:nudge shadow closed
// failed — through the sweep itself and through the controller's
// nudge-maintenance watchdog that wires it (the same call in legacy and
// supervisor dispatcher modes; the watchdog runs off the orders lane in
// both).
func TestRetiredSessionNudgeSweepDeadLettersOrphans(t *testing.T) {
	t.Run("direct-sweep", func(t *testing.T) {
		dir, mgr, fake, info := retiredNudgeTestCity(t)
		store := openNudgeBeadStore(dir)
		cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
		target := nudgeTarget{
			cityPath: dir, cfg: cfg, sessionID: info.ID, sessionName: info.SessionName,
			identity: "worker", agent: cfg.Agents[0],
		}
		var stdout, stderr bytes.Buffer
		if code := deliverSessionNudgeWithWorker(target, store, fake, nudgeDeliveryImmediate, &stdout, &stderr); code != 0 {
			t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0 (queued); stderr: %s", code, stderr.String())
		}
		if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now())
		if err != nil {
			t.Fatalf("sweepRetiredSessionNudges: %v", err)
		}
		if moved != 1 {
			t.Fatalf("sweep moved %d items, want 1", moved)
		}
		pending, inFlight, dead := sweepQueueBuckets(t, dir)
		if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 {
			t.Fatalf("queue pending/inFlight/dead = %d/%d/%d, want 0/0/1", len(pending), len(inFlight), len(dead))
		}
		if dead[0].LastError != "session-terminated" {
			t.Fatalf("dead item LastError = %q, want session-terminated", dead[0].LastError)
		}
		assertShadowTerminated(t, store, dead[0])
		// The sweep is idempotent: a second pass moves nothing.
		if moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now()); err != nil || moved != 0 {
			t.Fatalf("second sweep = (%d, %v), want (0, nil)", moved, err)
		}
	})

	for _, dispatcher := range []string{"legacy", "supervisor"} {
		t.Run("watchdog-wiring/"+dispatcher, func(t *testing.T) {
			dir, mgr, fake, info := retiredNudgeTestCity(t)
			store := openNudgeBeadStore(dir)
			cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
			cfg.Daemon.NudgeDispatcher = dispatcher
			target := nudgeTarget{
				cityPath: dir, cfg: cfg, sessionID: info.ID, sessionName: info.SessionName,
				identity: "worker", agent: cfg.Agents[0],
			}
			var stdout, stderr bytes.Buffer
			if code := deliverSessionNudgeWithWorker(target, store, fake, nudgeDeliveryImmediate, &stdout, &stderr); code != 0 {
				t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0 (queued); stderr: %s", code, stderr.String())
			}
			if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID); err != nil {
				t.Fatalf("Close: %v", err)
			}
			var controllerStderr bytes.Buffer
			cr := &CityRuntime{
				cityName:            "test-city",
				cfg:                 cfg,
				cityPath:            dir,
				standaloneCityStore: store,
				stdout:              io.Discard,
				stderr:              &controllerStderr,
				logPrefix:           "gc test",
			}
			cr.runNudgeMailSweepWatchdog(cfg, time.Now())
			pending, inFlight, dead := sweepQueueBuckets(t, dir)
			if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 {
				t.Fatalf("queue pending/inFlight/dead = %d/%d/%d, want 0/0/1; controller stderr: %s",
					len(pending), len(inFlight), len(dead), controllerStderr.String())
			}
			if dead[0].LastError != "session-terminated" {
				t.Fatalf("dead item LastError = %q, want session-terminated", dead[0].LastError)
			}
			if !strings.Contains(controllerStderr.String(), "dead-lettered 1 nudge(s) whose session terminated") {
				t.Fatalf("controller stderr = %q, want the dead-letter line", controllerStderr.String())
			}
			if strings.Contains(controllerStderr.String(), "retired-session sweep:") {
				t.Fatalf("controller stderr = %q, want no sweep error", controllerStderr.String())
			}
			assertShadowTerminated(t, store, dead[0])
		})
	}
}

// TestRetiredSessionSweepSkipsUnfencedAndConfigless asserts the sweep's
// cheap-exit paths: a queue with no fenced pending item never opens a store
// (moved 0, no error), and a nil config sweep is a no-op.
func TestRetiredSessionSweepSkipsUnfencedAndConfigless(t *testing.T) {
	dir, _, _, _ := retiredNudgeTestCity(t)
	store := openNudgeBeadStore(dir)
	enqueueSweepItem(t, dir, store, "worker", "", "n-unfenced")
	if moved, err := sweepRetiredSessionNudges(dir, nil, store, beads.SessionStore{Store: store}, time.Now()); err != nil || moved != 0 {
		t.Fatalf("nil-config sweep = (%d, %v), want (0, nil)", moved, err)
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
	if moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now()); err != nil || moved != 0 {
		t.Fatalf("unfenced sweep = (%d, %v), want (0, nil)", moved, err)
	}
}

// TestRetiredSessionNudgeSweepKeepsSeatedItems covers AC-5 of
// gastownhall/gascity#4534: a pending item whose fenced session is not
// retired, or whose key a configured named session, a configured agent
// template, or an open session answers, stays Pending whatever its fence.
func TestRetiredSessionNudgeSweepKeepsSeatedItems(t *testing.T) {
	t.Run("fence-not-retired-open-session", func(t *testing.T) {
		dir, _, _, info := retiredNudgeTestCity(t)
		store := openNudgeBeadStore(dir)
		cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
		enqueueSweepItem(t, dir, store, info.ID, info.ID, "n-open-fence")
		moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now())
		if err != nil || moved != 0 {
			t.Fatalf("sweep = (%d, %v), want (0, nil)", moved, err)
		}
		pending, _, dead := sweepQueueBuckets(t, dir)
		if len(pending) != 1 || len(dead) != 0 {
			t.Fatalf("queue pending/dead = %d/%d, want 1/0", len(pending), len(dead))
		}
	})
	t.Run("named-session-key", func(t *testing.T) {
		dir, mgr, _, info := retiredNudgeTestCity(t)
		store := openNudgeBeadStore(dir)
		cfg := &config.City{
			Agents:        []config.Agent{{Name: "worker", Provider: "claude"}},
			NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker"}},
		}
		if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		enqueueSweepItem(t, dir, store, "reviewer", info.ID, "n-named-key")
		moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now())
		if err != nil || moved != 0 {
			t.Fatalf("sweep = (%d, %v), want (0, nil)", moved, err)
		}
		pending, _, dead := sweepQueueBuckets(t, dir)
		if len(pending) != 1 || len(dead) != 0 {
			t.Fatalf("queue pending/dead = %d/%d, want 1/0", len(pending), len(dead))
		}
	})
	t.Run("agent-template-key", func(t *testing.T) {
		dir, mgr, _, info := retiredNudgeTestCity(t)
		store := openNudgeBeadStore(dir)
		cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
		if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		enqueueSweepItem(t, dir, store, "worker", info.ID, "n-template-key")
		moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now())
		if err != nil || moved != 0 {
			t.Fatalf("sweep = (%d, %v), want (0, nil)", moved, err)
		}
		pending, _, dead := sweepQueueBuckets(t, dir)
		if len(pending) != 1 || len(dead) != 0 {
			t.Fatalf("queue pending/dead = %d/%d, want 1/0", len(pending), len(dead))
		}
	})
	t.Run("open-session-answers-key", func(t *testing.T) {
		// The fence's session closed, but another open session holds the
		// item's key: a successor seat serves it through Leg B.
		dir, mgr, _, closed := retiredNudgeTestCity(t)
		store := openNudgeBeadStore(dir)
		cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
		if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), closed.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		successor, err := mgr.CreateSession(context.Background(), session.CreateOptions{
			Template: "worker", Title: "Successor", Command: "claude", WorkDir: dir, Provider: "claude",
			ExtraMeta: map[string]string{"session_origin": "manual", "alias": "review"},
		})
		if err != nil {
			t.Fatalf("Create successor: %v", err)
		}
		enqueueSweepItem(t, dir, store, "review", closed.ID, "n-successor-key")
		moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now())
		if err != nil || moved != 0 {
			t.Fatalf("sweep = (%d, %v), want (0, nil)", moved, err)
		}
		if successor.ID == "" {
			t.Fatal("successor session missing")
		}
		pending, _, dead := sweepQueueBuckets(t, dir)
		if len(pending) != 1 || len(dead) != 0 {
			t.Fatalf("queue pending/dead = %d/%d, want 1/0", len(pending), len(dead))
		}
	})
}

// TestRetiredSessionNudgeSweepFailuresLeaveItemsAlone covers AC-6 of
// gastownhall/gascity#4534: an unreadable or missing fence row, or a fence
// that changed between the snapshot and the locked run, leaves the item
// exactly where it is — the read failure is returned for the controller's
// stderr, never read as a retirement — and an InFlight item is not a
// candidate at all.
func TestRetiredSessionNudgeSweepFailuresLeaveItemsAlone(t *testing.T) {
	t.Run("fence-row-not-found", func(t *testing.T) {
		dir, _, _, _ := retiredNudgeTestCity(t)
		store := openNudgeBeadStore(dir)
		cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
		enqueueSweepItem(t, dir, store, "ghost", "gc-no-such-session", "n-missing-row")
		moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now())
		if err == nil || !strings.Contains(err.Error(), `reading session "gc-no-such-session"`) {
			t.Fatalf("sweep err = %v, want the named read error", err)
		}
		if moved != 0 {
			t.Fatalf("sweep moved %d items, want 0", moved)
		}
		pending, _, dead := sweepQueueBuckets(t, dir)
		if len(pending) != 1 || len(dead) != 0 {
			t.Fatalf("queue pending/dead = %d/%d, want 1/0", len(pending), len(dead))
		}
	})
	t.Run("census-fails", func(t *testing.T) {
		dir, mgr, _, info := retiredNudgeTestCity(t)
		store := openNudgeBeadStore(dir)
		cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
		if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		enqueueSweepItem(t, dir, store, info.ID, info.ID, "n-census-down")
		moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: failingListStore{Store: store}}, time.Now())
		if err == nil || !strings.Contains(err.Error(), "open-session census") {
			t.Fatalf("sweep err = %v, want the census error", err)
		}
		if moved != 0 {
			t.Fatalf("sweep moved %d items, want 0", moved)
		}
		pending, _, dead := sweepQueueBuckets(t, dir)
		if len(pending) != 1 || len(dead) != 0 {
			t.Fatalf("queue pending/dead = %d/%d, want 1/0", len(pending), len(dead))
		}
	})
	t.Run("in-flight-not-touched", func(t *testing.T) {
		dir, mgr, _, info := retiredNudgeTestCity(t)
		store := openNudgeBeadStore(dir)
		cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}}
		target := nudgeTarget{
			cityPath: dir, cfg: cfg, sessionID: info.ID, sessionName: info.SessionName,
			identity: "worker", agent: cfg.Agents[0],
		}
		enqueueSweepItem(t, dir, store, info.ID, info.ID, "n-inflight")
		claimed, err := openNudgeQueue(dir, beads.NudgesStore{}).Queue.ClaimBatch(target.claimTarget(dir, nil), time.Now())
		if err != nil || len(claimed) != 1 || claimed[0].ID != "n-inflight" {
			t.Fatalf("ClaimBatch = (%d, %v), want the item", len(claimed), err)
		}
		if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		moved, err := sweepRetiredSessionNudges(dir, cfg, store, beads.SessionStore{Store: store}, time.Now())
		if err != nil || moved != 0 {
			t.Fatalf("sweep = (%d, %v), want (0, nil)", moved, err)
		}
		pending, inFlight, dead := sweepQueueBuckets(t, dir)
		if len(pending) != 0 || len(inFlight) != 1 || len(dead) != 0 {
			t.Fatalf("queue pending/inFlight/dead = %d/%d/%d, want 0/1/0", len(pending), len(inFlight), len(dead))
		}
	})
	t.Run("fence-changed-under-the-lock", func(t *testing.T) {
		// The plan came from the snapshot; the locked run must only move the
		// item whose fence still matches it.
		now := time.Now().UTC()
		planned := map[string]queuedNudge{
			"n-same":  {ID: "n-same", SessionID: "gc-1", ContinuationEpoch: "5"},
			"n-moved": {ID: "n-moved", SessionID: "gc-1", ContinuationEpoch: "5"},
		}
		state := &nudgeQueueState{Pending: []queuedNudge{
			{ID: "n-same", SessionID: "gc-1", ContinuationEpoch: "5"},
			{ID: "n-moved", SessionID: "gc-1", ContinuationEpoch: "6"},
		}}
		ledger := newNudgeShadowLedger(now)
		if moved := deadLetterRetiredFencePlan(state, planned, ledger, now); moved != 1 {
			t.Fatalf("moved %d items, want 1 (only the unchanged fence)", moved)
		}
		if len(state.Pending) != 1 || state.Pending[0].ID != "n-moved" {
			t.Fatalf("pending = %v, want the re-fenced item", state.Pending)
		}
		if len(state.Dead) != 1 || state.Dead[0].ID != "n-same" || state.Dead[0].LastError != "session-terminated" {
			t.Fatalf("dead = %v, want n-same with session-terminated", state.Dead)
		}
	})
}
