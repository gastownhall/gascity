package sling

import (
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// closeSyntheticInputConvoy best-effort-closes the synthetic input convoy a
// failed pour minted for targetID, so an aborted instantiation does not
// strand an open claim-attracting bead (observed as accumulating
// "input convoy for <bead>" debris when every pour of a formula failed at
// the same stage). Only the pour's own artifact is closed: a caller-provided
// convoy target (convoyID == targetID), an empty id, a bead that is not a
// synthetic convoy, or an already-terminal convoy is left untouched. The
// pour's original error is the failure to surface, so close errors are
// ignored.
func closeSyntheticInputConvoy(store beads.Store, convoyID, targetID string) {
	if store == nil || convoyID == "" || convoyID == targetID {
		return
	}
	b, err := store.Get(convoyID)
	if err != nil || b.Type != "convoy" || b.Metadata[beadmeta.SyntheticMetadataKey] != "true" {
		return
	}
	if convoycore.IsTerminalStatus(b.Status) {
		return
	}
	_ = store.Close(convoyID) //nolint:errcheck // best-effort cleanup of the pour's own artifact
}
