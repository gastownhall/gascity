package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type fakeQuerier struct {
	b beads.Bead
}

func (f fakeQuerier) Get(_ string) (beads.Bead, error) {
	return f.b, nil
}

// TestAttachedHintSuppressedWhenIssueVarCarriesBead is the ga-tj5jbm RED
// test: the legacy sling path always auto-stamps gc.var.issue = beadID
// (BuildSlingFormulaVars), and every route-table formula resolves the bead
// back via `bd show $ROOT_ID` (Route B). The advisory must not fire when
// that route is live.
func TestAttachedHintSuppressedWhenIssueVarCarriesBead(t *testing.T) {
	const beadID = "ga-oaz41a"
	userVars := []string{"slug=" + beadID} // what deacon-dispatch.sh passes
	vars := BuildSlingFormulaVars("mol-plan-breakdown", beadID, userVars, config.Agent{}, SlingDeps{})
	if got := vars["issue"]; got != beadID {
		t.Fatalf("precondition: legacy path must stamp issue var; got %q", got)
	}
	q := fakeQuerier{b: beads.Bead{ID: beadID, Description: "instructions the agent needs"}}
	if hint := attachedBeadInstructionsDroppedHint(q, beadID, userVars); hint != "" {
		t.Fatalf("FALSE POSITIVE: hint fired even though gc.var.issue carries the bead:\n  %s", hint)
	}
}

// TestAttachedHintFiresWhenIssueVarNotStamped guards against deleting the
// hint outright: when the caller explicitly voids the issue var (so Route B
// genuinely isn't live) and supplies no context_path/requirements_path
// either, the bead's instructions really are invisible to the formula and
// the advisory should still fire.
func TestAttachedHintFiresWhenIssueVarNotStamped(t *testing.T) {
	const beadID = "ga-oaz41a"
	userVars := []string{"issue="}
	q := fakeQuerier{b: beads.Bead{ID: beadID, Description: "instructions the agent needs"}}
	hint := attachedBeadInstructionsDroppedHint(q, beadID, userVars)
	if hint == "" {
		t.Fatalf("hint should still fire when no route (issue var, context_path, requirements_path) carries the bead's description")
	}
}

// TestAttachedHintSuppressedWhenContextOrRequirementsPathSupplied preserves
// the pre-existing behavior: the caller-supplied context_path/
// requirements_path route (#3681) suppresses the hint regardless of the
// issue var's state.
func TestAttachedHintSuppressedWhenContextOrRequirementsPathSupplied(t *testing.T) {
	const beadID = "ga-oaz41a"
	q := fakeQuerier{b: beads.Bead{ID: beadID, Description: "instructions the agent needs"}}
	for _, userVars := range [][]string{
		{"issue=", "context_path=/tmp/ctx"},
		{"issue=", "requirements_path=/tmp/reqs.md"},
	} {
		if hint := attachedBeadInstructionsDroppedHint(q, beadID, userVars); hint != "" {
			t.Fatalf("hint should stay suppressed when context_path/requirements_path is supplied: vars=%v hint=%q", userVars, hint)
		}
	}
}
