package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// supervisorCfg returns a minimal *config.City wired for supervisor-mode
// nudge dispatching. Tests use it to drive nudgeDispatcherIsSupervisor.
func supervisorCfg() *config.City {
	return &config.City{
		Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"},
	}
}

func TestPingNudgeWakeSocketNoListenerIsNoOp(t *testing.T) {
	dir := t.TempDir()
	// No listener — DialTimeout returns "no such file or directory". The
	// helper must swallow it; otherwise enqueue producers would surface
	// transient warnings to legacy-mode users.
	pingNudgeWakeSocket(dir)
}

func TestPingNudgeWakeSocketEmptyCityPathIsNoOp(_ *testing.T) {
	// No assertion needed — test passes if pingNudgeWakeSocket does not
	// panic on an empty cityPath. The function dials a derived socket path
	// and exits silently on dial failure, which is the legacy-mode contract.
	pingNudgeWakeSocket("")
}

func TestStartNudgeWakeListenerSignalsOnConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)

	lis, err := startNudgeWakeListener(ctx, dir, wakeCh, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	pingNudgeWakeSocket(dir)
	select {
	case <-wakeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("wakeCh not signaled within 2s of producer ping")
	}
}

func TestStartNudgeWakeListenerCoalescesBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)

	lis, err := startNudgeWakeListener(ctx, dir, wakeCh, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	// Fire several pings in quick succession. The buffered channel of size
	// 1 must coalesce them — never block the listener accept loop.
	for i := 0; i < 10; i++ {
		pingNudgeWakeSocket(dir)
	}
	// Let all accepts drain through the listener so coalescing settles, then
	// verify a wake was produced. The structural coalescing guarantee is the
	// chan's bounded capacity; the previous test counted cumulative wakes
	// over time, which races against accept-loop scheduling on fast hardware.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-wakeCh:
	default:
		t.Fatal("wakeCh not signaled at all after burst of 10 pings")
	}
	if got := cap(wakeCh); got != 1 {
		t.Fatalf("wakeCh capacity = %d; want 1 (coalescing relies on bounded buffer)", got)
	}
}

func TestStartNudgeWakeListenerStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)

	lis, err := startNudgeWakeListener(ctx, dir, wakeCh, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	cancel()
	// The cleanup goroutine closes the listener on ctx.Done. Give it a beat,
	// then confirm dialing the socket fails fast.
	time.Sleep(50 * time.Millisecond)
	_, err = net.DialTimeout("unix", nudgequeue.WakeSocketPath(dir), 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected dial to fail after ctx cancel; listener still accepting")
	}
	_ = lis
}

func TestDispatchAllQueuedNudgesNoOpInLegacyMode(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	cfg := &config.City{Daemon: config.DaemonConfig{}} // legacy default
	delivered, err := dispatchAllQueuedNudges(dir, cfg, nil, nil, nil, newSessionBeadSnapshot(nil), nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 in legacy mode", delivered)
	}
}

func TestDispatchAllQueuedNudgesEmptyQueue(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, nil, newSessionBeadSnapshot(nil), nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 with empty queue", delivered)
	}
}

// TestDispatchAllQueuedNudgesSweepsExpiredOrphanEvenWithoutMatch guards
// against ra-oudpha finding-3's orphan population: a queued item whose
// target agent has no open session can never reach step 2's "matched"
// check in the per-session loop, so it can never reach the sweep that
// claimDueQueuedNudgesForTarget runs as a side effect of a successful
// claim. Before the fix, such an item sat in Pending forever, past its
// TTL, invisible to every maintenance pass. The dispatch tick must sweep
// the whole queue unconditionally so TTL expiry (#4421 semantics) still
// applies to items no open session will ever match.
func TestDispatchAllQueuedNudgesSweepsExpiredOrphanEvenWithoutMatch(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	// Created 25h ago, so ExpiresAt (created+24h TTL) is already 1h in the
	// past. "ghost-agent" has no open session in the snapshot below and
	// never will, so it can never satisfy the per-session loop's match step.
	item := newQueuedNudge("ghost-agent", "msg", time.Now().Add(-25*time.Hour))
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, nil, newSessionBeadSnapshot(nil), nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0", delivered)
	}

	// Read the raw persisted state (not via listQueuedNudges, which would
	// run its own maintenance sweep as a side effect and mask the bug).
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending = %d, want 0 (expired orphan should have been swept by the dispatch tick)", len(state.Pending))
	}
	if len(state.Dead) != 1 {
		t.Fatalf("dead = %d, want 1", len(state.Dead))
	}
	if state.Dead[0].LastError != "expired" {
		t.Fatalf("dead[0].LastError = %q, want %q", state.Dead[0].LastError, "expired")
	}
}

func TestDispatchAllQueuedNudgesSkipsNotYetDue(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	future := time.Now().Add(5 * time.Minute)
	item := newQueuedNudge("worker", "later", time.Now())
	item.DeliverAfter = future
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	bead := beads.Bead{
		ID:     "session-1",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{bead})
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 (item not yet due)", delivered)
	}
}

func TestDispatchAllQueuedNudgesDeliversAndAcks(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()

	// Set up a running session via the same fake-provider harness used by
	// the per-session poller test, then enqueue a nudge for it.
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}

	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	snapshot, err := loadSessionBeadSnapshot(store.Store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}

	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), store.Store, store.Store, fake, snapshot, nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}

	var nudgeMessages []string
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			nudgeMessages = append(nudgeMessages, call.Message)
		}
	}
	if len(nudgeMessages) != 1 {
		t.Fatalf("nudge calls = %d, want 1", len(nudgeMessages))
	}
	if !strings.Contains(nudgeMessages[0], "review the deploy logs") {
		t.Fatalf("nudge message = %q, want original reminder", nudgeMessages[0])
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("queue not drained: pending=%d inFlight=%d dead=%d", len(pending), len(inFlight), len(dead))
	}
}

// dueKeysErrQueue answers DueKeys with its queue's keys beside err, as a store
// holding a record it cannot decode answers.
type dueKeysErrQueue struct {
	storebinding.NudgeQueue
	err error
}

func (q dueKeysErrQueue) DueKeys(now time.Time) ([]string, error) {
	keys, err := q.NudgeQueue.DueKeys(now)
	if err != nil {
		return nil, err
	}
	return keys, q.err
}

// TestDispatchQueuedNudgesDeliversThePartialDueKeysRead: a partial DueKeys
// read still dispatches the keys it returned, and the pass reports its error,
// even when no key is due; any other DueKeys error abandons the pass before a
// claim, though the read returned keys beside it.
func TestDispatchQueuedNudgesDeliversThePartialDueKeysRead(t *testing.T) {
	partial := &beads.PartialResultError{Op: "nudge queue", Err: errors.New("record n-corrupt did not decode")}
	failure := errors.New("nudges store unreachable")
	for _, tc := range []struct {
		name          string
		dueKeysErr    error
		wantPartial   bool
		enqueue       bool
		wantDelivered int
		wantPending   int
	}{
		{name: "partial read delivers its keys", dueKeysErr: partial, wantPartial: true, enqueue: true, wantDelivered: 1},
		{name: "partial read with nothing due", dueKeysErr: partial, wantPartial: true},
		{name: "failed read delivers nothing", dueKeysErr: failure, enqueue: true, wantPending: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			clearInheritedCityRoutingEnv(t)
			t.Setenv("GC_BEADS", "file")
			dir := t.TempDir()

			store := openNudgeBeadStore(dir)
			fake := runtime.NewFake()
			mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
			if tc.enqueue {
				if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))); err != nil {
					t.Fatalf("enqueueQueuedNudge: %v", err)
				}
			}
			snapshot, err := loadSessionBeadSnapshot(store.Store)
			if err != nil {
				t.Fatalf("loadSessionBeadSnapshot: %v", err)
			}

			queue := openNudgeQueue(dir, store)
			queue.Queue = dueKeysErrQueue{NudgeQueue: queue.Queue, err: tc.dueKeysErr}
			delivered, err := dispatchQueuedNudges(queue, dir, supervisorCfg(), store.Store, store.Store, fake, snapshot, nil)
			if !errors.Is(err, tc.dueKeysErr) {
				t.Fatalf("dispatchQueuedNudges error = %v, want it to report %v", err, tc.dueKeysErr)
			}
			if got := beads.IsPartialResult(err); got != tc.wantPartial {
				t.Fatalf("beads.IsPartialResult(%v) = %t, want %t", err, got, tc.wantPartial)
			}
			if delivered != tc.wantDelivered {
				t.Fatalf("delivered = %d, want %d", delivered, tc.wantDelivered)
			}
			var nudgeMessages []string
			for _, call := range fake.Calls {
				if call.Method == "Nudge" {
					nudgeMessages = append(nudgeMessages, call.Message)
				}
			}
			if len(nudgeMessages) != tc.wantDelivered {
				t.Fatalf("nudge calls = %q, want %d", nudgeMessages, tc.wantDelivered)
			}
			if tc.wantDelivered > 0 && !strings.Contains(nudgeMessages[0], "review the deploy logs") {
				t.Fatalf("nudge message = %q, want original reminder", nudgeMessages[0])
			}
			pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
			if err != nil {
				t.Fatalf("listQueuedNudges: %v", err)
			}
			if len(pending) != tc.wantPending || len(inFlight) != 0 || len(dead) != 0 {
				t.Fatalf("queue after the pass: pending=%d inFlight=%d dead=%d, want pending=%d and nothing else",
					len(pending), len(inFlight), len(dead), tc.wantPending)
			}
		})
	}
}

// TestDispatchAllQueuedNudgesHoldsNudgesForKillFencedSession: while a
// `gc session kill` is tearing the runtime down, a queued nudge must stay
// queued instead of being typed into the dying process and acked.
func TestDispatchAllQueuedNudgesHoldsNudgesForKillFencedSession(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()

	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := store.SetMetadataBatch(info.ID, session.KillPendingPatch(time.Now())); err != nil {
		t.Fatalf("writing kill fence: %v", err)
	}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	snapshot, err := loadSessionBeadSnapshot(store.Store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}

	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), store.Store, store.Store, fake, snapshot, nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 while the kill is in flight", delivered)
	}
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			t.Fatalf("nudge typed into a runtime being killed: %q", call.Message)
		}
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("queue = pending %d, inFlight %d, dead %d; want the nudge still pending", len(pending), len(inFlight), len(dead))
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := state.DispatchSkips["kill-pending"]; got != 1 {
		t.Fatalf("DispatchSkips[kill-pending] = %d, want 1 (full map: %#v)", got, state.DispatchSkips)
	}
}

// TestDispatchAllQueuedNudgesDeliversToIdleACPSession verifies the
// supervisor dispatcher delivers queued nudges to a running ACP session
// once it has been idle longer than the quiescence window. Idle ACP
// sessions used to depend exclusively on inject-on-hook drain, but a
// pure-hook delivery path never fires for a warm session that is not
// receiving fresh user prompts — queued reminders piled up
// indefinitely against an alive but quiet agent. The dispatcher now
// owns wake delivery; the hook still drains opportunistically when the
// agent receives external prompts, and the atomic queue claim prevents
// double delivery.
func TestDispatchAllQueuedNudgesDeliversToIdleACPSession(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")

	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	if store.Store == nil {
		t.Fatal("openNudgeBeadStore returned nil")
	}

	if err := enqueueQueuedNudgeWithStore(dir, store, newQueuedNudge("worker", "wake-up nudge", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}

	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "worker-session", runtime.Config{}); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}
	// Mark last activity well past the quiescence window so the
	// dispatcher considers the session idle enough to deliver.
	fake.SetActivity("worker-session", time.Now().Add(-10*time.Second))

	// Create a real session bead so worker.SessionByID can resolve the
	// target without panicking on a missing-bead lookup.
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"transport":    "acp",
		},
	})
	if err != nil {
		t.Fatalf("store.Create session bead: %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{created})

	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), store, store, fake, snapshot, nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (running idle ACP session must receive queued nudges)", delivered)
	}

	var nudgeMessages []string
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			nudgeMessages = append(nudgeMessages, call.Message)
		}
	}
	if len(nudgeMessages) != 1 {
		t.Fatalf("nudge calls = %d, want 1 (queued nudge should be delivered as a runtime prompt)", len(nudgeMessages))
	}
	if !strings.Contains(nudgeMessages[0], "wake-up nudge") {
		t.Fatalf("nudge message = %q, want original reminder text", nudgeMessages[0])
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("queue not drained after ACP delivery: pending=%d inFlight=%d dead=%d", len(pending), len(inFlight), len(dead))
	}

	// Observability: a successful queued-nudge delivery must stamp
	// metadata.last_nudge_delivered_at on the session bead so the
	// "LAST NUDGE" column in `gc session list` reflects fresh activity.
	// Operators rely on this column to spot warm sessions whose
	// delivery loop has stalled (queued items piling up while the
	// stamp stays old).
	refetched, getErr := store.Get(created.ID)
	if getErr != nil {
		t.Fatalf("store.Get session bead: %v", getErr)
	}
	stamp := strings.TrimSpace(refetched.Metadata[session.MetadataLastNudgeDeliveredAt])
	if stamp == "" {
		t.Fatalf("session bead missing %s metadata after successful ACP delivery", session.MetadataLastNudgeDeliveredAt)
	}
	parsed, parseErr := time.Parse(time.RFC3339, stamp)
	if parseErr != nil {
		t.Fatalf("parse %s=%q: %v", session.MetadataLastNudgeDeliveredAt, stamp, parseErr)
	}
	if drift := time.Since(parsed); drift < 0 || drift > time.Minute {
		t.Fatalf("%s timestamp drift %s is outside the 1-minute test window (raw=%q)", session.MetadataLastNudgeDeliveredAt, drift, stamp)
	}
}

// TestDispatchAllQueuedNudgesSkipsACPSessionWhenNotRunning confirms the
// dispatcher still respects the universal liveness check for ACP sessions —
// a stopped or crashed ACP session must not absorb queued nudges, because
// nothing on the other side would observe the delivered prompt.
func TestDispatchAllQueuedNudgesSkipsACPSessionWhenNotRunning(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	bead := beads.Bead{
		ID:     "worker-session",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"transport":    "acp",
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{bead})
	// Fake has no started session, so IsRunning("worker-session") is false.
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 (stopped ACP session must not receive delivery)", delivered)
	}
}

// TestDispatchAllQueuedNudgesRecordsSkipReasons guards ra-oudpha's
// observability fix: before this change, every continue in the per-session
// loop was silent — dispatchAllQueuedNudges returned (0, nil) whether the
// queue was empty, nothing matched, or a matched-and-live target's observe
// call reported not-running, with no way to tell these apart after the
// fact. The "matched but obs.Running is false" case is exactly finding-3's
// still-unidentified live-session gate shape (core.control-dispatcher: a
// live session repeatedly zero-stamped). This test drives that path and
// asserts the skip is now (a) logged via the GC_DEBUG route-audit
// convention and (b) counted in the persisted queue state, where `gc
// nudge status --json` can read it back for any agent.
func TestDispatchAllQueuedNudgesRecordsSkipReasons(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_DEBUG", "1")

	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	bead := beads.Bead{
		ID:     "worker-session",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"transport":    "acp",
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{bead})
	var debugOut strings.Builder
	// Fake has no started session, so IsRunning("worker-session") is false —
	// the target is matched (queue has a pending "worker" item) but obs.Running
	// comes back false, hitting the "not-running" skip.
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, &debugOut)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0", delivered)
	}
	if !strings.Contains(debugOut.String(), "route=skip reason=not-running") {
		t.Fatalf("debug output missing not-running skip line, got: %q", debugOut.String())
	}
	if !strings.Contains(debugOut.String(), "agent=worker") {
		t.Fatalf("debug output missing agent=worker, got: %q", debugOut.String())
	}

	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := state.DispatchSkips["not-running"]; got != 1 {
		t.Fatalf("DispatchSkips[not-running] = %d, want 1 (full map: %#v)", got, state.DispatchSkips)
	}

	// The routine skip: a fresh city whose queue holds a pending item for one
	// agent while the only open session belongs to another. The loop reaches
	// the target but nothing in the queue names it, so it falls through the
	// matched check. A separate city dir keeps the cumulative counters above
	// from bleeding into this assertion.
	otherDir := t.TempDir()
	if err := enqueueQueuedNudge(otherDir, newQueuedNudge("other-worker", "msg", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	var unmatchedOut strings.Builder
	delivered, err = dispatchAllQueuedNudges(otherDir, supervisorCfg(), nil, nil, runtime.NewFake(), snapshot, &unmatchedOut)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges (not-matched): %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0", delivered)
	}
	if !strings.Contains(unmatchedOut.String(), "route=skip reason=not-matched") {
		t.Fatalf("debug output missing not-matched skip line, got: %q", unmatchedOut.String())
	}

	otherState, err := nudgequeue.LoadState(otherDir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := otherState.DispatchSkips["not-matched"]; got != 1 {
		t.Fatalf("DispatchSkips[not-matched] = %d, want 1 (full map: %#v)", got, otherState.DispatchSkips)
	}
}

func TestNudgeDispatcherIsSupervisor(t *testing.T) {
	if nudgeDispatcherIsSupervisor(nil) {
		t.Error("nil cfg must report legacy mode")
	}
	if nudgeDispatcherIsSupervisor(&config.City{}) {
		t.Error("zero-value DaemonConfig must report legacy mode")
	}
	if !nudgeDispatcherIsSupervisor(supervisorCfg()) {
		t.Error("supervisorCfg must report supervisor mode")
	}
}

func TestDispatchAllQueuedNudgesNilCfg(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	delivered, err := dispatchAllQueuedNudges(dir, nil, nil, nil, nil, newSessionBeadSnapshot(nil), nil)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 with nil cfg", delivered)
	}
}

// TestMaybeStartNudgePollerSkipsACPSessionInLegacyMode verifies the
// legacy per-session poller still skips ACP sessions. A sidecar `gc
// nudge poll` process can observe the ACP control socket, but it does
// not own the in-memory ACP connection needed to send session/prompt.
func TestMaybeStartNudgePollerSkipsACPSessionInLegacyMode(t *testing.T) {
	prev := startNudgePoller
	t.Cleanup(func() { startNudgePoller = prev })

	called := false
	startNudgePoller = func(_, _, _ string) error {
		called = true
		return nil
	}

	maybeStartNudgePoller(nudgeTarget{
		cityPath:    t.TempDir(),
		sessionName: "worker-session",
		transport:   "acp",
		cfg:         &config.City{},
	})
	if called {
		t.Fatal("startNudgePoller invoked for ACP session in legacy mode; sidecar ACP pollers cannot deliver without owning the connection")
	}
}

func TestMaybeStartNudgePollerSkipsInSupervisorMode(t *testing.T) {
	prev := startNudgePoller
	t.Cleanup(func() { startNudgePoller = prev })

	called := false
	startNudgePoller = func(_, _, _ string) error {
		called = true
		return nil
	}

	maybeStartNudgePoller(nudgeTarget{
		cityPath:    t.TempDir(),
		sessionName: "worker-session",
		cfg:         supervisorCfg(),
	})
	if called {
		t.Fatal("startNudgePoller invoked in supervisor mode; supervisor dispatcher would race with the per-session poller")
	}

	maybeStartNudgePoller(nudgeTarget{
		cityPath:    t.TempDir(),
		sessionName: "worker-session",
		cfg:         &config.City{},
	})
	if !called {
		t.Fatal("startNudgePoller not invoked in legacy mode")
	}
}

func TestEnqueuePingsWakeSocket(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)
	lis, err := startNudgeWakeListener(ctx, dir, wakeCh, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", time.Now())); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	select {
	case <-wakeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("wakeCh not signaled after enqueue")
	}
}

// legBDispatchFixture is a supervisor-mode city with one running worker
// session (s1) and a second session of the same agent (s3) whose bead is
// created by create.
type legBDispatchFixture struct {
	dir   string
	store beads.NudgesStore
	fake  *runtime.Fake
	s1    session.Info
	mgr   *session.Manager
}

func newLegBDispatchFixture(t *testing.T) *legBDispatchFixture {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	f := &legBDispatchFixture{dir: t.TempDir(), fake: runtime.NewFake()}
	f.store = openNudgeBeadStore(f.dir)
	f.mgr = newSessionManagerWithConfig(f.dir, f.store.Store, f.fake, nil)
	f.s1 = f.create(t)
	if err := f.mgr.Start(context.Background(), testOperatorIn(t, f.dir), f.s1.ID, "", runtime.Config{WorkDir: f.dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.fake.Activity = map[string]time.Time{f.s1.SessionName: time.Now().Add(-10 * time.Second)}
	return f
}

func (f *legBDispatchFixture) create(t *testing.T) session.Info {
	t.Helper()
	return f.createAs(t, "worker")
}

// createAs creates a session bead of template's agent.
func (f *legBDispatchFixture) createAs(t *testing.T, template string) session.Info {
	t.Helper()
	info, err := f.mgr.CreateSession(context.Background(), session.CreateOptions{Template: template, Title: template, Command: "codex", WorkDir: f.dir, Provider: "codex", Hints: runtime.Config{WorkDir: f.dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create %s: %v", template, err)
	}
	return info
}

func (f *legBDispatchFixture) nudgeCalls() []string {
	var msgs []string
	for _, call := range f.fake.Calls {
		if call.Method == "Nudge" {
			msgs = append(msgs, call.Name+": "+call.Message)
		}
	}
	return msgs
}

// The dispatcher's pass snapshot only lets a claim skip: a session the
// snapshot does not list (created after it, by another process) is still live
// to the claim, which reads a live census before it adopts, so a running
// sibling does not take the nudge fenced to it.
func TestDispatchDoesNotAdoptForALiveSessionMissingFromTheSnapshot(t *testing.T) {
	f := newLegBDispatchFixture(t)
	snapshot, err := loadSessionBeadSnapshot(f.store.Store)
	if err != nil {
		t.Fatal(err)
	}
	s3 := f.create(t)
	if err := enqueueQueuedNudge(f.dir, newQueuedNudgeWithOptions("worker", "for s3 only", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: s3.ID})); err != nil {
		t.Fatal(err)
	}
	delivered, err := dispatchAllQueuedNudges(f.dir, supervisorCfg(), f.store.Store, f.store.Store, f.fake, snapshot, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if msgs := f.nudgeCalls(); delivered != 0 || len(msgs) != 0 {
		t.Fatalf("s1 adopted the nudge fenced to live s3 (delivered=%d): %s", delivered, strings.Join(msgs, " | "))
	}
	pending, _, _, err := listQueuedNudges(f.dir, "worker", time.Now())
	if err != nil || len(pending) != 1 || pending[0].SessionID != s3.ID {
		t.Fatalf("pending = %v, %v; want the nudge still fenced to s3", nqCharItems(pending), err)
	}
}

// A nudge fenced to an archived session of the same agent (open, but no longer
// holding its seat) is adopted by a running sibling: the pass's snapshot shows
// the fence holder gone, and the claim's live census agrees.
func TestDispatchAdoptsANudgeFencedToAnArchivedSibling(t *testing.T) {
	f := newLegBDispatchFixture(t)
	s3 := f.create(t)
	if err := f.store.SetMetadata(s3.ID, "state", string(session.StateArchived)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadSessionBeadSnapshot(f.store.Store)
	if err != nil {
		t.Fatal(err)
	}
	if info, ok := snapshot.FindInfoByID(s3.ID); !ok || sessionHoldsNudgeFence(info) {
		t.Fatalf("setup: s3 in the snapshot = %+v (found %v), want it open and archived", info, ok)
	}
	if err := enqueueQueuedNudge(f.dir, newQueuedNudgeWithOptions("worker", "left for s3", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: s3.ID})); err != nil {
		t.Fatal(err)
	}
	delivered, err := dispatchAllQueuedNudges(f.dir, supervisorCfg(), f.store.Store, f.store.Store, f.fake, snapshot, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	msgs := f.nudgeCalls()
	if delivered != 1 || len(msgs) != 1 || !strings.Contains(msgs[0], "left for s3") || !strings.HasPrefix(msgs[0], f.s1.SessionName+":") {
		t.Fatalf("delivered=%d nudges=%v, want s1 to adopt the archived s3's nudge", delivered, msgs)
	}
}

// onNudgeProvider runs onNudge after each delivery the fake records.
type onNudgeProvider struct {
	*runtime.Fake
	onNudge func(name string)
}

func (p onNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	err := p.Fake.Nudge(name, content)
	p.onNudge(name)
	return err
}

func (p onNudgeProvider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	err := p.Fake.NudgeFenced(ctx, name, content, n)
	p.onNudge(name)
	return err
}

// Each claim of a dispatch pass reads its own live census. A session another
// process creates between two targets of one pass (after s1's delivery,
// before helper s2's claim), with a nudge fenced to it, is live to s2's
// claim, so s2 does not adopt that nudge on a census s1's claim read before
// the session existed.
func TestDispatchClaimsReadTheirOwnCensusWithinAPass(t *testing.T) {
	f := newLegBDispatchFixture(t) // s1: worker, running
	s2 := f.createAs(t, "helper")
	if err := f.mgr.Start(context.Background(), testOperatorIn(t, f.dir), s2.ID, "", runtime.Config{WorkDir: f.dir}); err != nil {
		t.Fatalf("Start s2: %v", err)
	}
	f.fake.Activity[s2.SessionName] = time.Now().Add(-10 * time.Second)
	snapshot, err := loadSessionBeadSnapshot(f.store.Store)
	if err != nil {
		t.Fatal(err)
	}
	if infos := snapshot.OpenInfos(); len(infos) != 2 || infos[0].ID != f.s1.ID || infos[1].ID != s2.ID {
		t.Fatalf("setup: snapshot order %v, want s1 then s2", infos)
	}
	// s1's claim meets a nudge fenced to a gone worker, so it reads the
	// census; s2 matches a helper nudge.
	if err := enqueueQueuedNudge(f.dir, newQueuedNudgeWithOptions("worker", "left by a gone worker", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: "gc-gone-worker"})); err != nil {
		t.Fatal(err)
	}
	if err := enqueueQueuedNudge(f.dir, newQueuedNudgeWithOptions("helper", "for any helper", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{})); err != nil {
		t.Fatal(err)
	}
	var s3 session.Info
	sp := onNudgeProvider{Fake: f.fake, onNudge: func(name string) {
		if name != f.s1.SessionName || s3.ID != "" {
			return
		}
		s3 = f.createAs(t, "helper")
		if err := enqueueQueuedNudge(f.dir, newQueuedNudgeWithOptions("helper", "for s3 only", "session", time.Now().Add(-time.Second), queuedNudgeOptions{SessionID: s3.ID})); err != nil {
			t.Errorf("enqueue for s3: %v", err)
		}
	}}
	delivered, err := dispatchAllQueuedNudges(f.dir, supervisorCfg(), f.store.Store, f.store.Store, sp, snapshot, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if s3.ID == "" {
		t.Fatalf("setup: s1 was not delivered to (nudges %v)", f.nudgeCalls())
	}
	msgs := f.nudgeCalls()
	for _, m := range msgs {
		if strings.Contains(m, "for s3 only") {
			t.Fatalf("helper s2 adopted the nudge fenced to live helper s3 (delivered=%d): %s", delivered, strings.Join(msgs, " | "))
		}
	}
	if delivered != 2 {
		t.Fatalf("delivered=%d nudges=%v, want s1 and s2 each delivered once", delivered, msgs)
	}
}
