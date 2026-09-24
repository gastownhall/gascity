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

	// citylayout owns this path. Joining a literal onto a runtime helper here
	// is how this check spent its whole life scanning .gc/runtime/sessions while
	// the writers filled .gc/sessions.
	sessionsDir := citylayout.SessionDiagnosticsDir(ctx.CityPath)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			r.Status = StatusOK
			r.Message = "no session runtime directory; nothing to check"
			return r
		}
		r.Status = StatusError
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("cannot read session diagnostic directory %s: %v", sessionsDir, err)
		return r
	}

	var details []string
	var malformed []string
	sessions := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			// Reporting a non-directory child as clean is the same false-green
			// this check exists to close. Collect it and keep scanning: an
			// early return here would suppress the genuine unconfirmed-nudge
			// details for every remaining session.
			malformed = append(malformed, name)
			continue
		}
		for _, filename := range nudgeUnconfirmedDiagnosticFiles {
			path := filepath.Join(sessionsDir, name, filename)
			if _, statErr := os.Stat(path); statErr == nil {
				details = append(details, fmt.Sprintf("session %q: %s", name, filename))
				sessions[name] = struct{}{}
			} else if !os.IsNotExist(statErr) {
				r.Status = StatusError
				r.Severity = SeverityAdvisory
				r.Message = fmt.Sprintf("cannot inspect session diagnostic %s: %v", path, statErr)
				return r
			}
		}
	}

	if len(details) == 0 && len(malformed) == 0 {
		r.Status = StatusOK
		r.Message = "no unconfirmed nudge deliveries"
		return r
	}

	sort.Strings(details)
	sort.Strings(malformed)
	for _, name := range malformed {
		details = append(details, fmt.Sprintf("unexpected non-directory entry %q under %s", name, sessionsDir))
	}

	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	// A session holding both diagnostic files is one session, not two; the
	// details stay per-artifact.
	r.Message = fmt.Sprintf("%d session(s) have an unconfirmed nudge delivery", len(sessions))
	if len(malformed) > 0 {
		if len(sessions) == 0 {
			r.Message = fmt.Sprintf("%d unexpected non-directory entry(ies) under %s", len(malformed), sessionsDir)
		} else {
			r.Message += fmt.Sprintf("; %d unexpected non-directory entry(ies) under %s", len(malformed), sessionsDir)
		}
	}
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
