package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// fakeGCInProgressWindow answers `gc ready --status in_progress` with the rows
// in $FAKE_GC_IN_PROGRESS_ROWS the way the real reader answers them: rows whose
// issue_type an --exclude-type names (matched exactly) or carrying an
// --exclude-label label dropped, then bounded by --limit (after the merge and
// the order, so a --limit=1 read is the first row and nothing else), then,
// under --until-unblocked, ended at the first row with no blocker that is not
// closed. A row marked "unresolvable" is one whose blocker no store holds:
// the read fails when it has to resolve that row, which is every row of the
// window, or under --until-unblocked every row up to the cut. Every other
// `gc ready` answers an empty array, so only the crash-recovery tier can decide
// the outcome.
const fakeGCInProgressWindow = `#!/bin/sh
[ "$1" = ready ] || { printf '[]'; exit 0; }
case " $* " in
  *" --status in_progress "*) ;;
  *) printf '[]'; exit 0 ;;
esac
limit=0
until=false
excluded='[]'
excluded_types='[]'
prev=
for arg in "$@"; do
  [ "$prev" = --exclude-label ] && excluded=$(printf '%s' "$excluded" | jq -c --arg label "$arg" '. + [$label]')
  [ "$prev" = --exclude-type ] && excluded_types=$(printf '%s' "$excluded_types" | jq -c --arg t "$arg" '. + [$t]')
  case "$arg" in
    --exclude-type=*) excluded_types=$(printf '%s' "$excluded_types" | jq -c --arg t "${arg#--exclude-type=}" '. + [$t]') ;;
    --limit=*) limit="${arg#--limit=}" ;;
    --until-unblocked) until=true ;;
  esac
  prev="$arg"
done
rows=$(printf '%s' "$FAKE_GC_IN_PROGRESS_ROWS" | jq -c --argjson excluded "$excluded" --argjson excluded_types "$excluded_types" --argjson limit "$limit" --argjson until "$until" '
  def unblocked: ([(.blocked_by // [])[] | select(((.status // "") | ascii_downcase) != "closed")] | length) == 0;
  [.[] | select((.issue_type // "") as $t | $excluded_types | index($t) == null)
       | select(([(.labels // [])[] | . as $label | select($excluded | index($label) != null)] | length) == 0)]
  | (if $limit > 0 then .[:$limit] else . end)
  | if $until then
      reduce .[] as $row ({rows: [], done: false};
        if .done then . else .rows += [$row] | .done = (($row.unresolvable // false) or ($row | unblocked)) end)
      | .rows
    else . end')
bad=$(printf '%s' "$rows" | jq -r '[.[] | select(.unresolvable // false) | .id][0] // empty')
if [ -n "$bad" ]; then
  printf 'gc ready: rig store: resolving blocker "gr-elsewhere" of "%s": bead not found\n' "$bad" >&2
  exit 1
fi
printf '%s' "$rows"
`

// Federated in_progress rows, in the reader's order. The gated rows sort first,
// the way a P0 claim in one store sorts ahead of a P2 claim in another.
const (
	federatedBlockedWinnerRow = `{"id":"gcr-blocked","status":"in_progress","assignee":"sess-1","priority":0,"blocked_by":[{"id":"gcr-gate","status":"open"}]}`
	federatedServableRow      = `{"id":"gcs-servable","status":"in_progress","assignee":"sess-1","priority":2}`
	federatedBlockedStepRow   = `{"id":"gcs-blocked-step","status":"in_progress","assignee":"sess-1","priority":1,"blocked_by":[{"id":"gcs-gate","status":"open"}]}`
	federatedAnchorRow        = `{"id":"gc-root","status":"in_progress","assignee":"sess-1","priority":0,"metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2"}}`
)

// federatedHeldWinnerRow carries a dispatch hold and no blocker.
var federatedHeldWinnerRow = `{"id":"gc-held","status":"in_progress","assignee":"sess-1","priority":0,"labels":["` + beadmeta.HoldExternalLabel + `"]}`

// federatedUnresolvableRow is a claim whose blocking edge targets a bead no
// store holds, so the reader cannot resolve its blockers.
const federatedUnresolvableRow = `{"id":"gcr-unresolvable","status":"in_progress","assignee":"sess-1","priority":3,"unresolvable":true}`

func federatedInProgressRows(rows ...string) string {
	return "[" + strings.Join(rows, ",") + "]"
}

// federatedInProgressQuery is one generated query that carries the
// crash-recovery tier.
type federatedInProgressQuery struct {
	name    string
	command string
	// servesAnchor is whether the query serves an assigned graph workflow root
	// from this tier: the standalone tier does, while the combined work query
	// defers it behind the session's executable work.
	servesAnchor bool
}

// federatedInProgressQueries builds, on a federated topology, both queries the
// crash-recovery tier appears in — the standalone assigned_in_progress_query and
// the combined work_query — for a plain agent and for the legacy
// control-dispatcher shape, whose tier loops over two candidate identities.
func federatedInProgressQueries() []federatedInProgressQuery {
	return federatedInProgressQueriesFor(federatedTopology())
}

// federatedInProgressQueriesFor is federatedInProgressQueries on topo.
func federatedInProgressQueriesFor(topo QueryTopology) []federatedInProgressQuery {
	var out []federatedInProgressQuery
	for _, shape := range []struct {
		name  string
		agent *Agent
	}{
		{"worker", &Agent{Name: "worker"}},
		{"legacy control", &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}},
	} {
		out = append(out,
			federatedInProgressQuery{shape.name + "/assigned_in_progress_query", shape.agent.EffectiveAssignedInProgressQueryFor(topo), true},
			federatedInProgressQuery{shape.name + "/work_query", shape.agent.EffectiveWorkQueryFor(topo), false},
		)
	}
	return out
}

// runFederatedInProgressWindow runs command with GC_SESSION_ID=sess-1 against
// fakeGCInProgressWindow serving rows, and returns the served ids in order.
func runFederatedInProgressWindow(t *testing.T, command, rows string) []string {
	t.Helper()
	res := runGeneratedQuery(t, command, map[string]string{
		"GC_SESSION_ID":            "sess-1",
		"FAKE_GC_IN_PROGRESS_ROWS": rows,
	}, fakeGCInProgressWindow)
	if res.exit != 0 {
		t.Fatalf("query exited %d; stderr=%s", res.exit, res.stderr)
	}
	return workQueryOutputIDOrder(t, strings.TrimSpace(res.stdout))
}

// TestFederatedInProgressTierServesTheFirstServableRowOfTheWindow pins that a
// blocked or held row first in the federated reader's order does not hide the
// session's servable claim behind it. `gc ready` merges every store into one
// ordered answer, so a one-row read was one row for the whole city: when that
// row was gated the tier fell through, and the session was handed fresh work
// while it still held a claim it could resume — the accumulation the tier
// exists to stop. `gc hook` reads each store separately, so there one store's
// gated row never hid another store's claim.
func TestFederatedInProgressTierServesTheFirstServableRowOfTheWindow(t *testing.T) {
	requireJQ(t)
	for _, tc := range []struct {
		name string
		rows string
		want []string
	}{
		{"blocked winner", federatedInProgressRows(federatedBlockedWinnerRow, federatedServableRow), []string{"gcs-servable"}},
		{"held winner", federatedInProgressRows(federatedHeldWinnerRow, federatedServableRow), []string{"gcs-servable"}},
		{"blocked and held ahead", federatedInProgressRows(federatedHeldWinnerRow, federatedBlockedWinnerRow, federatedServableRow), []string{"gcs-servable"}},
		// The control: with no servable row the tier still falls through, and
		// the query answers no work rather than a gated claim.
		{"no servable row", federatedInProgressRows(federatedHeldWinnerRow, federatedBlockedWinnerRow), []string{}},
	} {
		for _, q := range federatedInProgressQueries() {
			t.Run(tc.name+"/"+q.name, func(t *testing.T) {
				got := runFederatedInProgressWindow(t, q.command, tc.rows)
				if !slices.Equal(got, tc.want) {
					t.Fatalf("served %v, want %v", got, tc.want)
				}
			})
		}
	}
}

// TestFederatedInProgressTierServesTheClaimAheadOfAnUnresolvableRow pins that a
// claim the tier never serves cannot fail it. The window is twenty rows, and the
// reader fails a read whose rows' blockers it cannot resolve, so reading the
// window whole made a claim blocked by a bead no store holds fail the query for
// the session's resumable claim ahead of it: no crash recovery, and a failed
// hook. The tier's read ends at the first unblocked row, so a row past it is
// never resolved.
func TestFederatedInProgressTierServesTheClaimAheadOfAnUnresolvableRow(t *testing.T) {
	requireJQ(t)
	for _, tc := range []struct {
		name string
		rows string
	}{
		{"leading claim", federatedInProgressRows(federatedServableRow, federatedUnresolvableRow)},
		{"behind a gated claim", federatedInProgressRows(federatedHeldWinnerRow, federatedBlockedWinnerRow, federatedServableRow, federatedUnresolvableRow)},
	} {
		for _, q := range federatedInProgressQueries() {
			t.Run(tc.name+"/"+q.name, func(t *testing.T) {
				if got := runFederatedInProgressWindow(t, q.command, tc.rows); !slices.Equal(got, []string{"gcs-servable"}) {
					t.Fatalf("served %v, want the resumable claim gcs-servable", got)
				}
			})
		}
	}
}

// TestFederatedInProgressTierFailsOnAnUnresolvableRowAheadOfTheCut is the
// fail-loud half: a claim whose blockers cannot be resolved, at or ahead of the
// first claim the tier could serve, may be the claim to resume, so the query
// fails, naming it, rather than fall through and hand the session fresh work.
func TestFederatedInProgressTierFailsOnAnUnresolvableRowAheadOfTheCut(t *testing.T) {
	requireJQ(t)
	for _, tc := range []struct {
		name string
		rows string
	}{
		{"leading row", federatedInProgressRows(federatedUnresolvableRow, federatedServableRow)},
		{"behind a gated claim", federatedInProgressRows(federatedBlockedWinnerRow, federatedUnresolvableRow, federatedServableRow)},
	} {
		for _, q := range federatedInProgressQueries() {
			t.Run(tc.name+"/"+q.name, func(t *testing.T) {
				res := runGeneratedQuery(t, q.command, map[string]string{
					"GC_SESSION_ID":            "sess-1",
					"FAKE_GC_IN_PROGRESS_ROWS": tc.rows,
				}, fakeGCInProgressWindow)
				if res.exit == 0 {
					t.Fatalf("query exited 0 with %q; an unresolvable claim ahead of the cut must fail it", res.stdout)
				}
				if !strings.Contains(res.stderr, "gcr-unresolvable") {
					t.Fatalf("stderr = %q, want the unresolvable claim named", res.stderr)
				}
			})
		}
	}
}

// TestFederatedWorkQueryServesTheServableStepBehindAGraphAnchor pins the same
// window on the anchor path. The combined work query defers an assigned graph
// workflow root and reads the window again for an ordinary claim; that pick has
// to skip a gated step too, or the root is served while a step the session can
// resume sits behind the gated one. The standalone tier still serves the root.
func TestFederatedWorkQueryServesTheServableStepBehindAGraphAnchor(t *testing.T) {
	requireJQ(t)
	rows := federatedInProgressRows(federatedAnchorRow, federatedBlockedStepRow, federatedServableRow)
	for _, q := range federatedInProgressQueries() {
		t.Run(q.name, func(t *testing.T) {
			want := []string{"gcs-servable"}
			if q.servesAnchor {
				want = []string{"gc-root"}
			}
			if got := runFederatedInProgressWindow(t, q.command, rows); !slices.Equal(got, want) {
				t.Fatalf("served %v, want %v", got, want)
			}
		})
	}
}

// TestFederatedInProgressTierServesUnparseableOutputUnchanged pins that the
// window's narrowing fails open like the serve gate after it: reader output jq
// cannot parse is served as printed, never dropped, so a log-prefixed answer
// cannot disable crash recovery.
func TestFederatedInProgressTierServesUnparseableOutputUnchanged(t *testing.T) {
	requireJQ(t)
	const unparseable = `gc: log line [{"id":"gcs-servable","status":"in_progress"}]`
	fakeGC := `#!/bin/sh
case "$1" in
  ready) printf '%s' '` + unparseable + `' ;;
  *) printf '[]' ;;
esac
`
	a := &Agent{Name: "worker"}
	res := runGeneratedQuery(t, a.EffectiveAssignedInProgressQueryFor(federatedTopology()), map[string]string{"GC_SESSION_ID": "sess-1"}, fakeGC)
	if res.exit != 0 {
		t.Fatalf("query exited %d; stderr=%s", res.exit, res.stderr)
	}
	if got := strings.TrimSpace(res.stdout); got != unparseable {
		t.Fatalf("served %q, want the reader's unparseable output unchanged %q", got, unparseable)
	}
}
