package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// killHookProvider runs hooks around the runtime teardown so a test can observe
// the durable row, and act on it, at the instant the kill stops the runtime.
type killHookProvider struct {
	runtime.Provider
	beforeStop func()
	afterStop  func()
}

func (p *killHookProvider) Stop(name string) error {
	if p.beforeStop != nil {
		p.beforeStop()
	}
	err := p.Provider.Stop(name)
	if p.afterStop != nil {
		p.afterStop()
	}
	return err
}

// wrapKillPokeProvider wraps the provider newKillPokeSession installed with a
// killHookProvider and returns the inner provider (the one a racing controller
// would observe directly).
func wrapKillPokeProvider(t *testing.T, hooks *killHookProvider) runtime.Provider {
	t.Helper()
	oldBuild := buildSessionProviderByName
	inner, err := oldBuild(nil, "", config.SessionConfig{}, "", "")
	if err != nil {
		t.Fatalf("building fixture provider: %v", err)
	}
	hooks.Provider = inner
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return hooks, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	return inner
}

func stubKillPoke(t *testing.T) {
	t.Helper()
	old := sessionKillPokeController
	sessionKillPokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() { sessionKillPokeController = old })
}

func setKillFixtureMetadata(t *testing.T, store beads.Store, id string, kvs map[string]string) {
	t.Helper()
	if err := store.SetMetadataBatch(id, kvs); err != nil {
		t.Fatalf("SetMetadataBatch(%s): %v", id, err)
	}
}

// TestCmdSessionKill_RecordsKillIntentBeforeStoppingRuntime pins the ordering:
// when the runtime is torn down, the row must already say asleep/killed and
// carry the kill fence, so no observer can see a row claiming a runtime that is
// already gone. After the kill the fence is lifted and the asleep intent stays.
func TestCmdSessionKill_RecordsKillIntentBeforeStoppingRuntime(t *testing.T) {
	const identity = killPokeSessionIdentity
	store, bead, _ := newKillPokeSession(t, "s-gc-kill-order")
	stubKillPoke(t)

	var atStop beads.Bead
	stops := 0
	wrapKillPokeProvider(t, &killHookProvider{beforeStop: func() {
		stops++
		atStop = mustGetBead(t, store, bead.ID)
	}})

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stops != 1 {
		t.Fatalf("runtime teardowns = %d, want 1", stops)
	}
	if got := atStop.Metadata["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("state at teardown = %q, want asleep", got)
	}
	if got := atStop.Metadata["sleep_reason"]; got != string(sessionpkg.SleepReasonKilled) {
		t.Errorf("sleep_reason at teardown = %q, want killed", got)
	}
	if got := atStop.Metadata["state_reason"]; got != sessionpkg.KillPendingReason {
		t.Errorf("state_reason at teardown = %q, want %q", got, sessionpkg.KillPendingReason)
	}

	final := mustGetBead(t, store, bead.ID)
	if got := final.Metadata["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("final state = %q, want asleep", got)
	}
	if got := final.Metadata["sleep_reason"]; got != string(sessionpkg.SleepReasonKilled) {
		t.Errorf("final sleep_reason = %q, want killed", got)
	}
	if got := final.Metadata["state_reason"]; got != "" {
		t.Errorf("final state_reason = %q, want the kill fence lifted", got)
	}
	if final.Metadata["synced_at"] == "" {
		t.Error("final synced_at is empty, want the kill's sync stamp")
	}
}

// TestCmdSessionKill_ReconcileBetweenIntentAndTeardownLeavesKilledRowAlone is
// the regression for the kill-ordering race. A controller tick runs at both
// edges of the runtime teardown. Before the fix the kill stopped the runtime
// first, so the tick after the teardown saw a row claiming awake next to a
// dead runtime and treated it as a crash: it wiped the conversation key and
// marked a continuation reset (desired), or closed the bead mid-kill
// (undesired). The tick before the teardown is the reverse window the fence
// covers: an asleep row next to a still-live runtime must not be healed back
// to awake, which would re-open the same race once the Stop lands.
func TestCmdSessionKill_ReconcileBetweenIntentAndTeardownLeavesKilledRowAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired bool
	}{
		{name: "desired", desired: true},
		{name: "undesired", desired: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const identity = killPokeSessionIdentity
			const sessionName = "s-gc-kill-interleave"
			store, bead, cityDir := newKillPokeSession(t, sessionName)
			stubKillPoke(t)
			setKillFixtureMetadata(t, store, bead.ID, map[string]string{
				"session_key":         "conversation-1",
				"started_config_hash": "config-1",
				"last_woke_at":        time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			})
			cfg, err := loadCityConfig(cityDir, io.Discard)
			if err != nil {
				t.Fatalf("loadCityConfig: %v", err)
			}
			desiredState := map[string]TemplateParams{}
			if tc.desired {
				desiredState[sessionName] = TemplateParams{Command: "true", SessionName: sessionName, TemplateName: identity}
			}

			hooks := &killHookProvider{}
			inner := wrapKillPokeProvider(t, hooks)
			var tickLog bytes.Buffer
			tick := func(edge string) {
				current := mustGetBead(t, store, bead.ID)
				if current.Status == "closed" {
					return
				}
				stateBefore := current.Metadata["state"]
				reconcileSessionBeads(
					context.Background(), []beads.Bead{current}, desiredState,
					configuredSessionNames(cfg, "", store), cfg, inner, store,
					nil, nil, nil, newDrainTracker(), map[string]int{identity: 1}, false, nil, "",
					nil, clock.Real{}, events.Discard, 0, 0, &tickLog, &tickLog,
					withStartStabilityWaiter(immediateStartStabilityWaiter),
					withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
				)
				after := mustGetBead(t, store, bead.ID)
				if got := after.Metadata["state"]; stateBefore == string(sessionpkg.StateAsleep) && got != stateBefore {
					t.Errorf("tick %s teardown moved the killed row from asleep to %q (closed=%v)", edge, got, after.Status == "closed")
				}
			}
			hooks.beforeStop = func() { tick("before") }
			hooks.afterStop = func() { tick("after") }

			var stdout, stderr bytes.Buffer
			if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
			}

			final := mustGetBead(t, store, bead.ID)
			if final.Status == "closed" {
				t.Fatalf("killed session bead was closed by the racing tick (close_reason=%q); tick log:\n%s", final.Metadata["close_reason"], tickLog.String())
			}
			if got := final.Metadata["session_key"]; got != "conversation-1" {
				t.Errorf("session_key = %q, want conversation-1 kept (a racing tick treated the kill as a crash); tick log:\n%s", got, tickLog.String())
			}
			if got := final.Metadata["continuation_reset_pending"]; got != "" {
				t.Errorf("continuation_reset_pending = %q, want empty (a racing tick treated the kill as a crash)", got)
			}
			if got := final.Metadata["state"]; got != string(sessionpkg.StateAsleep) {
				t.Errorf("final state = %q, want asleep", got)
			}
			if got := final.Metadata["sleep_reason"]; got != string(sessionpkg.SleepReasonKilled) {
				t.Errorf("final sleep_reason = %q, want killed", got)
			}
			if got := final.Metadata["state_reason"]; got != "" {
				t.Errorf("final state_reason = %q, want the kill fence lifted", got)
			}
			if inner.IsRunning(sessionName) {
				t.Errorf("runtime %q running after the kill; a racing tick restarted it mid-kill", sessionName)
			}
		})
	}
}

// TestCmdSessionKill_StopFailureRestoresRow defines the recovery for a stop
// that fails after the intent is durable: the runtime is still alive, so the
// row must not claim otherwise. The kill fails and every key the fence wrote
// goes back to its pre-kill value.
func TestCmdSessionKill_StopFailureRestoresRow(t *testing.T) {
	const identity = killPokeSessionIdentity
	const sessionName = "s-gc-kill-stop-fails"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)
	setKillFixtureMetadata(t, store, bead.ID, map[string]string{
		"last_woke_at": "2026-09-01T10:00:00Z",
		"state_reason": "creation_complete",
	})
	before := mustGetBead(t, store, bead.ID)

	hooks := &killHookProvider{}
	inner := wrapKillPokeProvider(t, hooks)
	fake, ok := inner.(*runtime.Fake)
	if !ok {
		t.Fatalf("fixture provider = %T, want *runtime.Fake", inner)
	}
	fake.StopErrors[sessionName] = errors.New("tmux: kill-session refused")

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionKill = %d, want 1 when the runtime survives; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "kill-session refused") {
		t.Errorf("stderr = %q, want the stop error", stderr.String())
	}
	if !fake.IsRunning(sessionName) {
		t.Fatalf("test premise: runtime should still be running after the failed stop")
	}

	after := mustGetBead(t, store, bead.ID)
	for key := range sessionpkg.KillPendingPatch(time.Now()) {
		if got, want := after.Metadata[key], before.Metadata[key]; got != want {
			t.Errorf("after failed kill %s = %q, want pre-kill %q", key, got, want)
		}
	}
	if got, want := after.Metadata["synced_at"], before.Metadata["synced_at"]; got != want {
		t.Errorf("after failed kill synced_at = %q, want pre-kill %q", got, want)
	}
}

// TestCmdSessionKill_StopFailureRollbackLeavesNewerWriteAlone pins the rollback
// fence: if something else rewrote the row after the kill fence landed, the
// failed kill must not stomp that newer state with its pre-kill snapshot.
func TestCmdSessionKill_StopFailureRollbackLeavesNewerWriteAlone(t *testing.T) {
	const identity = killPokeSessionIdentity
	const sessionName = "s-gc-kill-rollback-superseded"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)

	hooks := &killHookProvider{}
	inner := wrapKillPokeProvider(t, hooks)
	fake := inner.(*runtime.Fake)
	fake.StopErrors[sessionName] = errors.New("stop failed")
	hooks.beforeStop = func() {
		// The extra key grows the file so the kill's own store handle cannot
		// miss this write on the file store's mtime+size read fast path.
		setKillFixtureMetadata(t, store, bead.ID, map[string]string{
			"state":             string(sessionpkg.StateActive),
			"state_reason":      "creation_complete",
			"sleep_reason":      "",
			"test_newer_writer": "a write that landed after the kill fence",
		})
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionKill = %d, want 1; stderr=%s", code, stderr.String())
	}
	after := mustGetBead(t, store, bead.ID)
	if got := after.Metadata["state"]; got != string(sessionpkg.StateActive) {
		t.Errorf("state = %q, want the newer active write kept", got)
	}
	if got := after.Metadata["state_reason"]; got != "creation_complete" {
		t.Errorf("state_reason = %q, want the newer write kept", got)
	}
	if !strings.Contains(stderr.String(), "restoring session") {
		t.Errorf("stderr = %q, want a warning that the rollback was skipped", stderr.String())
	}
}

// TestCmdSessionKill_StopErrorWithRuntimeGoneSucceeds: a stop that reports an
// error but leaves no runtime behind achieved what the kill is for.
func TestCmdSessionKill_StopErrorWithRuntimeGoneSucceeds(t *testing.T) {
	const identity = killPokeSessionIdentity
	const sessionName = "s-gc-kill-stop-err-gone"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)

	hooks := &killHookProvider{}
	inner := wrapKillPokeProvider(t, hooks)
	fake := inner.(*runtime.Fake)
	hooks.beforeStop = func() {
		// The runtime dies on its own and the provider then reports the
		// teardown as an error.
		delete(fake.StopErrors, sessionName)
		_ = fake.Stop(sessionName)
		fake.StopErrors[sessionName] = errors.New("no such session")
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0 when the runtime is gone; stderr=%s", code, stderr.String())
	}
	final := mustGetBead(t, store, bead.ID)
	if got := final.Metadata["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("final state = %q, want asleep", got)
	}
	if got := final.Metadata["state_reason"]; got != "" {
		t.Errorf("final state_reason = %q, want the kill fence lifted", got)
	}
}

// TestCmdSessionKill_AwakeRowWithDeadRuntimeStillSucceeds guards the #3629 case
// under the new order: the row says awake but the runtime is already gone.
// With the fence written first, Manager.Kill reads an asleep row and reports
// "not active"; the kill must still succeed and leave the row asleep.
func TestCmdSessionKill_AwakeRowWithDeadRuntimeStillSucceeds(t *testing.T) {
	const identity = killPokeSessionIdentity
	const sessionName = "s-gc-kill-stale-awake"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)

	hooks := &killHookProvider{}
	inner := wrapKillPokeProvider(t, hooks)
	if err := inner.Stop(sessionName); err != nil {
		t.Fatalf("pre-stopping runtime: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}
	final := mustGetBead(t, store, bead.ID)
	if got := final.Metadata["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("final state = %q, want asleep", got)
	}
	if got := final.Metadata["sleep_reason"]; got != string(sessionpkg.SleepReasonKilled) {
		t.Errorf("final sleep_reason = %q, want killed", got)
	}
	if got := final.Metadata["state_reason"]; got != "" {
		t.Errorf("final state_reason = %q, want the kill fence lifted", got)
	}
}

// openConditionalKillFenceStore opens a store whose conditional writer
// resolves, so the fence writes take the revision-fenced path.
func openConditionalKillFenceStore(t *testing.T) beads.Store {
	t.Helper()
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Require,
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	if writer, _, err := beads.ResolveConditionalWriter(result.Store); err != nil || writer == nil {
		t.Fatalf("test premise: conditional writer did not resolve (err=%v)", err)
	}
	return result.Store
}

func createKillFenceSessionBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "session",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-fence",
			"state":        string(sessionpkg.StateAwake),
			"state_reason": "creation_complete",
			"last_woke_at": "2026-09-01T10:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	return b
}

// TestSessionKillFence_ConditionalStoreLifecycle drives write, rollback, and
// clear through the revision-fenced path.
func TestSessionKillFence_ConditionalStoreLifecycle(t *testing.T) {
	store := openConditionalKillFenceStore(t)
	b := createKillFenceSessionBead(t, store)

	fence, err := writeSessionKillFence(store, b.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("writeSessionKillFence: %v", err)
	}
	fenced := mustGetBead(t, store, b.ID)
	if !fence.owns(fenced) {
		t.Fatalf("fence does not own the row it wrote: %#v", fenced.Metadata)
	}
	if err := fence.rollback(store); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	restored := mustGetBead(t, store, b.ID)
	if got := restored.Metadata["state"]; got != string(sessionpkg.StateAwake) {
		t.Errorf("restored state = %q, want awake", got)
	}
	if got := restored.Metadata["state_reason"]; got != "creation_complete" {
		t.Errorf("restored state_reason = %q, want creation_complete", got)
	}
	if got := restored.Metadata["last_woke_at"]; got != "2026-09-01T10:00:00Z" {
		t.Errorf("restored last_woke_at = %q, want the pre-kill value", got)
	}
	if err := fence.clear(store); !errors.Is(err, errSessionKillFenceSuperseded) {
		t.Fatalf("clear after rollback = %v, want errSessionKillFenceSuperseded", err)
	}

	fence, err = writeSessionKillFence(store, b.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("second writeSessionKillFence: %v", err)
	}
	if err := fence.clear(store); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared := mustGetBead(t, store, b.ID)
	if got := cleared.Metadata["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("cleared state = %q, want asleep", got)
	}
	if got := cleared.Metadata["state_reason"]; got != "" {
		t.Errorf("cleared state_reason = %q, want empty", got)
	}
}

// TestApplySessionKillFencePatch_RedecidesAfterConcurrentWrite pins the
// fenced write: a write that lands between the read and the conditional
// update fails the revision check, and the helper re-reads and re-decides on
// the fresh row instead of writing a decision based on a stale one.
func TestApplySessionKillFencePatch_RedecidesAfterConcurrentWrite(t *testing.T) {
	store := openConditionalKillFenceStore(t)
	b := createKillFenceSessionBead(t, store)

	var observedStates []string
	err := applySessionKillFencePatch(store, b.ID, func(cur beads.Bead) (map[string]string, bool) {
		observedStates = append(observedStates, cur.Metadata["state"])
		if len(observedStates) == 1 {
			// A concurrent writer lands after this observation.
			if err := store.SetMetadata(b.ID, "state", string(sessionpkg.StateActive)); err != nil {
				t.Fatalf("concurrent SetMetadata: %v", err)
			}
		}
		return map[string]string{"sleep_reason": "observed-" + cur.Metadata["state"]}, true
	})
	if err != nil {
		t.Fatalf("applySessionKillFencePatch: %v", err)
	}
	if len(observedStates) != 2 {
		t.Fatalf("decide calls = %d (%v), want 2: the stale decision must be retried", len(observedStates), observedStates)
	}
	if got := mustGetBead(t, store, b.ID).Metadata["sleep_reason"]; got != "observed-active" {
		t.Errorf("sleep_reason = %q, want the decision made on the fresh row", got)
	}
}

// pendingWake is a pending explicit wake request (CONTRACT v5.7 D7).
var pendingWake = map[string]string{"wake_request": "explicit", "wake_requested_at": "2026-10-08T11:00:00Z"}

// TestCmdSessionKill_ClearsPendingWake is D7 rule 3: the kill supersedes a
// pending wake in its own write, through the kill fence or, for a runtime
// already gone, the post-kill sync. A stale wake would keep a killed idle seat
// wanted. Kills either write dropping the clear.
func TestCmdSessionKill_ClearsPendingWake(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stopped bool
	}{{name: "fenced"}, {name: "runtime already gone", stopped: true}} {
		t.Run(tc.name, func(t *testing.T) {
			const sessionName = "s-gc-kill-wake"
			store, bead, _ := newKillPokeSession(t, sessionName)
			stubKillPoke(t)
			setKillFixtureMetadata(t, store, bead.ID, pendingWake)
			inner := wrapKillPokeProvider(t, &killHookProvider{})
			if tc.stopped {
				setKillFixtureMetadata(t, store, bead.ID, map[string]string{"state": string(sessionpkg.StateAsleep)})
				if err := inner.Stop(sessionName); err != nil {
					t.Fatalf("pre-stopping runtime: %v", err)
				}
			}

			var stdout, stderr bytes.Buffer
			if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
			}
			final := mustGetBead(t, store, bead.ID)
			for key := range pendingWake {
				if got := final.Metadata[key]; got != "" {
					t.Errorf("after kill %s = %q, want cleared", key, got)
				}
			}
		})
	}
}

// TestCmdSessionKill_StopFailureRestoresPendingWake: the wake clear rides in
// the kill-fence patch, so a failed kill's rollback restores the wake with
// the rest of the row. Kills a separate clear write the rollback misses.
func TestCmdSessionKill_StopFailureRestoresPendingWake(t *testing.T) {
	const sessionName = "s-gc-kill-wake-restore"
	store, bead, _ := newKillPokeSession(t, sessionName)
	stubKillPoke(t)
	setKillFixtureMetadata(t, store, bead.ID, pendingWake)
	var atStop beads.Bead
	hooks := &killHookProvider{}
	hooks.beforeStop = func() { atStop = mustGetBead(t, store, bead.ID) }
	fake := wrapKillPokeProvider(t, hooks).(*runtime.Fake)
	fake.StopErrors[sessionName] = errors.New("tmux: kill-session refused")

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionKill = %d, want 1 when the runtime survives; stderr=%s", code, stderr.String())
	}
	final := mustGetBead(t, store, bead.ID)
	for key, want := range pendingWake {
		if got := atStop.Metadata[key]; got != "" {
			t.Errorf("under the fence %s = %q, want cleared", key, got)
		}
		if got := final.Metadata[key]; got != want {
			t.Errorf("after rollback %s = %q, want %q restored", key, got, want)
		}
	}
}

// TestCmdSessionSuspend_ManagedClearsPendingWake is D7 rule 3 for the
// managed `gc session suspend`, which writes the hold and leaves the stop to
// the controller. Kills the call site writing a patch without the clear.
func TestCmdSessionSuspend_ManagedClearsPendingWake(t *testing.T) {
	store, bead, _ := newKillPokeSession(t, "s-gc-suspend-wake")
	setKillFixtureMetadata(t, store, bead.ID, pendingWake)
	oldManaged, oldPoke, oldEnqueue := sessionSuspendManagedReconciler, sessionSuspendPokeController, sessionSuspendEnqueueController
	sessionSuspendManagedReconciler = func(string) bool { return true }
	sessionSuspendPokeController = func(string) error { return nil }
	sessionSuspendEnqueueController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() {
		sessionSuspendManagedReconciler, sessionSuspendPokeController, sessionSuspendEnqueueController = oldManaged, oldPoke, oldEnqueue
	})

	var stdout, stderr bytes.Buffer
	if code := cmdSessionSuspend([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionSuspend = %d, want 0; stderr=%s", code, stderr.String())
	}
	final := mustGetBead(t, store, bead.ID)
	if final.Metadata["state"] != "suspended" || final.Metadata["sleep_intent"] != "user-hold" || final.Metadata["held_until"] == "" {
		t.Fatalf("managed suspend left %v, want the hold", final.Metadata)
	}
	for key := range pendingWake {
		if got := final.Metadata[key]; got != "" {
			t.Errorf("after managed suspend %s = %q, want cleared", key, got)
		}
	}
}

// TestCmdSessionKill_ClearsAUserHoldsTimer: a kill clears a user-hold
// intent, and with it the operator's held_until, so no heartbeat-shaped hold
// is left; a heartbeat hold (no intent) keeps its timer, and a failed kill's
// rollback restores both.
func TestCmdSessionKill_ClearsAUserHoldsTimer(t *testing.T) {
	heldUntil := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for name, tc := range map[string]struct {
		intent   string
		stopFail bool
		want     string
	}{
		"user hold":             {intent: "user-hold", want: ""},
		"heartbeat":             {want: heldUntil},
		"user hold, stop fails": {intent: "user-hold", stopFail: true, want: heldUntil},
	} {
		t.Run(name, func(t *testing.T) {
			sessionName := "s-gc-kill-hold"
			store, bead, _ := newKillPokeSession(t, sessionName)
			stubKillPoke(t)
			setKillFixtureMetadata(t, store, bead.ID, map[string]string{"held_until": heldUntil, "sleep_intent": tc.intent})
			fake := wrapKillPokeProvider(t, &killHookProvider{}).(*runtime.Fake)
			if tc.stopFail {
				fake.StopErrors[sessionName] = errors.New("tmux: kill-session refused")
			}
			var stdout, stderr bytes.Buffer
			_ = cmdSessionKill([]string{killPokeSessionIdentity}, &stdout, &stderr)
			final := mustGetBead(t, store, bead.ID)
			if got := final.Metadata["held_until"]; got != tc.want {
				t.Fatalf("held_until = %q, want %q; stderr=%s", got, tc.want, stderr.String())
			}
			if tc.stopFail && final.Metadata["sleep_intent"] != tc.intent {
				t.Fatalf("rollback sleep_intent = %q, want %q", final.Metadata["sleep_intent"], tc.intent)
			}
		})
	}
}
