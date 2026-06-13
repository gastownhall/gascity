package extmsg

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// resolveLiveSessionID maps a stable session name to the current live session
// bead ID, returning session.ErrSessionNotFound when no open session owns the
// name. It is a package-level var so tests can substitute a deterministic
// resolver without standing up real session beads (mirrors timeNow).
var resolveLiveSessionID = session.ResolveSessionID

// overlayLiveSessionID re-points *target at the current live bead for the
// given stable session name. It is a no-op when the stored bead is still the
// active owner of the name (fast path avoiding name-resolution round-trips) or
// when name resolution fails. It mutates *target in place.
//
// "Not closed" alone does not prove the stored bead still owns the name: a
// retired named session is archived without being closed
// (session.RetireNamedSessionPatch clears its identifiers but leaves the bead
// open so historical references can be reassigned). An open-but-identity-released
// bead no longer owns the name, so it must fall through to name resolution;
// otherwise routing keeps targeting the retired bead instead of its respawned
// replacement.
func overlayLiveSessionID(store beads.Store, name, currentID string, target *string) {
	if name == "" {
		return
	}
	if b, err := store.Get(currentID); err == nil && b.Status != "closed" &&
		!session.LifecycleIdentityReleased(b.Status, b.Metadata) {
		return
	}
	liveID, err := resolveLiveSessionID(store, name)
	if err != nil || liveID == "" {
		return
	}
	*target = liveID
}

// sessionNameForSelector resolves a bind selector (a session bead ID, alias,
// or session name) to the stable session name recorded on the target bead.
// Bindings store this name so they can follow the session across respawn.
//
// It is best-effort: on any lookup failure it returns the empty string and the
// binding falls back to pure session-ID behavior. A non-empty result is always
// the bead's recorded session_name, never the raw selector.
func sessionNameForSelector(store beads.Store, selector string) string {
	selector = strings.TrimSpace(selector)
	if store == nil || selector == "" {
		return ""
	}
	id, err := session.ResolveSessionIDAllowClosed(store, selector)
	if err != nil {
		return ""
	}
	bead, err := store.Get(id)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(bead.Metadata["session_name"])
}
