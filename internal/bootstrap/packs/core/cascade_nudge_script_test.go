package core

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// cascadeFake describes the gc state a cascade-nudge run sees.
type cascadeFake struct {
	// events is the JSONL `gc events` returns.
	events string
	// deps is the JSON array `gc bd dep list` returns for every blocker.
	deps string
	// rigs is the JSON `gc rig list` returns; empty means no output.
	rigs string
	// members maps a session template to the names of its active sessions.
	members map[string][]string
}

// cascadeRun is a fake gc install for the cascade-nudge script.
type cascadeRun struct {
	binDir     string
	logPath    string
	eventsPath string
	// failDir holds one marker file per session name whose nudge must fail.
	failDir string
}

// newCascadeFake installs a fake gc that serves f. `gc session list
// --template T` returns the members of T, and `gc session nudge S` exits 1
// while failDir/S exists.
func newCascadeFake(t *testing.T, f cascadeFake) cascadeRun {
	t.Helper()
	dir := t.TempDir()
	run := cascadeRun{
		eventsPath: filepath.Join(dir, "events.jsonl"),
		failDir:    filepath.Join(dir, "fail"),
	}
	if err := os.WriteFile(run.eventsPath, []byte(f.events), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if err := os.Mkdir(run.failDir, 0o755); err != nil {
		t.Fatalf("mkdir fail dir: %v", err)
	}
	templates := make([]string, 0, len(f.members))
	for tmpl := range f.members {
		templates = append(templates, tmpl)
	}
	sort.Strings(templates)
	var lists strings.Builder
	for _, tmpl := range templates {
		sessions := make([]string, 0, len(f.members[tmpl]))
		for _, name := range f.members[tmpl] {
			sessions = append(sessions, `{"name":"`+name+`"}`)
		}
		lists.WriteString("    '" + tmpl + `') printf '%s\n' '{"sessions":[` + strings.Join(sessions, ",") + `]}' ;;` + "\n")
	}
	rigs := ""
	if f.rigs != "" {
		rigs = "rig) printf '%s\\n' '" + f.rigs + "' ;;\n"
	}
	run.binDir, run.logPath = fakeGCBin(t, `case "$1" in
events) cat '`+run.eventsPath+`' ;;
bd) printf '%s\n' '`+f.deps+`' ;;
`+rigs+`session)
  if [ "$2" = "list" ]; then
    tmpl=""; prev=""
    for a in "$@"; do [ "$prev" = "--template" ] && tmpl="$a"; prev="$a"; done
    case "$tmpl" in
`+lists.String()+`    *) printf '%s\n' '{"sessions":[]}' ;;
    esac
  elif [ "$2" = "nudge" ] && [ -e '`+run.failDir+`/'"$3" ]; then
    exit 1
  fi ;;
esac
exit 0
`)
	return run
}

// nudgeLines returns the logged `gc session nudge` calls and clears the log
// so the next run starts empty.
func (r cascadeRun) nudgeLines(t *testing.T) []string {
	t.Helper()
	logged, err := os.ReadFile(r.logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read gc log: %v", err)
	}
	var nudges []string
	for _, line := range strings.Split(string(logged), "\n") {
		if strings.HasPrefix(line, "gc session nudge ") {
			nudges = append(nudges, line)
		}
	}
	if err := os.WriteFile(r.logPath, nil, 0o644); err != nil {
		t.Fatalf("clear gc log: %v", err)
	}
	return nudges
}

func readCascadeState(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "cascade-nudge-on-blocker-close-state.json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	state := map[string]string{}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode state %q: %v", data, err)
	}
	return state
}

func stateKeys(state map[string]string) []string {
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

const blkClosedEvent = `{"seq":7,"type":"bead.closed","actor":"gc","subject":"ga-blk","payload":{"bead":{"id":"ga-blk","status":"closed"}}}` + "\n"

// TestCascadeNudgeWakesRoutedPoolMember pins the graph.v2 wait shape: a ready
// step carries gc.routed_to (a pool base, the members' template) and an empty
// assignee until a session claims it. Closing its blocker must nudge the
// pool's active member, not the pool base, which names no session.
func TestCascadeNudgeWakesRoutedPoolMember(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	run := newCascadeFake(t, cascadeFake{
		events:  blkClosedEvent,
		deps:    `[{"id":"ga-dep","status":"open","assignee":"","metadata":{"gc.routed_to":"gascity/pool"}}]`,
		members: map[string][]string{"gascity/pool": {"pool-1"}},
	})
	stateDir := filepath.Join(t.TempDir(), "state")
	if out, err := runCascadeNudge(t, run.binDir, stateDir); err != nil {
		t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
	}
	nudges := run.nudgeLines(t)
	want := "gc session nudge pool-1 blocker ga-blk closed — routed dependent ga-dep may be ready to claim"
	if len(nudges) != 1 || nudges[0] != want {
		t.Fatalf("nudges = %q, want exactly [%q]", nudges, want)
	}
	if got := stateKeys(readCascadeState(t, stateDir)); len(got) != 1 || got[0] != "ga-blk|ga-dep" {
		t.Fatalf("state keys = %q, want [ga-blk|ga-dep]", got)
	}
}

// TestCascadeNudgeWakesRoutedUnassignedDependent pins the no-members fallback:
// a routed target with no active template members (a single-session agent or
// an explicit slot) is nudged directly.
func TestCascadeNudgeWakesRoutedUnassignedDependent(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(blkClosedEvent), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	binDir, logPath := fakeGCBin(t, `case "$1" in
events) cat '`+eventsPath+`' ;;
bd) printf '%s\n' '[{"id":"ga-dep","status":"open","assignee":"","metadata":{"gc.routed_to":"worker-pool"}}]' ;;
session) [ "$2" = "list" ] && printf '%s\n' '{"sessions":[]}' ;;
esac
exit 0
`)
	if out, err := runCascadeNudge(t, binDir, filepath.Join(dir, "state")); err != nil {
		t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
	}
	got, logged := countNudges(t, logPath)
	if got != 1 || !strings.Contains(logged, "gc session nudge worker-pool ") {
		t.Fatalf("nudges = %d, want exactly 1 to routed target worker-pool for an unassigned routed dependent; gc calls:\n%s", got, logged)
	}
}

// TestCascadeNudgeRoutedRowEdgeCases pins how the dependent filter treats
// mixed, malformed, and ineligible rows alongside routed ones.
func TestCascadeNudgeRoutedRowEdgeCases(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; cascade-nudge-on-blocker-close.sh requires it")
	}
	const (
		assignedMsg = "gc session nudge worker-1 blocker ga-blk closed — your dependent ga-asg may be unblocked"
		routedMsg   = " blocker ga-blk closed — routed dependent ga-dep may be ready to claim"
	)
	cases := []struct {
		name      string
		events    string
		deps      string
		rigs      string
		members   map[string][]string
		wantNudge []string
		wantState []string
	}{{
		name:      "routed row before assigned row",
		deps:      `[{"id":"ga-dep","status":"open","metadata":{"gc.routed_to":"gascity/pool"}},{"id":"ga-asg","status":"open","assignee":"worker-1"}]`,
		members:   map[string][]string{"gascity/pool": {"pool-1"}},
		wantNudge: []string{"gc session nudge pool-1" + routedMsg, assignedMsg},
		wantState: []string{"ga-blk|ga-asg", "ga-blk|ga-dep"},
	}, {
		name:      "ineligible rows",
		deps:      `[{"id":"ga-none","status":"open"},{"id":"ga-closed","status":"closed","metadata":{"gc.routed_to":"gascity/pool"}},{"id":"ga-busy","status":"in_progress","metadata":{"gc.routed_to":"gascity/pool"}},{"id":"external:x:ga-1","external":true}]`,
		members:   map[string][]string{"gascity/pool": {"pool-1"}},
		wantState: []string{},
	}, {
		name:      "malformed metadata next to assigned row",
		deps:      `[{"id":"ga-str","status":"open","metadata":"gc.routed_to"},{"id":"ga-num","status":"open","metadata":{"gc.routed_to":7}},{"id":"ga-asg","status":"open","assignee":"worker-1"}]`,
		wantNudge: []string{assignedMsg},
		wantState: []string{"ga-blk|ga-asg"},
	}, {
		name:      "pool with two active members",
		deps:      `[{"id":"ga-dep","status":"deferred","metadata":{"gc.routed_to":"gascity/pool"}}]`,
		members:   map[string][]string{"gascity/pool": {"pool-1", "pool-2"}},
		wantNudge: []string{"gc session nudge pool-1" + routedMsg, "gc session nudge pool-2" + routedMsg},
		wantState: []string{"ga-blk|ga-dep"},
	}, {
		name:      "routed target with no active members",
		deps:      `[{"id":"ga-dep","status":"open","metadata":{"gc.routed_to":"gascity/solo"}}]`,
		wantNudge: []string{"gc session nudge gascity/solo" + routedMsg},
		wantState: []string{"ga-blk|ga-dep"},
	}, {
		name:      "dependent rig suspended",
		events:    `{"seq":7,"type":"bead.closed","subject":"hq-blk","payload":{"bead":{"id":"hq-blk","status":"closed"}}}` + "\n",
		deps:      `[{"id":"ga-dep","status":"open","metadata":{"gc.routed_to":"gascity/pool"}}]`,
		rigs:      `{"rigs":[{"name":"gascity","prefix":"ga","suspended":true}]}`,
		members:   map[string][]string{"gascity/pool": {"pool-1"}},
		wantState: []string{},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := tc.events
			if events == "" {
				events = blkClosedEvent
			}
			run := newCascadeFake(t, cascadeFake{events: events, deps: tc.deps, rigs: tc.rigs, members: tc.members})
			stateDir := filepath.Join(t.TempDir(), "state")
			if out, err := runCascadeNudge(t, run.binDir, stateDir); err != nil {
				t.Fatalf("cascade-nudge-on-blocker-close.sh failed: %v\n%s", err, out)
			}
			if got := run.nudgeLines(t); strings.Join(got, "\n") != strings.Join(tc.wantNudge, "\n") {
				t.Fatalf("nudges = %q, want %q", got, tc.wantNudge)
			}
			if got := stateKeys(readCascadeState(t, stateDir)); strings.Join(got, ",") != strings.Join(tc.wantState, ",") {
				t.Fatalf("state keys = %q, want %q", got, tc.wantState)
			}
		})
	}
}
