package formula

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// SelfClosingControlEdge is a compiled readiness edge whose blocker is the
// control node that closes the blocked node.
type SelfClosingControlEdge struct {
	// NodeID is the blocked node — the one the control closes.
	NodeID string
	// ControlID is the control node blocking it.
	ControlID string
	// DepType is the compiled dependency type that carries the block.
	DepType string
}

func (e SelfClosingControlEdge) String() string {
	return fmt.Sprintf("%s is blocked by %s (%s), which closes it", e.NodeID, e.ControlID, e.DepType)
}

// isReadinessBlockingDepType reports whether a compiled dependency type gates
// readiness and close. MEASURED against bd 1.1.0: a "blocks" dependency on an
// open issue keeps the dependent out of `bd ready` AND makes `bd update
// --status closed` fail with "cannot close blocked issue"; a "tracks"
// dependency does neither. "waits-for" and "conditional-blocks" are the other
// scheduling types the SDK emits (see sling.isCycleSensitiveDep). The empty
// type is bd's default, which is "blocks". "parent-child" is an ownership
// edge, always child -> parent, and never names a control as the parent.
func isReadinessBlockingDepType(depType string) bool {
	switch depType {
	case "", "blocks", "waits-for", "conditional-blocks":
		return true
	default:
		return false
	}
}

// FindSelfClosingControlEdges returns every readiness-blocking edge in a
// compiled graph whose blocker closes the node it blocks. Such an edge is a
// permanent deadlock: the control cannot close its target while the target is
// blocked, and the control is the only bead that would ever clear the blocker.
//
// The result is sorted so callers can render a stable message.
func FindSelfClosingControlEdges(steps []RecipeStep, deps []RecipeDep) []SelfClosingControlEdge {
	if len(steps) == 0 || len(deps) == 0 {
		return nil
	}
	metaByID := make(map[string]map[string]string, len(steps))
	for _, step := range steps {
		metaByID[step.ID] = step.Metadata
	}

	var found []SelfClosingControlEdge
	for _, dep := range deps {
		if !isReadinessBlockingDepType(dep.Type) {
			continue
		}
		controlMeta, ok := metaByID[dep.DependsOnID]
		if !ok {
			continue
		}
		if !beadmeta.ControlClosesNode(controlMeta, dep.StepID, metaByID[dep.StepID]) {
			continue
		}
		found = append(found, SelfClosingControlEdge{
			NodeID:    dep.StepID,
			ControlID: dep.DependsOnID,
			DepType:   dep.Type,
		})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].NodeID != found[j].NodeID {
			return found[i].NodeID < found[j].NodeID
		}
		return found[i].ControlID < found[j].ControlID
	})
	return found
}

// ValidateNoSelfClosingControlEdges fails a compile that would mint a node
// blocked by the control bead that closes it. Every producer of control
// dependencies routes through this check at the compile chokepoint, so the
// shape is unrepresentable in a compiled recipe rather than merely absent from
// the current formula corpus (ga-a6zy9).
func ValidateNoSelfClosingControlEdges(recipeName string, steps []RecipeStep, deps []RecipeDep) error {
	found := FindSelfClosingControlEdges(steps, deps)
	if len(found) == 0 {
		return nil
	}
	rendered := make([]string, 0, len(found))
	for _, edge := range found {
		rendered = append(rendered, edge.String())
	}
	return fmt.Errorf("compiling formula %q: control close cycle: %s", recipeName, strings.Join(rendered, "; "))
}

// RewriteRecipeDepsToControls repoints, in place, every readiness edge that
// waited on a rewritten step so it waits on that step's freshly minted control
// instead — except an edge whose dependent node is the node that control
// closes. That one edge is the ga-a6zy9 deadlock: a scope body blocked by its
// own scope-check can never close, because the scope-check IS its closer.
//
// nodes supplies the metadata of the pre-existing graph nodes and controls the
// metadata of the newly minted control nodes; replacements maps a rewritten
// step ID to its control ID.
func RewriteRecipeDepsToControls(deps []RecipeDep, nodes, controls []RecipeStep, replacements map[string]string) {
	if len(deps) == 0 || len(replacements) == 0 {
		return
	}
	nodeMeta := make(map[string]map[string]string, len(nodes))
	for _, node := range nodes {
		nodeMeta[node.ID] = node.Metadata
	}
	controlMeta := make(map[string]map[string]string, len(controls))
	for _, control := range controls {
		controlMeta[control.ID] = control.Metadata
	}
	for i := range deps {
		replacement, ok := replacements[deps[i].DependsOnID]
		if !ok {
			continue
		}
		if meta, ok := controlMeta[replacement]; ok &&
			beadmeta.ControlClosesNode(meta, deps[i].StepID, nodeMeta[deps[i].StepID]) {
			continue
		}
		deps[i].DependsOnID = replacement
	}
}
