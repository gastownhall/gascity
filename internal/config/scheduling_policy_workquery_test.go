package config

import (
	"strings"
	"testing"
)

const (
	priorityFIFOJQSort = `sort_by((.priority // 2), (.created_at // ""), (.id // ""))`
	fifoJQSort         = `sort_by((.created_at // ""), (.id // ""))`
)

// Every generated query an agent can emit. If a new admission query appears it
// belongs here, so it cannot quietly keep a hardcoded order.
func generatedQueriesFor(a *Agent) map[string]string {
	return map[string]string{
		"work":                a.EffectiveWorkQuery(),
		"pool_demand":         a.EffectivePoolDemandQuery(),
		"routed_pool":         a.EffectiveRoutedPoolQuery(),
		"sling":               a.EffectiveSlingQuery(),
		"assigned_ready":      a.EffectiveAssignedReadyQuery(),
		"assigned_inprogress": a.EffectiveAssignedInProgressQuery(),
	}
}

func TestGeneratedQueriesUsePriorityFIFOByDefault(t *testing.T) {
	agent := &Agent{Name: "polecat"}
	for name, q := range generatedQueriesFor(agent) {
		if strings.Contains(q, "--sort ") && !strings.Contains(q, "--sort hybrid") {
			t.Errorf("%s: default query uses a non-hybrid sort: %s", name, q)
		}
		if strings.Contains(q, "--sort oldest") {
			t.Errorf("%s: default query must not use fifo's --sort oldest", name)
		}
		if strings.Contains(q, "sort_by(") && !strings.Contains(q, priorityFIFOJQSort) {
			t.Errorf("%s: default jq sort is not the priority_fifo key: %s", name, q)
		}
	}
}

func TestGeneratedQueriesHonorFIFOPolicy(t *testing.T) {
	agent := &Agent{Name: "polecat", SchedulingPolicy: "fifo"}
	for name, q := range generatedQueriesFor(agent) {
		if strings.Contains(q, "--sort hybrid") {
			t.Errorf("%s: fifo query still asks bd for hybrid order: %s", name, q)
		}
		if strings.Contains(q, priorityFIFOJQSort) {
			t.Errorf("%s: fifo query still sorts jq on priority: %s", name, q)
		}
		if strings.Contains(q, "sort_by(") && !strings.Contains(q, fifoJQSort) {
			t.Errorf("%s: fifo jq sort is not the fifo key: %s", name, q)
		}
	}
}

// An explicit priority_fifo must generate byte-identical output to unset, or the
// documented "unset means priority_fifo" contract is a lie.
func TestExplicitPriorityFIFOMatchesUnsetByte(t *testing.T) {
	unset := generatedQueriesFor(&Agent{Name: "polecat"})
	explicit := generatedQueriesFor(&Agent{Name: "polecat", SchedulingPolicy: "priority_fifo"})
	for name, want := range unset {
		if got := explicit[name]; got != want {
			t.Errorf("%s: explicit priority_fifo diverges from unset\n got: %s\nwant: %s", name, got, want)
		}
	}
}

// fifo and priority_fifo must actually differ somewhere, otherwise the setting
// is decorative and these tests would pass vacuously.
func TestFIFOPolicyActuallyChangesTheQuery(t *testing.T) {
	def := generatedQueriesFor(&Agent{Name: "polecat"})
	fifo := generatedQueriesFor(&Agent{Name: "polecat", SchedulingPolicy: "fifo"})
	differing := 0
	for name, d := range def {
		if fifo[name] != d {
			differing++
		}
	}
	if differing == 0 {
		t.Fatal("fifo produced identical queries to priority_fifo: the policy is not wired into query generation")
	}
}
