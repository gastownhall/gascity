package worker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type recordingDeferredSubmits struct{ items []nudgequeue.Item }

func (q *recordingDeferredSubmits) EnqueueDeferred(item nudgequeue.Item) error {
	q.items = append(q.items, item)
	return nil
}

// queueFollowUpThroughFactory submits a follow-up the session cannot take yet
// through the factory's manager, so it is deferred.
//
// Deferring a codex follow-up also starts the session's nudge poller, and the
// session package's poller seam is unexported, so this package cannot stub
// it. These tests rely on ensureSessionSubmitPoller refusing to exec a Go test
// binary (isGoTestExecutable); the refusal is swallowed by the submit path, so
// no poller child is spawned. The guard holds under go test and under remote
// execution, but not when the test binary runs through a symlinked Bazel
// runfiles tree (local Bazel): there os.Executable resolves the symlink, the
// guard misses, and each deferring test starts one detached poller child.
func queueFollowUpThroughFactory(t *testing.T, factory *Factory, cityPath string) {
	t.Helper()
	ctx := context.Background()
	info, err := factory.manager.CreateSession(ctx, sessionpkg.CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	city, err := sessionpkg.NewCityDir(cityPath)
	if err != nil {
		t.Fatalf("NewCityDir: %v", err)
	}
	actor := sessionpkg.Actor{Kind: sessionpkg.ActorOperator, City: city}
	outcome, err := factory.manager.Submit(ctx, actor, info.ID, "follow up later", sessionpkg.BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, sessionpkg.SubmitIntentFollowUp)
	if err != nil {
		t.Fatalf("Submit(follow_up): %v", err)
	}
	if !outcome.Queued {
		t.Fatal("Submit(follow_up) should report queued")
	}
}

// FactoryConfig.DeferredSubmits reaches the factory's session manager: a
// deferred submit lands in it, not in the city's state.json.
func TestFactoryThreadsDeferredSubmitsToTheManager(t *testing.T) {
	cityPath := t.TempDir()
	queue := &recordingDeferredSubmits{}
	factory, err := NewFactory(FactoryConfig{CityPath: cityPath, Store: beads.NewMemStore(), Provider: runtime.NewFake(), DeferredSubmits: queue})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	queueFollowUpThroughFactory(t, factory, cityPath)
	if len(queue.items) != 1 || queue.items[0].Message != "follow up later" {
		t.Fatalf("deferred submits = %+v, want the follow-up", queue.items)
	}
	if _, err := os.Stat(nudgequeue.StatePath(cityPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json stat = %v, want it unwritten", err)
	}
}

// Without DeferredSubmits, the factory's manager defers into state.json.
func TestFactoryWithoutDeferredSubmitsUsesTheStateFile(t *testing.T) {
	cityPath := t.TempDir()
	factory, err := NewFactory(FactoryConfig{CityPath: cityPath, Store: beads.NewMemStore(), Provider: runtime.NewFake()})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	queueFollowUpThroughFactory(t, factory, cityPath)
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil || len(state.Pending) != 1 || state.Pending[0].Message != "follow up later" {
		t.Fatalf("state.json pending = %+v, %v; want the follow-up", state.Pending, err)
	}
}

// A session's runtime handle (Factory.SessionRuntimeHandle), through which
// the queue, the conversation lane and mail and sling notices nudge a running
// session, waits behind the startup turn the session's queue still holds, as
// the session manager's own sends do: a nudge, live-only or not, is reported
// undelivered and types nothing while the turn is pending or claimed, unless
// it is the queue's delivery of that turn, under the key the claim pinned it
// to. A runtime handle with no session row, or for a row that does not exist,
// waits behind nothing.
func TestFactorySessionRuntimeHandleWaitsBehindTheQueuedStartupTurn(t *testing.T) {
	cityPath := t.TempDir()
	fake := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{CityPath: cityPath, Store: beads.NewMemStore(), Provider: fake})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	info, err := factory.manager.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	now := time.Now().UTC()
	turn := nudgequeue.Item{
		ID: "turn", Agent: info.ID, SessionID: info.ID, Source: nudgequeue.SourceSubmit, Message: "start working",
		CreatedAt: now, DeliverAfter: now, ExpiresAt: now.Add(time.Hour), Reference: nudgequeue.StartupTurnReference("L1"),
	}
	if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error {
		state.Pending = append(state.Pending, turn)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	actor := sessionpkg.Actor{Kind: sessionpkg.ActorAgent}
	nudge := func(what string, h Handle, req NudgeRequest) bool {
		t.Helper()
		before := len(fake.SnapshotCalls())
		result, err := h.Nudge(context.Background(), actor, req)
		if err != nil {
			t.Fatalf("%s: Nudge = %+v, %v", what, result, err)
		}
		var typed []string
		for _, call := range fake.SnapshotCalls()[before:] {
			if call.Method == "Nudge" || call.Method == "NudgeNow" {
				typed = append(typed, call.Message)
			}
		}
		if result.Delivered != (len(typed) == 1) {
			t.Fatalf("%s: Nudge = %+v, typed %q; want it typed exactly when delivered", what, result, typed)
		}
		return result.Delivered
	}
	held, err := factory.SessionRuntimeHandle(info.ID, info.SessionName, "codex", "", nil)
	if err != nil {
		t.Fatalf("SessionRuntimeHandle: %v", err)
	}
	if nudge("live-only, behind the pending turn", held, NudgeRequest{Text: "mail notice", Wake: NudgeWakeLiveOnly}) {
		t.Fatal("a live-only nudge typed ahead of the pending startup turn")
	}
	if nudge("default, behind the pending turn", held, NudgeRequest{Text: "operator nudge"}) {
		t.Fatal("a nudge typed ahead of the pending startup turn")
	}
	if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error {
		claimed := state.Pending[0]
		claimed.DeliveryBatch, claimed.ClaimedAt, claimed.LeaseUntil = "nudge-batch-1", now, now.Add(time.Minute)
		state.Pending, state.InFlight = nil, []nudgequeue.Item{claimed}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if nudge("under another delivery's key, behind the claimed turn", held, NudgeRequest{Text: "lane message", Wake: NudgeWakeLiveOnly, DeliveryKey: "lane:op-1"}) {
		t.Fatal("a lane message typed ahead of the claimed startup turn")
	}
	if !nudge("the queue's delivery of the turn", held, NudgeRequest{Text: turn.Message, Wake: NudgeWakeLiveOnly, DeliveryKey: "nudge-batch-1"}) {
		t.Fatal("the queue's delivery of the claimed turn, under its key, was held")
	}
	rowless, err := factory.RuntimeHandle(info.SessionName, "codex", "", nil)
	if err != nil {
		t.Fatalf("RuntimeHandle: %v", err)
	}
	if !nudge("a runtime handle with no session row", rowless, NudgeRequest{Text: "runtime nudge", Wake: NudgeWakeLiveOnly}) {
		t.Fatal("a runtime handle with no session row was held")
	}
	missing, err := factory.SessionRuntimeHandle("gc-no-such-session", info.SessionName, "codex", "", nil)
	if err != nil {
		t.Fatalf("SessionRuntimeHandle(missing row): %v", err)
	}
	if !nudge("a session row that does not exist", missing, NudgeRequest{Text: "runtime nudge", Wake: NudgeWakeLiveOnly}) {
		t.Fatal("a runtime handle for a row that does not exist was held")
	}
}
