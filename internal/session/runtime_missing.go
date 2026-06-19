package session

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// RuntimeMissingInStore reports whether any open session bead for the agent
// (selected by its agent:<qualified> label) projects the runtime-missing
// lifecycle reason in the given store.
//
// It is pure and read-only — it does not open or close the store — so the
// control-dispatcher rig→city fallback (#3454) can share one implementation
// across every graph-routing entry point (CLI sling, API sling, and the
// re-decoration dispatch deps) instead of duplicating the projection. Any
// lookup failure returns false so routing keeps its normal rig-local binding
// rather than mis-routing on a transient store error.
func RuntimeMissingInStore(store beads.Store, qualifiedName string) bool {
	qualifiedName = strings.TrimSpace(qualifiedName)
	if store == nil || qualifiedName == "" {
		return false
	}
	sessions, err := store.List(beads.ListQuery{
		Type:   BeadType,
		Label:  "agent:" + qualifiedName,
		Status: "open",
	})
	if err != nil {
		return false
	}
	now := time.Now().UTC()
	for _, b := range sessions {
		if LifecycleDisplayReason(b.Status, b.Metadata, now) == LifecycleReasonRuntimeMissing {
			return true
		}
	}
	return false
}
