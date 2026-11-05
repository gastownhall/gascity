package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// splitStoreHandle is a second handle onto the same store, so the resolution
// takes its split-class path — one listing per question — over identical data.
type splitStoreHandle struct {
	beads.Store
}

// stepFixtureBead is one seeded bead; age orders created_at (larger is
// older) and fresh orders updated_at (larger is more recently updated).
type stepFixtureBead struct {
	id, typ, status, assignee, parent, description string
	age, fresh                                     int
	meta                                           map[string]string
}

func seedStepFixture(beadsIn []stepFixtureBead) *beads.MemStore {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seeded := make([]beads.Bead, 0, len(beadsIn))
	for _, in := range beadsIn {
		seeded = append(seeded, beads.Bead{
			ID:          in.id,
			Title:       "title " + in.id,
			Type:        in.typ,
			Status:      in.status,
			Assignee:    in.assignee,
			ParentID:    in.parent,
			Description: in.description,
			Metadata:    in.meta,
			CreatedAt:   base.Add(-time.Duration(in.age) * time.Minute),
			UpdatedAt:   base.Add(time.Duration(in.fresh) * time.Minute),
		})
	}
	return beads.NewMemStoreFrom(1000, seeded, nil)
}

// TestActiveWispStepSingleReadMatchesPerQuestionReads pins that answering the
// step reminder's in-progress questions from one listing resolves the same
// bead as asking each question its own listing, across every resolution
// branch: molecule, wisp, the newest-updated-of-five rule and its tie,
// in-progress and entry steps, the claimed and routed bridges, and the
// description fallback with its ten-bead sample.
func TestActiveWispStepSingleReadMatchesPerQuestionReads(t *testing.T) {
	ip, open := "in_progress", "open"
	me := "probe-a"
	manyClaimed := func(n int) []stepFixtureBead {
		var out []stepFixtureBead
		for i := 0; i < n; i++ {
			out = append(out, stepFixtureBead{id: fmt.Sprintf("w-%02d", i), typ: "task", status: ip, assignee: me, age: i})
		}
		return out
	}
	cases := map[string][]stepFixtureBead{
		"nothing": nil,
		"description": {
			{id: "t-empty", typ: "task", status: ip, assignee: me, age: 1},
			{id: "t-desc", typ: "task", status: ip, assignee: me, age: 2, description: "do it"},
			{id: "t-other", typ: "task", status: ip, assignee: "someone", age: 0, description: "not mine"},
		},
		"description beyond the sample": append(manyClaimed(10),
			stepFixtureBead{id: "t-old", typ: "task", status: ip, assignee: me, age: 50, description: "too old"}),
		"molecule with in-progress step": {
			{id: "m-1", typ: "molecule", status: ip, assignee: me, age: 3},
			{id: "s-1", typ: "step", status: ip, parent: "m-1", age: 2, description: "step one"},
			{id: "t-desc", typ: "task", status: ip, assignee: me, age: 0, description: "decoy"},
		},
		"molecule entry step": {
			{id: "m-1", typ: "molecule", status: ip, assignee: me, age: 3},
			{id: "s-2", typ: "step", status: open, parent: "m-1", age: 1, description: "second"},
			{id: "s-1", typ: "step", status: open, parent: "m-1", age: 2, description: "first"},
		},
		"wisp root": {
			{id: "w-1", typ: "wisp", status: ip, assignee: me, age: 3},
			{id: "s-1", typ: "step", status: ip, parent: "w-1", age: 2, description: "wisp step"},
		},
		"molecule preferred over wisp": {
			{id: "w-1", typ: "wisp", status: ip, assignee: me, age: 1, fresh: 9},
			{id: "m-1", typ: "molecule", status: ip, assignee: me, age: 3},
			{id: "s-w", typ: "step", status: ip, parent: "w-1", age: 0, description: "wisp step"},
			{id: "s-m", typ: "step", status: ip, parent: "m-1", age: 0, description: "molecule step"},
		},
		"newest updated of the five newest molecules": {
			{id: "m-0", typ: "molecule", status: ip, assignee: me, age: 0, fresh: 1},
			{id: "m-1", typ: "molecule", status: ip, assignee: me, age: 1, fresh: 2},
			{id: "m-2", typ: "molecule", status: ip, assignee: me, age: 2, fresh: 7},
			{id: "m-3", typ: "molecule", status: ip, assignee: me, age: 3, fresh: 3},
			{id: "m-4", typ: "molecule", status: ip, assignee: me, age: 4, fresh: 4},
			{id: "m-5", typ: "molecule", status: ip, assignee: me, age: 5, fresh: 99},
			{id: "s-2", typ: "step", status: ip, parent: "m-2", description: "m-2 step"},
			{id: "s-5", typ: "step", status: ip, parent: "m-5", description: "m-5 step (outside the sample)"},
		},
		"updated tie keeps the newest created": {
			{id: "m-0", typ: "molecule", status: ip, assignee: me, age: 0, fresh: 5},
			{id: "m-1", typ: "molecule", status: ip, assignee: me, age: 1, fresh: 5},
			{id: "s-0", typ: "step", status: ip, parent: "m-0", description: "m-0 step"},
			{id: "s-1", typ: "step", status: ip, parent: "m-1", description: "m-1 step"},
		},
		"claimed bridge": {
			{id: "root", typ: "molecule", status: open, age: 9},
			{id: "s-1", typ: "step", status: ip, parent: "root", description: "bridged step"},
			{id: "t-newer", typ: "task", status: ip, assignee: me, age: 0, description: "decoy"},
			{id: "t-src", typ: "task", status: ip, assignee: me, age: 1, meta: map[string]string{beadmeta.MoleculeIDMetadataKey: "root"}},
		},
		"routed bridge": {
			{id: "root", typ: "molecule", status: open, age: 9},
			{id: "s-1", typ: "step", status: open, parent: "root", description: "entry step"},
			{id: "t-src", typ: "task", status: open, age: 1, meta: map[string]string{beadmeta.MoleculeIDMetadataKey: "root", beadmeta.RoutedToMetadataKey: me}},
		},
	}
	wantStep := map[string]string{
		"nothing":                                     "<none>",
		"description":                                 "t-desc",
		"description beyond the sample":               "<none>",
		"molecule with in-progress step":              "s-1",
		"molecule entry step":                         "s-1",
		"wisp root":                                   "s-1",
		"molecule preferred over wisp":                "s-m",
		"newest updated of the five newest molecules": "s-2",
		"updated tie keeps the newest created":        "s-0",
		"claimed bridge":                              "s-1",
		"routed bridge":                               "s-1",
	}
	assignees := []string{me, "tc-1"}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			store := seedStepFixture(fixture)
			want, wantErr := resolveActiveWispStep(store, &splitStoreHandle{Store: store}, assignees)
			rec := beadstest.NewOpRecordingStore(store)
			got, gotErr := resolveActiveWispStep(rec, rec, assignees)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("errors differ: single read %v, per question %v", gotErr, wantErr)
			}
			if stepID(got) != stepID(want) {
				t.Fatalf("single read resolved %q, per-question reads %q", stepID(got), stepID(want))
			}
			if stepID(got) != wantStep[name] {
				t.Fatalf("resolved %q, want %q", stepID(got), wantStep[name])
			}
			claimedLists := 0
			for _, op := range rec.Ops() {
				if op.Kind == beadstest.OpList && op.Query.Status == "in_progress" && len(op.Query.Assignees) > 0 {
					claimedLists++
				}
			}
			if claimedLists != 1 {
				t.Fatalf("asked the store %d in-progress questions for the identities, want 1", claimedLists)
			}
		})
	}
}

func stepID(b *beads.Bead) string {
	if b == nil {
		return "<none>"
	}
	return b.ID
}
