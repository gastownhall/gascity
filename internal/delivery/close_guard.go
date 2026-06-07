package delivery

import (
	"errors"

	"github.com/gastownhall/gascity/internal/beads"
)

// ErrNotInTerminalPhase is returned by ValidateClosePrerequisites when a
// delivery bead (one with gc.phase set) is not yet in a terminal phase.
var ErrNotInTerminalPhase = errors.New("delivery bead not in terminal phase")

// ValidateClosePrerequisites returns ErrNotInTerminalPhase if b is a delivery
// bead (gc.phase is non-empty) and the phase is not terminal. Returns nil for
// non-delivery beads (unset gc.phase) or beads already in a terminal phase.
func ValidateClosePrerequisites(b beads.Bead) error {
	phase := b.Metadata[MetaKeyPhase]
	if phase == "" {
		return nil
	}
	if !IsTerminalPhase(phase) {
		return ErrNotInTerminalPhase
	}
	return nil
}
