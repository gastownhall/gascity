package config

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Regression coverage for the in-progress probe's open tier: a patrol wisp
// stays `open` for its whole life — pouring and assigning it claims nothing —
// so a wisp poured+assigned by an earlier cycle that died before claiming
// matched neither the in_progress scan nor any tier of the prompt-side
// AssignedInProgressQuery, and the next session poured a duplicate while the
// assigned wisp sat unseen (the refinery surplus-wisp accumulation).
//
// Like the sibling tests in workquery_ephemeral_inprogress_hold_test.go, these
// EXECUTE the generated shell against a fake `bd` on PATH so they pin
// observable behavior rather than the script's spelling. The fake routes on
// the query's status clause, which is what distinguishes the probe's two
// snapshot scans from each other and from every other read.

// fakeBdTwoStatusServes returns a fake `bd` whose `query` answers the
// in_progress scan with inProgressRows and the open scan with openRows. `show`
// serves showRows (the enrichment's bd show fallback); `ready` and everything
// else serve `[]` so the tiers below the probe stay empty unless a test says
// otherwise.
func fakeBdTwoStatusServes(inProgressRows, openRows, showRows string) string {
	return `#!/bin/sh
case "$1" in
  query)
    case "$*" in
      *status=in_progress*) printf '%s' '` + inProgressRows + `' ;;
      *status=open*) printf '%s' '` + openRows + `' ;;
      *) printf '[]' ;;
    esac
    ;;
  show) printf '%s' '` + showRows + `' ;;
  *) printf '[]' ;;
esac
`
}

// openWispRow is the `bd query` row for a poured, assigned, unclaimed patrol
// wisp root: ephemeral, molecule-typed, no dependencies, no holds.
func openWispRow(id string) string {
	return `{"id":"` + id + `","status":"open","assignee":"sess-1","title":"mol-refinery-patrol","issue_type":"molecule","labels":[],"ephemeral":true,"dependency_count":0}`
}

const emptyShowRow = `{"id":"x","status":"open","dependencies":[]}`

func runEphemeralInProgressTier(t *testing.T, bdScript string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	return rows
}

// TestEphemeralInProgressProbeServesOpenAssignedWisp is the duplicate-pour
// regression: an open, assigned, unheld wisp must be served by the
// crash-recovery tier's own read, not fall through invisible — the session
// then resumes the poured wisp instead of pouring another one.
func TestEphemeralInProgressProbeServesOpenAssignedWisp(t *testing.T) {
	rows := runEphemeralInProgressTier(t, fakeBdTwoStatusServes(`[]`, `[`+openWispRow("wisp-o")+`]`, emptyShowRow))
	if len(rows) != 1 || rows[0]["id"] != "wisp-o" {
		t.Fatalf("open assigned wisp was not served by the in-progress probe; got %v", rows)
	}
}

// TestEphemeralInProgressProbePrefersClaimedOverOpen pins the union's
// precedence: recovery serves the bead the session already claimed before any
// merely-claimable open sibling, so a crash never jumps to a different wisp.
func TestEphemeralInProgressProbePrefersClaimedOverOpen(t *testing.T) {
	inProgress := `[{"id":"wisp-c","status":"in_progress","assignee":"sess-1","issue_type":"molecule","labels":[],"dependency_count":0}]`
	rows := runEphemeralInProgressTier(t, fakeBdTwoStatusServes(inProgress, `[`+openWispRow("wisp-o")+`]`, emptyShowRow))
	if len(rows) != 1 || rows[0]["id"] != "wisp-c" {
		t.Fatalf("crash-recovery did not prefer the claimed in_progress wisp over the open one; got %v", rows)
	}
}

// TestEphemeralInProgressProbeSkipsOpenEpic pins the open side's epic
// exclusion: a molecule root with children has no executable spec, and serving
// one here would adopt a container the ready tiers refuse.
func TestEphemeralInProgressProbeSkipsOpenEpic(t *testing.T) {
	epic := `{"id":"mol-e","status":"open","assignee":"sess-1","issue_type":"epic","labels":[],"ephemeral":true,"dependency_count":0}`
	rows := runEphemeralInProgressTier(t, fakeBdTwoStatusServes(`[]`, `[`+epic+`]`, emptyShowRow))
	if len(rows) != 0 {
		t.Fatalf("open assigned epic root was served by the in-progress probe: %v", rows)
	}
}

// TestEphemeralInProgressProbeDropsHeldOpenWisp extends the gas-kg6 hold gate
// to the new open side: a wisp parked on a canonical dispatch hold must be
// dropped inside the jq selection, before the truncation, so it neither
// swallows the tier nor shadows an unheld sibling.
func TestEphemeralInProgressProbeDropsHeldOpenWisp(t *testing.T) {
	held := `{"id":"wisp-h","status":"open","assignee":"sess-1","issue_type":"molecule","labels":["` + beadmeta.HoldExternalLabel + `"],"dependency_count":0}`
	free := openWispRow("wisp-f")
	rows := runEphemeralInProgressTier(t, fakeBdTwoStatusServes(`[]`, `[`+held+`,`+free+`]`, emptyShowRow))
	if len(rows) != 1 || rows[0]["id"] != "wisp-f" {
		t.Fatalf("held open wisp was not dropped ahead of the unheld sibling; got %v", rows)
	}
}

// TestEphemeralInProgressProbeGatesOpenRowThroughShow pins that the open side
// goes through the same bd show enrichment the in_progress side uses: an open
// wisp whose blocking dependency is still open is withheld by the serve gate
// (the enrichment resolves blockers over hydrated dependency rows), instead of
// being served raw the way the ungated probe era served blocked in_progress
// steps (ga-qjozkw).
func TestEphemeralInProgressProbeGatesOpenRowThroughShow(t *testing.T) {
	blockedShow := `[{"id":"wisp-o","status":"open","dependencies":[{"id":"step-1","status":"open","dependency_type":"blocks"}]}]`
	rows := runEphemeralInProgressTier(t, fakeBdTwoStatusServes(`[]`, `[`+openWispRow("wisp-o")+`]`, blockedShow))
	if len(rows) != 0 {
		t.Fatalf("dependency-blocked open wisp was served by the in-progress probe: %v", rows)
	}
}

// TestLegacyControlEphemeralInProgressProbeServesOpenWisp pins that the
// control-dispatcher variant carries the identical open tier — it shares the
// probe builder, so a widening applied to only one shape would leave the
// other blind to its own assigned wisps.
func TestLegacyControlEphemeralInProgressProbeServesOpenWisp(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := legacyControlAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"},
		fakeBdTwoStatusServes(`[]`, `[`+openWispRow("wisp-o")+`]`, emptyShowRow))

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 1 || rows[0]["id"] != "wisp-o" {
		t.Fatalf("legacy-control crash-recovery tier did not serve the open assigned wisp; got %q", out)
	}
}

// TestDeferringEphemeralInProgressProbeServesOpenWisp pins the graph-anchor
// variant's open tier the same way; its serve gate runs behind the workflow
// anchor branch, and the same fix applied to only the standard shape would
// strand the anchor cities.
func TestDeferringEphemeralInProgressProbeServesOpenWisp(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := standardAssignedInProgressWorkQueryScriptDeferringGraphAnchor(QueryTopology{}) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"},
		fakeBdTwoStatusServes(`[]`, `[`+openWispRow("wisp-o")+`]`, emptyShowRow))

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 1 || rows[0]["id"] != "wisp-o" {
		t.Fatalf("deferring crash-recovery tier did not serve the open assigned wisp; got %q", out)
	}
}

// TestEphemeralInProgressProbeSharesOpenSnapshotWithReadyTier pins the
// memoization coupling that keeps the combined work query's open scan at one:
// the probe's open snapshot block must be byte-identical to the ready tier's,
// so the later ephemeralAssignedReadyProbeScript call hits the `_set` guard
// instead of re-scanning (workquery_ephemeral_scan_reuse_test.go pins the
// execution count; this pins the text the count depends on).
func TestEphemeralInProgressProbeSharesOpenSnapshotWithReadyTier(t *testing.T) {
	probeScript := ephemeralAssignedInProgressSnapshotShell()
	readyScript := ephemeralStatusSnapshotShell("open_ephemeral", "open")
	if !strings.Contains(probeScript, readyScript) {
		t.Fatalf("in-progress probe's open snapshot drifted from the ready tier's;\nprobe:  %s\nready:  %s",
			probeScript, readyScript)
	}
}
