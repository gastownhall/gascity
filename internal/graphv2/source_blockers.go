package graphv2

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// ReadyBlockingEdges returns beadID's ready-blocking dependency edges in store
// (beads.IsReadyBlockingDependencyType), each as a blocker of its own edge
// type: the target trimmed, an empty target or beadID itself skipped, and one
// blocker per target, in the order the store lists the edges. It is the one
// reader of the blockers a workflow launched for a work bead inherits, for a
// sling launch (SourceBlockers) and a convoy drain's item workflow alike.
func ReadyBlockingEdges(store beads.StoreReader, beadID string) ([]molecule.Blocker, error) {
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		return nil, fmt.Errorf("listing dependencies of %s: %w", beadID, err)
	}
	var edges []molecule.Blocker
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		target := strings.TrimSpace(dep.DependsOnID)
		if target == "" || target == beadID || slices.ContainsFunc(edges, func(b molecule.Blocker) bool { return b.ID == target }) {
			continue
		}
		edges = append(edges, molecule.Blocker{ID: target, Type: dep.Type})
	}
	return edges, nil
}

// EdgeSatisfied reports whether target's own state already satisfies
// blocker's edge onto it, so a launched workflow need not carry the edge. A
// blocks or conditional-blocks edge is judged by the rule every store applies
// to it (beads.DependencySatisfied). A waits-for edge never is: bd and native
// Dolt judge it by the target's open children, not the target's status, so the
// edge is kept for the store to judge by those children.
func EdgeSatisfied(blocker molecule.Blocker, target beads.Bead) bool {
	if blocker.Type == waitsForDepType {
		return false
	}
	return beads.DependencySatisfied(target.Status, beads.ReadinessWorkOutcome(target.Metadata))
}

// waitsForDepType is the fanout-gate dependency type, which a store judges by
// its target's children rather than by the target itself.
const waitsForDepType = "waits-for"

// SourceBlockers returns the blockers a workflow launched from sourceID must
// wait for: sourceID's ready-blocking edges (ReadyBlockingEdges), read from
// work, the store that holds sourceID, sorted by id. Each keeps its edge's
// type.
//
// A graph workflow routes its own steps rather than the source bead, so the
// source bead's edges hold nothing once the workflow is launched. Copying
// these edges onto the workflow's beads (molecule.Options.BlockedBy) holds the
// workflow on the source bead's own edges. A copy keeps its edge's type but
// not its payload. bd reads a waits-for edge's gate from the payload, so it
// reads the copy as a plain all-children gate, and a source edge carrying
// gate = "any-children" or also_blocks holds the source differently from its
// copy. A hold the source bead takes from a blocked parent, which bd
// propagates down parent-child edges, is not one of its own edges and is not
// copied.
//
// An edge its target already satisfies (EdgeSatisfied) is dropped, and so is
// one onto sourceID's own workflow root: a replacement workflow never waits on
// the one it replaces. Every other edge is kept as written, including one onto
// an external ref or a target work does not hold, so the store judges its
// target as it judges the target of sourceID's own edge.
func SourceBlockers(work beads.WorkStore, sourceID string) ([]molecule.Blocker, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, nil
	}
	edges, err := ReadyBlockingEdges(work, sourceID)
	if err != nil || len(edges) == 0 {
		return nil, err
	}
	held, err := beads.GetMany(work, molecule.BlockerIDs(edges))
	if err != nil {
		return nil, fmt.Errorf("reading blockers of %s: %w", sourceID, err)
	}
	own := ownWorkflowRoots{work: work, sourceID: sourceID}
	blockers := make([]molecule.Blocker, 0, len(edges))
	for _, edge := range edges {
		target, ok := held[edge.ID]
		if ok && EdgeSatisfied(edge, target) {
			continue
		}
		if ok {
			isOwn, err := own.contains(target)
			if err != nil {
				return nil, err
			}
			if isOwn {
				continue
			}
		}
		blockers = append(blockers, edge)
	}
	return sortedBlockers(blockers), nil
}

// WorkflowBlockers returns the blockers the workflow rooted at rootID still
// waits for: the edges its launch wired onto every bead it created, root
// included (molecule.Options.BlockedBy), read back from the root in store,
// less those whose target now satisfies them (EdgeSatisfied) and the root's
// edges onto its own steps. It only reads, so a launch that reuses an existing
// workflow reports what that workflow holds on, not what its source bead's
// edges say now.
func WorkflowBlockers(store beads.StoreReader, rootID string) ([]molecule.Blocker, error) {
	edges, err := ReadyBlockingEdges(store, rootID)
	if err != nil || len(edges) == 0 {
		return nil, err
	}
	held, err := beads.GetMany(store, molecule.BlockerIDs(edges))
	if err != nil {
		return nil, fmt.Errorf("reading blockers of workflow %s: %w", rootID, err)
	}
	blockers := make([]molecule.Blocker, 0, len(edges))
	for _, edge := range edges {
		target, ok := held[edge.ID]
		if ok && (EdgeSatisfied(edge, target) || target.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID) {
			continue
		}
		blockers = append(blockers, edge)
	}
	return sortedBlockers(blockers), nil
}

// sortedBlockers sorts blockers by id in place and returns them, or nil when
// there are none.
func sortedBlockers(blockers []molecule.Blocker) []molecule.Blocker {
	if len(blockers) == 0 {
		return nil
	}
	slices.SortFunc(blockers, func(a, b molecule.Blocker) int { return strings.Compare(a.ID, b.ID) })
	return blockers
}

// ownWorkflowRoots recognizes the workflow roots launched from sourceID: a
// root naming it in gc.source_bead_id, or a root whose input convoy is a
// synthetic convoy tracking it (the convoy-first --on launch). The tracking
// convoys are read once, on first need.
type ownWorkflowRoots struct {
	work      beads.WorkStore
	sourceID  string
	convoys   []string
	convoysOK bool
}

func (o *ownWorkflowRoots) contains(b beads.Bead) (bool, error) {
	if !sourceworkflow.IsWorkflowRoot(b) {
		return false, nil
	}
	if strings.TrimSpace(b.Metadata[beadmeta.SourceBeadIDMetadataKey]) == o.sourceID {
		return true, nil
	}
	inputConvoy := strings.TrimSpace(b.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if inputConvoy == "" {
		return false, nil
	}
	if !o.convoysOK {
		tracking, err := convoycore.TrackingConvoysForItem(o.work, o.sourceID)
		if err != nil {
			return false, fmt.Errorf("reading convoys tracking %s: %w", o.sourceID, err)
		}
		for _, c := range tracking {
			if c.Metadata[beadmeta.SyntheticMetadataKey] == "true" {
				o.convoys = append(o.convoys, c.ID)
			}
		}
		o.convoysOK = true
	}
	return slices.Contains(o.convoys, inputConvoy), nil
}

// CrossStoreBlockersError refuses a launch whose workflow cannot wait on its
// source bead's blockers: the workflow is created in a graph store relocated
// away from the store holding the source bead, and that graph store does not
// hold BlockerIDs, so no edge on a workflow bead could hold them.
//
// The refusal stands in for a hold. A convoy drain holds its item workflow on
// such a blocker through a cross-store gate the controller releases
// (dispatch's cross_store_gate.go), but a gate is owned by its drain control,
// and a launch outside a drain has no control to own one, so the launch is
// refused rather than started unheld. A launch the caller never asked for (a
// target's default_sling_formula) routes the source bead plainly instead,
// which its own edges hold.
type CrossStoreBlockersError struct {
	SourceID   string
	BlockerIDs []string
}

// Error names the source bead and its blockers and says how to clear them.
func (e *CrossStoreBlockersError) Error() string {
	return fmt.Sprintf("%s is blocked by %s, which the workflow's store does not hold, so the workflow cannot wait for %s; close %s or remove the edge with `bd dep remove %s <blocker>`, then re-sling",
		e.SourceID, strings.Join(e.BlockerIDs, ", "), pluralThem(len(e.BlockerIDs)), pluralThem(len(e.BlockerIDs)), e.SourceID)
}

func pluralThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// GraphHeldBlockers returns blockers (SourceBlockers of sourceID) as edges a
// workflow created in graph can hold, for a graph store relocated away from
// the store holding sourceID. An "external:<id>" ref names <id>, and an
// external capability names no bead and is dropped (beads.ExternalBlockerTarget,
// as the drain reads it); each edge keeps its type. A blocker graph does not
// hold fails the launch with a *CrossStoreBlockersError rather than leaving the
// workflow free to run.
func GraphHeldBlockers(graph beads.StoreReader, sourceID string, blockers []molecule.Blocker) ([]molecule.Blocker, error) {
	targets := make([]molecule.Blocker, 0, len(blockers))
	for _, blocker := range blockers {
		target, capability := beads.ExternalBlockerTarget(blocker.ID)
		if capability || target == "" || slices.ContainsFunc(targets, func(b molecule.Blocker) bool { return b.ID == target }) {
			continue
		}
		targets = append(targets, molecule.Blocker{ID: target, Type: blocker.Type})
	}
	if len(targets) == 0 {
		return nil, nil
	}
	held, err := beads.GetMany(graph, molecule.BlockerIDs(targets))
	if err != nil {
		return nil, fmt.Errorf("reading blockers of %s in the workflow's store: %w", sourceID, err)
	}
	var missing []string
	for _, target := range targets {
		if _, ok := held[target.ID]; !ok {
			missing = append(missing, target.ID)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, &CrossStoreBlockersError{SourceID: sourceID, BlockerIDs: missing}
	}
	return sortedBlockers(targets), nil
}
