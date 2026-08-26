package beads

import (
	"context"
	"errors"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// closePolicyNativeDoltStorage models abc4's new generic-update close gate:
// sending status=closed through UpdateIssue is refused, while CloseIssue keeps
// the force-close behavior Gas City's Store contract had on bf97.
type closePolicyNativeDoltStorage struct {
	*nativeDoltMemStorage
	refusal              error
	closedUpdateAttempts int
	closeCalls           int
}

func (s *closePolicyNativeDoltStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *closePolicyNativeDoltStorage) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	if status, ok := updates["status"]; ok && status == "closed" {
		s.closedUpdateAttempts++
		return s.refusal
	}
	return s.nativeDoltMemStorage.UpdateIssue(ctx, id, updates, actor)
}

func (s *closePolicyNativeDoltStorage) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	s.closeCalls++
	return s.nativeDoltMemStorage.CloseIssue(ctx, id, reason, actor, session)
}

func TestNativeDoltStoreUpdateForceClosesAcrossABC4PolicyRefusals(t *testing.T) {
	for _, refusal := range []struct {
		name string
		err  error
	}{
		{name: "live_blocker", err: beadslib.ErrCloseBlocked},
		{name: "open_child", err: issueops.ErrCloseOpenChildren},
	} {
		for _, shape := range []struct {
			name        string
			mixed       bool
			wantTitle   string
			wantLabel   bool
			wantOutcome string
		}{
			{name: "status_only", wantTitle: "before"},
			{name: "mixed_fields", mixed: true, wantTitle: "after", wantLabel: true, wantOutcome: "pass"},
		} {
			for _, route := range []struct {
				name string
				run  func(*NativeDoltStore, string, UpdateOpts) error
			}{
				{name: "direct", run: func(store *NativeDoltStore, id string, opts UpdateOpts) error {
					return store.Update(id, opts)
				}},
				{name: "Store.Tx", run: func(store *NativeDoltStore, id string, opts UpdateOpts) error {
					return store.Tx("legacy force-close update", func(tx Tx) error { return tx.Update(id, opts) })
				}},
			} {
				t.Run(refusal.name+"/"+shape.name+"/"+route.name, func(t *testing.T) {
					storage := &closePolicyNativeDoltStorage{
						nativeDoltMemStorage: newNativeDoltMemStorage(),
						refusal:              refusal.err,
					}
					store := newNativeDoltStoreForTest(storage)
					target, err := store.Create(Bead{Title: "before", Type: "epic", Metadata: map[string]string{"kept": "yes"}})
					if err != nil {
						t.Fatalf("Create target: %v", err)
					}
					other, err := store.Create(Bead{Title: "live related issue"})
					if err != nil {
						t.Fatalf("Create related issue: %v", err)
					}
					if refusal.name == "live_blocker" {
						if err := store.DepAdd(target.ID, other.ID, "blocks"); err != nil {
							t.Fatalf("DepAdd blocker: %v", err)
						}
					} else {
						if err := store.DepAdd(other.ID, target.ID, "parent-child"); err != nil {
							t.Fatalf("DepAdd child: %v", err)
						}
					}

					closed := "closed"
					opts := UpdateOpts{Status: &closed}
					if shape.mixed {
						title := "after"
						opts.Title = &title
						opts.Labels = []string{"terminal"}
						opts.Metadata = map[string]string{"outcome": "pass"}
					}
					if err := route.run(store, target.ID, opts); err != nil {
						t.Fatalf("Update: %v", err)
					}
					got, err := store.Get(target.ID)
					if err != nil {
						t.Fatalf("Get: %v", err)
					}
					if got.Status != "closed" || got.Title != shape.wantTitle || got.Metadata["kept"] != "yes" || got.Metadata["outcome"] != shape.wantOutcome {
						t.Fatalf("updated bead = %+v, want closed with all sibling fields", got)
					}
					if forceCloseContainsString(got.Labels, "terminal") != shape.wantLabel {
						t.Fatalf("labels = %v, want terminal", got.Labels)
					}
					if storage.closedUpdateAttempts != 0 {
						t.Fatalf("generic status=closed attempts = %d, want zero", storage.closedUpdateAttempts)
					}
					if storage.closeCalls != 1 {
						t.Fatalf("CloseIssue calls = %d, want one", storage.closeCalls)
					}
				})
			}
		}
	}
}

func TestNativeDoltStoreUpdateClosedRollsBackSiblingFieldsWhenForceCloseFails(t *testing.T) {
	wantErr := errors.New("injected force-close failure")
	storage := &nativeDoltFailingCloseStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
	}
	storage.closeIssue = func(ctx context.Context, id, reason, actor, session string) error {
		if err := storage.nativeDoltMemStorage.CloseIssue(ctx, id, reason, actor, session); err != nil {
			return err
		}
		return wantErr
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "before", Metadata: map[string]string{"kept": "yes"}, Labels: []string{"before"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	closed := "closed"
	title := "after"
	err = store.Update(created.ID, UpdateOpts{
		Status:       &closed,
		Title:        &title,
		Labels:       []string{"after"},
		RemoveLabels: []string{"before"},
		Metadata:     map[string]string{"outcome": "pass"},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Update error = %v, want %v", err, wantErr)
	}
	after, getErr := store.Get(created.ID)
	if getErr != nil {
		t.Fatalf("Get after: %v", getErr)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed force close left partial fields:\n got: %+v\nwant: %+v", after, before)
	}
}

func forceCloseContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
