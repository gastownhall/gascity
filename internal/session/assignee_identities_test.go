package session

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The assignee-identity codec is the confined vocabulary shared by the
// reconciler orphan-release loops and the API assignee list filter/stamper.
// These tests pin AssigneeIdentities to the cmd/gc sessionBeadAssigneeIdentities
// case table it replaces (via InfoFromPersistedBead, proving bead<->Info
// agreement) and pin AssigneeIdentifier to the internal/api
// sessionBeadAssigneeIdentifier precedence it replaces, so the enumerated term
// set and the stamped identity form stay byte-identical after the dedup. A
// direct-Info case proves the RAW SessionNameMetadata field is read (no
// sessionNameFor(ID) fallback leak).

func TestAssigneeIdentities(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want []string
	}{
		{
			name: "empty bead produces no identities",
			bead: beads.Bead{},
			want: []string{},
		},
		{
			name: "id only",
			bead: beads.Bead{ID: "mc-xyz"},
			want: []string{"mc-xyz"},
		},
		{
			name: "session_name only",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "worker-mc-live"}},
			want: []string{"worker-mc-live"},
		},
		{
			name: "configured_named_identity only",
			bead: beads.Bead{Metadata: map[string]string{"configured_named_identity": "reviewer"}},
			want: []string{"reviewer"},
		},
		{
			name: "alias only",
			bead: beads.Bead{Metadata: map[string]string{"alias": "nux"}},
			want: []string{"nux"},
		},
		{
			name: "alias_history single entry",
			bead: beads.Bead{Metadata: map[string]string{"alias_history": "previous"}},
			want: []string{"previous"},
		},
		{
			name: "alias_history multiple entries",
			bead: beads.Bead{Metadata: map[string]string{"alias_history": "first,second,third"}},
			want: []string{"first", "second", "third"},
		},
		{
			name: "all fields populated",
			bead: beads.Bead{
				ID: "mc-xyz",
				Metadata: map[string]string{
					"session_name":              "worker-mc-live",
					"configured_named_identity": "reviewer",
					"alias":                     "rictus",
					"alias_history":             "nux",
				},
			},
			want: []string{"mc-xyz", "worker-mc-live", "reviewer", "rictus", "nux"},
		},
		{
			name: "whitespace-only values are trimmed and skipped",
			bead: beads.Bead{
				ID: "  ",
				Metadata: map[string]string{
					"session_name":              "   ",
					"configured_named_identity": "\t",
					"alias":                     " ",
					"alias_history":             "  ,  , real ,  ",
				},
			},
			want: []string{"real"},
		},
		{
			name: "values with surrounding whitespace are trimmed",
			bead: beads.Bead{
				ID: "  mc-xyz  ",
				Metadata: map[string]string{
					"session_name":              "  worker-mc-live  ",
					"configured_named_identity": "  reviewer  ",
					"alias":                     "  nux  ",
				},
			},
			want: []string{"mc-xyz", "worker-mc-live", "reviewer", "nux"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssigneeIdentities(infoFromPersistedBead(tt.bead))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d identities %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i, id := range got {
				if id != tt.want[i] {
					t.Errorf("identity[%d] = %q, want %q (full got=%v, want=%v)", i, id, tt.want[i], got, tt.want)
				}
			}
		})
	}
}

// TestAssigneeIdentitiesReadsRawSessionName proves AssigneeIdentities reads the
// RAW SessionNameMetadata field, not Info.SessionName (which falls back to
// sessionNameFor(ID)). A blank SessionNameMetadata must not leak the derived
// runtime name into the identity set.
func TestAssigneeIdentitiesReadsRawSessionName(t *testing.T) {
	i := Info{ID: "s1", SessionName: "s-gc-derived", SessionNameMetadata: ""}
	if got, want := AssigneeIdentities(i), []string{"s1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AssigneeIdentities = %#v, want %#v (must not leak sessionNameFor(ID))", got, want)
	}
}

func TestAssigneeIdentifier(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "session_name wins",
			info: Info{ID: "s1", SessionNameMetadata: "sn", Alias: "al", ConfiguredNamedIdentity: "ni"},
			want: "sn",
		},
		{
			name: "alias when no session_name",
			info: Info{ID: "s1", Alias: "al", ConfiguredNamedIdentity: "ni"},
			want: "al",
		},
		{
			name: "configured named identity when no session_name or alias",
			info: Info{ID: "s1", ConfiguredNamedIdentity: "ni"},
			want: "ni",
		},
		{
			name: "bead id fallback when no name metadata",
			info: Info{ID: "s1"},
			want: "s1",
		},
		{
			name: "whitespace-only values skipped, falls through to id",
			info: Info{ID: "s1", SessionNameMetadata: "  ", Alias: "\t", ConfiguredNamedIdentity: " "},
			want: "s1",
		},
		{
			name: "values trimmed",
			info: Info{ID: "s1", SessionNameMetadata: "  sn  "},
			want: "sn",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AssigneeIdentifier(tt.info); got != tt.want {
				t.Errorf("AssigneeIdentifier = %q, want %q", got, tt.want)
			}
		})
	}
}
