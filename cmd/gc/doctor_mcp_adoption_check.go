package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
)

// mcpAdoptionDoctorCheck reports provider-native MCP files whose own content
// gc's projection does not carry: files gc leaves alone because adopting them
// would drop it, and files gc adopted before it left such files alone, whose
// servers survive only in the adoption snapshot.
type mcpAdoptionDoctorCheck struct {
	cityPath string
	cfg      *config.City
	lookPath config.LookPathFunc
}

func newMCPAdoptionDoctorCheck(cityPath string, cfg *config.City, lookPath config.LookPathFunc) *mcpAdoptionDoctorCheck {
	return &mcpAdoptionDoctorCheck{cityPath: cityPath, cfg: cfg, lookPath: lookPath}
}

func (*mcpAdoptionDoctorCheck) Name() string                     { return "mcp-adoption" }
func (*mcpAdoptionDoctorCheck) CanFix() bool                     { return false }
func (*mcpAdoptionDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (*mcpAdoptionDoctorCheck) WarmupEligible() bool { return false }

func (c *mcpAdoptionDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	findings := inspectMCPAdoption(c.cityPath, c.cfg, c.lookPath)
	if len(findings) == 0 {
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "gc's MCP projection drops nothing from provider-native MCP files"}
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusWarning,
		Message: summarizeMCPIssues(findings),
		Details: findings,
		FixHint: "each finding says what to do; to hand a file gc adopted back, stop the city, restore the file from its " +
			"snapshot, and delete <root>/.gc/mcp-managed/<provider>.json, and gc leaves it alone from then on",
	}
}

// inspectMCPAdoption checks the MCP target of every configured agent, at a
// scope root (stage 1) or in its own work dir (stage 2), for content gc
// leaves alone or has dropped. It resolves work dirs without creating them
// and writes nothing. Agents whose MCP does not resolve are mcp-config's to
// report. Work dirs that exist only per session, such as a pool instance's,
// are not visited; resolveTemplate warns about those as their sessions start.
func inspectMCPAdoption(cityPath string, cfg *config.City, lookPath config.LookPathFunc) []string {
	if cfg == nil {
		return nil
	}
	type target struct {
		projection materialize.MCPProjection
		agents     []string
	}
	byKey := make(map[string]*target)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		view, err := resolveConfiguredAgentMCPProjectionWith(cityPath, cfg, agent, lookPath, true)
		if err != nil || (view.Delivery != "stage1" && view.Delivery != "stage2") || len(view.Projection.Servers) == 0 {
			continue
		}
		key := view.Projection.Provider + "|" + filepath.Clean(view.Projection.Target)
		if byKey[key] == nil {
			byKey[key] = &target{projection: view.Projection}
		}
		byKey[key].agents = append(byKey[key].agents, agent.QualifiedName())
	}
	var findings []string
	for _, key := range sortedMapKeys(byKey) {
		t := byKey[key]
		sort.Strings(t.agents)
		refused, err := t.projection.AdoptionRefusal(fsys.OSFS{})
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s (%s): %v", t.projection.Target, t.projection.Provider, err))
			continue
		}
		if refused != nil {
			findings = append(findings, describeMCPAdoptionRefused(t.agents, t.projection, refused))
			continue
		}
		dropped, dir, err := t.projection.DroppedAtAdoption(fsys.OSFS{})
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s (%s): %v", t.projection.Target, t.projection.Provider, err))
			continue
		}
		if len(dropped) == 0 {
			continue
		}
		quoted := make([]string, len(dropped))
		for i, name := range dropped {
			quoted[i] = fmt.Sprintf("%q", name)
		}
		findings = append(findings, fmt.Sprintf(
			"%s (%s): gc adopted this file and replaced MCP servers it held that gc does not project (%s); "+
				"their definitions are saved under %s. Define the ones you still need in gc's MCP catalog (an mcp/ directory); "+
				"delete those snapshots once you no longer need them to clear this warning",
			t.projection.Target, t.projection.Provider, strings.Join(quoted, ", "), dir))
	}
	return findings
}
