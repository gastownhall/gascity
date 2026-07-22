package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	beadslib "github.com/steveyegge/beads"
)

type bdCloseAllTransitionFixture struct {
	mu    sync.Mutex
	beads map[string]Bead
}

func newBdCloseAllTransitionFixture() *bdCloseAllTransitionFixture {
	now := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)
	return &bdCloseAllTransitionFixture{beads: map[string]Bead{
		"bd-live":   {ID: "bd-live", Title: "live", Status: "open", Type: "task", CreatedAt: now, UpdatedAt: now, Revision: 11},
		"bd-closed": {ID: "bd-closed", Title: "closed", Status: "closed", Type: "task", CreatedAt: now, UpdatedAt: now, Revision: 21},
	}}
}

func (f *bdCloseAllTransitionFixture) runner(_ string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
	if len(args) == 2 && args[1] == "--help" {
		return []byte("Flags:\n      --if-revision int\n"), nil
	}
	switch args[0] {
	case "query":
		for _, arg := range args {
			if !strings.HasPrefix(arg, "id=") {
				continue
			}
			bead, ok := f.beads[strings.TrimPrefix(arg, "id=")]
			if !ok {
				return []byte(`[]`), nil
			}
			return json.Marshal([]map[string]any{{
				"id":         bead.ID,
				"title":      bead.Title,
				"status":     bead.Status,
				"issue_type": bead.Type,
				"created_at": bead.CreatedAt,
				"updated_at": bead.UpdatedAt,
				"metadata":   bead.Metadata,
				"revision":   bead.Revision,
			}})
		}
		return nil, fmt.Errorf("query missing id: %q", args)
	case "dep":
		return []byte(`[]`), nil
	case "update":
		var ids []string
		for i := 2; i < len(args) && !strings.HasPrefix(args[i], "--"); i++ {
			ids = append(ids, args[i])
		}
		if len(ids) == 1 {
			if expected, ok := revisionArg(args); ok && expected != f.beads[ids[0]].Revision {
				return conditionalPreconditionBody(expected, f.beads[ids[0]].Revision), errors.New("exit status 9")
			}
		}
		metadata := make(map[string]string)
		for i := 2 + len(ids); i+1 < len(args); i += 2 {
			if args[i] != "--set-metadata" {
				continue
			}
			key, value, ok := strings.Cut(args[i+1], "=")
			if ok {
				metadata[key] = value
			}
		}
		for _, id := range ids {
			bead := f.beads[id]
			if bead.Metadata == nil {
				bead.Metadata = make(StringMap)
			}
			for key, value := range metadata {
				bead.Metadata[key] = value
			}
			bead.Revision++
			f.beads[id] = bead
		}
		return []byte(`[]`), nil
	case "close":
		if expected, ok := revisionArg(args); ok {
			id := ""
			for _, arg := range args[1:] {
				if _, exists := f.beads[arg]; exists {
					id = arg
					break
				}
			}
			if id != "" && expected != f.beads[id].Revision {
				return conditionalPreconditionBody(expected, f.beads[id].Revision), errors.New("exit status 9")
			}
		}
		for _, arg := range args {
			bead, ok := f.beads[arg]
			if !ok {
				continue
			}
			bead.Status = "closed"
			bead.Revision++
			f.beads[arg] = bead
		}
		return []byte(`[]`), nil
	default:
		return nil, fmt.Errorf("unexpected bd args %q", args)
	}
}

func (f *bdCloseAllTransitionFixture) envRunner(dir, name string, _ map[string]string, args ...string) ([]byte, error) {
	return f.runner(dir, name, args...)
}

func TestCloseAllTransitionsMemAndFilePreserveBatchSemantics(t *testing.T) {
	tests := []struct {
		name string
		open func(*testing.T) Store
	}{
		{name: "mem", open: func(*testing.T) Store { return NewMemStore() }},
		{name: "file", open: func(t *testing.T) Store {
			store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
			if err != nil {
				t.Fatalf("OpenFileStore: %v", err)
			}
			return store
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.open(t)
			live, err := store.Create(Bead{Title: "live"})
			if err != nil {
				t.Fatalf("Create live: %v", err)
			}
			alreadyClosed, err := store.Create(Bead{Title: "already closed"})
			if err != nil {
				t.Fatalf("Create already closed: %v", err)
			}
			if err := store.Close(alreadyClosed.ID); err != nil {
				t.Fatalf("Close setup: %v", err)
			}

			transitioner, ok := CloseAllTransitionerFor(store)
			if !ok {
				t.Fatalf("CloseAllTransitionerFor(%T) reported unsupported", store)
			}
			result, err := transitioner.CloseAllWithTransitions(
				[]string{live.ID, alreadyClosed.ID, live.ID},
				map[string]string{"batch": "preserved"},
			)
			if err != nil {
				t.Fatalf("CloseAllWithTransitions: %v", err)
			}
			if result.Count != 1 {
				t.Fatalf("Count = %d, want deduplicated newly-closed count 1", result.Count)
			}
			liveTransition, ok := result.TransitionFor(live.ID)
			if !ok || !liveTransition.Transitioned || liveTransition.Before.Status == "closed" ||
				liveTransition.After.Status != "closed" || liveTransition.After.Metadata["batch"] != "preserved" {
				t.Fatalf("live transition = %#v ok=%v, want authoritative metadata-bearing close", liveTransition, ok)
			}
			closedTransition, ok := result.TransitionFor(alreadyClosed.ID)
			if !ok || closedTransition.Transitioned || closedTransition.After.Metadata["batch"] != "" {
				t.Fatalf("already-closed transition = %#v ok=%v, want unchanged skipped row", closedTransition, ok)
			}
		})
	}
}

func TestFileStoreCloseAllTransitionRollsBackOnSaveFailure(t *testing.T) {
	fs := fsys.NewFake()
	const path = "/city/.gc/beads.json"
	store, err := OpenFileStore(fs, path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	created, err := store.Create(Bead{Title: "rollback"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantErr := errors.New("disk full")
	fs.Errors[path+".tmp"] = wantErr

	result, err := store.CloseAllWithTransitions([]string{created.ID}, map[string]string{"batch": "rolled-back"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("CloseAllWithTransitions error = %v, want %v", err, wantErr)
	}
	if result.Count != 0 || len(result.Transitions) != 0 {
		t.Fatalf("result = %#v, want no reported transition after rollback", result)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after rollback: %v", err)
	}
	if got.Status != "open" || got.Metadata["batch"] != "" {
		t.Fatalf("durable row after rollback = %#v, want original open row", got)
	}
}

func TestBdStoreCloseAllTransitionPreservesBatchCountAndMetadata(t *testing.T) {
	fixture := newBdCloseAllTransitionFixture()
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	result, err := store.CloseAllWithTransitions(
		[]string{"bd-live", "bd-closed"},
		map[string]string{"batch": "bd-preserved"},
	)
	if err != nil {
		t.Fatalf("CloseAllWithTransitions: %v", err)
	}
	if result.Count != 2 {
		t.Fatalf("Count = %d, want BdStore's existing successful batch count 2", result.Count)
	}
	live, ok := result.TransitionFor("bd-live")
	if !ok || !live.Transitioned || live.After.Metadata["batch"] != "bd-preserved" {
		t.Fatalf("live transition = %#v ok=%v, want metadata-bearing close", live, ok)
	}
	closed, ok := result.TransitionFor("bd-closed")
	if !ok || closed.Transitioned || closed.After.Metadata["batch"] != "bd-preserved" {
		t.Fatalf("closed transition = %#v ok=%v, want unchanged status with existing metadata behavior", closed, ok)
	}
}

type nativeCloseAllCommitErrorStorage struct {
	*nativeCloseTransitionStorage
	err error
}

func (s *nativeCloseAllCommitErrorStorage) RunInTransaction(
	ctx context.Context,
	commitMsg string,
	fn func(beadslib.Transaction) error,
) error {
	if err := s.nativeCloseTransitionStorage.RunInTransaction(ctx, commitMsg, fn); err != nil {
		return err
	}
	return s.err
}

func TestNativeDoltStoreCloseAllTransitionReturnsCommittedSnapshotWithError(t *testing.T) {
	wantErr := errors.New("close acknowledgement failed")
	storage := &nativeCloseAllCommitErrorStorage{
		nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
		err:                          wantErr,
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "native ambiguous close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := store.CloseAllWithTransitions([]string{created.ID}, map[string]string{"batch": "committed"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("CloseAllWithTransitions error = %v, want %v", err, wantErr)
	}
	if result.Count != 0 {
		t.Fatalf("Count = %d, want existing partial-error count 0", result.Count)
	}
	transition, ok := result.TransitionFor(created.ID)
	if !ok || transition.Transitioned || transition.After.Status != "closed" || transition.After.Metadata["batch"] != "committed" {
		t.Fatalf("transition = %#v ok=%v, want verified committed state without ambiguous ownership", transition, ok)
	}
}

func TestClassStoresForwardCloseAllTransitionerHandle(t *testing.T) {
	base := NewMemStore()
	stores := []Store{
		WorkStore{Store: base},
		GraphStore{Store: base},
		SessionStore{Store: base},
		MailStore{Store: base},
		OrdersStore{Store: base},
		NudgesStore{Store: base},
	}
	for _, store := range stores {
		transitioner, ok := CloseAllTransitionerFor(store)
		if !ok {
			t.Fatalf("CloseAllTransitionerFor(%T) reported unsupported", store)
		}
		if transitioner != CloseAllTransitioner(base) {
			t.Fatalf("CloseAllTransitionerFor(%T) returned %T, want exact underlying %T", store, transitioner, base)
		}
	}
}
