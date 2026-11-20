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
// For each name the first member in input order that wants it supplies the
// entry, so a sink with a single member desires exactly that member's
// EffectiveSet.
func UnionSinkMembers(members []SinkMember) SinkUnion {
	union := SinkUnion{WantedBy: make(map[string]string)}
	byName := make(map[string]SkillEntry)
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
		for _, e := range EffectiveSet(m.Shared, m.Local) {
			if _, ok := byName[e.Name]; ok {
				continue
			}
			byName[e.Name] = e
			union.WantedBy[e.Name] = m.Agent
		}
	}
	union.Desired = make([]SkillEntry, 0, len(byName))
	for _, e := range byName {
		union.Desired = append(union.Desired, e)
	}
	sort.Slice(union.Desired, func(i, j int) bool { return union.Desired[i].Name < union.Desired[j].Name })
	return union
}
