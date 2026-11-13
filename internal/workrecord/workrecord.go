// Package workrecord analyzes bead close-time work-record coverage: which
// gated beads carry a valid gc.work_outcome and which do not.
package workrecord

import (
	"github.com/gastownhall/gascity/internal/beads"
)

// CoverageReport summarizes work-record coverage across a set of beads.
type CoverageReport struct {
	TotalGated int      `json:"total_gated"`
	Covered    int      `json:"covered"`
	Missing    int      `json:"missing"`
	Coverage   float64  `json:"coverage"`
	MissingIDs []string `json:"missing_ids,omitempty"`
}

// IsGatedBead reports whether bead is subject to the work-record close gate.
func IsGatedBead(_ beads.Bead) bool {
	return false
}

// ValidOutcome reports whether v is a recognized gc.work_outcome value.
func ValidOutcome(_ string) bool {
	return false
}

// AnalyzeCoverage computes work-record coverage across beadList.
func AnalyzeCoverage(_ []beads.Bead) CoverageReport {
	return CoverageReport{}
}
