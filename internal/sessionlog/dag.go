package sessionlog

import "slices"

// dagNode is a node in the conversation DAG.
type dagNode struct {
	uuid      string
	parentID  string // parentUuid (empty string = root)
	lineIndex int    // 0-based position in JSONL file
	entry     *Entry
}

// DagResult is the result of resolving a session's DAG to its active
// conversation branch.
type DagResult struct {
	// ActiveBranch is the messages on the active branch, root to tip.
	ActiveBranch []*Entry

	// OrphanedToolUseIDs contains tool_use IDs on the active branch
	// that have no matching tool_result anywhere in the session.
	OrphanedToolUseIDs map[string]bool

	// HasBranches is true if the session has multiple tips (forks).
	HasBranches bool

	// CompactionCount is the number of compact_boundary entries on
	// the active branch.
	CompactionCount int
}

// BuildDag resolves a slice of entries into the active conversation branch.
//
// Algorithm:
//  1. Build maps: uuid → node, parentUuid → children
//  2. Find tips: entries with no children
//  3. Select active tip: most recent timestamp, tiebreaker longest branch
//  4. Walk tip to root via parentUuid chain (following logicalParentUuid
//     across compact boundaries)
//  5. Include parallel results attached to selected tool invocations
//  6. Find orphaned tool_use blocks on active branch
func BuildDag(entries []*Entry) *DagResult {
	nodeMap := make(map[string]*dagNode)
	childrenMap := make(map[string][]string) // parentUuid → child uuids

	// Build node and children maps.
	for i, e := range entries {
		if e.UUID == "" {
			continue // skip entries without UUID (file-history-snapshot, etc.)
		}
		node := &dagNode{
			uuid:      e.UUID,
			parentID:  e.ParentUUID,
			lineIndex: i,
			entry:     e,
		}
		nodeMap[e.UUID] = node
		childrenMap[e.ParentUUID] = append(childrenMap[e.ParentUUID], e.UUID)
	}

	// Find tips (nodes with no children).
	type tipInfo struct {
		node   *dagNode
		length int
	}
	var tips []tipInfo
	for _, node := range nodeMap {
		children := childrenMap[node.uuid]
		if len(children) == 0 {
			// A parallel result is an attachment to its invocation, not a new
			// conversation tip that can displace later assistant tool calls.
			parallelResult := false
			if parent := nodeMap[node.parentID]; parent != nil && attachedToolResult(node.entry, parent.entry) {
				for _, siblingID := range childrenMap[node.parentID] {
					sibling := nodeMap[siblingID]
					if sibling == nil || sibling.uuid == node.uuid || sibling.entry.Type != "assistant" {
						continue
					}
					for _, block := range sibling.entry.ContentBlocks() {
						if block.Type == "tool_use" {
							parallelResult = true
							break
						}
					}
					if parallelResult {
						break
					}
				}
			}
			if parallelResult {
				continue
			}
			length := walkBranchLength(node.uuid, nodeMap)
			tips = append(tips, tipInfo{node: node, length: length})
		}
	}

	if len(tips) == 0 {
		return &DagResult{}
	}

	// Select active tip: latest timestamp wins, then longest branch,
	// then latest lineIndex.
	best := tips[0]
	for _, t := range tips[1:] {
		bestTs := best.node.entry.Timestamp
		currentTs := t.node.entry.Timestamp
		switch {
		case currentTs.After(bestTs):
			best = t
		case bestTs.After(currentTs):
			// keep best
		default:
			// Same timestamp — prefer longer branch, then later line.
			if t.length > best.length ||
				(t.length == best.length && t.node.lineIndex > best.node.lineIndex) {
				best = t
			}
		}
	}

	// Walk from tip to root.
	var activeBranch []*Entry
	activeBranchUUIDs := make(map[string]bool)
	visited := make(map[string]bool)
	compactionCount := 0

	current := best.node
	for current != nil && !visited[current.uuid] {
		visited[current.uuid] = true
		activeBranch = append(activeBranch, current.entry)
		activeBranchUUIDs[current.uuid] = true

		if current.entry.IsCompactBoundary() {
			compactionCount++
		}

		// Determine next: parentUuid, or logicalParentUuid for compact boundaries.
		nextID := current.parentID
		if nextID == "" && current.entry.LogicalParentUUID != "" {
			nextID = current.entry.LogicalParentUUID
		}

		if nextID == "" {
			break
		}

		next, ok := nodeMap[nextID]
		if !ok && current.entry.LogicalParentUUID != "" {
			// logicalParentUuid references a message not in this file.
			// Fallback: find the node with highest lineIndex before current.
			next = findFallbackParent(current.lineIndex, nodeMap, visited)
		} else if !ok {
			break
		}
		current = next
	}

	// Reverse to get root → tip order.
	for i, j := 0, len(activeBranch)-1; i < j; i, j = i+1, j-1 {
		activeBranch[i], activeBranch[j] = activeBranch[j], activeBranch[i]
	}

	// Claude can attach each parallel result directly to its own tool call,
	// making the earlier result a sibling of the later call's branch. Retain
	// pure results tied to a selected invocation without importing other branch
	// text. Merge at their recorded position while preserving branch order.
	for _, entry := range entries {
		if activeBranchUUIDs[entry.UUID] || !activeBranchUUIDs[entry.ParentUUID] {
			continue
		}
		parent, node := nodeMap[entry.ParentUUID], nodeMap[entry.UUID]
		if parent == nil || node == nil || node.entry != entry || node.lineIndex <= parent.lineIndex || !attachedToolResult(entry, parent.entry) {
			continue
		}
		insertion := len(activeBranch)
		for i, current := range activeBranch {
			if nodeMap[current.UUID].lineIndex > node.lineIndex {
				insertion = i
				break
			}
		}
		activeBranch = slices.Insert(activeBranch, insertion, entry)
		activeBranchUUIDs[entry.UUID] = true
	}
	// Preserve cross-branch completion detection for runtimes such as Pi.
	allToolResultIDs := collectAllToolResultIDs(entries)

	// Find orphaned tool_use blocks in the selected conversation.
	orphaned := findOrphanedToolUses(activeBranch, allToolResultIDs)

	return &DagResult{
		ActiveBranch:       activeBranch,
		OrphanedToolUseIDs: orphaned,
		HasBranches:        len(tips) > 1,
		CompactionCount:    compactionCount,
	}
}

// attachedToolResult accepts only result records whose tool IDs belong to
// their direct parent invocation. Mixed user text and unrelated branches cannot
// be reintroduced into the selected conversation by a matching string alone.
func attachedToolResult(entry, parent *Entry) bool {
	if entry.Type != "user" && entry.Type != "result" && entry.Type != "tool_result" {
		return false
	}
	calls := make(map[string]bool)
	for _, block := range parent.ContentBlocks() {
		if block.Type == "tool_use" && block.ID != "" {
			calls[block.ID] = true
		}
	}
	if len(calls) == 0 {
		return false
	}
	blocks := entry.ContentBlocks()
	if len(blocks) == 0 {
		return entry.ToolUseID != "" && calls[entry.ToolUseID] && (entry.Type == "result" || entry.Type == "tool_result")
	}
	for _, block := range blocks {
		if block.Type != "tool_result" || !calls[block.ToolUseID] {
			return false
		}
	}
	return true
}

// conversationTypes are message types that count toward branch length.
var conversationTypes = map[string]bool{
	"user":      true,
	"assistant": true,
}

// walkBranchLength counts conversation messages (user/assistant) from
// tip to root. Used for branch selection tiebreaking.
func walkBranchLength(tipUUID string, nodeMap map[string]*dagNode) int {
	count := 0
	visited := make(map[string]bool)
	currentID := tipUUID

	for currentID != "" && !visited[currentID] {
		visited[currentID] = true
		node, ok := nodeMap[currentID]
		if !ok {
			break
		}
		if conversationTypes[node.entry.Type] {
			count++
		}
		nextID := node.parentID
		if nextID == "" && node.entry.LogicalParentUUID != "" {
			nextID = node.entry.LogicalParentUUID
		}
		if nextID != "" && nodeMap[nextID] == nil && node.entry.LogicalParentUUID != "" {
			fb := findFallbackParent(node.lineIndex, nodeMap, visited)
			if fb != nil {
				currentID = fb.uuid
			} else {
				break
			}
		} else {
			currentID = nextID
		}
	}
	return count
}

// findFallbackParent returns the node with the highest lineIndex before
// beforeIdx that hasn't been visited. Used when a compact_boundary's
// logicalParentUuid doesn't exist in this session file.
func findFallbackParent(beforeIdx int, nodeMap map[string]*dagNode, visited map[string]bool) *dagNode {
	var best *dagNode
	for _, n := range nodeMap {
		if n.lineIndex >= beforeIdx || visited[n.uuid] {
			continue
		}
		if best == nil || n.lineIndex > best.lineIndex {
			best = n
		}
	}
	return best
}

// collectAllToolResultIDs scans all entries for tool_result blocks and
// returns a set of their tool_use_id references. This scans the entire
// session (not just active branch) because parallel tool calls can
// produce results on sibling branches.
func collectAllToolResultIDs(entries []*Entry) map[string]bool {
	ids := make(map[string]bool)
	for _, e := range entries {
		// Top-level tool_result entries carry the tool_use_id directly.
		if e.ToolUseID != "" && (e.Type == "result" || e.Type == "tool_result") {
			ids[e.ToolUseID] = true
		}
		// Also check nested content blocks.
		blocks := e.ContentBlocks()
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				ids[b.ToolUseID] = true
			}
		}
	}
	return ids
}

// findOrphanedToolUses returns tool_use IDs on the active branch that
// have no matching tool_result anywhere in the session.
func findOrphanedToolUses(activeBranch []*Entry, allToolResultIDs map[string]bool) map[string]bool {
	toolUseIDs := make(map[string]bool)
	for _, e := range activeBranch {
		blocks := e.ContentBlocks()
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != "" {
				toolUseIDs[b.ID] = true
			}
		}
	}

	orphaned := make(map[string]bool)
	for id := range toolUseIDs {
		if !allToolResultIDs[id] {
			orphaned[id] = true
		}
	}
	if len(orphaned) == 0 {
		return nil
	}
	return orphaned
}
