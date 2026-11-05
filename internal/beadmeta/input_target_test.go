package beadmeta

import (
	"reflect"
	"testing"
)

// InputTargetIDs must return the distinct recorded target ids of a synthetic
// input convoy, from InputTargetIDMetadataKey and DrainMemberIDMetadataKey, in
// a deterministic order, without consulting tracking edges (#8729).
func TestInputTargetIDsReturnsDistinctRecordedTargets(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		want     []string
	}{
		{
			name: "graph.v2 single-item input convoy",
			metadata: map[string]string{
				SyntheticMetadataKey:     "true",
				InputTargetIDMetadataKey: "work-1",
			},
			want: []string{"work-1"},
		},
		{
			name: "drain unit convoy",
			metadata: map[string]string{
				SyntheticMetadataKey:         "true",
				SyntheticKindMetadataKey:     "drain-unit-convoy",
				DrainMemberIDMetadataKey:     "member-9",
				DrainMemberAccessMetadataKey: "direct",
			},
			want: []string{"member-9"},
		},
		{
			name: "both stamps deduped",
			metadata: map[string]string{
				InputTargetIDMetadataKey: "work-1",
				DrainMemberIDMetadataKey: "work-1",
			},
			want: []string{"work-1"},
		},
		{
			name: "distinct stamps both kept in stamp order",
			metadata: map[string]string{
				InputTargetIDMetadataKey: "work-1",
				DrainMemberIDMetadataKey: "member-9",
			},
			want: []string{"work-1", "member-9"},
		},
		{
			name:     "no stamps",
			metadata: map[string]string{SyntheticMetadataKey: "true"},
			want:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InputTargetIDs(tc.metadata); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("InputTargetIDs(%v) = %q, want %q", tc.metadata, got, tc.want)
			}
		})
	}
}

// Whitespace around a recorded id is a typo away from an unresolvable lookup,
// so InputTargetIDs trims stamps and drops the empty results.
func TestInputTargetIDsTrimsAndDropsEmpty(t *testing.T) {
	if got := InputTargetIDs(map[string]string{
		InputTargetIDMetadataKey: "  spaced  ",
		DrainMemberIDMetadataKey: "   ",
	}); !reflect.DeepEqual(got, []string{"spaced"}) {
		t.Fatalf("InputTargetIDs = %q, want [spaced]", got)
	}
}

// A nil metadata map is a convoy whose stamps were never written, not a
// missing convoy: the accessor answers "no recorded targets", not panic.
func TestInputTargetIDsNilMap(t *testing.T) {
	if got := InputTargetIDs(nil); got != nil {
		t.Fatalf("InputTargetIDs(nil) = %q, want nil", got)
	}
}
