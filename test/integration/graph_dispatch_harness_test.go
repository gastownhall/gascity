//go:build integration

package integration

import (
	"os"
	"os/exec"
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
			cmd := exec.Command("bash", "-c", fn+"\nshould_use_hook_fallback\n")
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, tc.env...)
			got := cmd.Run() == nil // exit 0 => fallback selected
			if got != tc.want {
				t.Fatalf("should_use_hook_fallback(env=%v) selected=%v, want %v", tc.env, got, tc.want)
			}
		})
	}
}
