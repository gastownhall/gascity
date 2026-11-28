package main

import (
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// skillSinkWriter is one configured agent that materializes skills into a
// sink directory.
type skillSinkWriter struct {
	// Name is the agent's template identity: pool instances share their
	// template's skills, so the template writes once.
	Name string
	// Agent is the template's config.
	Agent *config.Agent
}

// skillSink is one sink directory and every configured agent that writes
// it.
type skillSink struct {
	// Dir is the sink path as the first writer spells it; the materializer
	// writes the operator's spelling.
	Dir string
	// Writers lists the sink's writers in config order.
	Writers []skillSinkWriter
}

// skillSinkIndex groups configured skill writers by the physical sink
// directory they write. Keys are pathutil.NormalizePathForCompare of the
// sink, so a rig path that aliases another rig's path or the city root
// shares one key.
type skillSinkIndex struct {
	sinks map[string]*skillSink
}

func newSkillSinkIndex() *skillSinkIndex {
	return &skillSinkIndex{sinks: make(map[string]*skillSink)}
}

// add records that agent (identified by name) writes dir. A writer is
// listed once per sink.
func (x *skillSinkIndex) add(dir, name string, agent *config.Agent) {
	key := pathutil.NormalizePathForCompare(dir)
	sink := x.sinks[key]
	if sink == nil {
		sink = &skillSink{Dir: dir}
		x.sinks[key] = sink
	}
	for _, w := range sink.Writers {
		if w.Name == name {
			return
		}
	}
	sink.Writers = append(sink.Writers, skillSinkWriter{Name: name, Agent: agent})
}

// sorted returns the sinks ordered by key.
func (x *skillSinkIndex) sorted() []*skillSink {
	keys := make([]string, 0, len(x.sinks))
	for key := range x.sinks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*skillSink, 0, len(keys))
	for _, key := range keys {
		out = append(out, x.sinks[key])
	}
	return out
}

// stage1SkillSinkDir returns the scope-root sink stage 1 writes for agent,
// or false when stage 1 does not materialize the agent.
func stage1SkillSinkDir(rt materializationRuntime, cfg *config.City, cityPath string, agent *config.Agent) (string, bool) {
	if !canStage1Materialize(rt, agent) {
		return "", false
	}
	vendor, ok := materialize.VendorSink(effectiveAgentProviderFamily(agent, cfg.Workspace.Provider, cfg.Providers))
	if !ok {
		return "", false
	}
	// Use the un-canonicalized scope root so the materializer writes into
	// the operator-intended location (e.g., /city/rigs/fe even when it is a
	// symlink to /private/city/...); the index key and the materializer
	// canonicalize for comparison.
	scopeRoot := resolveAgentScopeRoot(agent, cityPath, cfg.Rigs)
	if !filepath.IsAbs(scopeRoot) {
		scopeRoot = filepath.Join(cityPath, scopeRoot)
	}
	return filepath.Join(scopeRoot, vendor), true
}

// buildSkillSinkIndex indexes every configured agent's skill sinks in cfg.
func buildSkillSinkIndex(cityPath string, cfg *config.City) *skillSinkIndex {
	idx := newSkillSinkIndex()
	if cfg == nil {
		return idx
	}
	rt := cityMaterializationRuntime(cfg, cityPath)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if dir, ok := stage1SkillSinkDir(rt, cfg, cityPath, agent); ok {
			idx.add(dir, agent.QualifiedName(), agent)
		}
	}
	return idx
}

// stage1SkillSinks returns every scope-root sink stage 1 reconciles, with
// all of its configured writers, sorted by sink.
func stage1SkillSinks(cityPath string, cfg *config.City) []*skillSink {
	return buildSkillSinkIndex(cityPath, cfg).sorted()
}
