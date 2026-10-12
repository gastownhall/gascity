package sling

import "testing"

// TestShouldPromoteWorkflowLaunchStatus pins the exact status set the launch
// promote predicate treats as "not yet claimed".
//
// This set is load-bearing beyond the launch path: the periodic wisp GC's
// steplessRootIsAbandoned (cmd/gc/wisp_gc.go) classifies claim state by asking
// this same question, and a stepless root it classifies as unclaimed past the
// idle TTL gets CLOSED. Adding a status to the true arm for a legitimate launch
// reason therefore also widens what the reaper destroys — change this table and
// the GC's stepless coverage together, deliberately.
func TestShouldPromoteWorkflowLaunchStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{"empty means freshly created", "", true},
		{"open", "open", true},
		{"ready", "ready", true},
		{"todo", "todo", true},
		{"triage", "triage", true},
		{"backlog", "backlog", true},
		{"padded and uppercased open still matches", " OPEN ", true},
		{"in_progress is already claimed", "in_progress", false},
		{"closed", "closed", false},
		{"blocked", "blocked", false},
		{"paused", "paused", false},
		{"done", "done", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldPromoteWorkflowLaunchStatus(tt.status); got != tt.want {
				t.Fatalf("ShouldPromoteWorkflowLaunchStatus(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}
