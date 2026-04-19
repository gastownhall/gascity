package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/validation"
)

// runStage1SkillMaterialization performs stage-1 skill materialization
// for every eligible agent in cfg. Stage 1 materializes at each
// agent's scope root (city or rig path). Session-worktree
// materialization (stage 2) is a separate PreStart-based path wired
// in by template_resolve.go via skill_integration.go.
//
// Stage-1 runs in the gc controller process on the host filesystem,
// so eligibility is "can the agent read from this host scope root?"
// — broader than the stage-2 "runtime executes host-side PreStart"
// gate. tmux and subprocess are both eligible (both read files from
// the host). k8s and acp are not (k8s pods don't share the scope
// root; acp runs in-process and doesn't read from it). Hybrid is
// per-session-routed; conservatively ineligible until v0.15.2.
//
// Catalog load happens once per scope per call and feeds every
// agent's materialization in this tick. Per-agent errors
// (LoadAgentCatalog, MaterializeAgent) are logged to stderr and do
// not abort the pass — the supervisor should continue reconciling
// every other agent. Shared-catalog load failures are also logged and
// then downgraded to an empty shared desired set, while preserving
// owned-root cleanup so stale gc-managed symlinks can still be pruned.
func runStage1SkillMaterialization(cityPath string, cfg *config.City, stderr io.Writer) error {
	if cfg == nil {
		return nil
	}
	catalogs := make(map[string]materialize.CityCatalog)
	loadCatalog := func(rigName string) materialize.CityCatalog {
		if cat, ok := catalogs[rigName]; ok {
			return cat
		}
		cat, err := loadSharedSkillCatalog(cfg, rigName)
		if err != nil {
			if stderr != nil {
				if rigName == "" {
					fmt.Fprintf(stderr, "gc: stage-1 materialize-skills: load shared skill catalog for city scope: %v\n", err) //nolint:errcheck // best-effort stderr
				} else {
					fmt.Fprintf(stderr, "gc: stage-1 materialize-skills: load shared skill catalog for rig %q: %v\n", rigName, err) //nolint:errcheck // best-effort stderr
				}
			}
			cat.Entries = nil
			cat.Shadowed = nil
			catalogs[rigName] = cat
			return catalogs[rigName]
		}
		catalogs[rigName] = cat
		return cat
	}

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if !canStage1Materialize(cfg.Session.Provider, agent) {
			continue
		}
		provider := effectiveAgentProvider(agent, cfg.Workspace.Provider)
		vendor, ok := materialize.VendorSink(provider)
		if !ok {
			continue
		}

		agentCat, lerr := materialize.LoadAgentCatalog(agent.SkillsDir)
		if lerr != nil {
			fmt.Fprintf(stderr, "gc: stage-1 materialize-skills for agent %q: LoadAgentCatalog %q: %v\n", //nolint:errcheck // best-effort stderr
				agent.QualifiedName(), agent.SkillsDir, lerr)
			// Continue with empty agent catalog rather than skipping the
			// whole materialization — the shared catalog still delivers.
			agentCat = materialize.AgentCatalog{}
		}

		rigName := agentRigScopeName(agent, cfg.Rigs)
		cityCat := loadCatalog(rigName)
		desired := materialize.EffectiveSet(cityCat, agentCat)

		// Resolve the agent's scope root to an absolute path. Use the
		// un-canonicalized form here so the materializer writes into
		// the operator-intended location (e.g., /city/rigs/fe even
		// when it's a symlink to /private/city/...). canonicalisation
		// happens at comparison time inside MaterializeAgent via
		// EvalSymlinks, so owner-root matching still works.
		scopeRoot := resolveAgentScopeRoot(agent, cityPath, cfg.Rigs)
		if !filepath.IsAbs(scopeRoot) {
			scopeRoot = filepath.Join(cityPath, scopeRoot)
		}
		sinkDir := filepath.Join(scopeRoot, vendor)

		owned := append([]string{}, cityCat.OwnedRoots...)
		if agentCat.OwnedRoot != "" {
			owned = append(owned, agentCat.OwnedRoot)
		}
		if len(desired) == 0 && len(owned) == 0 {
			continue
		}

		res, merr := materialize.Run(materialize.Request{
			SinkDir:     sinkDir,
			Desired:     desired,
			OwnedRoots:  owned,
			LegacyNames: materialize.LegacyStubNames(),
		})
		if merr != nil {
			fmt.Fprintf(stderr, "gc: stage-1 materialize-skills for agent %q at %s: %v\n", //nolint:errcheck // best-effort stderr
				agent.QualifiedName(), sinkDir, merr)
			continue
		}
		for _, s := range res.Skipped {
			fmt.Fprintf(stderr, "gc: agent %q skipped skill %q at %s — %s\n", //nolint:errcheck // best-effort stderr
				agent.QualifiedName(), s.Name, s.Path, s.Reason)
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(stderr, "gc: agent %q stage-1 materialize warning: %s\n", //nolint:errcheck // best-effort stderr
				agent.QualifiedName(), w)
		}
	}
	return nil
}

// checkSkillCollisions runs the skill-collision validator before
// materialization. Two agents sharing the same (scope-root, vendor)
// sink cannot both provide an agent-local skill under the same name
// — one of them would overwrite the other's symlink with a different
// target. Returns a formatted error suitable for direct display to
// the operator; nil when there are no collisions.
//
// `gc start` uses this as a hard gate (returning an error fails
// start). The supervisor tick runs it on every reconcile and fails
// the tick's materialize step on violation, leaving previously-
// materialized skills in place.
//
// cityPath is used to rewrite the "<city>" sentinel in the formatted
// error to the operator-visible city root.
func checkSkillCollisions(cfg *config.City, cityPath string) error {
	if cfg == nil {
		return nil
	}
	collisions := validation.ValidateSkillCollisions(cfg)
	if len(collisions) == 0 {
		return nil
	}
	return fmt.Errorf("%s", doctor.FormatSkillCollisions(collisions, cityPath))
}
