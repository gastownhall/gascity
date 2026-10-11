package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cascadeNudgeScriptPath is the on-disk copy of the cascade-nudge order's
// script. The test runs it, so it needs a real path.
const cascadeNudgeScriptPath = "assets/scripts/cascade-nudge-on-blocker-close.sh"

// cascadeNudgeEnvKeys are the settings that would point the script at a live
// session's city, state, or trace file. The test supplies its own state dir.
var cascadeNudgeEnvKeys = map[string]struct{}{
	"GC_CITY":                    {},
	"GC_CITY_RUNTIME_DIR":        {},
	"GC_PACK_STATE_DIR":          {},
	"GC_BD_TRACE_JSON":           {},
	"GC_CASCADE_NUDGE_LOOKBACK":  {},
	"GC_CASCADE_NUDGE_RETENTION": {},
}

func runCascadeNudge(t *testing.T, binDir, stateDir string) (string, error) {
	t.Helper()
	return runPackScript(t, cascadeNudgeScriptPath, binDir, cascadeNudgeEnvKeys,
		[]string{"GC_PACK_STATE_DIR=" + stateDir})
}

func countNudges(t *testing.T, logPath string) (int, string) {
	t.Helper()
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(logged), "\n") {
		if strings.HasPrefix(line, "gc session nudge ") {
			n++
		}
	}
	return n, string(logged)
}

// TestCascadeNudgeAbsorbsDuplicateCloseEvents pins the order's half of the
// at-least-once bead.closed contract. One close can reach the bus twice, from
// the process that closed the bead and from the controller's cache when its
// read saw the close first. When the copies land on either side of an order
// evaluation the order runs once per copy, and the second run's lookback
// window holds both. The dependent's assignee must still be nudged once.
func TestCascadeNudgeAbsorbsDuplicateCloseEvents(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	binDir, logPath := fakeGCBin(t, `case "$1" in
events) cat '`+eventsPath+`' ;;
bd) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"worker-1"}]' ;;
esac
exit 0
`)
	stateDir := filepath.Join(dir, "state")
	copies := []string{
		`{"seq":7,"type":"bead.closed","actor":"gc","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}`,
		`{"seq":8,"type":"bead.closed","actor":"cache-reconcile","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}`,
	}

	var events strings.Builder
	for i, line := range copies {
		events.WriteString(line + "\n")
		if err := os.WriteFile(eventsPath, []byte(events.String()), 0o644); err != nil {
			t.Fatalf("write events: %v", err)
		}
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("run %d: cascade-nudge-on-blocker-close.sh failed: %v\n%s", i+1, err, out)
		}
		if got, logged := countNudges(t, logPath); got != 1 {
			t.Fatalf("after run %d: nudges = %d, want 1 for one close delivered twice; gc calls:\n%s", i+1, got, logged)
		}
	}
	_, logged := countNudges(t, logPath)
	if !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed") {
		t.Fatalf("the nudge must reach the dependent's assignee and name the blocker; gc calls:\n%s", logged)
	}
}
