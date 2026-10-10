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
// materializer once per sink with the union of what those agents want.
// A pass per agent would let each agent prune the agent-local skills
// another agent wrote, since the sink's ownership manifest marks every
// gc-written link as prunable by any later pass. The grouping covers
// the stage-1 passes (gc start, city start, applied reload) only; the
// per-session path still materializes one agent per run (see
// materializeSkillsIntoWorkdir).
//
// Catalog load happens once per scope per call and feeds every
// agent's materialization in this tick. A sink with an agent catalog
// that cannot be read, a name it cannot resolve, or a Run error is
// logged to stderr and left as it is for the pass; the pass goes on to
// reconcile every other sink. Shared-catalog load failures are also
// logged and then downgraded to an empty shared desired set, while
// preserving owned-root cleanup so stale gc-managed symlinks can still
// be pruned.
//
// The returned error joins one error per sink the pass left as it was
// (stage1UnreconciledSinks splits it), so a caller can surface them; it
// is nil when every sink reconciled. It never means the pass stopped
// early.
func runStage1SkillMaterialization(cityPath string, cfg *config.City, stderr io.Writer) error {
	if cfg == nil {
		return nil
	}
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

	skipPass := stage1SkillSkipLog.beginPass(cityPath)
	defer skipPass.finish()

	sinks := make(map[string]*stage1Sink)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if !canStage1Materialize(cfg.Session.Provider, agent) {
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

		// Group by the sink's physical directory: a rig path that is a
		// symlink to another rig's path, or to the city root, reaches
		// the same sink, and two passes over one sink would prune each
		// other's links. The first agent's spelling is the one written.
		sinkKey := pathutil.NormalizePathForCompare(sinkDir)
		sink := sinks[sinkKey]
		if sink == nil {
			sink = newStage1Sink(sinkDir)
			sinks[sinkKey] = sink
		}
		sink.add(agent.QualifiedName(), cityCat, agentCat)
		if lerr != nil {
			// Fail closed: without this agent's catalog the pass cannot
			// tell its links from stale ones, so the sink is left as it
			// is rather than pruning them.
			sink.unreadable = append(sink.unreadable, fmt.Sprintf("agent %q: LoadAgentCatalog %q: %v",
				agent.QualifiedName(), agent.SkillsDir, lerr))
		}
	}

	var unreconciled []error
	sinkKeys := make([]string, 0, len(sinks))
	for key := range sinks {
		sinkKeys = append(sinkKeys, key)
	}
	sort.Strings(sinkKeys)
	for _, key := range sinkKeys {
		sink := sinks[key]
		dir := sink.dir
		// An agent catalog the pass could not read, or a name the sink
		// cannot resolve, degrades this sink's reconciliation for the
		// pass: its existing links stay, and additions and removals for
		// every agent in it wait until the problem is resolved. The
		// config itself still applies, and every other sink reconciles
		// normally.
		if problems := sink.problems(); len(problems) > 0 {
			for _, p := range problems {
				fmt.Fprintf(stderr, "gc: stage-1 materialize-skills at %s: %s; sink not reconciled this pass (existing links kept, additions and removals wait until it is resolved)\n", dir, p) //nolint:errcheck // best-effort stderr
			}
			unreconciled = append(unreconciled, fmt.Errorf("skill sink %s not reconciled: %s", dir, strings.Join(problems, "; ")))
			sink.markIncomplete(skipPass)
			continue
		}
		desired := sink.desired()
		if len(desired) == 0 && len(sink.owned) == 0 {
			continue
		}

		res, merr := materialize.Run(materialize.Request{
			SinkDir:          dir,
			Desired:          desired,
			OwnedRoots:       sink.owned,
			LegacyNames:      materialize.LegacyStubNames(),
			LegacyOwnedRoots: materialize.LegacyOwnedRootsFor(cityPath),
		})
		if merr != nil {
			sink.markIncomplete(skipPass)
			fmt.Fprintf(stderr, "gc: stage-1 materialize-skills at %s (agents %s): %v\n", dir, sink.agentList(), merr) //nolint:errcheck // best-effort stderr
			unreconciled = append(unreconciled, fmt.Errorf("skill sink %s (agents %s) not reconciled: %w", dir, sink.agentList(), merr))
			continue
		}
		for _, s := range res.Skipped {
			// A skip is the standing outcome for user content at a sink
			// path; report it once per episode, not on every pass
			// (skill_skip_log.go). It is reported once per sink, under
			// the first agent that wants the skill.
			agent := sink.wantedBy(s.Name)
			msg := fmt.Sprintf("gc: agent %q skipped skill %q at %s — %s", agent, s.Name, s.Path, s.Reason)
			if skipPass.shouldReport(agent, s.Path, msg) {
				fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
			}
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(stderr, "gc: stage-1 materialize warning at %s (agents %s): %s\n", dir, sink.agentList(), w) //nolint:errcheck // best-effort stderr
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

// stage1SinkWant is one skill a shared sink must hold, with the first
// agent (in config order) that asked for it.
type stage1SinkWant struct {
	entry materialize.SkillEntry
	agent string
}

// stage1Sink accumulates, for one sink directory, the union of the
// skills wanted by every agent that materializes into it, and the union
// of the gc-managed roots those agents may prune under. Shared-catalog
// and agent-local entries are kept apart so desired can apply the
// precedence materialize.EffectiveSet applies to one agent.
type stage1Sink struct {
	dir        string
	agents     []string
	shared     map[string]stage1SinkWant
	local      map[string]stage1SinkWant
	clashes    []stage1SinkClash
	unreadable []string
	owned      []string
	ownedSeen  map[string]bool
}

// stage1SinkClash records a name wanted from two sources in the same
// class (shared or agent-local).
type stage1SinkClash struct {
	name  string
	local bool
	msg   string
}

// newStage1Sink returns an empty sink that the materializer writes at dir.
func newStage1Sink(dir string) *stage1Sink {
	return &stage1Sink{
		dir:       dir,
		shared:    make(map[string]stage1SinkWant),
		local:     make(map[string]stage1SinkWant),
		ownedSeen: make(map[string]bool),
	}
}

// add merges one agent's shared and agent-local catalogs into the sink.
func (s *stage1Sink) add(agent string, city materialize.CityCatalog, local materialize.AgentCatalog) {
	s.agents = append(s.agents, agent)
	for _, e := range city.Entries {
		s.want(s.shared, false, agent, e)
	}
	for _, e := range local.Entries {
		s.want(s.local, true, agent, e)
	}
	for _, root := range city.OwnedRoots {
		s.addOwned(root)
	}
	s.addOwned(local.OwnedRoot)
}

// want records that agent wants entry e in the given class. A name
// wanted from one source by several agents is wanted once; a second
// source for the same name is recorded as a clash. Sources are compared
// by physical directory, so two paths to one directory are one source.
func (s *stage1Sink) want(class map[string]stage1SinkWant, local bool, agent string, e materialize.SkillEntry) {
	prev, ok := class[e.Name]
	if !ok {
		class[e.Name] = stage1SinkWant{entry: e, agent: agent}
		return
	}
	if pathutil.NormalizePathForCompare(prev.entry.Source) != pathutil.NormalizePathForCompare(e.Source) {
		s.clashes = append(s.clashes, stage1SinkClash{
			name:  e.Name,
			local: local,
			msg: fmt.Sprintf("skill %q is provided by agent %q from %s and by agent %q from %s",
				e.Name, prev.agent, prev.entry.Source, agent, e.Source),
		})
	}
}

// conflicts returns the clashes the sink cannot resolve: two agent-local
// sources for one name, or two shared sources for a name no agent-local
// entry overrides. An agent-local override settles a shared clash,
// because the sink links the agent-local source either way. The sink
// holds one link per name, so choosing between two sources of the same
// class would silently drop one agent's skill.
func (s *stage1Sink) conflicts() []string {
	var out []string
	for _, c := range s.clashes {
		if _, overridden := s.local[c.name]; c.local || !overridden {
			out = append(out, c.msg)
		}
	}
	return out
}

// problems returns why the sink cannot be reconciled this pass: agent
// catalogs that could not be read, then unresolved conflicts.
func (s *stage1Sink) problems() []string {
	return append(append([]string(nil), s.unreadable...), s.conflicts()...)
}

// agentList names the sink's agents, in config order, for log lines.
func (s *stage1Sink) agentList() string {
	quoted := make([]string, len(s.agents))
	for i, agent := range s.agents {
		quoted[i] = fmt.Sprintf("%q", agent)
	}
	return strings.Join(quoted, ", ")
}

// addOwned adds root to the sink's prunable roots once.
func (s *stage1Sink) addOwned(root string) {
	if root == "" || s.ownedSeen[root] {
		return
	}
	s.ownedSeen[root] = true
	s.owned = append(s.owned, root)
}

// desired returns the sink's wanted entries, sorted by name, with the
// precedence materialize.EffectiveSet applies to one agent: an
// agent-local entry overrides a shared entry of the same name. The sink
// holds one link per name, so an override from one agent also replaces
// the shared version for every other agent writing the sink.
func (s *stage1Sink) desired() []materialize.SkillEntry {
	var shared materialize.CityCatalog
	for _, w := range s.shared {
		shared.Entries = append(shared.Entries, w.entry)
	}
	var local materialize.AgentCatalog
	for _, w := range s.local {
		local.Entries = append(local.Entries, w.entry)
	}
	return materialize.EffectiveSet(shared, local)
}

// wantedBy returns the agent a skip of name is reported under: the agent
// whose entry desired chose for that name.
func (s *stage1Sink) wantedBy(name string) string {
	if w, ok := s.local[name]; ok {
		return w.agent
	}
	return s.shared[name].agent
}

// markIncomplete carries every agent's previously reported skips in this
// sink over to the next pass, since this pass did not finish the sink.
func (s *stage1Sink) markIncomplete(pass *skillSkipPass) {
	for _, agent := range s.agents {
		pass.markIncomplete(agent)
	}
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
