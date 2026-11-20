package materialize

import "sort"

// SinkMember is one agent's catalogs as they apply to one vendor sink.
// Several agents write one sink when they share a scope root and a
// provider family; a stage-1 pass reconciles that sink once, against the
// union of every member's catalogs.
type SinkMember struct {
	// Agent is the member's qualified agent name, used only to attribute
	// a skill or a conflict to an agent in reports.
	Agent string
	// Shared is the shared catalog of the member's scope.
	Shared CityCatalog
	// Local is the member's agent-local catalog.
	Local AgentCatalog
}

// SinkUnion is the desired state of one sink written by several agents.
type SinkUnion struct {
	// Desired is the union of the members' effective sets, sorted by Name.
	Desired []SkillEntry
	// OwnedRoots is every member's shared roots and agent-local root,
	// deduplicated, in first-seen order.
	OwnedRoots []string
	// WantedBy maps each desired skill name to the first member, in input
	// order, whose entry it is.
	WantedBy map[string]string
}

// UnionSinkMembers merges the catalogs of every agent that writes one sink.
// An agent-local entry beats a shared entry of the same name across the
// whole sink, whatever the members' order, which is each member's own
// EffectiveSet rule applied to the sink. Within a class, the first member
// in input order that has the name supplies the entry, so a sink with a
// single member desires exactly that member's EffectiveSet.
func UnionSinkMembers(members []SinkMember) SinkUnion {
	type pick struct {
		entry SkillEntry
		agent string
	}
	union := SinkUnion{WantedBy: make(map[string]string)}
	local := make(map[string]pick)
	shared := make(map[string]pick)
	seenRoot := make(map[string]bool)
	addRoot := func(root string) {
		if root == "" || seenRoot[root] {
			return
		}
		seenRoot[root] = true
		union.OwnedRoots = append(union.OwnedRoots, root)
	}
	for _, m := range members {
		for _, root := range m.Shared.OwnedRoots {
			addRoot(root)
		}
		addRoot(m.Local.OwnedRoot)
		for _, e := range m.Local.Entries {
			if _, ok := local[e.Name]; !ok {
				local[e.Name] = pick{entry: e, agent: m.Agent}
			}
		}
		for _, e := range m.Shared.Entries {
			if _, ok := shared[e.Name]; !ok {
				shared[e.Name] = pick{entry: e, agent: m.Agent}
			}
		}
	}
	union.Desired = make([]SkillEntry, 0, len(local)+len(shared))
	for name, p := range local {
		union.Desired = append(union.Desired, p.entry)
		union.WantedBy[name] = p.agent
	}
	for name, p := range shared {
		if _, overridden := local[name]; overridden {
			continue
		}
		union.Desired = append(union.Desired, p.entry)
		union.WantedBy[name] = p.agent
	}
	sort.Slice(union.Desired, func(i, j int) bool { return union.Desired[i].Name < union.Desired[j].Name })
	return union
}
