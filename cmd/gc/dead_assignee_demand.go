package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// confirmedDeadAssigneeTemplates maps each assignee identity that resolves
// to exactly one closed session in sessionInfos (FR-1: confirmed-dead) to
// the pool template stranded work assigned to it should count against
// (FR-2). An identity matching zero closed sessions (never seen, or its
// only session is still open/idle/asleep) or more than one closed session
// (ambiguous) is omitted — callers must treat a missing key as "not
// confirmed dead" and never guess a template for it.
func confirmedDeadAssigneeTemplates(cfg *config.City, sessionInfos []sessionpkg.Info) map[string]string {
	matches := make(map[string][]sessionpkg.Info)
	for _, sb := range sessionInfos {
		if !sb.Closed {
			continue
		}
		for _, id := range sessionBeadAssigneeIdentitiesInfo(sb) {
			matches[id] = append(matches[id], sb)
		}
	}
	result := make(map[string]string, len(matches))
	for assignee, infos := range matches {
		if len(infos) != 1 {
			continue
		}
		if template := deadAssigneeFallbackTemplate(cfg, assignee, infos[0]); template != "" {
			result[assignee] = template
		}
	}
	return result
}

// deadAssigneeFallbackTemplate resolves the pool template a confirmed-dead
// assignee's stranded work falls back to, in strict precedence order: (1) a
// configured agent directly named by the assignee identity itself, (2) the
// dead session's own persisted template. This is FR-2 steps 2-3; callers
// apply a work bead's own gc.routed_to (FR-2 step 1) ahead of this.
func deadAssigneeFallbackTemplate(cfg *config.City, assignee string, deadInfo sessionpkg.Info) string {
	if agentCfg := findAgentByTemplate(cfg, assignee); agentCfg != nil {
		return agentCfg.QualifiedName()
	}
	template := strings.TrimSpace(normalizedSessionTemplateInfo(deadInfo, cfg))
	if template == "" {
		template = strings.TrimSpace(deadInfo.Template)
	}
	return template
}
