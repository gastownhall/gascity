package session

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// DispositionLife is the lifecycle a disposition move writes beside the
// class: given the move (from, to) on the fresh row, the keys outside the
// class to write with it, or nil.
type DispositionLife func(from, to Disposition, fresh Info) MetadataPatch

// DispositionWrite is what a disposition move did.
type DispositionWrite struct {
	Result CommitResult
	// To is the disposition the row was left with.
	To Disposition
	// Wrote is the patch written (the class keys the move changed and its
	// lifecycle), and Was the row's values for those keys on the read it
	// was decided on: a rollback restores Was where the row still reads
	// Wrote. Both are nil when nothing was written.
	Wrote, Was MetadataPatch
}

// ApplyDisposition is the one writer of a row's disposition. On each fresh
// read of d's row that still carries d's facts, it decodes the row's
// disposition, moves it by ev from by (Disposition.Apply, the table), and
// writes the class keys the move changed with life's lifecycle keys, in one
// write fenced on that read, up to three attempts. It writes only what
// changed, so a store without conditional writes (maintainer-city, D-I) can
// lose none of a concurrent writer's other keys. A refused cell writes
// nothing and returns its *ErrDispositionRefused; a move that changes
// nothing and writes no lifecycle lands unwritten. It reads the row live, as
// Commit does, and writes as UpdateMetadataFenced does.
func (s *Store) ApplyDisposition(d Decided, by ActorKind, ev DispEvent, life DispositionLife, now time.Time) (DispositionWrite, error) {
	if d.info.ID == "" {
		return DispositionWrite{}, fmt.Errorf("session: disposition move of a zero decision")
	}
	return s.applyDisposition(d.info.ID, d.Match, startCommitMaxAttempts, by, ev, life, now)
}

// ApplyDispositionFresh is ApplyDisposition decided on each fresh read
// itself: an operator's or agent's own act (a suspend, a heartbeat), which
// rests on no earlier read, retried up to operatorSuspendAttempts times, as
// it competes with every controller write.
func (s *Store) ApplyDispositionFresh(id string, by ActorKind, ev DispEvent, life DispositionLife, now time.Time) (DispositionWrite, error) {
	return s.applyDisposition(id, func(Info) bool { return true }, operatorSuspendAttempts, by, ev, life, now)
}

func (s *Store) applyDisposition(id string, holds func(Info) bool, attempts int, by ActorKind, ev DispEvent, life DispositionLife, now time.Time) (DispositionWrite, error) {
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return DispositionWrite{}, fmt.Errorf("updating session %q: %w", id, err)
	}
	for attempt := 0; attempt < attempts; attempt++ {
		b, err := s.freshBead(id)
		if err != nil {
			return DispositionWrite{}, err
		}
		fresh := infoFromPersistedBead(b)
		switch {
		case b.Status == "closed":
			return DispositionWrite{Result: CommitClosed}, nil
		case !holds(fresh):
			return DispositionWrite{Result: CommitMoved}, nil
		}
		patch, to, err := MoveDisposition(b.Metadata, fresh, by, ev, life, now)
		w := DispositionWrite{Result: CommitLanded, To: to}
		switch {
		case err != nil:
			return DispositionWrite{To: to}, err
		case len(patch) == 0:
			return w, nil
		}
		opts := beads.UpdateOpts{Metadata: map[string]string(patch)}
		if writer == nil {
			err = s.store.Update(id, opts)
		} else {
			err = writer.UpdateIfMatch(id, b.Revision, opts)
		}
		switch {
		case err == nil:
			w.Wrote, w.Was = patch, MetadataPatch{}
			for k := range patch {
				w.Was[k] = b.Metadata[k]
			}
			return w, nil
		case writer == nil || !beads.IsPreconditionFailed(err):
			return DispositionWrite{To: to}, fmt.Errorf("updating session %q: %w", id, err)
		}
	}
	return DispositionWrite{Result: CommitContended}, nil
}

// MoveDisposition is ApplyDisposition's decision on one read, for a writer
// that fences its own write: meta's disposition moved by ev from by, and the
// patch that writes it: the class keys the move changed (Disposition.Changed)
// with life's lifecycle keys. The patch is nil when the move changes nothing
// and life writes nothing; a refused cell is its *ErrDispositionRefused.
func MoveDisposition(meta map[string]string, fresh Info, by ActorKind, ev DispEvent, life DispositionLife, now time.Time) (MetadataPatch, Disposition, error) {
	from := DecodeDisposition(meta)
	to, err := from.Apply(by, ev, State(fresh.MetadataState), now)
	if err != nil {
		return nil, from, err
	}
	patch := to.Changed()
	if life != nil {
		for k, v := range life(from, to, fresh) {
			if _, class := to.Encode()[k]; class {
				return nil, from, fmt.Errorf("session: a disposition move's lifecycle writes the class key %q", k)
			}
			patch[k] = v
		}
	}
	if len(patch) == 0 {
		return nil, to, nil
	}
	return patch, to, nil
}

// LifecycleOnly is patch without the disposition class's keys: the part of a
// lifecycle builder's patch a disposition move may write beside its own.
func LifecycleOnly(patch MetadataPatch) MetadataPatch {
	out := make(MetadataPatch, len(patch))
	for k, v := range patch {
		out[k] = v
	}
	for _, k := range dispositionKeys {
		delete(out, k)
	}
	return out
}
