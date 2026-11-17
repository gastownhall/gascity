package beads_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdListCalls returns the args of every `bd list` call in calls.
func bdListCalls(calls [][]string) [][]string {
	var out [][]string
	for _, args := range calls {
		if len(args) > 0 && args[0] == "list" {
			out = append(out, args)
		}
	}
	return out
}

// metadataFieldArgs returns the values passed to --metadata-field in args.
func metadataFieldArgs(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--metadata-field" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// ListQuery.Matches reads an absent metadata key as "", so {k: ""} keeps rows
// that lack k. bd's --metadata-field k= compares the stored JSON value, which an
// absent key does not have, so pushing the pair drops rows Matches accepts. An
// empty-valued pair is therefore left to the client-side cut, and every other
// pair still travels.
func TestBdStoreListDoesNotPushEmptyMetadataValue(t *testing.T) {
	cases := []struct {
		name string
		md   map[string]string
		want []string
	}{
		{"empty only", map[string]string{"k": ""}, nil},
		{"empty beside pushable", map[string]string{"k": "", "gc.routed_to": "rig/pool"}, []string{"gc.routed_to=rig/pool"}},
		{"non-empty only", map[string]string{"gc.routed_to": "rig/pool"}, []string{"gc.routed_to=rig/pool"}},
	}
	for _, tier := range []beads.TierMode{beads.TierIssues, beads.TierBoth} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("tier%d/%s", tier, tc.name), func(t *testing.T) {
				var calls [][]string
				s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
				if _, err := s.List(beads.ListQuery{Metadata: tc.md, TierMode: tier}); err != nil {
					t.Fatal(err)
				}
				lists := bdListCalls(calls)
				if len(lists) != 1 {
					t.Fatalf("tier %v: %d bd list calls, want 1: %q", tier, len(lists), calls)
				}
				if got := metadataFieldArgs(lists[0]); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("tier %v: --metadata-field args = %q, want %q (bd list args %q)", tier, got, tc.want, lists[0])
				}
			})
		}
	}
}

// A metadata query's read stays unbounded even when every pair was left
// client-side: a bd-side limit would cut rows before the client filter runs.
func TestBdStoreListEmptyMetadataValueKeepsReadUnbounded(t *testing.T) {
	var calls [][]string
	s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
	if _, err := s.List(beads.ListQuery{Metadata: map[string]string{"k": ""}, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	lists := bdListCalls(calls)
	if len(lists) != 1 {
		t.Fatalf("%d bd list calls, want 1: %q", len(lists), calls)
	}
	if joined := " " + strings.Join(lists[0], " ") + " "; !strings.Contains(joined, " --limit 0 ") {
		t.Fatalf("bd list args %q, want --limit 0 (client-side limit)", lists[0])
	}
}
