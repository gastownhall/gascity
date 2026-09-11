package runproj

// terminalStatuses are the node statuses that satisfy an upstream blocker.
// Port of TS TERMINAL_STATUSES (display-state.ts).
var terminalStatuses = map[string]bool{
	"completed": true,
	"done":      true,
	"failed":    true,
	"skipped":   true,
	"canceled":  true,
}

// applyDisplayNodeStates promotes pending nodes to ready based on their
// upstream edges (dep-waiting nodes stay pending). Port of TS
// applyDisplayNodeStates minus its pending→blocked promotion. The returned
// slice is a fresh copy, mirroring the TS immutable update (the field order of
// each node is preserved).
func applyDisplayNodeStates(nodes []RunDisplayNode, edges []RunDisplayEdge) []RunDisplayNode {
	byID := make(map[string]RunDisplayNode, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	inbound := buildInboundEdges(edges, byID)

	statusByID := make(map[string]string, len(nodes))
	for _, node := range nodes {
		statusByID[node.ID] = displayStatusFor(node, inbound[node.ID], byID)
	}

	out := make([]RunDisplayNode, len(nodes))
	for i, node := range nodes {
		status := node.Status
		if s, ok := statusByID[node.ID]; ok {
			status = s
		}
		if status == node.Status {
			out[i] = node
			continue
		}
		updated := node
		updated.Status = status
		instances := make([]RunExecutionInstance, len(node.ExecutionInstances))
		for j, inst := range node.ExecutionInstances {
			if inst.CurrentIteration && inst.Status == "pending" {
				inst.Status = status
			}
			instances[j] = inst
		}
		updated.ExecutionInstances = instances
		out[i] = updated
	}
	return out
}

func displayStatusFor(node RunDisplayNode, blockers []string, byID map[string]RunDisplayNode) string {
	if node.Status != "pending" {
		return node.Status
	}
	if len(blockers) == 0 {
		return "ready"
	}
	for _, blockerID := range blockers {
		blocker, ok := byID[blockerID]
		if !ok || !terminalStatuses[blocker.Status] {
			// Waiting on an unfinished upstream is the normal life of a
			// pending node, not an alarm: it stays "pending". "blocked" is
			// reserved for nodes the store itself marks blocked (operator
			// attention), which return through the early exit above.
			return "pending"
		}
	}
	return "ready"
}

func buildInboundEdges(edges []RunDisplayEdge, byID map[string]RunDisplayNode) map[string][]string {
	inbound := make(map[string][]string)
	for _, edge := range edges {
		if _, ok := byID[edge.From]; !ok {
			continue
		}
		if _, ok := byID[edge.To]; !ok {
			continue
		}
		inbound[edge.To] = append(inbound[edge.To], edge.From)
	}
	return inbound
}
