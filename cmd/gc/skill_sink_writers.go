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
	// ScopeRoot reports that the sink is some agent's scope-root sink, so
	// stage 1 reconciles it.
	ScopeRoot bool
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

// add records that agent (identified by name) writes dir; scopeRoot marks
// a stage-1 write into the agent's scope root. A writer is listed once
// per sink.
func (x *skillSinkIndex) add(dir, name string, agent *config.Agent, scopeRoot bool) {
	key := pathutil.NormalizePathForCompare(dir)
	sink := x.sinks[key]
	if sink == nil {
		sink = &skillSink{Dir: dir}
		x.sinks[key] = sink
	}
	sink.ScopeRoot = sink.ScopeRoot || scopeRoot
	for _, w := range sink.Writers {
		if w.Name == name {
			return
		}
	}
	sink.Writers = append(sink.Writers, skillSinkWriter{Name: name, Agent: agent})
}

// lookup returns the indexed sink at dir, or nil when no configured agent
// writes it.
func (x *skillSinkIndex) lookup(dir string) *skillSink {
	return x.sinks[pathutil.NormalizePathForCompare(dir)]
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

// stage2SkillSinkDirs returns the per-session sinks stage 2 writes for
// agent: the vendor sink under each work dir its sessions resolve to (the
// template identity and every pool slot) that differs from the agent's
// scope root, the condition under which a session's PreStart
// materializes (template_resolve.go). Work dirs are resolved purely, with
// no filesystem check; an identity whose work dir does not resolve is
// skipped, since its session cannot start there.
func stage2SkillSinkDirs(rt materializationRuntime, cfg *config.City, cityPath string, agent *config.Agent) []string {
	if !isStage2EligibleSession(rt, agent) {
		return nil
	}
	vendor, ok := materialize.VendorSink(effectiveAgentProviderFamily(agent, cfg.Workspace.Provider, cfg.Providers))
	if !ok {
		return nil
	}
	cityName := loadedCityName(cfg, cityPath)
	scopeRoot := agentScopeRoot(agent, cityPath, cfg.Rigs)
	type sessionIdentity struct {
		agent *config.Agent
		name  string
	}
	identities := []sessionIdentity{{agent, agent.QualifiedName()}}
	for _, slot := range codexHookPoolSlots(agent) {
		instance, qualified, _ := poolDesiredRequestIdentity(agent, slot)
		identities = append(identities, sessionIdentity{instance, qualified})
	}
	var dirs []string
	seen := make(map[string]bool)
	for _, id := range identities {
		workDir, err := resolveConfiguredWorkDirPathUnvalidated(cityPath, cityName, id.name, id.agent, cfg.Rigs)
		if err != nil || workDir == "" {
			continue
		}
		if canonicaliseFilePath(workDir, cityPath) == scopeRoot {
			continue
		}
		dir := filepath.Join(workDir, vendor)
		if key := pathutil.NormalizePathForCompare(dir); !seen[key] {
			seen[key] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// buildSkillSinkIndex indexes every configured agent's skill sinks in cfg:
// the scope-root sink stage 1 writes and the work-dir sinks its sessions'
// stage 2 writes. Writers are template identities, so pool instances
// share their template's entry.
func buildSkillSinkIndex(cityPath string, cfg *config.City) *skillSinkIndex {
	idx := newSkillSinkIndex()
	if cfg == nil {
		return idx
	}
	rt := cityMaterializationRuntime(cfg, cityPath)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if dir, ok := stage1SkillSinkDir(rt, cfg, cityPath, agent); ok {
			idx.add(dir, agent.QualifiedName(), agent, true)
		}
		for _, dir := range stage2SkillSinkDirs(rt, cfg, cityPath, agent) {
			idx.add(dir, agent.QualifiedName(), agent, false)
		}
	}
	return idx
}

// stage1SkillSinks returns every scope-root sink stage 1 reconciles, with
// all of its configured writers (including agents whose sessions' work
// dir is that scope root), sorted by sink.
func stage1SkillSinks(cityPath string, cfg *config.City) []*skillSink {
	var out []*skillSink
	for _, sink := range buildSkillSinkIndex(cityPath, cfg).sorted() {
		if sink.ScopeRoot {
			out = append(out, sink)
		}
	}
	return out
}
