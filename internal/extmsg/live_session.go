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
