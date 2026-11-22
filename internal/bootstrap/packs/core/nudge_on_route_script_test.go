package core

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// the state dir.
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
		members:      members,
		failDir:      dir,
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
