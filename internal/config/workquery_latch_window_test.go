package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the routed ready tier's window contract (#7436): the tier's
// bounded read (--limit=20) takes its rows BEFORE the prefer script drops the
// structural workflow latches, so a window whose 20 oldest routed rows are all
// stamped expanded roots (or scope/spec latches) hides a ready routed
// scope-member step that sorts after them. The worker answers "[]" while the
// reconciler's unbounded count-form still counts that step, so the pool keeps
// spawning workers that drain on nothing. The conditional unbounded re-read
// restores reachability exactly when a full window was all latches.

// latchWindowRows builds n open, unassigned, stamped graph.v2 workflow roots
// routed to the mayor target: the rows that fill a routed window and are all
// removed by the prefer script's executable-workflow filter.
func latchWindowRows(n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":"gcg-latch-window-%03d","issue_type":"task","status":"open","created_at":"2026-10-01T00:00:%02dZ",`+
			`"metadata":{"gc.kind":"workflow","gc.workflow_expanded":"true","gc.formula_contract":"graph.v2","gc.routed_to":"mayor"}}`, i+1, i)
	}
	return rows
}

// routedLatchMemberRow is a real ready routed work bead: an unassigned
// scope-member step behind the window's latches.
func routedLatchMemberRow() string {
	return `{"id":"gcg-ready-member","issue_type":"task","status":"open","created_at":"2026-10-02T00:00:00Z","metadata":{"gc.scope_role":"member","gc.routed_to":"mayor"}}`
}

// latchWindowBdScript answers a full work query for one target:
//
//   - the bounded routed read (gc.routed_to, --limit=20) with $GC_TEST_ROUTED_WINDOW,
//   - the unbounded routed re-read (gc.routed_to, --limit 0) with $GC_TEST_ROUTED_ALL,
//   - every other tier with [].
//
// The gc.run_target migration tier is matched before the routed ones because
// its metadata-field set (gc.run_target, gc.kind) is disjoint from gc.routed_to.
const latchWindowBdScript = `#!/bin/sh
set -eu
case "$1" in
  list) printf '[]' ;;
  query) printf '[]' ;;
  ready)
    case "$*" in
      *"gc.run_target="*) printf '[]' ;;
      *"--limit 0"*) printf '%s' "$GC_TEST_ROUTED_ALL" ;;
      *"gc.routed_to="*) printf '%s' "$GC_TEST_ROUTED_WINDOW" ;;
      *) printf '[]' ;;
    esac
    ;;
  *) printf '[]' ;;
esac
`

// TestEffectiveWorkQueryLatchWindowServesReadyMember is AC-1: a routed window
// holding routedReadyWindowRows stamped expanded roots sorts ahead of a ready
// routed scope-member step, so the bounded read returns only latches. The work
// query must serve the member — the same row the reconciler's unbounded
// count-form counts — instead of "[]".
//
// The unbounded re-read also carries the latches: proving the member survives
// the same prefer filter there, not just a hand-picked one-row answer.
func TestEffectiveWorkQueryLatchWindowServesReadyMember(t *testing.T) {
	a := Agent{Name: "mayor"}
	latches := strings.Join(latchWindowRows(routedReadyWindowRows), ",")
	window := "[" + latches + "]"
	all := "[" + latches + "," + routedLatchMemberRow() + "]"

	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_TEST_ROUTED_WINDOW": window,
		"GC_TEST_ROUTED_ALL":    all,
	}, latchWindowBdScript)
	if !strings.Contains(out, "gcg-ready-member") {
		t.Fatalf("EffectiveWorkQuery() = %q, want the ready routed scope-member served past a window of %d stamped latches", out, routedReadyWindowRows)
	}
}

// TestEffectiveWorkQueryLatchWindowRereadSkippedWhenWindowServesWork is AC-2:
// the re-read fires only when a FULL window was entirely latches. A short
// window, a window with one surviving executable row, or a window whose
// survivor is an unstamped expanded workflow root (the legacy-root residual,
// kept as a claimable anchor) must cost exactly one bounded routed read and
// answer what the base path answered.
func TestEffectiveWorkQueryLatchWindowRereadSkippedWhenWindowServesWork(t *testing.T) {
	member := routedLatchMemberRow()
	// An unstamped graph.v2 root: legacy-expanded or root-only, it survives the
	// latch filter as a claimable anchor and must keep doing so.
	unstampedRoot := `{"id":"gcg-legacy-root","issue_type":"task","status":"open","created_at":"2026-10-01T00:00:30Z","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"mayor"}}`

	for _, tc := range []struct {
		name   string
		window string
		want   string
	}{
		{
			name:   "short window with a surviving member",
			window: "[" + strings.Join(latchWindowRows(3), ",") + "," + member + "]",
			want:   "gcg-ready-member",
		},
		{
			name:   "full window with a surviving member",
			window: "[" + strings.Join(latchWindowRows(routedReadyWindowRows-1), ",") + "," + member + "]",
			want:   "gcg-ready-member",
		},
		{
			name:   "full window with an unstamped expanded root",
			window: "[" + strings.Join(latchWindowRows(routedReadyWindowRows-1), ",") + "," + unstampedRoot + "]",
			want:   "gcg-legacy-root",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Agent{Name: "mayor"}
			logPath := filepath.Join(t.TempDir(), "routed-reads.log")
			out := runEffectiveWorkQuery(t, a, map[string]string{
				"GC_TEST_ROUTED_WINDOW": tc.window,
				"GC_TEST_ROUTED_ALL":    "[]",
				"GC_TEST_READ_LOG":      logPath,
			}, `#!/bin/sh
set -eu
case "$1" in
  list) printf '[]' ;;
  query) printf '[]' ;;
  ready)
    case "$*" in
      *"gc.run_target="*) printf '[]' ;;
      *"--limit 0"*) printf '[]' ;;
      *"gc.routed_to="*) printf x >> "$GC_TEST_READ_LOG"; printf '%s' "$GC_TEST_ROUTED_WINDOW" ;;
      *) printf '[]' ;;
    esac
    ;;
  *) printf '[]' ;;
esac
`)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("EffectiveWorkQuery() = %q, want candidate %q served from the bounded window", out, tc.want)
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				if os.IsNotExist(err) {
					t.Fatalf("EffectiveWorkQuery() issued no bounded routed read")
				}
				t.Fatalf("read routed-read log: %v", err)
			}
			if reads := strings.Count(string(log), "x"); reads != 1 {
				t.Fatalf("work query issued %d bounded routed reads over a window that serves work, want exactly 1", reads)
			}
		})
	}
}

// TestEffectiveWorkQueryLatchWindowRereadFailsLoudly is AC-3: when the
// unbounded re-read's reader fails, the query fails with the reader-failure
// message instead of answering "[]" — the same never-a-no-work-verdict
// contract every other tier read carries (workQueryReadFailure).
func TestEffectiveWorkQueryLatchWindowRereadFailsLoudly(t *testing.T) {
	a := Agent{Name: "mayor"}
	window := "[" + strings.Join(latchWindowRows(routedReadyWindowRows), ",") + "]"

	stdout, stderr, exit := runWorkQueryWithFakeBdCapture(t, a.EffectiveWorkQuery(), map[string]string{
		"GC_TEST_ROUTED_WINDOW": window,
		"GC_TEST_ROUTED_ALL":    "[]",
	}, `#!/bin/sh
set -eu
case "$1" in
  list) printf '[]' ;;
  query) printf '[]' ;;
  ready)
    case "$*" in
      *"gc.run_target="*) printf '[]' ;;
      *"--limit 0"*) printf 'bd: store exploded\n' >&2; exit 1 ;;
      *"gc.routed_to="*) printf '%s' "$GC_TEST_ROUTED_WINDOW" ;;
      *) printf '[]' ;;
    esac
    ;;
  *) printf '[]' ;;
esac
`)
	if exit == 0 {
		t.Fatalf("work query exited 0 over a failed unbounded routed re-read and printed %q (stderr=%q); the failure must not be answered as \"no work\"", stdout, stderr)
	}
	if strings.Contains(stdout, "[]") {
		t.Errorf("work query printed %q over a failed re-read; \"[]\" is what every consumer reads as an empty queue", stdout)
	}
	if !strings.Contains(stderr, "bd ready failed") {
		t.Errorf("work query stderr = %q, want it to name the failed reader", stderr)
	}
}

// runWorkQueryWithFakeBdCapture runs command with a fake `bd` on PATH and
// returns stdout, stderr and the exit status without failing the test on a
// non-zero exit, for tests whose subject IS the failure path.
func runWorkQueryWithFakeBdCapture(t *testing.T, command string, env map[string]string, bdScript string) (stdout, stderr string, exit int) {
	t.Helper()
	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	commandEnv := []string{"PATH=" + tmp + ":" + os.Getenv("PATH")}
	for k, v := range env {
		commandEnv = append(commandEnv, k+"="+v)
	}
	return runShellCommandCapture(t, command, commandEnv)
}
