package config

import (
	"fmt"
	"strings"
)

// legacyWorkspaceFieldMarkers are stable substrings that uniquely identify
// each warning produced by DetectLegacyWorkspaceFields. Callers that enforce
// strict warning policies use them to keep these soft-deprecation warnings
// non-fatal.
var legacyWorkspaceFieldMarkers = []string{
	"workspace.provider is deprecated",
	"workspace.start_command is deprecated",
	"workspace.suspended is deprecated",
	"workspace.install_agent_hooks is deprecated",
	"workspace.global_fragments is deprecated",
}

// IsLegacyWorkspaceFieldWarning reports whether warning is one of the
// soft-deprecation warnings emitted by DetectLegacyWorkspaceFields.
func IsLegacyWorkspaceFieldWarning(warning string) bool {
	for _, marker := range legacyWorkspaceFieldMarkers {
		if strings.Contains(warning, marker) {
			return true
		}
	}
	return false
}

// DetectLegacyWorkspaceFields emits one soft-deprecation warning per
// populated v1 [workspace] sub-field that has a v.next replacement.
// Per docs/packv2/skew-analysis.md, these surfaces will be removed
// from [workspace] in a future release. Each warning starts with
// "<source>: workspace.<field> is deprecated:" and includes the
// suggested replacement.
//
// This function runs alongside ValidateSemantics during config load
// and contributes to Provenance.Warnings. Output ordering matches the
// declaration order below so warning text is stable across runs.
//
// Detection rules per field:
//   - workspace.provider: warn when non-empty.
//   - workspace.start_command: warn when non-empty.
//   - workspace.suspended: warn when true (the false zero-value is
//     indistinguishable from unset).
//   - workspace.install_agent_hooks: warn when non-empty.
//   - workspace.global_fragments: warn when non-empty.
func DetectLegacyWorkspaceFields(cfg *City, source string) []string {
	return detectLegacyWorkspaceFields(cfg, source, nil)
}

func detectLegacyWorkspaceFields(cfg *City, defaultSource string, workspaceSources map[string]string) []string {
	if cfg == nil {
		return nil
	}
	ws := cfg.Workspace

	var warnings []string
	emit := func(field, suggestion string) {
		source := defaultSource
		if workspaceSources != nil {
			if fieldSource := workspaceSources[field]; fieldSource != "" {
				source = fieldSource
			}
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s: workspace.%s is deprecated: %s",
			source, field, suggestion,
		))
	}

	if ws.Provider != "" {
		emit("provider", "Set provider per agent in agents/<name>/agent.toml.")
	}
	if ws.StartCommand != "" {
		emit("start_command", "Use per-agent `start_command` in `agent.toml` instead.")
	}
	if ws.Suspended {
		emit("suspended", "This will move to `.gc/site.toml` in a future release. No action is required now.")
	}
	if len(ws.InstallAgentHooks) > 0 {
		emit("install_agent_hooks", "Set install_agent_hooks per agent in agents/<name>/agent.toml.")
	}
	if len(ws.GlobalFragments) > 0 {
		emit("global_fragments", "Use `[agent_defaults] append_fragments` or explicit `{{ template }}` instead.")
	}
	return warnings
}
