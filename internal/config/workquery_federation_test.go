package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file executes the generated commands rather than reading them.
//
// The property under test is the one the whole federation exists for and the
// one a string assertion cannot reach: a leg the reader could not open or read
// must NOT arrive at the consumer as "no work". `gc ready` answers a dead leg
// with a non-zero exit naming the rig, because a short array is
// indistinguishable from an empty queue (cmd/gc/ready_federation.go). If the
// generated shell captured that exit into an empty $r and fell through to the
// next tier — which is exactly what the single-store tiers do, on purpose — the
// query would print "[]" and exit 0, and the whole slice would buy nothing.
//
// So each case runs the real generated script against a fake `gc` that fails the
// way a dead rig makes it fail, and asserts the script's own exit status and
// stdout.

// deadLegStderr is what `gc ready` writes when a rig store cannot be opened —
// the shape readyRigLegStores produces. The tests assert it reaches the caller's
// stderr, because the exit code alone does not name the rig.
const deadLegStderr = `gc ready: rig "rig-A" store: stat .gc/beads.json: not a directory`

// fakeGCReadyFails is a `gc` stand-in whose `ready` subcommand fails the way a
// dead leg makes the real one fail: nothing on stdout, the rig named on stderr,
// non-zero exit.
const fakeGCReadyFails = `#!/bin/sh
case "$1" in
  ready) printf '%s\n' '` + deadLegStderr + `' >&2; exit 1 ;;
  *) printf '[]' ;;
esac
`

// fakeGCReadyServes answers every `gc ready` with one routed bead, so a case can
// tell "the tier fired" apart from "the tier was skipped".
const fakeGCReadyServes = `#!/bin/sh
case "$1" in
  ready) printf '[{"id":"gcg-1","status":"open","issue_type":"task"}]' ;;
  *) printf '[]' ;;
esac
`

// federatedTopology is a split city on bd-1.0.5 semantics.
func federatedTopology() QueryTopology {
	return QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}, FederatedReady: true}
}

func singleStoreTopology() QueryTopology {
	return QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}
}

// TestFederatedWorkQueryPropagatesADeadLeg is the load-bearing test of this
// slice: the routed tier's reader fails, and the work query must fail with it.
func TestFederatedWorkQueryPropagatesADeadLeg(t *testing.T) {
	a := &Agent{Name: "worker"}
	res := runGeneratedQuery(t, a.EffectiveWorkQueryFor(federatedTopology()), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, fakeGCReadyFails)

	if res.exit == 0 {
		t.Fatalf("the federated work_query exited 0 over a dead leg and printed %q; a work query that answers a failed federated read with an empty array has re-created the exact fail-open `gc ready` exits non-zero to close", res.stdout)
	}
	if strings.Contains(res.stdout, "[]") {
		t.Errorf("the federated work_query printed %q over a dead leg; `[]` is what the consumer reads as \"no work\"", res.stdout)
	}
	if !strings.Contains(res.stderr, "rig-A") {
		t.Errorf("work_query stderr = %q, want the dead rig named — an exit code the operator cannot attribute to a store is not a diagnosis", res.stderr)
	}
}

// TestFederatedAssignedReadyTierPropagatesADeadLeg covers the OTHER federated
// tier of the same script. It is a separate case because the tiers fail through
// different clauses, and the assigned tier runs first: a swallow there is
// invisible in the routed tier's result.
func TestFederatedAssignedReadyTierPropagatesADeadLeg(t *testing.T) {
	a := &Agent{Name: "worker"}
	res := runGeneratedQuery(t, a.EffectiveAssignedReadyQueryFor(federatedTopology()), map[string]string{
		"GC_SESSION_ID": "worker-sess",
	}, fakeGCReadyFails)

	if res.exit == 0 {
		t.Fatalf("the federated assigned_ready_query exited 0 over a dead leg and printed %q; assigned graph work would read as \"nothing assigned to me\"", res.stdout)
	}
	if !strings.Contains(res.stderr, "rig-A") {
		t.Errorf("assigned_ready_query stderr = %q, want the dead rig named", res.stderr)
	}
}

// TestFederatedPoolDemandPropagatesADeadLeg pins the reconciler half. This one
// already had the discipline (the count-form's `|| exit $?` chain predates the
// federation); the case exists so the swap cannot quietly remove it, because a
// zero count is what stops a pool from spawning.
func TestFederatedPoolDemandPropagatesADeadLeg(t *testing.T) {
	a := &Agent{Name: "worker"}
	res := runGeneratedQuery(t, a.EffectivePoolDemandQueryFor(federatedTopology()), nil, fakeGCReadyFails)

	if res.exit == 0 {
		t.Fatalf("the federated scale_check exited 0 over a dead leg and printed %q; a count of 0 is what drains a pool", res.stdout)
	}
	if strings.TrimSpace(res.stdout) == "0" {
		t.Errorf("the federated scale_check printed a count of 0 over a dead leg; \"no demand\" and \"could not read the demand\" must not be the same answer")
	}
}

// fakeBDFails is a `bd` stand-in that fails every subcommand the way an
// unreadable store makes the real one fail. The single-store work query still
// shells `bd` for its crash-recovery tier (`bd list --status in_progress`)
// even after ga-g4odhq moved every ready read onto `gc ready`, so this is what
// exercises THAT tier's fall-through.
//
// It appends each invocation to $FAKE_BD_LOG because the single-store tiers
// redirect bd's stderr to /dev/null: without the log there is no evidence the
// failure injection was reached at all, which is how the case used to pass
// vacuously.
const fakeBDFails = `#!/bin/sh
[ -n "$FAKE_BD_LOG" ] && printf '%s\n' "$*" >> "$FAKE_BD_LOG"
printf 'bd: store unreadable\n' >&2
exit 1
`

// fakeGCFails is a `gc` stand-in that fails every subcommand the way an
// unreadable store makes the real `gc ready` fail. ga-g4odhq routes every
// topology's ready read through `gc ready` — so the Go-side gc.work_outcome
// veto (ga-beg8uc) always runs — which means a single-store city now shells
// `gc` too, and this is what exercises ITS tiers' fall-through. It logs to
// $FAKE_GC_LOG for the same reason fakeBDFails logs to $FAKE_BD_LOG.
const fakeGCFails = `#!/bin/sh
[ -n "$FAKE_GC_LOG" ] && printf '%s\n' "$*" >> "$FAKE_GC_LOG"
printf 'gc: store unreadable\n' >&2
exit 1
`

// TestSingleStoreWorkQueryKeepsItsFallThrough is the other half of the byte
// identity claim, stated as behavior rather than as bytes: a non-relocated
// city still swallows a failing read and falls through to the next tier. This
// is not an oversight being pinned — the tiers have somewhere to fall through
// TO, and changing it would alter what every deployed city does on a flaky
// store.
//
// Since ga-g4odhq, a single-store work query shells BOTH binaries: `bd` for
// the crash-recovery tier (`bd list --status in_progress`, untouched by the
// fix) and `gc` for every ready read (`gc ready`, so the Go-side
// gc.work_outcome veto runs on every topology, not only a federated one). This
// case fails both and proves neither failure propagates — unlike the
// federated cases above, where readyReaderFailurePropagation makes a failing
// `gc ready` fail the whole query on purpose. GC_SESSION_ID is set so the
// crash-recovery and assigned-ready tiers' identity loop actually runs instead
// of skipping to the routed-pool tier alone.
func TestSingleStoreWorkQueryKeepsItsFallThrough(t *testing.T) {
	a := &Agent{Name: "worker"}
	command := a.EffectiveWorkQueryFor(singleStoreTopology())
	if !strings.Contains(command, "gc ready") {
		t.Fatalf("the single-store work_query no longer shells `gc ready`; ga-g4odhq requires every topology's ready read to route through the Go-side gc.work_outcome veto: %q", command)
	}
	bdLog := filepath.Join(t.TempDir(), "bd-invocations")
	gcLog := filepath.Join(t.TempDir(), "gc-invocations")
	res := runGeneratedQueryWithBD(t, command, map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_SESSION_ID":     "worker-sess",
		"FAKE_BD_LOG":       bdLog,
		"FAKE_GC_LOG":       gcLog,
	}, fakeGCFails, fakeBDFails)
	if res.exit != 0 {
		t.Fatalf("the single-store work_query exited %d over a failing bd and gc; a non-relocated city must behave exactly as it does today (stderr=%q)", res.exit, res.stderr)
	}
	if strings.TrimSpace(res.stdout) != "[]" {
		t.Errorf("single-store work_query stdout = %q, want %q", res.stdout, "[]")
	}
	bdInvocations, err := os.ReadFile(bdLog)
	if err != nil || !strings.Contains(string(bdInvocations), "in_progress") {
		t.Fatalf("the failing `bd` was never asked for the crash-recovery `list --status in_progress` (log=%q, err=%v); a case whose failure injection is not invoked proves nothing about the fall-through", bdInvocations, err)
	}
	gcInvocations, err := os.ReadFile(gcLog)
	if err != nil || !strings.Contains(string(gcInvocations), "ready") {
		t.Fatalf("the failing `gc` was never asked for `ready` (log=%q, err=%v); a case whose failure injection is not invoked proves nothing about the fall-through", gcInvocations, err)
	}
}

// TestFederatedWorkQueryServesTheRoutedTier proves the failure cases above are
// not passing because the script exits before it reaches a reader.
func TestFederatedWorkQueryServesTheRoutedTier(t *testing.T) {
	a := &Agent{Name: "worker"}
	res := runGeneratedQuery(t, a.EffectiveWorkQueryFor(federatedTopology()), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, fakeGCReadyServes)
	if res.exit != 0 {
		t.Fatalf("the federated work_query exited %d over a healthy reader (stderr=%q)", res.exit, res.stderr)
	}
	if !strings.Contains(res.stdout, "gcg-1") {
		t.Fatalf("the federated work_query printed %q, want the routed graph bead the federated reader served", res.stdout)
	}
}

// generatedQueryResult is one execution of a generated command.
type generatedQueryResult struct {
	stdout string
	stderr string
	exit   int
}

// fakeBDEmpty answers every `bd` subcommand with an empty array, so only the
// `gc ready` tiers can decide a federated query's outcome.
const fakeBDEmpty = "#!/bin/sh\nprintf '[]'\n"

// runGeneratedQuery runs a generated command with a fake `gc` (and a `bd` that
// answers everything with an empty array, so only the `gc ready` tiers can
// decide the outcome) first on PATH, and reports its streams and exit status.
func runGeneratedQuery(t *testing.T, command string, env map[string]string, gcScript string) generatedQueryResult {
	t.Helper()
	return runGeneratedQueryWithBD(t, command, env, gcScript, fakeBDEmpty)
}

// runGeneratedQueryWithBD is runGeneratedQuery with the `bd` stand-in chosen by
// the caller, for the single-store cases whose tiers shell `bd` rather than `gc`
// and would otherwise never reach the injected failure.
func runGeneratedQueryWithBD(t *testing.T, command string, env map[string]string, gcScript, bdScript string) generatedQueryResult {
	t.Helper()
	tmp := t.TempDir()
	writeFakeBin(t, filepath.Join(tmp, "gc"), gcScript)
	writeFakeBin(t, filepath.Join(tmp, "bd"), bdScript)

	commandEnv := []string{"PATH=" + tmp + ":" + os.Getenv("PATH")}
	for k, v := range env {
		commandEnv = append(commandEnv, k+"="+v)
	}
	stdout, stderr, exit := runShellCommandCapture(t, command, commandEnv)
	return generatedQueryResult{stdout: stdout, stderr: stderr, exit: exit}
}

func writeFakeBin(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", filepath.Base(path), err)
	}
}
