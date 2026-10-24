package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
)

// planStage1MCPProjection reports what runStage1MCPProjection would change,
// writing nothing: the provider-native MCP files it would create, update,
// adopt, or remove (orphan cleanup included), and the .gitignore files it
// would extend. It returns the errors that would fail the real pass, so a
// preview fails where the start would.
func planStage1MCPProjection(cityPath string, cfg *config.City, lookPath config.LookPathFunc, stderr io.Writer) ([]string, error) {
	targets, err := buildStage1MCPTargetsWith(cityPath, cfg, lookPath, stderr, true)
	if err != nil {
		return nil, err
	}
	fs := fsys.OSFS{}
	var lines []string
	desired := make(map[string]bool, len(targets))
	gitignore := make(map[string][]string)
	for _, target := range targets {
		plan, err := target.Projection.Plan(fs)
		if err != nil {
			return nil, fmt.Errorf("reconciling %s: %w", target.Projection.Target, err)
		}
		if line := describeMCPPlan(plan); line != "" {
			lines = append(lines, line)
		}
		if plan.Refused != nil {
			// The real pass leaves the target alone: it neither claims it
			// nor adds it to .gitignore.
			continue
		}
		desired[managedKey(target.Projection.Provider, target.Root)] = true
		if len(target.Projection.Servers) > 0 {
			gitignore[target.Root] = append(gitignore[target.Root], mcpGitignoreEntry(target.Projection))
		}
	}
	orphans, err := planOrphanStage1Targets(cityPath, cfg, desired)
	if err != nil {
		return nil, err
	}
	lines = append(lines, orphans...)
	for _, root := range sortedMapKeys(gitignore) {
		missing := missingMCPGitignoreEntries(fs, root, gitignore[root])
		if len(missing) == 0 {
			continue
		}
		path := filepath.Join(root, ".gitignore")
		verb := "update"
		if _, err := fs.Stat(path); err != nil {
			verb = "create"
		}
		lines = append(lines, fmt.Sprintf("%s %s: add %s", verb, path, strings.Join(missing, ", ")))
	}
	return lines, nil
}

// planOrphanStage1Targets reports what cleanupOrphanStage1Targets would do to
// managed MCP targets no agent claims anymore: remove the file, or remove
// gc's servers from it where other settings remain.
func planOrphanStage1Targets(cityPath string, cfg *config.City, desired map[string]bool) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	var lines []string
	for _, root := range collectStage1ScopeRoots(cityPath, cfg) {
		markers, err := orphanStage1Markers(root, desired)
		if err != nil {
			return nil, err
		}
		for _, provider := range markers {
			orphan, err := materialize.BuildMCPProjection(provider, root, nil)
			if err != nil {
				return nil, fmt.Errorf("building orphan projection for %s at %s: %w", provider, root, err)
			}
			plan, err := orphan.Plan(fsys.OSFS{})
			if err != nil {
				return nil, fmt.Errorf("planning orphan %s cleanup at %s: %w", provider, root, err)
			}
			if line := describeMCPPlan(plan); line != "" {
				lines = append(lines, line+" (no agent claims it)")
			}
		}
	}
	return lines, nil
}

// describeMCPPlan renders one projection plan as a dry-run line, or "" when
// the target would not change.
func describeMCPPlan(plan materialize.MCPProjectionPlan) string {
	servers := fmt.Sprintf("%d MCP server", plan.Servers)
	if plan.Servers != 1 {
		servers += "s"
	}
	switch {
	case plan.Refused != nil:
		return fmt.Sprintf("leave %s (%s) as it is: %s; %s", plan.Target, plan.Provider, plan.Refused.Reason(), plan.Refused.Remedy())
	case plan.Adopt && plan.Provider == materialize.MCPProviderClaude:
		return fmt.Sprintf("adopt %s (%s): save its current content under %s, then replace it with %s",
			plan.Target, plan.Provider, plan.SnapshotDir, servers)
	case plan.Adopt:
		return fmt.Sprintf("adopt %s (%s): save its current content under %s, then replace its MCP servers with %s",
			plan.Target, plan.Provider, plan.SnapshotDir, servers)
	case plan.Action == materialize.MCPPlanCreate:
		return fmt.Sprintf("create %s (%s) with %s", plan.Target, plan.Provider, servers)
	case plan.Action == materialize.MCPPlanUpdate && plan.Servers == 0:
		return fmt.Sprintf("update %s (%s): remove gc's MCP servers", plan.Target, plan.Provider)
	case plan.Action == materialize.MCPPlanUpdate:
		return fmt.Sprintf("update %s (%s) to %s", plan.Target, plan.Provider, servers)
	case plan.Action == materialize.MCPPlanRemove:
		return fmt.Sprintf("remove %s (%s)", plan.Target, plan.Provider)
	default:
		return ""
	}
}

// planStage1SkillMaterialization reports what runStage1SkillMaterialization
// would change, writing nothing: per skill sink, the links it would create,
// re-point, or remove. Errors are reported inline, as the real pass logs
// them, and never fail the preview.
func planStage1SkillMaterialization(cityPath string, cfg *config.City, stderr io.Writer) []string {
	if cfg == nil {
		return nil
	}
	type sinkPlan struct {
		created, relinked, removed, stubs map[string]bool
	}
	sinks := make(map[string]*sinkPlan)
	for _, req := range stage1SkillRequests(cityPath, cfg, stderr) {
		req.Request.DryRun = true
		res, err := materialize.Run(req.Request)
		if err != nil {
			fmt.Fprintf(stderr, "gc: stage-1 materialize-skills for agent %q at %s: %v\n", req.Agent, req.Request.SinkDir, err) //nolint:errcheck // best-effort stderr
			continue
		}
		plan := sinks[req.Request.SinkDir]
		if plan == nil {
			plan = &sinkPlan{created: map[string]bool{}, relinked: map[string]bool{}, removed: map[string]bool{}, stubs: map[string]bool{}}
			sinks[req.Request.SinkDir] = plan
		}
		for _, name := range res.Created {
			plan.created[name] = true
		}
		for _, name := range res.Relinked {
			plan.relinked[name] = true
		}
		for _, name := range res.Removed {
			plan.removed[name] = true
		}
		for _, name := range res.LegacyMigrated {
			plan.stubs[name] = true
		}
	}
	dirs := make([]string, 0, len(sinks))
	for dir := range sinks {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var lines []string
	for _, dir := range dirs {
		plan := sinks[dir]
		var parts []string
		if n := len(plan.created); n > 0 {
			parts = append(parts, fmt.Sprintf("link %d skill%s", n, plural(n)))
		}
		if n := len(plan.relinked); n > 0 {
			parts = append(parts, fmt.Sprintf("re-point %d skill link%s", n, plural(n)))
		}
		if n := len(plan.removed); n > 0 {
			parts = append(parts, fmt.Sprintf("remove %d stale skill link%s", n, plural(n)))
		}
		if n := len(plan.stubs); n == 1 {
			parts = append(parts, "remove 1 legacy skill stub directory")
		} else if n > 1 {
			parts = append(parts, fmt.Sprintf("remove %d legacy skill stub directories", n))
		}
		if len(parts) > 0 {
			lines = append(lines, fmt.Sprintf("%s in %s", strings.Join(parts, ", "), dir))
		}
	}
	return lines
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
