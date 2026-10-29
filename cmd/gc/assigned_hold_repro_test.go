package main

// Assigned-ready hold starvation regression (#8010).
//
// A pool seat whose identity has an OPEN bead assigned to it and parked on a
// canonical dispatch hold, while an unassigned step routed to the same
// identity is ready. bd's Ready() ignores hold labels, so the assigned-ready
// read served that held row and `exit 0`-ed before the routed pool-demand
// tier ran; the hook then stripped the held row (isHeldHookCandidate) and
// reported drain/no_work, so the routed step was never queried. The
// assigned-ready reader now excludes beadmeta.DispatchHoldLabels before its
// --limit=1, so a held assignment is neither served nor able to mask the
// routed tier.
//
// These tests run the REAL generated work query against a fake reader whose
// ready arm honors --exclude-label the way real bd does — filtering rows
// before the limit, not after — and then feed the output to the real claim
// path through doHookClaim.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	assignedHoldAlias     = "example/pool.worker"
	assignedHoldSession   = "session-example"
	assignedHoldSessionQN = "pool__worker-" + assignedHoldSession
	assignedHoldStepID    = "test-routed"
	assignedHoldDecoyID   = "test-held"
)

// assignedHoldStepRow is the OPEN, unassigned step routed to the pool alias.
func assignedHoldStepRow() string {
	return `[{"id":"` + assignedHoldStepID + `","status":"open","issue_type":"task","assignee":"","metadata":{"gc.routed_to":"` + assignedHoldAlias + `"}}]`
}

// assignedHoldDecoyRow is a bead assigned to assignee and parked on holdLabel:
// the row bd's hold-blind Ready() would otherwise hand the assigned-ready tier.
func assignedHoldDecoyRow(assignee, holdLabel string, inProgress bool) string {
	status := "open"
	if inProgress {
		status = "in_progress"
	}
	return `[{"id":"` + assignedHoldDecoyID + `","status":"` + status + `","issue_type":"task","assignee":"` + assignee + `","labels":["` + holdLabel + `"]}]`
}

// assignedHoldFakeCLI renders the fake reader binary: `bd` for the single-store
// topology, `gc` for the federated one. Every ready/list answer is filtered by
// the --exclude-label flags the caller passed, dropping rows whose labels
// carry an excluded value before the answer is bounded — the way bd filters in
// SQL before LIMIT — so a correct reader-side fix is not masked by a stub that
// ignores the flags. Each invocation appends `tool $*` to $REPRO_BD_LOG, which
// is what the vacuity guard reads.
func assignedHoldFakeCLI(tool, readyRow, inProgressRow, decoyAssignee string) string {
	step := assignedHoldStepRow()
	return `#!/bin/sh
echo "` + tool + ` $*" >> "$REPRO_BD_LOG"
args="$*"
rows=""
case "$1" in
  ready)
    case "$args" in
      *"--status in_progress"*) rows='` + inProgressRow + `' ;;
      *"--assignee=` + decoyAssignee + `"*) rows='` + readyRow + `' ;;
      *"--metadata-field gc.routed_to=` + assignedHoldAlias + `"*) rows='` + step + `' ;;
      *) rows='[]' ;;
    esac ;;
  list)
    case "$args" in
      *"--assignee=` + decoyAssignee + `"*) rows='` + inProgressRow + `' ;;
      *) rows='[]' ;;
    esac ;;
  show) printf '%s' '[{"id":"` + assignedHoldDecoyID + `","dependencies":[]}]'; exit 0 ;;
  *) printf '[]'; exit 0 ;;
esac
filt=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--exclude-label" ]; then
    filt="$filt | select(((.labels // []) | index(\"$a\")) == null)"
  fi
  prev="$a"
done
if [ -n "$filt" ]; then
  rows=$(printf '%s' "$rows" | jq "[.[]$filt]" 2>/dev/null)
fi
printf '%s' "$rows"
`
}

// assignedHoldRunWorkQuery runs the real generated work query for the pool seat
// against the fake reader pair and returns the query output and the reader
// call log.
func assignedHoldRunWorkQuery(t *testing.T, readyRow, inProgressRow, decoyAssignee string, federated bool) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq required")
	}
	a := config.Agent{
		Name:              "worker",
		Dir:               "example",
		BindingName:       "pool",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
	}
	if a.QualifiedName() != assignedHoldAlias {
		t.Fatalf("QualifiedName = %q", a.QualifiedName())
	}
	topo := config.QueryTopology{Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}}
	if federated {
		topo.FederatedReady = true
	}
	query := a.EffectiveWorkQueryFor(topo)

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "bd"), []byte(assignedHoldFakeCLI("bd", readyRow, inProgressRow, decoyAssignee)), 0o755); err != nil {
		t.Fatal(err)
	}
	if federated {
		if err := os.WriteFile(filepath.Join(tmp, "gc"), []byte(assignedHoldFakeCLI("gc", readyRow, inProgressRow, decoyAssignee)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(tmp, "bd.log")
	cmd := exec.Command("sh", "-c", query)
	cmd.Env = []string{
		"PATH=" + tmp + ":" + os.Getenv("PATH"),
		"REPRO_BD_LOG=" + logPath,
		"GC_SESSION_ID=" + assignedHoldSession,
		"GC_SESSION_NAME=" + assignedHoldSessionQN,
		"GC_ALIAS=" + assignedHoldAlias,
		"GC_SESSION_ORIGIN=ephemeral",
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("work query failed: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	return string(out), string(log)
}

// assignedHoldClaim feeds workQueryOutput into the real claim path.
func assignedHoldClaim(t *testing.T, workQueryOutput string) (hookClaimJSONResult, string) {
	t.Helper()
	claimed := ""
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return workQueryOutput, nil },
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			claimed = id
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           assignedHoldSession,
		IdentityCandidates: hookClaimIdentityCandidates(assignedHoldSession, assignedHoldSessionQN, assignedHoldAlias),
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

// assignedHoldAssertAssignedReadExcludesHolds is the vacuity guard: the test
// must fail, not pass vacuously, when the assigned-ready read never ran or ran
// without the dispatch-hold exclusion. Every assigned-ready read in the log
// (the `ready` calls carrying an --assignee filter but not the in_progress
// status filter) must exclude both canonical dispatch holds, and must never
// exclude bd's native human label: the assigned tiers stay human-transparent
// (hold:mayor/hold:external park a bead for dispatch, human parks it for a
// person, and assigned work is owned work).
func assignedHoldAssertAssignedReadExcludesHolds(t *testing.T, log string) {
	t.Helper()
	var assigned []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, " ready ") && strings.Contains(line, "--assignee=") && !strings.Contains(line, "--status in_progress") {
			assigned = append(assigned, line)
		}
	}
	if len(assigned) == 0 {
		t.Fatalf("assigned-ready read never ran; reader calls:\n%s", log)
	}
	for _, line := range assigned {
		if !strings.Contains(line, "--exclude-label hold:mayor") || !strings.Contains(line, "--exclude-label hold:external") {
			t.Errorf("assigned-ready read does not exclude dispatch holds: %q", line)
		}
		if strings.Contains(line, "--exclude-label human") {
			t.Errorf("assigned-ready read must stay human-transparent: %q", line)
		}
	}
}

// TestAssignedHoldHeldOpenAliasAssignmentStillServesRoutedStep is the #8010
// regression: a held OPEN assignment to any of the seat's identities must not
// starve the routed step, on either reader.
func TestAssignedHoldHeldOpenAliasAssignmentStillServesRoutedStep(t *testing.T) {
	identities := map[string]string{
		"session-id":   assignedHoldSession,
		"session-name": assignedHoldSessionQN,
		"alias":        assignedHoldAlias,
	}
	for _, federated := range []bool{false, true} {
		for identName, decoyAssignee := range identities {
			for _, holdLabel := range []string{"hold:mayor", "hold:external"} {
				t.Run(identName+"/"+holdLabel, func(t *testing.T) {
					readyRow := assignedHoldDecoyRow(decoyAssignee, holdLabel, false)
					out, log := assignedHoldRunWorkQuery(t, readyRow, `[]`, decoyAssignee, federated)
					assignedHoldAssertAssignedReadExcludesHolds(t, log)
					res, claimed := assignedHoldClaim(t, out)
					if res.Action != "work" || res.BeadID != assignedHoldStepID || claimed != assignedHoldStepID {
						t.Fatalf("held open assignment to %s (%s) starved the routed step: action=%s reason=%s bead=%q claimed=%q (work query output %s)",
							identName, holdLabel, res.Action, res.Reason, res.BeadID, claimed, out)
					}
				})
			}
		}
	}
}

// Control: with no held assignment the routed step is served. Green before
// and after the fix.
func TestAssignedHoldControlNoDecoyServesRoutedStep(t *testing.T) {
	out, _ := assignedHoldRunWorkQuery(t, `[]`, `[]`, assignedHoldAlias, false)
	res, claimed := assignedHoldClaim(t, out)
	if res.Action != "work" || res.BeadID != assignedHoldStepID || claimed != assignedHoldStepID {
		t.Fatalf("control without decoy did not serve the routed step: action=%s reason=%s bead=%q claimed=%q (work query output %s)",
			res.Action, res.Reason, res.BeadID, claimed, out)
	}
}

// Control: a held IN_PROGRESS assignment is already excluded by the
// crash-recovery tier's serve gate, so it never reaches the assigned-ready
// read. Green before and after the fix.
func TestAssignedHoldControlHeldInProgressDecoyServesRoutedStep(t *testing.T) {
	inProgressRow := assignedHoldDecoyRow(assignedHoldAlias, "hold:external", true)
	out, _ := assignedHoldRunWorkQuery(t, `[]`, inProgressRow, assignedHoldAlias, false)
	res, claimed := assignedHoldClaim(t, out)
	if res.Action != "work" || res.BeadID != assignedHoldStepID || claimed != assignedHoldStepID {
		t.Fatalf("control with held in_progress decoy did not serve the routed step: action=%s reason=%s bead=%q claimed=%q (work query output %s)",
			res.Action, res.Reason, res.BeadID, claimed, out)
	}
}
