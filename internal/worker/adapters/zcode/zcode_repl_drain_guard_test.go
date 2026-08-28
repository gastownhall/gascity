package zcode_test

import (
	"os/exec"
	"strings"
	"testing"

	zcodeadapter "github.com/gastownhall/gascity/internal/worker/adapters/zcode"
)

// TestZcodeReplDrainGuardToleratesUnsetMore pins the fix for ga-uqdim3.
//
// zcode-repl's paste-coalescing loop reads an extra line with a timeout
// (DRAIN_TIMEOUT=1) to see whether more input is already buffered. On GNU
// bash (confirmed on this host's 5.3.9), a timed-out read still assigns the
// target variable ("<set, empty>") whether it hits EOF or TMOUT, so the
// guard that follows never sees an unset name. On macOS's bash 3.2, a
// timed-out read leaves the variable genuinely unset, and the guard
// dereferences it under `set -u`, aborting the whole turn.
//
// That divergence can't be reproduced by actually timing out a `read` on
// Linux -- bash 5 will always assign the variable here. So this test forces
// the hazard directly (drain_rc=1, "more" unset) and evaluates the real
// guard text extracted live from the embedded script, anchored on lines
// that don't change across the fix, so it keeps testing the actual source
// even after the guard is edited.
func TestZcodeReplDrainGuardToleratesUnsetMore(t *testing.T) {
	block := extractDrainGuardBlock(t, string(zcodeadapter.Script()))

	script := "set -u\nline=''\ndrain_rc=1\nunset more\nwhile :; do\n" + block + "\ndone\necho GUARD_SURVIVED\n"
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "GUARD_SURVIVED") {
		t.Fatalf("zcode-repl's drain-loop guard dereferences an unset $more under "+
			"set -u -- this is what aborts real sessions on macOS's bash 3.2 (ga-uqdim3): "+
			"%v\noutput:\n%s\nextracted guard block:\n%s", err, out, block)
	}
}

// extractDrainGuardBlock returns the body of zcode-repl's inner drain loop,
// from the "did the drain read succeed" branch through (not including) its
// closing "done". Anchored on drain_rc, which is untouched by the ga-uqdim3
// fix, rather than on the vulnerable "$more" line itself, so the extraction
// still finds the block after that line is edited.
func extractDrainGuardBlock(t *testing.T, src string) string {
	t.Helper()

	lines := strings.Split(src, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "if (( drain_rc == 0 )); then") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("zcode-repl: drain_rc guard anchor not found; script structure changed, update this test")
	}

	end := -1
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "done" {
			end = i
			break
		}
	}
	if end < 0 {
		t.Fatal("zcode-repl: no closing 'done' found after the drain_rc guard; update this test")
	}

	block := strings.Join(lines[start:end], "\n")
	if !strings.Contains(block, "more") {
		t.Fatalf("zcode-repl: extracted drain-loop block doesn't reference $more, anchors are wrong:\n%s", block)
	}
	return block
}
