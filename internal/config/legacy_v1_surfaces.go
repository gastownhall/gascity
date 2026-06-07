package config

import (
	"errors"
	"fmt"
	"strings"
)

// legacyV1SurfaceMarkers are stable substrings that uniquely identify
// each warning produced by DetectLegacyV1Surfaces. Callers (e.g. the
// strict-mode collision filter) use them to recognize v1-surface
// migration guidance and keep it non-fatal.
var legacyV1SurfaceMarkers = []string{
	"[[agent]] tables are deprecated",
	"[packs] is deprecated",
	"workspace.includes is deprecated",
	"workspace.default_rig_includes is deprecated",
}

// IsLegacyV1SurfaceWarning reports whether warning is one of the loud
// deprecation warnings emitted by DetectLegacyV1Surfaces. These are
// migration guidance — informational, not collision/integrity errors —
// and must stay non-fatal under strict-mode reload checks.
func IsLegacyV1SurfaceWarning(warning string) bool {
	for _, m := range legacyV1SurfaceMarkers {
		if strings.Contains(warning, m) {
			return true
		}
	}
	return false
}

// DetectLegacyV1Surfaces emits one loud deprecation warning per top-level
// v1 surface that the supplied configuration still populates. It is meant
// to run on freshly-parsed schema-2 city config files BEFORE any pack
// expansion takes place — pack expansion legitimately injects agents (and
// may merge workspace.includes / default_rig_includes from pack.toml
// defaults) into the same fields, and we only want to warn about
// user-authored city-layer declarations.
//
// Calling this function on the post-merge config will produce false
// positives for cities that consume packs which themselves use [[agent]]
// internally. Callers that cannot inject the call before pack expansion
// must snapshot len(cfg.Agents) etc. on the as-parsed root and pass the
// snapshot in via a pre-expansion *City value.
//
// Stable ordering: agent → packs → workspace.includes →
// workspace.default_rig_includes. Each warning is prefixed with the
// provided source (typically the city.toml path) and points operators at
// gc doctor as the canonical migration surface.
func DetectLegacyV1Surfaces(cfg *City, source string) []string {
	if cfg == nil {
		return nil
	}
	var warnings []string
	if len(cfg.Agents) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: [[agent]] tables are deprecated in v2; use directory-based "+
				"agents under agents/<name>/. Run `gc doctor` to inspect; `gc doctor --fix` handles the safe mechanical rewrites available in this wave.",
			source))
	}
	if len(cfg.Packs) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: [packs] is deprecated in v2; use [imports] + packs.lock. "+
				"Run `gc doctor` to inspect; `gc doctor --fix` migrates entries referenced by legacy workspace include lists, then migrate or remove any remaining [packs] entries manually.",
			source))
	}
	// Direct raw-field access is intentional here: detection runs before pack
	// expansion, and the accessors are used by post-parse migration paths.
	if len(cfg.Workspace.Includes) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: workspace.includes is deprecated in v2; use [imports]. "+
				"Run `gc doctor` to inspect; `gc doctor --fix` handles the safe mechanical rewrites available in this wave.",
			source))
	}
	if len(cfg.Workspace.DefaultRigIncludes) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: workspace.default_rig_includes is deprecated in v2; use "+
				"city.toml [defaults.rig.imports.<binding>]. Run "+
				"`gc doctor` to inspect; `gc doctor --fix` handles the safe mechanical rewrites available in this wave.",
			source))
	}
	return warnings
}

// LegacyV1SurfaceErrors returns hard-error diagnostics for legacy PackV1
// surfaces that are no longer supported in Wave 2 enforcement paths.
//
// This intentionally does not replace DetectLegacyV1Surfaces: callers like
// doctor and strict-warning filters still need stable warning strings while
// the broader remediation messaging stays aligned. Load paths that are ready
// to enforce should call LegacyV1SurfaceError instead.
func LegacyV1SurfaceErrors(cfg *City, source string, data ...[]byte) []string {
	if cfg == nil {
		return nil
	}

	locator := optionalConfigDiagnosticLocator(data)
	var errors []string
	if len(cfg.Agents) > 0 {
		errors = append(errors, LegacyInlineAgentSurfaceErrors(cfg, source, data...)...)
	}
	if len(cfg.Packs) > 0 {
		errors = append(errors, fmt.Sprintf(
			"%s: unsupported PackV1 [packs] entries; replace them with [imports] and regenerate packs.lock",
			sourceWithDiagnosticLine(source, locator.lineForPacksTable())))
	}
	if len(cfg.Workspace.Includes) > 0 {
		errors = append(errors, fmt.Sprintf(
			"%s: unsupported PackV1 workspace.includes; replace it with [imports.<binding>] entries",
			sourceWithDiagnosticLine(source, locator.lineForKey("workspace", "includes"))))
	}
	if len(cfg.Workspace.DefaultRigIncludes) > 0 {
		errors = append(errors, fmt.Sprintf(
			"%s: unsupported PackV1 workspace.default_rig_includes; move defaults into city.toml [defaults.rig.imports.<binding>]",
			sourceWithDiagnosticLine(source, locator.lineForKey("workspace", "default_rig_includes"))))
	}
	return errors
}

// LegacyV1SurfaceError aggregates legacy PackV1 surface violations into one
// load-time error for Wave 2 enforcement paths.
func LegacyV1SurfaceError(cfg *City, source string, data ...[]byte) error {
	violations := LegacyV1SurfaceErrors(cfg, source, data...)
	return configSurfaceError("PackV1 config surfaces are no longer supported", violations)
}

type fragmentLegacyV1SurfaceError struct {
	include string
	err     error
}

func (e *fragmentLegacyV1SurfaceError) Error() string {
	return fmt.Sprintf("fragment %q: %v", e.include, e.err)
}

func (e *fragmentLegacyV1SurfaceError) Unwrap() error {
	return e.err
}

// IsFragmentLegacyV1SurfaceError reports whether err came from a legacy PackV1
// surface authored in an included fragment rather than root city.toml/pack.toml.
func IsFragmentLegacyV1SurfaceError(err error) bool {
	var target *fragmentLegacyV1SurfaceError
	return errors.As(err, &target)
}

// LegacyInlineAgentSurfaceErrors returns hard-error diagnostics for inline
// [[agent]] tables. Unlike other fragment-level legacy surfaces, inline agents
// have a direct portable replacement and do not require machine-local state.
func LegacyInlineAgentSurfaceErrors(cfg *City, source string, data ...[]byte) []string {
	if cfg == nil || len(cfg.Agents) == 0 {
		return nil
	}
	locator := optionalConfigDiagnosticLocator(data)
	return []string{fmt.Sprintf(
		"%s: unsupported PackV1 [[agent]] tables; move each agent to agents/<name>/agent.toml",
		sourceWithDiagnosticLine(source, locator.lineForTable("agent")))}
}

// LegacyInlineAgentSurfaceError aggregates unsupported inline [[agent]]
// surfaces into one load-time error for schema=2 enforcement paths.
func LegacyInlineAgentSurfaceError(cfg *City, source string, data ...[]byte) error {
	violations := LegacyInlineAgentSurfaceErrors(cfg, source, data...)
	return configSurfaceError("PackV1 inline agent tables are no longer supported", violations)
}
