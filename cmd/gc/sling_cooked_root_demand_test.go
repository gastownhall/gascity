package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// seedCookedRun writes the shape `gc formula cook` leaves into store, the
// store a cook mints the run in: an expanded workflow root and its unrouted
// executable step beads.
func seedCookedRun(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title:  "pancakes",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.WorkflowExpandedMetadataKey: "true",
			beadmeta.ScopeKindMetadataKey:        "formula-cook",
		},
	})
	if err != nil {
		t.Fatalf("create cooked root: %v", err)
	}
	for _, title := range []string{"mix batter", "cook"} {
		if _, err := store.Create(beads.Bead{
			Title:    title,
			Type:     "task",
			Status:   "open",
			ParentID: root.ID,
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: root.ID,
			},
		}); err != nil {
			t.Fatalf("create step %q: %v", title, err)
		}
	}
	return root
}

// TestSlingCookedRootCreatesDemandForTarget reproduces #9039 on a city that
// keeps its workflows in the work store: `gc formula cook` followed by `gc
// sling <agent> <root>` must leave capacity demand for <agent>, so the
// controller wakes an on-demand worker to run the workflow. The expanded root
// is a structural latch excluded from demand, and its executable steps carry
// no route, so nothing is counted for the target.
//
// A split city keeps the cooked run in its infrastructure store, where gc
// sling refuses the root (#5974) and the run stays unrouted: that city
// launches the formula with --formula instead.
func TestSlingCookedRootCreatesDemandForTarget(t *testing.T) {
	t.Run("single-store", func(t *testing.T) {
		skipUnderSplitStrictGate(t)
		cityPath := oneShotCLICity(t, "")
		appendCityTOML(t, cityPath, onDemandWorkerTOML)
		work, err := openStoreAtForCity(cityPath, cityPath)
		if err != nil {
			t.Fatalf("open the work store: %v", err)
		}
		root := seedCookedRun(t, work)

		runCmdSling(t, "worker", root.ID)

		store, err := openStoreAtForCity(cityPath, cityPath)
		if err != nil {
			t.Fatalf("reopen the work store: %v", err)
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
			t.Fatalf("demand for worker after `gc sling worker %s` (cooked root) = 0, want > 0: no on-demand worker is ever woken", root.ID)
		}
		// The fix routes the run's members, not the latch: the #7497 guard
		// that keeps an expanded root out of demand must keep returning nil
		// for it.
		got, err := store.Get(root.ID)
		if err != nil {
			t.Fatalf("reread the cooked root: %v", err)
		}
		if candidates := controllerDemandRouteCandidates(got); candidates != nil {
			t.Fatalf("controllerDemandRouteCandidates(expanded root) = %v, want nil: the #7497 demand guard changed", candidates)
		}
	})

	t.Run("split", func(t *testing.T) {
		cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
		appendCityTOML(t, cityPath, onDemandWorkerTOML)
		root := seedCookedRun(t, splitCityGraphBinding(t, cityPath))
		assertSplitCityRefusesCookedRootSling(t, cityPath, root.ID)
	})
}
