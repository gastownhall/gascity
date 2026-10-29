package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestBeadsMetadataCASRefusesAClassFlipOnASplitCity is the `gc beads
// metadata-cas` entry point's table. It writes one caller-named metadata key
// on a bead in an exact city or rig WORK store, and the key grammar admits
// gc.kind and gc.root_bead_id; on a split city a swap that would make the
// work bead graph class is refused before the CAS runs. On a single-store city
// it swaps exactly as before.
func TestBeadsMetadataCASRefusesAClassFlipOnASplitCity(t *testing.T) {
	cases := []struct {
		name, key, next string
		refuse          bool
	}{
		{"gc.kind=workflow", "gc.kind", "workflow", true},
		{"gc.kind=wisp", "gc.kind", "wisp", true},
		{"gc.root_bead_id", "gc.root_bead_id", "gcg-1", true},
		{"gc.formula_contract", "gc.formula_contract", "graph.v2", true},
		{"ordinary key", "review_sha", "new", false},
	}
	for _, c := range cases {
		for _, split := range []bool{true, false} {
			arm := "single-store"
			if split {
				arm = "split"
			}
			t.Run(arm+"/"+c.name, func(t *testing.T) {
				clearGCEnv(t)
				configureIsolatedRuntimeEnv(t)
				cityPath := writeMetadataCASTestCity(t)
				if split {
					f, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.WriteString(bdSQLRefusalSplitStorage); err != nil {
						t.Fatal(err)
					}
					_ = f.Close()
				}
				store := beads.NewMemStore()
				bead, err := store.Create(beads.Bead{Title: "w", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return store, nil }, func(beads.Store) error { return nil })

				stdout, stderr, code := runMetadataCASTestCommandForStoreKey(cityPath, "city:demo", bead.ID, c.key, "--expected=", "--next="+c.next)
				got, err := store.Get(bead.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !split || !c.refuse {
					if code != 0 || got.Metadata[c.key] != c.next {
						t.Fatalf("code=%d meta[%s]=%q; want the swap; stdout=%q stderr=%q", code, c.key, got.Metadata[c.key], stdout, stderr)
					}
					return
				}
				if code == 0 {
					t.Fatalf("the swap ran; want the class-flip refusal; stdout=%q", stdout)
				}
				if class := coordclass.Classify(got); class != coordclass.ClassWork {
					t.Fatalf("refused swap still landed: bead is %s class", class)
				}
				if !strings.Contains(stderr, "work-class bead into a graph-class bead") || !strings.Contains(stderr, "gc sling") {
					t.Errorf("refusal = %q", stderr)
				}
			})
		}
	}
}

func runMetadataCASTestCommandForStoreKey(cityPath, storeRef, beadID, key string, extraArgs ...string) (stdout, stderr string, code int) {
	args := append([]string{"--key", key}, extraArgs...)
	// The shared runner pins --key review_sha first; a later --key wins.
	return runMetadataCASTestCommandForStore(cityPath, storeRef, beadID, args...)
}
