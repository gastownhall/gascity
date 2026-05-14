package config

import (
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
				"agents under agents/<name>/. Run `gc doctor` to inspect and `gc doctor --fix` for safe mechanical rewrites.",
			source))
	}
	if len(cfg.Packs) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: [packs] is deprecated in v2; use [imports] + packs.lock. "+
				"Run `gc doctor` to inspect and `gc doctor --fix` for safe mechanical rewrites.",
			source))
	}
	if len(cfg.Workspace.Includes) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: workspace.includes is deprecated in v2; use [imports]. "+
				"Run `gc doctor` to inspect and `gc doctor --fix` for safe mechanical rewrites.",
			source))
	}
	if len(cfg.Workspace.DefaultRigIncludes) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: workspace.default_rig_includes is deprecated in v2; use "+
				"root pack.toml [defaults.rig.imports.<binding>]. Run "+
				"`gc doctor` to inspect and `gc doctor --fix` for safe mechanical rewrites.",
			source))
	}
	return warnings
}
