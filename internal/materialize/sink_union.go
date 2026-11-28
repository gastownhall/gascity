package materialize

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// SinkUnion folds the skills of every agent that writes one sink
// directory into that sink's desired set. Several agents write one sink
// when they share a scope root and provider family, or when a session's
// work dir is another agent's scope root or another session's work dir.
// The sink holds one link per name and its ownership manifest records
// every gc-written link, so a pass that reconciled one agent alone would
// prune the links its co-writers want. Every pass over a sink folds the
// same writers, so the passes agree on what the sink holds.
//
// Shared-catalog and agent-local entries are kept apart so Desired can
// apply the precedence EffectiveSet applies to one agent: an agent-local
// entry overrides the shared entry of that name for the whole sink.
type SinkUnion struct {
	agents     []string
	agentSeen  map[string]bool
	shared     map[string]sinkWant
	local      map[string]sinkWant
	clashes    []sinkClash
	unreadable []string
	owned      []string
	ownedSeen  map[string]bool
}

// sinkWant is one skill the sink must hold, with the first agent (in the
// order agents were added) that asked for it.
type sinkWant struct {
	entry SkillEntry
	agent string
}

// sinkClash records a name wanted from two sources in the same class
// (shared or agent-local).
type sinkClash struct {
	name  string
	local bool
	msg   string
}

// NewSinkUnion returns an empty union.
func NewSinkUnion() *SinkUnion {
	return &SinkUnion{
		agentSeen: make(map[string]bool),
		shared:    make(map[string]sinkWant),
		local:     make(map[string]sinkWant),
		ownedSeen: make(map[string]bool),
	}
}

// Add merges one agent's shared and agent-local catalogs into the union.
// A name wanted from one source by several agents is wanted once; a
// second source for the same name in the same class is recorded as a
// clash. Sources are compared by physical directory, so two paths to one
// directory are one source.
func (u *SinkUnion) Add(agent string, city CityCatalog, local AgentCatalog) {
	if !u.agentSeen[agent] {
		u.agentSeen[agent] = true
		u.agents = append(u.agents, agent)
	}
	for _, e := range city.Entries {
		u.want(u.shared, false, agent, e)
	}
	for _, e := range local.Entries {
		u.want(u.local, true, agent, e)
	}
	for _, root := range city.OwnedRoots {
		u.addOwned(root)
	}
	u.addOwned(local.OwnedRoot)
}

// MarkUnreadable records that one writer's catalog could not be read.
// Without it the union cannot tell that writer's links from stale ones,
// so the union is incomplete and Problems reports reason.
func (u *SinkUnion) MarkUnreadable(reason string) {
	u.unreadable = append(u.unreadable, reason)
}

func (u *SinkUnion) want(class map[string]sinkWant, local bool, agent string, e SkillEntry) {
	prev, ok := class[e.Name]
	if !ok {
		class[e.Name] = sinkWant{entry: e, agent: agent}
		return
	}
	if pathutil.NormalizePathForCompare(prev.entry.Source) != pathutil.NormalizePathForCompare(e.Source) {
		u.clashes = append(u.clashes, sinkClash{
			name:  e.Name,
			local: local,
			msg: fmt.Sprintf("skill %q is provided by agent %q from %s and by agent %q from %s",
				e.Name, prev.agent, prev.entry.Source, agent, e.Source),
		})
	}
}

func (u *SinkUnion) addOwned(root string) {
	if root == "" || u.ownedSeen[root] {
		return
	}
	u.ownedSeen[root] = true
	u.owned = append(u.owned, root)
}

// Problems returns why the sink must be held rather than reconciled:
// writer catalogs that could not be read, then clashes the union cannot
// resolve (two agent-local sources for one name, or two shared sources
// for a name no agent-local entry overrides). An agent-local override
// settles a shared clash, because the sink links the agent-local source
// either way. A held sink keeps its links and manifest; additions and
// removals wait until every problem is resolved.
func (u *SinkUnion) Problems() []string {
	out := append([]string(nil), u.unreadable...)
	for _, c := range u.clashes {
		if _, overridden := u.local[c.name]; c.local || !overridden {
			out = append(out, c.msg)
		}
	}
	return out
}

// Desired returns the sink's wanted entries, sorted by name. An
// agent-local entry overrides the shared entry of the same name for
// every agent writing the sink.
func (u *SinkUnion) Desired() []SkillEntry {
	var shared CityCatalog
	for _, w := range u.shared {
		shared.Entries = append(shared.Entries, w.entry)
	}
	var local AgentCatalog
	for _, w := range u.local {
		local.Entries = append(local.Entries, w.entry)
	}
	return EffectiveSet(shared, local)
}

// OwnedRoots returns the union of the writers' gc-managed roots, in the
// order they were added.
func (u *SinkUnion) OwnedRoots() []string {
	return append([]string(nil), u.owned...)
}

// Agents returns the writers in the order they were added.
func (u *SinkUnion) Agents() []string {
	return append([]string(nil), u.agents...)
}

// AgentList names the writers, quoted and comma-separated, for log lines.
func (u *SinkUnion) AgentList() string {
	quoted := make([]string, len(u.agents))
	for i, agent := range u.agents {
		quoted[i] = fmt.Sprintf("%q", agent)
	}
	return strings.Join(quoted, ", ")
}

// WantedBy returns the agent whose entry Desired chose for name, or ""
// when no writer wants it.
func (u *SinkUnion) WantedBy(name string) string {
	if w, ok := u.local[name]; ok {
		return w.agent
	}
	return u.shared[name].agent
}
