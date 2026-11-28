package execenv

import (
	"slices"
	"strings"
	"testing"
)

// This diagnostic uses only synthetic fixtures. It prints masks, never values,
// token hashes, inherited host ENV or process arguments.
func TestOwnedTupleBoundaryDiagnostic(t *testing.T) {
	fixture := []string{
		"GC_SESSION_ID=fixture-sid", "GC_TEMPLATE=fixture-route",
		"GC_RUNTIME_EPOCH=1", "GC_INSTANCE_TOKEN=fixture-private-token", "GC_TEST_OWNER_BOUNDARY_CHILD=1",
	}
	for _, tc := range []struct {
		name  string
		extra map[string]string
		want  int
	}{
		{"inherited credential filtered must not leave partial owner", nil, 0},
		{"explicit complete owned tuple remains complete", map[string]string{
			"GC_SESSION_ID": "fixture-sid", "GC_TEMPLATE": "fixture-route",
			"GC_RUNTIME_EPOCH": "1", "GC_INSTANCE_TOKEN": "fixture-private-token",
		}, 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mask := 0
			keys := []string{"GC_SESSION_ID", "GC_TEMPLATE", "GC_RUNTIME_EPOCH", "GC_INSTANCE_TOKEN"}
			for _, entry := range MergeMap(fixture, tc.extra) {
				key, value, _ := strings.Cut(entry, "=")
				if index := slices.Index(keys, key); index >= 0 && value != "" {
					mask |= 1 << index
				}
			}
			if mask != tc.want {
				t.Fatalf("owner nonempty mask=%02x, expected=%02x", mask, tc.want)
			}
		})
	}
}

func TestInheritedOwnerTupleIsNeutralWithoutChangingRouting(t *testing.T) {
	context := []string{
		"PATH=/bin", "GC_CITY=/fixture/city", "GC_WORK_QUERY_SESSION_ID=route-sid",
		"BEADS_HOLDER_ID=holder", "BEADS_CREDENTIALS_FILE=/fixture/private-creds",
	}
	for _, tuple := range [][]string{
		{"GC_SESSION_ID=fixture-sid", "GC_TEMPLATE=fixture-route", "GC_RUNTIME_EPOCH=1", "GC_INSTANCE_TOKEN=fixture-private-token"},
		{"GC_SESSION_ID=fixture-sid", "GC_TEMPLATE=fixture-route", "GC_RUNTIME_EPOCH=1"},
		{"GC_SESSION_ID=fixture-sid", "GC_INSTANCE_TOKEN="},
		{"GC_SESSION_ID", "GC_RUNTIME_EPOCH=", "GC_SESSION_ID=duplicate"},
	} {
		input := append(slices.Clone(context), tuple...)
		original := slices.Clone(input)
		want := context[:len(context)-1] // Existing credential-file filtering remains strict.
		for name, got := range map[string][]string{
			"filter": FilterInherited(input), "map": MergeMap(input, nil), "entries": MergeEntries(input, nil),
		} {
			if !slices.Equal(got, want) {
				t.Errorf("%s changed neutral routing context or retained ambient owner", name)
			}
		}
		if !slices.Equal(input, original) {
			t.Fatal("filter mutated parent environment")
		}
	}
}

func TestExplicitOwnerOverridesNeverBorrowAmbientAuthority(t *testing.T) {
	ambient := []string{"GC_SESSION_ID=ambient", "GC_TEMPLATE=ambient", "GC_RUNTIME_EPOCH=2", "GC_INSTANCE_TOKEN=ambient-private"}
	for _, overrides := range [][]string{
		{"GC_SESSION_ID=explicit", "GC_TEMPLATE=explicit", "GC_RUNTIME_EPOCH=3"},
		{"GC_INSTANCE_TOKEN=explicit-private"},
		{"GC_SESSION_ID=explicit", "GC_TEMPLATE=explicit", "GC_RUNTIME_EPOCH=3", "GC_INSTANCE_TOKEN="},
		{"GC_SESSION_ID=explicit", "GC_TEMPLATE=explicit", "GC_RUNTIME_EPOCH=3", "GC_INSTANCE_TOKEN=explicit-private"},
	} {
		// Partial explicit tuples retain their existing invalid representation;
		// they are never completed using inherited authority or silently admitted.
		if got := MergeEntries(ambient, overrides); !slices.Equal(got, overrides) {
			t.Fatal("explicit entries borrowed ambient authority")
		}
		values := make(map[string]string, len(overrides))
		for _, entry := range overrides {
			key, value, _ := strings.Cut(entry, "=")
			values[key] = value
		}
		got := MergeMap(ambient, values)
		if len(got) != len(overrides) {
			t.Fatal("explicit map borrowed ambient authority")
		}
		for _, entry := range got {
			key, value, _ := strings.Cut(entry, "=")
			if value != values[key] {
				t.Fatal("explicit map changed supplied authority")
			}
		}
	}
}
