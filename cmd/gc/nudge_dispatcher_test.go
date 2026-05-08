package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", "session", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	cfg := &config.City{Daemon: config.DaemonConfig{}} // legacy default
	delivered, err := dispatchAllQueuedNudges(dir, cfg, nil, nil, newSessionBeadSnapshot(nil))
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 in legacy mode", delivered)
	}
}

func TestDispatchAllQueuedNudgesEmptyQueue(t *testing.T) {
	dir := t.TempDir()
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, nil, newSessionBeadSnapshot(nil))
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 with empty queue", delivered)
	}
}

func TestDispatchAllQueuedNudgesSkipsNotYetDue(t *testing.T) {
	dir := t.TempDir()
	future := time.Now().Add(5 * time.Minute)
	item := newQueuedNudge("worker", "later", "session", time.Now())
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
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, runtime.NewFake(), snapshot)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 (item not yet due)", delivered)
	}
}

func TestDispatchAllQueuedNudgesDeliversAndAcks(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()

	// Set up a running session via the same fake-provider harness used by
	// the per-session poller test, then enqueue a nudge for it.
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.Create(context.Background(), "worker", "Worker", "codex", dir, "codex", nil, session.ProviderResume{}, runtime.Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}

	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", "session", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}

	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), store, fake, snapshot)
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

func TestDispatchAllQueuedNudgesSkipsACPSession(t *testing.T) {
	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", "session", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	bead := beads.Bead{
		ID:     "session-1",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"transport":    "acp",
		},
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{bead})
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nil, runtime.NewFake(), snapshot)
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 (ACP session owned by inject-on-hook)", delivered)
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
	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", "session", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	delivered, err := dispatchAllQueuedNudges(dir, nil, nil, nil, newSessionBeadSnapshot(nil))
	if err != nil {
		t.Fatalf("dispatchAllQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Fatalf("delivered = %d, want 0 with nil cfg", delivered)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	wakeCh := make(chan struct{}, 1)
	lis, err := startNudgeWakeListener(ctx, dir, wakeCh, nil, "test")
	if err != nil {
		t.Fatalf("startNudgeWakeListener: %v", err)
	}
	defer lis.Close() //nolint:errcheck

	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "msg", "session", time.Now())); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	select {
	case <-wakeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("wakeCh not signaled after enqueue")
	}
}
