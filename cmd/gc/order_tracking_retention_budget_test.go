package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// goneOnDeleteStore reports one bead as already missing when the retention
// prune deletes it, as a store does for an orphaned row that a listing still
// returns or for a bead another sweeper deleted first.
type goneOnDeleteStore struct {
	*beads.MemStore
	goneID string
}

func (s *goneOnDeleteStore) Delete(id string) error {
	if id == s.goneID {
		return fmt.Errorf("%s: %w", id, beads.ErrNotFound)
	}
	return s.MemStore.Delete(id)
}

func TestSweepClosedOrderTrackingRetentionTreatsMissingBeadAsDeleted(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	total := minClosedOrderTrackingRetained + 3
	seed := make([]beads.Bead, 0, total)
	for i := range total {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("orphan-%02d", i),
			Title:     "order:orphan",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-48*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:orphan", labelOrderTracking},
			Ephemeral: true,
		})
	}
	store := &goneOnDeleteStore{
		MemStore: beads.NewMemStoreFrom(100, seed, nil),
		goneID:   "orphan-00",
	}

	result, err := sweepClosedOrderTrackingRetentionAcrossStores([]beads.Store{store}, now, orderTrackingRetentionPolicy{
		deleteAfterClose: 24 * time.Hour,
		retainLast:       minClosedOrderTrackingRetained,
	}, nil, 0)
	if err != nil {
		t.Fatalf("sweepClosedOrderTrackingRetentionAcrossStores err = %v, want nil when a bead is already gone", err)
	}
	if result.storesSwept != 1 {
		t.Fatalf("storesSwept = %d, want 1", result.storesSwept)
	}
	if result.deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (the other eligible beads)", result.deleted)
	}
}

func TestSweepOrderTrackingCommandMaxDeleteStopsAfterBudget(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	writeFile(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "test-city"
prefix = "ct"

[beads.policies.order_tracking]
delete_after_close = "1ns"
`)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	total := minClosedOrderTrackingRetained + 5
	ids := make([]string, 0, total)
	for i := range total {
		tracking, err := store.Create(beads.Bead{
			Title:     "order:cleanup",
			Labels:    []string{"order-run:cleanup", labelOrderTracking},
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("Create(tracking-%d): %v", i, err)
		}
		if err := store.Close(tracking.ID); err != nil {
			t.Fatalf("Close(tracking-%d): %v", i, err)
		}
		ids = append(ids, tracking.ID)
	}

	countRemaining := func() int {
		t.Helper()
		reopened, err := openStoreAtForCity(cityDir, cityDir)
		if err != nil {
			t.Fatalf("openStoreAtForCity(city reopen): %v", err)
		}
		remaining := 0
		for _, id := range ids {
			if _, err := reopened.Get(id); err == nil {
				remaining++
			}
		}
		return remaining
	}
	run := func() string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := newOrderSweepTrackingCmd(&stdout, &stderr)
		cmd.SetOut(&stderr)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"--max-delete", "2", "--confirm"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("sweep-tracking --max-delete 2: %v; stderr: %s", err, stderr.String())
		}
		return stdout.String()
	}

	// Five beads are past the TTL beyond the retain-10 floor; a budget of 2
	// deletes two per run, exits 0, and leaves the rest for the next run.
	out := run()
	if got, want := countRemaining(), total-2; got != want {
		t.Fatalf("remaining after first run = %d, want %d", got, want)
	}
	if !strings.Contains(out, "deleted 2 closed order-tracking bead") || !strings.Contains(out, "stopped at --max-delete 2") {
		t.Fatalf("stdout = %q, want 2 deleted and the budget stop", out)
	}
	run()
	out = run()
	if got := countRemaining(); got != minClosedOrderTrackingRetained {
		t.Fatalf("remaining after three runs = %d, want the retain floor %d", got, minClosedOrderTrackingRetained)
	}
	if !strings.Contains(out, "deleted 1 closed order-tracking bead") || strings.Contains(out, "stopped at --max-delete") {
		t.Fatalf("stdout = %q, want the last bead deleted with no budget stop", out)
	}
}
