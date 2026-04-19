package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// expandAgentCommandTemplate expands Go text/template placeholders (e.g.
// {{.Rig}}, {{.AgentBase}}) in agent command strings such as scale_check,
// work_query, on_boot, on_death, and prompt-context command snippets. Rig-scoped
// agents rely on {{.Rig}} substitution so each path issues a rig-specific
// command instead of passing the literal template to sh.
//
// The expansion context mirrors work_dir's PathContext surface: Agent,
// AgentBase, Rig, RigRoot, CityRoot, and CityName.
//
// Malformed templates are logged to stderr and fall back to the raw string.
// This matches the graceful behavior of work_dir's ExpandTemplate without
// silently swallowing misconfiguration.
func expandAgentCommandTemplate(
	cityPath, cityName string,
	agentCfg *config.Agent,
	rigs []config.Rig,
	fieldName string,
	command string,
	stderr io.Writer,
) string {
	if agentCfg == nil || command == "" || !strings.Contains(command, "{{") {
		return command
	}
	expanded, err := workdirutil.ExpandCommandTemplate(command, cityPath, cityName, *agentCfg, rigs)
	if err != nil {
		if stderr != nil {
			if fieldName == "" {
				fieldName = "command"
			}
			fmt.Fprintf(stderr, "expandAgentCommandTemplate: agent %q field %q: %v (using raw command)\n", agentCfg.QualifiedName(), fieldName, err) //nolint:errcheck
		}
		return command
	}
	return expanded
}
