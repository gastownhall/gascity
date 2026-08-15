package config

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Regression coverage for the ephemeral (wisp) sibling of the gas-kg6 hold
// gate, from the #5114 review: ephemeralAssignedInProgressProbeScript sits in
// the same tier-1 slot as the `bd list --status in_progress` query, immediately
// after the block that received the nheld gate, and had no hold check of its
// own. A held ephemeral bead assigned to the session made the probe exit the
// script with only that bead; the Go-side filter then stripped it, so the hook
// reported action=drain / no_work while genuinely ready work sat unqueried —
// the same starvation the gas-kg6 fix exists to remove, wearing a different
// label.
//
// Like the sibling tests in workquery_inprogress_hold_test.go, these EXECUTE
// the generated shell against a fake `bd` on PATH so they pin observable
// behavior rather than the script's spelling.
//
// Scope note (ga-5736js): only the in_progress WORK-SERVING probe is
// hold-gated. ephemeralAssignedReadyProbeScript stays hold-transparent by
// design — pinned by
// TestEphemeralAssignedReadyProbeScriptDoesNotExcludeDispatchHoldLabels.

// heldEphemeralInProgressRow is the ephemeral-store crash-recovery row for a
// bead parked on label, as `bd query 'ephemeral=true AND status=in_progress'`
// reports it.
func heldEphemeralInProgressRow(label string) string {
	return `[{"id":"wisp-1","status":"in_progress","assignee":"sess-1","title":"wisp","labels":["` +
		label + `","mysql-cutover"]}]`
}

// fakeBdHeldEphemeralInProgress reproduces the review's scenario: `bd list`
// sees nothing (the bead lives in ephemeral storage, invisible to it),
// `bd query` reports one in_progress ephemeral bead assigned to the session
// and parked on label, and `bd ready` yields different, genuinely ready work.
func fakeBdHeldEphemeralInProgress(label string) string {
	return `#!/bin/sh
case "$1" in
  query) printf '%s' '` + heldEphemeralInProgressRow(label) + `' ;;
  ready) printf '%s' '[{"id":"wk-2","status":"open","assignee":"sess-1"}]' ;;
  *) printf '[]' ;;
esac
`
}

// TestEphemeralInProgressProbeFallsThroughWhenHeld is the primary regression
// at the shell layer: a parked ephemeral candidate must not swallow the tick —
// the ready-gated tier below still runs and serves the worker's other ready
// work, instead of the script exiting with a bead the Go filter must discard.
func TestEphemeralInProgressProbeFallsThroughWhenHeld(t *testing.T) {
	for _, label := range beadmeta.DispatchHoldLabels {
		t.Run(label, func(t *testing.T) {
			if _, err := exec.LookPath("jq"); err != nil {
				t.Skip("jq not available; the work-query shell requires it")
			}
			script := standardAssignedWorkQueryScript(QueryTopology{}) + `printf "[]"`
			out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, fakeBdHeldEphemeralInProgress(label))

			var rows []map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
			}
			if len(rows) != 1 || rows[0]["id"] != "wk-2" {
				t.Fatalf("held ephemeral in_progress candidate did not fall through to the ready tier; got %q", out)
			}
		})
	}
}

// TestEphemeralInProgressProbeSkipsHeldCandidate isolates the probe's tier:
// with no ready work behind it, a parked ephemeral candidate yields nothing
// rather than being re-served.
func TestEphemeralInProgressProbeSkipsHeldCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bdScript := `#!/bin/sh
case "$1" in
  query) printf '%s' '` + heldEphemeralInProgressRow(beadmeta.HoldExternalLabel) + `' ;;
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
		t.Fatalf("held ephemeral in_progress bead was re-served by the crash-recovery tier: %v", rows)
	}
}

// TestEphemeralInProgressProbeServesUnheldCandidate is the load-bearing
// negative control: non-canonical hold-ish labels must not suppress the
// re-serve, and a held first row must not shadow an unheld second one — the
// filter drops held rows before the probe truncates to one candidate.
func TestEphemeralInProgressProbeServesUnheldCandidate(t *testing.T) {
	cases := []struct {
		name string
		rows string
		want string
	}{
		{
			name: "non-canonical labels are not holds",
			rows: `[{"id":"wisp-1","status":"in_progress","assignee":"sess-1","labels":["mpr-human-hold","needs-mayor"]}]`,
			want: "wisp-1",
		},
		{
			name: "held first row does not shadow an unheld second",
			rows: `[{"id":"wisp-1","status":"in_progress","assignee":"sess-1","labels":["` + beadmeta.HoldExternalLabel + `"]},` +
				`{"id":"wisp-2","status":"in_progress","assignee":"sess-1","labels":[]}]`,
			want: "wisp-2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath("jq"); err != nil {
				t.Skip("jq not available; the work-query shell requires it")
			}
			bdScript := `#!/bin/sh
case "$1" in
  query) printf '%s' '` + tc.rows + `' ;;
  *) printf '[]' ;;
esac
`
			script := standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
			out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

			var rows []map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
			}
			if len(rows) != 1 || rows[0]["id"] != tc.want {
				t.Fatalf("unheld ephemeral in_progress bead was not served (want %s); got %q", tc.want, out)
			}
		})
	}
}

// TestLegacyControlEphemeralInProgressProbeSkipsHeldCandidate pins that the
// control-dispatcher variant carries the identical guard — it shares
// ephemeralAssignedInProgressProbeScript, so a fix applied to only one shape
// would silently leave the other jammed.
func TestLegacyControlEphemeralInProgressProbeSkipsHeldCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bdScript := `#!/bin/sh
case "$1" in
  query) printf '%s' '` + heldEphemeralInProgressRow(beadmeta.HoldMayorLabel) + `' ;;
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
		t.Fatalf("held ephemeral in_progress bead was re-served by the legacy-control crash-recovery tier: %v", rows)
	}
}

// TestEphemeralInProgressProbeUnparseableFallsThrough pins the probe's failure
// mode, which differs from the bd-list tier's fail-open serve: the probe's jq
// consumes `bd query` stdout directly, so garbage yields an empty candidate and
// the probe falls through. It must never serve raw non-JSON stdout, and the
// hold filter must not change that.
func TestEphemeralInProgressProbeUnparseableFallsThrough(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bdScript := `#!/bin/sh
case "$1" in
  query) printf '%s' 'warning: ephemeral store not initialized' ;;
  *) printf '[]' ;;
esac
`
	script := standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("unparseable ephemeral query stdout must fall through, not be served: got %q", out)
	}
}
