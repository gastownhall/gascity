// Package coordclass is the single source of truth for classifying a bead
// into the coordination class that decides which store and storage policy
// govern it. It lifts the classifier that previously lived inline in
// cmd/gc/bead_policy_store.go (policyNameForBead / policyNameForGraphPlan)
// into a leaf package so the same taxonomy can be shared by the per-class
// store seam without importing cmd/gc.
//
// The Class string values are identical to the policy-name strings the
// classifier returned before the lift, so the storage-tier mapping in
// cmd/gc (effectiveBeadStorage / defaultBeadStorage), which is keyed off
// those strings, keeps working unchanged. The lift changes no behavior.
package coordclass

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// Class identifies the coordination class of a bead. Its string value is the
// policy name used by the storage-tier mapping; an empty Class is the default
// (ordinary work).
type Class string

const (
	// ClassWork is the default class for ordinary work beads. Its empty
	// string value preserves the "no policy" sentinel the classifier
	// returned for unmatched beads.
	ClassWork Class = ""
	// ClassWisp marks root-only wisp molecules.
	ClassWisp Class = "wisp"
	// ClassWorkflow marks graph (v2) workflow beads. Its value is the
	// "workflow" policy-name string.
	ClassWorkflow Class = "workflow"
	// ClassGraph is an alias for ClassWorkflow: the fork calls this class
	// "graph" while the policy string is "workflow". Both resolve to the
	// same value so the storage-tier map stays keyed on "workflow".
	ClassGraph = ClassWorkflow
	// ClassOrderTracking marks order-tracking bookkeeping beads.
	ClassOrderTracking Class = "order_tracking"
	// ClassSession marks chat/agent session beads.
	ClassSession Class = "session"
	// ClassWait marks durable session wait beads.
	ClassWait Class = "wait"
	// ClassNudge marks nudge beads.
	ClassNudge Class = "nudge"
)

const (
	// MarkerNudgeLabel is the label applied to nudge beads. It moved here
	// from cmd/gc so the classifier can read it without importing cmd/gc;
	// cmd/gc re-aliases it as nudgeBeadLabel.
	MarkerNudgeLabel = "gc:nudge"
	// MarkerOrderTrackingLabel is the label applied to order-tracking
	// bookkeeping beads. It moved here from cmd/gc so the classifier can
	// read it without importing cmd/gc; cmd/gc re-aliases it as
	// labelOrderTracking.
	MarkerOrderTrackingLabel = "order-tracking"
)

// Classify returns the coordination class of a bead. It is the verbatim lift
// of cmd/gc's policyNameForBead: the same predicates evaluated in the same
// order (wisp -> order_tracking -> session -> wait -> nudge -> workflow ->
// work), so classification is byte-identical to the pre-lift behavior.
func Classify(b beads.Bead) Class {
	switch {
	case isWispPolicyMetadata(b.Metadata) || b.Type == "wisp" || hasBeadLabel(b.Labels, "gc:wisp") || hasBeadLabel(b.Labels, "wisp"):
		return ClassWisp
	case hasBeadLabel(b.Labels, MarkerOrderTrackingLabel):
		return ClassOrderTracking
	case hasBeadLabel(b.Labels, session.LabelSession) || b.Type == session.BeadType:
		return ClassSession
	case hasBeadLabel(b.Labels, session.WaitBeadLabel):
		return ClassWait
	case hasBeadLabel(b.Labels, MarkerNudgeLabel):
		return ClassNudge
	case isWorkflowPolicyMetadata(b.Metadata):
		return ClassWorkflow
	default:
		return ClassWork
	}
}

// ClassifyGraphPlan returns the coordination class of a graph apply plan. It
// is the verbatim lift of cmd/gc's policyNameForGraphPlan: it returns
// ClassWisp if any node looks like a wisp, then ClassWorkflow if any node
// looks like a workflow, and ClassWork otherwise.
func ClassifyGraphPlan(plan *beads.GraphApplyPlan) Class {
	for _, node := range plan.Nodes {
		if isWispPolicyMetadata(node.Metadata) || hasBeadLabel(node.Labels, "gc:wisp") || hasBeadLabel(node.Labels, "wisp") {
			return ClassWisp
		}
	}
	for _, node := range plan.Nodes {
		if isWorkflowPolicyMetadata(node.Metadata) || isWorkflowPolicyMetadata(node.MetadataRefs) {
			return ClassWorkflow
		}
	}
	return ClassWork
}

func isWispPolicyMetadata(metadata map[string]string) bool {
	return metadata[beadmeta.KindMetadataKey] == beadmeta.KindWisp
}

func isWorkflowPolicyMetadata(metadata map[string]string) bool {
	if metadata == nil {
		return false
	}
	return metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow ||
		metadata[beadmeta.FormulaContractMetadataKey] == beadmeta.FormulaContractGraphV2 ||
		strings.TrimSpace(metadata[beadmeta.RootBeadIDMetadataKey]) != ""
}

func hasBeadLabel(labels []string, label string) bool {
	return slices.Contains(labels, label)
}
