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
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// FoldStats reports observable signal from a fold pass. DecodeMisses counts
// bead.* events whose payload did not decode to a bead with an id — the exact
// silent-starve signature the run-view RCA identified. A non-zero value on a
// live tail means the projection is dropping bead snapshots and the run view
// will render blank/stale; the caller (the dashboard run tailer) surfaces it.
type FoldStats struct {
	DecodeMisses int
}

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
// applied (so the caller can advance its cursor) and the pass's FoldStats (so a
// silent decode-starve becomes observable rather than a bare continue).
func Apply(into map[string]beads.Bead, evts []events.Event) (lastSeq uint64, stats FoldStats) {
	for i := range evts {
		e := &evts[i]
		if e.Seq > lastSeq {
			lastSeq = e.Seq
		}
		if !beadEventTypes[e.Type] {
			continue
		}
		b, ok := decodeBead(*e)
		if !ok {
			stats.DecodeMisses++
			continue
		}
		if e.Type == events.BeadDeleted {
			delete(into, b.ID)
			continue
		}
		into[b.ID] = b
	}
	return lastSeq, stats
}

// decodeBead extracts a beads.Bead from a bead.* event via the shared canonical
// decoder (beads.DecodeBeadEventPayload) — the raw bead snapshot
// CachingStore.notifyChange emits, with the wrapped {"bead": ...} form accepted
// as a tolerant fallback. A payload without an id is a decode miss.
//
// After decoding, it backfills the run/step correlation ids from the event
// envelope onto the bead's metadata when the envelope carries them and the
// payload does not. The correlation spine promoted run_id/session_id/step_id to
// typed envelope fields and is removing the metadata duplication the run view
// still groups on; backfilling here keeps run/step grouping alive once the
// duplicate metadata is gone. run_id is intentionally NOT synthesized —
// ResolveRunID already falls back to the bead's own id.
func decodeBead(e events.Event) (beads.Bead, bool) {
	b, ok := beads.DecodeBeadEventPayload(e.Payload)
	if !ok {
		return beads.Bead{}, false
	}
	b.Metadata = backfillEnvelopeIDs(b.Metadata, e)
	return b, true
}

// backfillEnvelopeIDs fills the step/session correlation ids from the event
// envelope into a bead metadata map, but only when the envelope carries the id
// and the payload metadata does not (the payload snapshot stays authoritative).
func backfillEnvelopeIDs(md map[string]string, e events.Event) map[string]string {
	md = fillIfAbsent(md, beadmeta.StepIDMetadataKey, e.StepID)
	md = fillIfAbsent(md, beadmeta.SessionIDMetadataKey, e.SessionID)
	return md
}

func fillIfAbsent(md map[string]string, key, value string) map[string]string {
	if value == "" || md[key] != "" {
		return md
	}
	if md == nil {
		md = make(map[string]string, 1)
	}
	md[key] = value
	return md
}
