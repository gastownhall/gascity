package beads

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	beadslib "github.com/steveyegge/beads"
)

type issueGraphSnapshotReaderForTest interface {
	IssueGraphSnapshot(ListQuery) ([]Bead, map[string][]Dep, error)
}

func TestMemStoreIssueGraphSnapshotReturnsIssuesAndEdgesFromOneSnapshot(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	blocker, err := store.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	work, err := store.Create(Bead{Title: "work"})
	if err != nil {
		t.Fatalf("Create work: %v", err)
	}
	if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	rows, depsByID, err := store.IssueGraphSnapshot(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("IssueGraphSnapshot: %v", err)
	}
	if len(rows) != 2 || len(depsByID) != 2 {
		t.Fatalf("rows=%+v deps=%+v", rows, depsByID)
	}
	if deps, present := depsByID[blocker.ID]; !present || len(deps) != 0 {
		t.Fatalf("blocker deps = %+v, present=%v, want present empty", deps, present)
	}
	if deps := depsByID[work.ID]; len(deps) != 1 || deps[0].DependsOnID != blocker.ID || deps[0].Type != "blocks" {
		t.Fatalf("work deps = %+v", deps)
	}
}

func TestFileStoreIssueGraphSnapshotRefreshesBeforeAtomicRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "beads.json")
	reader, err := OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	writer, err := OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	blocker, err := writer.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	work, err := writer.Create(Bead{Title: "work"})
	if err != nil {
		t.Fatalf("Create work: %v", err)
	}
	if err := writer.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	rows, depsByID, err := reader.IssueGraphSnapshot(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("IssueGraphSnapshot: %v", err)
	}
	if len(rows) != 2 || len(depsByID[work.ID]) != 1 {
		t.Fatalf("rows=%+v deps=%+v, want writer's complete graph", rows, depsByID)
	}
}

func TestNativeDoltStoreIssueGraphSnapshotUsesOneReadTransaction(t *testing.T) {
	t.Parallel()

	txCalls := 0
	tx := issueGraphSnapshotTransaction{
		search: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if filter.Ephemeral == nil || *filter.Ephemeral || !filter.IncludeDependencies {
				t.Fatalf("filter = %+v, want durable issues with dependencies", filter)
			}
			return []*beadslib.Issue{{
				ID:         "ga-1",
				Title:      "work",
				Status:     beadslib.StatusOpen,
				IssueType:  beadslib.TypeTask,
				Priority:   1,
				Metadata:   []byte(`{}`),
				RowVersion: 9,
			}}, nil
		},
		dependencies: func(_ context.Context, issueID string) ([]*beadslib.Dependency, error) {
			if issueID != "ga-1" {
				t.Fatalf("dependency issue id = %q, want ga-1", issueID)
			}
			return []*beadslib.Dependency{{IssueID: "ga-1", DependsOnID: "ga-2", Type: beadslib.DepBlocks}}, nil
		},
	}
	storage := &nativeDoltStorageSpy{
		runInTransaction: func(_ context.Context, commitMsg string, fn func(beadslib.Transaction) error) error {
			txCalls++
			if commitMsg != "" {
				t.Fatalf("read transaction commit message = %q, want empty", commitMsg)
			}
			return fn(tx)
		},
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			t.Fatal("SearchIssues called outside transaction")
			return nil, nil
		},
		getDependencyRecords: func(context.Context, string) ([]*beadslib.Dependency, error) {
			t.Fatal("GetDependencyRecords called outside transaction")
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	reader, ok := any(store).(issueGraphSnapshotReaderForTest)
	if !ok {
		t.Fatal("NativeDoltStore does not implement atomic issue graph snapshots")
	}

	rows, depsByID, err := reader.IssueGraphSnapshot(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierIssues})
	if err != nil {
		t.Fatalf("IssueGraphSnapshot: %v", err)
	}
	if txCalls != 1 {
		t.Fatalf("transactions = %d, want 1", txCalls)
	}
	if len(rows) != 1 || rows[0].Revision != 9 {
		t.Fatalf("rows = %+v", rows)
	}
	if deps := depsByID["ga-1"]; len(deps) != 1 || deps[0].DependsOnID != "ga-2" || deps[0].Type != "blocks" {
		t.Fatalf("dependencies = %+v", deps)
	}
}

func TestNativeDoltStoreIssueGraphSnapshotReturnsNoPartialGraphOnFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("dependency read failed")
	tx := issueGraphSnapshotTransaction{
		search: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return []*beadslib.Issue{{
				ID: "ga-1", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask,
				Priority: 1, Metadata: []byte(`{}`), RowVersion: 4,
			}}, nil
		},
		dependencies: func(context.Context, string) ([]*beadslib.Dependency, error) {
			return nil, wantErr
		},
	}
	storage := &nativeDoltStorageSpy{
		runInTransaction: func(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
			return fn(tx)
		},
	}
	reader, ok := any(newNativeDoltStoreForTest(storage)).(issueGraphSnapshotReaderForTest)
	if !ok {
		t.Fatal("NativeDoltStore does not implement atomic issue graph snapshots")
	}

	rows, depsByID, err := reader.IssueGraphSnapshot(ListQuery{AllowScan: true, IncludeClosed: true})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if rows != nil || depsByID != nil {
		t.Fatalf("partial result rows=%+v deps=%+v", rows, depsByID)
	}
}

func TestNativeDoltStoreIssueGraphSnapshotPreservesPersistedStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []beadslib.Status{"blocked", "deferred", "hooked"} {
		t.Run(string(status), func(t *testing.T) {
			tx := issueGraphSnapshotTransaction{
				search: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
					return []*beadslib.Issue{{ID: "ga-1", Title: "work", Status: status, IssueType: beadslib.TypeTask, Metadata: []byte(`{}`), RowVersion: 9}}, nil
				},
				dependencies: func(context.Context, string) ([]*beadslib.Dependency, error) { return nil, nil },
			}
			store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
				runInTransaction: func(_ context.Context, _ string, fn func(beadslib.Transaction) error) error { return fn(tx) },
			})
			rows, _, err := store.IssueGraphSnapshot(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierIssues})
			if err != nil || len(rows) != 1 || rows[0].Status != string(status) {
				t.Fatalf("snapshot rows=%+v err=%v, want persisted status %q", rows, err, status)
			}
		})
	}
}

func TestNativeDoltStoreIssueGraphSnapshotPushesExactStatusBeforeLimit(t *testing.T) {
	t.Parallel()
	tx := issueGraphSnapshotTransaction{
		search: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if filter.Status == nil || *filter.Status != beadslib.StatusOpen || len(filter.ExcludeStatus) != 0 || filter.Limit != 1 {
				t.Fatalf("filter=%+v, want exact open predicate before limit=1", filter)
			}
			return []*beadslib.Issue{{ID: "ga-open", Title: "work", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Metadata: []byte(`{}`), RowVersion: 9}}, nil
		},
		dependencies: func(context.Context, string) ([]*beadslib.Dependency, error) { return nil, nil },
	}
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		runInTransaction: func(_ context.Context, _ string, fn func(beadslib.Transaction) error) error { return fn(tx) },
	})
	rows, _, err := store.IssueGraphSnapshot(ListQuery{Status: "open", Limit: 1, Sort: SortCreatedAsc, TierMode: TierIssues})
	if err != nil || len(rows) != 1 || rows[0].ID != "ga-open" {
		t.Fatalf("snapshot rows=%+v err=%v, want one exact open row", rows, err)
	}
}

type issueGraphSnapshotTransaction struct {
	beadslib.Transaction
	search       func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error)
	dependencies func(context.Context, string) ([]*beadslib.Dependency, error)
}

func (tx issueGraphSnapshotTransaction) SearchIssues(ctx context.Context, query string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	return tx.search(ctx, query, filter)
}

func (tx issueGraphSnapshotTransaction) GetDependencyRecords(ctx context.Context, issueID string) ([]*beadslib.Dependency, error) {
	return tx.dependencies(ctx, issueID)
}
