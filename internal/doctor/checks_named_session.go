package doctor

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
)

// NamedAlwaysMinConflictCheck warns when an agent both backs a mode="always"
// named session and sets min_active_sessions > 0.
type NamedAlwaysMinConflictCheck struct {
	cfg *config.City
}

// NewNamedAlwaysMinConflictCheck creates a check for duplicate pool session
// footguns caused by named sessions and pool minimums.
func NewNamedAlwaysMinConflictCheck(cfg *config.City) *NamedAlwaysMinConflictCheck {
	return &NamedAlwaysMinConflictCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *NamedAlwaysMinConflictCheck) Name() string {
	return "named-always-min-conflict"
}

// Run warns when any non-suspended agent backs a mode="always" named session
// and has min_active_sessions > 0.
func (c *NamedAlwaysMinConflictCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no config; nothing to check"
		return r
	}

	alwaysSessions := map[string]string{}
	for _, ns := range c.cfg.NamedSessions {
		if ns.ModeOrDefault() == "always" {
			alwaysSessions[ns.TemplateQualifiedName()] = ns.QualifiedName()
		}
	}
	if len(alwaysSessions) == 0 {
		r.Status = StatusOK
		r.Message = "no mode=always named sessions"
		return r
	}

	var details []string
	for i := range c.cfg.Agents {
		a := &c.cfg.Agents[i]
		if a.Suspended {
			continue
		}
		minimum := a.EffectiveMinActiveSessions()
		if minimum <= 0 {
			continue
		}
		nsName, ok := alwaysSessions[a.QualifiedName()]
		if !ok {
			continue
		}
		details = append(details, fmt.Sprintf(
			"agent %q: min_active_sessions=%d with named_session %q mode=always; set min_active_sessions=0 to avoid a duplicate pool session",
			a.QualifiedName(), minimum, nsName,
		))
	}

	if len(details) == 0 {
		r.Status = StatusOK
		r.Message = "no named-always / min_active_sessions conflicts"
		return r
	}
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%d agent(s) combine mode=always with min_active_sessions>0", len(details))
	r.Details = details
	return r
}

// CanFix returns false because config authors must decide whether the
// duplicate pool session is intentional.
func (c *NamedAlwaysMinConflictCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *NamedAlwaysMinConflictCheck) Fix(_ *CheckContext) error { return nil }
