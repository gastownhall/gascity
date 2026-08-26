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
	touchCalls           int
	touchIssue           func(context.Context, string, string) error
}

func (s *closePolicyNativeDoltStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *closePolicyNativeDoltStorage) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	if status, ok := updates["status"]; ok && status == "closed" {
		s.closedUpdateAttempts++
		if s.refusal != nil {
			return s.refusal
		}
	}
	return s.nativeDoltMemStorage.UpdateIssue(ctx, id, updates, actor)
}

func (s *closePolicyNativeDoltStorage) UpdateIssueChecked(
	ctx context.Context,
	id string,
	updates map[string]interface{},
	actor string,
	opts beadslib.UpdateIssueOptions,
) error {
	if status, ok := updates["status"]; ok && status == "closed" {
		s.closedUpdateAttempts++
		if s.refusal != nil {
			return s.refusal
		}
	}
	return s.nativeDoltMemStorage.UpdateIssueChecked(ctx, id, updates, actor, opts)
}

func (s *closePolicyNativeDoltStorage) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	s.closeCalls++
	return s.nativeDoltMemStorage.CloseIssue(ctx, id, reason, actor, session)
}

func (s *closePolicyNativeDoltStorage) TouchIssue(ctx context.Context, id, actor string) error {
	s.touchCalls++
	if s.touchIssue != nil {
		return s.touchIssue(ctx, id, actor)
	}
	issue, err := s.GetIssue(ctx, id)
	if err != nil {
		return err
	}
	if issue == nil {
		return ErrNotFound
	}
	return s.nativeDoltMemStorage.UpdateIssue(ctx, id, map[string]interface{}{"title": issue.Title}, actor)
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

func TestNativeDoltStoreUpdateIfMatchClosedPreservesClosePolicy(t *testing.T) {
	closed := "closed"
	for _, refusal := range []struct {
		name string
		err  error
	}{
		{name: "live_blocker", err: beadslib.ErrCloseBlocked},
		{name: "open_child", err: issueops.ErrCloseOpenChildren},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			storage := &closePolicyNativeDoltStorage{
				nativeDoltMemStorage: newNativeDoltMemStorage(),
				refusal:              refusal.err,
			}
			store := newNativeDoltStoreForTest(storage)
			created, err := store.Create(Bead{Title: "policy target"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			before, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get before: %v", err)
			}

			err = store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{Status: &closed})
			if !errors.Is(err, refusal.err) {
				t.Fatalf("UpdateIfMatch error = %v, want %v", err, refusal.err)
			}
			after, getErr := store.Get(created.ID)
			if getErr != nil {
				t.Fatalf("Get after: %v", getErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused status-only close mutated bead:\n got: %+v\nwant: %+v", after, before)
			}
			if storage.closedUpdateAttempts != 1 || storage.closeCalls != 0 || storage.touchCalls != 0 {
				t.Fatalf("write calls = generic:%d force-close:%d touch:%d, want 1/0/0",
					storage.closedUpdateAttempts, storage.closeCalls, storage.touchCalls)
			}
		})
	}
}

func TestNativeDoltStoreUpdateIfMatchMixedClosedRollsBackOnClosePolicy(t *testing.T) {
	closed := "closed"
	title := "must roll back"
	for _, refusal := range []struct {
		name string
		err  error
	}{
		{name: "live_blocker", err: beadslib.ErrCloseBlocked},
		{name: "open_child", err: issueops.ErrCloseOpenChildren},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			storage := &closePolicyNativeDoltStorage{
				nativeDoltMemStorage: newNativeDoltMemStorage(),
				refusal:              refusal.err,
			}
			store := newNativeDoltStoreForTest(storage)
			oldParent, err := store.Create(Bead{Title: "old parent"})
			if err != nil {
				t.Fatalf("Create old parent: %v", err)
			}
			newParent, err := store.Create(Bead{Title: "new parent"})
			if err != nil {
				t.Fatalf("Create new parent: %v", err)
			}
			created, err := store.Create(Bead{
				Title:    "before",
				ParentID: oldParent.ID,
				Labels:   []string{"keep", "remove"},
			})
			if err != nil {
				t.Fatalf("Create target: %v", err)
			}
			before, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get before: %v", err)
			}

			err = store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{
				Status:       &closed,
				Title:        &title,
				ParentID:     &newParent.ID,
				Labels:       []string{"added"},
				RemoveLabels: []string{"remove"},
			})
			if !errors.Is(err, refusal.err) {
				t.Fatalf("UpdateIfMatch error = %v, want %v", err, refusal.err)
			}
			after, getErr := store.Get(created.ID)
			if getErr != nil {
				t.Fatalf("Get after: %v", getErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refused mixed close left partial mutation:\n got: %+v\nwant: %+v", after, before)
			}
			if storage.closedUpdateAttempts != 1 || storage.closeCalls != 0 || storage.touchCalls != 0 {
				t.Fatalf("write calls = generic:%d force-close:%d touch:%d, want 1/0/0",
					storage.closedUpdateAttempts, storage.closeCalls, storage.touchCalls)
			}
		})
	}
}

func TestNativeDoltStoreUpdateIfMatchMixedClosedUsesGenericPolicyAndTouchesLast(t *testing.T) {
	storage := &closePolicyNativeDoltStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	oldParent, err := store.Create(Bead{Title: "old parent"})
	if err != nil {
		t.Fatalf("Create old parent: %v", err)
	}
	newParent, err := store.Create(Bead{Title: "new parent"})
	if err != nil {
		t.Fatalf("Create new parent: %v", err)
	}
	created, err := store.Create(Bead{
		Title:    "before",
		ParentID: oldParent.ID,
		Labels:   []string{"keep", "remove"},
	})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	touchedAfterComposite := false
	var touchState Bead
	storage.touchIssue = func(ctx context.Context, id, actor string) error {
		issue, err := storage.GetIssue(ctx, id)
		if err != nil {
			return err
		}
		atTouch, err := beadFromNativeIssue(issue)
		if err != nil {
			return err
		}
		touchState = atTouch
		deps, err := storage.GetDependencyRecords(ctx, id)
		if err != nil {
			return err
		}
		parentAtTouch := false
		for _, dep := range deps {
			if dep != nil && dep.Type == beadslib.DepParentChild && dep.DependsOnID == newParent.ID {
				parentAtTouch = true
				break
			}
		}
		touchedAfterComposite = atTouch.Status == "closed" && atTouch.Title == "after" &&
			parentAtTouch && forceCloseContainsString(atTouch.Labels, "added") &&
			forceCloseContainsString(atTouch.Labels, "keep") && !forceCloseContainsString(atTouch.Labels, "remove")
		return storage.nativeDoltMemStorage.UpdateIssue(ctx, id, map[string]interface{}{"title": issue.Title}, actor)
	}
	closed := "closed"
	title := "after"
	err = store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{
		Status:       &closed,
		Title:        &title,
		ParentID:     &newParent.ID,
		Labels:       []string{"added"},
		RemoveLabels: []string{"remove"},
	})
	if err != nil {
		t.Fatalf("UpdateIfMatch: %v", err)
	}
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if after.Status != "closed" || after.Title != title || after.ParentID != newParent.ID ||
		!forceCloseContainsString(after.Labels, "added") || !forceCloseContainsString(after.Labels, "keep") ||
		forceCloseContainsString(after.Labels, "remove") {
		t.Fatalf("mixed conditional close = %+v, want all fields applied", after)
	}
	if after.Revision == before.Revision {
		t.Fatalf("revision after mixed conditional close = %d, want fresh from %d", after.Revision, before.Revision)
	}
	if !touchedAfterComposite || storage.touchCalls != 1 {
		t.Fatalf("touch after composite = %t, calls = %d, state = %+v, want true/1", touchedAfterComposite, storage.touchCalls, touchState)
	}
	if storage.closedUpdateAttempts != 1 || storage.closeCalls != 0 {
		t.Fatalf("close calls = generic:%d force-close:%d, want 1/0", storage.closedUpdateAttempts, storage.closeCalls)
	}
}

func TestNativeDoltStoreUpdateIfMatchAlreadyClosedWithLabelsStillTouches(t *testing.T) {
	storage := &closePolicyNativeDoltStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "already closed", Labels: []string{"remove"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed := "closed"
	if err := store.Update(created.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("force-close fixture: %v", err)
	}
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	touchSawLabels := false
	storage.touchIssue = func(ctx context.Context, id, actor string) error {
		issue, err := storage.GetIssue(ctx, id)
		if err != nil {
			return err
		}
		touchSawLabels = forceCloseContainsString(issue.Labels, "added") && !forceCloseContainsString(issue.Labels, "remove")
		return storage.nativeDoltMemStorage.UpdateIssue(ctx, id, map[string]interface{}{"title": issue.Title}, actor)
	}
	if err := store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{
		Status:       &closed,
		Labels:       []string{"added"},
		RemoveLabels: []string{"remove"},
	}); err != nil {
		t.Fatalf("UpdateIfMatch: %v", err)
	}
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if after.Status != "closed" || !forceCloseContainsString(after.Labels, "added") || forceCloseContainsString(after.Labels, "remove") {
		t.Fatalf("already-closed mixed update = %+v", after)
	}
	if after.Revision == before.Revision {
		t.Fatalf("revision after related mutation = %d, want fresh from %d", after.Revision, before.Revision)
	}
	if !touchSawLabels || storage.touchCalls != 1 {
		t.Fatalf("touch saw final labels = %t, calls = %d, want true/1", touchSawLabels, storage.touchCalls)
	}
	if storage.closeCalls != 1 {
		t.Fatalf("force-close calls = %d, want fixture close only", storage.closeCalls)
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
