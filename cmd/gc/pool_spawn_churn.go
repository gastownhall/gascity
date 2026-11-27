package main

import "github.com/gastownhall/gascity/internal/config"

// clampBlindSpawnDemand limits each template allowance lists to its named
// work beads plus its allowance: the blind remainder of its count, the seats
// no work bead is named for, is withheld. A control dispatcher is never
// limited: its count is the control work it must serve. It edits counts and
// demand in place, which the merge built for this pass; a template allowance
// does not list keeps its count.
func clampBlindSpawnDemand(cfg *config.City, allowance map[string]int, counts map[string]int, demand map[string]scaleCheckDemand) {
	for template, allowed := range allowance {
		count, ok := counts[template]
		if !ok {
			continue
		}
		if agent := findAgentByTemplate(cfg, template); agent != nil && config.IsDeterministicControlDispatcher(agent) {
			continue
		}
		entry, named := demand[template]
		limit := len(entry.WorkBeadIDs) + max(allowed, 0)
		if count > limit {
			counts[template] = limit
		}
		if named && entry.Count > limit {
			entry.Count = limit
			demand[template] = entry
		}
	}
}
