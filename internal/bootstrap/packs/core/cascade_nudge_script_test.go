package core

import (
	"fmt"
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
bd) case "$*" in
    *direction=down*) printf '%s\n' '[{"id":"ga-blk","status":"closed"}]' ;;
    *) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"worker-1"}]' ;;
    esac ;;
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

// cascadeNudgeOnce runs the script once over one close of ga-blk whose
// dependent ga-dep, assigned to worker-1, has the blockers down answers for
// (a JSON array, or a shell command's exit for a failed read), and returns
// the nudges it sent.
func cascadeNudgeOnce(t *testing.T, down string) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	event := `{"seq":7,"type":"bead.closed","actor":"gc","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}`
	if err := os.WriteFile(eventsPath, []byte(event+"\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	binDir, logPath := fakeGCBin(t, `case "$1" in
events) cat '`+eventsPath+`' ;;
bd) case "$*" in
    *direction=down*) `+down+` ;;
    *) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"worker-1"}]' ;;
    esac ;;
esac
exit 0
`)
	if out, err := runCascadeNudge(t, binDir, filepath.Join(dir, "state")); err != nil {
		t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
	}
	return countNudges(t, logPath)
}

// A dependent a blocker still holds is not nudged: its assignee has nothing
// to do yet, and a nudge would wake an asleep seat parked on it for nothing
// (ga-ylpbsy.16, RCA R4). The seat wakes on its own once the dependent is
// ready. A blocker holds as beads.DependencySatisfied judges it: not closed,
// or closed blocked (gc.work_outcome=blocked) and not a formula step that
// passed.
func TestCascadeNudgeSkipsADependentStillBlocked(t *testing.T) {
	cases := map[string]string{
		"another blocker closed blocked": `[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"closed","metadata":{"gc.work_outcome":"blocked"}}]`,
		// mol-do-work stamps gc.outcome=pass on the work bead it closes,
		// blocked or not: without gc.step_ref it is no dispatch verdict.
		"another work bead closed blocked with gc.outcome=pass": `[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"closed","metadata":{"gc.work_outcome":"blocked","gc.outcome":"pass"}}]`,
		"another formula step closed blocked that failed":       `[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"closed","metadata":{"gc.work_outcome":"blocked","gc.step_ref":"mol.step","gc.outcome":"fail"}}]`,
		"the closing blocker closed blocked":                    `[{"id":"ga-blk","status":"closed","metadata":{"gc.work_outcome":"blocked"}}]`,
		// A row this read cannot judge proves nothing, but an open one
		// beside it still holds the dependent.
		"an open blocker beside an external one": `[{"id":"ga-blk","status":"closed"},{"id":"gb-ext","status":"","external":true},{"id":"ga-other","status":"open"}]`,
	}
	for _, status := range []string{"open", "in_progress", "blocked", "deferred", "hooked"} {
		cases["another blocker "+status] = `[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"` + status + `"}]`
	}
	// A pinned blocker holds on both reads, though bd's readiness counts it
	// satisfied. The bd passthrough prints bd's own status, as bd 1.3's
	// `dep list --json` does for it; the in-process class-binding read
	// (doBdByIDDepList) prints the bead its store reads, and the store folds
	// every status but closed and in_progress to open (mapBdStatus).
	cases["another blocker pinned, as the bd passthrough prints it"] = `[{"id":"ga-blk","status":"closed","dependency_type":"blocks"},{"id":"ga-other","status":"pinned","dependency_type":"blocks"}]`
	cases["another blocker pinned, as the class-binding read prints it"] = `[{"id":"ga-blk","status":"closed","dependency_type":"blocks"},{"id":"ga-other","status":"open","dependency_type":"blocks"}]`
	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			if got, logged := cascadeNudgeOnce(t, `printf '%s\n' '`+rows+`'`); got != 0 {
				t.Fatalf("nudges = %d, want 0 while a blocker holds ga-dep; gc calls:\n%s", got, logged)
			}
		})
	}
}

// The close of a dependent's last blocker that holds it nudges its assignee
// once.
func TestCascadeNudgeNudgesOnTheLastBlockersClose(t *testing.T) {
	for name, down := range map[string]string{
		"every blocker closed":                               `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"closed"}]'`,
		"the other blocker closed with another work outcome": `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"closed","metadata":{"gc.work_outcome":"shipped"}}]'`,
		// A formula step dispatch passed is advanced past: its work outcome
		// does not gate readiness (beads.ReadinessWorkOutcome).
		"the other blocker a formula step closed blocked that passed":   `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"closed","metadata":{"gc.work_outcome":"blocked","gc.step_ref":"mol.step","gc.outcome":"pass"}}]'`,
		"the closing blocker a formula step closed blocked that passed": `printf '%s\n' '[{"id":"ga-blk","status":"closed","metadata":{"gc.work_outcome":"blocked","gc.step_ref":"mol.step","gc.outcome":"pass"}}]'`,
		"the closing blocker read before its close":                     `printf '%s\n' '[{"id":"ga-blk","status":"open"}]'`,
		"no blocker listed": `printf '%s\n' '[]'`,
		// A row this read cannot judge proves nothing: a blocker outside the
		// dependent's store comes back external with no status, and any
		// status an external row carries is not its owning store's.
		"the other blocker external":                           `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"gb-ext","status":"","external":true}]'`,
		"the other blocker external, whatever status it shows": `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"gb-ext","status":"open","external":true}]'`,
		"the other blocker with no status":                     `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other"}]'`,
		// A read that fails proves nothing: the nudge goes out, as before.
		"a failed blocker read":       `exit 1`,
		"an unparseable blocker read": `printf 'not json\n'`,
		"a blocker read not a list":   `printf '%s\n' '{"error":"unavailable"}'`,
	} {
		t.Run(name, func(t *testing.T) {
			got, logged := cascadeNudgeOnce(t, down)
			if got != 1 || !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed") {
				t.Fatalf("nudges = %d, want one to worker-1; gc calls:\n%s", got, logged)
			}
		})
	}
}

// A still-blocked pair is recorded: the lookback's later runs see the same
// close again and must not read the dependent's blockers again.
func TestCascadeNudgeRecordsAStillBlockedPair(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	event := `{"seq":7,"type":"bead.closed","actor":"gc","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}`
	if err := os.WriteFile(eventsPath, []byte(event+"\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	binDir, logPath := fakeGCBin(t, `case "$1" in
events) cat '`+eventsPath+`' ;;
bd) case "$*" in
    *direction=down*) printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"open"}]' ;;
    *) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"worker-1"}]' ;;
    esac ;;
esac
exit 0
`)
	stateDir := filepath.Join(dir, "state")
	for i := range 2 {
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("run %d: cascade-nudge-on-blocker-close.sh failed: %v\n%s", i+1, err, out)
		}
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(logged), "--direction=down"); got != 1 {
		t.Fatalf("blocker reads = %d, want 1 across two runs; gc calls:\n%s", got, logged)
	}
	if n, _ := countNudges(t, logPath); n != 0 {
		t.Fatalf("nudges = %d, want 0", n)
	}
}

// cascadeRun is one run of the script in a sequence: the blockers whose
// closes reach the bus before it, the blockers ga-dep's read answers, and the
// nudges sent once it ends.
type cascadeRun struct {
	closed []string
	down   string
	want   int
}

// cascadeNudgeRuns runs the script once per run over one state dir. Each
// run's lookback holds every close so far, ga-dep (assigned to worker-1) is
// every blocker's one dependent, and its blockers read as the run's down. It
// checks the nudges after each run and returns the gc calls.
func cascadeNudgeRuns(t *testing.T, runs []cascadeRun) string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	downPath := filepath.Join(dir, "down.json")
	binDir, logPath := fakeGCBin(t, `case "$1" in
events) cat '`+eventsPath+`' ;;
bd) case "$*" in
    *direction=down*) cat '`+downPath+`' ;;
    *) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"worker-1"}]' ;;
    esac ;;
esac
exit 0
`)
	stateDir := filepath.Join(dir, "state")
	var events strings.Builder
	seq := 7
	for i, r := range runs {
		for _, id := range r.closed {
			fmt.Fprintf(&events, `{"seq":%d,"type":"bead.closed","actor":"gc","subject":%q,"payload":{"bead":{"id":%q,"status":"closed"}}}`+"\n", seq, id, id)
			seq++
		}
		if err := os.WriteFile(eventsPath, []byte(events.String()), 0o644); err != nil {
			t.Fatalf("write events: %v", err)
		}
		if err := os.WriteFile(downPath, []byte(r.down+"\n"), 0o644); err != nil {
			t.Fatalf("write blockers: %v", err)
		}
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("run %d: cascade-nudge-on-blocker-close.sh failed: %v\n%s", i+1, err, out)
		}
		if got, logged := countNudges(t, logPath); got != r.want {
			t.Fatalf("after run %d: nudges = %d, want %d; gc calls:\n%s", i+1, got, r.want, logged)
		}
	}
	_, logged := countNudges(t, logPath)
	return logged
}

// A dependent only its closing blocker's own blocked close holds is not
// recorded: that blocker's later close that passes is the same (blocker,
// dependent) pair, and must still nudge.
func TestCascadeNudgeLeavesAPairItsOwnBlockedCloseHoldsUnrecorded(t *testing.T) {
	logged := cascadeNudgeRuns(t, []cascadeRun{
		{closed: []string{"ga-blk"}, down: `[{"id":"ga-blk","status":"closed","metadata":{"gc.work_outcome":"blocked"}}]`},
		// The blocker is reopened and closes again, and this time it passes.
		{closed: []string{"ga-blk"}, down: `[{"id":"ga-blk","status":"closed","metadata":{"gc.work_outcome":"shipped"}}]`, want: 1},
	})
	if !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed") {
		t.Fatalf("the passing close must nudge worker-1; gc calls:\n%s", logged)
	}
}

// A blocker's own blocked close outranks another blocker that still holds
// the dependent: its pair stays unrecorded even then, so once the other
// blocker has closed (its pair recorded, the dependent still held by the
// blocked close), the blocker's later close that passes still nudges.
func TestCascadeNudgeLeavesItsOwnBlockedCloseUnrecordedBesideAnotherBlocker(t *testing.T) {
	logged := cascadeNudgeRuns(t, []cascadeRun{
		{closed: []string{"ga-blk"}, down: `[{"id":"ga-blk","status":"closed","metadata":{"gc.work_outcome":"blocked"}},{"id":"ga-other","status":"open"}]`},
		{closed: []string{"ga-other"}, down: `[{"id":"ga-blk","status":"closed","metadata":{"gc.work_outcome":"blocked"}},{"id":"ga-other","status":"closed"}]`},
		// ga-blk is reopened and closes again, and this time it passes.
		{closed: []string{"ga-blk"}, down: `[{"id":"ga-blk","status":"closed","metadata":{"gc.work_outcome":"shipped"}},{"id":"ga-other","status":"closed"}]`, want: 1},
	})
	if !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed") {
		t.Fatalf("ga-blk's passing close must nudge worker-1; gc calls:\n%s", logged)
	}
}
