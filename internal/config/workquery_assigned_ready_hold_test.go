package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Coverage for #8010: the assigned-ready read serves its first row and exits
// the work query, so a dispatch-held OPEN assignment it returns starves every
// tier below it. The read excludes beadmeta.DispatchHoldLabels (never the human
// label) before its --limit=1, so a held row is neither served nor able to
// shadow an unheld assignment. These tests EXECUTE the generated query against a
// fake reader that honors the filter flags, so they pin observable behavior.

// fakeFilteringReader stands in for both `bd` and `gc`. `ready` and `list`
// filter the rows in $FAKE_ROWS by --status (default open), --assignee,
// --unassigned, every --metadata-field key=value and every --exclude-label,
// then apply --limit, in file order; `show` reports no dependencies; anything
// else is empty. Every call is logged to $FAKE_READ_LOG as "<binary> <args>".
const fakeFilteringReader = `#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >> "$FAKE_READ_LOG"
case "$1" in
  ready|list) ;;
  show) printf '[{"dependencies":[]}]'; exit 0 ;;
  *) printf '[]'; exit 0 ;;
esac
shift
status=open; assignee=""; unassigned=false; meta='[]'; excl='[]'; limit=0
while [ $# -gt 0 ]; do
  case "$1" in
    --status) shift; status=$1 ;;
    --assignee=*) assignee=${1#--assignee=} ;;
    --unassigned) unassigned=true ;;
    --metadata-field) shift; meta=$(printf '%s' "$meta" | jq -c --arg kv "$1" '. + [$kv]') ;;
    --exclude-label) shift; excl=$(printf '%s' "$excl" | jq -c --arg l "$1" '. + [$l]') ;;
    --limit=*) limit=${1#--limit=} ;;
    --limit) shift; limit=$1 ;;
  esac
  shift
done
jq -c --arg st "$status" --arg a "$assignee" --argjson u "$unassigned" \
  --argjson meta "$meta" --argjson excl "$excl" --argjson lim "$limit" '
  [.[] | . as $row | select(
    ($row.status == $st)
    and ($a == "" or ($row.assignee // "") == $a)
    and (($u | not) or ($row.assignee // "") == "")
    and all($meta[]; . as $kv | ($kv | index("=")) as $i |
      (($row.metadata // {})[$kv[:$i]] // "") == $kv[$i+1:])
    and ([($row.labels // [])[] | select(IN($excl[]))] | length) == 0
  )] | if $lim > 0 then .[:$lim] else . end' "$FAKE_ROWS"
`

const (
	assignedHoldTestAlias       = "example/pool.worker"
	assignedHoldTestSessionID   = "sess-1"
	assignedHoldTestSessionName = "pool__worker-sess-1"
)

type fakeReaderRow struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Type     string            `json:"issue_type"`
	Assignee string            `json:"assignee"`
	Labels   []string          `json:"labels,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func assignedOpenRow(id, assignee string, labels ...string) fakeReaderRow {
	return fakeReaderRow{ID: id, Status: "open", Type: "task", Assignee: assignee, Labels: labels}
}

func routedOpenRow(id, target string) fakeReaderRow {
	return fakeReaderRow{ID: id, Status: "open", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target}}
}

func assignedHoldPoolAgent() *Agent {
	zero, one := 0, 1
	return &Agent{Name: "worker", Dir: "example", BindingName: "pool", MinActiveSessions: &zero, MaxActiveSessions: &one}
}

// assignedHoldTopologies are the three assigned-ready reader shapes: the bd
// 1.0.4 single-store read, the bd 1.0.5 --include-ephemeral read, and the
// federated `gc ready` read.
func assignedHoldTopologies() map[string]QueryTopology {
	return map[string]QueryTopology{
		"bd104":     {},
		"bd105":     singleStoreTopology(),
		"federated": federatedTopology(),
	}
}

func assignedHoldSeatEnv() map[string]string {
	return map[string]string{
		"GC_SESSION_ID":     assignedHoldTestSessionID,
		"GC_SESSION_NAME":   assignedHoldTestSessionName,
		"GC_ALIAS":          assignedHoldTestAlias,
		"GC_SESSION_ORIGIN": "ephemeral",
	}
}

// runFilteredQuery runs command over rows with env and returns the decoded
// output rows and the reader call log.
func runFilteredQuery(t *testing.T, command string, env map[string]string, rows []fakeReaderRow) ([]fakeReaderRow, string) {
	t.Helper()
	requireJQ(t)
	tmp := t.TempDir()
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	rowsPath := filepath.Join(tmp, "rows.json")
	if err := os.WriteFile(rowsPath, data, 0o644); err != nil {
		t.Fatalf("write rows: %v", err)
	}
	logPath := filepath.Join(tmp, "reads.log")
	queryEnv := map[string]string{"FAKE_ROWS": rowsPath, "FAKE_READ_LOG": logPath}
	for k, v := range env {
		queryEnv[k] = v
	}
	res := runGeneratedQueryWithBD(t, command, queryEnv, fakeFilteringReader, fakeFilteringReader)
	if res.exit != 0 {
		t.Fatalf("query exited %d; stderr=%s", res.exit, res.stderr)
	}
	var got []fakeReaderRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
		t.Fatalf("query output is not a JSON row array: %v (output %q)", err, res.stdout)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read reader log: %v", err)
	}
	return got, string(log)
}

// assertServed fails unless the query's first served row is wantID.
func assertServed(t *testing.T, got []fakeReaderRow, wantID string) {
	t.Helper()
	if len(got) == 0 || got[0].ID != wantID {
		t.Fatalf("query served %+v, want %s first", got, wantID)
	}
}

// TestAssignedReadyHeldOnlyFallsThroughToRoutedTier: a held OPEN assignment on
// any seat identity, for either hold and every reader shape, yields an empty
// assigned-ready page, so the routed step below is served.
func TestAssignedReadyHeldOnlyFallsThroughToRoutedTier(t *testing.T) {
	identities := map[string]string{
		"GC_SESSION_ID":   assignedHoldTestSessionID,
		"GC_SESSION_NAME": assignedHoldTestSessionName,
		"GC_ALIAS":        assignedHoldTestAlias,
	}
	for topoName, topo := range assignedHoldTopologies() {
		for identityName, identity := range identities {
			for _, hold := range beadmeta.DispatchHoldLabels {
				t.Run(topoName+"/"+identityName+"/"+hold, func(t *testing.T) {
					got, log := runFilteredQuery(t, assignedHoldPoolAgent().EffectiveWorkQueryFor(topo), assignedHoldSeatEnv(), []fakeReaderRow{
						assignedOpenRow("held", identity, hold),
						routedOpenRow("routed", assignedHoldTestAlias),
					})
					assertServed(t, got, "routed")
					if !strings.Contains(log, "--assignee="+identity+" --exclude-label hold:mayor --exclude-label hold:external --json --limit=1") {
						t.Errorf("assigned-ready read for %s does not exclude the dispatch holds before its limit; reads:\n%s", identity, log)
					}
				})
			}
		}
	}
}

// TestLegacyControlAssignedReadyHeldOnlyFallsThrough covers the control
// dispatcher's candidate loop, which reads the same assigned-ready command for
// its own identity and the legacy workflow-control alias.
func TestLegacyControlAssignedReadyHeldOnlyFallsThrough(t *testing.T) {
	const legacyAlias = "rig/workflow-control"
	for topoName, topo := range assignedHoldTopologies() {
		for _, hold := range beadmeta.DispatchHoldLabels {
			t.Run(topoName+"/"+hold, func(t *testing.T) {
				script := legacyControlAssignedReadyWorkQueryScript(topo) + `printf "[]"`
				got, _ := runFilteredQuery(t, script, map[string]string{"GC_ALIAS": "rig/" + ControlDispatcherAgentName},
					[]fakeReaderRow{assignedOpenRow("held", legacyAlias, hold)})
				if len(got) != 0 {
					t.Fatalf("legacy candidate loop served %+v, want the held workflow-control assignment skipped", got)
				}
			})
		}
	}
}

// TestAssignedReadyScriptsExcludeDispatchHoldsNotHuman pins the generator: both
// assigned-ready loops carry every canonical dispatch hold and never the human
// label, on both readers.
func TestAssignedReadyScriptsExcludeDispatchHoldsNotHuman(t *testing.T) {
	for topoName, topo := range assignedHoldTopologies() {
		for name, got := range map[string]string{
			"standardAssignedReadyWorkQueryScript":      standardAssignedReadyWorkQueryScript(topo),
			"legacyControlAssignedReadyWorkQueryScript": legacyControlAssignedReadyWorkQueryScript(topo),
		} {
			for _, label := range beadmeta.DispatchHoldLabels {
				if want := `--exclude-label "` + label + `"`; !strings.Contains(got, want) {
					t.Errorf("%s/%s() missing %q: %q", topoName, name, want, got)
				}
			}
			if strings.Contains(got, `--exclude-label "`+beadmeta.HumanLabel+`"`) {
				t.Errorf("%s/%s() excludes the human label, which is not a dispatch hold: %q", topoName, name, got)
			}
		}
	}
}

// TestAssignedReadyServesUnheldAssignmentBehindHeldHead: the exclusion rides
// the reader ahead of its --limit=1, so a held assignment that sorts first
// cannot hide an unheld one for the same identity, and the assigned tier
// answers without reaching the routed tier.
func TestAssignedReadyServesUnheldAssignmentBehindHeldHead(t *testing.T) {
	for topoName, topo := range map[string]QueryTopology{"bd": singleStoreTopology(), "federated": federatedTopology()} {
		t.Run(topoName, func(t *testing.T) {
			got, log := runFilteredQuery(t, assignedHoldPoolAgent().EffectiveWorkQueryFor(topo), assignedHoldSeatEnv(), []fakeReaderRow{
				assignedOpenRow("held-head", assignedHoldTestAlias, beadmeta.HoldMayorLabel),
				assignedOpenRow("unheld", assignedHoldTestAlias),
				routedOpenRow("routed", assignedHoldTestAlias),
			})
			if len(got) != 1 || got[0].ID != "unheld" {
				t.Fatalf("query served %+v, want only the unheld assignment", got)
			}
			if strings.Contains(log, beadmeta.RoutedToMetadataKey+"=") {
				t.Errorf("the routed tier ran although the assigned tier had work; reads:\n%s", log)
			}
		})
	}
}

// TestAssignedReadyStaysHumanTransparent: bd's human label is not a dispatch
// hold, nor is a non-canonical hold:human label, so a task assigned to the seat
// that carries one is still served ahead of routed work on every reader shape.
func TestAssignedReadyStaysHumanTransparent(t *testing.T) {
	for topoName, topo := range assignedHoldTopologies() {
		for _, label := range []string{beadmeta.HumanLabel, "hold:human"} {
			t.Run(topoName+"/"+label, func(t *testing.T) {
				got, _ := runFilteredQuery(t, assignedHoldPoolAgent().EffectiveWorkQueryFor(topo), assignedHoldSeatEnv(), []fakeReaderRow{
					assignedOpenRow("labeled", assignedHoldTestAlias, label),
					routedOpenRow("routed", assignedHoldTestAlias),
				})
				if len(got) != 1 || got[0].ID != "labeled" {
					t.Fatalf("query served %+v, want the %s-labeled assignment ahead of the routed step", got, label)
				}
			})
		}
	}
}
