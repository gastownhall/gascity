package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// nudgeUnconfirmedDiagnosticFiles are the diagnostic filenames written by
// internal/runtime/tmux when a nudge or startup-nudge submit could not be
// confirmed delivered (recordUnconfirmedSubmit / recordUnconfirmedNudge).
var nudgeUnconfirmedDiagnosticFiles = []string{
	"nudge-unconfirmed.log",
	"startup-nudge-unconfirmed.log",
}

// NudgeUnconfirmedCheck surfaces sessions whose most recent nudge or
// startup-nudge submit could not be confirmed delivered. tmux's fallback
// nudge path has no busy-state indicator to confirm delivery against, so it
// cannot retry (retrying would duplicate every successful send); instead it
// writes a diagnostic file. This check is the reconciliation that reads that
// file back so the unconfirmed outcome reaches an operator instead of being
// silently dropped.
type NudgeUnconfirmedCheck struct{}

// NewNudgeUnconfirmedCheck creates a check for unconfirmed nudge deliveries.
func NewNudgeUnconfirmedCheck() *NudgeUnconfirmedCheck {
	return &NudgeUnconfirmedCheck{}
}

// Name returns the check identifier.
func (c *NudgeUnconfirmedCheck) Name() string { return "nudge-unconfirmed" }

// Run scans the city's runtime sessions directory for unconfirmed-nudge
// diagnostic files and warns when any are present.
func (c *NudgeUnconfirmedCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	sessionsDir := filepath.Join(citylayout.RuntimeDataDir(ctx.CityPath), "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		r.Status = StatusOK
		r.Message = "no session runtime directory; nothing to check"
		return r
	}

	var details []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		for _, filename := range nudgeUnconfirmedDiagnosticFiles {
			path := filepath.Join(sessionsDir, name, filename)
			if _, statErr := os.Stat(path); statErr == nil {
				details = append(details, fmt.Sprintf("session %q: %s", name, filename))
			}
		}
	}

	if len(details) == 0 {
		r.Status = StatusOK
		r.Message = "no unconfirmed nudge deliveries"
		return r
	}

	sort.Strings(details)
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%d session(s) have an unconfirmed nudge delivery", len(details))
	r.Details = details
	return r
}

// CanFix returns false because an unconfirmed delivery requires an operator
// to check whether the agent actually saw the nudge, not an automated fix.
func (c *NudgeUnconfirmedCheck) CanFix() bool { return false }

// Fix is a no-op; see CanFix.
func (c *NudgeUnconfirmedCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is on-demand only via `gc doctor`.
func (c *NudgeUnconfirmedCheck) WarmupEligible() bool { return false }
