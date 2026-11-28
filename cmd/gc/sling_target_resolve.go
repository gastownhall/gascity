package main

import (
	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/config"
)

// resolveSlingTarget resolves a gc sling target. Every target
// resolveAgentIdentity accepts resolves exactly as it does there. Only when
// that fails does it try namepool pool members (e.g. "frontend/furiosa"),
// the themed names gc status prints for pool members; a member resolves to
// the same instance copy numeric members use, whose PoolName is the base
// pool, so the bead routes to the pool.
//
// Namepool resolution is deliberately sling-only. resolveAgentIdentity must
// stay namepool-blind: gc hook relies on it failing for a namepool session's
// own runtime identity (GC_AGENT/GC_ALIAS) to fall back to GC_TEMPLATE and
// keep the session's identity in the work query.
func resolveSlingTarget(cfg *config.City, target, rigContext string) (config.Agent, bool) {
	if a, ok := resolveAgentIdentity(cfg, target, rigContext); ok {
		return a, true
	}
	return resolveNamepoolMember(cfg, func(pool *config.Agent, member string) bool {
		return target == pool.QualifiedInstanceName(member)
	})
}

// resolveNamepoolMember returns the instance copy of the single in-bound
// namepool member for which match reports true. It resolves nothing when no
// member or more than one member matches: namepool names are not unique
// across pools.
func resolveNamepoolMember(cfg *config.City, match func(pool *config.Agent, member string) bool) (config.Agent, bool) {
	var (
		found   config.Agent
		matches int
	)
	for i := range cfg.Agents {
		pool := &cfg.Agents[i]
		seen := make(map[string]bool, len(pool.NamepoolNames))
		for _, member := range pool.NamepoolNames {
			if seen[member] {
				continue
			}
			seen[member] = true
			if _, ok := agentutil.PoolMemberSlot(*pool, member); !ok || !match(pool, member) {
				continue
			}
			matches++
			found = deepCopyAgent(pool, member, pool.Dir)
		}
	}
	if matches != 1 {
		return config.Agent{}, false
	}
	return found, true
}
