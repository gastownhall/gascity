package config

import (
	"fmt"
	"strings"
)

type legacyWorkspaceFieldRule struct {
	field      string
	suggestion string
	defined    func(Workspace, map[string]string) bool
}

var legacyWorkspaceFieldRules = []legacyWorkspaceFieldRule{
	{
		field:      "provider",
		suggestion: "Set provider per agent in agents/<name>/agent.toml.",
		defined: func(ws Workspace, _ map[string]string) bool {
			return ws.Provider != ""
		},
	},
	{
		field:      "start_command",
		suggestion: "Use per-agent `start_command` in `agent.toml` instead.",
		defined: func(ws Workspace, _ map[string]string) bool {
			return ws.StartCommand != ""
		},
	},
	{
		field:      "suspended",
		suggestion: "This will move to `.gc/site.toml` in a future release. No action is required now.",
		defined: func(ws Workspace, workspaceSources map[string]string) bool {
			return ws.Suspended || workspaceFieldDefined(workspaceSources, "suspended")
		},
	},
	{
		field:      "install_agent_hooks",
		suggestion: "Set install_agent_hooks per agent in agents/<name>/agent.toml.",
		defined: func(ws Workspace, _ map[string]string) bool {
			return len(ws.InstallAgentHooks) > 0
		},
	},
	{
		field:      "global_fragments",
		suggestion: "Use `[agent_defaults] append_fragments` or explicit `{{ template }}` instead.",
		defined: func(ws Workspace, _ map[string]string) bool {
			return len(ws.GlobalFragments) > 0
		},
	},
}

// IsLegacyWorkspaceFieldWarning reports whether warning is one of the
// soft-deprecation warnings emitted by DetectLegacyWorkspaceFields.
func IsLegacyWorkspaceFieldWarning(warning string) bool {
	for _, rule := range legacyWorkspaceFieldRules {
		if strings.Contains(warning, legacyWorkspaceFieldMarker(rule.field)) {
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
//   - workspace.suspended: warn when true, or when load provenance shows the
//     field was explicitly defined.
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

	for _, rule := range legacyWorkspaceFieldRules {
		if rule.defined(ws, workspaceSources) {
			emit(rule.field, rule.suggestion)
		}
	}
	return warnings
}

func legacyWorkspaceFieldMarker(field string) string {
	return "workspace." + field + " is deprecated"
}

func workspaceFieldDefined(workspaceSources map[string]string, field string) bool {
	if workspaceSources == nil {
		return false
	}
	return workspaceSources[field] != ""
}
