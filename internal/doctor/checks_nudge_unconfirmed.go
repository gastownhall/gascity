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

// LiveSessionNamesFunc returns the tmux session names of every session that is
// still live (not closed), which is also the directory name its diagnostics
// are written under.
type LiveSessionNamesFunc func() (map[string]struct{}, error)

// NudgeUnconfirmedOption configures a NudgeUnconfirmedCheck.
type NudgeUnconfirmedOption func(*NudgeUnconfirmedCheck)

// WithLiveSessionNames scopes the check to sessions that are still live. The
// diagnostic files are never cleared, so without this the warning also counts
// every session that ever wrote one -- a number that only grows as sessions
// close, whatever the current delivery health is. Diagnostics of sessions
// outside the returned set are reported as a skipped count, not as findings.
//
// If fn returns an error the check cannot tell live from closed, so it counts
// every session directory and says why in the details.
func WithLiveSessionNames(fn LiveSessionNamesFunc) NudgeUnconfirmedOption {
	return func(c *NudgeUnconfirmedCheck) {
		c.liveSessions = fn
	}
}

// NudgeUnconfirmedCheck surfaces sessions whose most recent nudge or
// startup-nudge submit could not be confirmed delivered. tmux's fallback
// nudge path has no busy-state indicator to confirm delivery against, so it
// cannot retry (retrying would duplicate every successful send); instead it
// writes a diagnostic file. This check is the reconciliation that reads that
// file back so the unconfirmed outcome reaches an operator instead of being
// silently dropped.
type NudgeUnconfirmedCheck struct {
	liveSessions LiveSessionNamesFunc
}

// NewNudgeUnconfirmedCheck creates a check for unconfirmed nudge deliveries.
// Without WithLiveSessionNames every session directory is counted.
func NewNudgeUnconfirmedCheck(opts ...NudgeUnconfirmedOption) *NudgeUnconfirmedCheck {
	c := &NudgeUnconfirmedCheck{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name returns the check identifier.
func (c *NudgeUnconfirmedCheck) Name() string { return "nudge-unconfirmed" }

// nudgeDiagnostic is one diagnostic file found under a session directory.
type nudgeDiagnostic struct {
	session  string
	filename string
}

// Run scans the city's runtime sessions directory for unconfirmed-nudge
// diagnostic files and warns when any belong to a live session.
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

	var found []nudgeDiagnostic
	var malformed []string
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
				found = append(found, nudgeDiagnostic{session: name, filename: filename})
			} else if !os.IsNotExist(statErr) {
				r.Status = StatusError
				r.Severity = SeverityAdvisory
				r.Message = fmt.Sprintf("cannot inspect session diagnostic %s: %v", path, statErr)
				return r
			}
		}
	}

	// The live set is only worth resolving when there is something to scope.
	var (
		liveErr error
		closed  map[string]struct{}
	)
	if len(found) > 0 && c.liveSessions != nil {
		var live map[string]struct{}
		if live, liveErr = c.liveSessions(); liveErr == nil {
			found, closed = splitLiveDiagnostics(found, live)
		}
	}

	var details []string
	sessions := make(map[string]struct{})
	for _, d := range found {
		details = append(details, fmt.Sprintf("session %q: %s", d.session, d.filename))
		sessions[d.session] = struct{}{}
	}

	if len(details) == 0 && len(malformed) == 0 {
		r.Status = StatusOK
		r.Message = "no unconfirmed nudge deliveries"
		if len(closed) > 0 {
			r.Message = fmt.Sprintf("no unconfirmed nudge deliveries for live sessions (%d closed session(s) skipped)", len(closed))
		}
		return r
	}

	sort.Strings(details)
	sort.Strings(malformed)
	for _, name := range malformed {
		details = append(details, fmt.Sprintf("unexpected non-directory entry %q under %s", name, sessionsDir))
	}
	if liveErr != nil {
		details = append(details, fmt.Sprintf("cannot list live sessions, so closed sessions are counted too: %v", liveErr))
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
	if len(closed) > 0 {
		r.Message += fmt.Sprintf("; %d closed session(s) skipped", len(closed))
	}
	r.Details = details
	return r
}

// splitLiveDiagnostics keeps the diagnostics of sessions in live and returns
// the distinct names of the sessions it dropped.
func splitLiveDiagnostics(found []nudgeDiagnostic, live map[string]struct{}) (kept []nudgeDiagnostic, closed map[string]struct{}) {
	closed = make(map[string]struct{})
	for _, d := range found {
		if _, ok := live[d.session]; ok {
			kept = append(kept, d)
			continue
		}
		closed[d.session] = struct{}{}
	}
	return kept, closed
}

// CanFix returns false because an unconfirmed delivery requires an operator
// to check whether the agent actually saw the nudge, not an automated fix.
func (c *NudgeUnconfirmedCheck) CanFix() bool { return false }

// Fix is a no-op; see CanFix.
func (c *NudgeUnconfirmedCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is on-demand only via `gc doctor`.
func (c *NudgeUnconfirmedCheck) WarmupEligible() bool { return false }
