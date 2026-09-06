package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// graphResidentInProgressRow is what the federated reader serves for a claim
// that lives in a relocated class binding: the row a single-store `bd list`
// could not see at all, already carrying blocked_by resolved from the leg that
// holds it.
func graphResidentInProgressRow(blockedBy string) string {
	return `[{"id":"gcg-own","status":"in_progress","assignee":"sess-1","title":"graph step","blocked_by":` + blockedBy + `}]`
}

// fakeGCServingInProgress answers `gc ready --status in_progress` with the given
// row and everything else with an empty array, so only the crash-recovery tier
// can decide the outcome.
func fakeGCServingInProgress(row string) string {
	return `#!/bin/sh
case "$1" in
  ready) printf '%s' '` + row + `' ;;
  *) printf '[]' ;;
esac
`
}

// fakeBDRecordingShow answers every bd subcommand with an empty array and
// records whether `bd show` was ever reached, which is how a test proves the
// presence key short-circuited the single-store fallback.
const fakeBDRecordingShow = `#!/bin/sh
case "$1" in
  show) printf '%s\n' "$@" >> "$BD_SHOW_LOG"; printf '[]' ;;
  *) printf '[]' ;;
esac
`

// runFederatedInProgressTier executes the FEDERATED crash-recovery tier against
// a fake gc/bd pair and returns the decoded rows plus whether `bd show` ran.
func runFederatedInProgressTier(t *testing.T, row string) (rows []map[string]any, bdShowRan bool) {
	t.Helper()
	requireJQ(t)
	showLog := filepath.Join(t.TempDir(), "bd-show.log")
	script := standardAssignedInProgressWorkQueryScript(federatedTopology()) + `printf "[]"`
	res := runGeneratedQueryWithBD(t, script, map[string]string{
		"GC_SESSION_ID": "sess-1",
		"BD_SHOW_LOG":   showLog,
	}, fakeGCServingInProgress(row), fakeBDRecordingShow)
	if res.exit != 0 {
		t.Fatalf("federated tier exited %d; stderr=%s", res.exit, res.stderr)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, res.stdout)
	}
	if _, err := os.Stat(showLog); err == nil {
		bdShowRan = true
	}
	return rows, bdShowRan
}

func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
}

// TestFederatedInProgressTierServesAGraphResidentClaim is the row-level proof of
// the residency fix: a claim visible only through the federated reader is
// served, and its carried blocked_by is used INSTEAD of a `bd show` that would
// resolve nothing for a relocated id.
func TestFederatedInProgressTierServesAGraphResidentClaim(t *testing.T) {
	rows, bdShowRan := runFederatedInProgressTier(t, graphResidentInProgressRow(`[]`))
	if len(rows) != 1 || rows[0]["id"] != "gcg-own" {
		t.Fatalf("rows = %v, want the graph-resident claim served", rows)
	}
	if bdShowRan {
		t.Error("the federated tier fell back to `bd show` for a row that already carried blocked_by; on a split city that resolves nothing and would overwrite a correct answer with an empty one")
	}
}

// TestFederatedInProgressTierSkipsAGatedGraphResidentClaim is the
// differently-failing control on the gate dimension: the same row with an OPEN
// blocker must NOT be served, or every tick re-serves a step no worker can
// advance.
func TestFederatedInProgressTierSkipsAGatedGraphResidentClaim(t *testing.T) {
	rows, _ := runFederatedInProgressTier(t, graphResidentInProgressRow(`[{"id":"gcg-gate","status":"open"}]`))
	if len(rows) != 0 {
		t.Fatalf("rows = %v, want none: the row's only blocker is open", rows)
	}
}

// TestFederatedInProgressTierServesAClosedBlockerRow is the other side of the
// gate control: a closed blocker does not block.
func TestFederatedInProgressTierServesAClosedBlockerRow(t *testing.T) {
	rows, _ := runFederatedInProgressTier(t, graphResidentInProgressRow(`[{"id":"gcg-gate","status":"closed"}]`))
	if len(rows) != 1 || rows[0]["id"] != "gcg-own" {
		t.Fatalf("rows = %v, want the row served: its only blocker is closed", rows)
	}
}

// TestFederatedInProgressTierFallsBackWhenTheRowCarriesNoBlockedBy pins that the
// presence key is a KEY, not a replacement: a federated row without the field
// still gets the `bd show` enrichment, so the swap cannot silently disable the
// gate check.
func TestFederatedInProgressTierFallsBackWhenTheRowCarriesNoBlockedBy(t *testing.T) {
	_, bdShowRan := runFederatedInProgressTier(t,
		`[{"id":"gcg-own","status":"in_progress","assignee":"sess-1","title":"graph step"}]`)
	if !bdShowRan {
		t.Error("the federated tier skipped its enrichment for a row with no blocked_by; the gate check would then never run")
	}
}

// TestFederatedInProgressTierPropagatesADeadLeg pins the fail-loud half. An
// empty resume answer produced by a dead leg is exactly the silent blindness
// this swap exists to end, so the tier must exit with the reader.
func TestFederatedInProgressTierPropagatesADeadLeg(t *testing.T) {
	requireJQ(t)
	script := standardAssignedInProgressWorkQueryScript(federatedTopology()) + `printf "[]"`
	res := runGeneratedQuery(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, fakeGCReadyFails)
	if res.exit == 0 {
		t.Fatalf("federated crash-recovery tier exited 0 on a dead leg; stdout=%q", res.stdout)
	}
	if !strings.Contains(res.stderr, "rig") {
		t.Errorf("stderr = %q, want the dead leg named", res.stderr)
	}
}

// TestSingleStoreInProgressTierStillShellsBdList is the topology control: the
// deployed single-store path is untouched by all of the above.
func TestSingleStoreInProgressTierStillShellsBdList(t *testing.T) {
	script := standardAssignedInProgressWorkQueryScript(singleStoreTopology())
	if !strings.Contains(script, bdListInProgressCommand) {
		t.Fatalf("single-store crash-recovery tier no longer shells %q: %q", bdListInProgressCommand, script)
	}
	if strings.Contains(script, gcReadyCommand) {
		t.Fatalf("single-store crash-recovery tier shells the federated reader: %q", script)
	}
}
