package main

// Assigned-hold starvation (#8010). A pool seat has an OPEN bead assigned to
// one of its identities and parked on a dispatch hold, while an unassigned step
// routed to the seat's alias is ready. These tests run the REAL generated work
// query against a fake store reader that honors the filter flags the query
// passes, then feed its output to the real claim path.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	assignedHoldAlias       = "example/pool.worker"
	assignedHoldSession     = "session-example"
	assignedHoldSessionName = "pool__worker-" + assignedHoldSession
	assignedHoldStepID      = "test-routed"
	assignedHoldDecoyID     = "test-held"
)

// assignedHoldFakeReader stands in for both `bd` and `gc`. `ready` and `list`
// filter the rows in $FAKE_ROWS by --status (default open), --assignee,
// --unassigned, every --metadata-field key=value and every --exclude-label,
// then apply --limit; `show` reports no dependencies; anything else is empty.
// Every call is logged to $FAKE_READ_LOG as "<binary> <args>".
const assignedHoldFakeReader = `#!/bin/sh
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

type assignedHoldRow struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Type     string            `json:"issue_type"`
	Assignee string            `json:"assignee"`
	Labels   []string          `json:"labels,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func assignedHoldRoutedStep() assignedHoldRow {
	return assignedHoldRow{
		ID:       assignedHoldStepID,
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: assignedHoldAlias},
	}
}

func assignedHoldDecoy(status, assignee, hold string) assignedHoldRow {
	return assignedHoldRow{ID: assignedHoldDecoyID, Status: status, Type: "task", Assignee: assignee, Labels: []string{hold}}
}

func assignedHoldTopology(federated bool) config.QueryTopology {
	return config.QueryTopology{
		Beads:          config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105},
		FederatedReady: federated,
	}
}

// assignedHoldRunWorkQuery runs the pool seat's generated work query over rows
// and returns its stdout and the reader call log.
func assignedHoldRunWorkQuery(t *testing.T, topo config.QueryTopology, rows []assignedHoldRow) (string, string) {
	t.Helper()
	a := config.Agent{
		Name:              "worker",
		Dir:               "example",
		BindingName:       "pool",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
	}
	if a.QualifiedName() != assignedHoldAlias {
		t.Fatalf("QualifiedName = %q, want %q", a.QualifiedName(), assignedHoldAlias)
	}
	tmp := t.TempDir()
	for _, bin := range []string{"bd", "gc"} {
		if err := os.WriteFile(filepath.Join(tmp, bin), []byte(assignedHoldFakeReader), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", bin, err)
		}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	rowsPath := filepath.Join(tmp, "rows.json")
	if err := os.WriteFile(rowsPath, data, 0o644); err != nil {
		t.Fatalf("write rows: %v", err)
	}
	logPath := filepath.Join(tmp, "reads.log")
	out, err := shellWorkQueryWithEnv(a.EffectiveWorkQueryFor(topo), t.TempDir(), []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_ROWS=" + rowsPath,
		"FAKE_READ_LOG=" + logPath,
		"GC_SESSION_ID=" + assignedHoldSession,
		"GC_SESSION_NAME=" + assignedHoldSessionName,
		"GC_ALIAS=" + assignedHoldAlias,
		"GC_SESSION_ORIGIN=ephemeral",
	})
	if err != nil {
		t.Fatalf("work query failed: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read reader log: %v", err)
	}
	return out, string(log)
}

// assignedHoldClaim feeds workQueryOutput to the real claim path and returns
// the result plus every bead id the stub claim mutation was asked to claim.
func assignedHoldClaim(t *testing.T, workQueryOutput string) (hookClaimJSONResult, []string) {
	t.Helper()
	var claimed []string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return workQueryOutput, nil },
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			claimed = append(claimed, id)
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           assignedHoldSession,
		IdentityCandidates: hookClaimIdentityCandidates(assignedHoldSession, assignedHoldSessionName, assignedHoldAlias),
		RouteTargets:       hookClaimRouteTargets(assignedHoldAlias),
		JSON:               true,
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("work-query", t.TempDir(), opts, ops, &stdout, &stderr)
	var res hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("stdout not JSON: %v %q stderr=%q", err, stdout.String(), stderr.String())
	}
	return res, claimed
}

func assertAssignedHoldRoutedStepClaimed(t *testing.T, out string, res hookClaimJSONResult, claimed []string) {
	t.Helper()
	if res.Action != "work" || res.BeadID != assignedHoldStepID {
		t.Fatalf("held assignment starved the routed step: action=%s reason=%s bead=%q; work query output %q",
			res.Action, res.Reason, res.BeadID, out)
	}
	if len(claimed) != 1 || claimed[0] != assignedHoldStepID {
		t.Fatalf("claim mutations = %v, want only %s: the held bead must keep its status, assignee and labels", claimed, assignedHoldStepID)
	}
}

// TestAssignedHoldHeldOpenAliasAssignmentStillServesRoutedStep is the #8010
// regression: a held OPEN assignment on any of the seat's identities, on either
// assigned reader, must not short-circuit the query ahead of the routed tier.
func TestAssignedHoldHeldOpenAliasAssignmentStillServesRoutedStep(t *testing.T) {
	readers := []struct {
		name      string
		federated bool
		binary    string
	}{
		{name: "bd ready", federated: false, binary: "bd"},
		{name: "federated gc ready", federated: true, binary: "gc"},
	}
	identities := map[string]string{
		"GC_SESSION_ID":   assignedHoldSession,
		"GC_SESSION_NAME": assignedHoldSessionName,
		"GC_ALIAS":        assignedHoldAlias,
	}
	for _, reader := range readers {
		for identityName, identity := range identities {
			for _, hold := range beadmeta.DispatchHoldLabels {
				t.Run(reader.name+"/"+identityName+"/"+hold, func(t *testing.T) {
					out, log := assignedHoldRunWorkQuery(t, assignedHoldTopology(reader.federated), []assignedHoldRow{
						assignedHoldDecoy("open", identity, hold),
						assignedHoldRoutedStep(),
					})
					assignedRead := reader.binary + " ready --include-ephemeral --assignee=" + identity + " "
					var sawAssignedRead bool
					for _, line := range strings.Split(log, "\n") {
						if !strings.HasPrefix(line, assignedRead) {
							continue
						}
						sawAssignedRead = true
						if !strings.Contains(line, "--exclude-label hold:mayor --exclude-label hold:external") {
							t.Errorf("assigned-ready read %q does not exclude the dispatch holds", line)
						}
						if strings.Contains(line, "--exclude-label human") {
							t.Errorf("assigned-ready read %q excludes human, which is not a dispatch hold", line)
						}
					}
					if !sawAssignedRead {
						t.Fatalf("no %q call in reader log:\n%s", assignedRead, log)
					}
					res, claimed := assignedHoldClaim(t, out)
					assertAssignedHoldRoutedStepClaimed(t, out, res, claimed)
				})
			}
		}
	}
}

// TestAssignedHoldControlsServeRoutedStep holds the fixture honest: with no
// decoy, or with the held decoy already IN_PROGRESS (gated by the in-progress
// tier), the routed step is served.
func TestAssignedHoldControlsServeRoutedStep(t *testing.T) {
	cases := map[string][]assignedHoldRow{
		"no decoy":            {assignedHoldRoutedStep()},
		"held in_progress":    {assignedHoldDecoy("in_progress", assignedHoldAlias, beadmeta.HoldExternalLabel), assignedHoldRoutedStep()},
		"held in_progress/id": {assignedHoldDecoy("in_progress", assignedHoldSession, beadmeta.HoldMayorLabel), assignedHoldRoutedStep()},
	}
	for name, rows := range cases {
		for _, federated := range []bool{false, true} {
			t.Run(name+"/federated="+strconv.FormatBool(federated), func(t *testing.T) {
				out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), rows)
				res, claimed := assignedHoldClaim(t, out)
				assertAssignedHoldRoutedStepClaimed(t, out, res, claimed)
			})
		}
	}
}
