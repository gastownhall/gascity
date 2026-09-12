package doctor

import (
	"fmt"
	"os"
	"sort"
)

// SkillMissingSinkCandidate is one live session whose own skill sink may
// be invisible to its provider CLI: the session's WorkDir differs from the
// originating agent's scope root (a per-bead worktree, typically), so the
// scope-root materializer pass never reached it.
type SkillMissingSinkCandidate struct {
	SessionName string
	SessionSink string // WorkDir × vendor sink the seat's CLI actually reads
	ScopeSink   string // the originating agent's scope-root × vendor sink
}

// SkillMissingSinkCheck flags a live session's WorkDir that carries no
// materialized skill sink at all, even though the agent it was started
// from has skills materialized at its scope root. A provider whose CLI
// reads a cwd-relative sink (.claude/skills, .agents/skills) sees none of
// the pack/role skills once work moves into such a worktree — a real
// capability gap between that seat and one still running at the scope
// root (cr-mdi6h).
type SkillMissingSinkCheck struct {
	candidatesFn func() []SkillMissingSinkCandidate
}

// NewSkillMissingSinkCheck builds a check over the given lazy candidate
// enumerator. candidatesFn is evaluated inside Run, matching the sibling
// SkillDanglingSinkCheck's laziness: store-backed session enumeration
// never slows doctor check construction or a run that never reaches it.
func NewSkillMissingSinkCheck(candidatesFn func() []SkillMissingSinkCandidate) *SkillMissingSinkCheck {
	return &SkillMissingSinkCheck{candidatesFn: candidatesFn}
}

// Name returns the check identifier.
func (c *SkillMissingSinkCheck) Name() string { return "skill-missing-sink" }

// sinkHasEntries reports whether dir exists and contains at least one
// entry. A missing scope sink (no entries) means the agent has nothing to
// deliver yet — not a defect the session-side sink can be blamed for.
func sinkHasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// Run reports a warning for every candidate whose scope-root sink has
// materialized skills but whose own session sink is empty or absent.
func (c *SkillMissingSinkCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.candidatesFn == nil {
		r.Status = StatusOK
		r.Message = "no live sessions to check"
		return r
	}
	var missing []SkillMissingSinkCandidate
	for _, cand := range c.candidatesFn() {
		if cand.ScopeSink == "" || !sinkHasEntries(cand.ScopeSink) {
			continue
		}
		if sinkHasEntries(cand.SessionSink) {
			continue
		}
		missing = append(missing, cand)
	}
	if len(missing) == 0 {
		r.Status = StatusOK
		r.Message = "every live session's skill sink is materialized"
		return r
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].SessionName < missing[j].SessionName })
	details := make([]string, 0, len(missing))
	for _, m := range missing {
		details = append(details, fmt.Sprintf("%s: %s has no materialized skills (agent sink: %s)", m.SessionName, m.SessionSink, m.ScopeSink))
	}
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%d live session(s) cannot see their agent's materialized skills", len(missing))
	r.Details = details
	r.FixHint = "run `gc internal materialize-skills --agent <agent> --workdir <dir>` for each session, or route worktree creation through a step that materializes into the worktree"
	return r
}

// CanFix returns false — the fix requires per-session agent identity this
// check does not itself resolve to a --fix-safe action.
func (c *SkillMissingSinkCheck) CanFix() bool { return false }

// Fix is never called: CanFix returns false.
func (c *SkillMissingSinkCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false — the candidate scan opens the session
// store, which the `gc start` warm-up path should not pay for.
func (c *SkillMissingSinkCheck) WarmupEligible() bool { return false }
