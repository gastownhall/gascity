package runproj

import (
	"math"
	"sort"
	"strings"
)

// formulaPreviewNode mirrors a compiled-formula preview/step node. Port of TS
// FormulaPreviewNode (run-snapshot.ts) — only the id drives ordering.
type formulaPreviewNode struct {
	id string
}

// formulaDetailInput is the compiled-formula detail used to order run nodes. Port
// of the ordering-relevant slice of TS FormulaDetail. The bead-derived
// BuildRunDetail passes nil (no compiled formula), so orderRunNodeGroups is a
// no-op there; a live caller that fetches the supervisor's compiled formula can
// supply it to honor the authored step order.
type formulaDetailInput struct {
	name string
	// previewNodes mirrors preview.nodes: nil means absent (so formulaRankByAlias
	// falls back to steps, matching TS `??`); a non-nil empty slice means present
	// but empty (no fallback).
	previewNodes []formulaPreviewNode
	steps        []formulaPreviewNode
}

// orderRunNodeGroups orders groups by the compiled formula's authored step order,
// preserving snapshot order when no formula detail is available. Port of TS
// orderRunNodeGroups (formula-order.ts).
func orderRunNodeGroups(groups []runNodeGroup, formulaDetail *formulaDetailInput, rootBeadID string) []runNodeGroup {
	rankByAlias := formulaRankByAlias(formulaDetail)
	out := make([]runNodeGroup, len(groups))
	copy(out, groups)
	if len(rankByAlias) == 0 {
		return out
	}

	var formulaName string
	if formulaDetail != nil {
		formulaName = formulaDetail.name
	}

	type ranked struct {
		group runNodeGroup
		index int
		rank  float64
	}
	entries := make([]ranked, len(out))
	for i, group := range out {
		var rank float64
		if group.semanticNodeID == rootBeadID {
			rank = -1
		} else {
			rank = rankForGroup(group, rankByAlias, formulaName)
		}
		entries[i] = ranked{group: group, index: i, rank: rank}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].rank != entries[j].rank {
			return entries[i].rank < entries[j].rank
		}
		return entries[i].index < entries[j].index
	})
	for i, entry := range entries {
		out[i] = entry.group
	}
	return out
}

func formulaRankByAlias(formulaDetail *formulaDetailInput) map[string]int {
	ranks := make(map[string]int)
	if formulaDetail == nil {
		return ranks
	}
	// TS: `formulaDetail?.preview?.nodes ?? formulaDetail?.steps ?? []`. The
	// nullish `??` falls back to steps only when preview.nodes is ABSENT, so a
	// present-but-empty preview.nodes (non-nil, len 0) must NOT fall through.
	steps := formulaDetail.previewNodes
	if steps == nil {
		steps = formulaDetail.steps
	}
	for index, step := range steps {
		for _, alias := range aliasVariants(step.id, formulaDetail.name) {
			if _, ok := ranks[alias]; !ok {
				ranks[alias] = index
			}
		}
	}
	return ranks
}

func rankForGroup(group runNodeGroup, ranks map[string]int, formulaName string) float64 {
	rank := math.Inf(1)
	for _, alias := range groupAliases(group, formulaName) {
		if candidate, ok := ranks[alias]; ok && float64(candidate) < rank {
			rank = float64(candidate)
		}
	}
	return rank
}

func groupAliases(group runNodeGroup, formulaName string) []string {
	var base []string
	base = append(base, group.semanticNodeID)
	for _, bead := range group.beads {
		base = append(base, beadAliases(bead, formulaName)...)
	}
	var out []string
	for _, alias := range base {
		out = append(out, aliasVariants(alias, "")...)
	}
	return out
}

func beadAliases(bead runSnapshotBead, formulaName string) []string {
	var sources []string
	if v := nonEmpty(bead.id); v != "" {
		sources = append(sources, v)
	}
	if v := explicitLogicalBeadID(bead); v != "" {
		sources = append(sources, v)
	}
	if v := beadMeta(bead, "gc.step_id"); v != "" {
		sources = append(sources, v)
	}
	if v := normalizedStepRef(bead); v != "" {
		sources = append(sources, v)
	}
	var out []string
	for _, value := range sources {
		out = append(out, aliasVariants(value, formulaName)...)
	}
	return out
}

// aliasVariants expands a value into its externalized alias variants. Port of TS
// aliasVariants (formulaName "" means no prefix to strip).
func aliasVariants(value, formulaName string) []string {
	clean := nonEmpty(value)
	if clean == "" {
		return nil
	}
	stripped := stripFormulaPrefix(clean, formulaName)
	candidates := []string{clean, stripped, stripScopeCheckSuffix(clean), stripScopeCheckSuffix(stripped)}
	seen := make(map[string]bool)
	var out []string
	for _, candidate := range candidates {
		ext := externalizeID(candidate)
		if !seen[ext] {
			seen[ext] = true
			out = append(out, ext)
		}
	}
	return out
}

func stripFormulaPrefix(value, formulaName string) string {
	if formulaName == "" {
		return value
	}
	prefix := formulaName + "."
	return strings.TrimPrefix(value, prefix)
}

// ── lanes.ts ────────────────────────────────────────────────────────────────

const runLaneScope = "__run"

// buildRunDisplayLanes groups nodes into scope lanes. Port of TS
// buildRunDisplayLanes (lanes.ts), preserving first-seen scope order.
func buildRunDisplayLanes(nodes []RunDisplayNode) []RunDisplayLane {
	byScope := make(map[string]*RunDisplayLane)
	var order []string
	for _, node := range nodes {
		scope := runLaneScope
		if node.Scope.Kind == "scoped" {
			scope = node.Scope.Ref
		}
		lane, ok := byScope[scope]
		if !ok {
			label := scope
			if scope == runLaneScope {
				label = "Run"
			}
			lane = &RunDisplayLane{ID: scope, Label: label, NodeIDs: []string{}}
			byScope[scope] = lane
			order = append(order, scope)
		}
		lane.NodeIDs = append(lane.NodeIDs, node.ID)
	}
	lanes := make([]RunDisplayLane, 0, len(order))
	for _, scope := range order {
		lanes = append(lanes, *byScope[scope])
	}
	return lanes
}
