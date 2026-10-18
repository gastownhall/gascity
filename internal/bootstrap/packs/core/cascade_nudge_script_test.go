package core

import (
	"encoding/json"
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
// window holds both. The dependent must still be nudged exactly once —
// whether it is nudged at an assignee or, since #6020 left routed steps
// unassigned until a session claims them, at its gc.routed_to target.
func TestCascadeNudgeAbsorbsDuplicateCloseEvents(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}

	cases := []struct {
		name    string
		depJSON string
		members map[string][]string
		wantTo  string
	}{
		{
			name:    "assigned dependent",
			depJSON: `[{"id":"ga-dep","status":"open","assignee":"worker-1"}]`,
			wantTo:  "worker-1",
		},
		{
			name:    "routed unassigned dependent",
			depJSON: `[{"id":"ga-dep","status":"open","assignee":"","metadata":{"gc.routed_to":"gascity/pool"}}]`,
			members: map[string][]string{"gascity/pool": {"pool-1"}},
			wantTo:  "pool-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			eventsPath := filepath.Join(dir, "events.jsonl")
			binDir, logPath := fakeGCBin(t, cascadeFake{
				eventsPath: eventsPath,
				depJSON:    tc.depJSON,
				members:    tc.members,
				failDir:    dir,
			}.body())
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
				got, logged := countNudges(t, logPath)
				if got != 1 || countNudgesTo(t, logPath, tc.wantTo) != 1 {
					t.Fatalf("after run %d: nudges = %d (to %s = %d), want exactly 1 to %s for one close delivered twice; gc calls:\n%s",
						i+1, got, tc.wantTo, countNudgesTo(t, logPath, tc.wantTo), tc.wantTo, logged)
				}
			}
			_, logged := countNudges(t, logPath)
			if !strings.Contains(logged, "gc session nudge "+tc.wantTo+" blocker ga-blk closed") {
				t.Fatalf("the nudge must reach %s and name the blocker; gc calls:\n%s", tc.wantTo, logged)
			}
		})
	}
}

// cascadeFake describes the fake `gc` the routed-dependent tests need: the
// JSON array `gc bd dep list` answers with (the blocker's dependents, and
// each dependent's own blockers), the active members
// `gc session list` reports per --template, what `gc rig list` answers, and
// a dir whose marker files make a `gc session nudge <target>` exit 1.
type cascadeFake struct {
	eventsPath string
	depJSON    string
	// downJSON is what a dependent's blocker read (`gc bd dep list
	// --direction=down`) answers; empty answers [], no blocker listed, so
	// nothing else holds the dependent.
	downJSON string
	members  map[string][]string
	rigJSON  string
	failDir  string
}

// body renders the fake into the shell fragment fakeGCBin runs after logging
// its argv. Session-list answers only a matching --template; every nudge to
// a target with a marker file in failDir fails without logging success.
func (f cascadeFake) body() string {
	var b strings.Builder
	b.WriteString("case \"$1\" in\n")
	b.WriteString("events) cat '" + f.eventsPath + "' ;;\n")
	down := f.downJSON
	if down == "" {
		down = "[]"
	}
	b.WriteString("bd) case \"$*\" in\n")
	b.WriteString("    *direction=down*) printf '%s\\n' '" + down + "' ;;\n")
	b.WriteString("    *) printf '%s\\n' '" + f.depJSON + "' ;;\n")
	b.WriteString("    esac ;;\n")
	if f.rigJSON != "" {
		b.WriteString("rig) printf '%s\\n' '" + f.rigJSON + "' ;;\n")
	}
	b.WriteString("session)\n")
	b.WriteString("  if [ \"$2\" = \"list\" ]; then\n")
	b.WriteString("    tmpl=''\n")
	b.WriteString("    shift 2\n")
	b.WriteString("    while [ $# -gt 0 ]; do\n")
	b.WriteString("      if [ \"$1\" = \"--template\" ] && [ $# -ge 2 ]; then tmpl=\"$2\"; break; fi\n")
	b.WriteString("      shift\n")
	b.WriteString("    done\n")
	b.WriteString("    case \"$tmpl\" in\n")
	for tmpl, names := range f.members {
		sessions := make([]string, 0, len(names))
		for _, name := range names {
			sessions = append(sessions, `{"name":"`+name+`"}`)
		}
		b.WriteString("      '" + tmpl + "') printf '%s\\n' '{\"sessions\":[" + strings.Join(sessions, ",") + "]}' ;;\n")
	}
	b.WriteString("      *) printf '%s\\n' '{\"sessions\":[]}' ;;\n")
	b.WriteString("    esac\n")
	b.WriteString("  elif [ \"$2\" = \"nudge\" ]; then\n")
	b.WriteString("    tgt=\"$3\"\n")
	b.WriteString("    if [ \"$3\" = \"--rig\" ]; then tgt=\"$5\"; fi\n")
	b.WriteString("    if [ -e '" + f.failDir + "/'$tgt ]; then exit 1; fi\n")
	b.WriteString("  fi\n")
	b.WriteString("  ;;\n")
	b.WriteString("esac\n")
	b.WriteString("exit 0\n")
	return b.String()
}

// writeCascadeCloseEvent writes one bead.closed event for blocker and
// returns the events file path.
func writeCascadeCloseEvent(t *testing.T, blocker string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "events.jsonl")
	line := fmt.Sprintf(`{"seq":7,"type":"bead.closed","actor":"gc","subject":%q,"payload":{"bead":{"id":%q,"status":"closed"}}}`+"\n", blocker, blocker)
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	return p
}

// countNudgesTo counts the logged `gc session nudge <target> ` calls.
func countNudgesTo(t *testing.T, logPath, target string) int {
	t.Helper()
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(logged), "\n") {
		if strings.HasPrefix(line, "gc session nudge "+target+" ") {
			n++
		}
	}
	return n
}

// cascadeStateHasKey reports whether the order's dedup state records the
// given "<blocker>|<dependent>" pair.
func cascadeStateHasKey(t *testing.T, stateDir, key string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "cascade-nudge-on-blocker-close-state.json"))
	if err != nil {
		return false
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse cascade-nudge state: %v\n%s", err, data)
	}
	_, ok := state[key]
	return ok
}

// TestCascadeNudgeWakesRoutedPoolMember pins the graph.v2 wake-up for routed
// dependents. Since #6020 a ready step carries gc.routed_to and an empty
// assignee until a session claims it, so an assignee-only filter drops
// exactly the dependents a blocker close frees. The routed target is a pool
// BASE (the members' template, not a session name), so the wake-up must
// resolve members by template — a naive `gc session nudge <routed_to>`
// silently no-ops for the warm-idle pool this order exists to wake.
func TestCascadeNudgeWakesRoutedPoolMember(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}

	t.Run("resolves the pool base to its active member", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		binDir, logPath := fakeGCBin(t, cascadeFake{
			eventsPath: writeCascadeCloseEvent(t, "ga-blk"),
			depJSON:    `[{"id":"ga-dep","status":"open","assignee":"","metadata":{"gc.routed_to":"gascity/pool"}}]`,
			members:    map[string][]string{"gascity/pool": {"pool-1"}},
			failDir:    dir,
		}.body())
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
		}
		got, logged := countNudges(t, logPath)
		if got != 1 {
			t.Fatalf("nudges = %d, want exactly 1 for one routed dependent; gc calls:\n%s", got, logged)
		}
		if !strings.Contains(logged, "gc session nudge pool-1 blocker ga-blk closed — routed dependent ga-dep") {
			t.Fatalf("the nudge must reach the resolved pool member and name blocker and dependent; gc calls:\n%s", logged)
		}
		if strings.Contains(logged, "gc session nudge gascity/pool ") {
			t.Fatalf("the pool base is not a session name; the nudge must reach the member, not gascity/pool; gc calls:\n%s", logged)
		}
		if !cascadeStateHasKey(t, stateDir, "ga-blk|ga-dep") {
			t.Fatal("the (blocker, dependent) pair must be recorded so the nudge is not repeated")
		}
	})

	t.Run("nudges the routed target directly when it has no active members", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		binDir, logPath := fakeGCBin(t, cascadeFake{
			eventsPath: writeCascadeCloseEvent(t, "ga-blk"),
			depJSON:    `[{"id":"ga-dep","status":"open","assignee":"","metadata":{"gc.routed_to":"worker-pool"}}]`,
			failDir:    dir,
		}.body())
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
		}
		got, logged := countNudges(t, logPath)
		if got != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to the routed target itself; gc calls:\n%s", got, logged)
		}
		if !strings.Contains(logged, "gc session nudge worker-pool blocker ga-blk closed — routed dependent ga-dep") {
			t.Fatalf("a target with no members is a single-session agent or slot; nudge it directly; gc calls:\n%s", logged)
		}
		if !cascadeStateHasKey(t, stateDir, "ga-blk|ga-dep") {
			t.Fatal("the (blocker, dependent) pair must be recorded so the nudge is not repeated")
		}
	})

	t.Run("nudges every active member and records the pair once", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		binDir, logPath := fakeGCBin(t, cascadeFake{
			eventsPath: writeCascadeCloseEvent(t, "ga-blk"),
			depJSON:    `[{"id":"ga-dep","status":"open","assignee":"","metadata":{"gc.routed_to":"gascity/pool"}}]`,
			members:    map[string][]string{"gascity/pool": {"pool-1", "pool-2"}},
			failDir:    dir,
		}.body())
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
		}
		if got, logged := countNudges(t, logPath); got != 2 {
			t.Fatalf("nudges = %d, want one per pool member; gc calls:\n%s", got, logged)
		}
		if n := countNudgesTo(t, logPath, "pool-1"); n != 1 {
			t.Errorf("nudges to pool-1 = %d, want 1", n)
		}
		if n := countNudgesTo(t, logPath, "pool-2"); n != 1 {
			t.Errorf("nudges to pool-2 = %d, want 1", n)
		}
		if !cascadeStateHasKey(t, stateDir, "ga-blk|ga-dep") {
			t.Fatal("the (blocker, dependent) pair must be recorded")
		}
	})

	t.Run("mixed rows each get their own delivery", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		binDir, logPath := fakeGCBin(t, cascadeFake{
			eventsPath: writeCascadeCloseEvent(t, "ga-blk"),
			depJSON:    `[{"id":"ga-dep","status":"open","assignee":"","metadata":{"gc.routed_to":"gascity/pool"}},{"id":"ga-as","status":"open","assignee":"worker-1"}]`,
			members:    map[string][]string{"gascity/pool": {"pool-1"}},
			failDir:    dir,
		}.body())
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
		}
		if got, logged := countNudges(t, logPath); got != 2 {
			t.Fatalf("nudges = %d, want one per dependent; gc calls:\n%s", got, logged)
		} else {
			if !strings.Contains(logged, "gc session nudge pool-1 blocker ga-blk closed — routed dependent ga-dep") {
				t.Errorf("the routed row must keep the route message; gc calls:\n%s", logged)
			}
			if !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed — your dependent ga-as") {
				t.Errorf("the assigned row must keep the assignee message; gc calls:\n%s", logged)
			}
		}
		if !cascadeStateHasKey(t, stateDir, "ga-blk|ga-dep") || !cascadeStateHasKey(t, stateDir, "ga-blk|ga-as") {
			t.Fatal("both (blocker, dependent) pairs must be recorded")
		}
	})

	t.Run("skips dependents with nowhere to nudge", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		binDir, logPath := fakeGCBin(t, cascadeFake{
			eventsPath: writeCascadeCloseEvent(t, "ga-blk"),
			depJSON: `[` + strings.Join([]string{
				`{"id":"ga-none","status":"open"}`,
				`{"id":"ga-closed","status":"closed","metadata":{"gc.routed_to":"gascity/pool"}}`,
				`{"id":"ga-wip","status":"in_progress","metadata":{"gc.routed_to":"gascity/pool"}}`,
				`{"id":"external:x","external":true}`,
			}, ",") + `]`,
			members: map[string][]string{"gascity/pool": {"pool-1"}},
			failDir: dir,
		}.body())
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
		}
		if got, logged := countNudges(t, logPath); got != 0 {
			t.Fatalf("nudges = %d, want 0: no unblocked dependent has an assignee or route; gc calls:\n%s", got, logged)
		}
		for _, key := range []string{"ga-blk|ga-none", "ga-blk|ga-closed", "ga-blk|ga-wip", "ga-blk|external:x"} {
			if cascadeStateHasKey(t, stateDir, key) {
				t.Errorf("pair %q must not be recorded; nothing was nudged", key)
			}
		}
	})

	t.Run("skips malformed metadata, still nudges the assigned dependent", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		binDir, logPath := fakeGCBin(t, cascadeFake{
			eventsPath: writeCascadeCloseEvent(t, "ga-blk"),
			depJSON: `[` + strings.Join([]string{
				`{"id":"ga-str","status":"open","metadata":"oops"}`,
				`{"id":"ga-num","status":"open","metadata":{"gc.routed_to":7}}`,
				`{"id":"ga-ok","status":"open","assignee":"worker-1"}`,
			}, ",") + `]`,
			members: map[string][]string{"gascity/pool": {"pool-1"}},
			failDir: dir,
		}.body())
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
		}
		got, logged := countNudges(t, logPath)
		if got != 1 {
			t.Fatalf("nudges = %d, want 1: a malformed row must not empty the blocker's rows; gc calls:\n%s", got, logged)
		}
		if !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed") {
			t.Fatalf("the assigned sibling must still be nudged; gc calls:\n%s", logged)
		}
		if cascadeStateHasKey(t, stateDir, "ga-blk|ga-str") || cascadeStateHasKey(t, stateDir, "ga-blk|ga-num") {
			t.Fatal("skipped rows must not be recorded")
		}
		if !cascadeStateHasKey(t, stateDir, "ga-blk|ga-ok") {
			t.Fatal("the nudged pair must be recorded")
		}
	})

	t.Run("leaves a dependent in a suspended rig for its resume", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		binDir, logPath := fakeGCBin(t, cascadeFake{
			eventsPath: writeCascadeCloseEvent(t, "gb-blk"),
			depJSON:    `[{"id":"ga-dep","status":"open","assignee":"","metadata":{"gc.routed_to":"gascity/pool"}}]`,
			rigJSON:    `{"rigs":[{"prefix":"gb","name":"gb-rig"},{"prefix":"ga","name":"ga-rig","suspended":true}]}`,
			members:    map[string][]string{"gascity/pool": {"pool-1"}},
			failDir:    dir,
		}.body())
		if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
			t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
		}
		if got, logged := countNudges(t, logPath); got != 0 {
			t.Fatalf("nudges = %d, want 0: a suspended rig is left cold; gc calls:\n%s", got, logged)
		}
		if cascadeStateHasKey(t, stateDir, "gb-blk|ga-dep") {
			t.Fatal("the pair must not be recorded; the dependent waits for its rig to resume")
		}
	})
}

// TestCascadeNudgeKeepsAssigneePath pins the assigned-dependent delivery the
// order has always done: an open dependent with an assignee is nudged at
// that assignee with the unchanged message — whether or not it also carries
// gc.routed_to, since a claimed bead belongs to its session and the route is
// history — and without a pool lookup.
func TestCascadeNudgeKeepsAssigneePath(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}

	cases := []struct {
		name string
		row  string
	}{
		{"assignee only", `{"id":"ga-dep","status":"open","assignee":"worker-1"}`},
		{"assignee wins over a route", `{"id":"ga-dep","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"gascity/pool"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stateDir := filepath.Join(dir, "state")
			binDir, logPath := fakeGCBin(t, cascadeFake{
				eventsPath: writeCascadeCloseEvent(t, "ga-blk"),
				depJSON:    "[" + tc.row + "]",
				members:    map[string][]string{"gascity/pool": {"pool-1"}},
				failDir:    dir,
			}.body())
			if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
				t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
			}
			got, logged := countNudges(t, logPath)
			if got != 1 {
				t.Fatalf("nudges = %d, want exactly 1 to the assignee; gc calls:\n%s", got, logged)
			}
			if !strings.Contains(logged, "gc session nudge worker-1 blocker ga-blk closed — your dependent ga-dep may be unblocked") {
				t.Fatalf("the assigned dependent must keep the assignee nudge and message; gc calls:\n%s", logged)
			}
			if strings.Contains(logged, "gc session list") {
				t.Fatalf("an assigned dependent needs no pool lookup; gc calls:\n%s", logged)
			}
			if strings.Contains(logged, "gc session nudge pool-1") {
				t.Fatalf("a claimed bead belongs to its session; the route must not fire; gc calls:\n%s", logged)
			}
			if !cascadeStateHasKey(t, stateDir, "ga-blk|ga-dep") {
				t.Fatal("the (blocker, dependent) pair must be recorded")
			}
		})
	}
}

// TestCascadeNudgeRetriesFailedRoutedNudge pins the failure contract for the
// routed wake-up. An undeliverable nudge must not abort the run (a sibling
// dependent of the same blocker still gets theirs), must not be recorded as
// nudged, and is retried exactly once on the next run.
func TestCascadeNudgeRetriesFailedRoutedNudge(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	failMarker := filepath.Join(dir, "a-1")
	binDir, logPath := fakeGCBin(t, cascadeFake{
		eventsPath: writeCascadeCloseEvent(t, "ga-blk"),
		depJSON: `[` + strings.Join([]string{
			`{"id":"ga-dep-a","status":"open","assignee":"","metadata":{"gc.routed_to":"gascity/pool-a"}}`,
			`{"id":"ga-dep-b","status":"open","assignee":"","metadata":{"gc.routed_to":"gascity/pool-b"}}`,
		}, ",") + `]`,
		members: map[string][]string{
			"gascity/pool-a": {"a-1"},
			"gascity/pool-b": {"b-1"},
		},
		failDir: dir,
	}.body())

	if err := os.WriteFile(failMarker, nil, 0o644); err != nil {
		t.Fatalf("write fail marker: %v", err)
	}
	if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
		t.Fatalf("run 1 must absorb the failed nudge and exit 0: %v\n%s", err, out)
	}
	if n := countNudgesTo(t, logPath, "b-1"); n != 1 {
		t.Errorf("run 1: nudges to b-1 = %d, want 1; a failed nudge must not block the sibling dependent", n)
	}
	if cascadeStateHasKey(t, stateDir, "ga-blk|ga-dep-a") {
		t.Error("the failed pair must not be recorded as nudged")
	}
	if !cascadeStateHasKey(t, stateDir, "ga-blk|ga-dep-b") {
		t.Error("the delivered pair must be recorded")
	}

	if err := os.Remove(failMarker); err != nil {
		t.Fatalf("remove fail marker: %v", err)
	}
	if out, err := runCascadeNudge(t, binDir, stateDir); err != nil {
		t.Fatalf("run 2: cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
	}
	if n := countNudgesTo(t, logPath, "a-1"); n != 2 {
		t.Errorf("nudges to a-1 = %d, want 2 (one failed attempt, one retry)", n)
	}
	if n := countNudgesTo(t, logPath, "b-1"); n != 1 {
		t.Errorf("nudges to b-1 = %d, want 1; the recorded pair must not be re-nudged", n)
	}
	if !cascadeStateHasKey(t, stateDir, "ga-blk|ga-dep-a") {
		t.Error("the retried pair must be recorded once delivered")
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

// A dependent another open blocker still holds is not nudged: its assignee
// has nothing to do yet, and a nudge would wake an asleep seat parked on it
// for nothing (ga-ylpbsy.16, RCA R4). The seat wakes on its own once the
// dependent is ready.
func TestCascadeNudgeSkipsADependentStillBlocked(t *testing.T) {
	for _, status := range []string{"open", "in_progress", "blocked", "deferred", "hooked"} {
		t.Run(status, func(t *testing.T) {
			down := `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"` + status + `"}]'`
			if got, logged := cascadeNudgeOnce(t, down); got != 0 {
				t.Fatalf("nudges = %d, want 0 while ga-other is %s; gc calls:\n%s", got, status, logged)
			}
		})
	}
}

// The close of a dependent's last open blocker nudges its assignee once.
func TestCascadeNudgeNudgesOnTheLastBlockersClose(t *testing.T) {
	for name, down := range map[string]string{
		"every blocker closed":                      `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"closed"}]'`,
		"the other blocker pinned (bd: satisfied)":  `printf '%s\n' '[{"id":"ga-blk","status":"closed"},{"id":"ga-other","status":"pinned"}]'`,
		"the closing blocker read before its close": `printf '%s\n' '[{"id":"ga-blk","status":"open"}]'`,
		"no blocker listed":                         `printf '%s\n' '[]'`,
		// A read that fails proves nothing: the nudge goes out, as before.
		"a failed blocker read":       `exit 1`,
		"an unparseable blocker read": `printf 'not json\n'`,
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
