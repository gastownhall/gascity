package sessionlog

import (
	"encoding/json"
	"slices"
)

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
	// ActiveBranch is the messages on the active branch, root to tip. It
	// also holds the rows BuildDag retains from sibling branches: parallel
	// results attached to a selected tool call and rows of a selected
	// response. Retained rows appear at their recorded position, which may
	// follow the selected tip.
	ActiveBranch []*Entry

	// OrphanedToolUseIDs contains tool_use IDs on the active branch
	// that have no matching tool_result anywhere in the session.
	OrphanedToolUseIDs map[string]bool

	// HasBranches is true if the session has multiple tips (forks),
	// parallel result attachments excluded: a leaf that is a parallel
	// result attached to its tool call is not a tip.
	HasBranches bool

	// CompactionCount is the number of compact_boundary entries on
	// the active branch.
	CompactionCount int
}

// BuildDag resolves a slice of entries into the active conversation branch.
//
// Algorithm:
//  1. Build maps: uuid → node, parentUuid → children
//  2. Find tips: entries with no children, skipping parallel results
//  3. Select active tip: most recent timestamp, tiebreaker longest branch
//  4. Walk tip to root via parentUuid chain (following logicalParentUuid
//     across compact boundaries)
//  5. Collect all tool_result IDs from entire session
//  6. Retain sibling-branch rows of the selected conversation
//  7. Find orphaned tool_use blocks on active branch
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

	// Find tips (nodes with no children), skipping parallel results.
	type tipInfo struct {
		node   *dagNode
		length int
	}
	var tips []tipInfo
	for _, node := range nodeMap {
		children := childrenMap[node.uuid]
		if len(children) == 0 && !parallelToolResult(node, nodeMap, childrenMap) {
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

	// Collect all tool_result IDs from entire session (not just active branch).
	allToolResultIDs := collectAllToolResultIDs(entries)

	activeBranch = retainSiblingRows(entries, activeBranch, activeBranchUUIDs, nodeMap, allToolResultIDs)

	// Find orphaned tool_use blocks on active branch.
	orphaned := findOrphanedToolUses(activeBranch, allToolResultIDs)

	return &DagResult{
		ActiveBranch:       activeBranch,
		OrphanedToolUseIDs: orphaned,
		HasBranches:        len(tips) > 1,
		CompactionCount:    compactionCount,
	}
}

// parallelToolResult reports whether a leaf is a pure result attached to its
// invocation while a sibling row carries another tool call of the same
// response. Such a result is an attachment to its invocation, not a new
// conversation tip that can displace the later parallel call. A sibling call
// from a different response forked the conversation instead, so it only
// counts when both responses match or either one is unknown.
func parallelToolResult(node *dagNode, nodeMap map[string]*dagNode, childrenMap map[string][]string) bool {
	parent := nodeMap[node.parentID]
	if parent == nil || !attachedToolResult(node.entry, parent.entry) {
		return false
	}
	response := assistantResponseID(parent.entry)
	for _, siblingID := range childrenMap[node.parentID] {
		sibling := nodeMap[siblingID]
		if sibling == nil || sibling.uuid == node.uuid || sibling.entry.Type != "assistant" {
			continue
		}
		if other := assistantResponseID(sibling.entry); response != "" && other != "" && other != response {
			continue
		}
		if slices.ContainsFunc(sibling.entry.ContentBlocks(), func(block ContentBlock) bool { return block.Type == "tool_use" }) {
			return true
		}
	}
	return false
}

// retainSiblingRows merges into branch the rows of the selected conversation
// that sit on a sibling branch, at their recorded position while preserving
// branch order. Claude can attach each parallel result directly to its own
// tool call, making the earlier result a sibling of the later call's branch.
// Claude Code also splits one API response into rows chained by parentUuid
// that share message.id, so a later parallel call of that response can sit on
// the sibling branch too (observed on 2.1.296). Retain, to a fixed point, pure
// results tied to a selected invocation and rows continuing a selected row's
// response, without importing other branch text or abandoned calls.
func retainSiblingRows(entries, branch []*Entry, onBranch map[string]bool, nodeMap map[string]*dagNode, resultIDs map[string]bool) []*Entry {
	turns := branchTurns(branch, nodeMap)
	for changed := true; changed; {
		changed = false
		for _, entry := range entries {
			if onBranch[entry.UUID] || !onBranch[entry.ParentUUID] {
				continue
			}
			parent, node := nodeMap[entry.ParentUUID], nodeMap[entry.UUID]
			if parent == nil || node == nil || node.entry != entry || node.lineIndex <= parent.lineIndex {
				continue
			}
			if !attachedToolResult(entry, parent.entry) && !currentResponseRow(node, parent.entry, turns, resultIDs) {
				continue
			}
			insertion := slices.IndexFunc(branch, func(current *Entry) bool {
				return nodeMap[current.UUID].lineIndex > node.lineIndex
			})
			if insertion < 0 {
				insertion = len(branch)
			}
			branch = slices.Insert(branch, insertion, entry)
			onBranch[entry.UUID] = true
			changed = true
		}
	}
	return branch
}

// branchTurn is a row of the walked branch that answers or continues an
// assistant response: a user prompt (response "") or an assistant row.
type branchTurn struct {
	lineIndex int
	response  string
}

// branchTurns lists the user prompts and assistant rows of branch. A user row
// carrying a tool_result block is a result, even with queued text beside it,
// so it is not a prompt.
func branchTurns(branch []*Entry, nodeMap map[string]*dagNode) []branchTurn {
	var turns []branchTurn
	for _, entry := range branch {
		switch entry.Type {
		case "assistant":
			turns = append(turns, branchTurn{lineIndex: nodeMap[entry.UUID].lineIndex, response: assistantResponseID(entry)})
		case "user":
			if !slices.ContainsFunc(entry.ContentBlocks(), func(block ContentBlock) bool { return block.Type == "tool_result" }) {
				turns = append(turns, branchTurn{lineIndex: nodeMap[entry.UUID].lineIndex})
			}
		}
	}
	return turns
}

// currentResponseRow reports whether an off-branch assistant row continues
// its selected parent's response and is not an abandoned call.
func currentResponseRow(node *dagNode, parent *Entry, turns []branchTurn, resultIDs map[string]bool) bool {
	response := assistantResponseID(node.entry)
	if response == "" || assistantResponseID(parent) != response {
		return false
	}
	return !abandonedCall(node, response, turns, resultIDs)
}

// abandonedCall reports whether none of a response row's tool calls has a
// result anywhere in the session and the walked branch moved past the
// response after the row: a later user prompt or a later assistant row of
// another response. Until then the calls may still be running and stay open.
func abandonedCall(node *dagNode, response string, turns []branchTurn, resultIDs map[string]bool) bool {
	calls := toolCallIDs(node.entry)
	if len(calls) == 0 || slices.ContainsFunc(calls, func(id string) bool { return resultIDs[id] }) {
		return false
	}
	return slices.ContainsFunc(turns, func(turn branchTurn) bool {
		return turn.lineIndex > node.lineIndex && turn.response != response
	})
}

// toolCallIDs returns the IDs of the tool_use blocks in entry.
func toolCallIDs(entry *Entry) []string {
	var ids []string
	for _, block := range entry.ContentBlocks() {
		if block.Type == "tool_use" && block.ID != "" {
			ids = append(ids, block.ID)
		}
	}
	return ids
}

// assistantResponseID returns the API response identity (message.id) of an
// assistant row, or "" for other rows.
func assistantResponseID(entry *Entry) string {
	if entry.Type != "assistant" || len(entry.Message) == 0 {
		return ""
	}
	var message struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(entry.Message, &message) != nil {
		return ""
	}
	return message.ID
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
