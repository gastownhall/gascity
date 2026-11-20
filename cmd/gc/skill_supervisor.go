package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/pathutil"
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
// Agents with the same scope root and provider family share one vendor
// sink, and each sink is reconciled once per pass against the union of
// its member agents' catalogs (materialize.UnionSinkMembers), so no
// member's pass prunes another member's links.
//
// Catalog load happens once per scope per call and feeds every
// sink's materialization in this pass. Per-sink errors are logged to
// stderr and do not abort the pass — every other sink is still
// reconciled. Shared-catalog load failures are also logged and
// then downgraded to an empty shared desired set, while preserving
// owned-root cleanup so stale gc-managed symlinks can still be pruned.
// The returned error joins every per-sink error, or is nil.
func runStage1SkillMaterialization(cityPath string, cfg *config.City, stderr io.Writer) error {
	if cfg == nil {
		return nil
	}
	skipPass := stage1SkillSkipLog.beginPass(cityPath)
	defer skipPass.finish()

	var errs []error
	for _, req := range stage1SkillRequests(cityPath, cfg, stderr) {
		if len(req.Held) > 0 {
			// The sink keeps exactly its last reconciled state: a pass
			// against a partial desired set would prune or re-point links
			// another member still wants.
			reportHeldSkillSink(stderr, req)
			for _, agent := range req.Agents {
				skipPass.markIncomplete(agent)
			}
			errs = append(errs, fmt.Errorf("skill sink %s (%s) not reconciled: %s",
				req.Request.SinkDir, formatSinkAgents(req.Agents), strings.Join(req.Held, "; ")))
			continue
		}
		res, merr := materialize.Run(req.Request)
		if merr != nil {
			for _, agent := range req.Agents {
				skipPass.markIncomplete(agent)
			}
			fmt.Fprintf(stderr, "gc: stage-1 materialize-skills for %s at %s: %v\n", //nolint:errcheck // best-effort stderr
				formatSinkAgents(req.Agents), req.Request.SinkDir, merr)
			errs = append(errs, fmt.Errorf("skill sink %s (%s): %w", req.Request.SinkDir, formatSinkAgents(req.Agents), merr))
			continue
		}
		for _, s := range res.Skipped {
			// A skip is the standing outcome for user content at a sink
			// path; report it once per episode, not on every pass
			// (skill_skip_log.go). The skip is attributed to the member
			// whose entry the sink wanted at that name.
			agent := req.wantedBy(s.Name)
			msg := fmt.Sprintf("gc: agent %q skipped skill %q at %s — %s", agent, s.Name, s.Path, s.Reason)
			if skipPass.shouldReport(agent, s.Path, msg) {
				fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
			}
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(stderr, "gc: %s stage-1 materialize warning: %s\n", //nolint:errcheck // best-effort stderr
				formatSinkAgents(req.Agents), w)
		}
	}
	return errors.Join(errs...)
}

// stage1SkillRequest is the stage-1 skill materialization pass of one
// vendor sink, written on behalf of every agent that shares it.
type stage1SkillRequest struct {
	// Agents lists the sink's member agents in config order.
	Agents []string
	// Request reconciles the sink against the union of the members'
	// catalogs.
	Request materialize.Request
	// WantedBy attributes each desired skill name to a member agent.
	WantedBy map[string]string
	// Held lists the reasons the sink must not be written in this pass.
	// A request with any reason is reported, never run.
	Held []string
}

// reportHeldSkillSink logs why req's sink is left unchanged.
func reportHeldSkillSink(stderr io.Writer, req stage1SkillRequest) {
	for _, reason := range req.Held {
		fmt.Fprintf(stderr, "gc: stage-1 materialize-skills: leaving %s unchanged: %s\n", req.Request.SinkDir, reason) //nolint:errcheck // best-effort stderr
	}
}

// formatSinkConflict describes a skill name that members of one sink want
// from different sources of the same class.
func formatSinkConflict(c materialize.SinkConflict) string {
	class := "shared"
	if c.Local {
		class = "agent-local"
	}
	sources := make([]string, len(c.Sources))
	for i, src := range c.Sources {
		sources[i] = fmt.Sprintf("agent %q (%s)", src.Agent, src.Source)
	}
	return fmt.Sprintf("skill %q is wanted from different %s sources: %s", c.Name, class, strings.Join(sources, ", "))
}

// wantedBy names the member agent a report about skill name belongs to:
// the member whose entry the sink wants, else the sink's first member.
func (r stage1SkillRequest) wantedBy(name string) string {
	if agent, ok := r.WantedBy[name]; ok {
		return agent
	}
	return r.Agents[0]
}

// formatSinkAgents renders a sink's member agents for log lines.
func formatSinkAgents(agents []string) string {
	quoted := make([]string, len(agents))
	for i, a := range agents {
		quoted[i] = strconv.Quote(a)
	}
	if len(quoted) == 1 {
		return "agent " + quoted[0]
	}
	return "agents " + strings.Join(quoted, ", ")
}

// stage1SkillRequests builds the stage-1 skill pass of every eligible
// agent's sink, one request per physical sink, ordered by each sink's
// first member in config order. Each scope's shared catalog loads once.
// Catalog load errors are logged and degrade as
// runStage1SkillMaterialization documents. Both the real pass and the
// dry-run plan run exactly these requests.
func stage1SkillRequests(cityPath string, cfg *config.City, stderr io.Writer) []stage1SkillRequest {
	catalogs := make(map[string]materialize.CityCatalog)
	loadCatalog := func(rigName string) materialize.CityCatalog {
		if cat, ok := catalogs[rigName]; ok {
			return cat
		}
		result := loadSharedSkillCatalogWithFallback(cityPath, cfg, rigName)
		cat := result.Catalog
		if result.Err != nil {
			if stderr != nil {
				if rigName == "" {
					fmt.Fprintf(stderr, "gc: stage-1 materialize-skills: load shared skill catalog for city scope: %v\n", result.Err) //nolint:errcheck // best-effort stderr
				} else {
					fmt.Fprintf(stderr, "gc: stage-1 materialize-skills: load shared skill catalog for rig %q: %v\n", rigName, result.Err) //nolint:errcheck // best-effort stderr
				}
			}
			if result.Mode == sharedCatalogLoadDirect {
				cat.Entries = nil
				cat.Shadowed = nil
			}
			catalogs[rigName] = cat
			return catalogs[rigName]
		}
		catalogs[rigName] = cat
		return cat
	}

	type sinkGroup struct {
		sinkDir string
		agents  []string
		members []materialize.SinkMember
	}
	var groups []*sinkGroup
	groupBySink := make(map[string]*sinkGroup)
	rt := cityMaterializationRuntime(cfg, cityPath)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if !canStage1Materialize(rt, agent) {
			continue
		}
		provider := effectiveAgentProviderFamily(agent, cfg.Workspace.Provider, cfg.Providers)
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

		// Resolve the agent's scope root to an absolute path. Use the
		// un-canonicalized form here so the materializer writes into
		// the operator-intended location (e.g., /city/rigs/fe even
		// when it's a symlink to /private/city/...). canonicalisation
		// happens at comparison time inside MaterializeAgent via
		// EvalSymlinks, so owner-root matching still works. Sinks group
		// by their canonical path, so a symlinked rig path or a rig at
		// the city root joins the sink it really is, and the group keeps
		// its first member's spelling.
		scopeRoot := resolveAgentScopeRoot(agent, cityPath, cfg.Rigs)
		if !filepath.IsAbs(scopeRoot) {
			scopeRoot = filepath.Join(cityPath, scopeRoot)
		}
		sinkDir := filepath.Join(scopeRoot, vendor)
		key := pathutil.NormalizePathForCompare(sinkDir)
		group := groupBySink[key]
		if group == nil {
			group = &sinkGroup{sinkDir: sinkDir}
			groupBySink[key] = group
			groups = append(groups, group)
		}
		group.agents = append(group.agents, agent.QualifiedName())
		group.members = append(group.members, materialize.SinkMember{
			Agent:  agent.QualifiedName(),
			Shared: cityCat,
			Local:  agentCat,
		})
	}

	reqs := make([]stage1SkillRequest, 0, len(groups))
	for _, group := range groups {
		union := materialize.UnionSinkMembers(group.members)
		var held []string
		for _, c := range union.Conflicts {
			held = append(held, formatSinkConflict(c))
		}
		if len(held) == 0 && len(union.Desired) == 0 && len(union.OwnedRoots) == 0 {
			continue
		}
		reqs = append(reqs, stage1SkillRequest{
			Agents: group.agents,
			Request: materialize.Request{
				SinkDir:          group.sinkDir,
				Desired:          union.Desired,
				OwnedRoots:       union.OwnedRoots,
				LegacyNames:      materialize.LegacyStubNames(),
				LegacyOwnedRoots: materialize.LegacyOwnedRootsFor(cityPath),
			},
			WantedBy: union.WantedBy,
			Held:     held,
		})
	}
	return reqs
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
