package beadmeta

import "testing"

func TestResolveRunID(t *testing.T) {
	cases := []struct {
		name       string
		metadata   map[string]string
		selfID     string
		fallbackID string
		want       string
	}{
		{
			name:       "workflow_id wins (graph workflow)",
			metadata:   map[string]string{"workflow_id": "wf-1", "molecule_id": "mol-1", RootBeadIDMetadataKey: "root-1"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "wf-1",
		},
		{
			name:       "molecule_id next (poured/wisp)",
			metadata:   map[string]string{"molecule_id": "mol-1", RootBeadIDMetadataKey: "root-1"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "mol-1",
		},
		{
			name:       "gc.root_bead_id next (nested)",
			metadata:   map[string]string{RootBeadIDMetadataKey: "root-1"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "root-1",
		},
		{
			name:       "self id fallback (plain work bead, worker path)",
			metadata:   nil,
			selfID:     "b1",
			fallbackID: "s1",
			want:       "b1",
		},
		{
			name:       "final fallback (manual chat: no bead, session id)",
			metadata:   nil,
			selfID:     "",
			fallbackID: "s1",
			want:       "s1",
		},
		{
			name:       "compute path: empty final fallback yields self id",
			metadata:   nil,
			selfID:     "session-bead-9",
			fallbackID: "",
			want:       "session-bead-9",
		},
		{
			name:       "blank chain values are skipped",
			metadata:   map[string]string{"workflow_id": "  ", "molecule_id": "mol-3"},
			selfID:     "b1",
			fallbackID: "s1",
			want:       "mol-3",
		},
		{
			name:       "blank self id falls through to fallback",
			metadata:   nil,
			selfID:     "   ",
			fallbackID: "s1",
			want:       "s1",
		},
		{
			name:       "all empty yields empty",
			metadata:   map[string]string{},
			selfID:     "",
			fallbackID: "",
			want:       "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveRunID(tc.metadata, tc.selfID, tc.fallbackID); got != tc.want {
				t.Fatalf("ResolveRunID(%v, %q, %q) = %q, want %q", tc.metadata, tc.selfID, tc.fallbackID, got, tc.want)
			}
		})
	}
}

func TestResolveNestingRootID(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]string
		selfID   string
		want     string
	}{
		{
			name:     "workflow_id wins (graph workflow)",
			metadata: map[string]string{"workflow_id": "wf-1", RootBeadIDMetadataKey: "root-1", "molecule_id": "mol-1"},
			selfID:   "b1",
			want:     "wf-1",
		},
		{
			name:     "gc.root_bead_id next (nested in a run)",
			metadata: map[string]string{RootBeadIDMetadataKey: "root-1", "molecule_id": "mol-1"},
			selfID:   "b1",
			want:     "root-1",
		},
		{
			name:     "molecule_id alone is a forward pointer, never a nesting root",
			metadata: map[string]string{"molecule_id": "mol-1"},
			selfID:   "b1",
			want:     "b1",
		},
		{
			name:     "no chain self-roots",
			metadata: nil,
			selfID:   "b1",
			want:     "b1",
		},
		{
			name:     "blank chain values are skipped",
			metadata: map[string]string{"workflow_id": "  ", RootBeadIDMetadataKey: " root-2 ", "molecule_id": "mol-1"},
			selfID:   "b1",
			want:     "root-2",
		},
		{
			name:     "blank self id yields empty",
			metadata: map[string]string{"molecule_id": "mol-1"},
			selfID:   "  ",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveNestingRootID(tc.metadata, tc.selfID); got != tc.want {
				t.Fatalf("ResolveNestingRootID(%v, %q) = %q, want %q", tc.metadata, tc.selfID, got, tc.want)
			}
		})
	}
}

// TestResolveRunIDKeepsMoleculeIDAheadOfRootBeadID pins that cost and event
// grouping still follows molecule_id ("my attachment") even though nesting no
// longer does: the two resolvers differ on purpose.
func TestResolveRunIDKeepsMoleculeIDAheadOfRootBeadID(t *testing.T) {
	md := map[string]string{"molecule_id": "mol-1", RootBeadIDMetadataKey: "root-1"}
	if got := ResolveRunID(md, "b1", ""); got != "mol-1" {
		t.Fatalf("ResolveRunID(%v) = %q, want %q", md, got, "mol-1")
	}
	if got := ResolveNestingRootID(md, "b1"); got != "root-1" {
		t.Fatalf("ResolveNestingRootID(%v) = %q, want %q", md, got, "root-1")
	}
}
