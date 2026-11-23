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

// AnalyzeCoverage computes work-record coverage across beadList: of the
// gated beads (per Gated), how many carry a valid gc.work_outcome (per
// ValidOutcome) and which do not.
func AnalyzeCoverage(beadList []beads.Bead) CoverageReport {
	var r CoverageReport
	for _, b := range beadList {
		if !Gated(b) {
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
