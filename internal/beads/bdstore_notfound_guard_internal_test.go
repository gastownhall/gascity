package beads

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// An execution-launch failure — the `bd` binary missing from PATH — must NOT be
// classified as a bead-not-found, even though exec.ErrNotFound's message literally
// contains "not found". Otherwise a caller that reads not-found as "absent" (the
// target-scope inherited-scope walk) fails OPEN against a store it could not reach.
func TestIsBdNotFoundExcludesExecLaunchFailure(t *testing.T) {
	// The shape classifyBDExecResult produces: exec.ErrNotFound preserved via %w.
	if isBdNotFound(fmt.Errorf("getting bead %q: %w", "gc-x", exec.ErrNotFound)) {
		t.Fatal("wrapped exec-launch failure classified as bd not-found; the inherited walk would fail open")
	}
	// A bare *exec.Error (Unwrap → exec.ErrNotFound) is likewise excluded.
	if isBdNotFound(&exec.Error{Name: "bd", Err: exec.ErrNotFound}) {
		t.Fatal("*exec.Error classified as bd not-found")
	}
	// Genuine bd not-found signals are still detected (the exclusion must not
	// swallow a real not-found).
	for _, msg := range []string{
		"getting bead \"x\": no issues found matching the provided IDs",
		"issue not found",
		"no issue found",
	} {
		if !isBdNotFound(errors.New(msg)) {
			t.Errorf("genuine bd not-found %q no longer detected", msg)
		}
	}
}
