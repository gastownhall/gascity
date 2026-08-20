package config

import (
	"strings"
	"testing"
)

func TestDefaultWorkQueriesUsePriorityBandFIFO(t *testing.T) {
	for name, query := range map[string]string{
		"routed":    routedReadyTierCommand(QueryTopology{}),
		"migration": bdReadyPoolDemandMigrationShell("--limit=20", QueryTopology{}),
	} {
		if !strings.Contains(query, "--sort priority") {
			t.Errorf("%s query = %q, want --sort priority", name, query)
		}
		if strings.Contains(query, "--sort oldest") {
			t.Errorf("%s query = %q, must not use oldest-only ordering", name, query)
		}
	}

	jq := legacyEphemeralReadyFilterJQ("true", 20, false)
	want := `sort_by((.priority // 2), (if ((.created_at // "") == "") then 1 else 0 end), (.created_at // ""), (.id // ""))`
	if !strings.Contains(jq, want) {
		t.Fatalf("legacy jq = %q, want %q", jq, want)
	}
}

func TestPoolDemandCountRemainsOrderFree(t *testing.T) {
	query := poolDemandCountShell("worker-pool", QueryTopology{})
	canonicalLeg := strings.Split(query, "legacy_candidates=")[0]
	if strings.Contains(canonicalLeg, "--sort") {
		t.Fatalf("count query canonical leg = %q, want no sort for an order-insensitive count", canonicalLeg)
	}
}
