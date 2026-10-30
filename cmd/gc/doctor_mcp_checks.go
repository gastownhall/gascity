package main

import (
	"fmt"
	"path/filepath"
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
	// it is an implicit agent against one that defines its own servers.
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
		FixHint: `make the effective MCP payload identical for every agent that shares a provider-native target, or split them onto different targets`,
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

	type targetState struct {
		hashes map[string][]*config.Agent
	}

	targets := make(map[string]*targetState)
	var (
		issues []string
		skips  mcpSkipGroups
	)

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, agent, lookPath)
		if err != nil {
			issues = append(issues, fmt.Sprintf("agent %q: %v", agent.QualifiedName(), err))
			continue
		}
		skips.add(mcpAgentLabel(agent), view.Skipped)
		// Stage-1 claimants are counted as gc start counts them, empty
		// catalogs included; a stage-2 agent claims its work_dir target
		// only with servers to write. An ACP session receives its
		// servers in-band and writes no provider-native file, so it
		// shares no target with anyone.
		if !claimsStage1MCPTarget(view) && !claimsStage2MCPTarget(view) {
			continue
		}
		key := view.Projection.Provider + "|" + filepath.Clean(view.Projection.Target)
		state := targets[key]
		if state == nil {
			state = &targetState{hashes: make(map[string][]*config.Agent)}
			targets[key] = state
		}
		hash := view.Projection.Hash()
		state.hashes[hash] = append(state.hashes[hash], agent)
	}

	sort.Strings(issues)

	conflicts := make([]mcpTargetConflict, 0, len(targets))
	for key, state := range targets {
		if len(state.hashes) < 2 {
			continue
		}
		parts := strings.SplitN(key, "|", 2)
		hashes := make([]string, 0, len(state.hashes))
		for hash := range state.hashes {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)
		agents := make([]string, 0, 4)
		cause := ""
		for i, hash := range hashes {
			for _, member := range state.hashes[hash] {
				agents = append(agents, member.QualifiedName())
			}
			for _, other := range hashes[i+1:] {
				if cause != "" {
					break
				}
				cause = mcpAgentLocalConflictCauseAmong(state.hashes[hash], state.hashes[other])
			}
		}
		sort.Strings(agents)
		conflicts = append(conflicts, mcpTargetConflict{
			Provider: parts[0],
			Target:   parts[1],
			Agents:   agents,
			Cause:    cause,
		})
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Target != conflicts[j].Target {
			return conflicts[i].Target < conflicts[j].Target
		}
		return conflicts[i].Provider < conflicts[j].Provider
	})
	return mcpProjectionHealth{issues: issues, skips: skips.lines(), conflicts: conflicts}
}

// claimsStage2MCPTarget reports whether a resolved view writes a stage-2
// target: the provider-native file in a work_dir other than its scope root.
func claimsStage2MCPTarget(view resolvedMCPProjection) bool {
	return view.Delivery == "stage2" &&
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
