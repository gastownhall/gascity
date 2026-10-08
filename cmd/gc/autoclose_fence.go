package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/beads"
)

// errAutocloseRefused reports a fenced close whose CAS lost to a write that
// landed after the re-read. The decision may still hold; see autocloseRun.
var errAutocloseRefused = errors.New("autoclose: conditional close refused")

// autocloseCloseIfStill closes id with reason only if the store still holds it
// as the autoclose decided. Autoclose decides from reads that may be stale (a
// cached row, a scan-installed row without labels), so it re-reads the row live
// and asks still; a row that moved since (closed, reopened, labeled owned) is
// left alone. Then, by backend:
//
//   - Conditional writes resolved for the store (beads.conditional_writes auto
//     or require on a capable store: SQLite with the revision layout,
//     NativeDoltStore, MemStore, FileStore): the close is a CAS on the re-read
//     revision, so a write landing in between refuses it. The reason commits
//     with the close where the store has an atomic closer
//     (CloseWithMetadataIfMatch); otherwise it is stamped after the close.
//   - Otherwise (conditional writes off, the default on every city today;
//     BdStore on every bd build; the legacy SQLite layouts): the live re-check
//     is the guard, then an unconditional close. That is a deliberate
//     exception to the no-blind-write rule: these backends have no
//     conditional close to fence with, and leaving every autoclose undone on
//     them is worse than a one-round-trip window. A require-mode store that
//     cannot fence refuses instead (never a blind close).
//
// A refused CAS returns errAutocloseRefused, and a failed read or write its
// error, without retrying here: the caller re-runs its whole decision
// (beadCloseAutoclose), since the write that refused the CAS may have changed
// it. A cross-row premise (every convoy member terminal, a parent closed) is
// the caller's, from reads; no row CAS can fence it.
func autocloseCloseIfStill(store beads.Store, id, reason string, still func(beads.Bead) bool, closeUnconditional func() error) (bool, error) {
	fresh, err := beads.HandlesFor(store).Live.Get(id)
	if err != nil {
		return false, err
	}
	if !still(fresh) {
		return false, nil
	}
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil {
		return false, err // require on an incapable store: never a blind close
	}
	if writer == nil || fresh.Revision == 0 {
		return true, closeUnconditional()
	}
	if closer, ok := beads.AtomicConditionalCloserFor(store); ok {
		_, err = closer.CloseWithMetadataIfMatch(id, fresh.Revision, map[string]string{"close_reason": reason})
	} else if err = writer.CloseIfMatch(id, fresh.Revision); err == nil {
		if stampErr := store.SetMetadata(id, "close_reason", reason); stampErr != nil {
			fmt.Fprintf(os.Stderr, "autoclose: closed %s but could not stamp its close reason: %v\n", id, stampErr) //nolint:errcheck // best-effort stderr
		}
	}
	if beads.IsPreconditionFailed(err) {
		return false, errAutocloseRefused
	}
	return err == nil, err
}

// autocloseStill re-reads id live and reports whether still holds for it, for
// a close with no conditional form (a whole attachment subtree). A failed read
// is not a yes.
func autocloseStill(store beads.Store, id string, still func(beads.Bead) bool) (bool, error) {
	fresh, err := beads.HandlesFor(store).Live.Get(id)
	if err != nil {
		return false, err
	}
	return still(fresh), nil
}

// autocloseRun collects what one close-triggered autoclose run left
// undecided. A run that is not finished must not mark its trigger handled:
// the caller re-runs it or leaves it to the autoclose sweep.
type autocloseRun struct {
	// refused: a fenced close lost its CAS (errAutocloseRefused).
	refused bool
	// failed: a read the decision rests on, or a fenced close, failed.
	failed bool
}

// note folds one step's error in. A gone row (ErrNotFound) is an answer, and
// a require-mode refusal is configuration no retry changes; both finish.
func (r *autocloseRun) note(err error) {
	var required *beads.ConditionalWritesRequiredError
	switch {
	case r == nil || err == nil || errors.Is(err, beads.ErrNotFound) || errors.As(err, &required):
	case errors.Is(err, errAutocloseRefused):
		r.refused = true
	default:
		r.failed = true
	}
}

func (r *autocloseRun) merge(o autocloseRun) {
	r.refused = r.refused || o.refused
	r.failed = r.failed || o.failed
}

func (r *autocloseRun) finished() bool { return !r.refused && !r.failed }
