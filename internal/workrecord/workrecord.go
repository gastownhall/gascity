// Package workrecord analyzes bead close-time work-record coverage: which
// gated beads carry a valid gc.work_outcome and which do not.
package workrecord

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
// It applies to worker-claimable work units — plain task beads — and
// deliberately NOT to control/structural beads (anything carrying gc.kind:
// workflow roots, scope/run/check/drain steps, etc.) or non-task beads
// (convoy, message). This mirrors cmd/gc's isWorkRecordGatedBead exactly;
// that function now delegates here.
func IsGatedBead(bead beads.Bead) bool {
	if t := strings.TrimSpace(bead.Type); t != "" && t != "task" {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]) != "" {
		return false
	}
	return true
}

// ValidOutcome reports whether v is one of the four typed work-record close
// dispositions. This mirrors cmd/gc's validWorkOutcome exactly; that function
// now delegates here.
func ValidOutcome(v string) bool {
	switch v {
	case beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp,
		beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned:
		return true
	default:
		return false
	}
}

// AnalyzeCoverage computes work-record coverage across beadList: of the
// gated beads (per IsGatedBead), how many carry a valid gc.work_outcome
// (per ValidOutcome) and which do not.
func AnalyzeCoverage(beadList []beads.Bead) CoverageReport {
	var r CoverageReport
	for _, b := range beadList {
		if !IsGatedBead(b) {
			continue
		}
		r.TotalGated++
		outcome := strings.TrimSpace(b.Metadata[beadmeta.WorkOutcomeMetadataKey])
		if outcome != "" && ValidOutcome(outcome) {
			r.Covered++
		} else {
			r.Missing++
			r.MissingIDs = append(r.MissingIDs, b.ID)
		}
	}
	if r.TotalGated > 0 {
		r.Coverage = float64(r.Covered) / float64(r.TotalGated)
	}
	return r
}
