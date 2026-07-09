package session

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCanonicalIdentityFromMetadata(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want CanonicalIdentity
	}{
		{
			name: "nil metadata is absent",
			meta: nil,
			want: CanonicalIdentity{},
		},
		{
			name: "empty metadata is absent",
			meta: map[string]string{},
			want: CanonicalIdentity{},
		},
		{
			name: "name with positive slot",
			meta: map[string]string{
				CanonicalInstanceNameMetadata: "dir/agent-1",
				CanonicalPoolSlotMetadata:     "3",
			},
			want: CanonicalIdentity{QualifiedInstanceName: "dir/agent-1", PoolSlot: 3, Present: true},
		},
		{
			name: "name without slot is unslotted singleton",
			meta: map[string]string{CanonicalInstanceNameMetadata: "solo"},
			want: CanonicalIdentity{QualifiedInstanceName: "solo", PoolSlot: 0, Present: true},
		},
		{
			name: "name and slot trimmed",
			meta: map[string]string{CanonicalInstanceNameMetadata: "  dir/a  ", CanonicalPoolSlotMetadata: " 2 "},
			want: CanonicalIdentity{QualifiedInstanceName: "dir/a", PoolSlot: 2, Present: true},
		},
		{
			name: "whitespace-only name is absent",
			meta: map[string]string{CanonicalInstanceNameMetadata: "   ", CanonicalPoolSlotMetadata: "2"},
			want: CanonicalIdentity{},
		},
		{
			name: "slot without name is absent",
			meta: map[string]string{CanonicalPoolSlotMetadata: "4"},
			want: CanonicalIdentity{},
		},
		{
			name: "non-numeric slot is unslotted",
			meta: map[string]string{CanonicalInstanceNameMetadata: "a", CanonicalPoolSlotMetadata: "xyz"},
			want: CanonicalIdentity{QualifiedInstanceName: "a", PoolSlot: 0, Present: true},
		},
		{
			name: "zero slot is unslotted",
			meta: map[string]string{CanonicalInstanceNameMetadata: "a", CanonicalPoolSlotMetadata: "0"},
			want: CanonicalIdentity{QualifiedInstanceName: "a", PoolSlot: 0, Present: true},
		},
		{
			name: "negative slot is unslotted",
			meta: map[string]string{CanonicalInstanceNameMetadata: "a", CanonicalPoolSlotMetadata: "-1"},
			want: CanonicalIdentity{QualifiedInstanceName: "a", PoolSlot: 0, Present: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalIdentityFromMetadata(tc.meta); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CanonicalIdentityFromMetadata(%v) = %+v, want %+v", tc.meta, got, tc.want)
			}
		})
	}
}

// TestInfoCanonicalIdentityAccessor proves InfoFromPersistedBead mirrors the two
// canonical keys verbatim and that the Info.CanonicalIdentity() accessor equals
// CanonicalIdentityFromMetadata for every bead — the shared-helper drift guard
// that keeps the two projections identical (S2-6).
func TestInfoCanonicalIdentityAccessor(t *testing.T) {
	metas := []map[string]string{
		nil,
		{},
		{CanonicalInstanceNameMetadata: "dir/agent-2", CanonicalPoolSlotMetadata: "5"},
		{CanonicalInstanceNameMetadata: "solo"},
		{CanonicalInstanceNameMetadata: "  dir/a  ", CanonicalPoolSlotMetadata: " 2 "},
		{CanonicalPoolSlotMetadata: "4"}, // stray slot, no name
		{CanonicalInstanceNameMetadata: "a", CanonicalPoolSlotMetadata: "garbage"},
	}
	for _, meta := range metas {
		b := beads.Bead{ID: "s", Type: "gc:session", Status: "open", Labels: []string{"gc:session"}, Metadata: meta}
		info := InfoFromPersistedBead(b)
		if got, want := info.CanonicalInstanceNameMetadata, meta[CanonicalInstanceNameMetadata]; got != want {
			t.Errorf("meta=%v: mirror CanonicalInstanceNameMetadata = %q, want %q", meta, got, want)
		}
		if got, want := info.CanonicalPoolSlotMetadata, meta[CanonicalPoolSlotMetadata]; got != want {
			t.Errorf("meta=%v: mirror CanonicalPoolSlotMetadata = %q, want %q", meta, got, want)
		}
		if got, want := info.CanonicalIdentity(), CanonicalIdentityFromMetadata(meta); !reflect.DeepEqual(got, want) {
			t.Errorf("meta=%v: accessor = %+v, want %+v (drift between accessor and CanonicalIdentityFromMetadata)", meta, got, want)
		}
	}
}
