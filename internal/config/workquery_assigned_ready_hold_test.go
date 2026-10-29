package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Assigned-ready hold coverage (#8010), the ready-assignment sibling of the
// crash-recovery pins in workquery_inprogress_hold_test.go.
//
// bd's Ready() ignores hold labels, so the assigned-ready read used to serve a
// held OPEN assignment as its one row and `exit 0` before the routed
// pool-demand tier ran; the hook then stripped the held row and reported
// no_work while routed work sat unqueried below. The reader now excludes
// beadmeta.DispatchHoldLabels before its --limit=1 — the same flags the
// routed tier and the federated crash-recovery read already carry — so a held
// assignment is neither served nor able to mask other work. bd's native human
// label is not a dispatch hold: the assigned tiers stay human-transparent.
//
// These EXECUTE the generated shell against a fake reader whose ready arm
// honors --exclude-label the way real bd does — filtering rows before the
// limit, not after — so they pin observable behavior rather than the script's
// spelling, and a fix that filtered after the read would still fail them.

// assignedReadyHoldTestAgent is the worker whose pool-demand target the routed
// tier probes: hello-world/worker.
func assignedReadyHoldTestAgent() *Agent {
	return &Agent{Name: "worker", Dir: "hello-world"}
}

// assignedReadyHoldRoutedRow is the OPEN, unassigned step routed to the test
// agent's identity: the row the routed pool-demand tier serves.
func assignedReadyHoldRoutedRow() string {
	return `[{"id":"wk-routed","status":"open","issue_type":"task","assignee":"","metadata":{"gc.routed_to":"hello-world/worker"}}]`
}

// assignedReadyHoldAssignedRow is an OPEN task assigned to the given identity,
// optionally parked on holdLabel and/or flagged ephemeral.
func assignedReadyHoldAssignedRow(id, assignee, holdLabel string, ephemeral bool) string {
	return `[` + assignedReadyHoldAssignedRowObj(id, assignee, holdLabel, ephemeral) + `]`
}

// assignedReadyHoldAssignedRowObj is assignedReadyHoldAssignedRow's single row
// object, without the array brackets.
func assignedReadyHoldAssignedRowObj(id, assignee, holdLabel string, ephemeral bool) string {
	row := `{"id":"` + id + `","status":"open","issue_type":"task","assignee":"` + assignee + `"`
	if holdLabel != "" {
		row += `,"labels":["` + holdLabel + `"]`
	}
	if ephemeral {
		row += `,"ephemeral":true`
	}
	return row + `}`
}

// assignedReadyHoldFakeCLI renders the fake reader binary: `bd` on the
// single-store topologies, `gc` on the federated one. The assigned-ready arm
// answers a call carrying --exclude-label with filteredRows and a call without
// it with unfilteredRows, which is what proves the exclusion rides the reader:
// only a query that sends the flags sees the held rows dropped before the
// limit. Every invocation is logged to $ASSIGNED_READY_HOLD_LOG.
func assignedReadyHoldFakeCLI(tool, assignee, filteredRows, unfilteredRows, routedRow string) string {
	return `#!/bin/sh
echo "` + tool + ` $*" >> "$ASSIGNED_READY_HOLD_LOG"
args="$*"
rows=""
case "$1" in
  ready)
    case "$args" in
      *"--status in_progress"*) rows='[]' ;;
      *"--assignee=` + assignee + `"*)
        case "$args" in
          *"--exclude-label"*) rows='` + filteredRows + `' ;;
          *) rows='` + unfilteredRows + `' ;;
        esac ;;
      *"--metadata-field gc.routed_to=hello-world/worker"*) rows='` + routedRow + `' ;;
      *) rows='[]' ;;
    esac ;;
  list) rows='[]' ;;
  show) printf '%s' '[{"id":"wk-held","dependencies":[]}]'; exit 0 ;;
  *) printf '[]'; exit 0 ;;
esac
printf '%s' "$rows"
`
}

// assignedReadyHoldRunQuery runs the test agent's real generated work query for
// topo against the fake reader pair, with identityEnv naming the one identity
// variable the held row is assigned through. It returns the query output and
// the reader call log.
func assignedReadyHoldRunQuery(t *testing.T, topo QueryTopology, identityEnv map[string]string, assignee, filteredRows, unfilteredRows string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	logPath := filepath.Join(t.TempDir(), "reader.log")
	env := map[string]string{"ASSIGNED_READY_HOLD_LOG": logPath}
	for k, v := range identityEnv {
		env[k] = v
	}
	bdScript := assignedReadyHoldFakeCLI("bd", assignee, filteredRows, unfilteredRows, assignedReadyHoldRoutedRow())
	gcScript := fakeGCReadyFails
	if topo.FederatedReady {
		gcScript = assignedReadyHoldFakeCLI("gc", assignee, filteredRows, unfilteredRows, assignedReadyHoldRoutedRow())
	}
	res := runGeneratedQueryWithBD(t, assignedReadyHoldTestAgent().EffectiveWorkQueryFor(topo), env, gcScript, bdScript)
	if res.exit != 0 {
		t.Fatalf("work query exited %d: stderr=%s stdout=%s", res.exit, res.stderr, res.stdout)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read reader log: %v", err)
	}
	return strings.TrimSpace(res.stdout), string(log)
}

// assignedReadyHoldIdentityEnvs returns the identity variable sets under which
// the held row is assigned to the session id, the session name, and the alias
// in turn: the identity loop must exclude the held row on every one of them.
func assignedReadyHoldIdentityEnvs() map[string]map[string]string {
	return map[string]map[string]string{
		"session-id":   {"GC_SESSION_ID": "sess-1"},
		"session-name": {"GC_SESSION_NAME": "hello-world__worker-sess-1"},
		"alias":        {"GC_ALIAS": "hello-world/worker"},
	}
}

// assignedReadyHoldTopologies covers every reader the generated work query can
// run on: bd-1.0.4, bd-1.0.5, and the bd-1.0.5 federated `gc ready` reader.
func assignedReadyHoldTopologies() map[string]QueryTopology {
	return map[string]QueryTopology{
		"bd104":     {},
		"bd105":     singleStoreTopology(),
		"federated": federatedTopology(),
	}
}

// TestAssignedReadyHeldOnlyFallsThroughToRoutedTier is the #8010 starvation at
// the query layer: a held OPEN assignment to any identity yields an empty
// assigned-ready tier on every reader, and the work query falls through to the
// routed step instead of exiting on the held row.
func TestAssignedReadyHeldOnlyFallsThroughToRoutedTier(t *testing.T) {
	for topoName, topo := range assignedReadyHoldTopologies() {
		for identName, identityEnv := range assignedReadyHoldIdentityEnvs() {
			assignee := firstIdentityVar(identityEnv)
			for _, label := range beadmeta.DispatchHoldLabels {
				t.Run(topoName+"/"+identName+"/"+label, func(t *testing.T) {
					held := assignedReadyHoldAssignedRow("wk-held", assignee, label, false)
					out, log := assignedReadyHoldRunQuery(t, topo, identityEnv, assignee, `[]`, held)
					if out != assignedReadyHoldRoutedRow() {
						t.Fatalf("held assigned row did not fall through to the routed tier; got %q (reader calls:\n%s)", out, log)
					}
				})
			}
		}
	}
}

// TestAssignedReadyHeldEphemeralAssignmentFallsThroughToo pins that on
// bd-1.0.5 — where the assigned-ready read carries --include-ephemeral — a
// held OPEN ephemeral assignment rides the same exclusion: the reader flags
// exclude it exactly like a durable one.
func TestAssignedReadyHeldEphemeralAssignmentFallsThroughToo(t *testing.T) {
	for identName, identityEnv := range assignedReadyHoldIdentityEnvs() {
		assignee := firstIdentityVar(identityEnv)
		for _, label := range beadmeta.DispatchHoldLabels {
			t.Run(identName+"/"+label, func(t *testing.T) {
				held := assignedReadyHoldAssignedRow("wk-held-eph", assignee, label, true)
				out, log := assignedReadyHoldRunQuery(t, singleStoreTopology(), identityEnv, assignee, `[]`, held)
				if out != assignedReadyHoldRoutedRow() {
					t.Fatalf("held ephemeral assigned row did not fall through to the routed tier; got %q (reader calls:\n%s)", out, log)
				}
			})
		}
	}
}

// firstIdentityVar returns the value of the single identity variable an
// assignedReadyHoldIdentityEnvs entry sets.
func firstIdentityVar(env map[string]string) string {
	for _, v := range env {
		return v
	}
	return ""
}

// TestAssignedReadyScriptsExcludeDispatchHolds pins the generator directly, for
// both identity loops (the standard $id loop and the legacy control-dispatcher
// $cand loop) and every topology: the assigned-ready read carries both
// canonical dispatch holds and never the human label.
func TestAssignedReadyScriptsExcludeDispatchHolds(t *testing.T) {
	scripts := map[string]func(QueryTopology) string{
		"standardAssignedReadyWorkQueryScript":      standardAssignedReadyWorkQueryScript,
		"legacyControlAssignedReadyWorkQueryScript": legacyControlAssignedReadyWorkQueryScript,
	}
	for scriptName, script := range scripts {
		for topoName, topo := range assignedReadyHoldTopologies() {
			got := script(topo)
			for _, label := range beadmeta.DispatchHoldLabels {
				if want := `--exclude-label "` + label + `"`; !strings.Contains(got, want) {
					t.Errorf("%s(%s) = %q, missing %q", scriptName, topoName, got, want)
				}
			}
			if strings.Contains(got, `--exclude-label "human"`) {
				t.Errorf("%s(%s) = %q, must stay human-transparent", scriptName, topoName, got)
			}
		}
	}
}
