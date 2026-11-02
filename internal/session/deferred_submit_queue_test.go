package session

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// recordingDeferredQueue is a DeferredSubmitQueue that keeps what it is given.
type recordingDeferredQueue struct {
	items []nudgequeue.Item
	err   error
}

func (q *recordingDeferredQueue) EnqueueDeferred(item nudgequeue.Item) error {
	q.items = append(q.items, item)
	return q.err
}

// submitFollowUpToCodex creates a codex session and submits one follow-up to
// it, which the session cannot take yet, so it is queued.
func submitFollowUpToCodex(t *testing.T, mgr *Manager, store beads.Store) (Info, SubmitOutcome, error) {
	t.Helper()
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "alias", "helper-alias"); err != nil {
		t.Fatalf("SetMetadata(alias): %v", err)
	}
	origPoller := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startSessionSubmitPoller = origPoller })
	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "follow up later", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentFollowUp)
	return info, outcome, err
}

// An injected deferred-submit queue receives the item the city's state.json
// would have, and state.json is not written.
func TestSubmitFollowUpQueuesThroughAnInjectedDeferredQueue(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	queue := &recordingDeferredQueue{}
	mgr := NewManagerWithOptions(store, runtime.NewFake(), WithCityPath(cityPath), WithDeferredSubmitQueue(queue))

	before := time.Now().UTC()
	info, outcome, err := submitFollowUpToCodex(t, mgr, store)
	if err != nil {
		t.Fatalf("Submit(follow_up): %v", err)
	}
	if !outcome.Queued {
		t.Fatal("Submit(follow_up) should report queued")
	}
	if len(queue.items) != 1 {
		t.Fatalf("injected queue got %d items, want 1", len(queue.items))
	}
	item := queue.items[0]
	if !strings.HasPrefix(item.ID, "nudge-") || len(item.ID) != len("nudge-")+12 {
		t.Fatalf("ID = %q, want nudge-<12>", item.ID)
	}
	if item.Agent != "helper-alias" || item.SessionID != info.ID || item.Source != nudgequeue.SourceSubmit || item.Message != "follow up later" {
		t.Fatalf("item = %+v, want the follow-up addressed to helper-alias for %s", item, info.ID)
	}
	if item.ContinuationEpoch != deferredSubmitEpoch(beads.Bead{Metadata: mustSessionMetadata(t, store, info.ID)}) {
		t.Fatalf("ContinuationEpoch = %q, want the session's", item.ContinuationEpoch)
	}
	if item.CreatedAt.Before(before) || !item.DeliverAfter.Equal(item.CreatedAt) || !item.ExpiresAt.Equal(item.CreatedAt.Add(defaultQueuedSubmitTTL)) {
		t.Fatalf("times = created %v deliver %v expires %v, want deliver at creation and expiry a TTL on", item.CreatedAt, item.DeliverAfter, item.ExpiresAt)
	}
	if _, err := os.Stat(nudgequeue.StatePath(cityPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json stat = %v, want it unwritten with an injected queue", err)
	}
}

func mustSessionMetadata(t *testing.T, store beads.Store, id string) map[string]string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b.Metadata
}

// Without an injected queue, the deferred submit is the state.json append it
// always was: the file holds exactly the bytes the append writes for the item.
func TestSubmitFollowUpWithoutAnInjectedQueueWritesStateJSON(t *testing.T) {
	for _, opts := range [][]ManagerOption{nil, {WithDeferredSubmitQueue(nil)}} {
		store := beads.NewMemStore()
		cityPath := t.TempDir()
		mgr := NewManagerWithOptions(store, runtime.NewFake(), append([]ManagerOption{WithCityPath(cityPath)}, opts...)...)
		if _, _, err := submitFollowUpToCodex(t, mgr, store); err != nil {
			t.Fatalf("Submit(follow_up): %v", err)
		}
		got, err := os.ReadFile(nudgequeue.StatePath(cityPath))
		if err != nil {
			t.Fatalf("reading state.json: %v", err)
		}
		state, err := nudgequeue.LoadState(cityPath)
		if err != nil || len(state.Pending) != 1 {
			t.Fatalf("LoadState = %d pending, %v; want the one deferred submit", len(state.Pending), err)
		}
		twin := t.TempDir()
		if err := nudgequeue.WithState(twin, func(s *nudgequeue.State) error {
			s.Pending = append(s.Pending, state.Pending[0])
			nudgequeue.SortState(s)
			return nil
		}); err != nil {
			t.Fatalf("writing the twin: %v", err)
		}
		want, err := os.ReadFile(nudgequeue.StatePath(twin))
		if err != nil {
			t.Fatalf("reading the twin: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("state.json =\n%s\nwant the append's bytes\n%s", got, want)
		}
	}
}

// A failing injected queue fails the submit, with the cause kept.
func TestSubmitFollowUpReportsAnInjectedQueueFailure(t *testing.T) {
	store := beads.NewMemStore()
	boom := errors.New("queue refused")
	mgr := NewManagerWithOptions(store, runtime.NewFake(), WithCityPath(t.TempDir()), WithDeferredSubmitQueue(&recordingDeferredQueue{err: boom}))
	_, _, err := submitFollowUpToCodex(t, mgr, store)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "queueing deferred submit: ") {
		t.Fatalf("Submit error = %v, want the queue's failure wrapped as a deferred submit", err)
	}
}

// The city path still gates the deferred queue: an injected queue does not
// make a Manager with no city path offer follow-ups.
func TestDeferredSubmitQueueNeedsACityPath(t *testing.T) {
	origPoller := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startSessionSubmitPoller = origPoller })
	queue := &recordingDeferredQueue{}
	for _, tc := range []struct {
		name     string
		cityPath string
		want     bool
	}{
		{name: "no city path", cityPath: "", want: false},
		{name: "blank city path", cityPath: "  ", want: false},
		{name: "a city path", cityPath: t.TempDir(), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManagerWithOptions(beads.NewMemStore(), runtime.NewFake(), WithCityPath(tc.cityPath), WithDeferredSubmitQueue(queue))
			if got := mgr.deferredQueue() != nil; got != tc.want {
				t.Fatalf("has a deferred queue = %v, want %v", got, tc.want)
			}
			err := mgr.enqueueDeferredSubmitLocked(beads.Bead{ID: "s-1"}, "s-name", "hello", nudgequeue.SourceSubmit)
			if tc.want {
				if err != nil {
					t.Fatalf("enqueueDeferredSubmitLocked: %v", err)
				}
				return
			}
			if err == nil || err.Error() != "deferred submit is unavailable without a city path" {
				t.Fatalf("enqueueDeferredSubmitLocked error = %v, want the no-city-path refusal", err)
			}
		})
	}
	if len(queue.items) != 1 {
		t.Fatalf("injected queue got %d items, want only the city-path manager's", len(queue.items))
	}
}

// No non-test file in internal/session touches the state.json queue directly:
// deferred submits go through the Manager's DeferredSubmitQueue.
func TestSessionReachesTheNudgeQueueOnlyThroughDeferredSubmitQueue(t *testing.T) {
	var dir string
	if root := bazeltest.OverrideRoot(); root != "" {
		dir = filepath.Join(root, "internal", "session")
	} else {
		_, file, _, ok := goruntime.Caller(0)
		if !ok {
			t.Fatal("runtime.Caller failed")
		}
		dir = filepath.Dir(file)
	}
	forbidden := map[string]bool{"WithState": true, "LoadState": true, "WithdrawWaitNudges": true, "StatePath": true, "SortState": true}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "nudgequeue" && forbidden[sel.Sel.Name] {
				t.Errorf("%s calls nudgequeue.%s; queue deferred submits through the Manager's DeferredSubmitQueue", name, sel.Sel.Name)
			}
			return true
		})
	}
}
