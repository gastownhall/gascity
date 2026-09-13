package main

import "testing"

// TestFormatDeadAssigneeReopenedMessage pins the operator-facing wording for
// bead.dead_assignee_reopened, including the bare-template-as-assignee case
// (ga-r22k2y): some routing paths write the template name itself into
// Assignee rather than a concrete session identity, and reusing the generic
// "assigned to dead session <template>" wording there is misleading — no
// session named after the template ever existed to die. deadAssignee ==
// routedTo is the signal that distinguishes the two shapes.
func TestFormatDeadAssigneeReopenedMessage(t *testing.T) {
	tests := []struct {
		name         string
		beadID       string
		deadAssignee string
		routedTo     string
		want         string
	}{
		{
			name:         "dead named session distinct from route",
			beadID:       "gc-1",
			deadAssignee: "sess-dead-123",
			routedTo:     "worker",
			want: "reopened routed work gc-1 assigned to dead session sess-dead-123 " +
				"(route worker); assignee cleared so the pool can reclaim it",
		},
		{
			name:         "bare template name as assignee",
			beadID:       "gc-2",
			deadAssignee: "worker",
			routedTo:     "worker",
			want: "reopened routed work gc-2 routed to worker with no live session " +
				"claiming it; assignee cleared so the pool can reclaim it",
		},
		{
			name:         "empty assignee never reads as the bare-template case",
			beadID:       "gc-3",
			deadAssignee: "",
			routedTo:     "worker",
			want: "reopened routed work gc-3 assigned to dead session <unknown> " +
				"(route worker); assignee cleared so the pool can reclaim it",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatDeadAssigneeReopenedMessage(tc.beadID, tc.deadAssignee, tc.routedTo)
			if got != tc.want {
				t.Fatalf("formatDeadAssigneeReopenedMessage(%q, %q, %q) = %q, want %q",
					tc.beadID, tc.deadAssignee, tc.routedTo, got, tc.want)
			}
		})
	}
}
