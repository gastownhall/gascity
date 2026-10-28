package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestFormulaCookMetaRefusesAClassFlipOnASplitCity is the `gc formula cook
// --meta` entry point's table: the audit's R3 (a v1 poured root stamped
// gc.root_bead_id in the work store, after which every routed gc command
// refused), and the reverse inside the binding. A refused cook writes nothing
// at all: the guard runs before the molecule is instantiated, not after. On a
// single-store city every row cooks and stamps exactly as before.
func TestFormulaCookMetaRefusesAClassFlipOnASplitCity(t *testing.T) {
	cases := []struct {
		name, formula string
		meta          []string
		splitRefuses  bool
		want          string
	}{
		{"v1 poured root given gc.root_bead_id", "legacy-work", []string{"gc.root_bead_id=gcg-999"}, true, "work-class bead into a graph-class bead"},
		{"v1 poured root given gc.kind=wisp", "legacy-work", []string{"gc.kind=wisp"}, true, "work-class bead into a graph-class bead"},
		{"v1 poured root given gc.kind=workflow", "legacy-work", []string{"gc.kind=workflow"}, true, "work-class bead into a graph-class bead"},
		{"v1 poured root given gc.formula_contract", "legacy-work", []string{"gc.formula_contract=graph.v2"}, true, "work-class bead into a graph-class bead"},
		{"v1 wisp root stripped of gc.kind", "vapor-work", []string{"gc.kind=task"}, true, "graph-class bead into a work-class bead"},
		{"v1 poured root, ordinary meta", "legacy-work", []string{"gc.trace=abc123"}, false, ""},
		{"graph.v2 root, ordinary meta", "graph-work", []string{"gc.trace=abc123"}, false, ""},
	}
	for _, c := range cases {
		for _, split := range []bool{true, false} {
			arm := "single-store"
			if split {
				arm = "split"
			}
			t.Run(arm+"/"+c.name, func(t *testing.T) {
				cityDir := oneShotCookCity(t)
				var graph beads.Store
				if split {
					graph = splittest.NewClassStore(t, config.BeadClassGraph)
					seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))
				} else {
					seedCLIStorageRoutes(t, cityDir, nil)
				}
				args := []string{}
				for _, m := range c.meta {
					args = append(args, "--meta", m)
				}
				out, err := cookFormulaErr(t, c.formula, args...)
				if !split || !c.splitRefuses {
					if err != nil {
						t.Fatalf("cook failed: %v\n%s", err, out)
					}
					return
				}
				if err == nil {
					t.Fatalf("cook succeeded; want the class-flip refusal\n%s", out)
				}
				if !strings.Contains(out+err.Error(), c.want) {
					t.Errorf("refusal does not carry %q: %v\n%s", c.want, err, out)
				}
				work, openErr := openStoreAtForCity(cityDir, cityDir)
				if openErr != nil {
					t.Fatal(openErr)
				}
				for name, store := range map[string]beads.Store{"work": work, "graph": graph} {
					rows, listErr := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
					if listErr != nil {
						t.Fatal(listErr)
					}
					if len(rows) > 0 {
						t.Errorf("a refused cook wrote %d bead(s) to the %s store: %+v", len(rows), name, rows)
					}
				}
			})
		}
	}
}

// TestFormulaCookMetaFlipCheckUsesTheRecipeRoot pins cookMetaClassFlip on the
// recipe shapes directly, including the holds a single-store city answers.
func TestFormulaCookMetaFlipCheckUsesTheRecipeRoot(t *testing.T) {
	if flip := cookMetaClassFlip(nil, map[string]string{"gc.kind": "wisp"}, []coordclass.Class{coordclass.ClassWork}); flip == nil {
		t.Fatal("a nil recipe judged as a work root must still refuse a graph stamp in a work store")
	}
	if flip := cookMetaClassFlip(nil, map[string]string{"gc.kind": "wisp"}, nil); flip != nil {
		t.Fatalf("single-store holds refused: %v", flip)
	}
}
