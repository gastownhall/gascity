package main

import (
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/beads"
)

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
//     is the guard. It narrows the window to one round trip but cannot close
//     it.
//
// A refused close is not retried: the next close trigger, or the autoclose
// sweep's next departure, re-decides. A cross-row premise (every convoy member
// terminal, a parent closed) is the caller's, from reads; no row CAS can fence
// it.
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
		return false, nil
	}
	return err == nil, err
}

// autocloseStill re-reads id live and reports whether still holds for it, for
// a close with no conditional form (a whole attachment subtree).
func autocloseStill(store beads.Store, id string, still func(beads.Bead) bool) bool {
	fresh, err := beads.HandlesFor(store).Live.Get(id)
	return err == nil && still(fresh)
}
