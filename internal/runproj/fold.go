// Package runproj projects the dashboard run view from a city's append-only
// event log (.gc/events.jsonl) — the OSS-local analog of the hosted ClickHouse
// run projection. It folds bead lifecycle events into the latest bead snapshot
// per id and (in later phases) builds the RunSummary and run-detail off that
// fold, so the run view no longer scans the beads molecule history.
//
// Layering: this is object-model-layer code. It depends only on internal/beads
// and internal/events, never on the API or CLI layers, so the same projection
// can back any consumer.
package runproj

import (
	"encoding/json"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// beadEventTypes are the event types the fold consumes; everything else is
// ignored. Kept as a set so callers can pre-filter a read if they want.
var beadEventTypes = map[string]bool{
	events.BeadCreated: true,
	events.BeadUpdated: true,
	events.BeadClosed:  true,
	events.BeadDeleted: true,
}

// Fold reduces a chronological (seq-ordered) event slice to the latest bead
// snapshot per id. bead.created/updated/closed upsert the snapshot;
// bead.deleted removes it. Non-bead events are ignored. The result is the input
// to buildRunSummary / buildRunDetail.
func Fold(evts []events.Event) map[string]beads.Bead {
	out := make(map[string]beads.Bead)
	Apply(out, evts)
	return out
}

// Apply folds evts into an existing bead map in place (the live-tail path:
// apply newly-watched events to the warm snapshot). Returns the highest seq
// applied, so the caller can advance its cursor.
func Apply(into map[string]beads.Bead, evts []events.Event) (lastSeq uint64) {
	for i := range evts {
		e := &evts[i]
		if e.Seq > lastSeq {
			lastSeq = e.Seq
		}
		if !beadEventTypes[e.Type] {
			continue
		}
		b, ok := decodeBead(e.Payload)
		if !ok {
			continue
		}
		if e.Type == events.BeadDeleted {
			delete(into, b.ID)
			continue
		}
		into[b.ID] = b
	}
	return lastSeq
}

// FoldFile reads the event log at path (events.ReadFiltered transparently walks
// rotated .gz archives) and folds it, returning the bead map and the highest
// seq seen — the cursor a live tailer resumes from.
func FoldFile(path string) (beadsByID map[string]beads.Bead, lastSeq uint64, err error) {
	evts, err := events.ReadFiltered(path, events.Filter{})
	if err != nil {
		return nil, 0, err
	}
	out := make(map[string]beads.Bead)
	lastSeq = Apply(out, evts)
	return out, lastSeq, nil
}

// decodeBead extracts a beads.Bead from a bead.* event payload. The current
// payload shape is {"bead": <snapshot>}; older logs wrote the raw snapshot
// directly, so both are accepted. A payload without an id is treated as a
// decode miss.
func decodeBead(payload json.RawMessage) (beads.Bead, bool) {
	if len(payload) == 0 {
		return beads.Bead{}, false
	}
	var env struct {
		Bead beads.Bead `json:"bead"`
	}
	if err := json.Unmarshal(payload, &env); err == nil && env.Bead.ID != "" {
		return env.Bead, true
	}
	var raw beads.Bead
	if err := json.Unmarshal(payload, &raw); err == nil && raw.ID != "" {
		return raw, true
	}
	return beads.Bead{}, false
}
