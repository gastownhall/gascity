package main

import (
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// skillSinkWriter is one configured agent whose stage-2 pass writes a
// skill sink outside its own scope root: a copy of the agent's config
// and the identity its materialize-skills PreStart runs under (pool
// instances collapse to the pool template's name, templateNameFor).
type skillSinkWriter struct {
	Agent     config.Agent
	Qualified string
}

// skillSinkWriterIndex maps a sink's canonical path to the configured
// writers whose stage-2 passes write it, in config order. Stage 1 uses
// it as the reverse lookup a scope-root sink's union needs: a session
// whose work_dir resolves into another scope's root materializes into
// that scope's sink, so the sink's writers are not only the agents
// scoped there (issue #7549).
type skillSinkWriterIndex map[string][]skillSinkWriter

// stage2WriterIdentity is one session identity an agent materializes
// under: the agent config the work-dir template expands against (the
// instance for a pool slot, the agent itself otherwise) and that
// instance's qualified name.
type stage2WriterIdentity struct {
	Agent     *config.Agent
	Qualified string
}

// stage2WriterIdentities lists the configured identities through which
// an agent's sessions run: the agent itself, or one per configured pool
// slot (codexHookPoolSlots), each with the instance the slot's work-dir
// template resolves for. Dynamic aliases beyond configured slots each
// get their own worktree work dir with this agent as sole writer, so
// they need no index entry.
func stage2WriterIdentities(agent *config.Agent) []stage2WriterIdentity {
	slots := codexHookPoolSlots(agent)
	if len(slots) == 0 {
		return []stage2WriterIdentity{{Agent: agent, Qualified: agent.QualifiedName()}}
	}
	idents := make([]stage2WriterIdentity, 0, len(slots))
	for _, slot := range slots {
		instanceAgent, qualified, _ := poolDesiredRequestIdentity(agent, slot)
		idents = append(idents, stage2WriterIdentity{Agent: instanceAgent, Qualified: qualified})
	}
	return idents
}

// buildSkillSinkWriterIndex computes which skill sinks the city's
// stage-2-eligible agents write through their configured work dirs:
// every identity whose resolved work dir is outside the agent's scope
// root (the same condition under which template_resolve.go injects the
// per-session materialize-skills PreStart) indexes the sink under that
// work dir. A work dir that cannot be resolved statically — a broken
// template — fails the session start that would write it, so no
// materialization runs there from that agent either.
//
// The index is keyed by the sink's canonical path, matching how
// stage1SkillRequests groups sinks, so an aliased path a work dir and a
// scope root spell differently still meets its writers.
func buildSkillSinkWriterIndex(cityPath string, cfg *config.City, rt materializationRuntime) skillSinkWriterIndex {
	if cfg == nil {
		return nil
	}
	index := skillSinkWriterIndex{}
	cityName := loadedCityName(cfg, cityPath)
	seen := make(map[string]bool)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if !isStage2EligibleSession(rt, agent) {
			continue
		}
		provider := effectiveAgentProviderFamily(agent, cfg.Workspace.Provider, cfg.Providers)
		vendor, ok := materialize.VendorSink(provider)
		if !ok {
			continue
		}
		scopeRoot := agentScopeRoot(agent, cityPath, cfg.Rigs)
		for _, ident := range stage2WriterIdentities(agent) {
			workDir, err := resolveConfiguredWorkDirPathUnvalidated(cityPath, cityName, ident.Qualified, ident.Agent, cfg.Rigs)
			if err != nil || workDir == "" {
				continue
			}
			if canonicaliseFilePath(workDir, cityPath) == scopeRoot {
				continue
			}
			key := pathutil.NormalizePathForCompare(filepath.Join(workDir, vendor))
			name := templateNameFor(agent, ident.Qualified)
			dedupe := key + "\x00" + name
			if seen[dedupe] {
				continue
			}
			seen[dedupe] = true
			index[key] = append(index[key], skillSinkWriter{Agent: *ident.Agent, Qualified: name})
		}
	}
	return index
}

// stage2SinkWriters folds every configured writer of the skill sink at
// sinkDir into one SinkUnion, so a stage-2 pass reconciles the sink over
// all of them instead of pruning the links another writer put there
// (issue #7549). The writers are:
//
//   - the invoking agent, whose catalogs the caller already resolved
//     (the controller's snapshot when it passed one);
//   - the stage-1 writers whose scope root the sink sits at;
//   - the other stage-2 writers configured for the sink through their
//     work dirs (buildSkillSinkWriterIndex).
//
// Shared catalogs for co-writers resolve once per rig scope: the
// invoker's scope reuses the invoker's catalog (snapshot or live), so
// every writer of one sink folds the same fold inputs a stage-1 pass
// over that sink would; other scopes load live. A live load that fails
// marks the union unreadable — the sink is held rather than reconciled
// against a catalog that may be missing entries — and a writer whose
// catalogs load but hold no skills is skipped, since the PreStart its
// materialization rides on never fires without one.
func stage2SinkWriters(cfg *config.City, invoker *config.Agent, cityPath, sinkDir string, invokerCityCat materialize.CityCatalog, invokerAgentCat materialize.AgentCatalog) *materialize.SinkUnion {
	union := materialize.NewSinkUnion()
	folded := map[string]bool{}
	invokerName := templateNameFor(invoker, invoker.QualifiedName())
	folded[invokerName] = true
	union.AddAgent(invokerName, invokerCityCat, invokerAgentCat)

	invokerScope := agentRigScopeName(invoker, cfg.Rigs)
	scopeCats := map[string]materialize.CityCatalog{}
	scopeCat := func(rigName string) (materialize.CityCatalog, bool) {
		if rigName == invokerScope {
			return invokerCityCat, true
		}
		if cat, ok := scopeCats[rigName]; ok {
			return cat, true
		}
		cat, err := loadSharedSkillCatalog(cfg, rigName)
		if err != nil {
			return cat, false
		}
		scopeCats[rigName] = cat
		return cat, true
	}

	key := pathutil.NormalizePathForCompare(sinkDir)
	rt := cityMaterializationRuntime(cfg, cityPath)
	foldWriter := func(a *config.Agent, name string) {
		cat, catOK := scopeCat(agentRigScopeName(a, cfg.Rigs))
		agentCat, lerr := materialize.LoadAgentCatalog(a.SkillsDir)
		if lerr == nil && catOK && len(materialize.EffectiveSet(cat, agentCat)) == 0 {
			// No PreStart fires for a writer with no skills, so it
			// writes nothing into this sink.
			return
		}
		folded[name] = true
		union.AddAgent(name, cat, agentCat)
		if lerr != nil {
			union.MarkUnreadable(fmt.Sprintf("agent %q: LoadAgentCatalog %q: %v",
				name, a.SkillsDir, lerr))
		}
		if !catOK {
			union.MarkUnreadable(fmt.Sprintf("agent %q: load shared skill catalog for scope %q: unavailable",
				name, agentRigScopeName(a, cfg.Rigs)))
		}
	}

	// Scope-root writers: stage-1-eligible agents whose scope-root sink
	// canonicalizes to this sink. They are folded unconditionally — a
	// stage-1 pass folds them whatever their catalogs hold.
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if !canStage1Materialize(rt, a) {
			continue
		}
		provider := effectiveAgentProviderFamily(a, cfg.Workspace.Provider, cfg.Providers)
		vendor, ok := materialize.VendorSink(provider)
		if !ok {
			continue
		}
		scopeRoot := resolveAgentScopeRoot(a, cityPath, cfg.Rigs)
		if !filepath.IsAbs(scopeRoot) {
			scopeRoot = filepath.Join(cityPath, scopeRoot)
		}
		if pathutil.NormalizePathForCompare(filepath.Join(scopeRoot, vendor)) != key {
			continue
		}
		if folded[a.QualifiedName()] {
			continue
		}
		folded[a.QualifiedName()] = true
		cat, catOK := scopeCat(agentRigScopeName(a, cfg.Rigs))
		agentCat, lerr := materialize.LoadAgentCatalog(a.SkillsDir)
		union.AddAgent(a.QualifiedName(), cat, agentCat)
		if lerr != nil {
			union.MarkUnreadable(fmt.Sprintf("agent %q: LoadAgentCatalog %q: %v",
				a.QualifiedName(), a.SkillsDir, lerr))
		}
		if !catOK {
			union.MarkUnreadable(fmt.Sprintf("agent %q: load shared skill catalog for scope %q: unavailable",
				a.QualifiedName(), agentRigScopeName(a, cfg.Rigs)))
		}
	}

	// Stage-2 writers through configured work dirs, in config order.
	for _, w := range buildSkillSinkWriterIndex(cityPath, cfg, rt)[key] {
		if folded[w.Qualified] {
			continue
		}
		foldWriter(&w.Agent, w.Qualified)
	}
	return union
}
