package main

import (
	"bytes"
	"fmt"
	"log"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func TestWispGC_NilSafe(t *testing.T) {
	var wg wispGC
	if wg != nil {
		t.Error("nil wispGC should be nil")
	}
}

func TestWispGC_DisabledReturnsNil(t *testing.T) {
	wg := newWispGC(0, time.Hour, 0)
	if wg != nil {
		t.Error("zero interval should return nil")
	}
	wg = newWispGC(time.Hour, 0, 0)
	if wg != nil {
		t.Error("zero TTL should return nil")
	}
}

func TestWispGC_ShouldRunRespectsInterval(t *testing.T) {
	wg := newWispGC(5*time.Minute, time.Hour, 0)
	now := time.Now()

	if !wg.shouldRun(now) {
		t.Error("should run on first call")
	}

	wg.(*memoryWispGC).lastRun = now

	if wg.shouldRun(now.Add(time.Minute)) {
		t.Error("should not run before interval elapsed")
	}

	if !wg.shouldRun(now.Add(6 * time.Minute)) {
		t.Error("should run after interval elapsed")
	}
}

func TestWispGCForConfigUsesMailRetentionTTL(t *testing.T) {
	cfg := &config.City{}
	cfg.Daemon.WispGCInterval = "5m"
	cfg.Mail.RetentionTTL = "1h"

	wg := newWispGCForConfig(cfg)
	if wg == nil {
		t.Fatal("newWispGCForConfig returned nil")
	}
	memory := wg.(*memoryWispGC)
	if memory.ttl != 0 {
		t.Fatalf("ttl = %v, want 0", memory.ttl)
	}
	if memory.mailRetentionTTL != time.Hour {
		t.Fatalf("mailRetentionTTL = %v, want 1h", memory.mailRetentionTTL)
	}
}

func TestWispGC_PurgesExpiredMolecules(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBeadWithMetadata("wisp-1", now.Add(-2*time.Hour), "closed", "task", map[string]string{"gc.kind": "wisp"}),
		makeGCBead("mol-2", now.Add(-30*time.Minute), "closed", "molecule"),
		makeGCBead("mol-3", now.Add(-3*time.Hour), "closed", "molecule"),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 3 {
		t.Fatalf("purged = %d, want 3", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "wisp-1", "mol-3")
}

func TestWispGC_NothingExpired(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-10*time.Minute), "closed", "molecule"),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted = %v, want none", store.deletedIDs)
	}
}

func TestWispGCClosesGeneratedMembersOnlyForTerminalRoots(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBeadWithMetadata("completed-root", now.Add(-30*time.Minute), "closed", "task", map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.outcome":          "pass",
		}),
		makeGCBeadWithMetadata("completed-step", now.Add(-30*time.Minute), "open", "task", map[string]string{
			"gc.root_bead_id": "completed-root",
		}),
		makeGCBeadWithMetadata("superseded-root", now.Add(-30*time.Minute), "closed", "task", map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.outcome":          "canceled",
		}),
		makeGCBeadWithMetadata("partial-step", now.Add(-30*time.Minute), "open", "task", map[string]string{
			"gc.root_bead_id": "superseded-root",
		}),
		makeGCBeadWithMetadata("live-root", now.Add(-30*time.Minute), "in_progress", "task", map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		}),
		makeGCBeadWithMetadata("live-step", now.Add(-30*time.Minute), "open", "task", map[string]string{
			"gc.root_bead_id": "live-root",
		}),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
		t.Fatalf("runGC: %v", err)
	}

	for _, id := range []string{"completed-step", "partial-step"} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" || got.Metadata["gc.outcome"] != "skipped" {
			t.Fatalf("%s = status %q outcome %q, want closed/skipped", id, got.Status, got.Metadata["gc.outcome"])
		}
	}
	live, err := store.Get("live-step")
	if err != nil {
		t.Fatalf("Get(live-step): %v", err)
	}
	if live.Status != "open" {
		t.Fatalf("live-step status = %q, want open", live.Status)
	}
}

func TestWispGC_ClosesOpenSpecSidecarsForClosedWorkflowRoots(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBeadWithMetadata("closed-workflow", now.Add(-30*time.Minute), "closed", "task", map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		}),
		makeGCBeadWithMetadata("closed-workflow-spec", now.Add(-30*time.Minute), "open", "spec", map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": "closed-workflow",
			"gc.spec_for":     "implement",
		}),
		makeGCBeadWithMetadata("open-workflow", now.Add(-30*time.Minute), "open", "task", map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		}),
		makeGCBeadWithMetadata("open-workflow-spec", now.Add(-30*time.Minute), "open", "spec", map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": "open-workflow",
			"gc.spec_for":     "review",
		}),
		makeGCBeadWithMetadata("closed-task", now.Add(-30*time.Minute), "closed", "task", nil),
		makeGCBeadWithMetadata("closed-task-spec", now.Add(-30*time.Minute), "open", "spec", map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": "closed-task",
		}),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; spec repair should not count as purge", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted = %v, want none", store.deletedIDs)
	}

	closedSpec, err := store.Get("closed-workflow-spec")
	if err != nil {
		t.Fatalf("Get(closed-workflow-spec): %v", err)
	}
	if closedSpec.Status != "closed" {
		t.Fatalf("closed-workflow-spec status = %q, want closed", closedSpec.Status)
	}
	if got := closedSpec.Metadata["close_reason"]; got != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("close_reason = %q, want %q", got, sourceworkflow.WorkflowSpecSidecarClosedReason)
	}

	for _, id := range []string{"open-workflow-spec", "closed-task-spec"} {
		spec, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if spec.Status != "open" {
			t.Fatalf("%s status = %q, want open", id, spec.Status)
		}
	}
}

func TestWispGC_PurgesExpiredReadMessageRetention(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCMessageWisp("read-old", now.Add(-2*time.Hour), map[string]string{mail.ReadMetadataKey: "true"}),
		makeGCMessageWisp("unread-old", now.Add(-2*time.Hour), map[string]string{mail.ReadMetadataKey: "false"}),
		makeGCMessageWisp("unset-old", now.Add(-2*time.Hour), nil),
		makeGCMessageWisp("read-recent", now.Add(-30*time.Minute), map[string]string{mail.ReadMetadataKey: "true"}),
		{
			ID:        "read-main-tier",
			Status:    "open",
			Type:      "message",
			CreatedAt: now.Add(-2 * time.Hour),
			Metadata:  map[string]string{mail.ReadMetadataKey: "true"},
		},
		{
			ID:        "read-task-wisp",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			Metadata:  map[string]string{mail.ReadMetadataKey: "true"},
			Ephemeral: true,
		},
	})

	wg := newWispGC(5*time.Minute, 0, time.Hour)
	if wg == nil {
		t.Fatal("mail retention should enable wisp GC when interval is configured")
	}
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "read-old")
	for _, id := range []string{"unread-old", "unset-old", "read-recent", "read-main-tier", "read-task-wisp"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s should be preserved: %v", id, err)
		}
	}
}

func TestWispGC_ReadMessageRetentionZeroDisablesAndSuppressesLog(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCMessageWisp("read-old", now.Add(-2*time.Hour), map[string]string{mail.ReadMetadataKey: "true"}),
	})

	logOutput := captureWispGCLog(t, func() {
		wg := newWispGC(5*time.Minute, time.Hour, 0)
		purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
		if err != nil {
			t.Fatalf("runGC: %v", err)
		}
		if purged != 0 {
			t.Fatalf("purged = %d, want 0", purged)
		}
	})
	if strings.Contains(logOutput, "read message wisps") {
		t.Fatalf("log output = %q, want no read-message purge log", logOutput)
	}
	if _, err := store.Get("read-old"); err != nil {
		t.Fatalf("read-old should be preserved: %v", err)
	}
}

func TestWispGC_ReadMessageRetentionLogsCountAndTTL(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCMessageWisp("read-old", now.Add(-2*time.Hour), map[string]string{mail.ReadMetadataKey: "true"}),
	})

	logOutput := captureWispGCLog(t, func() {
		wg := newWispGC(5*time.Minute, 0, time.Hour)
		if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
			t.Fatalf("runGC: %v", err)
		}
	})
	want := "wisp gc: purged 1 read message wisps (retention_ttl=1h)"
	if !strings.Contains(logOutput, want) {
		t.Fatalf("log output = %q, want %q", logOutput, want)
	}
}

func TestWispGC_EmptyList(t *testing.T) {
	store := newGCStore(nil)
	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, time.Now())
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
}

func TestWispGC_DeleteErrorIsSurfacedAndContinues(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBead("mol-2", now.Add(-2*time.Hour), "closed", "molecule"),
	})
	store.deleteErrors["mol-1"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err == nil {
		t.Fatal("expected delete error to be surfaced")
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err = %v, want delete failure to be included", err)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-2")
}

func TestWispGC_PurgesExpiredMoleculeChildrenWithRoot(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		{
			ID:        "mol-1.2",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1.1",
		},
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := store.DepAdd("mol-1.2", "mol-1.1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.2->mol-1.1): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 root purge accounting", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "mol-1.1", "mol-1.2")
	for _, id := range []string{"mol-1", "mol-1.1", "mol-1.2"} {
		if _, err := store.Get(id); err == nil {
			t.Fatalf("Get(%s) succeeded after GC delete", id)
		}
	}
}

// TestWispGC_ClosureSkipsRootReopenedAfterSnapshot is the regression test for
// ra-nxppyo: closedWispGCEntries can answer from a stale cached snapshot. A
// root reopened (live work resumed) inside the cache window — after the
// snapshot was taken but before the purge sweep reaches it — must not have
// its full descendant closure destructively deleted.
func TestWispGC_ClosureSkipsRootReopenedAfterSnapshot(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-reopen", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-reopen.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-reopen",
		},
	})
	if err := store.DepAdd("mol-reopen.1", "mol-reopen", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-reopen.1->mol-reopen): %v", err)
	}

	entries, err := closedWispGCEntries(store)
	if err != nil {
		t.Fatalf("closedWispGCEntries: %v", err)
	}

	if err := store.Reopen("mol-reopen"); err != nil {
		t.Fatalf("Reopen(mol-reopen): %v", err)
	}

	purged, err := purgeExpiredBeadClosures(store, entries, now, 0)
	if err != nil {
		t.Fatalf("purgeExpiredBeadClosures: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0 (root was reopened after the snapshot)", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %v, want none", store.deletedIDs)
	}
	root, err := store.Get("mol-reopen")
	if err != nil {
		t.Fatalf("Get(mol-reopen): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("mol-reopen status = %q, want open (reopen must survive)", root.Status)
	}
	if _, err := store.Get("mol-reopen.1"); err != nil {
		t.Fatalf("Get(mol-reopen.1) should still exist: %v", err)
	}
}

// TestWispGC_ClosureSurfacesLiveRecheckError asserts that a transient live-read
// failure during the pre-delete re-verify is SURFACED rather than silently
// treated as "already gone". A backend outage must produce a visible sweep
// error, not a zero-purge sweep with a nil error — and must still never delete.
func TestWispGC_ClosureSurfacesLiveRecheckError(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-boom", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-boom.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-boom",
		},
	})
	if err := store.DepAdd("mol-boom.1", "mol-boom", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-boom.1->mol-boom): %v", err)
	}

	entries, err := closedWispGCEntries(store)
	if err != nil {
		t.Fatalf("closedWispGCEntries: %v", err)
	}

	// The live re-verify now fails for a reason other than not-found.
	store.getErrors["mol-boom"] = fmt.Errorf("backend down")

	purged, err := purgeExpiredBeadClosures(store, entries, now, 0)
	if err == nil {
		t.Fatal("purgeExpiredBeadClosures: want error, got nil (a live-read failure must not be swallowed)")
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0 (live re-verify failed)", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %v, want none", store.deletedIDs)
	}
}

func TestWispGC_PurgesExpiredClosureAcrossStorageTiers(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "wisp-root",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			Metadata:  map[string]string{"gc.kind": "wisp"},
			Ephemeral: true,
		},
		{
			ID:        "metadata-child",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			Metadata:  map[string]string{"gc.root_bead_id": "wisp-root"},
			Ephemeral: true,
		},
		{
			ID:        "parent-child",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "wisp-root",
			Ephemeral: true,
		},
		{
			ID:        "no-history-child",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "metadata-child",
			NoHistory: true,
		},
	})
	if err := store.DepAdd("parent-child", "wisp-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(parent-child->wisp-root): %v", err)
	}
	if err := store.DepAdd("no-history-child", "metadata-child", "parent-child"); err != nil {
		t.Fatalf("DepAdd(no-history-child->metadata-child): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 root purge accounting", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "wisp-root", "metadata-child", "parent-child", "no-history-child")
}

func TestWispGC_DoesNotDeleteExternalDependents(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		makeGCBead("external-1", now.Add(-2*time.Hour), "open", "task"),
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := store.DepAdd("external-1", "mol-1.1", "blocks"); err != nil {
		t.Fatalf("DepAdd(external-1->mol-1.1): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "mol-1.1")
	if _, err := store.Get("external-1"); err != nil {
		t.Fatalf("external dependent was deleted: %v", err)
	}
}

func TestWispGC_PurgesParentChildOwnedDependentsWithoutMetadata(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		{
			ID:        "mol-1.2",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
		},
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := store.DepAdd("mol-1.2", "mol-1.1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.2->mol-1.1): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "mol-1.1", "mol-1.2")
}

func TestWispGC_LeavesRootWhenChildDeleteFails(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	store.deleteErrors["mol-1.1"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err == nil {
		t.Fatal("expected child delete error")
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err = %v, want delete failure to be included", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0 when child delete fails", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted = %v, want none", store.deletedIDs)
	}
	if _, err := store.Get("mol-1"); err != nil {
		t.Fatalf("root deleted after child failure: %v", err)
	}
	if _, err := store.Get("mol-1.1"); err != nil {
		t.Fatalf("child unexpectedly deleted after failure: %v", err)
	}
}

func TestWispGC_PartialChildDeleteRemainsRetryable(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		{
			ID:        "mol-1.1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1.1",
		},
		{
			ID:        "mol-1.2",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
	})
	if err := store.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := store.DepAdd("mol-1.1.1", "mol-1.1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1.1->mol-1.1): %v", err)
	}
	if err := store.DepAdd("mol-1.2", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.2->mol-1): %v", err)
	}
	store.deleteErrors["mol-1.2"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err == nil {
		t.Fatal("expected first pass child delete error")
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("first pass err = %v, want delete failure to be included", err)
	}
	if purged != 0 {
		t.Fatalf("first purged = %d, want 0", purged)
	}
	if _, err := store.Get("mol-1"); err != nil {
		t.Fatalf("root deleted after partial child failure: %v", err)
	}
	if _, err := store.Get("mol-1.2"); err != nil {
		t.Fatalf("failing child deleted unexpectedly: %v", err)
	}
	if _, err := store.Get("mol-1.1"); err == nil {
		t.Fatalf("expected an earlier child to be deleted before downstream failure")
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1.1.1", "mol-1.1")

	delete(store.deleteErrors, "mol-1.2")
	purged, err = wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC second pass: %v", err)
	}
	if purged != 1 {
		t.Fatalf("second purged = %d, want 1", purged)
	}
	for _, id := range []string{"mol-1", "mol-1.2"} {
		if _, err := store.Get(id); err == nil {
			t.Fatalf("Get(%s) succeeded after retry cleanup", id)
		}
	}
}

func TestWispGC_PreservesOrderTrackingBeads(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBeadWithLabels("track-old", now.Add(-3*time.Hour), "closed", "task", labelOrderTracking),
		makeGCBeadWithLabels("track-new", now.Add(-10*time.Minute), "closed", "task", labelOrderTracking),
		makeGCBeadWithLabels("track-open", now.Add(-5*time.Hour), "open", "task", labelOrderTracking),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1")
	for _, id := range []string{"track-old", "track-new", "track-open"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s should be preserved for order-tracking retention: %v", id, err)
		}
	}
}

func TestWispGC_PreservesLegacyIssuesTierTrackingBeads(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "track-legacy",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-3 * time.Hour),
			Labels:    []string{labelOrderTracking},
		},
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
	if _, err := store.Get("track-legacy"); err != nil {
		t.Fatalf("legacy tracking bead should be preserved: %v", err)
	}
}

func TestWispGC_DoesNotListOrderTrackingBeads(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBeadWithLabels("track-old", now.Add(-3*time.Hour), "closed", "task", labelOrderTracking),
	})
	store.listErrors[gcQueryKey{Status: "closed", Label: labelOrderTracking}] = fmt.Errorf("tracking list failed")

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1")
	if _, err := store.Get("track-old"); err != nil {
		t.Fatalf("order-tracking bead should be preserved: %v", err)
	}
}

func TestWispGC_TrackingBeadsDoNotDeleteParentChildDescendants(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBeadWithLabels("track-old", now.Add(-3*time.Hour), "closed", "task", labelOrderTracking),
		{
			ID:        "track-child",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-3 * time.Hour),
			ParentID:  "track-old",
		},
	})
	if err := store.DepAdd("track-child", "track-old", "parent-child"); err != nil {
		t.Fatalf("DepAdd(track-child->track-old): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted IDs = %v, want none", store.deletedIDs)
	}
	for _, id := range []string{"track-old", "track-child"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s should be preserved: %v", id, err)
		}
	}
}

func TestWispGC_ListErrorFailsRun(t *testing.T) {
	store := newGCStore(nil)
	store.listErrors[gcQueryKey{Status: "closed", Type: "molecule"}] = fmt.Errorf("molecule list failed")

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	_, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, time.Now())
	if err == nil {
		t.Fatal("expected list error")
	}
}

func TestWispGC_ReapsClosedOrphanWhenRootAbsent(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	// Root "ghost-root" is never inserted: the orphan descendant's root is gone.
	store := newGCStore([]beads.Bead{
		makeGCOrphanWisp("orphan-1", now.Add(-2*time.Hour), "ghost-root"),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "orphan-1")
}

func TestWispGC_ReapsClosedOrphanWhenRootClosed(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	store := newGCStore([]beads.Bead{
		// Closed task root (terminal) without gc.kind=wisp so the root-rooted
		// closure purge does not enumerate it; the orphan path must still reap.
		makeGCBead("term-root", now.Add(-2*time.Hour), "closed", "task"),
		makeGCOrphanWisp("orphan-2", now.Add(-2*time.Hour), "term-root"),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "orphan-2")
	// The terminal root itself is out of scope for the orphan path.
	if _, err := store.Get("term-root"); err != nil {
		t.Fatalf("term-root should remain: %v", err)
	}
}

func TestWispGC_DoesNotReapWhenRootOpen(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("live-root", now.Add(-2*time.Hour), "in_progress", "molecule"),
		makeGCOrphanWisp("orphan-live", now.Add(-2*time.Hour), "live-root"),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; descendant of a live root must never be reaped", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted = %v, want none", store.deletedIDs)
	}
	if _, err := store.Get("orphan-live"); err != nil {
		t.Fatalf("orphan-live must still be Get-able while root is open: %v", err)
	}
}

func TestWispGC_DryRunDefaultReapsNothing(t *testing.T) {
	withReapOrphansEnforced(t, false)
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCOrphanWisp("orphan-dry", now.Add(-2*time.Hour), "ghost-root"),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	var purged int
	var runErr error
	logOutput := captureWispGCLog(t, func() {
		purged, runErr = wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	})
	if runErr != nil {
		t.Fatalf("runGC: %v", runErr)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0 in dry-run", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted = %v, want none in dry-run", store.deletedIDs)
	}
	if _, err := store.Get("orphan-dry"); err != nil {
		t.Fatalf("orphan-dry must survive dry-run: %v", err)
	}
	if !strings.Contains(logOutput, "would be reaped") {
		t.Fatalf("log = %q, want dry-run notice containing %q", logOutput, "would be reaped")
	}
}

func TestWispGC_ReapHonorsBatchCap(t *testing.T) {
	withReapOrphansEnforced(t, true)
	withReapOrphanBatchCap(t, 1)
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCOrphanWisp("orphan-cap-1", now.Add(-2*time.Hour), "ghost-root"),
		makeGCOrphanWisp("orphan-cap-2", now.Add(-2*time.Hour), "ghost-root"),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (batch cap)", purged)
	}
	if len(store.deletedIDs) != 1 {
		t.Fatalf("deleted = %v, want exactly 1 per sweep", store.deletedIDs)
	}
}

// TestWispGC_ReapBatchCapBoundsAttemptsNotJustSuccesses is the post-merge
// regression for the finding that the orphan reaper's batch cap advanced only on
// SUCCESSFUL deletes: a failing delete backend could attempt every eligible
// orphan in a single tick even though the cap is the safety bound for sweep
// work. With the cap at 1 and EVERY eligible orphan's delete failing, a sweep
// must stop after a single delete ATTEMPT — leaving the rest for a later tick —
// rather than walking the whole backlog because no success ever advanced the
// counter. Both candidates fail so the assertion is independent of which one the
// store lists first.
func TestWispGC_ReapBatchCapBoundsAttemptsNotJustSuccesses(t *testing.T) {
	withReapOrphansEnforced(t, true)
	withReapOrphanBatchCap(t, 1)
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCOrphanWisp("orphan-fail-1", now.Add(-2*time.Hour), "ghost-root"),
		makeGCOrphanWisp("orphan-fail-2", now.Add(-2*time.Hour), "ghost-root"),
	})
	store.deleteErrors["orphan-fail-1"] = fmt.Errorf("delete failed")
	store.deleteErrors["orphan-fail-2"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err == nil {
		t.Fatal("expected reap delete error to be surfaced")
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err = %v, want delete failure to be included", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0 (every delete failed)", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %v, want none (every delete failed)", store.deletedIDs)
	}
	if len(store.deleteAttempts) != 1 {
		t.Fatalf("delete attempts = %v (n=%d), want exactly 1; the batch cap must bound delete ATTEMPTS, not just successful reaps", store.deleteAttempts, len(store.deleteAttempts))
	}
}

// TestWispGC_ReapsRootlessPlainTaskWisp is the regression for
// gastownhall/gascity#3780: a closed, wisp-tier, type=task row with no
// gc.root_bead_id pointer has no owning root to check for collectibility --
// it is its own closure boundary, so its already-closed status (guaranteed by
// the candidates query) is sufficient to reap it. Previously such rows were
// skipped outright and accumulated uncollected in the wisp tier.
func TestWispGC_ReapsRootlessPlainTaskWisp(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	noRoot := makeGCBeadWithMetadata("no-root", now.Add(-2*time.Hour), "closed", "task", map[string]string{})
	noRoot.Ephemeral = true
	store := newGCStore([]beads.Bead{noRoot})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1; a rootless plain-task wisp is its own closure boundary", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "no-root")
}

// TestWispGC_ReapSkipsRootlessNonTaskRow preserves the original safety
// boundary for any rootless closed wisp-tier row that is NOT a plain task:
// an unrecognized shape the reaper cannot prove safe to collect stays out of
// scope, same as before #3780.
func TestWispGC_ReapSkipsRootlessNonTaskRow(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	noRoot := makeGCBeadWithMetadata("no-root-other", now.Add(-2*time.Hour), "closed", "note", map[string]string{})
	noRoot.Ephemeral = true
	store := newGCStore([]beads.Bead{noRoot})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; a rootless non-task row stays out of scope", purged)
	}
	if _, err := store.Get("no-root-other"); err != nil {
		t.Fatalf("no-root-other must be preserved: %v", err)
	}
}

// TestWispGC_ReapSkipsRootlessPlainTaskWithChildren pins the leaf fence on the
// rootless branch: deleteWorkflowBead removes a SINGLE bead, not a closure, so
// a rootless closed plain task that owns a parent-child subtree must stay out
// of scope. Reaping it would strand its descendants — rootless themselves and
// no longer reachable from any root — beyond either GC path forever.
func TestWispGC_ReapSkipsRootlessPlainTaskWithChildren(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	noRoot := makeGCBeadWithMetadata("no-root", now.Add(-2*time.Hour), "closed", "task", map[string]string{})
	noRoot.Ephemeral = true
	child := makeGCBeadWithMetadata("no-root.1", now.Add(-2*time.Hour), "closed", "step", map[string]string{})
	child.Ephemeral = true
	child.ParentID = "no-root"
	store := newGCStore([]beads.Bead{noRoot, child})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; a rootless plain task that owns children is not a leaf", purged)
	}
	for _, id := range []string{"no-root", "no-root.1"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s must be preserved: %v", id, err)
		}
	}
}

// TestWispGC_ReapSkipsRootlessPlainTaskWithParent pins the other half of the
// leaf fence: a rootless closed plain task that is itself a child of a live
// molecule root is a subtree MEMBER, not an independent closure boundary, so
// the reaper must leave it to the owning root's closure purge.
func TestWispGC_ReapSkipsRootlessPlainTaskWithParent(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	child := makeGCBeadWithMetadata("no-root", now.Add(-2*time.Hour), "closed", "task", map[string]string{})
	child.Ephemeral = true
	child.ParentID = "live-root"
	store := newGCStore([]beads.Bead{
		makeGCBead("live-root", now.Add(-2*time.Hour), "in_progress", "molecule"),
		child,
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; a rootless plain task with a parent is not a leaf", purged)
	}
	if _, err := store.Get("no-root"); err != nil {
		t.Fatalf("no-root must be preserved: %v", err)
	}
}

// TestWispGC_ReapSkipsRootlessMessageWisp pins the mail exclusion the rootless
// branch now leans on: mail wisps are created as type=message, so the type!=task
// guard keeps the wisp tier's largest population out of the orphan reaper and
// leaves PurgeReadMessageWisps authoritative over it.
func TestWispGC_ReapSkipsRootlessMessageWisp(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	msg := makeGCMessageWisp("closed-msg", now.Add(-2*time.Hour), nil)
	msg.Status = "closed"
	store := newGCStore([]beads.Bead{msg})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; a rootless message wisp belongs to the mail purge, not the orphan reaper", purged)
	}
	if _, err := store.Get("closed-msg"); err != nil {
		t.Fatalf("closed-msg must be preserved: %v", err)
	}
}

// TestWispGC_ReapDryRunBoundsRootlessProbes pins the probe cap on the rootless
// branch. The delete batch cap is gated on enforcement, so under the shipped
// DRY-RUN default (GC_WISP_GC_REAP_ORPHANS unset) nothing bounded the leaf-ness
// probes: every aged rootless candidate cost a Children read per controller
// tick — a bd subprocess apiece in production — against precisely the backlog
// this reaper exists to drain. The cap must bound those reads with no delete
// ever attempted, and the sweep must say the dry-run estimate is now a floor.
func TestWispGC_ReapDryRunBoundsRootlessProbes(t *testing.T) {
	withReapOrphansEnforced(t, false)
	withReapOrphanProbeCap(t, 2)
	now := time.Now()
	var seed []beads.Bead
	for i := 0; i < 6; i++ {
		bead := makeGCBeadWithMetadata(fmt.Sprintf("rootless-%d", i), now.Add(-2*time.Hour), "closed", "task", map[string]string{})
		bead.Ephemeral = true
		seed = append(seed, bead)
	}
	store := newGCStore(seed)

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	var purged int
	var runErr error
	output := captureWispGCLog(t, func() {
		purged, runErr = wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	})
	if runErr != nil {
		t.Fatalf("runGC: %v", runErr)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; the dry-run default never deletes", purged)
	}
	if len(store.deleteAttempts) != 0 {
		t.Fatalf("delete attempts = %v, want none in dry-run", store.deleteAttempts)
	}
	if store.childrenCalls > 2 {
		t.Fatalf("childrenCalls = %d, want <= 2; the probe cap must bound leaf-ness reads in dry-run too", store.childrenCalls)
	}
	if store.childrenCalls == 0 {
		t.Fatal("childrenCalls = 0; the sweep must still probe up to the cap")
	}
	if !strings.Contains(output, "rootless-orphan scan stopped after") {
		t.Fatalf("log = %q, want the truncation notice so the dry-run estimate is not silently reported as the full backlog", output)
	}
}

// TestWispGC_ReapSkipsRootlessTaskWithParentDepOnly pins the dep-row half of
// the leaf fence. Ownership is carried by the parent_id COLUMN or by a
// parent-child DEP ROW, and some step beads have only the dep row — which is
// why collectExpiredBeadClosure walks both. A column-only fence would reap such
// a row out from under a live parent.
func TestWispGC_ReapSkipsRootlessTaskWithParentDepOnly(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	child := makeGCBeadWithMetadata("no-root", now.Add(-2*time.Hour), "closed", "task", map[string]string{})
	child.Ephemeral = true
	store := newGCStore([]beads.Bead{
		makeGCBead("live-root", now.Add(-2*time.Hour), "in_progress", "molecule"),
		child,
	})
	if err := store.DepAdd("no-root", "live-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; a rootless plain task owned by a live parent through a dep row is not a leaf", purged)
	}
	if _, err := store.Get("no-root"); err != nil {
		t.Fatalf("no-root must be preserved: %v", err)
	}
}

// TestWispGC_ReapSkipsRootlessTaskWithChildDepOnly is the other direction of
// the same dep-row fence: a rootless closed plain task whose child is linked to
// it only by a parent-child dep row still owns a subtree, so reaping it — a
// SINGLE-bead delete — would strand that child beyond either GC path.
func TestWispGC_ReapSkipsRootlessTaskWithChildDepOnly(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	parent := makeGCBeadWithMetadata("no-root", now.Add(-2*time.Hour), "closed", "task", map[string]string{})
	parent.Ephemeral = true
	child := makeGCBeadWithMetadata("dep-child", now.Add(-2*time.Hour), "closed", "step", map[string]string{})
	child.Ephemeral = true
	store := newGCStore([]beads.Bead{parent, child})
	if err := store.DepAdd("dep-child", "no-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0; a rootless plain task with a dep-linked child is not a leaf", purged)
	}
	for _, id := range []string{"no-root", "dep-child"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s must be preserved: %v", id, err)
		}
	}
}

func TestWispGC_ReapDeleteErrorSurfacedAndContinues(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCOrphanWisp("orphan-err", now.Add(-2*time.Hour), "ghost-root"),
		makeGCOrphanWisp("orphan-ok", now.Add(-2*time.Hour), "ghost-root"),
	})
	store.deleteErrors["orphan-err"] = fmt.Errorf("delete failed")

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err == nil {
		t.Fatal("expected reap delete error to be surfaced")
	}
	if !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("err = %v, want delete failure to be included", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (other orphan still reaped)", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "orphan-ok")
}

// TestWispGC_DoesNotReapWhenRootGetErrors covers the fail-safe default branch of
// the orphan reaper: a non-NotFound store.Get error on the root must NOT be read
// as "root collectible". A transient read failure has to leave the descendant in
// place (so an in-flight workflow is never stripped of its closed steps) and
// surface the error rather than swallowing it.
func TestWispGC_DoesNotReapWhenRootGetErrors(t *testing.T) {
	withReapOrphansEnforced(t, true)
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCOrphanWisp("orphan-unreadable", now.Add(-2*time.Hour), "flaky-root"),
	})
	// The root read fails with a non-NotFound error (e.g. a transient store
	// outage), so collectibility cannot be proven.
	store.getErrors["flaky-root"] = fmt.Errorf("store temporarily unavailable")

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err == nil {
		t.Fatal("expected unreadable-root Get error to be surfaced")
	}
	if !strings.Contains(err.Error(), "resolving root") {
		t.Fatalf("err = %v, want error wrapping \"resolving root\"", err)
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0 when the root is unreadable", purged)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("deleted = %v, want none when the root is unreadable", store.deletedIDs)
	}
	if _, getErr := store.MemStore.Get("orphan-unreadable"); getErr != nil {
		t.Fatalf("orphan-unreadable must be preserved while its root is unreadable: %v", getErr)
	}
}

// withCloseAbandonedEnforced runs fn with the abandoned-root closer forced
// into enforce mode, restoring the prior package state afterward. Tests must
// not depend on the GC_WISP_GC_CLOSE_ABANDONED env var (which defaults to
// dry-run).
func withCloseAbandonedEnforced(t *testing.T, fn func()) {
	t.Helper()
	prev := closeAbandonedEnforced
	closeAbandonedEnforced = func() bool { return true }
	defer func() { closeAbandonedEnforced = prev }()
	fn()
}

// withCloseAbandonedTTL runs fn with the abandoned-root TTL temporarily set to
// ttl, restoring the prior value afterward.
func withCloseAbandonedTTL(t *testing.T, ttl time.Duration, fn func()) {
	t.Helper()
	prev := wispGCCloseAbandonedTTL
	wispGCCloseAbandonedTTL = ttl
	defer func() { wispGCCloseAbandonedTTL = prev }()
	fn()
}

func TestWispGC_ClosesAbandonedOpenRootWhenAllDescendantsTerminal(t *testing.T) {
	now := time.Now()
	// Root CreatedAt is recent (within the 1h closed-root purge TTL below) but
	// idle past the close TTL we shrink to 5m, so the sweep closes it without
	// the same-tick purge then deleting it (letting us assert the close state).
	store := newGCStore([]beads.Bead{
		{
			ID:        "mol-root",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Metadata:  map[string]string{"gc.formula_contract": "graph.v2"},
		},
		{
			ID:        "mol-root.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-30 * time.Minute),
			ParentID:  "mol-root",
		},
		{
			ID:        "mol-root.2",
			Status:    "tombstone",
			Type:      "task",
			CreatedAt: now.Add(-30 * time.Minute),
			ParentID:  "mol-root",
		},
	})
	if err := store.DepAdd("mol-root.1", "mol-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-root.1->mol-root): %v", err)
	}
	if err := store.DepAdd("mol-root.2", "mol-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-root.2->mol-root): %v", err)
	}

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("mol-root")
	if err != nil {
		t.Fatalf("Get(mol-root): %v", err)
	}
	if root.Status != "closed" {
		t.Fatalf("mol-root status = %q, want closed", root.Status)
	}
	if got := root.Metadata["close_reason"]; got != abandonedRootCloseReason {
		t.Fatalf("close_reason = %q, want %q", got, abandonedRootCloseReason)
	}
}

// TestWispGC_ClosesAbandonedV1MoleculeRootWithoutWorkflowMetadata proves the
// abandoned-root closer covers the same root universe as the reactive autoclose
// path. A v1 poured molecule root carries type=molecule but NEITHER gc.kind nor
// gc.formula_contract (see internal/formula/compile.go), so sourceworkflow
// .IsWorkflowRoot rejects it. The reactive autocloseMoleculeIfComplete still
// closes type=molecule roots, so when its final child-close event is lost this
// periodic backstop must close the molecule too — otherwise it leaks forever.
func TestWispGC_ClosesAbandonedV1MoleculeRootWithoutWorkflowMetadata(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "v1-mol",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
		},
		{
			ID:        "v1-mol.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-30 * time.Minute),
			ParentID:  "v1-mol",
		},
	})
	if err := store.DepAdd("v1-mol.1", "v1-mol", "parent-child"); err != nil {
		t.Fatalf("DepAdd(v1-mol.1->v1-mol): %v", err)
	}

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("v1-mol")
	if err != nil {
		t.Fatalf("Get(v1-mol): %v", err)
	}
	if root.Status != "closed" {
		t.Fatalf("v1-mol status = %q, want closed (v1 molecule backstop must close it)", root.Status)
	}
	if got := root.Metadata["close_reason"]; got != abandonedRootCloseReason {
		t.Fatalf("close_reason = %q, want %q", got, abandonedRootCloseReason)
	}
}

// TestWispGC_ClosesAbandonedInProgressGraphRootWhenAllDescendantsTerminal proves
// the abandoned-root sweep enumerates nonterminal (open AND in_progress) roots.
// Graph.v2 workflow roots are promoted to in_progress at launch
// (internal/sling/sling.go PromoteWorkflowLaunchBead), so an open-only candidate
// query would make a stale in_progress graph root with terminal descendants
// invisible to the sweep — the exact lost-finalize leak this sweep exists to
// close. The root carries type=task with gc.kind=workflow + gc.formula_contract
// =graph.v2, matching what internal/formula/compile.go emits for graph workflows.
func TestWispGC_ClosesAbandonedInProgressGraphRootWhenAllDescendantsTerminal(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "graph-root",
			Status:    "in_progress",
			Type:      "task",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			},
		},
		{
			ID:        "graph-root.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-30 * time.Minute),
			ParentID:  "graph-root",
		},
		{
			ID:        "graph-root.2",
			Status:    "tombstone",
			Type:      "task",
			CreatedAt: now.Add(-30 * time.Minute),
			ParentID:  "graph-root",
		},
	})
	if err := store.DepAdd("graph-root.1", "graph-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(graph-root.1->graph-root): %v", err)
	}
	if err := store.DepAdd("graph-root.2", "graph-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(graph-root.2->graph-root): %v", err)
	}

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("graph-root")
	if err != nil {
		t.Fatalf("Get(graph-root): %v", err)
	}
	if root.Status != "closed" {
		t.Fatalf("graph-root status = %q, want closed (in_progress graph root must be swept)", root.Status)
	}
	if got := root.Metadata["close_reason"]; got != abandonedRootCloseReason {
		t.Fatalf("close_reason = %q, want %q", got, abandonedRootCloseReason)
	}
}

// TestWispGC_CollectsClosedGraphWorkflowRoot proves the closed-root purge
// enumerates closed graph.v2 workflow roots. These compile as type=task carrying
// gc.kind=workflow and gc.formula_contract=graph.v2 (NOT type=molecule — see
// internal/formula/compile.go), so the prior molecule/wisp-only enumeration left
// a graph workflow root the abandoned-root sweep can close as permanent closed
// residue. The purge must collect the same root universe the sweep can close.
func TestWispGC_CollectsClosedGraphWorkflowRoot(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "graph-root",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			},
		},
		{
			ID:        "graph-root.step",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "graph-root",
			Metadata:  map[string]string{"gc.root_bead_id": "graph-root"},
		},
	})
	if err := store.DepAdd("graph-root.step", "graph-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(graph-root.step->graph-root): %v", err)
	}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged < 1 {
		t.Fatalf("purged = %d, want >= 1; closed graph.v2 workflow root must be collected", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "graph-root", "graph-root.step")
	if _, err := store.Get("graph-root"); err == nil {
		t.Fatal("graph-root should have been collected by the closed-root purge")
	}
}

// TestWispGC_ClosurePurgeHonorsBatchCap is the post-merge regression for the
// finding that the closed-root closure purge had no per-tick bound. The selector
// expansion (adding graph.v2/workflow roots) made a never-before-collected class
// of closed roots eligible, so the first GC tick after deploy could delete the
// whole accumulated backlog's ownership closures in one uncapped pass. With the
// cap shrunk to 1 a single sweep purges at most one root closure and the rest
// drain on later ticks; a second sweep collects the next root, proving the
// backlog clears across ticks rather than being dropped.
func TestWispGC_ClosurePurgeHonorsBatchCap(t *testing.T) {
	withClosurePurgeBatchCap(t, 1)
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-a", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBead("mol-b", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCBead("mol-c", now.Add(-2*time.Hour), "closed", "molecule"),
	})

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (closure purge batch cap bounds roots per tick)", purged)
	}
	if len(store.deletedIDs) != 1 {
		t.Fatalf("deleted = %v, want exactly 1 root closure per capped sweep", store.deletedIDs)
	}

	purged2, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC second sweep: %v", err)
	}
	if purged2 != 1 {
		t.Fatalf("second sweep purged = %d, want 1 (backlog drains across ticks)", purged2)
	}
	if len(store.deletedIDs) != 2 {
		t.Fatalf("after two sweeps deleted = %v, want 2 distinct root closures", store.deletedIDs)
	}
}

func TestWispGC_LeavesOpenRootWithLiveDescendant(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBeadWithMetadata("mol-root", now.Add(-2*time.Hour), "open", "molecule", map[string]string{"gc.formula_contract": "graph.v2"}),
		{
			ID:        "mol-root.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-root",
		},
		{
			ID:        "mol-root.2",
			Status:    "in_progress",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-root",
		},
	})
	if err := store.DepAdd("mol-root.1", "mol-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-root.1->mol-root): %v", err)
	}
	if err := store.DepAdd("mol-root.2", "mol-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-root.2->mol-root): %v", err)
	}

	withCloseAbandonedEnforced(t, func() {
		wg := newWispGC(5*time.Minute, time.Hour, 0)
		if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
			t.Fatalf("runGC: %v", err)
		}
	})

	root, err := store.Get("mol-root")
	if err != nil {
		t.Fatalf("Get(mol-root): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("mol-root status = %q, want open (live descendant)", root.Status)
	}
}

// TestWispGC_LeavesSteplessRoot pins the instantiator race window: a stepless
// root that is still INSIDE the close TTL may simply be mid-instantiation (root
// written, steps not yet), so the sweep must leave it alone. The TTL — not
// steplessness alone — is what bounds that window; see
// TestWispGC_ClosesAbandonedSteplessUnclaimedRootPastTTL for the far side of it.
func TestWispGC_LeavesSteplessRoot(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBeadWithMetadata("mol-root", now.Add(-2*time.Hour), "open", "molecule", map[string]string{"gc.formula_contract": "graph.v2"}),
	})

	withCloseAbandonedEnforced(t, func() {
		// Close TTL well beyond the root's 2h idle age: the root is inside the
		// instantiator race window.
		withCloseAbandonedTTL(t, 24*time.Hour, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("mol-root")
	if err != nil {
		t.Fatalf("Get(mol-root): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("stepless mol-root status = %q, want open (must not race instantiator)", root.Status)
	}
}

// TestWispGC_ClosesAbandonedSteplessUnclaimedRootPastTTL covers the leaked
// root-only patrol wisp (ga-98b): poured stepless, left at the unclaimed pour
// status, never picked up, idle past the TTL. Before this case the sweep
// skipped every stepless root unconditionally, so this exact shape — the one
// that actually accumulates — was the one shape GC could never reap.
func TestWispGC_ClosesAbandonedSteplessUnclaimedRootPastTTL(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "wisp-leaked",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Ephemeral: true,
			Metadata:  map[string]string{"gc.kind": "wisp"},
		},
	})

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("wisp-leaked")
	if err != nil {
		t.Fatalf("Get(wisp-leaked): %v", err)
	}
	if root.Status != "closed" {
		t.Fatalf("stepless unclaimed wisp status = %q, want closed (leaked past TTL)", root.Status)
	}
	if got := root.Metadata["close_reason"]; got != abandonedRootCloseReason {
		t.Fatalf("close_reason = %q, want %q", got, abandonedRootCloseReason)
	}
}

// TestWispGC_ClosesAssignedButUnclaimedSteplessRootPastTTL pins the behavior
// steplessRootIsAbandoned's doc comment declares intentional: routed demand
// that has sat unclaimed past the TTL is reaped. The candidate query applies
// no assignee filter, so an assigned root reaches the predicate exactly as an
// unassigned one does — asserted here so a future edit cannot flip it silently.
func TestWispGC_ClosesAssignedButUnclaimedSteplessRootPastTTL(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "wisp-routed",
			Status:    "open",
			Type:      "molecule",
			Assignee:  "repo/refinery",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Ephemeral: true,
			Metadata:  map[string]string{"gc.kind": "wisp"},
		},
	})

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("wisp-routed")
	if err != nil {
		t.Fatalf("Get(wisp-routed): %v", err)
	}
	if root.Status != "closed" {
		t.Fatalf("assigned unclaimed wisp status = %q, want closed (stale routed demand past TTL)", root.Status)
	}
}

// TestWispGC_LeavesSteplessClaimedRootPastTTL is the safety half of the
// stepless allowance. A claimed (in_progress) stepless root is held by a live
// worker, and a root bead's UpdatedAt does NOT advance while its agent works —
// so idle age alone cannot distinguish "abandoned" from "busy" here. Only the
// unclaimed pour status can, and this root no longer carries it.
func TestWispGC_LeavesSteplessClaimedRootPastTTL(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "wisp-live",
			Status:    "in_progress",
			Type:      "molecule",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Ephemeral: true,
			Metadata:  map[string]string{"gc.kind": "wisp"},
		},
	})

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("wisp-live")
	if err != nil {
		t.Fatalf("Get(wisp-live): %v", err)
	}
	if root.Status != "in_progress" {
		t.Fatalf("stepless claimed wisp status = %q, want in_progress (live worker holds it)", root.Status)
	}
}

// TestWispGC_DryRunDefaultDoesNotCloseSteplessRoot proves the stepless
// allowance inherits the sweep's dry-run default rather than bypassing it.
func TestWispGC_DryRunDefaultDoesNotCloseSteplessRoot(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "wisp-leaked",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Ephemeral: true,
			Metadata:  map[string]string{"gc.kind": "wisp"},
		},
	})

	var logOutput string
	withCloseAbandonedTTL(t, 5*time.Minute, func() {
		logOutput = captureWispGCLog(t, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("wisp-leaked")
	if err != nil {
		t.Fatalf("Get(wisp-leaked): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("stepless wisp status = %q, want open (dry-run default must not close)", root.Status)
	}
	if !strings.Contains(logOutput, "would be closed (dry-run") {
		t.Fatalf("log output = %q, want dry-run would-close log", logOutput)
	}
}

// TestWispGC_LeavesSteplessExemptRootPastTTL proves the gc.gc_exempt opt-out
// still protects a stepless unclaimed root, so a deployment can park a
// perpetual root-only root without the sweep reaping it.
func TestWispGC_LeavesSteplessExemptRootPastTTL(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "wisp-exempt",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Ephemeral: true,
			Metadata:  map[string]string{"gc.kind": "wisp", beadmeta.GCExemptMetadataKey: "true"},
		},
	})

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("wisp-exempt")
	if err != nil {
		t.Fatalf("Get(wisp-exempt): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("exempt stepless wisp status = %q, want open (gc.gc_exempt opt-out)", root.Status)
	}
}

// TestWispGC_LeavesSteplessRootWithLiveAttachmentSourcePastTTL pins the
// attached-wisp exception. privatizeAttachedRootOnlyWisp
// (internal/sling/sling.go) leaves an attached root-only wisp as a type=molecule
// root with gc.kind stripped, deliberately never routed and never claimed — the
// SOURCE bead is the claimable unit — so it is unclaimed by construction and the
// claim predicate alone would close it one TTL after pour. The source bead's
// forward molecule_id pointer is what keeps it alive; closing the root out from
// under a live source would un-block findBlockingMolecule and let a second
// attachment land on the same source bead.
func TestWispGC_LeavesSteplessRootWithLiveAttachmentSourcePastTTL(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "wisp-attached",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Ephemeral: true,
		},
		{
			ID:        "src-live",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Metadata:  map[string]string{beadmeta.MoleculeIDMetadataKey: "wisp-attached"},
		},
	})

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("wisp-attached")
	if err != nil {
		t.Fatalf("Get(wisp-attached): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("attached stepless wisp status = %q, want open (live source bead still attached)", root.Status)
	}
}

// TestWispGC_ClosesSteplessRootWhenAttachmentSourceTerminal is the far side of
// the attachment guard: once the source bead goes terminal there is no live
// attachment state left to protect, so the root reaps normally. Without this
// case the guard above could silently blunt the fix into "never close a
// stepless root" again.
func TestWispGC_ClosesSteplessRootWhenAttachmentSourceTerminal(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID:        "wisp-attached",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Ephemeral: true,
		},
		{
			ID:        "src-done",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-30 * time.Minute),
			UpdatedAt: now.Add(-30 * time.Minute),
			Metadata:  map[string]string{beadmeta.MoleculeIDMetadataKey: "wisp-attached"},
		},
	})

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("wisp-attached")
	if err != nil {
		t.Fatalf("Get(wisp-attached): %v", err)
	}
	if root.Status != "closed" {
		t.Fatalf("attached stepless wisp status = %q, want closed (source bead terminal)", root.Status)
	}
	if got := root.Metadata["close_reason"]; got != abandonedRootCloseReason {
		t.Fatalf("close_reason = %q, want %q", got, abandonedRootCloseReason)
	}
}

func TestWispGC_RespectsTTLCutoff(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		// Root last active 30m ago — younger than the 1h close TTL below.
		{
			ID:        "mol-root",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: now.Add(-2 * time.Hour),
			UpdatedAt: now.Add(-30 * time.Minute),
			Metadata:  map[string]string{"gc.formula_contract": "graph.v2"},
		},
		{
			ID:        "mol-root.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-root",
		},
	})
	if err := store.DepAdd("mol-root.1", "mol-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-root.1->mol-root): %v", err)
	}

	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, time.Hour, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("mol-root")
	if err != nil {
		t.Fatalf("Get(mol-root): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("mol-root status = %q, want open (within TTL)", root.Status)
	}
}

func TestWispGC_SkipsZFCExemptRoot(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBeadWithMetadata("zfc-root", now.Add(-2*time.Hour), "open", "molecule", map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.gc_exempt":        "true",
		}),
		{
			ID:        "zfc-root.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "zfc-root",
		},
	})
	if err := store.DepAdd("zfc-root.1", "zfc-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(zfc-root.1->zfc-root): %v", err)
	}

	withCloseAbandonedEnforced(t, func() {
		wg := newWispGC(5*time.Minute, time.Hour, 0)
		if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
			t.Fatalf("runGC: %v", err)
		}
	})

	root, err := store.Get("zfc-root")
	if err != nil {
		t.Fatalf("Get(zfc-root): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("ZFC-exempt root status = %q, want open (must never auto-close)", root.Status)
	}
}

func TestWispGC_DryRunDefaultDoesNotClose(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBeadWithMetadata("mol-root", now.Add(-2*time.Hour), "open", "molecule", map[string]string{"gc.formula_contract": "graph.v2"}),
		{
			ID:        "mol-root.1",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-root",
		},
	})
	if err := store.DepAdd("mol-root.1", "mol-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-root.1->mol-root): %v", err)
	}

	// Default (no enforce override): closeAbandonedEnforced reads the env var
	// which is unset under the env-stripped test harness, so the sweep must be
	// dry-run and mutate nothing — but it should log the would-close candidate.
	// Shrink the close TTL so the 2h-idle root is eligible (otherwise the TTL
	// guard would skip it and there would be nothing to dry-run-log).
	var logOutput string
	withCloseAbandonedTTL(t, 5*time.Minute, func() {
		logOutput = captureWispGCLog(t, func() {
			wg := newWispGC(5*time.Minute, time.Hour, 0)
			if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})

	root, err := store.Get("mol-root")
	if err != nil {
		t.Fatalf("Get(mol-root): %v", err)
	}
	if root.Status != "open" {
		t.Fatalf("mol-root status = %q, want open (dry-run default must not close)", root.Status)
	}
	if !strings.Contains(logOutput, "would be closed (dry-run") {
		t.Fatalf("log output = %q, want dry-run would-close log", logOutput)
	}
}

type gcQueryKey struct {
	Status   string
	Type     string
	Label    string
	Metadata string
}

type gcTestStore struct {
	*beads.MemStore
	listErrors   map[gcQueryKey]error
	deleteErrors map[string]error
	getErrors    map[string]error
	deletedIDs   []string
	// deleteAttempts records every Delete call, success or failure, so tests can
	// assert that a batch cap bounds delete ATTEMPTS and not merely successful
	// deletes (a failed delete leaves no trace in deletedIDs).
	deleteAttempts []string
	// childrenCalls and depListCalls count the leaf-ness probes the rootless
	// orphan branch performs. Each is one backend read in production (BdStore
	// runs a bd subprocess per call), so tests assert the probe cap bounds them
	// even in the dry-run default where no delete is ever attempted.
	childrenCalls int
	depListCalls  int
}

func newGCStore(existing []beads.Bead) *gcTestStore {
	return &gcTestStore{
		MemStore:     beads.NewMemStoreFrom(0, existing, nil),
		listErrors:   map[gcQueryKey]error{},
		deleteErrors: map[string]error{},
		getErrors:    map[string]error{},
	}
}

func (s *gcTestStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if err := s.listErrors[gcQueryKey{Status: query.Status, Type: query.Type, Label: query.Label, Metadata: metadataQueryKey(query.Metadata)}]; err != nil {
		return nil, err
	}
	return s.MemStore.List(query)
}

func (s *gcTestStore) Get(id string) (beads.Bead, error) {
	if err := s.getErrors[id]; err != nil {
		return beads.Bead{}, err
	}
	return s.MemStore.Get(id)
}

func (s *gcTestStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.childrenCalls++
	return s.MemStore.Children(parentID, opts...)
}

func (s *gcTestStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depListCalls++
	return s.MemStore.DepList(id, direction)
}

func (s *gcTestStore) Delete(id string) error {
	s.deleteAttempts = append(s.deleteAttempts, id)
	if err := s.deleteErrors[id]; err != nil {
		return err
	}
	if err := s.MemStore.Delete(id); err != nil {
		return err
	}
	s.deletedIDs = append(s.deletedIDs, id)
	return nil
}

//nolint:unparam // helper mirrors makeGCBeadWithLabels signature for readability
func makeGCBead(id string, createdAt time.Time, status, beadType string) beads.Bead {
	return makeGCBeadWithLabels(id, createdAt, status, beadType)
}

func makeGCBeadWithLabels(id string, createdAt time.Time, status, beadType string, labels ...string) beads.Bead {
	// Order-tracking beads live in the no-history tier in production;
	// mirror that here so wisp_gc's tier-aware queries see them.
	noHistory := false
	for _, l := range labels {
		if l == labelOrderTracking {
			noHistory = true
			break
		}
	}
	return beads.Bead{
		ID:        id,
		Status:    status,
		Type:      beadType,
		CreatedAt: createdAt,
		Labels:    labels,
		NoHistory: noHistory,
	}
}

func makeGCBeadWithMetadata(id string, createdAt time.Time, status, beadType string, metadata map[string]string) beads.Bead {
	bead := makeGCBead(id, createdAt, status, beadType)
	bead.Metadata = metadata
	return bead
}

func makeGCMessageWisp(id string, createdAt time.Time, metadata map[string]string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Status:    "open",
		Type:      "message",
		CreatedAt: createdAt,
		Metadata:  metadata,
		Ephemeral: true,
	}
}

// makeGCOrphanWisp builds a closed wisp-tier descendant carrying a
// gc.root_bead_id pointer. Ephemeral:true places it in the wisp tier so the
// orphan reaper's TierWisps query sees it. It deliberately omits gc.kind=wisp so
// the root-rooted closure purge does not enumerate it as a root.
func makeGCOrphanWisp(id string, createdAt time.Time, rootID string) beads.Bead {
	bead := makeGCBeadWithMetadata(id, createdAt, "closed", "task", map[string]string{
		beadmeta.RootBeadIDMetadataKey: rootID,
	})
	bead.Ephemeral = true
	return bead
}

// withReapOrphansEnforced toggles the orphan-reap enforcement indirection for
// the duration of a test, restoring the prior value on cleanup.
func withReapOrphansEnforced(t *testing.T, enforce bool) {
	t.Helper()
	prev := reapOrphansEnforced
	reapOrphansEnforced = func() bool { return enforce }
	t.Cleanup(func() { reapOrphansEnforced = prev })
}

// withReapOrphanBatchCap overrides the per-sweep reap cap for the duration of a
// test, restoring the prior value on cleanup.
func withReapOrphanBatchCap(t *testing.T, batchCap int) {
	t.Helper()
	prev := wispGCReapOrphanBatchCap
	wispGCReapOrphanBatchCap = batchCap
	t.Cleanup(func() { wispGCReapOrphanBatchCap = prev })
}

// withReapOrphanProbeCap overrides the per-sweep rootless leaf-ness probe cap
// for the duration of a test, restoring the prior value on cleanup.
func withReapOrphanProbeCap(t *testing.T, probeCap int) {
	t.Helper()
	prev := wispGCReapOrphanProbeCap
	wispGCReapOrphanProbeCap = probeCap
	t.Cleanup(func() { wispGCReapOrphanProbeCap = prev })
}

// withClosurePurgeBatchCap overrides the per-sweep closed-root closure purge cap
// for the duration of a test, restoring the prior value on cleanup.
func withClosurePurgeBatchCap(t *testing.T, batchCap int) {
	t.Helper()
	prev := wispGCClosurePurgeBatchCap
	wispGCClosurePurgeBatchCap = batchCap
	t.Cleanup(func() { wispGCClosurePurgeBatchCap = prev })
}

func captureWispGCLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()
	fn()
	return buf.String()
}

func metadataQueryKey(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+metadata[key])
	}
	return strings.Join(parts, "\x00")
}

func assertDeletedIDs(t *testing.T, deleted []string, want ...string) {
	t.Helper()
	if len(deleted) != len(want) {
		t.Fatalf("deleted = %v, want %v", deleted, want)
	}
	seen := map[string]bool{}
	for _, id := range deleted {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("deleted = %v, want %v", deleted, want)
		}
	}
}

var _ beads.Store = (*gcTestStore)(nil)
