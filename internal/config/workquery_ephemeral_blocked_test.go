package config

import (
	"os/exec"
	"strings"
	"testing"
)

// ephemeralQueryFixture mirrors, key-for-key, a real
// `bd query --json 'ephemeral=true AND status=<s>'` row as emitted by
// bd 1.1.0 (d076e6346). Captured 2026-07-31 against the gascity store.
//
// The load-bearing property is what is ABSENT: there is no `dependencies`
// array, no `blocked_by`, and no `is_blocked`. Readiness is summarized only
// as the `dependency_count` scalar, which says nothing about whether those
// dependencies are still open. Both ephemeral work-query probes filter on
// fields this payload does not carry, so their dependency gates evaluate to
// "not blocked" for every row.
//
// The fixture models one molecule step ("security") that is blocked by an
// open earlier step, assigned to the agent. A correct readiness gate must
// withhold it.
const ephemeralQueryFixture = `[
  {
    "id": "ti-1vamw",
    "title": "Security review against OWASP Top 10",
    "description": "step 3 of mol-code-review",
    "status": "%s",
    "priority": 2,
    "issue_type": "step",
    "assignee": "tincan-iris/reviewer",
    "created_at": "2026-07-31T04:54:07Z",
    "updated_at": "2026-07-31T04:54:07Z",
    "started_at": "2026-07-31T04:54:07Z",
    "metadata": {"gc.step_ref": "mol-code-review.security"},
    "ephemeral": true,
    "dependency_count": 1,
    "dependent_count": 1,
    "comment_count": 0
  }
]`

func runJQFilter(t *testing.T, filter, payload string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the ephemeral work-query probes filter with jq")
	}
	cmd := exec.Command("jq", append(append([]string{}, args...), filter)...)
	cmd.Stdin = strings.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq %q failed: %v", filter, err)
	}
	return strings.TrimSpace(string(out))
}

// TestEphemeralReadyProbeWithholdsBlockedStep pins that the ephemeral
// ready tier does not serve a step whose blocking dependency is still open.
//
// legacyEphemeralReadyFilterJQ counts open blockers by walking
// `(.dependencies // [])[]`. `bd query --json` never emits `dependencies`
// (see ephemeralQueryFixture), so the walk yields nothing, the count is 0,
// and every blocked ephemeral step passes the gate. The molecule's ordering
// is then decided solely by `sort_by(.created_at)` — and every step of a
// cooked molecule shares one second-granularity created_at, so the tie is
// broken by arbitrary store order.
//
// Repro for ga-qjozkw: a reviewer landed on the 'security' step while
// 'intake' and 'style' were still open and unclaimed.
func TestEphemeralReadyProbeWithholdsBlockedStep(t *testing.T) {
	filter := legacyEphemeralReadyFilterJQ(`select((.assignee // "") == $id)`, 1, false)
	payload := strings.ReplaceAll(ephemeralQueryFixture, "%s", "open")

	got := runJQFilter(t, filter, payload, "--arg", "id", "tincan-iris/reviewer")

	if got != "[]" {
		t.Errorf("ephemeral ready probe served a step with an open blocking dependency.\n"+
			"want: []\ngot:  %s\n\n"+
			"legacyEphemeralReadyFilterJQ reads (.dependencies // [])[], but "+
			"`bd query --json` emits only a dependency_count scalar — the "+
			"blocker walk is a structural no-op and the gate always passes.", got)
	}
}

// TestEphemeralInProgressProbeGatesOnReadiness pins that the ephemeral
// crash-recovery tier applies a readiness gate at all.
//
// The persistent twin in standardAssignedInProgressWorkQueryScript wraps its
// `bd list --status in_progress` read in inProgressBlockedByEnrichmentScript,
// which re-reads the candidate with `bd show --json` (whose dependency rows
// ARE hydrated with dependency_type + status) and attaches blocked_by so the
// shared Go filter can skip it. ephemeralAssignedInProgressProbeScript takes
// the same class of read and applies no gate whatsoever, so a blocked,
// in_progress, assigned wisp step is re-served on every hook tick.
func TestEphemeralInProgressProbeGatesOnReadiness(t *testing.T) {
	script := ephemeralAssignedInProgressProbeScript("id", QueryTopology{})

	// The probe's own jq must not be the only filter: it selects on assignee
	// alone. Assert the emitted script performs readiness gating, by either
	// enriching blocked_by (the persistent tier's proven route) or filtering
	// dependencies inline.
	gated := strings.Contains(script, "blocked_by") ||
		strings.Contains(script, "dependency_type") ||
		strings.Contains(script, "dependencies")
	if !gated {
		t.Errorf("ephemeral in_progress probe applies no readiness gate:\n%s\n\n"+
			"A blocked+in_progress+assigned wisp step is served unchanged. The "+
			"hook-side defensive filter cannot compensate: `bd query --json` "+
			"emits neither blocked_by nor is_blocked, so both "+
			"isDepBlockedHookCandidate and isSelfBlockedHookCandidate correctly "+
			"read the row as unblocked.", script)
	}
}

// TestEphemeralInProgressProbeIgnoresBD105Semantics documents that the
// ephemeral in_progress probe discards includeEphemeralReady, so upgrading the
// city to bd>=1.0.5 ready semantics does not close the hole. The ephemeral
// READY probe disables itself under those semantics (bd ready
// --include-ephemeral covers it); the in_progress probe keeps running ungated.
func TestEphemeralInProgressProbeIgnoresBD105Semantics(t *testing.T) {
	legacy := ephemeralAssignedInProgressProbeScript("id", QueryTopology{})
	modern := ephemeralAssignedInProgressProbeScript("id", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}})

	if legacy != modern {
		t.Skip("in_progress probe now varies with bd ready semantics; revisit this pin")
	}
	if strings.TrimSpace(modern) == "" {
		return // probe disabled under modern semantics — hole closed
	}
	if !strings.Contains(modern, "blocked_by") && !strings.Contains(modern, "dependencies") {
		t.Errorf("under bd>=1.0.5 ready semantics the ephemeral in_progress probe "+
			"still runs with no readiness gate:\n%s", modern)
	}
}
