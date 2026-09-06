package config

import (
	"strings"
	"testing"
)

// TestLegacyEphemeralHookQueriesPushSelectivePredicatesIntoBd prevents the
// hook's compatibility probes from hydrating every matching wisp and filtering
// only after bd has returned the full result. Each selector that bd's query
// language can express must be part of the database-side expression.
func TestLegacyEphemeralHookQueriesPushSelectivePredicatesIntoBd(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		expression string
	}{
		{
			name:       "assigned in progress",
			script:     ephemeralAssignedInProgressProbeScript("id", QueryTopology{}),
			expression: `"ephemeral=true AND status=in_progress AND assignee=\"$id\""`,
		},
		{
			name:       "assigned ready",
			script:     ephemeralAssignedReadyProbeScript("id", QueryTopology{}),
			expression: `"ephemeral=true AND status=open AND assignee=\"$id\""`,
		},
		{
			name:       "unassigned pool demand",
			script:     legacyEphemeralPoolDemandShell(20, QueryTopology{}, true),
			expression: `'ephemeral=true AND status=open AND assignee=none'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const prefix = "bd query --json "
			start := strings.Index(tt.script, prefix)
			if start < 0 {
				t.Fatalf("generated probe has no bd query: %q", tt.script)
			}
			tail := tt.script[start+len(prefix):]
			end := strings.Index(tail, " --limit=0")
			if end < 0 {
				t.Fatalf("generated probe has no explicit query limit: %q", tt.script)
			}
			if got := tail[:end]; got != tt.expression {
				t.Fatalf("bd query expression = %q, want %q", got, tt.expression)
			}
		})
	}
}
