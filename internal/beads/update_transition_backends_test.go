package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	beadslib "github.com/steveyegge/beads"
)

type bdUpdateTransitionFixture struct {
	mu         sync.Mutex
	updated    bool
	updateErr  error
	updateArgs []string
	updateEnv  map[string]string
}

type nativeUpdateCommitErrorStorage struct {
	*commitCountingMemStorage
	err error
}

func (s *nativeUpdateCommitErrorStorage) RunInTransaction(ctx context.Context, msg string, fn func(beadslib.Transaction) error) error {
	if err := s.commitCountingMemStorage.RunInTransaction(ctx, msg, fn); err != nil {
		return err
	}
	return s.err
}

type nativeUpdateRollbackThenRawCloseStorage struct {
	*nativeDoltMemStorage
	targetID string
	err      error
}

func (s *nativeUpdateRollbackThenRawCloseStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	s.store.mu.Lock()
	seq, beads, deps := s.store.snapshot()
	s.store.mu.Unlock()
	if err := fn(nativeDoltTransactionForTest{storage: s}); err != nil {
		s.store.restoreFrom(seq, beads, deps)
		return err
	}

	// Model an indeterminate commit that did not retain this transaction,
	// followed by a raw bd writer that ignores Gas City's cooperative scope.
	s.store.restoreFrom(seq, beads, deps)
	if err := s.store.Close(s.targetID); err != nil {
		return err
	}
	return s.err
}

func (f *bdUpdateTransitionFixture) runner(_ string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
	if len(args) == 2 && args[1] == "--help" {
		switch args[0] {
		case "update", "close", "assign", "delete":
			return []byte("Flags:\n      --if-revision int\n"), nil
		}
	}
	switch args[0] {
	case "show", "query":
		bead := Bead{
			ID:        "bd-42",
			Title:     "before",
			Status:    "open",
			Type:      "task",
			CreatedAt: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
			Labels:    []string{"remove"},
			Metadata:  StringMap{"existing": "kept"},
		}
		if f.updated {
			priority := 1
			bead.Title = "after"
			bead.Status = "closed"
			bead.Type = "bug"
			bead.Priority = &priority
			bead.Description = "updated atomically"
			bead.ParentID = "parent-1"
			bead.Assignee = "worker-1"
			bead.Labels = []string{"added"}
			bead.Metadata = StringMap{"existing": "kept", "audit": "atomic"}
			bead.UpdatedAt = time.Date(2026, 7, 16, 12, 0, 1, 0, time.UTC)
		}
		out, err := json.Marshal([]Bead{bead})
		return out, err
	case "dep":
		return []byte(`[]`), nil
	case "update":
		f.updateArgs = slices.Clone(args)
		f.updated = true
		return []byte(`[{"id":"bd-42"}]`), f.updateErr
	default:
		return nil, fmt.Errorf("unexpected bd args %q", args)
	}
}

func (f *bdUpdateTransitionFixture) envRunner(dir, name string, env map[string]string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.updateEnv = make(map[string]string, len(env))
	for key, value := range env {
		f.updateEnv[key] = value
	}
	f.mu.Unlock()
	return f.runner(dir, name, args...)
}

func TestUpdateTransitionMemAndFileBackendsReturnDurableSnapshots(t *testing.T) {
	tests := []struct {
		name string
		open func(*testing.T) Store
	}{
		{
			name: "mem",
			open: func(*testing.T) Store { return NewMemStore() },
		},
		{
			name: "file",
			open: func(t *testing.T) Store {
				store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
				if err != nil {
					t.Fatalf("OpenFileStore: %v", err)
				}
				return store
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.open(t)
			created, err := store.Create(Bead{
				Title:  "before",
				Labels: []string{"keep", "remove"},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			transitioner, ok := UpdateTransitionerFor(store)
			if !ok {
				t.Fatalf("UpdateTransitionerFor(%T) reported unsupported", store)
			}
			closed := "closed"
			title := "after"
			beadType := "bug"
			priority := 1
			description := "updated atomically"
			parentID := "parent-1"
			assignee := "worker-1"
			transition, err := transitioner.UpdateWithTransition(created.ID, UpdateOpts{
				Title:        &title,
				Status:       &closed,
				Type:         &beadType,
				Priority:     &priority,
				Description:  &description,
				ParentID:     &parentID,
				Assignee:     &assignee,
				Labels:       []string{"added"},
				RemoveLabels: []string{"remove"},
				Metadata:     map[string]string{"audit": "atomic"},
			})
			if err != nil {
				t.Fatalf("UpdateWithTransition: %v", err)
			}
			if transition.Before.Status != "open" || transition.Before.Title != "before" {
				t.Fatalf("Before = %#v, want original open row", transition.Before)
			}
			if !transition.TransitionedToClosed || !transition.AuthoritativeAfter(created.ID) {
				t.Fatalf("transition = %#v, want authoritative open-to-closed transition", transition)
			}
			got := transition.After
			if got.Status != closed || got.Title != title || got.Type != beadType ||
				got.Priority == nil || *got.Priority != priority || got.Description != description ||
				got.ParentID != parentID || got.Assignee != assignee || got.Metadata["audit"] != "atomic" ||
				!slices.Contains(got.Labels, "keep") || !slices.Contains(got.Labels, "added") ||
				slices.Contains(got.Labels, "remove") {
				t.Fatalf("After = %#v, want every UpdateOpts field applied", got)
			}

			durable, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get durable row: %v", err)
			}
			if durable.Revision != got.Revision || !durable.UpdatedAt.Equal(got.UpdatedAt) {
				t.Fatalf("After revision/time = %d/%s, durable = %d/%s", got.Revision, got.UpdatedAt, durable.Revision, durable.UpdatedAt)
			}

			transition, err = transitioner.UpdateWithTransition(created.ID, UpdateOpts{Status: &closed})
			if err != nil {
				t.Fatalf("UpdateWithTransition already closed: %v", err)
			}
			if transition.TransitionedToClosed {
				t.Fatal("TransitionedToClosed = true for an already-closed row")
			}
		})
	}
}

func TestBdStoreUpdateTransitionKeepsSnapshotsAndFencedRowUpdateUnderOneLease(t *testing.T) {
	fixture := &bdUpdateTransitionFixture{}
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	closed := "closed"
	title := "after"
	beadType := "bug"
	priority := 1
	description := "updated atomically"
	assignee := "worker-1"
	transition, err := store.UpdateWithTransition("bd-42", UpdateOpts{
		Title:       &title,
		Status:      &closed,
		Type:        &beadType,
		Priority:    &priority,
		Description: &description,
		Assignee:    &assignee,
		Metadata:    map[string]string{"audit": "atomic"},
	})
	if err != nil {
		t.Fatalf("UpdateWithTransition: %v", err)
	}
	if transition.Before.Status != "open" || transition.After.Status != "closed" ||
		!transition.TransitionedToClosed || !transition.AuthoritativeAfter("bd-42") {
		t.Fatalf("transition = %#v, want authoritative open-to-closed snapshots", transition)
	}
	if transition.After.Title != title || transition.After.Metadata["audit"] != "atomic" {
		t.Fatalf("After = %#v, want complete updated snapshot", transition.After)
	}

	fixture.mu.Lock()
	args := slices.Clone(fixture.updateArgs)
	env := fixture.updateEnv
	fixture.mu.Unlock()
	for _, flag := range []string{
		"--title", "--status", "--type", "--priority", "--description",
		"--assignee", "--set-metadata", conditionalWriteFlag,
	} {
		if !slices.Contains(args, flag) {
			t.Fatalf("bd update args = %q, missing %s", args, flag)
		}
	}
	if env[lifecycleMutationScopeEnv] == "" || env[lifecycleMutationTokenEnv] == "" {
		t.Fatalf("bd update env = %#v, want lifecycle lease inheritance", env)
	}
}

func TestBdStoreUpdateTransitionDoesNotClaimAmbiguousCommittedWrite(t *testing.T) {
	wantErr := errors.New("hook failed after durable update")
	fixture := &bdUpdateTransitionFixture{updateErr: wantErr}
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	closed := "closed"
	transition, err := store.UpdateWithTransition("bd-42", UpdateOpts{Status: &closed})
	if !errors.Is(err, wantErr) {
		t.Fatalf("UpdateWithTransition error = %v, want %v", err, wantErr)
	}
	if transition.TransitionedToClosed || !transition.AuthoritativeAfter("bd-42") || transition.After.Status != "closed" {
		t.Fatalf("transition = %#v, want authoritative state without unprovable ownership", transition)
	}
}

func TestBdStoreUpdateTransitionRejectsUnfenceableRelationEditsBeforeMutation(t *testing.T) {
	fixture := &bdUpdateTransitionFixture{}
	store := NewBdStore(t.TempDir(), fixture.runner, WithBdStoreCommandEnvRunner(fixture.envRunner))

	closed := "closed"
	parent := "parent-1"
	transition, err := store.UpdateWithTransition("bd-42", UpdateOpts{
		Status:   &closed,
		ParentID: &parent,
		Labels:   []string{"added"},
	})
	if !errors.Is(err, ErrUpdateTransitionUnsupported) {
		t.Fatalf("UpdateWithTransition error = %v, want ErrUpdateTransitionUnsupported", err)
	}
	if transition.AuthoritativeAfter("bd-42") {
		t.Fatalf("transition = %#v, want no mutation result", transition)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.updateArgs) != 0 {
		t.Fatalf("bd update args = %q, want no mutation", fixture.updateArgs)
	}
}

func TestNativeDoltStoreUpdateTransitionUsesSingleTransaction(t *testing.T) {
	storage := &commitCountingMemStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{
		Title:    "before",
		Labels:   []string{"remove"},
		Metadata: map[string]string{"existing": "kept"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed := "closed"
	title := "after"
	transition, err := store.UpdateWithTransition(created.ID, UpdateOpts{
		Title:        &title,
		Status:       &closed,
		Labels:       []string{"added"},
		RemoveLabels: []string{"remove"},
		Metadata:     map[string]string{"audit": "atomic"},
	})
	if err != nil {
		t.Fatalf("UpdateWithTransition: %v", err)
	}
	if storage.commits != 1 {
		t.Fatalf("native transaction commits = %d, want 1", storage.commits)
	}
	if transition.Before.Status != "open" || transition.After.Status != "closed" ||
		!transition.TransitionedToClosed || !transition.AuthoritativeAfter(created.ID) {
		t.Fatalf("transition = %#v, want authoritative open-to-closed snapshots", transition)
	}
	if transition.After.Title != title || transition.After.Metadata["existing"] != "kept" ||
		transition.After.Metadata["audit"] != "atomic" ||
		!slices.Contains(transition.After.Labels, "added") || slices.Contains(transition.After.Labels, "remove") {
		t.Fatalf("After = %#v, want complete updated snapshot", transition.After)
	}
	durable, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get durable row: %v", err)
	}
	if !durable.UpdatedAt.Equal(transition.After.UpdatedAt) {
		t.Fatalf("After updated_at = %s, durable = %s", transition.After.UpdatedAt, durable.UpdatedAt)
	}
}

func TestNativeDoltStoreUpdateTransitionReturnsSnapshotWithoutOwnershipOnCommitError(t *testing.T) {
	wantErr := errors.New("commit acknowledgement failed")
	storage := &nativeUpdateCommitErrorStorage{
		commitCountingMemStorage: &commitCountingMemStorage{nativeDoltMemStorage: newNativeDoltMemStorage()},
		err:                      wantErr,
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed := "closed"
	transition, err := store.UpdateWithTransition(created.ID, UpdateOpts{Status: &closed})
	if !errors.Is(err, wantErr) {
		t.Fatalf("UpdateWithTransition error = %v, want %v", err, wantErr)
	}
	if transition.TransitionedToClosed || !transition.AuthoritativeAfter(created.ID) || transition.After.Status != "closed" {
		t.Fatalf("transition = %#v, want authoritative close without ownership after indeterminate commit", transition)
	}
}

func TestNativeDoltStoreUpdateTransitionDoesNotClaimRollbackThenRawClose(t *testing.T) {
	wantErr := errors.New("commit outcome indeterminate")
	storage := &nativeUpdateRollbackThenRawCloseStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		err:                  wantErr,
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "raw writer wins after rollback"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	storage.targetID = created.ID

	closed := "closed"
	transition, err := store.UpdateWithTransition(created.ID, UpdateOpts{Status: &closed})
	if !errors.Is(err, wantErr) {
		t.Fatalf("UpdateWithTransition error = %v, want %v", err, wantErr)
	}
	if transition.TransitionedToClosed {
		t.Fatal("TransitionedToClosed = true for an indeterminate rollback followed by a raw close")
	}
	if !transition.AuthoritativeAfter(created.ID) || transition.After.Status != "closed" {
		t.Fatalf("transition = %#v, want authoritative raw-winner closed snapshot", transition)
	}
}

func TestClassStoresForwardUpdateTransitionerHandle(t *testing.T) {
	base := NewMemStore()
	wrapped := []Store{
		WorkStore{Store: base},
		GraphStore{Store: base},
		SessionStore{Store: base},
		MailStore{Store: base},
		OrdersStore{Store: base},
		NudgesStore{Store: base},
	}
	for _, store := range wrapped {
		transitioner, ok := UpdateTransitionerFor(store)
		if !ok {
			t.Fatalf("UpdateTransitionerFor(%T) reported unsupported", store)
		}
		if transitioner != UpdateTransitioner(base) {
			t.Fatalf("UpdateTransitionerFor(%T) returned %T, want exact underlying %T", store, transitioner, base)
		}
	}
}
