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
		hashes map[string][]string
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
		if len(view.Catalog.Servers) == 0 || strings.TrimSpace(view.Projection.Provider) == "" || strings.TrimSpace(view.Projection.Target) == "" {
			continue
		}
		key := view.Projection.Provider + "|" + filepath.Clean(view.Projection.Target)
		state := targets[key]
		if state == nil {
			state = &targetState{hashes: make(map[string][]string)}
			targets[key] = state
		}
		hash := view.Projection.Hash()
		state.hashes[hash] = append(state.hashes[hash], agent.QualifiedName())
	}

	sort.Strings(issues)

	conflicts := make([]mcpTargetConflict, 0, len(targets))
	for key, state := range targets {
		if len(state.hashes) < 2 {
			continue
		}
		parts := strings.SplitN(key, "|", 2)
		agents := make([]string, 0, 4)
		for _, members := range state.hashes {
			sort.Strings(members)
			agents = append(agents, members...)
		}
		sort.Strings(agents)
		conflicts = append(conflicts, mcpTargetConflict{
			Provider: parts[0],
			Target:   parts[1],
			Agents:   agents,
		})
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
