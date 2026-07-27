package config

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// Regression coverage for the crash-recovery re-serve defect: the in_progress
// ("crash recovery") work-query tier used to return a bead that could not
// progress, because `bd list --status in_progress` performs no readiness
// computation and emits neither blocked_by nor is_blocked. The hook-side
// defensive filter (filterUnreadyHookCandidates -> isDepBlockedHookCandidate)
// keys on blocked_by, so an absent array read as "not blocked" and a
// gate-blocked or dependency-blocked step was re-served on every hook tick.
//
// These tests EXECUTE the generated shell against a fake `bd` on PATH, so they
// pin observable behaviour rather than the script's spelling (the byte-for-byte
// shape is pinned separately by TestWorkQueryGolden).
//
// Substituting `bd ready` for `bd list` is NOT a valid fix -- bd ready excludes
// in_progress by design -- so TestInProgressTierServesUnblockedCandidate below
// is load-bearing: without it, a "fix" that silences the churn by serving
// nothing at all would look green.

const inProgressListRow = `[{"id":"wk-1","status":"in_progress","assignee":"sess-1","title":"work"}]`

// fakeBdWithDeps returns a fake bd that reports one in_progress assigned bead
// from `bd list` and the given dependency rows from `bd show`. `bd ready`
// returns empty so assertions isolate the in_progress tier.
func fakeBdWithDeps(depsJSON string) string {
	return `#!/bin/sh
case "$1" in
  list) printf '%s' '` + inProgressListRow + `' ;;
  show) printf '%s' '[{"id":"wk-1","status":"in_progress","dependencies":` + depsJSON + `}]' ;;
  *) printf '[]' ;;
esac
`
}

// runInProgressTier executes the in_progress tier of the default work query
// against a fake bd and returns the decoded rows.
func runInProgressTier(t *testing.T, bdScript string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	// `printf "[]"` is the terminal fallback the real query uses when no tier
	// produces a candidate.
	script := standardAssignedInProgressWorkQueryScript(false) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	return rows
}

// TestInProgressTierSkipsGateBlockedCandidate is the primary regression: a
// human gate filed after the step was claimed stores a ready-blocking "blocks"
// edge on the blocked bead. The tier must not serve it.
func TestInProgressTierSkipsGateBlockedCandidate(t *testing.T) {
	rows := runInProgressTier(t, fakeBdWithDeps(
		`[{"id":"gate-1","status":"open","dependency_type":"blocks","await_type":"human"}]`))
	if len(rows) != 0 {
		t.Fatalf("gate-blocked in_progress bead was re-served by the crash-recovery tier: %v", rows)
	}
}

// TestInProgressTierSkipsDependencyBlockedCandidate pins that the defect is not
// gate-specific: a plain unclosed "blocks" dependency is the same edge type and
// must suppress the re-serve identically.
func TestInProgressTierSkipsDependencyBlockedCandidate(t *testing.T) {
	rows := runInProgressTier(t, fakeBdWithDeps(
		`[{"id":"dep-1","status":"open","dependency_type":"blocks"}]`))
	if len(rows) != 0 {
		t.Fatalf("dependency-blocked in_progress bead was re-served: %v", rows)
	}
}

// TestInProgressTierServesUnblockedCandidate is the anti-regression guard for
// the fix itself: crash recovery must still work. A fix that simply swapped in
// `bd ready` (which excludes in_progress) would stop the churn while silently
// disabling recovery, and would fail here.
func TestInProgressTierServesUnblockedCandidate(t *testing.T) {
	rows := runInProgressTier(t, fakeBdWithDeps(`[]`))
	if len(rows) != 1 {
		t.Fatalf("unblocked in_progress bead was NOT served; crash recovery is broken: %v", rows)
	}
	if rows[0]["id"] != "wk-1" {
		t.Fatalf("served the wrong bead: %v", rows)
	}
	if _, ok := rows[0]["blocked_by"]; !ok {
		t.Errorf("served row is missing the blocked_by array the hook-side filter reads: %v", rows)
	}
}

// TestInProgressTierServesCandidateWithClosedBlocker pins that a resolved gate
// releases the step. Without this, answering a gate would strand the work
// instead of resuming it.
func TestInProgressTierServesCandidateWithClosedBlocker(t *testing.T) {
	rows := runInProgressTier(t, fakeBdWithDeps(
		`[{"id":"gate-1","status":"closed","dependency_type":"blocks","await_type":"human"}]`))
	if len(rows) != 1 {
		t.Fatalf("step with a CLOSED blocker was not resumed: %v", rows)
	}
}

// TestInProgressTierIgnoresNonBlockingDependencyTypes pins the type filter
// against beads.IsReadyBlockingDependencyType. parent-child and tracks edges
// never block readiness -- treating them as blockers would strand every
// molecule step, since each carries a tracks/parent-child edge to its root.
func TestInProgressTierIgnoresNonBlockingDependencyTypes(t *testing.T) {
	for _, depType := range []string{"parent-child", "tracks", "related", "discovered-from"} {
		t.Run(depType, func(t *testing.T) {
			rows := runInProgressTier(t, fakeBdWithDeps(
				`[{"id":"root-1","status":"open","dependency_type":"`+depType+`"}]`))
			if len(rows) != 1 {
				t.Fatalf("non-blocking %q edge wrongly suppressed the re-serve: %v", depType, rows)
			}
		})
	}
}

// TestInProgressTierFallsThroughWhenBlocked pins that a blocked candidate does
// not swallow the tick: the ready-gated tier still runs, so a session holding
// one blocked step can still be served its other ready assigned work. The stock
// script short-circuited with `&& exit 0` and never reached the ready tier.
func TestInProgressTierFallsThroughWhenBlocked(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	// Same fake bd, except `bd ready` yields a different, genuinely ready bead.
	bdScript := `#!/bin/sh
case "$1" in
  list) printf '%s' '` + inProgressListRow + `' ;;
  show) printf '%s' '[{"id":"wk-1","status":"in_progress","dependencies":[{"id":"gate-1","status":"open","dependency_type":"blocks"}]}]' ;;
  ready) printf '%s' '[{"id":"wk-2","status":"open","assignee":"sess-1"}]' ;;
  *) printf '[]' ;;
esac
`
	script := standardAssignedWorkQueryScript(false) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 1 || rows[0]["id"] != "wk-2" {
		t.Fatalf("blocked in_progress candidate did not fall through to the ready tier; got %q", out)
	}
}

// TestLegacyControlInProgressTierSkipsBlockedCandidate pins that the
// control-dispatcher variant of the same tier
// (legacyControlAssignedInProgressWorkQueryScript) carries the identical
// dep-blind `bd list --status in_progress` query and therefore the identical
// defect. Fixing only the standard tier would leave rigs on the legacy control
// shape churning exactly as before.
func TestLegacyControlInProgressTierSkipsBlockedCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := legacyControlAssignedInProgressWorkQueryScript(false) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"},
		fakeBdWithDeps(`[{"id":"gate-1","status":"open","dependency_type":"blocks","await_type":"human"}]`))

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 0 {
		t.Fatalf("legacy-control tier re-served a gate-blocked in_progress bead: %v", rows)
	}
}

// TestLegacyControlInProgressTierServesUnblockedCandidate is the matching
// anti-regression guard: crash recovery must survive on the legacy shape too.
func TestLegacyControlInProgressTierServesUnblockedCandidate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := legacyControlAssignedInProgressWorkQueryScript(false) + `printf "[]"`
	out := runShellWithFakeBd(t, script, map[string]string{"GC_SESSION_ID": "sess-1"},
		fakeBdWithDeps(`[]`))

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
	}
	if len(rows) != 1 || rows[0]["id"] != "wk-1" {
		t.Fatalf("legacy-control tier did not serve an unblocked in_progress bead: %v", rows)
	}
}
