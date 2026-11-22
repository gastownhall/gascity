package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// cookAttachBlockedSource opens a single-store cook city's work store and
// creates an open blocker and a source bead with a blocks dependency on it.
func cookAttachBlockedSource(t *testing.T) (work beads.Store, blocker, source beads.Bead) {
	t.Helper()
	cityDir := oneShotCookCity(t)
	seedCLIStorageRoutes(t, cityDir, nil)
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	blocker, err = work.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	source, err = work.Create(beads.Bead{Title: "attach target", Type: "task"})
	if err != nil {
		t.Fatalf("create attach bead: %v", err)
	}
	if err := work.DepAdd(source.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	return work, blocker, source
}

// readyCookedWorkflowBeads returns the Ready beads of the workflow rootID.
func readyCookedWorkflowBeads(t *testing.T, work beads.Store, rootID string) []string {
	t.Helper()
	ready, err := work.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	var out []string
	for _, b := range ready {
		if b.ID == rootID || b.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID {
			out = append(out, b.ID+" "+b.Title)
		}
	}
	return out
}

// TestFormulaCookAttachGraphV2WaitsForSourceBlockers is AC-5: cooking a
// graph.v2 formula onto a blocked bead with --attach creates a workflow that
// waits for the blocker, and names the blocker in the output.
func TestFormulaCookAttachGraphV2WaitsForSourceBlockers(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		work, blocker, source := cookAttachBlockedSource(t)

		res := cookFormula(t, "graph-work", "--attach", source.ID)

		if ready := readyCookedWorkflowBeads(t, work, res.RootID); len(ready) > 0 {
			t.Errorf("cooked workflow beads Ready while %s is open: %v", blocker.ID, ready)
		}
		if !slices.Equal(res.WaitingOn, []string{blocker.ID}) {
			t.Errorf("waiting_on = %v, want [%s]", res.WaitingOn, blocker.ID)
		}
	})
	t.Run("text", func(t *testing.T) {
		work, blocker, source := cookAttachBlockedSource(t)

		out, err := cookFormulaErr(t, "graph-work", "--attach", source.ID)
		if err != nil {
			t.Fatalf("gc formula cook --attach: %v\n%s", err, out)
		}
		if want := "Waits for: " + blocker.ID + "\n"; !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
		roots, err := work.List(beads.ListQuery{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}})
		if err != nil {
			t.Fatalf("list workflow roots: %v", err)
		}
		if len(roots) != 1 {
			t.Fatalf("workflow roots = %d, want 1", len(roots))
		}
		if ready := readyCookedWorkflowBeads(t, work, roots[0].ID); len(ready) > 0 {
			t.Errorf("cooked workflow beads Ready while %s is open: %v", blocker.ID, ready)
		}
	})
}
