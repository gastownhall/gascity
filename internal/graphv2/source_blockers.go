package graphv2

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// SourceBlockers returns the beads a workflow launched from sourceID must wait
// for: the targets of sourceID's ready-blocking dependencies, read from work,
// the store that holds sourceID. The ids are deduped and sorted.
//
// A graph workflow routes its own steps rather than the source bead, so the
// source bead's edges hold nothing once the workflow is launched. Copying
// these ids onto every workflow bead (molecule.Options.BlockedBy) holds the
// workflow by the same readiness rule that holds the source bead.
//
// A target the store's readiness rule already judges satisfied
// (beads.DependencySatisfied) is dropped, and so is one of sourceID's own
// workflow roots: a replacement workflow never waits on the one it replaces.
// Every other target is kept as written, including an external ref and a
// target work does not hold, so the store judges it as it judges sourceID's
// own edge.
func SourceBlockers(work beads.WorkStore, sourceID string) ([]string, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, nil
	}
	deps, err := work.DepList(sourceID, "down")
	if err != nil {
		return nil, fmt.Errorf("listing dependencies of %s: %w", sourceID, err)
	}
	var targets []string
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		target := strings.TrimSpace(dep.DependsOnID)
		if target == "" || target == sourceID || slices.Contains(targets, target) {
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	held, err := beads.GetMany(work, targets)
	if err != nil {
		return nil, fmt.Errorf("reading blockers of %s: %w", sourceID, err)
	}
	own := ownWorkflowRoots{work: work, sourceID: sourceID}
	blockers := make([]string, 0, len(targets))
	for _, target := range targets {
		b, ok := held[target]
		if ok && beads.DependencySatisfied(b.Status, beads.ReadinessWorkOutcome(b.Metadata)) {
			continue
		}
		if ok {
			isOwn, err := own.contains(b)
			if err != nil {
				return nil, err
			}
			if isOwn {
				continue
			}
		}
		blockers = append(blockers, target)
	}
	slices.Sort(blockers)
	if len(blockers) == 0 {
		return nil, nil
	}
	return blockers, nil
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
// as the drain reads it). A blocker graph does not hold fails the launch with
// a *CrossStoreBlockersError rather than leaving the workflow free to run.
func GraphHeldBlockers(graph beads.StoreReader, sourceID string, blockers []string) ([]string, error) {
	targets := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		target, capability := beads.ExternalBlockerTarget(blocker)
		if capability || target == "" || slices.Contains(targets, target) {
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	held, err := beads.GetMany(graph, targets)
	if err != nil {
		return nil, fmt.Errorf("reading blockers of %s in the workflow's store: %w", sourceID, err)
	}
	var missing []string
	for _, target := range targets {
		if _, ok := held[target]; !ok {
			missing = append(missing, target)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, &CrossStoreBlockersError{SourceID: sourceID, BlockerIDs: missing}
	}
	slices.Sort(targets)
	return targets, nil
}
