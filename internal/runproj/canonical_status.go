package runproj

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// CanonicalRunStatus is the closed lifecycle vocabulary shared by the typed
// run API and dashboard-local aggregate projections.
type CanonicalRunStatus string

// Canonical run lifecycle states shared by API and dashboard projections.
const (
	CanonicalRunStatusPending   CanonicalRunStatus = "pending"
	CanonicalRunStatusActive    CanonicalRunStatus = "active"
	CanonicalRunStatusWaiting   CanonicalRunStatus = "waiting"
	CanonicalRunStatusCanceling CanonicalRunStatus = "canceling"
	CanonicalRunStatusCompleted CanonicalRunStatus = "completed"
	CanonicalRunStatusFailed    CanonicalRunStatus = "failed"
	CanonicalRunStatusCanceled  CanonicalRunStatus = "canceled"
	CanonicalRunStatusSkipped   CanonicalRunStatus = "skipped"
)

// CanonicalRunStatusCounts is a complete census of CanonicalRunStatus values.
type CanonicalRunStatusCounts struct {
	Pending   int `json:"pending"`
	Active    int `json:"active"`
	Waiting   int `json:"waiting"`
	Canceling int `json:"canceling"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
	Skipped   int `json:"skipped"`
}

// CanonicalRunCensus is one immutable snapshot published by an incremental run
// projector. Ready distinguishes a real all-zero census from a cold projection
// that has not completed its first replay.
type CanonicalRunCensus struct {
	Ready          bool
	StatusCounts   CanonicalRunStatusCounts
	Partial        bool
	PartialReasons []string
}

// CanonicalRunStatusForLane derives one run's canonical lifecycle state. A
// terminal root is authoritative even when a lingering member keeps the lane
// in an active phase. Without a root, the lane phase and started-member count
// still provide the defensive best-effort state.
func CanonicalRunStatusForLane(lane RunLane, root *beads.Bead, startedCount int) CanonicalRunStatus {
	if root != nil && strings.TrimSpace(root.Status) == "closed" {
		switch strings.TrimSpace(root.Metadata[beadmeta.OutcomeMetadataKey]) {
		case beadmeta.OutcomeFail:
			return CanonicalRunStatusFailed
		case beadmeta.OutcomeSkipped:
			return CanonicalRunStatusSkipped
		case beadmeta.OutcomeCanceled:
			return CanonicalRunStatusCanceled
		default:
			return CanonicalRunStatusCompleted
		}
	}
	if root != nil && strings.TrimSpace(root.Metadata[beadmeta.CancelRequestedMetadataKey]) != "" {
		return CanonicalRunStatusCanceling
	}
	if lane.Phase == "blocked" {
		return CanonicalRunStatusWaiting
	}
	if startedCount == 0 {
		return CanonicalRunStatusPending
	}
	return CanonicalRunStatusActive
}

// CountCanonicalRunStatuses classifies every supplied run lane against the
// same folded bead snapshot. The caller supplies the uncapped lane census from
// BuildRunSummaryWithAllLanes so row limits never truncate the counts.
func CountCanonicalRunStatuses(beadList []beads.Bead, lanes []RunLane) CanonicalRunStatusCounts {
	byID := make(map[string]beads.Bead, len(beadList))
	for i := range beadList {
		byID[beadList[i].ID] = beadList[i]
	}
	startedByRun := canonicalStartedMembersByRun(beadList, lanes)

	var counts CanonicalRunStatusCounts
	for i := range lanes {
		lane := lanes[i]
		root, found := byID[lane.ID]
		var rootPtr *beads.Bead
		if found {
			rootPtr = &root
		}
		switch CanonicalRunStatusForLane(lane, rootPtr, startedByRun[lane.ID]) {
		case CanonicalRunStatusPending:
			counts.Pending++
		case CanonicalRunStatusActive:
			counts.Active++
		case CanonicalRunStatusWaiting:
			counts.Waiting++
		case CanonicalRunStatusCanceling:
			counts.Canceling++
		case CanonicalRunStatusCompleted:
			counts.Completed++
		case CanonicalRunStatusFailed:
			counts.Failed++
		case CanonicalRunStatusCanceled:
			counts.Canceled++
		case CanonicalRunStatusSkipped:
			counts.Skipped++
		}
	}
	return counts
}

func canonicalStartedMembersByRun(beadList []beads.Bead, lanes []RunLane) map[string]int {
	roots := make(map[string]struct{}, len(lanes))
	counts := make(map[string]int, len(lanes))
	for i := range lanes {
		roots[lanes[i].ID] = struct{}{}
		counts[lanes[i].ID] = 0
	}
	for i := range beadList {
		bead := beadList[i]
		status := strings.TrimSpace(bead.Status)
		if status != "in_progress" && status != "closed" {
			continue
		}
		candidates := make(map[string]struct{}, 4)
		for _, rootID := range []string{
			bead.ParentID,
			bead.Metadata[beadmeta.RootBeadIDMetadataKey],
			strings.TrimSpace(bead.Metadata[beadmeta.MoleculeIDMetadataKey]),
		} {
			if _, ok := roots[rootID]; ok {
				candidates[rootID] = struct{}{}
			}
		}
		for offset, char := range bead.ID {
			if char != '.' {
				continue
			}
			if rootID := bead.ID[:offset]; rootID != "" {
				if _, ok := roots[rootID]; ok {
					candidates[rootID] = struct{}{}
				}
			}
		}
		for rootID := range candidates {
			if bead.ID != rootID {
				counts[rootID]++
			}
		}
	}
	return counts
}
