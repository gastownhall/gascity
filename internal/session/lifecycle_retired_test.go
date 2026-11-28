package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// excludedNamedCloseReasons are the close_reason/state values that retire a
// closed named row for good (ClosedNamedSessionReopenEligibleMetadata).
var excludedNamedCloseReasons = []string{"duplicate", "duplicate-repair", "gc_swept", "orphaned", "reconfigured", "stale-session", string(StateFailedCreate)}

func TestLifecycleRetired(t *testing.T) {
	named := func(extra map[string]string) map[string]string {
		md := map[string]string{NamedSessionIdentityMetadata: "lead"}
		for k, v := range extra {
			md[k] = v
		}
		return md
	}
	cases := []struct {
		name   string
		status string
		md     map[string]string
		want   bool
	}{
		{name: "asleep", status: "open", md: map[string]string{"state": string(StateAsleep)}, want: false},
		{name: "drained by sleep reason", status: "open", md: map[string]string{"state": string(StateAsleep), "sleep_reason": "drained"}, want: false},
		{name: "drained", status: "open", md: map[string]string{"state": "drained"}, want: false},
		{name: "draining", status: "open", md: map[string]string{"state": string(StateDraining)}, want: false},
		{name: "quarantined", status: "open", md: map[string]string{"state": string(StateQuarantined)}, want: false},
		{name: "archived", status: "open", md: map[string]string{"state": string(StateArchived)}, want: false},
		{name: "archived without continuity", status: "open", md: map[string]string{"state": string(StateArchived), "continuity_eligible": "false"}, want: false},
		{name: "closing", status: "open", md: map[string]string{"state": "closing"}, want: false},
		{name: "active", status: "open", md: map[string]string{"state": string(StateActive)}, want: false},
		{name: "closed pool row", status: "closed", md: map[string]string{"state": string(StateAsleep), "pool_slot": "2"}, want: true},
		{name: "closed ad-hoc row", status: "closed", md: map[string]string{}, want: true},
		{name: "open row stored closed", status: "open", md: map[string]string{"state": "closed"}, want: true},
		{name: "closed named row with no close reason", status: "closed", md: named(nil), want: false},
		{name: "closed named row without continuity", status: "closed", md: named(map[string]string{"continuity_eligible": "false"}), want: true},
		{name: "open named row asleep", status: "open", md: named(map[string]string{"state": string(StateAsleep)}), want: false},
	}
	for _, reason := range excludedNamedCloseReasons {
		cases = append(cases,
			struct {
				name   string
				status string
				md     map[string]string
				want   bool
			}{name: "closed named row close_reason " + reason, status: "closed", md: named(map[string]string{"close_reason": reason}), want: true},
			struct {
				name   string
				status string
				md     map[string]string
				want   bool
			}{name: "closed named row state " + reason, status: "closed", md: named(map[string]string{"state": reason}), want: true},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LifecycleRetired(tc.status, tc.md); got != tc.want {
				t.Fatalf("LifecycleRetired(%q, %v) = %v, want %v", tc.status, tc.md, got, tc.want)
			}
		})
	}
}

func TestClosedNamedSessionReopenEligibleMetadataMatchesBead(t *testing.T) {
	mds := []map[string]string{
		nil,
		{},
		{"continuity_eligible": "false"},
		{"continuity_eligible": "true"},
		{"close_reason": "explicit"},
	}
	for _, reason := range excludedNamedCloseReasons {
		mds = append(mds, map[string]string{"close_reason": reason}, map[string]string{"state": " " + reason + " "})
	}
	for _, md := range mds {
		b := beads.Bead{Status: "closed", Metadata: md}
		got, want := ClosedNamedSessionReopenEligibleMetadata(md), closedNamedSessionReopenEligible(b)
		if got != want {
			t.Fatalf("ClosedNamedSessionReopenEligibleMetadata(%v) = %v, closedNamedSessionReopenEligible = %v", md, got, want)
		}
	}
	if !ClosedNamedSessionReopenEligibleMetadata(map[string]string{}) {
		t.Fatal("a closed named row with no close reason must be reopen-eligible")
	}
	if ClosedNamedSessionReopenEligibleMetadata(map[string]string{"close_reason": "orphaned"}) {
		t.Fatal("an orphaned close must not be reopen-eligible")
	}
}
