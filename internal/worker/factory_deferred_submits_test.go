package worker

import (
	"context"
	"errors"
	"os"
	"testing"

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
