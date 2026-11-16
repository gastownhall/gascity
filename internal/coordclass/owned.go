package coordclass

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// typeStartupHealthEpisode is a session's startup-health episode, which
// holds it down while quarantined. Canonical: session.StartupHealthEpisodeType.
const typeStartupHealthEpisode = "startup-health-episode"

// ControllerOwned reports a bead only the controller and its verbs write: a
// session row (type session, or the gc:session label on any type, a
// crash-damaged row's empty type included), a startup-health episode, a
// convergence root, or an operation-journal row (type op: op and fact rows).
// The generic bead API never writes one, on either side of a write
// (architecture B, mc-lp7zb): the session verbs, the controller's startup
// health, the convergence handler and the op journal's producers and lanes
// do. Type and label are matched whatever their case and surrounding space,
// as a formula's check matches them, so no spelling of one passes for work.
func ControllerOwned(b beads.Bead) bool {
	return ControllerOwnedType(b.Type) || ControllerOwnedLabel(b.Labels) >= 0
}

// ControllerOwnedType reports a type that makes a bead ControllerOwned.
func ControllerOwnedType(typ string) bool {
	t := strings.TrimSpace(typ)
	return strings.EqualFold(t, typeSession) || strings.EqualFold(t, typeStartupHealthEpisode) ||
		strings.EqualFold(t, typeConvergence) || strings.EqualFold(t, typeOp)
}

// ControllerOwnedLabel is the index of the first label that makes a bead
// ControllerOwned (the session label), or -1.
func ControllerOwnedLabel(labels []string) int {
	for i, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), labelSession) {
			return i
		}
	}
	return -1
}
