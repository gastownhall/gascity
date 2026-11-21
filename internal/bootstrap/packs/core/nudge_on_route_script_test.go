package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nudgeOnRouteScriptPath is the on-disk copy of the nudge-on-route order's
// script. The test runs it, so it needs a real path.
const nudgeOnRouteScriptPath = "assets/scripts/nudge-on-route.sh"

// nudgeOnRouteEnvKeys are the settings that would point the script at a live
// session's city, state, or trace file. The test supplies its own state dir.
var nudgeOnRouteEnvKeys = map[string]struct{}{
	"GC_CITY":                     {},
	"GC_CITY_RUNTIME_DIR":         {},
	"GC_PACK_STATE_DIR":           {},
	"GC_BD_TRACE_JSON":            {},
	"GC_NUDGE_ON_ROUTE_LOOKBACK":  {},
	"GC_NUDGE_ON_ROUTE_RETENTION": {},
	"GC_NUDGE_ON_ROUTE_MESSAGE":   {},
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
