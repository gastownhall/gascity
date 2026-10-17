package config

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Regression coverage for gascity#8010, the OPEN-tier twin of the hold-label
// defect fixed for the in_progress tier (gas-kg6, see
// workquery_inprogress_hold_test.go).
//
// The assigned-ready ("pre-assigned") tier reads one row via
// `bd ready --assignee="$id" --json --limit=1` and short-circuited with
// `exit 0` on any nonempty result, with no check for a canonical dispatch
// hold. A bead parked on a hold has no blocking dependency, so `bd ready`
// returned it, the tier served it, and the script exited before the
// routed-pool tier below it (Tier 3, probe_pool_demand) ever ran — starving
// real, unassigned, routed demand behind an unrelated held row. The hook's
// own hold filter correctly rejected the served candidate afterward, but by
// then the query had already answered and exited; the hook reported
// drain/no_work despite claimable routed work existing.
//
// Scope note (ga-5736js, unchanged): this fix is the WORK-SERVING decision
// only. ephemeralAssignedReadyProbeScript (the bd-1.0.4 wisp tier) and
// dispatch_control_ready.go's filterReadyByAssignee stay hold-transparent by
// design, pinned by TestEphemeralAssignedReadyProbeScriptDoesNotExcludeDispatchHoldLabels
// in workquery_hold_label_test.go — not touched here.

// heldAssignedReadyRow is the assigned-ready row for a bead parked on label.
func heldAssignedReadyRow(label string) string {
	return `[{"id":"held-task","status":"open","assignee":"sess-1","title":"work","labels":["` +
		label + `","mysql-cutover"]}]`
}

// TestAssignedReadyTierSkipsHeldCandidate isolates the tier itself: with no
// routed work behind it, a held candidate yields nothing rather than being
// served.
func TestAssignedReadyTierSkipsHeldCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	for _, label := range beadmeta.DispatchHoldLabels {
		t.Run(label, func(t *testing.T) {
			bdScript := `#!/bin/sh
case "$1" in
  ready) printf '%s' '` + heldAssignedReadyRow(label) + `' ;;
  *) printf '[]' ;;
esac
`
			script := standardAssignedReadyWorkQueryScript(QueryTopology{}) + `printf "[]"`
			out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

			var rows []map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
			}
			if len(rows) != 0 {
				t.Fatalf("held assigned-ready bead was served by the tier: %v", rows)
			}
		})
	}
}

// TestAssignedReadyTierServesUnheldCandidate is the load-bearing negative
// control. Without it, a "fix" that silences the churn by serving nothing at
// all — defeating the pre-assigned tier entirely — would look green.
func TestAssignedReadyTierServesUnheldCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	// Ordinary labels, including hold-ish ones that are NOT canonical dispatch
	// holds, must not suppress the serve.
	bdScript := `#!/bin/sh
case "$1" in
  ready) printf '%s' '[{"id":"wk-1","status":"open","assignee":"sess-1","labels":["mpr-human-hold","needs-mayor"]}]' ;;
  *) printf '[]' ;;
esac
`
	script := standardAssignedReadyWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 1 || rows[0]["id"] != "wk-1" {
		t.Fatalf("pre-assigned serving broke: an unheld assigned-ready bead must still be served; got %q", out)
	}
}

// TestLegacyControlAssignedReadyTierSkipsHeldCandidate pins that the
// control-dispatcher variant of the same tier carries the identical guard —
// it shares assignedReadyServeGate, so a fix applied to only one shape would
// silently leave the other jammed.
func TestLegacyControlAssignedReadyTierSkipsHeldCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bdScript := `#!/bin/sh
case "$1" in
  ready) printf '%s' '` + heldAssignedReadyRow(beadmeta.HoldMayorLabel) + `' ;;
  *) printf '[]' ;;
esac
`
	script := legacyControlAssignedReadyWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 0 {
		t.Fatalf("held assigned-ready bead was served by the legacy-control tier: %v", rows)
	}
}

// TestAssignedReadyTierServesUnparseableHeldCandidateFailOpen pins the
// fail-open contract for the new guard: a malformed or log-prefixed `bd
// ready` stdout must degrade to serving the candidate unchanged, never to
// dropping it. An over-eager hold check that treated "cannot parse labels"
// as "held" would disable the pre-assigned tier on any bd stdout hiccup.
func TestAssignedReadyTierServesUnparseableHeldCandidateFailOpen(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bdScript := `#!/bin/sh
case "$1" in
  ready) printf '%s' 'WARN: slow store
[{"id":"wk-1","status":"open","assignee":"sess-1"}]' ;;
  *) printf '[]' ;;
esac
`
	script := standardAssignedReadyWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)
	if !strings.Contains(out, "wk-1") {
		t.Fatalf("log-prefixed bd ready stdout must still be served (fail-open), got %q", out)
	}
}

// TestEffectiveWorkQueryAssignedReadyFallsThroughToRoutedWhenHeld is the
// end-to-end gascity#8010 regression: a held OPEN bead assigned to the
// worker's own identity must not swallow the tick before the routed (Tier 3)
// query ever runs. Mirrors TestEffectiveWorkQueryAssignedTierSurfacesEpicWisp's
// harness. bd-1.0.5 (BeadsBDCompatibility105) matches the issue's own
// archived repro exactly, so ephemeralAssignedReadyProbeScript is a no-op and
// out of the picture — this test exercises only the tier this fix touches.
func TestEffectiveWorkQueryAssignedReadyFallsThroughToRoutedWhenHeld(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	for _, label := range beadmeta.DispatchHoldLabels {
		t.Run(label, func(t *testing.T) {
			a := Agent{Name: "worker", Dir: "hello-world"}
			out := runEffectiveWorkQueryForBeads(t, a, BeadsConfig{BDCompatibility: BeadsBDCompatibility105}, map[string]string{
				"GC_SESSION_ID":     "sess-1",
				"GC_SESSION_ORIGIN": "ephemeral",
			}, `#!/bin/sh
set -eu
case "$1" in
  ready)
    case "$*" in
      *"--assignee=sess-1"*)
        printf '%s' '`+heldAssignedReadyRow(label)+`' ;;
      *"--metadata-field gc.routed_to=hello-world/worker"*)
        printf '[{"id":"routed-task","issue_type":"task"}]' ;;
      *) printf '[]' ;;
    esac ;;
  *) printf '[]' ;;
esac
`)
			if !strings.Contains(out, "routed-task") {
				t.Fatalf("EffectiveWorkQuery() did not fall through to routed work behind a held assigned-ready row (label %s): %q", label, out)
			}
			if strings.Contains(out, "held-task") {
				t.Fatalf("EffectiveWorkQuery() served the held row instead of falling through to routed work: %q", out)
			}
		})
	}
}
