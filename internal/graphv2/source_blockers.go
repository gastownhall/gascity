package graphv2

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// SourceBlockers returns the beads a workflow launched from sourceID must wait
// for: the targets of sourceID's ready-blocking dependencies, read from work,
// the store that holds sourceID. The ids are deduped and sorted.
//
// A graph workflow routes its own steps rather than the source bead, so the
// source bead's edges hold nothing once the workflow is launched. Copying
// these ids onto every workflow bead (molecule.Options.BlockedBy) holds the
// workflow by the same readiness rule that holds the source bead.
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
	slices.Sort(targets)
	return targets, nil
}
