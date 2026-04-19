package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// expandProbeCommandTemplate expands Go text/template placeholders (e.g.
// {{.Rig}}, {{.AgentBase}}) in a controller-side probe command such as an
// agent's scale_check or work_query. Rig-scoped pool agents rely on
// {{.Rig}} substitution so each rig's probe asks about its own bead
// routing. Before this helper, those commands were passed verbatim to
// sh, producing literal "{{.Rig}}" in argv.
//
// The expansion context mirrors the work_dir template surface — same
// PathContext fields (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName)
// — so anything users can reference in work_dir also works here.
//
// Malformed templates are logged to stderr and fall back to the raw
// string. This matches the graceful behavior of work_dir's ExpandTemplate
// without silently swallowing misconfiguration.
func expandProbeCommandTemplate(
	cityPath, cityName string,
	agentCfg *config.Agent,
	rigs []config.Rig,
	command string,
	stderr io.Writer,
) string {
	if agentCfg == nil || command == "" || !strings.Contains(command, "{{") {
		return command
	}
	ctx := workdirutil.PathContextForQualifiedName(cityPath, cityName, agentCfg.QualifiedName(), *agentCfg, rigs)
	expanded, err := workdirutil.ExpandTemplateStrict(command, ctx)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "expandProbeCommandTemplate: agent %q command %q: %v (using raw command)\n", agentCfg.QualifiedName(), command, err) //nolint:errcheck
		}
		return command
	}
	return expanded
}
