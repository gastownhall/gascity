package runproj

import "github.com/gastownhall/gascity/internal/beadmeta"

// buildRunDisplayEdges projects a run snapshot's dependency edges into the
// display graph, preferring logical edges and bridging across hidden scope-check
// nodes. Port of TS buildRunDisplayEdges (edges.ts).
func buildRunDisplayEdges(raw runSnapshot, physicalToSemantic map[string]string, nodes []RunDisplayNode) []RunDisplayEdge {
	logicalEdges := projectEdges(raw.logicalEdges, physicalToSemantic, nodes, nil)
	if len(logicalEdges) > 0 {
		return logicalEdges
	}
	return projectEdges(raw.deps, physicalToSemantic, nodes, bridgeableScopeCheckIDs(raw))
}

func projectEdges(deps []runSnapshotDep, physicalToSemantic map[string]string, nodes []RunDisplayNode, bridgeableHiddenIDs map[string]bool) []RunDisplayEdge {
	visible := make(map[string]bool)
	for _, node := range nodes {
		if node.VisibleInGraph {
			visible[node.ID] = true
		}
	}
	outgoing := outgoingDeps(deps)
	seen := make(map[string]bool)
	edges := []RunDisplayEdge{}

	for _, dep := range deps {
		rawFrom := nonEmpty(dep.from)
		rawTo := nonEmpty(dep.to)
		if rawFrom == "" || rawTo == "" {
			continue
		}
		if nonEmpty(dep.kind) == "tracks" {
			continue
		}
		from := semanticOf(physicalToSemantic, rawFrom)
		to := semanticOf(physicalToSemantic, rawTo)
		kind := nonEmpty(dep.kind)
		hasKind := kind != ""
		if visible[from] && visible[to] {
			edges = pushEdge(edges, seen, from, to, kind, hasKind)
			continue
		}
		if visible[from] && bridgeableHiddenIDs[rawTo] {
			edges = bridgeHiddenEdges(edges, seen, from, rawTo, outgoing, visible, bridgeableHiddenIDs, physicalToSemantic, kind, hasKind, make(map[string]bool))
		}
	}
	return edges
}

func bridgeHiddenEdges(edges []RunDisplayEdge, seen map[string]bool, source, currentRawID string, outgoing map[string][]runSnapshotDep, visible, bridgeableHiddenIDs map[string]bool, physicalToSemantic map[string]string, inheritedKind string, hasInheritedKind bool, visited map[string]bool) []RunDisplayEdge {
	if visited[currentRawID] {
		return edges
	}
	visited[currentRawID] = true
	for _, dep := range outgoing[currentRawID] {
		rawTo := nonEmpty(dep.to)
		if rawTo == "" {
			continue
		}
		kind := nonEmpty(dep.kind)
		if kind == "tracks" {
			continue
		}
		target := semanticOf(physicalToSemantic, rawTo)
		edgeKind, hasEdgeKind := kind, kind != ""
		if !hasEdgeKind {
			edgeKind, hasEdgeKind = inheritedKind, hasInheritedKind
		}
		if visible[target] {
			edges = pushEdge(edges, seen, source, target, edgeKind, hasEdgeKind)
		} else if bridgeableHiddenIDs[rawTo] {
			edges = bridgeHiddenEdges(edges, seen, source, rawTo, outgoing, visible, bridgeableHiddenIDs, physicalToSemantic, edgeKind, hasEdgeKind, visited)
		}
	}
	return edges
}

func pushEdge(edges []RunDisplayEdge, seen map[string]bool, from, to, kind string, hasKind bool) []RunDisplayEdge {
	if from == to {
		return edges
	}
	edgeKind := "dependency"
	if hasKind {
		edgeKind = kind
	}
	key := from + "->" + to + ":" + edgeKind
	if seen[key] {
		return edges
	}
	seen[key] = true
	return append(edges, RunDisplayEdge{From: from, To: to, Kind: edgeKind})
}

func outgoingDeps(deps []runSnapshotDep) map[string][]runSnapshotDep {
	out := make(map[string][]runSnapshotDep)
	for _, dep := range deps {
		from := nonEmpty(dep.from)
		to := nonEmpty(dep.to)
		if from == "" || to == "" {
			continue
		}
		out[from] = append(out[from], dep)
	}
	return out
}

// bridgeableScopeCheckIDs collects the bead ids of scope-check constructs, which
// edges may bridge across. Port of TS bridgeableScopeCheckIds.
func bridgeableScopeCheckIDs(raw runSnapshot) map[string]bool {
	ids := make(map[string]bool)
	for _, bead := range raw.beads {
		id := nonEmpty(bead.id)
		if id == "" {
			continue
		}
		kind := nonEmpty(bead.metadata[beadmeta.KindMetadataKey])
		if kind == "" {
			kind = nonEmpty(bead.kind)
		}
		if kind == "scope-check" {
			ids[id] = true
		}
	}
	return ids
}

// semanticOf maps a physical id to its semantic id, falling back to the
// externalized raw id (TS `physicalToSemantic.get(raw) ?? externalizeId(raw)`).
func semanticOf(physicalToSemantic map[string]string, rawID string) string {
	if semantic, ok := physicalToSemantic[rawID]; ok {
		return semantic
	}
	return externalizeID(rawID)
}
