package runproj

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// Projector folds bead lifecycle events into the latest snapshot per id while
// preserving first-seen (creation) order. BuildRunSummary groups by first-seen
// order (mirroring the JS Map insertion order of the dashboard's listBeads
// read), so a plain Fold map — whose Go iteration order is random — would make a
// live run view flicker between requests. The per-city tailer drives a Projector
// instead: a cold ColdLoad over the full log, then incremental Apply of newly
// tailed events, and Beads() hands BuildRunSummary a deterministic slice.
//
// A Projector is not safe for concurrent use; the tailer mutates it from its
// single loop goroutine and publishes the built summary under its own lock.
type Projector struct {
	beads   map[string]beads.Bead
	order   []string
	lastSeq uint64
}

// NewProjector returns an empty projector.
func NewProjector() *Projector {
	return &Projector{beads: make(map[string]beads.Bead)}
}

// ColdLoad folds the entire event log at path (events.ReadFiltered transparently
// walks rotated .gz archives) into the projector. Safe to call once on a fresh
// projector before the incremental tail begins.
func (p *Projector) ColdLoad(path string) error {
	evts, err := events.ReadFiltered(path, events.Filter{})
	if err != nil {
		return err
	}
	p.Apply(evts)
	return nil
}

// Apply folds a chronological event slice, upserting bead.created/updated/closed
// snapshots and removing bead.deleted ones, preserving first-seen order for new
// ids. It advances the cursor past every event (bead or not) and reports whether
// any bead snapshot changed, so the caller can skip a rebuild on a no-op tick.
func (p *Projector) Apply(evts []events.Event) (changed bool) {
	for i := range evts {
		e := &evts[i]
		if e.Seq > p.lastSeq {
			p.lastSeq = e.Seq
		}
		if !beadEventTypes[e.Type] {
			continue
		}
		b, ok := decodeBead(e.Payload)
		if !ok {
			continue
		}
		if e.Type == events.BeadDeleted {
			if _, exists := p.beads[b.ID]; exists {
				delete(p.beads, b.ID)
				p.removeOrder(b.ID)
				changed = true
			}
			continue
		}
		if _, exists := p.beads[b.ID]; !exists {
			p.order = append(p.order, b.ID)
		}
		p.beads[b.ID] = b
		changed = true
	}
	return changed
}

// Beads returns the folded beads in first-seen order — the deterministic input
// BuildRunSummary expects.
func (p *Projector) Beads() []beads.Bead {
	out := make([]beads.Bead, 0, len(p.order))
	for _, id := range p.order {
		if b, ok := p.beads[id]; ok {
			out = append(out, b)
		}
	}
	return out
}

// LastSeq returns the highest event seq applied — the cursor a live tail resumes
// from.
func (p *Projector) LastSeq() uint64 { return p.lastSeq }

func (p *Projector) removeOrder(id string) {
	for i, oid := range p.order {
		if oid == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			return
		}
	}
}
