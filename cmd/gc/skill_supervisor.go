package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

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
// Agents with the same scope root and provider family share one sink
// directory, so the pass runs the materializer once per sink with the
// union of what every writer of that sink wants (materialize.SinkUnion).
// A pass per agent would let each agent prune the agent-local skills
// another agent wrote, since the sink's ownership manifest marks every
// gc-written link as prunable by any later pass.
//
// Catalog load happens once per scope per call and feeds every sink in
// this pass. A sink with a writer catalog that cannot be read, a name
// it cannot resolve, or a Run error is logged to stderr and left as it
// is for the pass; the pass goes on to reconcile every other sink.
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
		if len(req.Problems) > 0 {
			reportHeldSkillSink(stderr, "gc: stage-1 materialize-skills", dir, req.Problems)
			unreconciled = append(unreconciled, fmt.Errorf("skill sink %s not reconciled: %s", dir, strings.Join(req.Problems, "; ")))
			req.markIncomplete(skipPass)
			continue
		}
		res, merr := materialize.Run(req.Request)
		if merr != nil {
			req.markIncomplete(skipPass)
			fmt.Fprintf(stderr, "gc: stage-1 materialize-skills at %s (agents %s): %v\n", dir, req.Union.AgentList(), merr) //nolint:errcheck // best-effort stderr
			unreconciled = append(unreconciled, fmt.Errorf("skill sink %s (agents %s) not reconciled: %w", dir, req.Union.AgentList(), merr))
			continue
		}
		for _, s := range res.Skipped {
			// A skip is the standing outcome for user content at a sink
			// path; report it once per episode, not on every pass
			// (skill_skip_log.go). It is reported once per sink, under
			// the agent whose entry the sink wants for that name.
			agent := req.Union.WantedBy(s.Name)
			msg := fmt.Sprintf("gc: agent %q skipped skill %q at %s — %s", agent, s.Name, s.Path, s.Reason)
			if skipPass.shouldReport(agent, s.Path, msg) {
				fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
			}
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(stderr, "gc: stage-1 materialize warning at %s (agents %s): %s\n", dir, req.Union.AgentList(), w) //nolint:errcheck // best-effort stderr
		}
	}
	return errors.Join(unreconciled...)
}

// reportHeldSkillSink logs why a sink was held rather than reconciled.
func reportHeldSkillSink(stderr io.Writer, prefix, dir string, problems []string) {
	for _, p := range problems {
		fmt.Fprintf(stderr, "%s at %s: %s; sink not reconciled this pass (existing links kept, additions and removals wait until it is resolved)\n", prefix, dir, p) //nolint:errcheck // best-effort stderr
	}
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

// stage1SkillRequest is one sink's stage-1 skill materialization pass,
// folded over every writer of that sink.
type stage1SkillRequest struct {
	// Union is the sink's writers and what they want.
	Union *materialize.SinkUnion
	// Problems is non-empty when the sink must be held this pass.
	Problems []string
	// Request is the materializer pass; it runs only when Problems is
	// empty.
	Request materialize.Request
}

// markIncomplete carries every writer's previously reported skips in
// this sink over to the next pass, since this pass did not finish it.
func (r stage1SkillRequest) markIncomplete(pass *skillSkipPass) {
	for _, agent := range r.Union.Agents() {
		pass.markIncomplete(agent)
	}
}

// stage1SkillRequests builds the stage-1 skill pass of every scope-root
// sink, sorted by sink, loading each scope's shared catalog once. Catalog
// load errors are logged and degrade as runStage1SkillMaterialization
// documents. Both the real pass and the dry-run plan run exactly these
// requests.
func stage1SkillRequests(cityPath string, cfg *config.City, stderr io.Writer) []stage1SkillRequest {
	catalogs := newSkillScopeCatalogs(func(rigName string) materialize.CityCatalog {
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
		}
		return cat
	})

	var reqs []stage1SkillRequest
	for _, sink := range stage1SkillSinks(cityPath, cfg) {
		union := materialize.NewSinkUnion()
		for _, w := range sink.Writers {
			addSkillSinkWriter(union, w.Agent, w.Name, catalogs.load(agentRigScopeName(w.Agent, cfg.Rigs)))
		}
		owned := union.OwnedRoots()
		desired := union.Desired()
		problems := union.Problems()
		if len(problems) == 0 && len(desired) == 0 && len(owned) == 0 {
			continue
		}
		reqs = append(reqs, stage1SkillRequest{
			Union:    union,
			Problems: problems,
			Request: materialize.Request{
				SinkDir:          sink.Dir,
				Desired:          desired,
				OwnedRoots:       owned,
				LegacyNames:      materialize.LegacyStubNames(),
				LegacyOwnedRoots: materialize.LegacyOwnedRootsFor(cityPath),
			},
		})
	}
	return reqs
}

// addSkillSinkWriter folds one writer's agent-local catalog and its
// scope's shared catalog into union. A writer whose agent catalog cannot
// be read makes the union incomplete, so the sink is held rather than
// pruning that writer's links.
func addSkillSinkWriter(union *materialize.SinkUnion, agent *config.Agent, name string, cityCat materialize.CityCatalog) {
	agentCat, err := materialize.LoadAgentCatalog(agent.SkillsDir)
	union.Add(name, cityCat, agentCat)
	if err != nil {
		union.MarkUnreadable(fmt.Sprintf("agent %q: LoadAgentCatalog %q: %v", name, agent.SkillsDir, err))
	}
}

// skillScopeCatalogs loads each scope's shared skill catalog once.
type skillScopeCatalogs struct {
	loaded map[string]materialize.CityCatalog
	loadFn func(rigName string) materialize.CityCatalog
}

func newSkillScopeCatalogs(loadFn func(rigName string) materialize.CityCatalog) *skillScopeCatalogs {
	return &skillScopeCatalogs{loaded: make(map[string]materialize.CityCatalog), loadFn: loadFn}
}

func (c *skillScopeCatalogs) load(rigName string) materialize.CityCatalog {
	if cat, ok := c.loaded[rigName]; ok {
		return cat
	}
	cat := c.loadFn(rigName)
	c.loaded[rigName] = cat
	return cat
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
