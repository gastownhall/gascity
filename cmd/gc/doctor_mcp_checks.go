package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

type mcpConfigDoctorCheck struct {
	cityPath string
	cfg      *config.City
	lookPath config.LookPathFunc
}

type mcpSharedTargetDoctorCheck struct {
	cityPath string
	cfg      *config.City
	lookPath config.LookPathFunc
}

type mcpTargetConflict struct {
	Provider string
	Target   string
	Agents   []string
	// Cause names the agent-local mcp/ directory behind the conflict when
	// an implicit agent and an agent with servers from its own mcp/
	// directory project different payloads at their scope root
	// (mcpAgentLocalConflictCause).
	Cause string
}

func newMCPConfigDoctorCheck(cityPath string, cfg *config.City, lookPath config.LookPathFunc) *mcpConfigDoctorCheck {
	return &mcpConfigDoctorCheck{cityPath: cityPath, cfg: cfg, lookPath: lookPath}
}

func newMCPSharedTargetDoctorCheck(cityPath string, cfg *config.City, lookPath config.LookPathFunc) *mcpSharedTargetDoctorCheck {
	return &mcpSharedTargetDoctorCheck{cityPath: cityPath, cfg: cfg, lookPath: lookPath}
}

func (*mcpConfigDoctorCheck) Name() string                     { return "mcp-config" }
func (*mcpConfigDoctorCheck) CanFix() bool                     { return false }
func (*mcpConfigDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *mcpConfigDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	health := inspectMCPProjectionHealth(c.cityPath, c.cfg, c.lookPath)
	switch {
	case len(health.issues) > 0:
		return &doctor.CheckResult{
			Name:    c.Name(),
			Status:  doctor.StatusError,
			Message: summarizeMCPIssues(health.issues),
			Details: append(append([]string(nil), health.issues...), health.skips...),
			FixHint: `fix the reported MCP definitions or provider/runtime settings, then rerun "gc doctor"`,
		}
	case len(health.skips) > 0:
		return &doctor.CheckResult{
			Name:    c.Name(),
			Status:  doctor.StatusWarning,
			Message: summarizeMCPIssues(health.skips),
			Details: health.skips,
			FixHint: "these agents start without the named MCP servers; if one needs a server, switch it to a provider that can receive it",
		}
	default:
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "MCP definitions and delivery paths are valid"}
	}
}

func (*mcpSharedTargetDoctorCheck) Name() string                     { return "mcp-shared-target" }
func (*mcpSharedTargetDoctorCheck) CanFix() bool                     { return false }
func (*mcpSharedTargetDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *mcpSharedTargetDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	conflicts := inspectMCPProjectionHealth(c.cityPath, c.cfg, c.lookPath).conflicts
	if len(conflicts) == 0 {
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "no projected MCP target conflicts"}
	}
	details := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		detail := fmt.Sprintf(
			"%s (%s): %s",
			conflict.Target,
			conflict.Provider,
			strings.Join(conflict.Agents, ", "),
		)
		if conflict.Cause != "" {
			detail += "; " + conflict.Cause
		}
		details = append(details, detail)
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusError,
		Message: summarizeMCPIssues(details),
		Details: details,
		FixHint: `make the effective MCP payload identical for every agent that shares a provider-native target (define shared servers in an mcp/ directory they all receive), or give them different work_dirs`,
	}
}

// mcpProjectionHealth is what gc doctor reports about MCP projection.
type mcpProjectionHealth struct {
	// issues are per-agent errors: broken definitions, undeliverable
	// targets, and servers in an agent's own mcp/ directory that its
	// provider cannot receive.
	issues []string
	// skips name agents that start without shared-layer servers their
	// provider cannot receive.
	skips []string
	// conflicts are shared targets with diverging payloads.
	conflicts []mcpTargetConflict
}

func inspectMCPProjectionHealth(cityPath string, cfg *config.City, lookPath config.LookPathFunc) mcpProjectionHealth {
	if cfg == nil || len(cfg.Agents) == 0 {
		return mcpProjectionHealth{}
	}
	var (
		// stage1 holds the claimants gc start compares at stage 1, empty
		// catalogs included; writers holds the agents with servers to
		// write, at stage 1 or stage 2.
		stage1, writers mcpTargetClaims
		issues          []string
		skips           mcpSkipGroups
	)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, agent, lookPath)
		if err != nil {
			issues = append(issues, fmt.Sprintf("agent %q: %v", agent.QualifiedName(), err))
			continue
		}
		skips.add(mcpAgentLabel(agent), view.Skipped)
		// A stage-2 work_dir can be another agent's scope root, so writers
		// are compared across deliveries. An empty stage-1 catalog is
		// compared only with the other stage-1 claimants of its target, as
		// gc start compares it. An ACP session receives its servers
		// in-band and writes no provider-native file, so it shares no
		// target with anyone.
		key := view.Projection.Provider + "|" + filepath.Clean(view.Projection.Target)
		if claimsStage1MCPTarget(view) {
			stage1.add(key, view)
		}
		if writesMCPTarget(view) {
			writers.add(key, view)
		}
	}
	sort.Strings(issues)
	return mcpProjectionHealth{
		issues:    issues,
		skips:     skips.lines(),
		conflicts: mcpTargetConflicts(cfg.Session.Provider, &stage1, &writers),
	}
}

// mcpTargetConflicts returns every target whose claimants in one of sets
// project different payloads, sorted by target and provider. A target that
// conflicts in more than one set is reported once, naming the claimants of
// each set it conflicts in and the first agent-local cause.
func mcpTargetConflicts(sessionProvider string, sets ...*mcpTargetClaims) []mcpTargetConflict {
	byKey := make(map[string]*mcpTargetConflict)
	for _, set := range sets {
		for key, claim := range set.byKey {
			if len(claim.hashes) < 2 {
				continue
			}
			conflict := byKey[key]
			if conflict == nil {
				target := claim.first().Projection
				conflict = &mcpTargetConflict{Provider: target.Provider, Target: filepath.Clean(target.Target)}
				byKey[key] = conflict
			}
			conflict.Agents = append(conflict.Agents, claim.agents(claim.hashes...)...)
			if conflict.Cause == "" {
				conflict.Cause = claim.agentLocalCause(sessionProvider)
			}
		}
	}
	conflicts := make([]mcpTargetConflict, 0, len(byKey))
	for _, conflict := range byKey {
		slices.Sort(conflict.Agents)
		conflict.Agents = slices.Compact(conflict.Agents)
		conflicts = append(conflicts, *conflict)
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Target != conflicts[j].Target {
			return conflicts[i].Target < conflicts[j].Target
		}
		return conflicts[i].Provider < conflicts[j].Provider
	})
	return conflicts
}

// writesMCPTarget reports whether a resolved view has servers to write to a
// provider-native file: at stage 1 to its scope root, or at stage 2 to a
// work_dir other than its scope root.
func writesMCPTarget(view resolvedMCPProjection) bool {
	return (view.Delivery == "stage1" || view.Delivery == "stage2") &&
		len(view.Catalog.Servers) > 0 &&
		strings.TrimSpace(view.Projection.Provider) != "" &&
		strings.TrimSpace(view.Projection.Target) != ""
}

func summarizeMCPIssues(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return fmt.Sprintf("%s (and %d more)", items[0], len(items)-1)
	}
}
