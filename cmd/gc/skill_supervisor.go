package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
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
// Agents with the same scope root and provider family share one sink
// directory, so the pass groups agents by sink and runs the
// materializer once per sink with the union of what those agents want
// (materialize.SinkUnion). A pass per agent would let each agent prune
// the agent-local skills another agent wrote, since the sink's
// ownership manifest marks every gc-written link as prunable by any
// later pass.
//
// Catalog load happens once per scope per call and feeds every
// agent's materialization in this tick. A sink with an agent catalog
// that cannot be read, a name two writers want from different
// sources, or a Run error is logged to stderr and left as it is for
// the pass; the pass goes on to reconcile every other sink.
// Shared-catalog load failures are also logged and then downgraded to
// an empty shared desired set, while preserving owned-root cleanup so
// stale gc-managed symlinks can still be pruned.
//
// The returned error joins one error per sink the pass left as it was
// (stage1UnreconciledSinks splits it), so a caller can surface them; it
// is nil when every sink reconciled. It never means the pass stopped
// early.
func runStage1SkillMaterialization(cityPath string, cfg *config.City, stderr io.Writer) error {
	if cfg == nil {
		return nil
	}
	skipPass := stage1SkillSkipLog.beginPass(cityPath)
	defer skipPass.finish()

	var unreconciled []error
	for _, req := range stage1SkillRequests(cityPath, cfg, stderr) {
		dir := req.Request.SinkDir
		if problems := req.Problems; len(problems) > 0 {
			// An agent catalog the pass could not read, or a name the
			// sink cannot resolve, degrades this sink's reconciliation
			// for the pass: its existing links stay, and additions and
			// removals for every agent in it wait until the problem is
			// resolved. The config itself still applies, and every other
			// sink reconciles normally.
			for _, p := range problems {
				fmt.Fprintf(stderr, "gc: stage-1 materialize-skills at %s: %s; sink not reconciled this pass (existing links kept, additions and removals wait until it is resolved)\n", dir, p) //nolint:errcheck // best-effort stderr
			}
			unreconciled = append(unreconciled, fmt.Errorf("skill sink %s not reconciled: %s", dir, strings.Join(problems, "; ")))
			for _, agent := range req.Union.Agents() {
				skipPass.markIncomplete(agent)
			}
			continue
		}
		res, merr := materialize.Run(req.Request)
		if merr != nil {
			for _, agent := range req.Union.Agents() {
				skipPass.markIncomplete(agent)
			}
			fmt.Fprintf(stderr, "gc: stage-1 materialize-skills at %s (agents %s): %v\n", dir, quotedAgentList(req.Union.Agents()), merr) //nolint:errcheck // best-effort stderr
			unreconciled = append(unreconciled, fmt.Errorf("skill sink %s (agents %s) not reconciled: %w", dir, quotedAgentList(req.Union.Agents()), merr))
			continue
		}
		for _, s := range res.Skipped {
			// A skip is the standing outcome for user content at a sink
			// path; report it once per episode, not on every pass
			// (skill_skip_log.go). It is reported once per sink, under
			// the first agent that wants the skill.
			agent := req.Union.WantedBy(s.Name)
			msg := fmt.Sprintf("gc: agent %q skipped skill %q at %s — %s", agent, s.Name, s.Path, s.Reason)
			if skipPass.shouldReport(agent, s.Path, msg) {
				fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
			}
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(stderr, "gc: stage-1 materialize warning at %s (agents %s): %s\n", dir, quotedAgentList(req.Union.Agents()), w) //nolint:errcheck // best-effort stderr
		}
	}
	return errors.Join(unreconciled...)
}

// stage1UnreconciledSinks splits the error runStage1SkillMaterialization
// returns into one error per sink it left as it was.
func stage1UnreconciledSinks(err error) []error {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		return joined.Unwrap()
	}
	if err != nil {
		return []error{err}
	}
	return nil
}

// stage1SkillRequest is one sink's stage-1 skill materialization pass.
type stage1SkillRequest struct {
	// Agent is the sink's first configured writer, in config order — the
	// reporting key for messages that predate per-sink grouping.
	Agent string
	// Union is the fold of every writer's catalogs for this sink. It is
	// kept so post-Run reporting can attribute a skip to the agent whose
	// entry the desired set chose for that name.
	Union *materialize.SinkUnion
	// Request is the materialization pass to run. It is meaningful only
	// when Problems is empty.
	Request materialize.Request
	// Problems holds why this pass cannot reconcile the sink (an agent
	// catalog that could not be read, or a name two writers want from
	// different sources). A request with Problems is held: it is not run,
	// and the sink keeps its links until the problem resolves.
	Problems []string
}

// stage1SkillRequests builds the stage-1 skill pass of every eligible
// agent, grouped so each physical sink directory is materialized once,
// over the union of everything its configured writers want. Sinks are
// grouped by their canonical path, so a rig path that is a symlink to
// another rig's path (or to the city root) reaches one sink, and the
// first writer's spelling of the directory is the one written.
//
// Catalog load errors are logged and degrade as
// runStage1SkillMaterialization documents. Both the real pass and the
// dry-run plan run exactly these requests; a request with Problems is
// held in both.
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
		return catalogs[rigName]
	}

	type sinkState struct {
		dir   string
		union *materialize.SinkUnion
	}
	sinks := make(map[string]*sinkState)
	var sinkOrder []string
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

		rigName := agentRigScopeName(agent, cfg.Rigs)
		cityCat := loadCatalog(rigName)

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

		sinkKey := pathutil.NormalizePathForCompare(sinkDir)
		sink := sinks[sinkKey]
		if sink == nil {
			sink = &sinkState{dir: sinkDir, union: materialize.NewSinkUnion()}
			sinks[sinkKey] = sink
			sinkOrder = append(sinkOrder, sinkKey)
		}
		sink.union.AddAgent(agent.QualifiedName(), cityCat, agentCat)
		if lerr != nil {
			// Fail closed: without this agent's catalog the pass cannot
			// tell its links from stale ones, so the sink is left as it
			// is rather than pruning them.
			sink.union.MarkUnreadable(fmt.Sprintf("agent %q: LoadAgentCatalog %q: %v",
				agent.QualifiedName(), agent.SkillsDir, lerr))
		}
	}

	sort.Strings(sinkOrder)
	var reqs []stage1SkillRequest
	for _, key := range sinkOrder {
		sink := sinks[key]
		union := sink.union
		req := stage1SkillRequest{
			Agent:    union.Agents()[0],
			Union:    union,
			Problems: union.Problems(),
			Request: materialize.Request{
				SinkDir:          sink.dir,
				Desired:          union.Desired(),
				OwnedRoots:       union.OwnedRoots(),
				LegacyNames:      materialize.LegacyStubNames(),
				LegacyOwnedRoots: materialize.LegacyOwnedRootsFor(cityPath),
			},
		}
		if len(req.Problems) == 0 && len(req.Request.Desired) == 0 && len(req.Request.OwnedRoots) == 0 {
			continue
		}
		reqs = append(reqs, req)
	}
	return reqs
}

// quotedAgentList names a sink's writers, in fold order, for log lines.
func quotedAgentList(agents []string) string {
	quoted := make([]string, len(agents))
	for i, agent := range agents {
		quoted[i] = fmt.Sprintf("%q", agent)
	}
	return strings.Join(quoted, ", ")
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
