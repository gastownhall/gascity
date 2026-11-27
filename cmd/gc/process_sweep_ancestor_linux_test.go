//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// sweepAncestorRoleEnv selects the role a re-executed test binary plays in
// TestProcessSweep_LeavesALiveAncestorsOwnChild. It has no GC_ prefix: this
// package's TestMain scrubs GC_* variables before any test runs.
const sweepAncestorRoleEnv = "SWEEP_ANCESTOR_TEST_ROLE"

// A live ancestor of the controller that never set PR_SET_CHILD_SUBREAPER is
// not an adopter, so a root it spawned itself (bash running gc beside a
// session-tagged program) is owned, and the sweep leaves it even with its
// row closed. A helper parent that never set the flag runs the sweep below
// it, the way bash runs gc; the fake scanner reports a closed row's root
// whose parent is that helper, beside one of the same row reparented to
// init, which is reaped, so the sweep did run.
func TestProcessSweep_LeavesALiveAncestorsOwnChild(t *testing.T) {
	switch role := os.Getenv(sweepAncestorRoleEnv); role {
	case "", "ancestor":
		next := "ancestor"
		if role == "ancestor" {
			next = "sweeper"
		}
		cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
		cmd.Env = append(bazeltest.HelperProcessEnv(os.Environ()), sweepAncestorRoleEnv+"="+next)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s role failed: %v\n%s", next, err, out)
		}
		if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
			t.Fatalf("%s role did not run the test:\n%s", next, out)
		}
	case "sweeper":
		ancestor := os.Getppid()
		sp := newProcessTableSweepProvider(
			runtime.LiveRuntime{SessionID: sweepRowID, PID: 701, PPID: ancestor, StartIdentity: "s"},
			runtime.LiveRuntime{SessionID: sweepRowID, PID: 702, PPID: 1, StartIdentity: "s"},
		)
		var stderr bytes.Buffer
		store := beads.NewMemStoreFrom(0, sweepClosed, nil)
		if got := sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, store, "", &stderr); got != 1 {
			t.Fatalf("swept %d, want 1; stderr=%q", got, stderr.String())
		}
		if len(sp.terminated) != 1 || sp.terminated[0].PID != 702 {
			t.Fatalf("terminated %+v, want only pid 702; the root whose parent is the live ancestor %d must stay", sp.terminated, ancestor)
		}
	default:
		t.Fatalf("unknown %s=%q", sweepAncestorRoleEnv, role)
	}
}
