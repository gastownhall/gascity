package delivery

import (
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// M2 metadata keys written by the delivery-warden (vp-krai).
const (
	MetaKeyPhaseEnteredAt  = "gc.phase_entered_at"
	MetaKeyWardenRetries   = "gc.warden_retries"
	MetaKeyWardenEscalated = "gc.warden_escalated"
)

// PhaseBudgets maps each non-terminal delivery phase to its maximum allowed
// dwell time. Phases absent from this map have no enforced budget.
var PhaseBudgets = map[string]time.Duration{
	PhaseBuilding:        10 * time.Minute,
	PhaseCIPending:       30 * time.Minute,
	PhaseReviewPending:   60 * time.Minute,
	PhaseRework:          30 * time.Minute,
	PhaseDecisionPending: 20 * time.Minute,
	PhaseMergePending:    15 * time.Minute,
	PhaseConflicted:      60 * time.Minute,
}

// PhaseEnteredAt returns the time the bead entered its current phase.
// It reads gc.phase_entered_at as RFC3339 first (human-set), then as Unix
// seconds (written by the delivery-warden); falls back to b.UpdatedAt if
// absent or unparseable; falls back to b.CreatedAt if UpdatedAt is zero.
func PhaseEnteredAt(b beads.Bead) time.Time {
	if raw, ok := b.Metadata[MetaKeyPhaseEnteredAt]; ok && raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
		if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return time.Unix(unix, 0)
		}
	}
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
}

// PhaseAge returns time.Since(PhaseEnteredAt(b)).
func PhaseAge(b beads.Bead) time.Duration {
	return time.Since(PhaseEnteredAt(b))
}
