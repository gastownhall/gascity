package materialize

import (
	"path/filepath"
	"sort"
)

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

// SinkSource is one member's source directory for a skill name.
type SinkSource struct {
	Agent  string
	Source string
}

// SinkConflict is a skill name that members of one sink want from
// different source directories of the same class. No precedence rule
// settles it, so the sink cannot be reconciled until the configuration
// changes.
type SinkConflict struct {
	// Name is the contested skill name.
	Name string
	// Local is true when the competing sources are agent-local skills,
	// false when they are shared skills.
	Local bool
	// Sources lists every member's source for Name, sorted by Source,
	// then Agent.
	Sources []SinkSource
}

// SinkUnion is the desired state of one sink written by several agents.
type SinkUnion struct {
	// Desired is the union of the members' effective sets, sorted by
	// Name. Conflicted names are left out.
	Desired []SkillEntry
	// OwnedRoots is every member's shared roots and agent-local root,
	// deduplicated, in first-seen order.
	OwnedRoots []string
	// WantedBy maps each desired skill name to the first member, in input
	// order, whose entry it is.
	WantedBy map[string]string
	// Conflicts lists the names no precedence rule settles, sorted by
	// Name. A sink with conflicts must not be reconciled against Desired:
	// leaving a name out of Desired prunes its existing link.
	Conflicts []SinkConflict
}

// UnionSinkMembers merges the catalogs of every agent that writes one sink.
//
// Sources are identified by their canonical absolute path, so the same pack
// skill seen by several members, several members' copies of one agent skills
// directory, and aliased spellings of one directory are one candidate. An
// agent-local entry beats a shared entry of the same name across the whole
// sink, whatever the members' order, which is each member's own EffectiveSet
// rule applied to the sink; a local entry also settles a clash between
// shared entries. A name with exactly one candidate is desired, with the
// entry of the first member in input order that has it, so a sink with a
// single member desires exactly that member's EffectiveSet. A name with two
// or more candidates of the winning class is a conflict and is not desired.
func UnionSinkMembers(members []SinkMember) SinkUnion {
	union := SinkUnion{WantedBy: make(map[string]string)}
	local := make(map[string]*sinkCandidates)
	shared := make(map[string]*sinkCandidates)
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
			addSinkCandidate(local, e, m.Agent)
		}
		for _, e := range m.Shared.Entries {
			addSinkCandidate(shared, e, m.Agent)
		}
	}

	names := make([]string, 0, len(local)+len(shared))
	for name := range local {
		names = append(names, name)
	}
	for name := range shared {
		if _, overridden := local[name]; !overridden {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	union.Desired = make([]SkillEntry, 0, len(names))
	for _, name := range names {
		cands, isLocal := local[name]
		if !isLocal {
			cands = shared[name]
		}
		if len(cands.picks) > 1 {
			union.Conflicts = append(union.Conflicts, SinkConflict{Name: name, Local: isLocal, Sources: cands.sortedSources()})
			continue
		}
		union.Desired = append(union.Desired, cands.picks[0].entry)
		union.WantedBy[name] = cands.picks[0].agent
	}
	return union
}

// sinkCandidates collects one class's sources for one skill name.
type sinkCandidates struct {
	// picks holds one entry per distinct source identity, each from the
	// first member that has it.
	picks []sinkPick
	// sources holds every member's source, one per (agent, source).
	sources []SinkSource
}

// sinkPick is one distinct source for a name and the first member with it.
type sinkPick struct {
	identity string
	entry    SkillEntry
	agent    string
}

// addSinkCandidate records agent's entry e in class.
func addSinkCandidate(class map[string]*sinkCandidates, e SkillEntry, agent string) {
	cands := class[e.Name]
	if cands == nil {
		cands = &sinkCandidates{}
		class[e.Name] = cands
	}
	src := SinkSource{Agent: agent, Source: e.Source}
	known := false
	for _, s := range cands.sources {
		if s == src {
			known = true
			break
		}
	}
	if !known {
		cands.sources = append(cands.sources, src)
	}
	id := sourceIdentity(e.Source)
	for _, p := range cands.picks {
		if p.identity == id {
			return
		}
	}
	cands.picks = append(cands.picks, sinkPick{identity: id, entry: e, agent: agent})
}

// sortedSources returns the candidates' sources sorted by Source, then
// Agent.
func (c *sinkCandidates) sortedSources() []SinkSource {
	out := append([]SinkSource(nil), c.sources...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// sourceIdentity is the canonical absolute path that identifies a skill
// source, so aliased spellings of one directory compare equal. It falls
// back to the cleaned absolute path.
func sourceIdentity(source string) string {
	abs, err := filepath.Abs(source)
	if err != nil {
		return filepath.Clean(source)
	}
	canon, err := canonicalizePath(abs)
	if err != nil {
		return abs
	}
	return canon
}
