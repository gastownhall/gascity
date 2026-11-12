package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestConvoyList_CharacterizationGolden freezes the per-lane behavior of
// `gc convoy list` across the three routing lanes (remote / local-controller-
// alive / serverless). The seed exposes what the API route used to drop: an
// owned convoy with a closed legacy child, an open tracked child and a
// dangling track; an empty convoy; and a synthetic convoy the listing hides.
// Rows print in ID order on every lane, so the three lanes' stdout and json
// sections are byte-identical; only the stderr route lines and the request
// counts (1 list page + one /check per listed convoy) differ.
//
// Regenerate existing goldens (the flag writes through the runfiles symlinks
// and cannot create a missing golden) with:
//
//	bazel run --test_sharding_strategy=disabled //cmd/gc:gc_test -- -test.run='^TestConvoyList_CharacterizationGolden$' -chartest-update
func TestConvoyList_CharacterizationGolden(t *testing.T) {
	h := newCharCity(t, charCityBasic, func(t *testing.T, store beads.Store) {
		alpha, err := store.Create(beads.Bead{Title: "Alpha convoy", Type: "convoy", Labels: []string{"owned"}, Metadata: map[string]string{"target": "main"}})
		if err != nil {
			t.Fatalf("seed convoy: %v", err)
		}
		legacy, err := store.Create(beads.Bead{Title: "legacy child", ParentID: alpha.ID})
		if err != nil {
			t.Fatalf("seed legacy child: %v", err)
		}
		if err := store.Close(legacy.ID); err != nil {
			t.Fatalf("close legacy child: %v", err)
		}
		tracked, err := store.Create(beads.Bead{Title: "tracked child", Assignee: "assignee-a"})
		if err != nil {
			t.Fatalf("seed tracked child: %v", err)
		}
		if err := store.DepAdd(alpha.ID, tracked.ID, "tracks"); err != nil {
			t.Fatalf("track child: %v", err)
		}
		if err := store.DepAdd(alpha.ID, "gc-missing", "tracks"); err != nil {
			t.Fatalf("dangling track: %v", err)
		}
		if _, err := store.Create(beads.Bead{Title: "Beta convoy", Type: "convoy"}); err != nil {
			t.Fatalf("seed convoy: %v", err)
		}
		if _, err := store.Create(beads.Bead{Title: "launch input", Type: "convoy", Metadata: map[string]string{"gc.synthetic": "true"}}); err != nil {
			t.Fatalf("seed synthetic convoy: %v", err)
		}
	})
	h.runCharGolden(t, charCommand{
		name: "convoy-list",
		route: func(cityPath string, c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
			return routeConvoyList(cityPath, c, nilReason, convoyListOptions{jsonOut: jsonOut}, stdout, stderr)
		},
		readback: convoyReadback,
	})
}
