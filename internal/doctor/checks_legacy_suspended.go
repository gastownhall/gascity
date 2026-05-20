package doctor

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
)

// LegacySuspendedFieldCheck warns when city.toml carries the
// deprecated `suspended` boolean on `[workspace]` or any `[[rig]]`
// entry. Suspension state is no longer read from those fields: it
// lives in .gc/runtime/city-state.json and rig-state.json (explicit
// preference) and in `suspended_on_start` (committable default at
// start). Surfacing the legacy field lets users migrate at their
// own pace without behavioral surprises.
type LegacySuspendedFieldCheck struct {
	cfg *config.City
}

// NewLegacySuspendedFieldCheck creates a check that flags legacy
// `suspended` fields in city.toml.
func NewLegacySuspendedFieldCheck(cfg *config.City) *LegacySuspendedFieldCheck {
	return &LegacySuspendedFieldCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *LegacySuspendedFieldCheck) Name() string { return "legacy-suspended-field" }

// Run inspects [workspace] and each [[rig]] for the deprecated
// `suspended` field. Returns a warning when any is set.
func (c *LegacySuspendedFieldCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no city config loaded"
		return r
	}

	var issues []string
	if c.cfg.Workspace.Suspended {
		issues = append(issues, `[workspace] suspended = true is deprecated; if you meant "start the city suspended by default", rename it to suspended_on_start. Otherwise remove it — runtime city suspension now lives in .gc/runtime/city-state.json (managed by "gc suspend" / "gc resume").`)
	}
	for i := range c.cfg.Rigs {
		r := &c.cfg.Rigs[i]
		if !r.Suspended {
			continue
		}
		issues = append(issues, fmt.Sprintf(
			`[[rig]] %q suspended = true is deprecated; if you meant "start this rig suspended by default", rename it to suspended_on_start. Otherwise remove it — runtime rig suspension now lives in .gc/runtime/rig-state.json (managed by "gc rig suspend" / "gc rig resume").`,
			r.Name,
		))
	}

	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "no deprecated suspended fields in city.toml"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d deprecated suspended field(s) found in city.toml", len(issues))
	r.Details = issues
	r.FixHint = "rename `suspended` to `suspended_on_start` to preserve the committable default, or remove it to defer entirely to runtime state"
	return r
}

// CanFix reports whether automatic remediation is supported.
// Migration is intentionally manual: only the user can say whether
// a legacy `suspended = true` was meant as the committed default
// (rename to `suspended_on_start`) or as a transient runtime
// preference (remove from city.toml and re-run `gc suspend` /
// `gc rig suspend` to record it in runtime state).
func (c *LegacySuspendedFieldCheck) CanFix() bool { return false }

// Fix is a no-op; see [CanFix].
func (c *LegacySuspendedFieldCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible reports whether this check runs during `gc start`'s
// warm-up scan. It does — the warning is most useful right when the
// user is about to act on a stale-config view.
func (c *LegacySuspendedFieldCheck) WarmupEligible() bool { return true }
