package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cascadeScriptPath is the on-disk copy of the cascade-nudge order script.
// These tests run it against a fake `gc` so they can drive the event window
// and the dependency graph between runs.
const cascadeScriptPath = "assets/scripts/cascade-nudge-on-blocker-close.sh"

// cascadeFakeGC answers the four gc calls the script makes from files under
// $CASCADE_FIX: events.jsonl for `gc events`, <direction>-<id>.json for
// `gc bd dep list <id> --direction=<direction>` (filtered on
// dependency_type when --type is given), and it appends every
// `gc session nudge` to nudges.log.
const cascadeFakeGC = `#!/bin/sh
case "$1" in
rig) echo '{"rigs":[]}' ;;
events) cat "$CASCADE_FIX/events.jsonl" 2>/dev/null ;;
bd)
    id="$4"; dir=""; typ=""
    for a in "$@"; do
        case "$a" in
        --direction=*) dir="${a#--direction=}" ;;
        --type=*) typ="${a#--type=}" ;;
        esac
    done
    rows="$(cat "$CASCADE_FIX/$dir-$id.json" 2>/dev/null || echo '[]')"
    if [ -n "$typ" ]; then
        printf '%s' "$rows" | jq -c --arg t "$typ" '[.[] | select(.dependency_type == $t)]'
    else
        printf '%s\n' "$rows"
    fi
    ;;
session) printf '%s\n' "$*" >> "$CASCADE_FIX/nudges.log" ;;
esac
`

type cascadeFixture struct {
	t        *testing.T
	dir      string
	binDir   string
	stateDir string
}

func newCascadeFixture(t *testing.T) *cascadeFixture {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
	f := &cascadeFixture{t: t, dir: t.TempDir(), binDir: t.TempDir(), stateDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(f.binDir, "gc"), []byte(cascadeFakeGC), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}
	return f
}

func (f *cascadeFixture) write(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
}

// closed sets the bead.closed events in the lookback window.
func (f *cascadeFixture) closed(ids ...string) {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(`{"type":"bead.closed","payload":{"bead":{"id":"` + id + `"}}}` + "\n")
	}
	f.write("events.jsonl", b.String())
}

// run executes the script once with only the settings it reads from this
// fixture; a live session's GC_* variables are dropped so the host city's
// state dir and trace log are never touched.
func (f *cascadeFixture) run() {
	f.t.Helper()
	env := []string{}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GC_") || strings.HasPrefix(entry, "PATH=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"PATH="+f.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CASCADE_FIX="+f.dir,
		"GC_PACK_STATE_DIR="+f.stateDir,
	)
	cmd := exec.Command("bash", cascadeScriptPath)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("cascade script failed: %v\n%s", err, out)
	}
}

func (f *cascadeFixture) nudges() []string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "nudges.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("read nudges.log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func (f *cascadeFixture) wantNudges(n int) []string {
	f.t.Helper()
	got := f.nudges()
	if len(got) != n {
		f.t.Fatalf("nudges = %d, want %d:\n%s", len(got), n, strings.Join(got, "\n"))
	}
	return got
}

// TestCascadeNudgeIncludesInProgressDependent pins that a claimed dependent
// is nudged.
func TestCascadeNudgeIncludesInProgressDependent(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"in_progress","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed"}]`)
	f.closed("gc-b1")

	f.run()

	got := f.wantNudges(1)
	want := "session nudge worker blocker gc-b1 closed — your dependent gc-d1 has no open blockers now"
	if got[0] != want {
		t.Errorf("nudge = %q, want %q", got[0], want)
	}
}

// TestCascadeNudgeWaitsForLastBlocker pins that a dependent with two blockers
// is not sent back to work while one of them is still open: the first close
// nudges no one, and the second nudges once.
func TestCascadeNudgeWaitsForLastBlocker(t *testing.T) {
	f := newCascadeFixture(t)
	dependent := `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`
	f.write("up-gc-b1.json", dependent)
	f.write("up-gc-b2.json", dependent)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T1"},`+
		`{"id":"gc-b2","dependency_type":"blocks","status":"open"}]`)
	f.closed("gc-b1")
	f.run()
	f.wantNudges(0)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T1"},`+
		`{"id":"gc-b2","dependency_type":"blocks","status":"closed","closed_at":"T2"}]`)
	f.closed("gc-b1", "gc-b2")
	f.run()
	got := f.wantNudges(1)
	if !strings.Contains(got[0], "blocker gc-b2 closed") {
		t.Errorf("the last blocker's close should carry the nudge, got %q", got[0])
	}
}

// TestCascadeNudgeLastBlockerNudgesOnceAcrossRuns pins the dedupe across
// overlapping lookback windows: every run sees the same closes, and the
// dependent hears about it once.
func TestCascadeNudgeLastBlockerNudgesOnceAcrossRuns(t *testing.T) {
	f := newCascadeFixture(t)
	dependent := `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`
	f.write("up-gc-b1.json", dependent)
	f.write("up-gc-b2.json", dependent)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed"},{"id":"gc-b2","dependency_type":"blocks","status":"closed"}]`)
	f.closed("gc-b1", "gc-b2")

	f.run()
	f.run()

	f.wantNudges(1)
}

// TestCascadeNudgeDeferredDependentStillNudged guards the existing behavior
// for deferred dependents.
func TestCascadeNudgeDeferredDependentStillNudged(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"deferred","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed"}]`)
	f.closed("gc-b1")

	f.run()

	got := f.wantNudges(1)
	if !strings.HasPrefix(got[0], "session nudge worker ") {
		t.Errorf("nudge = %q, want it addressed to worker", got[0])
	}
}

// TestCascadeNudgeOtherBlockerClosedAsBlockedWaits pins that a blocker closed
// with gc.work_outcome=blocked still holds its dependent, as
// beads.DependencySatisfied decides.
func TestCascadeNudgeOtherBlockerClosedAsBlockedWaits(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed"},`+
		`{"id":"gc-b2","dependency_type":"blocks","status":"closed","metadata":{"gc.work_outcome":"blocked"}}]`)
	f.closed("gc-b1")

	f.run()

	f.wantNudges(0)
}

// TestCascadeNudgeBlockerClosedAsBlockedNudgesNoOne pins that a blocker's own
// close with gc.work_outcome=blocked does not nudge its dependent.
func TestCascadeNudgeBlockerClosedAsBlockedNudgesNoOne(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","metadata":{"gc.work_outcome":"blocked"}}]`)
	f.closed("gc-b1")

	f.run()

	f.wantNudges(0)
}

// TestCascadeNudgeBlockerClosedMidRunNudgesOnce pins that a blocker which
// closes after a run read its events, but before that run listed the
// dependent's blockers, does not nudge the dependent a second time when a
// later run sees its close.
func TestCascadeNudgeBlockerClosedMidRunNudgesOnce(t *testing.T) {
	f := newCascadeFixture(t)
	dependent := `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`
	f.write("up-gc-b1.json", dependent)
	f.write("up-gc-b2.json", dependent)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed"},{"id":"gc-b2","dependency_type":"blocks","status":"closed"}]`)

	f.closed("gc-b1")
	f.run()
	f.closed("gc-b1", "gc-b2")
	f.run()

	f.wantNudges(1)
}

// TestCascadeNudgeReopenedBlockerNudgesOnReclose pins that a wait is keyed on
// the blocker's close: a blocker that closes, is reopened while the other
// blocker closes, and closes again nudges the dependent it leaves ready.
func TestCascadeNudgeReopenedBlockerNudgesOnReclose(t *testing.T) {
	f := newCascadeFixture(t)
	dependent := `[{"id":"gc-d1","dependency_type":"blocks","status":"in_progress","assignee":"worker"}]`
	f.write("up-gc-b1.json", dependent)
	f.write("up-gc-b2.json", dependent)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T1"},`+
		`{"id":"gc-b2","dependency_type":"blocks","status":"open"}]`)
	f.closed("gc-b1")
	f.run()

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"open"},`+
		`{"id":"gc-b2","dependency_type":"blocks","status":"closed","closed_at":"T2"}]`)
	f.closed("gc-b2")
	f.run()
	f.wantNudges(0)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T3"},`+
		`{"id":"gc-b2","dependency_type":"blocks","status":"closed","closed_at":"T2"}]`)
	f.closed("gc-b1")
	f.run()
	got := f.wantNudges(1)
	if !strings.Contains(got[0], "blocker gc-b1 closed") {
		t.Errorf("the re-close should carry the nudge, got %q", got[0])
	}
}

// TestCascadeNudgeBlockerReclosedAfterBlockedOutcomeNudges pins that a blocker
// closed with gc.work_outcome=blocked, then reopened and closed done, nudges
// its dependent.
func TestCascadeNudgeBlockerReclosedAfterBlockedOutcomeNudges(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T1",`+
		`"metadata":{"gc.work_outcome":"blocked"}}]`)
	f.closed("gc-b1")
	f.run()
	f.wantNudges(0)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T2"}]`)
	f.run()
	f.wantNudges(1)
}

// TestCascadeNudgeOpenConditionalBlocksHoldsDependent pins that an open
// conditional-blocks dependency holds the dependent, as an open blocks
// dependency does.
func TestCascadeNudgeOpenConditionalBlocksHoldsDependent(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed"},`+
		`{"id":"gc-c1","dependency_type":"conditional-blocks","status":"open"}]`)
	f.closed("gc-b1")

	f.run()

	f.wantNudges(0)
}

// TestCascadeNudgeOpenWaitsForDoesNotHoldDependent pins that a waits-for
// dependency is not judged by its target's status: bd gates waits-for on the
// target's children, so an open target can leave the dependent ready.
func TestCascadeNudgeOpenWaitsForDoesNotHoldDependent(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed"},`+
		`{"id":"gc-s1","dependency_type":"waits-for","status":"open"}]`)
	f.closed("gc-b1")

	f.run()

	f.wantNudges(1)
}

// TestCascadeNudgeConditionalBlocksClosingLastNudges pins that the close of
// a conditional-blocks target releases a dependent that waited on it.
func TestCascadeNudgeConditionalBlocksClosingLastNudges(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`)
	f.write("up-gc-c1.json", `[{"id":"gc-d1","dependency_type":"conditional-blocks","status":"open","assignee":"worker"}]`)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T1"},`+
		`{"id":"gc-c1","dependency_type":"conditional-blocks","status":"open"}]`)
	f.closed("gc-b1")
	f.run()
	f.wantNudges(0)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T1"},`+
		`{"id":"gc-c1","dependency_type":"conditional-blocks","status":"closed","closed_at":"T2"}]`)
	f.closed("gc-b1", "gc-c1")
	f.run()
	f.run()
	got := f.wantNudges(1)
	if !strings.Contains(got[0], "blocker gc-c1 closed") {
		t.Errorf("the conditional-blocks close should carry the nudge, got %q", got[0])
	}
}

// TestCascadeNudgeReclosedBlockerRenudgesNudgedDependent pins that a nudge is
// keyed on the blocker's close: a dependent already nudged is nudged again
// when its blocker is reopened and closes again.
func TestCascadeNudgeReclosedBlockerRenudgesNudgedDependent(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T1"}]`)
	f.closed("gc-b1")
	f.run()
	f.wantNudges(1)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"open"}]`)
	f.run()
	f.wantNudges(1)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed","closed_at":"T2"}]`)
	f.run()
	f.run()
	f.wantNudges(2)
}

// TestCascadeNudgeIgnoresNonGatingDependents pins that a dependent linked to
// the closed bead by a type that does not gate on its close is not nudged.
func TestCascadeNudgeIgnoresNonGatingDependents(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"waits-for","status":"open","assignee":"worker"},`+
		`{"id":"gc-d2","dependency_type":"discovered-from","status":"open","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"waits-for","status":"closed"}]`)
	f.write("down-gc-d2.json", `[{"id":"gc-b1","dependency_type":"discovered-from","status":"closed"}]`)
	f.closed("gc-b1")

	f.run()

	f.wantNudges(0)
}

// TestCascadeNudgeIgnoresNonGatingDependencies pins that an open dependency
// of a type Ready() ignores does not hold the dependent.
func TestCascadeNudgeIgnoresNonGatingDependencies(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","dependency_type":"blocks","status":"open","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","dependency_type":"blocks","status":"closed"},`+
		`{"id":"gc-r1","dependency_type":"discovered-from","status":"open"}]`)
	f.closed("gc-b1")

	f.run()

	f.wantNudges(1)
}
