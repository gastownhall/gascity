package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file executes the generated single-store queries against a broken `bd`
// and pins the one property the rest of the system depends on: an empty queue
// and a failed query are different answers.
//
// The failure that motivated it was live. A worker's tool shell was a login
// shell whose /etc/profile dropped the directory holding `bd`. Every tier's
// read failed with "command not found", every failure was swallowed into an
// empty $r, and the script fell through to its terminal `printf "[]"` with exit
// 0 and nothing on stderr. `gc hook` read that as "no work", `gc hook --claim`
// answered {"action":"drain","reason":"no_work"}, and the pool spawned and
// drained a fresh session every few minutes while the routed bead was never
// claimed. A tool failure was indistinguishable from an idle queue.

// singleStoreTopologies are the two single-store shapes every deployment runs:
// the bd-1.0.4 one, whose ephemeral tier is a `bd query` scan, and the
// bd-1.0.5 one, whose ready reads include the ephemeral tier natively.
func singleStoreTopologies() map[string]QueryTopology {
	return map[string]QueryTopology{
		"bd104": {},
		"bd105": singleStoreTopology(),
	}
}

// workQueryKinds are the generated read-side queries a worker or its prompt
// runs. Each is a standalone script with its own failure handling, so each is
// exercised on its own.
func workQueryKinds(a *Agent, topo QueryTopology) map[string]string {
	return map[string]string{
		"work_query":                 a.EffectiveWorkQueryFor(topo),
		"assigned_in_progress_query": a.EffectiveAssignedInProgressQueryFor(topo),
		"assigned_ready_query":       a.EffectiveAssignedReadyQueryFor(topo),
		"routed_pool_query":          a.EffectiveRoutedPoolQueryFor(topo),
	}
}

// workerSessionEnv is a pool worker's identity: every assigned tier has an
// identity to read, and the ephemeral origin admits it to the routed tier.
func workerSessionEnv() map[string]string {
	return map[string]string{
		"GC_SESSION_ID":     "gc-sess-1",
		"GC_SESSION_NAME":   "worker-gc-sess-1",
		"GC_ALIAS":          "worker-1",
		"GC_SESSION_ORIGIN": "ephemeral",
	}
}

// pathWithoutBD returns a PATH that resolves sh, jq and the coreutils the
// generated query needs but no `bd`: the current PATH minus every directory
// holding a `bd`, plus a directory holding only a link to jq, since jq and bd
// commonly share an install directory.
func pathWithoutBD(t *testing.T) string {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is required to run the generated work query")
	}
	toolDir := t.TempDir()
	if err := os.Symlink(jq, filepath.Join(toolDir, "jq")); err != nil {
		t.Fatalf("link jq: %v", err)
	}
	dirs := []string{toolDir}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "bd")); err == nil {
			continue
		}
		dirs = append(dirs, dir)
	}
	path := strings.Join(dirs, string(os.PathListSeparator))
	if found, err := lookPathIn("bd", path); err == nil {
		t.Fatalf("pathWithoutBD still resolves bd at %s", found)
	}
	return path
}

func lookPathIn(file, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, file)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

// runGeneratedQueryWithoutBD runs command with no `bd` anywhere on PATH.
func runGeneratedQueryWithoutBD(t *testing.T, command string, env map[string]string) generatedQueryResult {
	t.Helper()
	commandEnv := []string{"PATH=" + pathWithoutBD(t)}
	for k, v := range env {
		commandEnv = append(commandEnv, k+"="+v)
	}
	stdout, stderr, exit := runShellCommandCapture(t, command, commandEnv)
	return generatedQueryResult{stdout: stdout, stderr: stderr, exit: exit}
}

// assertQueryFailedLoudly is the shared verdict: a non-zero exit, no "[]" a
// consumer could read as an empty queue, and a stderr line naming the failure.
func assertQueryFailedLoudly(t *testing.T, res generatedQueryResult, wantStderr string) {
	t.Helper()
	if res.exit == 0 {
		t.Fatalf("query exited 0 over a failed bd read and printed %q (stderr=%q); a failed read must not be answered as \"no work\"", res.stdout, res.stderr)
	}
	if strings.Contains(res.stdout, "[]") {
		t.Errorf("query printed %q over a failed bd read; \"[]\" is what every consumer reads as an empty queue", res.stdout)
	}
	if !strings.Contains(res.stderr, wantStderr) {
		t.Errorf("query stderr = %q, want it to name the failure (%q)", res.stderr, wantStderr)
	}
}

// TestSingleStoreWorkQueriesFailWhenBDIsMissing is the live failure: `bd` is
// not on the tool shell's PATH at all.
func TestSingleStoreWorkQueriesFailWhenBDIsMissing(t *testing.T) {
	a := &Agent{Name: "worker"}
	for topoName, topo := range singleStoreTopologies() {
		for kind, command := range workQueryKinds(a, topo) {
			t.Run(topoName+"/"+kind, func(t *testing.T) {
				res := runGeneratedQueryWithoutBD(t, command, workerSessionEnv())
				assertQueryFailedLoudly(t, res, "bd not found in PATH")
			})
		}
	}
}

// TestSingleStoreWorkQueriesFailWhenBDExitsNonZero covers a `bd` that is
// present but cannot answer: an unreadable store, a refused connection. The
// query's stderr must carry bd's own diagnostic as well as which read failed.
func TestSingleStoreWorkQueriesFailWhenBDExitsNonZero(t *testing.T) {
	a := &Agent{Name: "worker"}
	for topoName, topo := range singleStoreTopologies() {
		for kind, command := range workQueryKinds(a, topo) {
			t.Run(topoName+"/"+kind, func(t *testing.T) {
				res := runGeneratedQueryWithBD(t, command, workerSessionEnv(), fakeGCReadyFails, fakeBDFails)
				assertQueryFailedLoudly(t, res, "gc work query: bd ")
				if !strings.Contains(res.stderr, "bd: store unreadable") {
					t.Errorf("query stderr = %q, want bd's own diagnostic passed through", res.stderr)
				}
			})
		}
	}
}

// TestSingleStoreWorkQueriesStillAnswerAnEmptyQueue keeps the other half of the
// contract exactly as it was: a healthy `bd` with nothing to serve is "[]" and
// exit 0, with nothing on stderr.
func TestSingleStoreWorkQueriesStillAnswerAnEmptyQueue(t *testing.T) {
	a := &Agent{Name: "worker"}
	for topoName, topo := range singleStoreTopologies() {
		for kind, command := range workQueryKinds(a, topo) {
			t.Run(topoName+"/"+kind, func(t *testing.T) {
				res := runGeneratedQueryWithBD(t, command, workerSessionEnv(), fakeGCReadyFails, fakeBDEmpty)
				if res.exit != 0 {
					t.Fatalf("query over an empty healthy store exited %d (stderr=%q)", res.exit, res.stderr)
				}
				if strings.TrimSpace(res.stdout) != "[]" {
					t.Errorf("query over an empty healthy store printed %q, want []", res.stdout)
				}
				if strings.TrimSpace(res.stderr) != "" {
					t.Errorf("query over an empty healthy store wrote stderr %q, want none", res.stderr)
				}
			})
		}
	}
}

// fakeBDListFailsReadyServes fails only the crash-recovery read and serves a
// routed bead from every ready read: one broken tier among healthy ones.
const fakeBDListFailsReadyServes = `#!/bin/sh
case "$1" in
  list) printf 'bd: list: store unreadable\n' >&2; exit 1 ;;
  ready) printf '[{"id":"wq-1","status":"open","issue_type":"task","metadata":{"gc.routed_to":"worker"}}]' ;;
  *) printf '[]' ;;
esac
`

// TestSingleStoreWorkQueryFailsWhenOneTierFails pins the partial-failure
// decision. The crash-recovery tier cannot read, while the routed tier behind
// it would serve a bead. Falling through would hand this session fresh work
// while the work it already owns is invisible to it — the multiple-claims-per-
// session accumulation — so one failed tier fails the whole query rather than
// letting a partial answer stand in for the store.
func TestSingleStoreWorkQueryFailsWhenOneTierFails(t *testing.T) {
	a := &Agent{Name: "worker"}
	for topoName, topo := range singleStoreTopologies() {
		t.Run(topoName, func(t *testing.T) {
			res := runGeneratedQueryWithBD(t, a.EffectiveWorkQueryFor(topo), workerSessionEnv(), fakeGCReadyFails, fakeBDListFailsReadyServes)
			if res.exit == 0 {
				t.Fatalf("work_query exited 0 and printed %q although its crash-recovery read failed; a partial answer must not stand in for the store", res.stdout)
			}
			if strings.Contains(res.stdout, "wq-1") {
				t.Errorf("work_query served %q from a later tier after an earlier tier failed", res.stdout)
			}
			if !strings.Contains(res.stderr, "bd list --status in_progress failed") {
				t.Errorf("work_query stderr = %q, want the failed crash-recovery read named", res.stderr)
			}
		})
	}
}

// fakeBDQueryUnparseable answers the bd-1.0.4 ephemeral `bd query` scan with
// output that is not JSON, and every other read with an empty array.
const fakeBDQueryUnparseable = `#!/bin/sh
case "$1" in
  query) printf 'warning: something went sideways\n' ;;
  *) printf '[]' ;;
esac
`

// TestSingleStoreWorkQueryFailsOnUnparseableEphemeralScan covers the read whose
// output only jq ever sees. An unparseable `bd query` used to make every jq
// filter over it come back empty, so the ephemeral tier silently read as empty.
func TestSingleStoreWorkQueryFailsOnUnparseableEphemeralScan(t *testing.T) {
	a := &Agent{Name: "worker"}
	res := runGeneratedQueryWithBD(t, a.EffectiveWorkQueryFor(QueryTopology{}), workerSessionEnv(), fakeGCReadyFails, fakeBDQueryUnparseable)
	assertQueryFailedLoudly(t, res, "bd query output is not valid JSON")
}

// fakeBDQueryFailsReadyEmpty is a bd-1.0.4 store whose ready reads are healthy
// and empty but whose ephemeral `bd query` scan fails.
const fakeBDQueryFailsReadyEmpty = `#!/bin/sh
case "$1" in
  query) printf 'bd: query: store unreadable\n' >&2; exit 1 ;;
  *) printf '[]' ;;
esac
`

// TestPoolDemandCountFailsWhenTheEphemeralScanFails pins the controller's
// count-form. Its ready reads already short-circuit on failure; the bd-1.0.4
// ephemeral leg did not, so a broken `bd query` was counted as zero ephemeral
// demand and the pool never spawned for routed wisps.
func TestPoolDemandCountFailsWhenTheEphemeralScanFails(t *testing.T) {
	a := &Agent{Name: "worker"}
	res := runGeneratedQueryWithBD(t, a.EffectivePoolDemandQueryFor(QueryTopology{}), nil, fakeGCReadyFails, fakeBDQueryFailsReadyEmpty)
	if res.exit == 0 {
		t.Fatalf("scale_check exited 0 and printed %q over a failed ephemeral scan; a count of 0 is what stops a pool from spawning", res.stdout)
	}
	if !strings.Contains(res.stderr, "bd: query: store unreadable") {
		t.Errorf("scale_check stderr = %q, want bd's diagnostic", res.stderr)
	}
}
