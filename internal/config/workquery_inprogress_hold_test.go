package config

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Regression coverage for gas-kg6, the hold-label sibling of the dep-blocked
// defect pinned in workquery_inprogress_blocked_test.go.
//
// The in_progress ("crash recovery") tier short-circuits with `exit 0` on the
// first assigned in_progress bead it finds. A bead parked on a canonical
// dispatch hold has no open blocking dependency, so the blocked_by gate let it
// through: the tier served the parked bead and the ready-gated tier below it
// never ran. A worker that correctly parked its bead was therefore re-served
// that same bead forever and could never reach its ready queue.
//
// Like the dep-blocked tests, these EXECUTE the generated shell against a fake
// `bd` on PATH so they pin observable behavior rather than the script's
// spelling.
//
// Scope note (ga-5736js): this tier is being made hold-aware for the
// WORK-SERVING decision only. The assignee-scoped probes that answer "does a
// session still need to exist" (ephemeralAssignedReadyProbeScript,
// filterReadyByAssignee) stay hold-transparent by design — pinned by
// TestEphemeralAssignedReadyProbeScriptDoesNotExcludeDispatchHoldLabels.

// heldInProgressListRow is the crash-recovery row for a bead parked on label.
func heldInProgressListRow(label string) string {
	return `[{"id":"wk-1","status":"in_progress","assignee":"sess-1","title":"work","labels":["` +
		label + `","mysql-cutover"]}]`
}

// fakeBdHeldInProgress returns a fake bd whose `bd list` reports one assigned
// in_progress bead parked on label with NO blocking dependencies (so the
// blocked_by gate cannot be what suppresses it), and whose `bd ready` yields a
// different, genuinely ready assigned bead.
func fakeBdHeldInProgress(label string) string {
	return `#!/bin/sh
case "$1" in
  list) printf '%s' '` + heldInProgressListRow(label) + `' ;;
  show) printf '%s' '[{"id":"wk-1","status":"in_progress","dependencies":[]}]' ;;
  ready) printf '%s' '[{"id":"wk-2","status":"open","assignee":"sess-1"}]' ;;
  *) printf '[]' ;;
esac
`
}

// TestInProgressTierFallsThroughWhenHeld is the primary gas-kg6 regression at
// the shell layer: a parked candidate must not swallow the tick — the
// ready-gated tier still runs and serves the worker's other ready work.
func TestInProgressTierFallsThroughWhenHeld(t *testing.T) {
	for _, label := range beadmeta.DispatchHoldLabels {
		t.Run(label, func(t *testing.T) {
			if _, err := exec.LookPath("jq"); err != nil {
				t.Skip("jq not available; the work-query shell requires it")
			}
			script := standardAssignedWorkQueryScript(QueryTopology{}) + `printf "[]"`
			out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, fakeBdHeldInProgress(label))

			var rows []map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
			}
			if len(rows) != 1 || rows[0]["id"] != "wk-2" {
				t.Fatalf("held in_progress candidate did not fall through to the ready tier; got %q", out)
			}
		})
	}
}

// TestInProgressTierSkipsHeldCandidate isolates the tier itself: with no ready
// work behind it, a parked candidate yields nothing rather than being re-served.
func TestInProgressTierSkipsHeldCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bdScript := `#!/bin/sh
case "$1" in
  list) printf '%s' '` + heldInProgressListRow(beadmeta.HoldExternalLabel) + `' ;;
  show) printf '%s' '[{"id":"wk-1","status":"in_progress","dependencies":[]}]' ;;
  *) printf '[]' ;;
esac
`
	script := standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 0 {
		t.Fatalf("held in_progress bead was re-served by the crash-recovery tier: %v", rows)
	}
}

// TestInProgressTierServesUnheldCandidate is the load-bearing negative
// control. Without it, a "fix" that silences the churn by serving nothing at
// all — defeating crash recovery entirely — would look green.
func TestInProgressTierServesUnheldCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	// Ordinary labels, including hold-ish ones that are NOT canonical dispatch
	// holds, must not suppress the re-serve.
	bdScript := `#!/bin/sh
case "$1" in
  list) printf '%s' '[{"id":"wk-1","status":"in_progress","assignee":"sess-1","labels":["mpr-human-hold","needs-mayor"]}]' ;;
  show) printf '%s' '[{"id":"wk-1","status":"in_progress","dependencies":[]}]' ;;
  *) printf '[]' ;;
esac
`
	script := standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 1 || rows[0]["id"] != "wk-1" {
		t.Fatalf("crash recovery broke: an unheld, unblocked in_progress bead must still be served; got %q", out)
	}
}

// TestLegacyControlInProgressTierSkipsHeldCandidate pins that the
// control-dispatcher variant of the same tier carries the identical guard —
// it shares inProgressBlockedByEnrichmentScript, so a fix applied to only one
// shape would silently leave the other jammed.
func TestLegacyControlInProgressTierSkipsHeldCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bdScript := `#!/bin/sh
case "$1" in
  list) printf '%s' '` + heldInProgressListRow(beadmeta.HoldMayorLabel) + `' ;;
  show) printf '%s' '[{"id":"wk-1","status":"in_progress","dependencies":[]}]' ;;
  *) printf '[]' ;;
esac
`
	script := legacyControlAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 0 {
		t.Fatalf("held in_progress bead was re-served by the legacy-control crash-recovery tier: %v", rows)
	}
}

// TestInProgressTierServesUnparseableHeldCandidateFailOpen pins the fail-open
// contract for the new guard: a malformed or log-prefixed `bd list` stdout must
// degrade to serving the candidate unchanged, never to dropping it. An
// over-eager hold check that treated "cannot parse labels" as "held" would
// disable crash recovery on any bd stdout hiccup.
func TestInProgressTierServesUnparseableHeldCandidateFailOpen(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	const blob = "warning: store not initialized\nargs=list --status in_progress"
	bdScript := `#!/bin/sh
case "$1" in
  list) printf '%s' '` + blob + `' ;;
  *) printf '[]' ;;
esac
`
	script := standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	if out != blob {
		t.Fatalf("unparseable bd list stdout was not served unchanged: got %q, want %q", out, blob)
	}
}
