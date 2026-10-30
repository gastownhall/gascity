package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// TestRunTargetBackfillCheckFindsWorkflowRootsInTheGraphBinding is read-audit
// R13: workflow roots are graph class, so on a split city they live in the
// graph binding. The doctor check scanned only the work-store scopes, reported
// nothing to backfill and --fix repaired nothing there.
func TestRunTargetBackfillCheckFindsWorkflowRootsInTheGraphBinding(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	root, err := binding.Create(beads.Bead{
		Title: "legacy workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:      beadmeta.KindWorkflow,
			beadmeta.RunTargetMetadataKey: "pool/worker",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	work := splittest.NewWorkStore(t, "gc")
	check := newRunTargetRoutedToBackfillCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })

	targets, skipped := check.collect()
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	if len(targets) != 1 || targets[0].beadID != root.ID {
		t.Fatalf("targets = %+v, want the binding's root %s", targets, root.ID)
	}
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, err := binding.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "pool/worker" {
		t.Fatalf("root %s gc.routed_to = %q after --fix, want pool/worker", root.ID, got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// TestWaitDependencyReadReportsAStoreThatWillNotOpen is read-audit R15: a rig
// store that failed to open was skipped and the read ended in ErrNotFound, which
// the wait consumes as a DELETED dependency. It now fails with the open error.
func TestWaitDependencyReadReportsAStoreThatWillNotOpen(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "rigs", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[workspace]\nname = \"test-city\"\nprefix = \"gc\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"rigs/alpha\"\nprefix = \"al\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := waitDependencyScopeStore
	waitDependencyScopeStore = func(string, string) (beads.Store, error) {
		return nil, errors.New("the rig's bead store is unavailable")
	}
	t.Cleanup(func() { waitDependencyScopeStore = prev })

	_, err := loadWaitDependencyBead(cityPath, beads.NewMemStore(), "al-1")
	if err == nil {
		t.Fatal("the read succeeded although the rig store holding al-1 could not be opened")
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the read answered not-found (%v): a wait consumes that as a deleted dependency", err)
	}
}
