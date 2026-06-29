package coordclass

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Contract strings matched by [Classify]. Each mirrors a canonical definition
// elsewhere in the tree; guard_test.go pins the importable ones against their
// source. They are duplicated (not imported) to keep this package a leaf — the
// boundary function must not pull in session/cmd-gc/extmsg dependency graphs.
const (
	// labelSession marks session identity beads. Canonical: session.LabelSession.
	labelSession = "gc:session"
	// typeSession is the session bead type. Canonical: session.BeadType.
	typeSession = "session"
	// labelWait marks durable session-wait beads. Canonical: session.WaitBeadLabel.
	labelWait = "gc:wait"
	// labelOrderTracking marks order-dispatch tracking beads.
	// Canonical: cmd/gc/order_dispatch.go (labelOrderTracking).
	labelOrderTracking = "order-tracking"
	// labelNudge marks queued-nudge beads.
	// Canonical: cmd/gc/nudge_beads.go (nudgeBeadLabel).
	labelNudge = "gc:nudge"
	// typeMessage is the mail bead type.
	// Canonical: internal/mail/beadmail/beadmail.go (Type: "message").
	typeMessage = "message"
	// labelExtmsgPrefix is the shared prefix of every extmsg family locator
	// label (gc:extmsg-binding, -delivery, -group, -transcript, ...).
	// Canonical: internal/extmsg/labels.go.
	labelExtmsgPrefix = "gc:extmsg-"
	// typeConvoy is the convoy bead type. Convoys are work-class UNLESS marked
	// synthetic (graph.v2 input convoys, drain-unit convoys), which fold into
	// ClassGraph. Canonical: internal/convoy.
	typeConvoy = "convoy"
	// typeConvergence is the convergence-loop root bead type. Convergence roots
	// carry no graph metadata, so they need an explicit arm to fold into
	// ClassGraph (a deliberate decision — they are the convergence engine's
	// execution state). Canonical: cmd/gc/convergence_store.go (Type: "convergence").
	typeConvergence = "convergence"
	// formulaContractGraphV2 is the gc.formula_contract value identifying a
	// compiled graph.v2 workflow. Canonical: bd_policy_store / graphroute use
	// the bare literal "graph.v2".
	formulaContractGraphV2 = "graph.v2"
)

// Classify returns the owning [Class] of a bead.
//
// It is extracted from, and faithful to, the existing
// cmd/gc/bead_policy_store.go policyNameForBead precedence — wisp, then
// order-tracking, session, wait, nudge, workflow — mapped onto the coordclass
// ownership taxonomy (wisp+workflow → ClassGraph, session+wait → ClassSessions,
// order-tracking → ClassOrders, nudge → ClassNudges). It additionally adds
// three arms the policy classifier does NOT have today, all of which previously
// fell through to "" (→ work); each is pinned by the golden table in
// classify_test.go:
//
//   - ClassMessaging for type=message (mail) and gc:extmsg-* (extmsg).
//   - ClassGraph for synthetic convoys (gc.synthetic on a type=convoy bead),
//     so graph.v2 input convoys and drain-unit convoys travel with the graph
//     they glue, while user convoys remain ClassWork.
//   - ClassGraph for convergence roots (type=convergence), folding the
//     convergence engine's state in with the graph it pours.
//
// These are net-new routing decisions and carry real behavior risk. Note that
// Class (which backend) is orthogonal to beads.StorageClass (which tier): during
// the identity-transform migration phases the router keeps storage-tier
// selection on the existing policy path, so folding convergence into ClassGraph
// changes only its destination backend once graph relocates, not its tier today.
//
// Precedence is significant: order-tracking/session/wait/nudge are checked
// before the broad workflow arm (which matches any bead carrying gc.root_bead_id)
// exactly as policyNameForBead does, so a tracker or session bead is never
// misrouted to ClassGraph.
func Classify(b beads.Bead) Class {
	return classifyFields(b.Type, b.Labels, b.Metadata, nil)
}

// ClassifyGraphPlan returns the [Class] for an entire graph-apply plan. A
// graph-apply plan is the atomic instantiation unit for formula-v2 topology, so
// it is routed wholesale: if any node carries graph markers the whole plan is
// ClassGraph (preserving intra-plan edges, including embedded work-typed steps).
// An empty plan is ClassWork; a plan with no graph-marked node is classified by
// its root node as a defensive fallback (this does not occur for real formula
// pours, which always carry workflow/wisp/root metadata).
func ClassifyGraphPlan(plan *beads.GraphApplyPlan) Class {
	if plan == nil || len(plan.Nodes) == 0 {
		return ClassWork
	}
	for _, node := range plan.Nodes {
		if classifyFields(node.Type, node.Labels, node.Metadata, node.MetadataRefs) == ClassGraph {
			return ClassGraph
		}
	}
	root := plan.Nodes[0]
	for _, node := range plan.Nodes {
		if strings.TrimSpace(node.ParentKey) == "" && strings.TrimSpace(node.ParentID) == "" {
			root = node
			break
		}
	}
	return classifyFields(root.Type, root.Labels, root.Metadata, root.MetadataRefs)
}

// classifyFields is the shared decision used by both Classify and
// ClassifyGraphPlan, operating on the raw bead/node fields so it works for a
// beads.Bead and a beads.GraphApplyNode alike. metadataRefs holds a node's
// deferred metadata references (beads.GraphApplyNode.MetadataRefs) and is nil
// for a beads.Bead, whose metadata is already merged into a single map; the
// workflow arm consults both maps so a graph step node whose only
// gc.root_bead_id marker lives in MetadataRefs routes to ClassGraph, matching
// the storage-tier policyNameForGraphPlan classifier.
func classifyFields(beadType string, labels []string, metadata, metadataRefs map[string]string) Class {
	switch {
	case isWispMetadata(metadata) || beadType == beadmeta.KindWisp || hasLabel(labels, "gc:wisp") || hasLabel(labels, "wisp"):
		return ClassGraph
	case beadType == typeMessage || hasLabelPrefix(labels, labelExtmsgPrefix):
		return ClassMessaging
	case hasLabel(labels, labelOrderTracking):
		return ClassOrders
	case hasLabel(labels, labelSession) || beadType == typeSession:
		return ClassSessions
	case hasLabel(labels, labelWait):
		return ClassSessions
	case hasLabel(labels, labelNudge):
		return ClassNudges
	case isWorkflowMetadata(metadata) || isWorkflowMetadata(metadataRefs):
		return ClassGraph
	case beadType == typeConvoy && isSyntheticConvoy(metadata):
		return ClassGraph
	case beadType == typeConvergence:
		return ClassGraph
	default:
		return ClassWork
	}
}

// isWispMetadata mirrors bead_policy_store.isWispPolicyMetadata.
func isWispMetadata(metadata map[string]string) bool {
	return metadata[beadmeta.KindMetadataKey] == beadmeta.KindWisp
}

// isWorkflowMetadata mirrors bead_policy_store.isWorkflowPolicyMetadata: a bead
// is graph.v2 topology if it is a workflow root, declares the graph.v2
// contract, or carries any gc.root_bead_id (i.e. it is a node of some graph).
// The last clause is what captures the explosion's per-node child beads
// regardless of their varied types.
func isWorkflowMetadata(metadata map[string]string) bool {
	if metadata == nil {
		return false
	}
	return metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow ||
		metadata[beadmeta.FormulaContractMetadataKey] == formulaContractGraphV2 ||
		strings.TrimSpace(metadata[beadmeta.RootBeadIDMetadataKey]) != ""
}

// isSyntheticConvoy reports whether a convoy bead is system-minted glue
// (graph.v2 input convoy or drain-unit convoy) rather than a human/sling convoy.
func isSyntheticConvoy(metadata map[string]string) bool {
	if metadata == nil {
		return false
	}
	if strings.TrimSpace(metadata[beadmeta.SyntheticMetadataKey]) != "" {
		return true
	}
	return strings.TrimSpace(metadata[beadmeta.SyntheticKindMetadataKey]) != ""
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func hasLabelPrefix(labels []string, prefix string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}
