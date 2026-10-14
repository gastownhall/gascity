package tokenrender

import (
	"strconv"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/connect"
	"github.com/gastownhall/gascity/internal/session"
)

// ineligibleStates are the lifecycle base states that prove no runtime of
// the row's incarnation should be live (DES-CN D6). Every other state,
// unknown ones included, keeps its line: revocation errs toward
// availability, because the bearer is not a containment boundary (D14).
var ineligibleStates = map[session.BaseState]bool{
	session.BaseStateClosed:       true,
	session.BaseStateAsleep:       true,
	session.BaseStateDrained:      true,
	session.BaseStateSuspended:    true,
	session.BaseStateArchived:     true,
	session.BaseStateFailedCreate: true,
	session.BaseStateStopped:      true,
	session.BaseStateQuarantined:  true,
}

// EligibleTokenRow reports whether a session bead earns a bearer line and,
// if so, its row. The base state is session.ProjectLifecycle's projection of
// the bead's status and metadata; the generation is normalized exactly as the
// start path normalizes it (an unparsable or non-positive value becomes
// session.DefaultGeneration), so the line matches the bearer minted for that
// start. A bead id a bearer cannot carry is ineligible.
func EligibleTokenRow(b beads.Bead) (connect.TokenRow, bool) {
	if !connect.ValidSessionID(b.ID) {
		return connect.TokenRow{}, false
	}
	base := session.ProjectLifecycle(session.LifecycleInputFromMetadata(b.Status, b.Metadata)).BaseState
	if ineligibleStates[base] {
		return connect.TokenRow{}, false
	}
	generation, _ := strconv.Atoi(b.Metadata["generation"])
	if generation <= 0 {
		generation = session.DefaultGeneration
	}
	return connect.TokenRow{SessionID: b.ID, Generation: generation}, true
}
