package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
)

func TestOpenRecoversGeneratedIDSequence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first := New()
	if err := first.Open(ctx, coordstore.Config{DataDir: dir}); err != nil {
		t.Fatalf("first open: %v", err)
	}
	created, err := first.Create(ctx, coordstore.Record{Title: "first", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if created.ID != "sq-1" {
		t.Fatalf("first generated ID = %q, want sq-1", created.ID)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	second := New()
	if err := second.Open(ctx, coordstore.Config{DataDir: dir}); err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second close: %v", err)
		}
	})
	next, err := second.Create(ctx, coordstore.Record{Title: "second", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if next.ID != "sq-2" {
		t.Fatalf("next generated ID = %q, want sq-2", next.ID)
	}
}

func TestPurgeTerminalDeletesOldTerminalMainRecords(t *testing.T) {
	ctx := context.Background()
	adapter := New()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: t.TempDir()}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer adapter.Close() //nolint:errcheck

	now := time.Now()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-30 * time.Minute)

	oldClosed := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "old closed",
		Status:    "closed",
		Type:      "task",
		CreatedAt: old,
		Labels:    []string{"stale"},
		Metadata:  map[string]string{"kind": "terminal"},
	})
	oldCancelled := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "old canceled",
		Status:    "canceled",
		Type:      "task",
		CreatedAt: old,
	})
	oldExpired := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "old expired",
		Status:    "expired",
		Type:      "task",
		CreatedAt: old,
	})
	recentClosed := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "recent closed",
		Status:    "closed",
		Type:      "task",
		CreatedAt: recent,
	})
	oldOpen := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "old open",
		Status:    "open",
		Type:      "task",
		CreatedAt: old,
	})
	depTarget := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "dep target",
		Status:    "open",
		Type:      "task",
		CreatedAt: old,
	})
	if err := adapter.DepAdd(ctx, oldClosed.ID, depTarget.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	purged, err := adapter.PurgeTerminal(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PurgeTerminal: %v", err)
	}
	if purged != 3 {
		t.Fatalf("PurgeTerminal purged %d records, want 3", purged)
	}
	for _, id := range []string{oldClosed.ID, oldCancelled.ID, oldExpired.ID} {
		if _, err := adapter.Get(ctx, id); !coordstore.IsNotFound(err) {
			t.Fatalf("Get(%s) error = %v, want not found", id, err)
		}
	}
	for _, id := range []string{recentClosed.ID, oldOpen.ID, depTarget.ID} {
		if _, err := adapter.Get(ctx, id); err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
	}

	if count := countTerminalTestRows(t, adapter, "labels", oldClosed.ID); count != 0 {
		t.Fatalf("labels rows for purged record = %d, want 0", count)
	}
	if count := countTerminalTestRows(t, adapter, "metadata", oldClosed.ID); count != 0 {
		t.Fatalf("metadata rows for purged record = %d, want 0", count)
	}
	deps, err := adapter.DepList(ctx, oldClosed.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("deps for purged record = %d, want 0", len(deps))
	}
}

func TestGetReturnsFreshUpdatedAtAfterMutation(t *testing.T) {
	ctx := context.Background()
	adapter := New()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: t.TempDir()}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer adapter.Close() //nolint:errcheck

	// Seed with an explicit past CreatedAt so the mutation's wall-clock
	// updated_at is unambiguously later than the original timestamps.
	created, err := adapter.Create(ctx, coordstore.Record{
		Title:     "mutating record",
		Status:    "open",
		Type:      "task",
		CreatedAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.UpdatedAt.Equal(created.CreatedAt) {
		t.Fatalf("new record UpdatedAt = %v, want equal to CreatedAt %v", created.UpdatedAt, created.CreatedAt)
	}

	if err := adapter.SetMetadataBatch(ctx, created.ID, map[string]string{"phase": "running"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	got, err := adapter.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("Get UpdatedAt = %v, want strictly after CreatedAt %v", got.UpdatedAt, got.CreatedAt)
	}
	if !got.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("Get UpdatedAt = %v, want strictly after original UpdatedAt %v", got.UpdatedAt, created.UpdatedAt)
	}
}

func mustCreateTerminalTestRecord(ctx context.Context, t *testing.T, adapter *Adapter, r coordstore.Record) coordstore.Record {
	t.Helper()
	created, err := adapter.Create(ctx, r)
	if err != nil {
		t.Fatalf("Create(%q): %v", r.Title, err)
	}
	return created
}

func countTerminalTestRows(t *testing.T, adapter *Adapter, table, recordID string) int {
	t.Helper()
	var count int
	if err := adapter.readDB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE record_id=?", recordID).Scan(&count); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return count
}
