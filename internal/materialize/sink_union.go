package materialize

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// SinkUnion folds the catalogs of every configured writer of one skill
// sink into the desired set that sink must hold. A sink is shared
// whenever several agents materialize into one directory — two agents at
// one scope root, or a session whose work dir is another agent's scope
// root — and a pass that reconciles the sink for only one of them would
// prune the links the others put there: the sink's ownership manifest
// marks every gc-written link as prunable by any pass over the sink.
//
// The union is a pure fold over (agent, shared catalog, agent catalog)
// triples. Both the supervisor's stage-1 pass and the per-session
// stage-2 pass build one, so every pass over a sink computes the same
// desired set and passes converge regardless of which writer runs.
//
// Fold rules:
//
//   - Identical physical sources coalesce: a name one directory provides
//     through two catalog paths is wanted once.
//   - An agent-local entry overrides the shared entry of that name for
//     the whole sink, because the sink holds one link per name.
//   - Two distinct sources in the same class (shared or agent-local)
//     clash.
//   - An agent-local override settles a shared clash: the sink links the
//     agent-local source either way.
//   - A writer whose catalog could not be read makes the union
//     incomplete (MarkUnreadable).
//
// A clashing or incomplete union holds the sink: the caller reports
// Problems and runs no pass, so existing links and the manifest stay,
// and additions and removals wait for a pass that can compute the whole
// union.
type SinkUnion struct {
	agents     []string
	shared     map[string]sinkWant
	local      map[string]sinkWant
	clashes    []sinkClash
	unreadable []string
	owned      []string
	ownedSeen  map[string]bool
}

// sinkWant is one skill a sink must hold, with the first writer (in the
// fold's order) that asked for it.
type sinkWant struct {
	entry SkillEntry
	agent string
}

// sinkClash records a name wanted from two distinct sources in the same
// class.
type sinkClash struct {
	name  string
	local bool
	msg   string
}

// NewSinkUnion returns an empty fold for one sink.
func NewSinkUnion() *SinkUnion {
	return &SinkUnion{
		shared:    make(map[string]sinkWant),
		local:     make(map[string]sinkWant),
		ownedSeen: make(map[string]bool),
	}
}

// AddAgent merges one writer's shared and agent-local catalogs into the
// fold and contributes their gc-managed source roots to the union's
// OwnedRoots. agent is the writer's display name (its qualified config
// name); the first writer to want a name is the one later attributed to
// it by WantedBy.
func (s *SinkUnion) AddAgent(agent string, city CityCatalog, local AgentCatalog) {
	s.agents = append(s.agents, agent)
	for _, e := range city.Entries {
		s.want(s.shared, false, agent, e)
	}
	for _, e := range local.Entries {
		s.want(s.local, true, agent, e)
	}
	for _, root := range city.OwnedRoots {
		s.addOwned(root)
	}
	s.addOwned(local.OwnedRoot)
}

// MarkUnreadable records that one writer's catalog could not be read:
// without it the fold cannot tell that writer's links from stale ones, so
// the union is incomplete and the sink must be held. reason is a
// one-line description naming the agent and the failure.
func (s *SinkUnion) MarkUnreadable(reason string) {
	s.unreadable = append(s.unreadable, reason)
}

// want records that agent wants entry e in the given class. A name
// wanted from one source by several writers is wanted once; a second
// source for the same name is recorded as a clash. Sources are compared
// by physical directory, so two paths to one directory are one source.
func (s *SinkUnion) want(class map[string]sinkWant, local bool, agent string, e SkillEntry) {
	prev, ok := class[e.Name]
	if !ok {
		class[e.Name] = sinkWant{entry: e, agent: agent}
		return
	}
	if pathutil.NormalizePathForCompare(prev.entry.Source) != pathutil.NormalizePathForCompare(e.Source) {
		s.clashes = append(s.clashes, sinkClash{
			name:  e.Name,
			local: local,
			msg: fmt.Sprintf("skill %q is provided by agent %q from %s and by agent %q from %s",
				e.Name, prev.agent, prev.entry.Source, agent, e.Source),
		})
	}
}

// clashesHeld returns the clashes the sink cannot resolve: two
// agent-local sources for one name, or two shared sources for a name no
// agent-local entry overrides. An agent-local override settles a shared
// clash, because the sink links the agent-local source either way. The
// sink holds one link per name, so choosing between two sources of the
// same class would silently drop one writer's skill.
func (s *SinkUnion) clashesHeld() []string {
	var out []string
	for _, c := range s.clashes {
		if _, overridden := s.local[c.name]; c.local || !overridden {
			out = append(out, c.msg)
		}
	}
	return out
}

// Problems returns why the sink cannot be reconciled this pass: writers
// whose catalogs could not be read, then unresolved clashes. Empty when
// the union is complete and conflict-free.
func (s *SinkUnion) Problems() []string {
	return append(append([]string(nil), s.unreadable...), s.clashesHeld()...)
}

// Agents returns the writers folded so far, in fold order, for log lines
// and skip-history bookkeeping.
func (s *SinkUnion) Agents() []string {
	return append([]string(nil), s.agents...)
}

// addOwned adds root to the union's prunable roots once.
func (s *SinkUnion) addOwned(root string) {
	if root == "" || s.ownedSeen[root] {
		return
	}
	s.ownedSeen[root] = true
	s.owned = append(s.owned, root)
}

// Desired returns the sink's wanted entries, sorted by name, with the
// precedence EffectiveSet applies to one writer: an agent-local entry
// overrides a shared entry of the same name. The sink holds one link per
// name, so an override from one writer also replaces the shared version
// for every other writer of the sink.
func (s *SinkUnion) Desired() []SkillEntry {
	var shared CityCatalog
	for _, w := range s.shared {
		shared.Entries = append(shared.Entries, w.entry)
	}
	var local AgentCatalog
	for _, w := range s.local {
		local.Entries = append(local.Entries, w.entry)
	}
	return EffectiveSet(shared, local)
}

// WantedBy returns the writer a skip of name is reported under: the
// writer whose entry Desired chose for that name, or "" when no writer
// wants it.
func (s *SinkUnion) WantedBy(name string) string {
	if w, ok := s.local[name]; ok {
		return w.agent
	}
	if w, ok := s.shared[name]; ok {
		return w.agent
	}
	return ""
}

// OwnedRoots returns the union of the gc-managed source roots every
// folded writer may prune under, deduplicated, in fold order.
func (s *SinkUnion) OwnedRoots() []string {
	return append([]string(nil), s.owned...)
}
