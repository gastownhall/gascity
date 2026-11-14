package doctor

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// PoolAgentNudgeCheck warns when a pool-managed agent has no claim nudge.
//
// The pool execution backstop (cmd/gc/execution_backstop.go) re-delivers the
// agent's configured `nudge` to a pool seat that holds an in-progress claim
// but has gone quiet. When the template has no nudge there is nothing to
// deliver, so the backstop skips every probe attempt and goes straight to its
// terminal action after the 90-second grace: the seat is drained and its claim
// released, even if the agent was working. `nudge` is optional and pool
// templates routinely leave it unset, so this is the common shape, not the
// exotic one; the one-line fix is invisible until the drains are traced
// (gastownhall/gascity#6748, #5237).
type PoolAgentNudgeCheck struct {
	cfg *config.City
}

// NewPoolAgentNudgeCheck creates a check for pool agents whose sessions the
// execution backstop would drain unprobed.
func NewPoolAgentNudgeCheck(cfg *config.City) *PoolAgentNudgeCheck {
	return &PoolAgentNudgeCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *PoolAgentNudgeCheck) Name() string {
	return "pool-agent-missing-nudge"
}

// Run warns for every non-suspended pool-managed agent whose resolved Nudge is
// empty. Agents that only back a named session, and agents that cannot open a
// session at all (max_active_sessions = 0 with no pool minimum), are not pool
// seats and are skipped; so are implicit provider agents, which no config
// author wrote.
func (c *PoolAgentNudgeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no config; nothing to check"
		return r
	}

	namedTemplates := map[string]bool{}
	for _, ns := range c.cfg.NamedSessions {
		namedTemplates[ns.TemplateQualifiedName()] = true
	}

	var details []string
	for i := range c.cfg.Agents {
		a := &c.cfg.Agents[i]
		if a.Suspended || a.Implicit {
			continue
		}
		if !poolManagedAgent(a, namedTemplates) {
			continue
		}
		if strings.TrimSpace(a.Nudge) != "" {
			continue
		}
		details = append(details, fmt.Sprintf(
			"pool agent %s has no claim nudge: a quiet claim-holder is drained by the execution backstop after 90s with no probe (set `nudge` on the agent or its patch; see #6748)",
			a.QualifiedName(),
		))
	}

	if len(details) == 0 {
		r.Status = StatusOK
		r.Message = "every pool agent has a claim nudge"
		return r
	}
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%d pool agent(s) have no claim nudge", len(details))
	r.Details = details
	r.FixHint = "set `nudge = \"...\"` (e.g. the worker's `gc hook --claim` line) on each listed agent or its [[agent_patch]]; see gastownhall/gascity#6748"
	return r
}

// poolManagedAgent reports whether a's sessions are pool-managed, i.e. opened
// as ephemeral pool seats that the pool backstops govern. It mirrors
// config.Agent.SupportsInstanceExpansion, which keeps pool semantics for every
// shape except max_active_sessions = 1 without min_active_sessions or
// scale_check; that singleton is a pool seat too unless a [[named_session]]
// claims the template, in which case it is the named-session flavor.
func poolManagedAgent(a *config.Agent, namedTemplates map[string]bool) bool {
	if a.SupportsInstanceExpansion() {
		return true
	}
	return a.UsesCanonicalSingletonPoolIdentity() && !namedTemplates[a.QualifiedName()]
}

// CanFix returns false because the nudge text is the operator's to write.
func (c *PoolAgentNudgeCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *PoolAgentNudgeCheck) Fix(_ *CheckContext) error { return nil }
