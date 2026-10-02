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
// `gc bd dep list <id> --direction=<direction>`, and it appends every
// `gc session nudge` to nudges.log.
const cascadeFakeGC = `#!/bin/sh
case "$1" in
rig) echo '{"rigs":[]}' ;;
events) cat "$CASCADE_FIX/events.jsonl" 2>/dev/null ;;
bd)
    id="$4"; dir=""
    for a in "$@"; do
        case "$a" in --direction=*) dir="${a#--direction=}" ;; esac
    done
    cat "$CASCADE_FIX/$dir-$id.json" 2>/dev/null || echo '[]'
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
// is nudged. Owners often claim a bead before a blocker is added to it, and
// when that blocker closes the in_progress owner is exactly who needs to
// hear about it.
func TestCascadeNudgeIncludesInProgressDependent(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","status":"in_progress","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","status":"closed"}]`)
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
	dependent := `[{"id":"gc-d1","status":"open","assignee":"worker"}]`
	f.write("up-gc-b1.json", dependent)
	f.write("up-gc-b2.json", dependent)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","status":"closed"},{"id":"gc-b2","status":"open"}]`)
	f.closed("gc-b1")
	f.run()
	f.wantNudges(0)

	f.write("down-gc-d1.json", `[{"id":"gc-b1","status":"closed"},{"id":"gc-b2","status":"closed"}]`)
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
	dependent := `[{"id":"gc-d1","status":"open","assignee":"worker"}]`
	f.write("up-gc-b1.json", dependent)
	f.write("up-gc-b2.json", dependent)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","status":"closed"},{"id":"gc-b2","status":"closed"}]`)
	f.closed("gc-b1", "gc-b2")

	f.run()
	f.run()

	f.wantNudges(1)
}

// TestCascadeNudgeDeferredDependentStillNudged guards the existing behavior
// for deferred dependents.
func TestCascadeNudgeDeferredDependentStillNudged(t *testing.T) {
	f := newCascadeFixture(t)
	f.write("up-gc-b1.json", `[{"id":"gc-d1","status":"deferred","assignee":"worker"}]`)
	f.write("down-gc-d1.json", `[{"id":"gc-b1","status":"closed"}]`)
	f.closed("gc-b1")

	f.run()

	got := f.wantNudges(1)
	if !strings.HasPrefix(got[0], "session nudge worker ") {
		t.Errorf("nudge = %q, want it addressed to worker", got[0])
	}
}
