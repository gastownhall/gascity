package delivery

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// SetPhase transitions a bead to the given phase, writing gc.phase and
// appending to gc.phase_history atomically via SetMetadataBatch.
// If the bead is already in the target phase the call is a no-op (idempotent).
// Returns an error for illegal transitions or if the bead does not exist.
func SetPhase(store beads.Store, id, to string) error {
	b, err := store.Get(id)
	if err != nil {
		return fmt.Errorf("delivery.SetPhase: get bead %s: %w", id, err)
	}

	current := b.Metadata[MetaKeyPhase]

	// Idempotent no-op.
	if current == to {
		return nil
	}

	if err := ValidatePhaseTransition(current, to); err != nil {
		return err
	}

	history := appendHistory(b.Metadata[MetaKeyPhaseHistory], current, to)

	return store.SetMetadataBatch(id, map[string]string{
		MetaKeyPhase:        to,
		MetaKeyPhaseHistory: history,
	})
}

// FindDeliveryBeadByPRURL returns the first open bead that has gc.pr_url
// matching prURL and has gc.phase set (i.e. is a delivery bead).
// Returns (bead, true, nil) when found; (zero, false, nil) when not found.
func FindDeliveryBeadByPRURL(store beads.Store, prURL string) (beads.Bead, bool, error) {
	candidates, err := store.ListByMetadata(map[string]string{MetaKeyPRURL: prURL}, 0)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("delivery.FindDeliveryBeadByPRURL: %w", err)
	}

	for _, b := range candidates {
		if b.Metadata[MetaKeyPhase] != "" {
			return b, true, nil
		}
	}
	return beads.Bead{}, false, nil
}

// appendHistory returns the new gc.phase_history value after a from→to transition.
func appendHistory(existing, from, to string) string {
	entry := from + "→" + to
	if existing == "" {
		return entry
	}
	return strings.Join([]string{existing, entry}, ",")
}
