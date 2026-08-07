package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// poolMaxBoundChange records a ResolvedMaxActiveSessions change for one agent.
type poolMaxBoundChange struct {
	Name string
	Old  *int
	New  *int
}

// formatResolvedMaxActiveSessions formats a resolved max_active_sessions value
// for operator-facing reload output. Nil and negative mean unlimited.
func formatResolvedMaxActiveSessions(m *int) string {
	if m == nil || *m < 0 {
		return "unlimited"
	}
	return strconv.Itoa(*m)
}

// resolvedMaxEqual reports whether two resolved max values are operator-equivalent.
func resolvedMaxEqual(a, b *int) bool {
	return formatResolvedMaxActiveSessions(a) == formatResolvedMaxActiveSessions(b)
}

// poolMaxActiveSessionChanges diffs ResolvedMaxActiveSessions for agents present
// in both old and new configs. Uses the same resolution helper the reconciler
// uses (Agent.ResolvedMaxActiveSessions) so inheritance from rig/workspace is
// visible when only a parent bound changes.
func poolMaxActiveSessionChanges(oldCfg, newCfg *config.City) []poolMaxBoundChange {
	if oldCfg == nil || newCfg == nil {
		return nil
	}
	oldByName := make(map[string]*config.Agent, len(oldCfg.Agents))
	for i := range oldCfg.Agents {
		a := &oldCfg.Agents[i]
		oldByName[a.QualifiedName()] = a
	}
	var changes []poolMaxBoundChange
	for i := range newCfg.Agents {
		a := &newCfg.Agents[i]
		name := a.QualifiedName()
		oldA, ok := oldByName[name]
		if !ok {
			continue
		}
		oldM := oldA.ResolvedMaxActiveSessions(oldCfg)
		newM := a.ResolvedMaxActiveSessions(newCfg)
		if resolvedMaxEqual(oldM, newM) {
			continue
		}
		changes = append(changes, poolMaxBoundChange{Name: name, Old: oldM, New: newM})
	}
	sort.Slice(changes, func(i, j int) bool {
		return changes[i].Name < changes[j].Name
	})
	return changes
}

// formatPoolMaxBoundChangeLine formats one pool bound change for the reload Message.
func formatPoolMaxBoundChangeLine(c poolMaxBoundChange) string {
	return fmt.Sprintf("pool %s: max_active_sessions %s → %s",
		c.Name,
		formatResolvedMaxActiveSessions(c.Old),
		formatResolvedMaxActiveSessions(c.New),
	)
}

// poolMaxBoundDrainWarning returns a warning when the new bound is below the
// current active count. Decreases apply by lazy attrition (FR-PM.4): excess
// sessions are not killed; the reconciler stops replacing them when they finish
// (new demand is capped by the fresh ResolvedMaxActiveSessions).
func poolMaxBoundDrainWarning(c poolMaxBoundChange, active int) string {
	if c.New == nil || *c.New < 0 {
		return ""
	}
	if active <= *c.New {
		return ""
	}
	return fmt.Sprintf(
		"pool %s: max_active_sessions %s → %s cannot take full effect until sessions drain (%d active); excess sessions wind down via normal reconcile and are not replaced when they finish",
		c.Name,
		formatResolvedMaxActiveSessions(c.Old),
		formatResolvedMaxActiveSessions(c.New),
		active,
	)
}

// appendPoolMaxBoundFeedback adds per-pool max_active_sessions change lines to
// message and drain warnings to the warnings list. Pure: activeCounts is
// keyed by agent QualifiedName.
func appendPoolMaxBoundFeedback(message string, warnings []string, changes []poolMaxBoundChange, activeCounts map[string]int) (string, []string) {
	if len(changes) == 0 {
		return message, warnings
	}
	lines := make([]string, 0, len(changes)+1)
	if strings.TrimSpace(message) != "" {
		lines = append(lines, strings.TrimSpace(message))
	}
	for _, c := range changes {
		lines = append(lines, formatPoolMaxBoundChangeLine(c))
		active := 0
		if activeCounts != nil {
			active = activeCounts[c.Name]
		}
		if w := poolMaxBoundDrainWarning(c, active); w != "" {
			warnings = append(warnings, w)
		}
	}
	return strings.Join(lines, "\n"), warnings
}

// countActiveSessionsByTemplate returns open session counts keyed by agent
// QualifiedName. Prefers session beads when available; falls back to running
// provider sessions attributed via identity resolution against cfg.
func (cr *CityRuntime) countActiveSessionsByTemplate(cfg *config.City) map[string]int {
	counts := make(map[string]int)
	if cr == nil || cfg == nil {
		return counts
	}
	if store := cr.sessionsBeadStore(); store.Store != nil {
		infos, err := sessionFrontDoor(store.Store).ListAll(sessionpkg.ListAllOptions{})
		if err == nil {
			for _, info := range infos {
				if info.Closed {
					continue
				}
				template := strings.TrimSpace(normalizedSessionTemplateInfo(info, cfg))
				if template == "" {
					continue
				}
				counts[template]++
			}
			return counts
		}
	}
	if cr.sp == nil {
		return counts
	}
	running, err := cr.sp.ListRunning("")
	if err != nil {
		return counts
	}
	for _, name := range running {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		template := resolvedTemplateForIdentity(name, cfg)
		if template == "" {
			// Provider process names are sometimes city-prefixed
			// (e.g. "gc-city-planner"). Try the final dash-separated
			// segment as an agent identity before giving up.
			if idx := strings.LastIndex(name, "-"); idx >= 0 {
				if resolved := resolvedTemplateForIdentity(name[idx+1:], cfg); resolved != "" {
					template = resolved
				}
			}
		}
		if template == "" {
			// Match bounded pool instance "template-N" / "dir/template-N".
			if base := poolBaseIdentity(name); base != "" {
				template = resolvedTemplateForIdentity(base, cfg)
			}
		}
		if template != "" {
			counts[template]++
		}
	}
	return counts
}

// poolBaseIdentity strips a trailing "-N" pool slot suffix when present.
func poolBaseIdentity(sessionName string) string {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return ""
	}
	i := strings.LastIndex(sessionName, "-")
	if i <= 0 || i == len(sessionName)-1 {
		return ""
	}
	slot := sessionName[i+1:]
	for _, r := range slot {
		if r < '0' || r > '9' {
			return ""
		}
	}
	if slot == "0" || (len(slot) > 1 && slot[0] == '0') {
		return ""
	}
	return sessionName[:i]
}
