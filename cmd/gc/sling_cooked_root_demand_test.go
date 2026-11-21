package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestSlingCookedRootCreatesDemandForTarget reproduces #9039: `gc formula
// cook` followed by `gc sling <agent> <root>` must leave capacity demand for
// <agent>, so the controller wakes an on-demand worker to run the workflow.
// The expanded root stays a structural latch excluded from demand (#7497);
// the sling routes the run's unrouted executable steps instead.
func TestSlingCookedRootCreatesDemandForTarget(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	f, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open city.toml: %v", err)
	}
	if _, err := f.WriteString("\n[[agent]]\nname = \"worker\"\nmax_active_sessions = -1\nmin_active_sessions = 0\n"); err != nil {
		t.Fatalf("append agent: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close city.toml: %v", err)
	}

	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("open the work store: %v", err)
	}
	// The shape `gc formula cook` leaves: an expanded workflow root and its
	// unrouted executable step beads.
	root, err := work.Create(beads.Bead{
		Title:  "pancakes",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.WorkflowExpandedMetadataKey: "true",
			beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
			"gc.scope_kind":                      "formula-cook",
		},
	})
	if err != nil {
		t.Fatalf("create cooked root: %v", err)
	}
	var stepIDs []string
	for _, title := range []string{"mix batter", "cook"} {
		step, err := work.Create(beads.Bead{
			Title:    title,
			Type:     "task",
			Status:   "open",
			ParentID: root.ID,
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: root.ID,
				beadmeta.RoutedToMetadataKey:   "",
			},
		})
		if err != nil {
			t.Fatalf("create step %q: %v", title, err)
		}
		stepIDs = append(stepIDs, step.ID)
	}

	stdout, _ := runCmdSling(t, "worker", root.ID)

	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("reopen the work store: %v", err)
	}
	for _, id := range stepIDs {
		step, err := store.Get(id)
		if err != nil {
			t.Fatalf("get step %s: %v", id, err)
		}
		if got := step.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
			t.Errorf("step %s gc.routed_to = %q, want worker", id, got)
		}
	}
	counts, _, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{{
		template: "worker",
		storeKey: "city",
		store:    store,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errs = %v", errs)
	}
	if got := counts["worker"]; got == 0 {
		ready, _ := store.Ready()
		for _, b := range ready {
			t.Logf("ready %s kind=%q expanded=%q routed_to=%q", b.ID, b.Metadata[beadmeta.KindMetadataKey], b.Metadata[beadmeta.WorkflowExpandedMetadataKey], b.Metadata[beadmeta.RoutedToMetadataKey])
		}
		t.Fatalf("demand for worker after `gc sling worker %s` (cooked root) = 0, want > 0: no on-demand worker is ever woken; stdout=%s", root.ID, stdout)
	}
	slungRoot, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got := controllerDemandRouteCandidates(slungRoot); got != nil {
		t.Fatalf("controllerDemandRouteCandidates(root) = %v, want nil: the expanded root must stay out of demand (#7497)", got)
	}
}
