// Package delivery implements the PR phase state machine for the autonomous
// delivery pipeline. Phase transitions are the sole mutation path for gc.phase
// metadata — callers must never write gc.phase directly via SetMetadata.
package delivery

// Phase state constants for the PR delivery state machine.
const (
	PhaseBuilding        = "building"
	PhaseCIPending       = "ci-pending"
	PhaseReviewPending   = "review-pending"
	PhaseRework          = "rework"
	PhaseDecisionPending = "decision-pending"
	PhaseMergePending    = "merge-pending"
	PhaseConflicted      = "conflicted"
	PhaseMerged          = "merged"    // terminal
	PhaseAbandoned       = "abandoned" // terminal
)

// Metadata key constants written to bead records by the delivery package.
const (
	MetaKeyPhase        = "gc.phase"
	MetaKeyPhaseHistory = "gc.phase_history"
	MetaKeyPRURL        = "gc.pr_url"
)

// IsTerminalPhase reports whether p is a terminal phase (merged or abandoned).
func IsTerminalPhase(p string) bool {
	return p == PhaseMerged || p == PhaseAbandoned
}

// validTransitions encodes the legal phase adjacency table.
// An empty from-phase ("") is the bootstrap edge — a bead with no gc.phase yet
// may only transition to building.
var validTransitions = map[string]map[string]bool{
	"": {
		PhaseBuilding: true,
	},
	PhaseBuilding: {
		PhaseCIPending: true,
		PhaseAbandoned: true,
	},
	PhaseCIPending: {
		PhaseReviewPending: true,
		PhaseRework:        true,
		PhaseConflicted:    true,
		PhaseAbandoned:     true,
	},
	PhaseReviewPending: {
		PhaseRework:          true,
		PhaseDecisionPending: true,
		PhaseAbandoned:       true,
	},
	PhaseRework: {
		PhaseBuilding:  true,
		PhaseCIPending: true,
		PhaseAbandoned: true,
	},
	PhaseDecisionPending: {
		PhaseMergePending: true,
		PhaseRework:       true,
		PhaseAbandoned:    true,
	},
	PhaseMergePending: {
		PhaseMerged:     true,
		PhaseRework:     true,
		PhaseConflicted: true,
	},
	PhaseConflicted: {
		PhaseRework:    true,
		PhaseAbandoned: true,
	},
	PhaseMerged:    {},
	PhaseAbandoned: {},
}

// ValidatePhaseTransition returns an error if transitioning from → to is not
// a legal edge in the state machine. A nil error means the transition is allowed.
func ValidatePhaseTransition(from, to string) error {
	allowed, fromKnown := validTransitions[from]
	if !fromKnown {
		return &phaseTransitionError{from: from, to: to, reason: "unknown source phase"}
	}
	if !allowed[to] {
		return &phaseTransitionError{from: from, to: to, reason: "transition not allowed"}
	}
	return nil
}

type phaseTransitionError struct {
	from, to, reason string
}

func (e *phaseTransitionError) Error() string {
	return "delivery: invalid phase transition " + e.from + " → " + e.to + ": " + e.reason
}
