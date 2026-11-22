package core

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// nudgeOnRouteScriptPath is the on-disk copy of the nudge-on-route order's
// script. The test runs it, so it needs a real path.
const nudgeOnRouteScriptPath = "assets/scripts/nudge-on-route.sh"

// nudgeOnRouteEnvKeys are the settings that would point the script at a live
// session's city, state, or trace file. The test supplies its own state dir.
var nudgeOnRouteEnvKeys = map[string]struct{}{
	"GC_CITY":                        {},
	"GC_CITY_RUNTIME_DIR":            {},
	"GC_PACK_STATE_DIR":              {},
	"GC_BD_TRACE_JSON":               {},
	"GC_NUDGE_ON_ROUTE_WINDOW":       {},
	"GC_NUDGE_ON_ROUTE_LOOKBACK":     {},
	"GC_NUDGE_ON_ROUTE_MAX_LOOKBACK": {},
	"GC_NUDGE_ON_ROUTE_RETENTION":    {},
	"GC_NUDGE_ON_ROUTE_MESSAGE":      {},
}

// nudgeAgo renders a fixture event timestamp the filtering fake gc compares
// `--since` against: RFC3339 UTC, the given age ago.
func nudgeAgo(age time.Duration) string {
	return time.Now().UTC().Add(-age).Format(time.RFC3339)
}

// nudgeEvent renders one fixture event line: seq, type, timestamp, bead id,
// and routed target ("" for none), nested in the API envelope's payload.bead
// unless flat is set (the events.jsonl fallback shape, #5968).
func nudgeEvent(seq int, typ, ts, id, routedTo string, flat bool) string {
	bead := `{"id":"` + id + `","status":"open"`
	if routedTo != "" {
		bead += `,"metadata":{"gc.routed_to":"` + routedTo + `"}`
	}
	bead += `}`
	payload := bead
	if !flat {
		payload = `{"bead":` + bead + `}`
	}
	return fmt.Sprintf(`{"seq":%d,"type":%q,"actor":"gc","subject":%q,"ts":%q,"payload":%s}`,
		seq, typ, id, ts, payload)
}

// nudgeFixture writes the events file behind a filtering fake gc, seeds the
// optional high-water seq mark, and returns the fake's bin dir, its log, and
// the state dir. members maps a pool template to its live session names, as
// `gc session list --template` answers them.
func nudgeFixture(t *testing.T, seedSeq string, members map[string][]string, events ...string) (binDir, logPath, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(strings.Join(events, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	stateDir = filepath.Join(dir, "state")
	if seedSeq != "" {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatalf("mkdir state: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "nudge-on-route-seq"), []byte(seedSeq+"\n"), 0o644); err != nil {
			t.Fatalf("seed seq: %v", err)
		}
	}
	binDir, logPath = fakeGCBin(t, cascadeFake{
		eventsPath:   eventsPath,
		failDir:      dir,
		members:      members,
		filterEvents: true,
	}.body())
	return binDir, logPath, stateDir
}

// runNudgeOnRoute runs the script once against the fixture's state dir plus
// extra env, and returns its combined output.
func runNudgeOnRoute(t *testing.T, binDir, stateDir string, extraEnv ...string) (string, error) {
	t.Helper()
	return runPackScript(t, nudgeOnRouteScriptPath, binDir, nudgeOnRouteEnvKeys,
		append([]string{"GC_PACK_STATE_DIR=" + stateDir}, extraEnv...))
}

// nudgeReadSeq reads the persisted high-water seq mark, or "" when absent.
func nudgeReadSeq(t *testing.T, stateDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "nudge-on-route-seq"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// nudgeSinceArgs lists the --since argument of each logged gc events call,
// in order, so a test can pin which windows a run read.
func nudgeSinceArgs(t *testing.T, logPath string) []string {
	t.Helper()
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	var got []string
	for _, line := range strings.Split(string(logged), "\n") {
		if !strings.HasPrefix(line, "gc events ") {
			continue
		}
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "--since" {
				got = append(got, fields[i+1])
			}
		}
	}
	return got
}

// nudgeStateHasKey reports whether the dedup state records the
// "<bead>|<routed_to>" pair.
func nudgeStateHasKey(t *testing.T, stateDir, key string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "nudge-on-route-state.json"))
	if err != nil {
		return false
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse nudge-on-route state: %v\n%s", err, data)
	}
	_, ok := state[key]
	return ok
}

// TestNudgeOnRouteMatchesEveryBeadUpdatedShape pins the order against both
// wire shapes a bead.updated payload takes. `gc events` serves the typed API
// envelope, whose payload is {"bead":{...}} (the registered BeadEventPayload
// contract); the raw bead snapshot that CachingStore.notifyChange stores in
// .gc/events.jsonl is flat. The shape has flipped before (#4548), and the
// order exits 0 without a nudge when its selector matches nothing, so a
// consumer that reads only one shape disables the order silently. Each case
// feeds one captured event through the script and expects exactly one nudge.
func TestNudgeOnRouteMatchesEveryBeadUpdatedShape(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	cases := []struct {
		name   string
		events []string
		wantTo string
	}{
		{
			name:   "nested payload.bead, as gc events serves it",
			events: []string{`{"seq":1,"type":"bead.updated","actor":"gc","subject":"ga-routed","payload":{"bead":{"id":"ga-routed","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}}}`},
			wantTo: "gascity/builder",
		},
		{
			name:   "flat payload, as events.jsonl stores it",
			events: []string{`{"seq":1,"type":"bead.updated","actor":"gc","subject":"ga-routed","payload":{"id":"ga-routed","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}}`},
			wantTo: "gascity/builder",
		},
		{
			name: "one routing seen in both shapes is nudged once",
			events: []string{
				`{"seq":1,"type":"bead.updated","actor":"gc","subject":"ga-routed","payload":{"bead":{"id":"ga-routed","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}}}`,
				`{"seq":2,"type":"bead.updated","actor":"gc","subject":"ga-routed","payload":{"id":"ga-routed","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}}`,
			},
			wantTo: "gascity/builder",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			eventsPath := filepath.Join(dir, "events.jsonl")
			if err := os.WriteFile(eventsPath, []byte(strings.Join(tc.events, "\n")+"\n"), 0o644); err != nil {
				t.Fatalf("write events: %v", err)
			}
			binDir, logPath := fakeGCBin(t, cascadeFake{eventsPath: eventsPath, failDir: dir}.body())
			stateDir := filepath.Join(dir, "state")

			out, err := runPackScript(t, nudgeOnRouteScriptPath, binDir, nudgeOnRouteEnvKeys,
				[]string{"GC_PACK_STATE_DIR=" + stateDir})
			if err != nil {
				t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
			}
			got, logged := countNudges(t, logPath)
			if got != 1 || countNudgesTo(t, logPath, tc.wantTo) != 1 {
				t.Fatalf("nudges = %d (to %s = %d), want exactly 1 to %s; gc calls:\n%s",
					got, tc.wantTo, countNudgesTo(t, logPath, tc.wantTo), tc.wantTo, logged)
			}
		})
	}
}

// TestNudgeOnRouteIgnoresUnroutedBeads keeps the widened selector from
// matching a bead.updated event that carries no routing target, in either
// shape.
func TestNudgeOnRouteIgnoresUnroutedBeads(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	events := strings.Join([]string{
		`{"seq":1,"type":"bead.updated","payload":{"bead":{"id":"ga-a","status":"open","metadata":{}}}}`,
		`{"seq":2,"type":"bead.updated","payload":{"bead":{"id":"ga-b","status":"open","metadata":{"gc.routed_to":""}}}}`,
		`{"seq":3,"type":"bead.updated","payload":{"id":"ga-c","status":"open"}}`,
		`{"seq":4,"type":"bead.updated","payload":{"id":"ga-d","status":"open","metadata":{"gc.routed_to":""}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(eventsPath, []byte(events), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	binDir, logPath := fakeGCBin(t, cascadeFake{eventsPath: eventsPath, failDir: dir}.body())

	out, err := runPackScript(t, nudgeOnRouteScriptPath, binDir, nudgeOnRouteEnvKeys,
		[]string{"GC_PACK_STATE_DIR=" + filepath.Join(dir, "state")})
	if err != nil {
		t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
	}
	if got, logged := countNudges(t, logPath); got != 0 {
		t.Fatalf("nudges = %d, want 0: no event carries a routing target; gc calls:\n%s", got, logged)
	}
}

// TestNudgeOnRouteSeqCutAndCreatedEvents pins the persisted high-water seq
// cut: a routing stamped only in a bead.created payload is nudged, a run never
// re-reads events at or below the previous run's mark, non-bead events are
// ignored, and a mark above the event log's head (log reset or rebuilt) is
// reset rather than silencing the order until seq catches up.
func TestNudgeOnRouteSeqCutAndCreatedEvents(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	// setup writes events and an optional pre-seeded seq mark, and returns
	// a fake gc, its log, and the state dir.
	setup := func(t *testing.T, seed string, events ...string) (binDir, logPath, stateDir string) {
		t.Helper()
		dir := t.TempDir()
		eventsPath := filepath.Join(dir, "events.jsonl")
		if err := os.WriteFile(eventsPath, []byte(strings.Join(events, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write events: %v", err)
		}
		stateDir = filepath.Join(dir, "state")
		if seed != "" {
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatalf("mkdir state: %v", err)
			}
			if err := os.WriteFile(filepath.Join(stateDir, "nudge-on-route-seq"), []byte(seed+"\n"), 0o644); err != nil {
				t.Fatalf("seed seq: %v", err)
			}
		}
		binDir, logPath = fakeGCBin(t, cascadeFake{eventsPath: eventsPath, failDir: dir}.body())
		return binDir, logPath, stateDir
	}
	run := func(t *testing.T, binDir, stateDir string) {
		t.Helper()
		out, err := runPackScript(t, nudgeOnRouteScriptPath, binDir, nudgeOnRouteEnvKeys,
			[]string{"GC_PACK_STATE_DIR=" + stateDir})
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
	}
	readSeq := func(t *testing.T, stateDir string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(stateDir, "nudge-on-route-seq"))
		if err != nil {
			t.Fatalf("read seq mark: %v", err)
		}
		return strings.TrimSpace(string(data))
	}

	t.Run("bead.created routing is nudged once and the mark advances", func(t *testing.T) {
		binDir, logPath, stateDir := setup(t, "",
			`{"seq":5,"type":"bead.created","actor":"gc","subject":"ga-new","payload":{"bead":{"id":"ga-new","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}}}`)
		run(t, binDir, stateDir)
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/builder; gc calls:\n%s", got, logged)
		}
		if got := readSeq(t, stateDir); got != "5" {
			t.Fatalf("seq mark = %q, want 5", got)
		}

		// Second run over the same events: the seq cut alone must suppress
		// the nudge, so drop the dedup state that would otherwise mask it.
		if err := os.Remove(filepath.Join(stateDir, "nudge-on-route-state.json")); err != nil {
			t.Fatalf("remove dedup state: %v", err)
		}
		run(t, binDir, stateDir)
		if got, logged := countNudges(t, logPath); got != 1 {
			t.Fatalf("nudges after rerun = %d, want still 1; gc calls:\n%s", got, logged)
		}
	})

	t.Run("routing at or below the persisted mark is skipped", func(t *testing.T) {
		// The unrouted seq 10 keeps the log head at the mark, so this is the
		// seq cut and not the backwards-seq reset.
		binDir, logPath, stateDir := setup(t, "10",
			`{"seq":7,"type":"bead.updated","actor":"gc","subject":"ga-old","payload":{"bead":{"id":"ga-old","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}}}`,
			`{"seq":10,"type":"bead.updated","actor":"gc","subject":"ga-x","payload":{"bead":{"id":"ga-x","status":"open","metadata":{}}}}`)
		run(t, binDir, stateDir)
		if got, logged := countNudges(t, logPath); got != 0 {
			t.Fatalf("nudges = %d, want 0 for seq 7 <= mark 10; gc calls:\n%s", got, logged)
		}
	})

	t.Run("non-bead event with a routing-shaped payload is ignored", func(t *testing.T) {
		binDir, logPath, stateDir := setup(t, "",
			`{"seq":3,"type":"session.started","actor":"gc","subject":"ga-s","payload":{"bead":{"id":"ga-s","metadata":{"gc.routed_to":"gascity/builder"}}}}`)
		run(t, binDir, stateDir)
		if got, logged := countNudges(t, logPath); got != 0 {
			t.Fatalf("nudges = %d, want 0 for a session.started event; gc calls:\n%s", got, logged)
		}
	})

	t.Run("mark above the log head is reset", func(t *testing.T) {
		binDir, logPath, stateDir := setup(t, "1000",
			`{"seq":3,"type":"bead.updated","actor":"gc","subject":"ga-r","payload":{"bead":{"id":"ga-r","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}}}`)
		run(t, binDir, stateDir)
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/builder after reset; gc calls:\n%s", got, logged)
		}
		if got := readSeq(t, stateDir); got != "3" {
			t.Fatalf("seq mark = %q, want 3 after reset", got)
		}
	})
}

// TestNudgeOnRouteNudgesRoutingStampedAtCreation pins the #4382 fix's core
// case: a bead whose gc.routed_to is stamped in its create payload (gc order
// run, formula pours, poured step beads) emits bead.created and nothing else
// before it is claimed. A created-only routing older than the fast lookback —
// the claim's 11-minute case — must still be nudged exactly once and
// recorded, in both payload shapes; only the catch-up window bounds how far
// back a run without a persisted mark reaches.
func TestNudgeOnRouteNudgesRoutingStampedAtCreation(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	run := func(t *testing.T, events ...string) (logPath, stateDir string) {
		t.Helper()
		binDir, logPath, stateDir := nudgeFixture(t, "", nil, events...)
		out, err := runNudgeOnRoute(t, binDir, stateDir, "GC_NUDGE_ON_ROUTE_LOOKBACK=2m")
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
		return logPath, stateDir
	}

	t.Run("nested bead.created payload", func(t *testing.T) {
		logPath, stateDir := run(t,
			nudgeEvent(1, "bead.created", nudgeAgo(10*time.Minute), "ga-born", "gascity/builder", false))
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/builder for a created-only routing 10m old; gc calls:\n%s", got, logged)
		}
		if !nudgeStateHasKey(t, stateDir, "ga-born|gascity/builder") {
			t.Fatal("the (bead, routed_to) pair must be recorded so the nudge is not repeated")
		}
	})

	t.Run("flat bead.created payload", func(t *testing.T) {
		logPath, stateDir := run(t,
			nudgeEvent(1, "bead.created", nudgeAgo(10*time.Minute), "ga-born", "gascity/other-lane", true))
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/other-lane") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/other-lane for a flat created-only routing; gc calls:\n%s", got, logged)
		}
		if !nudgeStateHasKey(t, stateDir, "ga-born|gascity/other-lane") {
			t.Fatal("the (bead, routed_to) pair must be recorded so the nudge is not repeated")
		}
	})

	t.Run("created and updated for one pair nudge once", func(t *testing.T) {
		logPath, stateDir := run(t,
			nudgeEvent(1, "bead.created", nudgeAgo(10*time.Minute), "ga-born", "gascity/builder", false),
			nudgeEvent(2, "bead.updated", nudgeAgo(30*time.Second), "ga-born", "gascity/builder", false))
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1: one routing seen as created and updated; gc calls:\n%s", got, logged)
		}
		if !nudgeStateHasKey(t, stateDir, "ga-born|gascity/builder") {
			t.Fatal("the (bead, routed_to) pair must be recorded")
		}
	})

	t.Run("a routed bead.closed alone is not a routing", func(t *testing.T) {
		logPath, _ := run(t,
			nudgeEvent(1, "bead.closed", nudgeAgo(10*time.Minute), "ga-done", "gascity/builder", false))
		if got, logged := countNudges(t, logPath); got != 0 {
			t.Fatalf("nudges = %d, want 0: a routed bead.closed is not a new routing; gc calls:\n%s", got, logged)
		}
	})
}

// TestNudgeOnRouteCatchesUpPastTheFastWindow pins the two-window read. City
// seqs are assigned latest+1 under the recorder's mutex, so a fast read is
// contiguous with the persisted mark exactly when its lowest seq sits at or
// below MARK+1 and its head at or above MARK; only then is the short read
// trusted. Otherwise the run re-reads the catch-up window, so a dispatch gap
// longer than the fast window — routine under max_dispatches_per_tick —
// drops no routing, while steady state still costs one gc events call.
func TestNudgeOnRouteCatchesUpPastTheFastWindow(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	t.Run("routed event older than the fast window is caught up", func(t *testing.T) {
		binDir, logPath, stateDir := nudgeFixture(t, "10", nil,
			nudgeEvent(11, "bead.updated", nudgeAgo(10*time.Minute), "ga-gap", "gascity/builder", false),
			nudgeEvent(12, "bead.updated", nudgeAgo(30*time.Second), "ga-new", "", false))
		out, err := runNudgeOnRoute(t, binDir, stateDir)
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/builder for a routed event 10m old; gc calls:\n%s", got, logged)
		}
		if got := nudgeReadSeq(t, stateDir); got != "12" {
			t.Fatalf("seq mark = %q, want 12 (the highest seq read)", got)
		}
		if got := nudgeSinceArgs(t, logPath); !reflect.DeepEqual(got, []string{"2m", "1h"}) {
			t.Fatalf("gc events windows = %v, want the fast read then the catch-up re-read", got)
		}
	})

	t.Run("an empty fast read still catches up", func(t *testing.T) {
		binDir, logPath, stateDir := nudgeFixture(t, "10", nil,
			nudgeEvent(11, "bead.created", nudgeAgo(10*time.Minute), "ga-old", "gascity/builder", false))
		out, err := runNudgeOnRoute(t, binDir, stateDir)
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/builder when every event is older than the fast window; gc calls:\n%s", got, logged)
		}
		if got := nudgeReadSeq(t, stateDir); got != "11" {
			t.Fatalf("seq mark = %q, want 11", got)
		}
		if got := nudgeSinceArgs(t, logPath); !reflect.DeepEqual(got, []string{"2m", "1h"}) {
			t.Fatalf("gc events windows = %v, want the fast read then the catch-up re-read", got)
		}
	})

	t.Run("steady state reads only the fast window", func(t *testing.T) {
		binDir, logPath, stateDir := nudgeFixture(t, "10", nil,
			nudgeEvent(11, "bead.updated", nudgeAgo(30*time.Second), "ga-now", "gascity/builder", false))
		out, err := runNudgeOnRoute(t, binDir, stateDir)
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/builder; gc calls:\n%s", got, logged)
		}
		if got := nudgeReadSeq(t, stateDir); got != "11" {
			t.Fatalf("seq mark = %q, want 11", got)
		}
		if got := nudgeSinceArgs(t, logPath); !reflect.DeepEqual(got, []string{"2m"}) {
			t.Fatalf("gc events windows = %v, want only the fast read in steady state", got)
		}
	})

	t.Run("incomplete catch-up names the skipped span", func(t *testing.T) {
		binDir, logPath, stateDir := nudgeFixture(t, "10", nil,
			nudgeEvent(20, "bead.updated", nudgeAgo(30*time.Second), "ga-far", "gascity/builder", false),
			nudgeEvent(21, "bead.updated", nudgeAgo(30*time.Second), "ga-far2", "", false))
		out, err := runNudgeOnRoute(t, binDir, stateDir)
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "catch-up read starts at seq 20 above mark 10; 9 older event(s) not evaluated") {
			t.Fatalf("the catch-up read must name the span it could not reach; output:\n%s", out)
		}
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/builder for what the catch-up read did reach; gc calls:\n%s", got, logged)
		}
		if got := nudgeReadSeq(t, stateDir); got != "21" {
			t.Fatalf("seq mark = %q, want 21", got)
		}
	})

	t.Run("retention shorter than the catch-up window warns and still nudges", func(t *testing.T) {
		binDir, logPath, stateDir := nudgeFixture(t, "", nil,
			nudgeEvent(1, "bead.created", nudgeAgo(30*time.Second), "ga-keep", "gascity/builder", false))
		out, err := runNudgeOnRoute(t, binDir, stateDir, "GC_NUDGE_ON_ROUTE_RETENTION=30m")
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "GC_NUDGE_ON_ROUTE_RETENTION (30m) is shorter than GC_NUDGE_ON_ROUTE_MAX_LOOKBACK (1h)") {
			t.Fatalf("a short retention must warn on stderr; output:\n%s", out)
		}
		if got, logged := countNudges(t, logPath); got != 1 {
			t.Fatalf("nudges = %d, want 1: the warning must not skip the run; gc calls:\n%s", got, logged)
		}
	})

	t.Run("defaults do not warn", func(t *testing.T) {
		binDir, logPath, stateDir := nudgeFixture(t, "", nil,
			nudgeEvent(1, "bead.created", nudgeAgo(30*time.Second), "ga-quiet", "gascity/builder", false))
		out, err := runNudgeOnRoute(t, binDir, stateDir)
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
		if strings.Contains(out, "is shorter than") {
			t.Fatalf("the default retention covers the catch-up window; output:\n%s", out)
		}
		if got, _ := countNudges(t, logPath); got != 1 {
			t.Fatalf("nudges = %d, want 1", got)
		}
	})
}

// TestNudgeOnRouteEvaluatesEachEventOnce pins the mark's retry contract. A
// run that evaluates a routed pair whose nudge fails (usually a pool with no
// live member yet) advances the mark anyway — the pair has no dedup entry,
// so the bead's next bead.updated retries it — and the next run with nothing
// above the mark attempts no nudge and leaves the mark alone. A line that is
// not JSON (a torn events.jsonl write) must not drop the rest of the batch
// or wedge the mark: every event is evaluated line by line.
func TestNudgeOnRouteEvaluatesEachEventOnce(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	writeEvents := func(lines ...string) {
		t.Helper()
		if err := os.WriteFile(eventsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write events: %v", err)
		}
	}
	failMarker := filepath.Join(dir, "worker-1")
	seed := []string{
		"not json",
		nudgeEvent(2, "bead.updated", nudgeAgo(30*time.Second), "ga-plain", "", false),
		nudgeEvent(3, "bead.updated", nudgeAgo(30*time.Second), "ga-rty", "worker-1", false),
	}
	writeEvents(seed...)
	binDir, logPath := fakeGCBin(t, cascadeFake{
		eventsPath:   eventsPath,
		failDir:      dir,
		filterEvents: true,
	}.body())
	stateDir := filepath.Join(dir, "state")

	// Run 1: the nudge to worker-1 fails (fail marker), so the pair stays
	// unrecorded, but the events were evaluated: the mark advances.
	if err := os.WriteFile(failMarker, nil, 0o644); err != nil {
		t.Fatalf("write fail marker: %v", err)
	}
	out, err := runNudgeOnRoute(t, binDir, stateDir)
	if err != nil {
		t.Fatalf("run 1 failed: %v\n%s", err, out)
	}
	if got := countNudgesTo(t, logPath, "worker-1"); got != 1 {
		t.Fatalf("run 1: nudge attempts to worker-1 = %d, want 1", got)
	}
	if nudgeStateHasKey(t, stateDir, "ga-rty|worker-1") {
		t.Fatal("run 1: the failed pair must not be recorded as nudged")
	}
	if got := nudgeReadSeq(t, stateDir); got != "3" {
		t.Fatalf("run 1: seq mark = %q, want 3 (the failed nudge must not hold the mark back)", got)
	}

	// Run 2: same events, nothing above the mark, marker gone. No new
	// attempt, mark unchanged.
	if err := os.Remove(failMarker); err != nil {
		t.Fatalf("remove fail marker: %v", err)
	}
	out, err = runNudgeOnRoute(t, binDir, stateDir)
	if err != nil {
		t.Fatalf("run 2 failed: %v\n%s", err, out)
	}
	if got, logged := countNudges(t, logPath); got != 1 {
		t.Fatalf("run 2: nudges = %d, want still 1 (no event above the mark); gc calls:\n%s", got, logged)
	}
	if got := nudgeReadSeq(t, stateDir); got != "3" {
		t.Fatalf("run 2: seq mark = %q, want still 3", got)
	}

	// Run 3: seq 4 re-emits the pair (the reconciler refreshes live
	// routings); the retry goes out and is recorded.
	writeEvents(append(append([]string{}, seed...),
		nudgeEvent(4, "bead.updated", nudgeAgo(20*time.Second), "ga-rty", "worker-1", false))...)
	out, err = runNudgeOnRoute(t, binDir, stateDir)
	if err != nil {
		t.Fatalf("run 3 failed: %v\n%s", err, out)
	}
	if got := countNudgesTo(t, logPath, "worker-1"); got != 2 {
		t.Fatalf("run 3: nudges to worker-1 = %d, want 2 (one failed attempt, one retry)", got)
	}
	if !nudgeStateHasKey(t, stateDir, "ga-rty|worker-1") {
		t.Fatal("run 3: the retried pair must be recorded once delivered")
	}
	if got := nudgeReadSeq(t, stateDir); got != "4" {
		t.Fatalf("run 3: seq mark = %q, want 4", got)
	}

	// A batch with nothing routed still advances the mark.
	writeEvents(
		nudgeEvent(5, "bead.updated", nudgeAgo(15*time.Second), "ga-none", "", false),
		nudgeEvent(6, "bead.updated", nudgeAgo(10*time.Second), "ga-none2", "", false))
	out, err = runNudgeOnRoute(t, binDir, stateDir)
	if err != nil {
		t.Fatalf("run 4 failed: %v\n%s", err, out)
	}
	if got, logged := countNudges(t, logPath); got != 2 {
		t.Fatalf("run 4: nudges = %d, want still 2; gc calls:\n%s", got, logged)
	}
	if got := nudgeReadSeq(t, stateDir); got != "6" {
		t.Fatalf("run 4: seq mark = %q, want 6 (nothing routed still advances the mark)", got)
	}
}

// TestNudgeOnRouteFirstRunReadsMaxLookback pins the first-run contract:
// with no usable mark (seq file absent, non-numeric, or above every seq
// read), the run reads GC_NUDGE_ON_ROUTE_MAX_LOOKBACK — the only lookback
// it has — nudges every routed pair inside that window that is not already
// in the dedup state, and writes the highest seq read as the mark.
func TestNudgeOnRouteFirstRunReadsMaxLookback(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	for name, tc := range map[string]struct {
		seed       string
		wantWindow []string
	}{
		"no seq file":            {seed: "", wantWindow: []string{"1h"}},
		"garbage seq file":       {seed: "garbage", wantWindow: []string{"1h"}},
		"seq file above the log": {seed: "1000", wantWindow: []string{"2m", "1h"}},
	} {
		t.Run(name, func(t *testing.T) {
			binDir, logPath, stateDir := nudgeFixture(t, tc.seed, nil,
				nudgeEvent(5, "bead.created", nudgeAgo(40*time.Minute), "ga-first", "gascity/builder", false))
			out, err := runNudgeOnRoute(t, binDir, stateDir)
			if err != nil {
				t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
			}
			if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
				t.Fatalf("nudges = %d, want exactly 1 to gascity/builder for a routed event 40m old; gc calls:\n%s", got, logged)
			}
			if got := nudgeReadSeq(t, stateDir); got != "5" {
				t.Fatalf("seq mark = %q, want 5 (the highest seq read)", got)
			}
			// With no usable mark the catch-up window is the only read; a
			// mark above the log head still gets its fast read first, then
			// the catch-up re-read that replaces it.
			if got := nudgeSinceArgs(t, logPath); !reflect.DeepEqual(got, tc.wantWindow) {
				t.Fatalf("gc events windows = %v, want %v", got, tc.wantWindow)
			}
		})
	}

	t.Run("a smaller MAX_LOOKBACK bounds the first run", func(t *testing.T) {
		binDir, logPath, stateDir := nudgeFixture(t, "", nil,
			nudgeEvent(5, "bead.created", nudgeAgo(40*time.Minute), "ga-deep", "gascity/builder", false),
			nudgeEvent(6, "bead.created", nudgeAgo(3*time.Minute), "ga-fresh", "gascity/builder", false))
		out, err := runNudgeOnRoute(t, binDir, stateDir, "GC_NUDGE_ON_ROUTE_MAX_LOOKBACK=5m")
		if err != nil {
			t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
		}
		if got, logged := countNudges(t, logPath); got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
			t.Fatalf("nudges = %d, want exactly 1 to gascity/builder: only the 3m-old pair is inside a 5m catch-up window; gc calls:\n%s", got, logged)
		}
		if !nudgeStateHasKey(t, stateDir, "ga-fresh|gascity/builder") {
			t.Fatal("the pair inside the window must be recorded")
		}
		if nudgeStateHasKey(t, stateDir, "ga-deep|gascity/builder") {
			t.Fatal("the 40m-old pair is outside the 5m catch-up window and must not be nudged or recorded")
		}
		if got := nudgeReadSeq(t, stateDir); got != "6" {
			t.Fatalf("seq mark = %q, want 6", got)
		}
		if got := nudgeSinceArgs(t, logPath); !reflect.DeepEqual(got, []string{"5m"}) {
			t.Fatalf("gc events windows = %v, want one read of the configured 5m catch-up window", got)
		}
	})
}

// TestNudgeOnRouteCoalescesNudgesPerTarget pins the per-target coalescing: a
// batch that routes several beads to one target wakes that target once — one
// wake-up covers the whole batch, the target reads its hook and finds them all
// — while a pool target still fans out to every live member, and every
// delivered pair is recorded so no later run re-nudges it.
func TestNudgeOnRouteCoalescesNudgesPerTarget(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	ago := nudgeAgo(30 * time.Second)
	binDir, logPath, stateDir := nudgeFixture(t, "", map[string][]string{
		"gascity/pool": {"pool-1", "pool-2"},
	},
		nudgeEvent(1, "bead.created", ago, "ga-a", "gascity/builder", false),
		nudgeEvent(2, "bead.created", ago, "ga-b", "gascity/builder", false),
		nudgeEvent(3, "bead.created", ago, "ga-c", "gascity/builder", false),
		nudgeEvent(4, "bead.created", ago, "ga-p", "gascity/pool", false))
	out, err := runNudgeOnRoute(t, binDir, stateDir)
	if err != nil {
		t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
	}
	if got, logged := countNudges(t, logPath); got != 3 {
		t.Fatalf("nudges = %d, want 3: gascity/builder once for its three beads plus pool-1 and pool-2 once each; gc calls:\n%s", got, logged)
	}
	if got := countNudgesTo(t, logPath, "gascity/builder"); got != 1 {
		t.Fatalf("nudges to gascity/builder = %d, want 1: one wake-up covers every bead routed to the target", got)
	}
	if got := countNudgesTo(t, logPath, "pool-1"); got != 1 {
		t.Fatalf("nudges to pool-1 = %d, want 1", got)
	}
	if got := countNudgesTo(t, logPath, "pool-2"); got != 1 {
		t.Fatalf("nudges to pool-2 = %d, want 1", got)
	}
	for _, pair := range []string{
		"ga-a|gascity/builder", "ga-b|gascity/builder", "ga-c|gascity/builder",
		"ga-p|gascity/pool",
	} {
		if !nudgeStateHasKey(t, stateDir, pair) {
			t.Fatalf("pair %s must be recorded as nudged", pair)
		}
	}
	if got := nudgeReadSeq(t, stateDir); got != "4" {
		t.Fatalf("seq mark = %q, want 4", got)
	}

	// The next run over the same events nudges nothing: the seq cut alone
	// suppresses the batch.
	out, err = runNudgeOnRoute(t, binDir, stateDir)
	if err != nil {
		t.Fatalf("second run failed: %v\n%s", err, out)
	}
	if got, logged := countNudges(t, logPath); got != 3 {
		t.Fatalf("nudges after rerun = %d, want still 3; gc calls:\n%s", got, logged)
	}
}
