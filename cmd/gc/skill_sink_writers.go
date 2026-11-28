package main

import (
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
