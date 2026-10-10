//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// extractShellFunc returns the source text of a top-level shell function
// (header line through its column-0 closing brace) from a script file, so a
// test can execute the real definition without sourcing the whole script.
func extractShellFunc(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, name+"() {") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("function %q not found in %s", name, path)
	}
	for i := start + 1; i < len(lines); i++ {
		if lines[i] == "}" {
			return strings.Join(lines[start:i+1], "\n")
		}
	}
	t.Fatalf("no column-0 closing brace for %q in %s", name, path)
	return ""
}

// TestGraphDispatchTransientOnceBudgetSurvivesLostClose guards the ga-j88sfp
// gate flake: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash
// finishing the workflow with no review.attempt.2 (four release-gate sightings
// between 2026-09-08 and 2026-09-15).
//
// should_fail_transient_once used to consume its one-shot budget by writing the
// marker at decision time, before the failing close was known to have landed.
// close_with_result swallows every bd error and a pool worker can be SIGKILLed
// mid-close, so a dropped close left the attempt bead open with the budget
// already spent. The next pass over that bead — by the same worker or any other
// slot, since the marker dir is shared by the always-on worker and every
// polecat — saw the marker, skipped the injection and closed the attempt
// gc.outcome=pass. classifyRetryAttempt then reads a clean pass, schedules no
// retry, and the workflow finalizes pass with only .attempt.1.
//
// The budget must therefore be committed only against a landed failure, while
// still being spent exactly once when the close does land.
func TestGraphDispatchTransientOnceBudgetSurvivesLostClose(t *testing.T) {
	script := agentScript("graph-dispatch.sh")
	var fns strings.Builder
	for _, name := range []string{
		"trim_spaces",
		"sanitize_key",
		"ref_matches_suffix_list",
		"transient_once_marker",
		"should_fail_transient_once",
		"commit_transient_once",
	} {
		fns.WriteString(extractShellFunc(t, script, name))
		fns.WriteString("\n")
	}

	const ref = "mol-retry-recovery-smoke.review.attempt.1"

	// decide reports should_fail_transient_once for ref, optionally committing
	// the budget first, against a marker dir that persists across calls.
	decide := func(t *testing.T, stateDir, ref string, commitFirst bool) bool {
		t.Helper()
		body := fns.String()
		if commitFirst {
			body += "commit_transient_once " + ref + "\n"
		}
		body += "should_fail_transient_once " + ref + "\n"
		cmd := exec.Command("bash", "-c", body)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HARNESS_STATE_DIR=" + stateDir,
			"GC_GRAPH_TRANSIENT_ONCE_SUFFIXES=review.attempt.1",
		}
		return cmd.Run() == nil // exit 0 => inject a transient failure
	}

	t.Run("budget survives a close that never landed", func(t *testing.T) {
		stateDir := t.TempDir()
		if !decide(t, stateDir, ref, false) {
			t.Fatal("first decision declined to inject the transient failure")
		}
		// The close was dropped, so nothing committed. The retry the test is
		// built on only exists if the injection is still owed here.
		if !decide(t, stateDir, ref, false) {
			t.Fatal("transient-once budget was consumed without a landed close: " +
				"the injected failure is lost and the attempt closes pass, " +
				"so no .attempt.2 is ever scheduled (ga-j88sfp)")
		}
	})

	t.Run("budget is spent once the close lands", func(t *testing.T) {
		stateDir := t.TempDir()
		if !decide(t, stateDir, ref, false) {
			t.Fatal("first decision declined to inject the transient failure")
		}
		if decide(t, stateDir, ref, true) {
			t.Fatal("transient failure injected twice after a landed close; " +
				"the once-only contract is broken")
		}
	})

	t.Run("unselected ref is never injected", func(t *testing.T) {
		stateDir := t.TempDir()
		if decide(t, stateDir, "mol-retry-recovery-smoke.review.attempt.2", false) {
			t.Fatal("injected a transient failure for a ref outside the suffix list")
		}
	})
}

// TestGraphDispatchHookFallbackForNamedWorker guards the rc-gate regression
// introduced by 661cefebd: the test worker agent's fast path polls
// `bd ready --assignee=$ASSIGNEE` by NAME, but the deterministic control
// dispatcher assigns ralph re-iterated run_target=<worker> work to the
// always-on worker by its SESSION BEAD ID (assignee=<bead id>, gc.routed_to
// cleared). A name-only query can never match a bead-ID assignee, so a named
// session must fall back to `gc hook`, whose work query also resolves by
// GC_SESSION_ID. Without the fallback the worker name-polls into the void and
// the review workflow stalls (TestAdoptPRFormulaRetriesTransientReviewerStep).
//
// This executes the real should_use_hook_fallback definition so the harness
// invariant is pinned cheaply and deterministically (no 24-minute workflow).
func TestGraphDispatchHookFallbackForNamedWorker(t *testing.T) {
	fn := extractShellFunc(t, agentScript("graph-dispatch.sh"), "should_use_hook_fallback")

	cases := []struct {
		name string
		env  []string
		want bool // want should_use_hook_fallback to select the gc hook path
	}{
		{
			name: "always-on named worker (the regression)",
			env:  []string{"GC_SESSION_ORIGIN=named", "GC_TEMPLATE=worker", "GC_AGENT=worker"},
			want: true,
		},
		{
			name: "ephemeral pool session",
			env:  []string{"GC_SESSION_ORIGIN=ephemeral", "GC_TEMPLATE=polecat", "GC_AGENT=polecat-wisp-x"},
			want: true,
		},
		{
			name: "explicit fallback flag",
			env:  []string{"GC_GRAPH_HOOK_FALLBACK=1"},
			want: true,
		},
		{
			name: "instance name differs from template",
			env:  []string{"GC_TEMPLATE=polecat", "GC_AGENT=polecat-1"},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runGraphDispatchShell(fn+"\nshould_use_hook_fallback\n", append([]string{"PATH=" + os.Getenv("PATH")}, tc.env...))
			got := err == nil // exit 0 => fallback selected
			if got != tc.want {
				t.Fatalf("should_use_hook_fallback(env=%v) selected=%v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// runGraphDispatchShell runs script under bash with exactly env and returns
// its combined output; a non-zero exit is the returned error.
func runGraphDispatchShell(script string, env []string) (string, error) {
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// graphDispatchFuncs returns the named top-level functions of the graph
// worker script, concatenated so a test can run them without the main loop.
func graphDispatchFuncs(t *testing.T, names ...string) string {
	t.Helper()
	script := agentScript("graph-dispatch.sh")
	var fns strings.Builder
	for _, name := range names {
		fns.WriteString(extractShellFunc(t, script, name))
		fns.WriteString("\n")
	}
	return fns.String()
}

// fakeBdDir writes a bd stub that prints stdout and exits with code, and
// returns a PATH that resolves bd to it ahead of the real tools (jq, timeout).
func fakeBdDir(t *testing.T, stdout string, code int) string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("the graph worker script needs jq: %v", err)
	}
	dir := t.TempDir()
	body := "#!/bin/sh\ncat <<'JSON'\n" + stdout + "\nJSON\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestGraphDispatchReadBeadStateReadsAllFieldsInOneCall pins read_bead_state,
// which replaced three separate bd show calls (status, gc.outcome, assignee)
// in the graph worker: every field lands in its own variable, including when
// a field in the middle is empty, and a failed read leaves all three empty.
func TestGraphDispatchReadBeadStateReadsAllFieldsInOneCall(t *testing.T) {
	fns := graphDispatchFuncs(t, "json_payload", "read_bead_state")
	cases := []struct {
		name   string
		stdout string
		code   int
		want   string
	}{
		{
			name:   "object with every field",
			stdout: `{"id":"gc-1","status":"closed","assignee":"worker","metadata":{"gc.outcome":"pass"}}`,
			want:   "closed|pass|worker",
		},
		{
			name:   "empty outcome keeps the assignee in place",
			stdout: `[{"id":"gc-1","status":"in_progress","assignee":"worker","metadata":{}}]`,
			want:   "in_progress||worker",
		},
		{
			name:   "no metadata and no assignee",
			stdout: "warning: noise before the payload\n" + `[{"id":"gc-1","status":"open"}]`,
			want:   "open||",
		},
		{
			name:   "failed read",
			stdout: "Error: no issue found",
			code:   1,
			want:   "||",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runGraphDispatchShell("set -euo pipefail\n"+fns+
				`read_bead_state gc-1; printf '%s|%s|%s' "$BEAD_STATUS" "$BEAD_OUTCOME" "$BEAD_ASSIGNEE"`,
				[]string{"PATH=" + fakeBdDir(t, tc.stdout, tc.code)})
			if err != nil {
				t.Fatalf("read_bead_state: %v\n%s", err, out)
			}
			if out != tc.want {
				t.Fatalf("read_bead_state = %q, want %q", out, tc.want)
			}
		})
	}
}

// TestGraphDispatchRoutedQueueServesOnlyUnheldWork pins fetch_routed_queue,
// the graph worker's between-hook-polls read of its learned route: it serves
// what the routed tier of the default work query serves (the bd flags cover
// unassigned and non-epic), so a bead on a hold:* dispatch label is dropped,
// and an empty or failed read reports no work.
func TestGraphDispatchRoutedQueueServesOnlyUnheldWork(t *testing.T) {
	fns := graphDispatchFuncs(t, "json_payload", "fetch_routed_queue")
	cases := []struct {
		name   string
		stdout string
		code   int
		want   string // ids served, comma-separated; "" means the call fails
	}{
		{
			name:   "unheld work is served",
			stdout: `[{"id":"gc-1","labels":["pool:worker"]},{"id":"gc-2"},{"id":"gc-3","labels":["hold:external"]}]`,
			want:   "gc-1,gc-2",
		},
		{name: "only held work", stdout: `[{"id":"gc-3","labels":["hold:external"]}]`},
		{name: "empty queue", stdout: `[]`},
		{name: "bd failure", stdout: "Error: database unreachable", code: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runGraphDispatchShell("set -euo pipefail\n"+fns+
				`if q=$(fetch_routed_queue worker); then printf '%s' "$q" | jq -r '[.[].id] | join(",")'; else echo FAIL; fi`,
				[]string{"PATH=" + fakeBdDir(t, tc.stdout, tc.code)})
			if err != nil {
				t.Fatalf("fetch_routed_queue: %v\n%s", err, out)
			}
			got := strings.TrimSpace(out)
			want := tc.want
			if want == "" {
				want = "FAIL"
			}
			if got != want {
				t.Fatalf("fetch_routed_queue served %q, want %q", got, want)
			}
		})
	}
}

// TestGraphDispatchHookPollCadence pins when the graph worker runs gc hook:
// on every iteration until it has learned a route, then on a dispatch wake or
// once HOOK_POLL_INTERVAL seconds have passed since the last hook poll.
func TestGraphDispatchHookPollCadence(t *testing.T) {
	fn := graphDispatchFuncs(t, "hook_poll_due")
	cases := []struct {
		name  string
		setup string
		woke  string
		want  bool
	}{
		{name: "no route learned", setup: `LEARNED_ROUTE=""; LAST_HOOK_POLL=$SECONDS`, woke: "false", want: true},
		{name: "first poll after learning", setup: `LEARNED_ROUTE=worker; LAST_HOOK_POLL=""`, woke: "false", want: true},
		{name: "dispatch wake", setup: `LEARNED_ROUTE=worker; LAST_HOOK_POLL=$SECONDS`, woke: "true", want: true},
		{name: "recent hook poll", setup: `LEARNED_ROUTE=worker; LAST_HOOK_POLL=$SECONDS`, woke: "false", want: false},
		{name: "interval elapsed", setup: `LEARNED_ROUTE=worker; LAST_HOOK_POLL=$((SECONDS - 3))`, woke: "false", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runGraphDispatchShell(fn+"HOOK_POLL_INTERVAL=3\n"+tc.setup+"\nhook_poll_due "+tc.woke+"\n",
				[]string{"PATH=" + os.Getenv("PATH")})
			if got := err == nil; got != tc.want {
				t.Fatalf("hook_poll_due = %v, want %v", got, tc.want)
			}
		})
	}
}
