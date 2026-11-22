package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

// nudgeOnRouteScriptPath is the on-disk copy of the nudge-on-route order's
// script. The test runs it, so it needs a real path.
const nudgeOnRouteScriptPath = "assets/scripts/nudge-on-route.sh"

// nudgeOnRouteEnvKeys are the settings that would point the script at a live
// session's city, state, trace or outcome file. The test supplies its own
// state dir and outcome file.
var nudgeOnRouteEnvKeys = map[string]struct{}{
	"GC_CITY":                        {},
	"GC_CITY_RUNTIME_DIR":            {},
	"GC_PACK_STATE_DIR":              {},
	"GC_BD_TRACE_JSON":               {},
	"GC_ORDER_OUTCOME_FILE":          {},
	"GC_NUDGE_ON_ROUTE_WINDOW":       {},
	"GC_NUDGE_ON_ROUTE_LOOKBACK":     {},
	"GC_NUDGE_ON_ROUTE_MAX_LOOKBACK": {},
	"GC_NUDGE_ON_ROUTE_RETENTION":    {},
	"GC_NUDGE_ON_ROUTE_MESSAGE":      {},
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

// nudgeEvent describes one fixture event for the nudge-on-route script. An
// empty routedTo leaves the bead unrouted; flat emits the bead fields directly
// under .payload, as events.jsonl stores them, instead of the API's
// {"bead": …} envelope. age sets ts that far before now.
type nudgeEvent struct {
	seq      int
	typ      string
	bead     string
	routedTo string
	age      time.Duration
	flat     bool
}

// line renders the event as one JSON line.
func (e nudgeEvent) line(t *testing.T) string {
	t.Helper()
	meta := map[string]string{}
	if e.routedTo != "" {
		meta["gc.routed_to"] = e.routedTo
	}
	bead := map[string]any{"id": e.bead, "status": "open", "metadata": meta}
	var payload any = map[string]any{"bead": bead}
	if e.flat {
		payload = bead
	}
	data, err := json.Marshal(map[string]any{
		"seq":     e.seq,
		"type":    e.typ,
		"actor":   "gc",
		"subject": e.bead,
		"ts":      time.Now().UTC().Add(-e.age).Format(time.RFC3339),
		"payload": payload,
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(data)
}

// nudgeOnRouteRig is one city's worth of fixture for the nudge-on-route
// script: an events file served by a fake gc that honors --type and --since,
// the pack state dir, the outcome file each run may declare a partial run
// in, and a dir whose marker files fail a nudge.
type nudgeOnRouteRig struct {
	t           *testing.T
	dir         string
	eventsPath  string
	stateDir    string
	outcomePath string
	binDir      string
	logPath     string
}

// newNudgeOnRouteRig builds a rig whose fake gc reports members as each pool
// template's active sessions. filter selects the filtering events read; off,
// `gc events` answers the whole file.
func newNudgeOnRouteRig(t *testing.T, filter bool, members map[string][]string) *nudgeOnRouteRig {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}
	dir := t.TempDir()
	r := &nudgeOnRouteRig{
		t:           t,
		dir:         dir,
		eventsPath:  filepath.Join(dir, "events.jsonl"),
		stateDir:    filepath.Join(dir, "state"),
		outcomePath: filepath.Join(dir, "outcome.json"),
	}
	r.binDir, r.logPath = fakeGCBin(t, cascadeFake{
		eventsPath:   r.eventsPath,
		members:      members,
		failDir:      filepath.Join(dir, "fail"),
		filterEvents: filter,
	}.body())
	return r
}

// writeLines replaces the events file with the given raw lines.
func (r *nudgeOnRouteRig) writeLines(lines ...string) {
	r.t.Helper()
	if err := os.WriteFile(r.eventsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		r.t.Fatalf("write events: %v", err)
	}
}

// writeEvents replaces the events file with the given events.
func (r *nudgeOnRouteRig) writeEvents(events ...nudgeEvent) {
	r.t.Helper()
	lines := make([]string, 0, len(events))
	for _, e := range events {
		lines = append(lines, e.line(r.t))
	}
	r.writeLines(lines...)
}

// seedSeq writes the persisted seq mark as it would be left by a prior run.
func (r *nudgeOnRouteRig) seedSeq(mark string) {
	r.t.Helper()
	if err := os.MkdirAll(r.stateDir, 0o755); err != nil {
		r.t.Fatalf("mkdir state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir, "nudge-on-route-seq"), []byte(mark+"\n"), 0o644); err != nil {
		r.t.Fatalf("seed seq: %v", err)
	}
}

// seedState records the given "<bead>|<target>" pairs in the dedup state as
// nudged just now.
func (r *nudgeOnRouteRig) seedState(keys ...string) {
	r.t.Helper()
	state := make(map[string]string, len(keys))
	now := time.Now().UTC().Format(time.RFC3339)
	for _, k := range keys {
		state[k] = now
	}
	data, err := json.Marshal(state)
	if err != nil {
		r.t.Fatalf("marshal dedup state: %v", err)
	}
	if err := os.MkdirAll(r.stateDir, 0o755); err != nil {
		r.t.Fatalf("mkdir state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir, "nudge-on-route-state.json"), data, 0o644); err != nil {
		r.t.Fatalf("seed dedup state: %v", err)
	}
}

// failNudgesTo makes every `gc session nudge <target>` exit 1.
func (r *nudgeOnRouteRig) failNudgesTo(target string) {
	r.t.Helper()
	marker := filepath.Join(r.dir, "fail", target)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		r.t.Fatalf("mkdir fail marker dir: %v", err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		r.t.Fatalf("write fail marker: %v", err)
	}
}

// unfailNudgesTo lets nudges to target succeed again.
func (r *nudgeOnRouteRig) unfailNudgesTo(target string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, "fail", target)); err != nil {
		r.t.Fatalf("remove fail marker: %v", err)
	}
}

// run executes the script once and returns its combined output. Like the
// controller, it hands each run an empty GC_ORDER_OUTCOME_FILE.
func (r *nudgeOnRouteRig) run(extraEnv ...string) string {
	r.t.Helper()
	if err := os.WriteFile(r.outcomePath, nil, 0o644); err != nil {
		r.t.Fatalf("reset outcome file: %v", err)
	}
	env := append([]string{"GC_PACK_STATE_DIR=" + r.stateDir, "GC_ORDER_OUTCOME_FILE=" + r.outcomePath}, extraEnv...)
	out, err := runPackScript(r.t, nudgeOnRouteScriptPath, r.binDir, nudgeOnRouteEnvKeys, env)
	if err != nil {
		r.t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
	}
	return out
}

// wantOutcome checks the last run's declaration, decoded as the controller
// decodes it: a partial outcome with exactly reason, or, when reason is
// empty, no declaration at all.
func (r *nudgeOnRouteRig) wantOutcome(reason string) {
	r.t.Helper()
	data, err := os.ReadFile(r.outcomePath)
	if err != nil {
		r.t.Fatalf("read outcome file: %v", err)
	}
	out, declared, err := orders.DecodeExecOutcome(data)
	if err != nil {
		r.t.Fatalf("decode outcome declaration %q: %v", data, err)
	}
	if reason == "" {
		if declared {
			r.t.Fatalf("outcome = %+v, want no declaration", out)
		}
		return
	}
	if !declared || out.Outcome != orders.ExecOutcomePartial || out.Reason != reason {
		r.t.Fatalf("outcome = %+v (declared %t), want partial: %s", out, declared, reason)
	}
}

// seq returns the persisted seq mark, or "" when the file is absent.
func (r *nudgeOnRouteRig) seq() string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.stateDir, "nudge-on-route-seq"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		r.t.Fatalf("read seq mark: %v", err)
	}
	return strings.TrimSpace(string(data))
}

// hasKey reports whether the dedup state records the "<bead>|<target>" pair.
func (r *nudgeOnRouteRig) hasKey(key string) bool {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.stateDir, "nudge-on-route-state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		r.t.Fatalf("read dedup state: %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		r.t.Fatalf("parse dedup state: %v\n%s", err, data)
	}
	_, ok := state[key]
	return ok
}

// sinceReads lists the --since argument of each logged `gc events` call, in
// call order.
func (r *nudgeOnRouteRig) sinceReads() []string {
	r.t.Helper()
	logged, err := os.ReadFile(r.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("read gc log: %v", err)
	}
	var reads []string
	for _, line := range strings.Split(string(logged), "\n") {
		if !strings.HasPrefix(line, "gc events ") {
			continue
		}
		fields := strings.Fields(line)
		since := ""
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "--since" {
				since = fields[i+1]
			}
		}
		reads = append(reads, since)
	}
	return reads
}

// nudges returns how many nudges reached target and the full gc call log.
func (r *nudgeOnRouteRig) nudges(target string) (int, string) {
	r.t.Helper()
	_, logged := countNudges(r.t, r.logPath)
	return countNudgesTo(r.t, r.logPath, target), logged
}

// TestNudgeOnRouteNudgesRoutingStampedAtCreation pins #4382: a bead whose
// gc.routed_to is stamped in its create payload (gc order run, formula pours,
// poured step beads) emits bead.created and no bead.updated before it is
// claimed, so the order must read creations too, in both payload shapes. The
// fake gc honors --type exactly, so a bead.updated-only read sees nothing.
func TestNudgeOnRouteNudgesRoutingStampedAtCreation(t *testing.T) {
	const target = "gascity/builder"
	for _, tc := range []struct {
		name string
		flat bool
	}{
		{name: "nested payload.bead", flat: false},
		{name: "flat payload", flat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNudgeOnRouteRig(t, true, nil)
			r.writeEvents(nudgeEvent{seq: 1, typ: "bead.created", bead: "ga-born", routedTo: target, age: 30 * time.Second, flat: tc.flat})
			r.run()
			if got, logged := r.nudges(target); got != 1 {
				t.Fatalf("nudges to %s = %d, want exactly 1; gc calls:\n%s", target, got, logged)
			}
			if total, logged := countNudges(t, r.logPath); total != 1 {
				t.Fatalf("nudges = %d, want exactly 1; gc calls:\n%s", total, logged)
			}
			if !r.hasKey("ga-born|" + target) {
				t.Fatalf("dedup state lacks ga-born|%s after the nudge", target)
			}
		})
	}

	t.Run("creation and update of one routing nudge once", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.writeEvents(
			nudgeEvent{seq: 1, typ: "bead.created", bead: "ga-born", routedTo: target, age: 40 * time.Second},
			nudgeEvent{seq: 2, typ: "bead.updated", bead: "ga-born", routedTo: target, age: 30 * time.Second, flat: true},
		)
		r.run()
		if total, logged := countNudges(t, r.logPath); total != 1 {
			t.Fatalf("nudges = %d, want exactly 1 for one routing seen twice; gc calls:\n%s", total, logged)
		}
	})

	t.Run("a routed bead.closed is not a routing", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.writeEvents(nudgeEvent{seq: 1, typ: "bead.closed", bead: "ga-done", routedTo: target, age: 30 * time.Second})
		r.run()
		if total, logged := countNudges(t, r.logPath); total != 0 {
			t.Fatalf("nudges = %d, want 0 for bead.closed; gc calls:\n%s", total, logged)
		}
	})

	t.Run("a malformed line does not drop the batch", func(t *testing.T) {
		// The local fallback reads events.jsonl, which can hold a torn line;
		// each line is parsed on its own so the routing after it survives.
		r := newNudgeOnRouteRig(t, false, nil)
		r.writeLines(
			`{"seq":1,"type":"bead.updated","payload":{"bead":{"id":"ga-torn"`,
			nudgeEvent{seq: 2, typ: "bead.created", bead: "ga-born", routedTo: target, age: 30 * time.Second}.line(t),
		)
		r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want exactly 1 past a malformed line; gc calls:\n%s", target, got, logged)
		}
		if !r.hasKey("ga-born|" + target) {
			t.Fatalf("dedup state lacks ga-born|%s after the nudge", target)
		}
		if got := r.seq(); got != "2" {
			t.Fatalf("seq mark = %q, want 2", got)
		}
	})
}

// TestNudgeOnRouteFirstRunReadsMaxLookback pins the read used when there is
// no usable mark (no seq file, a non-numeric one, or one above every seq the
// log holds, as after a log reset): the whole GC_NUDGE_ON_ROUTE_MAX_LOOKBACK
// window, nudging every routed pair in it not already in the dedup state and
// recording the highest seq read as the mark. A reset is declared as a
// partial run, so the controller records it; a first run declares nothing.
func TestNudgeOnRouteFirstRunReadsMaxLookback(t *testing.T) {
	const target = "gascity/builder"
	for _, tc := range []struct {
		name        string
		seed        string
		wantOutcome string
	}{
		{name: "no seq file"},
		{name: "non-numeric seq file", seed: "garbage"},
		{name: "seq file above every seq", seed: "1000", wantOutcome: "event seq went backwards (7 < 1000); resetting mark"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNudgeOnRouteRig(t, true, nil)
			if tc.seed != "" {
				r.seedSeq(tc.seed)
			}
			r.seedState("ga-known|gascity/known")
			r.writeEvents(
				nudgeEvent{seq: 5, typ: "bead.updated", bead: "ga-known", routedTo: "gascity/known", age: 50 * time.Minute},
				nudgeEvent{seq: 6, typ: "bead.created", bead: "ga-old", routedTo: target, age: 40 * time.Minute},
				nudgeEvent{seq: 7, typ: "bead.updated", bead: "ga-plain", age: 35 * time.Minute},
			)
			r.run()
			if got, logged := r.nudges(target); got != 1 {
				t.Fatalf("nudges to %s = %d, want 1 for a 40m-old routing; gc calls:\n%s", target, got, logged)
			}
			if got, logged := r.nudges("gascity/known"); got != 0 {
				t.Fatalf("nudges to gascity/known = %d, want 0 for a pair already in the dedup state; gc calls:\n%s", got, logged)
			}
			if !r.hasKey("ga-old|" + target) {
				t.Fatalf("dedup state lacks ga-old|%s", target)
			}
			if got := r.seq(); got != "7" {
				t.Fatalf("seq mark = %q, want 7, the highest seq read", got)
			}
			reads := r.sinceReads()
			if len(reads) == 0 || reads[len(reads)-1] != "1h" {
				t.Fatalf("events reads = %q, want the last to be --since 1h", reads)
			}
			r.wantOutcome(tc.wantOutcome)
		})
	}

	// The catch-up window only reaches `gc events --since`, which takes any
	// Go duration, so a compound or fractional value must not stop the run.
	for _, tc := range []struct {
		env   string
		since string
	}{
		{env: "GC_NUDGE_ON_ROUTE_WINDOW=1h30m", since: "1h30m"},
		{env: "GC_NUDGE_ON_ROUTE_MAX_LOOKBACK=1.5h", since: "1.5h"},
	} {
		t.Run("catch-up window "+tc.env, func(t *testing.T) {
			// The filtering fake parses only single-term durations, so this
			// one answers the whole file whatever --since says.
			r := newNudgeOnRouteRig(t, false, nil)
			r.writeEvents(nudgeEvent{seq: 1, typ: "bead.created", bead: "ga-new", routedTo: target, age: 30 * time.Second})
			out := r.run(tc.env)
			if got, logged := r.nudges(target); got != 1 {
				t.Fatalf("nudges to %s = %d, want 1; gc calls:\n%s", target, got, logged)
			}
			if reads := r.sinceReads(); len(reads) != 1 || reads[0] != tc.since {
				t.Fatalf("events reads = %q, want exactly [%s]", reads, tc.since)
			}
			if strings.Contains(out, "is shorter than GC_NUDGE_ON_ROUTE_MAX_LOOKBACK") {
				t.Fatalf("want no retention warning; output:\n%s", out)
			}
			if got := r.seq(); got != "1" {
				t.Fatalf("seq mark = %q, want 1", got)
			}
		})
	}

	t.Run("GC_NUDGE_ON_ROUTE_MAX_LOOKBACK bounds the read", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.writeEvents(
			nudgeEvent{seq: 1, typ: "bead.created", bead: "ga-stale", routedTo: target, age: 10 * time.Minute},
			nudgeEvent{seq: 2, typ: "bead.created", bead: "ga-fresh", routedTo: "gascity/fresh", age: time.Minute},
		)
		r.run("GC_NUDGE_ON_ROUTE_MAX_LOOKBACK=5m")
		if got, logged := r.nudges(target); got != 0 {
			t.Fatalf("nudges to %s = %d, want 0 for a routing older than the 5m window; gc calls:\n%s", target, got, logged)
		}
		if got, logged := r.nudges("gascity/fresh"); got != 1 {
			t.Fatalf("nudges to gascity/fresh = %d, want 1; gc calls:\n%s", got, logged)
		}
		if reads := r.sinceReads(); len(reads) != 1 || reads[0] != "5m" {
			t.Fatalf("events reads = %q, want exactly [5m]", reads)
		}
		if got := r.seq(); got != "2" {
			t.Fatalf("seq mark = %q, want 2", got)
		}
	})
}

// TestNudgeOnRouteCatchesUpPastTheFastWindow pins the gap check. Each run
// reads the GC_NUDGE_ON_ROUTE_LOOKBACK fast window; when its seqs do not
// reach back to the mark, events were missed (the controller fired late), so
// the script re-reads the catch-up window instead of dropping them. A
// catch-up read that still starts above the mark names the span it skipped,
// on stderr and as a partial outcome the controller records.
func TestNudgeOnRouteCatchesUpPastTheFastWindow(t *testing.T) {
	const target = "gascity/builder"
	const gapLine = "nudge-on-route: catch-up read starts at seq "
	const retentionLine = "is shorter than GC_NUDGE_ON_ROUTE_MAX_LOOKBACK"

	t.Run("a routing older than the fast window is caught up", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.seedSeq("10")
		r.writeEvents(
			nudgeEvent{seq: 11, typ: "bead.updated", bead: "ga-late", routedTo: target, age: 10 * time.Minute},
			nudgeEvent{seq: 12, typ: "session.started", bead: "s-1", age: 30 * time.Second},
		)
		out := r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want 1 for seq 11 just above the mark; gc calls:\n%s", target, got, logged)
		}
		if got := r.seq(); got != "12" {
			t.Fatalf("seq mark = %q, want 12, the highest seq read", got)
		}
		if reads := r.sinceReads(); strings.Join(reads, ",") != "2m,1h" {
			t.Fatalf("events reads = %q, want [2m 1h]: the fast read, then the catch-up", reads)
		}
		if strings.Contains(out, gapLine) {
			t.Fatalf("catch-up reached the mark, want no gap line; output:\n%s", out)
		}
		r.wantOutcome("")
	})

	t.Run("a catch-up read of thousands of events is evaluated", func(t *testing.T) {
		// A busy city logs thousands of events an hour. 10,000 8-digit seqs
		// are 90 KB of seq text, more than a pipe holds (64 KiB) plus one
		// read, so a reader that stops at the first line always leaves the
		// writer blocked on a pipe it then closes: under pipefail the script
		// dies and the mark never advances. A read that fits in the pipe
		// fails only when the reader wins the race.
		const mark = 75000000
		const n = 10000
		r := newNudgeOnRouteRig(t, true, nil)
		r.seedSeq(strconv.Itoa(mark))
		events := make([]nudgeEvent, 0, n)
		for i := 1; i <= n; i++ {
			// Spread over the last 50 minutes, all inside the 1h catch-up
			// window: the 2m fast read holds only the newest ~400 and misses
			// the mark, so the run catches up.
			e := nudgeEvent{seq: mark + i, typ: "bead.updated", bead: fmt.Sprintf("ga-%d", i), age: time.Duration(n-i) * 300 * time.Millisecond}
			if i == 1 {
				e.routedTo = target
			}
			events = append(events, e)
		}
		r.writeEvents(events...)
		out := r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want 1 for the routing only the catch-up read holds; gc calls:\n%s", target, got, logged)
		}
		if total, logged := countNudges(t, r.logPath); total != 1 {
			t.Fatalf("nudges = %d, want exactly 1; gc calls:\n%s", total, logged)
		}
		if reads := r.sinceReads(); strings.Join(reads, ",") != "2m,1h" {
			t.Fatalf("events reads = %q, want [2m 1h]", reads)
		}
		if got, want := r.seq(), strconv.Itoa(mark+n); got != want {
			t.Fatalf("seq mark = %q, want %s, the highest seq read", got, want)
		}
		if strings.Contains(out, gapLine) {
			t.Fatalf("catch-up reached the mark, want no gap line; output:\n%s", out)
		}
		r.wantOutcome("")
	})

	t.Run("an empty fast read is caught up", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.seedSeq("10")
		r.writeEvents(
			nudgeEvent{seq: 11, typ: "bead.updated", bead: "ga-late", routedTo: target, age: 10 * time.Minute},
			nudgeEvent{seq: 12, typ: "bead.updated", bead: "ga-plain", age: 5 * time.Minute},
		)
		r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want 1; gc calls:\n%s", target, got, logged)
		}
		if reads := r.sinceReads(); strings.Join(reads, ",") != "2m,1h" {
			t.Fatalf("events reads = %q, want [2m 1h]", reads)
		}
		if got := r.seq(); got != "12" {
			t.Fatalf("seq mark = %q, want 12", got)
		}
	})

	t.Run("steady state reads only the fast window", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.seedSeq("10")
		r.writeEvents(nudgeEvent{seq: 11, typ: "bead.created", bead: "ga-new", routedTo: target, age: 30 * time.Second})
		out := r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want 1; gc calls:\n%s", target, got, logged)
		}
		if reads := r.sinceReads(); strings.Join(reads, ",") != "2m" {
			t.Fatalf("events reads = %q, want only [2m]", reads)
		}
		if got := r.seq(); got != "11" {
			t.Fatalf("seq mark = %q, want 11", got)
		}
		if strings.Contains(out, gapLine) || strings.Contains(out, retentionLine) {
			t.Fatalf("steady state wants no warning; output:\n%s", out)
		}
		r.wantOutcome("")
	})

	t.Run("an incomplete catch-up names the skipped span", func(t *testing.T) {
		// Seqs 11-19 stand for events that aged past the catch-up window or
		// were cut by the events walk budget.
		r := newNudgeOnRouteRig(t, true, nil)
		r.seedSeq("10")
		r.writeEvents(
			nudgeEvent{seq: 20, typ: "bead.updated", bead: "ga-after", routedTo: target, age: 30 * time.Second},
			nudgeEvent{seq: 21, typ: "session.started", bead: "s-1", age: 30 * time.Second},
		)
		out := r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want 1; gc calls:\n%s", target, got, logged)
		}
		if reads := r.sinceReads(); strings.Join(reads, ",") != "2m,1h" {
			t.Fatalf("events reads = %q, want [2m 1h]", reads)
		}
		const span = "catch-up read starts at seq 20 above mark 10; 9 older event(s) not evaluated"
		if want := "nudge-on-route: " + span; !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
		r.wantOutcome(span)
		if got := r.seq(); got != "21" {
			t.Fatalf("seq mark = %q, want 21: the mark advances past the gap", got)
		}
	})

	t.Run("retention shorter than the catch-up window warns and still runs", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.writeEvents(nudgeEvent{seq: 1, typ: "bead.created", bead: "ga-new", routedTo: target, age: 30 * time.Second})
		out := r.run("GC_NUDGE_ON_ROUTE_RETENTION=30m")
		want := "nudge-on-route: GC_NUDGE_ON_ROUTE_RETENTION (30m) is shorter than GC_NUDGE_ON_ROUTE_MAX_LOOKBACK (1h); a catch-up read can re-nudge pruned pairs"
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want 1 despite the warning; gc calls:\n%s", target, got, logged)
		}
	})

	t.Run("defaults and a first run warn about nothing", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.writeEvents(nudgeEvent{seq: 40, typ: "bead.created", bead: "ga-new", routedTo: target, age: 30 * time.Second})
		out := r.run()
		if strings.Contains(out, retentionLine) || strings.Contains(out, gapLine) {
			t.Fatalf("want no warning with defaults and no prior mark; output:\n%s", out)
		}
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want 1; gc calls:\n%s", target, got, logged)
		}
		r.wantOutcome("")
	})
}

// TestNudgeOnRouteEvaluatesEachEventOnce pins the mark's commit rule: it
// advances after every successful read, including one whose nudge failed, so
// a later run over the same events attempts nothing. The failed pair has no
// dedup entry, so the next event that re-emits the routing retries it.
func TestNudgeOnRouteEvaluatesEachEventOnce(t *testing.T) {
	const target = "lane-a"

	t.Run("a failed nudge is not retried from the same events", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		events := []nudgeEvent{
			{seq: 1, typ: "bead.updated", bead: "ga-x", age: 50 * time.Second},
			{seq: 2, typ: "bead.updated", bead: "ga-y", age: 40 * time.Second},
			{seq: 3, typ: "bead.updated", bead: "ga-r", routedTo: target, age: 30 * time.Second},
		}
		r.writeEvents(events...)
		r.failNudgesTo(target)
		r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("run 1: nudge attempts to %s = %d, want 1; gc calls:\n%s", target, got, logged)
		}
		if r.hasKey("ga-r|" + target) {
			t.Fatalf("run 1: a failed nudge must not record ga-r|%s", target)
		}
		if got := r.seq(); got != "3" {
			t.Fatalf("run 1: seq mark = %q, want 3 even though the nudge failed", got)
		}

		r.unfailNudgesTo(target)
		r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("run 2: nudge attempts to %s = %d, want still 1 with no event above the mark; gc calls:\n%s", target, got, logged)
		}
		if got := r.seq(); got != "3" {
			t.Fatalf("run 2: seq mark = %q, want 3", got)
		}

		events = append(events, nudgeEvent{seq: 4, typ: "bead.updated", bead: "ga-r", routedTo: target, age: 10 * time.Second})
		r.writeEvents(events...)
		r.run()
		if got, logged := r.nudges(target); got != 2 {
			t.Fatalf("run 3: nudge attempts to %s = %d, want 2 once the routing re-emits; gc calls:\n%s", target, got, logged)
		}
		if !r.hasKey("ga-r|" + target) {
			t.Fatalf("run 3: dedup state lacks ga-r|%s after the retried nudge", target)
		}
		if got := r.seq(); got != "4" {
			t.Fatalf("run 3: seq mark = %q, want 4", got)
		}
	})

	t.Run("a batch with nothing routed advances the mark", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.seedSeq("5")
		r.writeEvents(
			nudgeEvent{seq: 6, typ: "bead.updated", bead: "ga-x", age: 40 * time.Second},
			nudgeEvent{seq: 7, typ: "bead.created", bead: "ga-y", age: 30 * time.Second},
		)
		r.run()
		if total, logged := countNudges(t, r.logPath); total != 0 {
			t.Fatalf("nudges = %d, want 0; gc calls:\n%s", total, logged)
		}
		if got := r.seq(); got != "7" {
			t.Fatalf("seq mark = %q, want 7", got)
		}
	})

	t.Run("a failed events read leaves the mark", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.seedSeq("7")
		r.run()
		if got := r.seq(); got != "7" {
			t.Fatalf("seq mark = %q, want 7 after a failed read", got)
		}
	})
}

// TestNudgeOnRouteNudgesEachTargetOncePerBatch pins per-target coalescing.
// Once creations are read, a pour that stamps routing on N steps yields N new
// pairs for one target in one batch; each `gc session nudge` interrupts the
// session, so the target is nudged once and every pair is recorded with it.
// A pool base still fans out to each active member.
func TestNudgeOnRouteNudgesEachTargetOncePerBatch(t *testing.T) {
	const target = "gascity/builder"
	const pool = "gascity/pool"
	members := map[string][]string{pool: {"pool-1", "pool-2"}}
	batch := []nudgeEvent{
		{seq: 1, typ: "bead.created", bead: "ga-1", routedTo: target, age: 40 * time.Second},
		{seq: 2, typ: "bead.created", bead: "ga-2", routedTo: target, age: 30 * time.Second},
		{seq: 3, typ: "bead.created", bead: "ga-3", routedTo: target, age: 20 * time.Second},
		{seq: 4, typ: "bead.created", bead: "ga-4", routedTo: pool, age: 10 * time.Second},
	}

	t.Run("one nudge per target and one per pool member", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, members)
		r.writeEvents(batch...)
		out := r.run()
		for _, want := range []struct {
			to string
			n  int
		}{{target, 1}, {"pool-1", 1}, {"pool-2", 1}, {pool, 0}} {
			if got, logged := r.nudges(want.to); got != want.n {
				t.Fatalf("nudges to %s = %d, want %d; gc calls:\n%s", want.to, got, want.n, logged)
			}
		}
		if total, logged := countNudges(t, r.logPath); total != 3 {
			t.Fatalf("nudges = %d, want 3; gc calls:\n%s", total, logged)
		}
		for _, key := range []string{"ga-1|" + target, "ga-2|" + target, "ga-3|" + target, "ga-4|" + pool} {
			if !r.hasKey(key) {
				t.Fatalf("dedup state lacks %s", key)
			}
		}
		if !strings.Contains(out, "nudge-on-route: nudged 4 newly-routed bead(s)") {
			t.Fatalf("summary should count the 4 recorded pairs; output:\n%s", out)
		}
	})

	t.Run("a failed target records none of its pairs", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, members)
		r.writeEvents(batch...)
		r.failNudgesTo(target)
		r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudge attempts to %s = %d, want 1; gc calls:\n%s", target, got, logged)
		}
		for _, bead := range []string{"ga-1", "ga-2", "ga-3"} {
			if r.hasKey(bead + "|" + target) {
				t.Fatalf("dedup state records %s|%s after its target's nudge failed", bead, target)
			}
		}
		if !r.hasKey("ga-4|" + pool) {
			t.Fatalf("dedup state lacks ga-4|%s", pool)
		}
		if got := r.seq(); got != "4" {
			t.Fatalf("seq mark = %q, want 4", got)
		}
	})

	t.Run("a known pair does not hold back a new pair for its target", func(t *testing.T) {
		r := newNudgeOnRouteRig(t, true, nil)
		r.seedState("ga-1|" + target)
		r.writeEvents(batch[:2]...)
		r.run()
		if got, logged := r.nudges(target); got != 1 {
			t.Fatalf("nudges to %s = %d, want 1 for the new pair; gc calls:\n%s", target, got, logged)
		}
		if !r.hasKey("ga-1|"+target) || !r.hasKey("ga-2|"+target) {
			t.Fatalf("dedup state should hold both ga-1 and ga-2 for %s", target)
		}
	})
}
