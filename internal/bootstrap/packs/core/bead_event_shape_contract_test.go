//go:build unix

package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// beadPayloadShape names one of the two shapes `gc events` prints a bead.*
// event payload in.
type beadPayloadShape string

const (
	// beadPayloadWrapped is the API envelope: a running city's `gc events`
	// re-marshals the stored payload through the registered BeadEventPayload
	// (internal/api/convoy_event_stream.go toWireEvent), so the bead sits
	// under .payload.bead.
	beadPayloadWrapped beadPayloadShape = "wrapped"
	// beadPayloadFlat is the stored raw snapshot: CachingStore.notifyChange
	// writes json.Marshal(bead) and the `gc events` local fallback (stopped
	// city, supervisor 404; cmd/gc/cmd_events.go localWireEvent) passes it
	// through, so the bead fields sit directly under .payload.
	beadPayloadFlat beadPayloadShape = "flat"
)

// beadPayloadShapes lists every shape a shipped consumer must read. Both are
// documented in docs/reference/events.md "Filtering and Shape".
var beadPayloadShapes = []beadPayloadShape{beadPayloadWrapped, beadPayloadFlat}

// beadEventLine renders one `gc events` JSON line for a bead.* event whose
// payload carries bead (a JSON object) in the given shape.
func beadEventLine(shape beadPayloadShape, seq int, eventType, bead string) string {
	payload := bead
	if shape == beadPayloadWrapped {
		payload = `{"bead":` + bead + `}`
	}
	return fmt.Sprintf(`{"seq":%d,"type":%q,"actor":"gc","payload":%s}`, seq, eventType, payload) + "\n"
}

// beadEventConsumer is one shipped core-pack script that reads bead.* events
// from `gc events`, with the single event that must trigger it and the
// single side effect that event must produce.
type beadEventConsumer struct {
	// script is the file name under assets/scripts/.
	script string
	// eventType is the bead.* event the script selects.
	eventType string
	// bead is the event's bead snapshot, as a JSON object.
	bead string
	// envKeys are the inherited settings the run drops (see runPackScript).
	envKeys map[string]struct{}
	// fake returns the fake gc body that prints events as `gc events` output
	// and answers the script's other gc calls.
	fake func(t *testing.T, events string) string
	// sideEffect is the logged gc call prefix the event must produce exactly
	// once.
	sideEffect string
}

// beadEventConsumers is the shape-contract registry: every shipped core-pack
// script that reads bead.* events from `gc events`.
var beadEventConsumers = []beadEventConsumer{
	{
		script:    "nudge-on-route.sh",
		eventType: "bead.updated",
		bead:      `{"id":"ga-routed","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}`,
		envKeys:   nudgeOnRouteEnvKeys,
		fake: func(t *testing.T, events string) string {
			dir := t.TempDir()
			return cascadeFake{eventsPath: writeShapeEvents(t, dir, events), failDir: dir}.body()
		},
		sideEffect: "gc session nudge gascity/builder ",
	},
	{
		script:    "notify-on-human-gate-creation.sh",
		eventType: "bead.created",
		bead:      `{"id":"gcg-1","issue_type":"gate"}`,
		envKeys:   humanGateEnvKeys,
		fake: func(t *testing.T, events string) string {
			return humanGateFake{
				events: events,
				shows:  map[string]string{"gcg-1": `[{"id":"gcg-1","issue_type":"gate","status":"open","metadata":{"gc.gate_type":"human"}}]`},
			}.install(t)
		},
		sideEffect: "gc mail send ",
	},
	{
		script:    "cascade-nudge-on-blocker-close.sh",
		eventType: "bead.closed",
		bead:      `{"id":"ga-blk","status":"closed"}`,
		envKeys:   cascadeNudgeEnvKeys,
		fake: func(t *testing.T, events string) string {
			dir := t.TempDir()
			return cascadeFake{
				eventsPath: writeShapeEvents(t, dir, events),
				depJSON:    `[{"id":"ga-dep","status":"open","assignee":"worker-1"}]`,
				failDir:    dir,
			}.body()
		},
		sideEffect: "gc session nudge worker-1 ",
	},
}

// writeShapeEvents writes events as the fake `gc events` answer under dir and
// returns its path.
func writeShapeEvents(t *testing.T, dir, events string) string {
	t.Helper()
	p := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(p, []byte(events), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	return p
}

// TestBeadEventConsumersReadBothPayloadShapes holds every shipped bead.*
// consumer to both shapes `gc events` prints. Each consumer exits 0 without
// acting when its selector matches nothing, so a consumer that reads one
// shape is silently disabled whenever the other is printed (#5968, #4548).
func TestBeadEventConsumersReadBothPayloadShapes(t *testing.T) {
	requireJQ(t)
	for _, c := range beadEventConsumers {
		for _, shape := range beadPayloadShapes {
			t.Run(c.script+"/"+string(shape), func(t *testing.T) {
				binDir, logPath := fakeGCBin(t, c.fake(t, beadEventLine(shape, 1, c.eventType, c.bead)))
				script := filepath.Join("assets", "scripts", c.script)
				out, err := runPackScript(t, script, binDir, c.envKeys,
					[]string{"GC_PACK_STATE_DIR=" + filepath.Join(t.TempDir(), "state")})
				if err != nil {
					t.Fatalf("%s failed: %v\n%s", c.script, err, out)
				}
				calls := gcLog(t, logPath)
				if got := callsWithPrefix(calls, c.sideEffect); len(got) != 1 {
					t.Fatalf("%s on a %s %s payload: %q calls = %d, want exactly 1; gc calls:\n%s",
						c.script, shape, c.eventType, c.sideEffect, len(got), strings.Join(calls, "\n"))
				}
			})
		}
	}
}
