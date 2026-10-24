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
	Provider      string
	Target        string
	Agents        []string
	AgentLocalDir string
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
	issues, _ := inspectMCPProjectionHealth(c.cityPath, c.cfg, c.lookPath)
	if len(issues) == 0 {
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "MCP definitions and delivery paths are valid"}
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusError,
		Message: summarizeMCPIssues(issues),
		Details: issues,
		FixHint: `fix the reported MCP definitions or provider/runtime settings, then rerun "gc doctor"`,
	}
}

func (*mcpSharedTargetDoctorCheck) Name() string                     { return "mcp-shared-target" }
func (*mcpSharedTargetDoctorCheck) CanFix() bool                     { return false }
func (*mcpSharedTargetDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *mcpSharedTargetDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	_, conflicts := inspectMCPProjectionHealth(c.cityPath, c.cfg, c.lookPath)
	if len(conflicts) == 0 {
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "no projected MCP target conflicts"}
	}
	details := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		if conflict.AgentLocalDir != "" {
			details = append(details, fmt.Sprintf(
				"%s (%s): %s — caused by the agent-local MCP catalog at %s, which differs "+
					"from the provider's shared city-root catalog; give this agent its own "+
					"work_dir, or move its MCP server definitions into the city's mcp/ "+
					"directory instead",
				conflict.Target,
				conflict.Provider,
				strings.Join(conflict.Agents, ", "),
				conflict.AgentLocalDir,
			))
			continue
		}
		details = append(details, fmt.Sprintf(
			"%s (%s): %s",
			conflict.Target,
			conflict.Provider,
			strings.Join(conflict.Agents, ", "),
		))
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusError,
		Message: summarizeMCPIssues(details),
		Details: details,
		FixHint: `make the effective MCP payload identical for every agent that shares a provider-native target, or split them onto different targets`,
	}
}

func inspectMCPProjectionHealth(cityPath string, cfg *config.City, lookPath config.LookPathFunc) ([]string, []mcpTargetConflict) {
	if cfg == nil || len(cfg.Agents) == 0 {
		return nil, nil
	}

	type targetState struct {
		hashes  map[string][]string
		details map[string][]mcpConflictAgentRef
	}

	targets := make(map[string]*targetState)
	var issues []string

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, agent, lookPath)
		if err != nil {
			issues = append(issues, fmt.Sprintf("agent %q: %v", agent.QualifiedName(), err))
			continue
		}
		// Mirror buildStage1MCPTargetsWith's own filters exactly (same
		// delivery stage, same "nothing to project" test with && not
		// ||), so this check can never pass green on a city gc start is
		// about to reject. Before this fix the looser `||` here dropped
		// a provider's zero-server implicit agent out of the
		// comparison entirely whenever ANY of catalog/provider/target
		// was empty, so a real conflict against a non-implicit agent's
		// non-empty catalog at the same target never registered a
		// second hash group (gastownhall/gascity#8267).
		if view.Delivery != "stage1" {
			continue
		}
		if view.Projection.Provider == "" && len(view.Catalog.Servers) == 0 {
			continue
		}
		if strings.TrimSpace(view.Projection.Provider) == "" || strings.TrimSpace(view.Projection.Target) == "" {
			continue
		}
		key := view.Projection.Provider + "|" + filepath.Clean(view.Projection.Target)
		state := targets[key]
		if state == nil {
			state = &targetState{hashes: make(map[string][]string), details: make(map[string][]mcpConflictAgentRef)}
			targets[key] = state
		}
		hash := view.Projection.Hash()
		state.hashes[hash] = append(state.hashes[hash], agent.QualifiedName())
		state.details[hash] = append(state.details[hash], mcpConflictAgentRef{
			Name:     agent.QualifiedName(),
			Implicit: agent.Implicit,
			MCPDir:   agent.MCPDir,
		})
	}

	sort.Strings(issues)

	conflicts := make([]mcpTargetConflict, 0, len(targets))
	for key, state := range targets {
		if len(state.hashes) < 2 {
			continue
		}
		parts := strings.SplitN(key, "|", 2)
		agents := make([]string, 0, 4)
		var allDetails []mcpConflictAgentRef
		for _, members := range state.hashes {
			sort.Strings(members)
			agents = append(agents, members...)
		}
		for _, refs := range state.details {
			allDetails = append(allDetails, refs...)
		}
		sort.Strings(agents)
		conflict := mcpTargetConflict{
			Provider: parts[0],
			Target:   parts[1],
			Agents:   agents,
		}
		if dir, found := mcpAgentLocalConflictCause(allDetails, nil); found {
			conflict.AgentLocalDir = dir
		}
		conflicts = append(conflicts, conflict)
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Target != conflicts[j].Target {
			return conflicts[i].Target < conflicts[j].Target
		}
		return conflicts[i].Provider < conflicts[j].Provider
	})
	return issues, conflicts
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
