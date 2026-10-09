package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

type assigneeRoster struct {
	exact      map[string]struct{}
	poolStems  map[string]struct{}
	idPrefixes map[string]struct{}
	qualifiers map[string]struct{}
	problems   []string
}

var poolInstanceSuffix = regexp.MustCompile(`-\d+[a-z]?$`)

var generatedSessionShape = regexp.MustCompile(`^.*-(?:adhoc-[0-9a-f]{6,}|auto-\d+)$`)

var beadIDSuffix = regexp.MustCompile(`^(?:[0-9]+|[0-9a-z]{4,})$`)

var reservedAssignees = map[string]struct{}{
	"human": {},
}

func newAssigneeRosterAt(cfg *config.City, cityPath string) *assigneeRoster {
	r := &assigneeRoster{
		exact:      map[string]struct{}{},
		poolStems:  map[string]struct{}{},
		idPrefixes: map[string]struct{}{},
		qualifiers: map[string]struct{}{},
	}
	if cfg == nil {
		return r
	}
	if p := strings.TrimSpace(config.EffectiveHQPrefix(cfg)); p != "" {
		r.idPrefixes[p] = struct{}{}
	}
	for i := range cfg.Rigs {
		if p := strings.TrimSpace(cfg.Rigs[i].EffectivePrefix()); p != "" {
			r.idPrefixes[p] = struct{}{}
		}
		if name := strings.TrimSpace(cfg.Rigs[i].Name); name != "" {
			r.qualifiers[name] = struct{}{}
		}
	}
	if len(r.idPrefixes) > 0 {
		for _, p := range config.AllReservedClassPrefixes() { // residency:allow — extends the roster's bead-id prefix table, not a probe
			r.idPrefixes[p] = struct{}{}
		}
	}
	cityName := cfg.EffectiveCityName()
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		r.addExact(cfg.NamedSessions[i].IdentityName())
		r.addExact(identity)
		r.addExact(config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity))
	}
	for i := range cfg.Agents {
		name := strings.TrimSpace(cfg.Agents[i].Name)
		qualified := cfg.Agents[i].QualifiedName()
		if dir := strings.TrimSpace(cfg.Agents[i].Dir); dir != "" {
			r.qualifiers[dir] = struct{}{}
		}
		runtimeName := strings.TrimSpace(agent.SessionNameFor(cityName, qualified, cfg.Workspace.SessionTemplate))
		r.addExact(name)
		r.addExact(qualified)
		r.addExact(runtimeName)
		stems := []string{name, qualified, runtimeName, poolSessionStem(qualified)}
		tmuxAlias, err := resolveTmuxAliasForAgentIn(cityPath, cityName, cfg.Rigs, &cfg.Agents[i])
		if err != nil {
			r.problems = append(r.problems, fmt.Sprintf("agent %q: %v", qualified, err))
		} else if tmuxAlias != "" {
			r.addExact(tmuxAlias)
			stems = append(stems, tmuxAlias)
		}
		for _, stem := range stems {
			if stem != "" {
				r.poolStems[stem] = struct{}{}
			}
		}
		for _, pooled := range cfg.Agents[i].NamepoolNames {
			r.addExact(pooled)
			r.addExact(cfg.Agents[i].QualifiedInstanceName(pooled))
		}
	}
	return r
}

func (r *assigneeRoster) problemsList() []string {
	return append([]string(nil), r.problems...)
}

func (r *assigneeRoster) addSessionIdentities(infos []session.Info) {
	for _, info := range infos {
		for _, identity := range session.AssigneeIdentities(info) {
			r.addExact(identity)
		}
	}
}

func (r *assigneeRoster) Empty() bool {
	return len(r.exact) == 0 && len(r.poolStems) == 0
}

func (r *assigneeRoster) addExact(name string) {
	name = strings.TrimSpace(name)
	if name != "" {
		r.exact[name] = struct{}{}
	}
}

func (r *assigneeRoster) Resolves(assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return true
	}
	if _, ok := reservedAssignees[assignee]; ok {
		return true
	}
	for _, candidate := range r.pathCandidates(assignee) {
		if generatedSessionShape.MatchString(candidate) {
			return true
		}
		if r.looksLikeBeadID(candidate) {
			return true
		}
	}
	for _, candidate := range r.runtimeCandidates(assignee) {
		if r.resolvesRuntimeName(candidate) || r.resolvesRuntimeName(strings.TrimSuffix(candidate, poolRuntimeNameSuffix)) {
			return true
		}
	}
	return false
}

func (r *assigneeRoster) resolvesRuntimeName(name string) bool {
	if _, ok := r.exact[name]; ok {
		return true
	}
	if _, ok := r.poolStems[poolInstanceSuffix.ReplaceAllString(name, "")]; ok {
		return true
	}
	for idx := strings.Index(name, "-"); idx > 0; {
		if _, ok := r.poolStems[name[:idx]]; ok && r.looksLikeBeadID(name[idx+1:]) {
			return true
		}
		next := strings.Index(name[idx+1:], "-")
		if next < 0 {
			break
		}
		idx += next + 1
	}
	return false
}

func (r *assigneeRoster) looksLikeBeadID(assignee string) bool {
	if len(r.idPrefixes) == 0 {
		idx := strings.Index(assignee, "-")
		return idx > 0 && idx+1 < len(assignee) && beadIDSuffix.MatchString(assignee[idx+1:])
	}
	for prefix := range r.idPrefixes {
		if len(assignee) <= len(prefix) || assignee[len(prefix)] != '-' || !strings.EqualFold(assignee[:len(prefix)], prefix) {
			continue
		}
		if beadIDSuffix.MatchString(assignee[len(prefix)+1:]) {
			return true
		}
	}
	return false
}

func (r *assigneeRoster) runtimeCandidates(assignee string) []string {
	candidates := []string{assignee}
	if unsanitized := agent.UnsanitizeQualifiedNameFromSession(assignee); unsanitized != assignee {
		candidates = append(candidates, unsanitized)
	}
	return candidates
}

func (r *assigneeRoster) pathCandidates(assignee string) []string {
	candidates := []string{assignee}
	if idx := strings.LastIndex(assignee, "/"); idx > 0 && idx+1 < len(assignee) {
		if _, ok := r.qualifiers[assignee[:idx]]; ok {
			candidates = append(candidates, assignee[idx+1:])
		}
	}
	return candidates
}
