package session

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// DecideFresh reads id's row live and decides on it, resting on site's
// facts: an act decided on the row as it is now.
func (s *Store) DecideFresh(id string, site FactsSite) (Decided, error) {
	b, err := s.freshBead(id)
	if err != nil {
		return Decided{}, err
	}
	return Decide(infoFromPersistedBead(b), site), nil
}

// DispositionLife is the lifecycle a disposition move writes beside the
// class: given the move (from, to) on the fresh row, the keys outside the
// class to write with it, or nil.
type DispositionLife func(from, to Disposition, fresh Info) MetadataPatch

// ApplyDisposition is the one writer of a row's disposition. On each fresh
// read of d's row that still carries d's facts, it decodes the row's
// disposition, moves it by ev from by (Disposition.Apply, the table), and
// writes the whole class encoded with life's lifecycle keys, in one write
// fenced on that read. A refused cell writes nothing and returns its
// *ErrDispositionRefused; a move that changes nothing and writes no
// lifecycle lands unwritten. It returns the disposition the row was left
// with. It reads the row live, as Commit does, and writes as
// UpdateMetadataFenced does.
func (s *Store) ApplyDisposition(d Decided, by ActorKind, ev DispEvent, life DispositionLife, now time.Time) (CommitResult, Disposition, error) {
	if d.info.ID == "" {
		return 0, Disposition{}, fmt.Errorf("session: disposition move of a zero decision")
	}
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return 0, Disposition{}, fmt.Errorf("updating session %q: %w", d.info.ID, err)
	}
	for attempt := 0; attempt < startCommitMaxAttempts; attempt++ {
		b, err := s.freshBead(d.info.ID)
		if err != nil {
			return 0, Disposition{}, err
		}
		fresh := infoFromPersistedBead(b)
		switch {
		case b.Status == "closed":
			return CommitClosed, Disposition{}, nil
		case !d.Match(fresh):
			return CommitMoved, Disposition{}, nil
		}
		patch, to, err := MoveDisposition(b.Metadata, fresh, by, ev, life, now)
		switch {
		case err != nil:
			return 0, to, err
		case len(patch) == 0:
			return CommitLanded, to, nil
		}
		opts := beads.UpdateOpts{Metadata: map[string]string(patch)}
		if writer == nil {
			if err := s.store.Update(d.info.ID, opts); err != nil {
				return 0, to, err
			}
			return CommitLanded, to, nil
		}
		switch err := writer.UpdateIfMatch(d.info.ID, b.Revision, opts); {
		case err == nil:
			return CommitLanded, to, nil
		case !beads.IsPreconditionFailed(err):
			return 0, to, fmt.Errorf("updating session %q: %w", d.info.ID, err)
		}
	}
	return CommitContended, Disposition{}, nil
}

// MoveDisposition is ApplyDisposition's decision on one read, for a writer
// that fences its own write: meta's disposition moved by ev from by, and
// the patch that writes it, the whole class encoded with life's lifecycle
// keys. The patch is nil when the move changes nothing and life writes
// nothing; a refused cell is its *ErrDispositionRefused.
func MoveDisposition(meta map[string]string, fresh Info, by ActorKind, ev DispEvent, life DispositionLife, now time.Time) (MetadataPatch, Disposition, error) {
	from := DecodeDisposition(meta)
	to, err := from.Apply(by, ev, State(fresh.MetadataState), now)
	if err != nil {
		return nil, from, err
	}
	var lifePatch MetadataPatch
	if life != nil {
		lifePatch = life(from, to, fresh)
	}
	if to == from && len(lifePatch) == 0 {
		return nil, to, nil
	}
	patch := to.Encode()
	for k, v := range lifePatch {
		if _, class := patch[k]; class {
			return nil, from, fmt.Errorf("session: a disposition move's lifecycle writes the class key %q", k)
		}
		patch[k] = v
	}
	return patch, to, nil
}
