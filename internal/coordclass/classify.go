package coordclass

import (
	"slices"
	"sort"
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
	// labelWisp and labelGCWisp mark wisps (graph class). The bare form is
	// also an order-tracking OUTCOME marker; see classifyFields.
	labelWisp   = "wisp"
	labelGCWisp = "gc:wisp"
	// labelNudge marks queued-nudge beads.
	// Canonical: cmd/gc/nudge_beads.go (nudgeBeadLabel).
	labelNudge = "gc:nudge"
	// labelNudgeQueue marks the DURABLE QUEUE's own beads, a family disjoint
	// from the gc:nudge shadow beads above.
	// Canonical: internal/storebinding/beads_nudge_queue.go (nudgeQueueLabel);
	// pinned behaviorally against it by classify_nudge_queue_test.go, which
	// lives in the external test package because storebinding imports this one.
	//
	// It needs its own arm because the nudge arm matches EXACTLY: "gc:nudge-queue"
	// is not "gc:nudge", so a queue bead classified as ClassWork — a bead
	// NewBeadsNudgeQueue physically writes into the nudges-class store, answering
	// to the class that never holds it. That mismatch is not cosmetic: the
	// infra-class migration selects its snapshot with a not-Work filter
	// (cmd/gc/infra_class_migrate.go readInfraSnapshot), so queue beads were
	// excluded from the copy and left behind in the work store. Fixing the arm
	// makes that migration carry them, and makes an already-converged city's
	// containment re-check name the ones it stranded — which is the true report.
	labelNudgeQueue = "gc:nudge-queue"
	// typeOp is the conversation operation journal's row type, carried by
	// op, fact and runtime rows alike. Canonical: internal/opjournal
	// (BeadType).
	typeOp = "op"
	// labelOp marks an op row. Canonical: internal/opjournal (LabelOp).
	labelOp = "gc:op"
	// labelFact marks a fact row. Canonical: internal/opjournal (LabelFact).
	labelFact = "gc:fact"
	// labelRuntime marks a conversation's runtime row. Canonical:
	// internal/opjournal (LabelRuntime).
	labelRuntime = "gc:rt"
	// typeMessage is the mail bead type.
	// Canonical: internal/mail/beadmail/beadmail.go (Type: "message").
	typeMessage = "message"
	// labelExtmsgPrefix is the shared prefix of every extmsg family locator
	// label (gc:extmsg-binding, -delivery, -group, -transcript, ...).
	// Canonical: internal/extmsg/labels.go.
	labelExtmsgPrefix = "gc:extmsg-"
	// typeConvoy is the convoy bead type. EVERY convoy is work class, including
	// the synthetic ones the system mints as glue (graph.v2 input convoys,
	// drain-unit convoys). Canonical: internal/convoy.
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
//   - ClassWork for synthetic convoys (gc.synthetic / gc.synthetic_kind on a
//     type=convoy bead), stated as an explicit arm rather than left to the
//     default — see isSyntheticConvoy for why the arm has to be explicit and
//     why it has to run before the workflow arm.
//   - ClassGraph for convergence roots (type=convergence), folding the
//     convergence engine's state in with the graph it pours.
//
// Op, fact and runtime rows of the conversation operation journal (type
// "op", or the gc:op / gc:fact / gc:rt label) route to ClassSessions, so they
// live in the same store as the conversation rows they belong to. The arm runs
// right after the wait arm, ahead of the nudge, synthetic-convoy and workflow
// arms, so a stray gc.root_bead_id on an op never moves it to the graph.
//
// The nudge arm additionally matches the durable queue's own family
// (gc:nudge-queue) beside the shadow family (gc:nudge). That was a defect
// rather than a design choice — see labelNudgeQueue.
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
	// The order-tracking exclusion is the one deliberate divergence from
	// cmd/gc's policyNameForBead, and it fixes a real collision rather than
	// introducing one. orders.RunOutcomeWisp / WispFailed / WispCanceled stamp
	// the BARE "wisp" label on the tracking bead as an OUTCOME marker — it
	// records that the run dispatched a wisp, not that the bead IS one. The
	// dispatched wisp is its own bead, carrying gc:wisp or the wisp kind and no
	// order-tracking label, so it still classifies as graph here.
	//
	// Without the exclusion the wisp arm ran first and took the tracking bead,
	// so a wisp-dispatched run answered to the graph class while ListTracking
	// and RecentRunsAll read the orders leg only — on a class-split city those
	// runs were invisible to them. Excluding order-tracking is narrower than
	// reordering the arms: every other bead the wisp arm claims still reaches it
	// first.
	case !hasLabel(labels, labelOrderTracking) &&
		(isWispMetadata(metadata) || beadType == beadmeta.KindWisp || hasLabel(labels, labelGCWisp) || hasLabel(labels, labelWisp)):
		return ClassGraph
	case beadType == typeMessage || hasLabelPrefix(labels, labelExtmsgPrefix):
		return ClassMessaging
	case hasLabel(labels, labelOrderTracking):
		return ClassOrders
	case hasLabel(labels, labelSession) || beadType == typeSession:
		return ClassSessions
	case hasLabel(labels, labelWait):
		return ClassSessions
	case beadType == typeOp || hasLabel(labels, labelOp) || hasLabel(labels, labelFact) ||
		hasLabel(labels, labelRuntime):
		return ClassSessions
	case hasLabel(labels, labelNudge) || hasLabel(labels, labelNudgeQueue):
		return ClassNudges
	// Ahead of the workflow arm on purpose: a synthetic convoy is glue for a
	// graph and can carry that graph's markers, and the workflow arm would take
	// it on any one of them. See isSyntheticConvoy.
	case beadType == typeConvoy && isSyntheticConvoy(metadata):
		return ClassWork
	case isWorkflowMetadata(metadata) || isWorkflowMetadata(metadataRefs):
		return ClassGraph
	case beadType == typeConvergence:
		return ClassGraph
	default:
		return ClassWork
	}
}

// isWispMetadata mirrors bead_policy_store.isWispPolicyMetadata.
func isWispMetadata(metadata map[string]string) bool {
	return classMeta(metadata, beadmeta.KindMetadataKey) == beadmeta.KindWisp
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
	return classMeta(metadata, beadmeta.KindMetadataKey) == beadmeta.KindWorkflow ||
		classMeta(metadata, beadmeta.FormulaContractMetadataKey) == formulaContractGraphV2 ||
		strings.TrimSpace(classMeta(metadata, beadmeta.RootBeadIDMetadataKey)) != ""
}

// isSyntheticConvoy reports whether a convoy bead is system-minted glue
// (graph.v2 input convoy or drain-unit convoy) rather than a human/sling convoy.
//
// It exists to route those convoys to ClassWork, which is where the default
// would put them anyway — so the arm looks redundant and is not. Two things
// make it load-bearing:
//
// It has to run BEFORE the workflow arm. A synthetic convoy is minted as glue
// for a specific graph and carries that graph's identifying metadata (a
// drain-unit convoy names its control and its parent convoy; nothing stops a
// future one from carrying gc.root_bead_id, which is all the workflow arm
// needs). Left to the default, the classification of a convoy would depend on
// which incidental keys happen to be stamped on it.
//
// And a misclassification here is not a routing preference, it is a hard
// failure. A convoy exists to hold `tracks` edges to its members, its members
// are work beads, and convoy.TrackItemIn refuses an edge whose member is owned
// by another class because a dep row cannot reference an id its own store
// cannot resolve. A synthetic convoy classified anywhere but Work is a convoy
// that can never track anything.
func isSyntheticConvoy(metadata map[string]string) bool {
	if metadata == nil {
		return false
	}
	if strings.TrimSpace(classMeta(metadata, beadmeta.SyntheticMetadataKey)) != "" {
		return true
	}
	return strings.TrimSpace(classMeta(metadata, beadmeta.SyntheticKindMetadataKey)) != ""
}

// The class-relevant vocabulary: every label, label prefix and metadata key
// Classify can see. It is the ONE table both Classify and Mutation.TouchesClass
// read. Classify reads labels and metadata only through hasLabel,
// hasLabelPrefix and classMeta, and each of those answers as if a label, prefix
// or key outside the table were absent. So a new arm that reads something
// outside the table never fires (and the golden table fails), and anything a
// write touches outside the table cannot change a class — which is what lets
// TouchesClass skip the before-read for it.
var (
	classifierLabels = map[string]bool{
		labelWisp: true, labelGCWisp: true, labelOrderTracking: true, labelSession: true,
		labelWait: true, labelOp: true, labelFact: true, labelRuntime: true,
		labelNudge: true, labelNudgeQueue: true,
	}
	classifierLabelPrefixes = []string{labelExtmsgPrefix}
	classifierMetadataKeys  = map[string]bool{
		beadmeta.KindMetadataKey: true, beadmeta.FormulaContractMetadataKey: true,
		beadmeta.RootBeadIDMetadataKey: true, beadmeta.SyntheticMetadataKey: true,
		beadmeta.SyntheticKindMetadataKey: true,
	}
)

// ClassifierLabels returns the labels Classify matches exactly. It and its two
// siblings return copies of the class-relevant vocabulary, for tests that
// enumerate it.
func ClassifierLabels() []string { return sortedKeys(classifierLabels) }

// ClassifierLabelPrefixes returns the label prefixes Classify matches.
func ClassifierLabelPrefixes() []string { return append([]string(nil), classifierLabelPrefixes...) }

// ClassifierMetadataKeys returns the metadata keys Classify reads.
func ClassifierMetadataKeys() []string { return sortedKeys(classifierMetadataKeys) }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isClassifierLabel reports whether label l is in the class-relevant
// vocabulary.
func isClassifierLabel(l string) bool {
	if classifierLabels[l] {
		return true
	}
	for _, p := range classifierLabelPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// classMeta reads metadata key k as Classify sees it: "" for a key outside the
// table.
func classMeta(metadata map[string]string, k string) string {
	if !classifierMetadataKeys[k] {
		return ""
	}
	return metadata[k]
}

func hasLabel(labels []string, want string) bool {
	if !classifierLabels[want] {
		return false
	}
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func hasLabelPrefix(labels []string, prefix string) bool {
	if !slices.Contains(classifierLabelPrefixes, prefix) {
		return false
	}
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}
