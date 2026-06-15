package worker

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestResolveRunID(t *testing.T) {
	const session = "s-bead-9"
	cases := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "workflow_id wins (graph workflow)",
			bead: beads.Bead{ID: "b1", Metadata: map[string]string{
				"workflow_id":     "wf-1",
				"molecule_id":     "mol-1",
				"gc.root_bead_id": "root-1",
			}},
			want: "wf-1",
		},
		{
			name: "molecule_id next (poured/wisp)",
			bead: beads.Bead{ID: "b1", Metadata: map[string]string{
				"molecule_id":     "mol-1",
				"gc.root_bead_id": "root-1",
			}},
			want: "mol-1",
		},
		{
			name: "gc.root_bead_id next (nested)",
			bead: beads.Bead{ID: "b1", Metadata: map[string]string{"gc.root_bead_id": "root-1"}},
			want: "root-1",
		},
		{
			name: "bead id fallback (plain work bead)",
			bead: beads.Bead{ID: "b1"},
			want: "b1",
		},
		{
			name: "session id fallback (manual chat, no bead)",
			bead: beads.Bead{},
			want: session,
		},
		{
			name: "nil metadata is safe",
			bead: beads.Bead{ID: "b2", Metadata: nil},
			want: "b2",
		},
		{
			name: "blank metadata values skipped",
			bead: beads.Bead{ID: "b3", Metadata: map[string]string{"workflow_id": "  ", "molecule_id": "mol-3"}},
			want: "mol-3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRunID(tc.bead, session); got != tc.want {
				t.Fatalf("resolveRunID = %q, want %q", got, tc.want)
			}
		})
	}
}
