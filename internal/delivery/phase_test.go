package delivery_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/delivery"
)

func TestPhaseConstants(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"PhaseBuilding", delivery.PhaseBuilding},
		{"PhaseCIPending", delivery.PhaseCIPending},
		{"PhaseReviewPending", delivery.PhaseReviewPending},
		{"PhaseRework", delivery.PhaseRework},
		{"PhaseDecisionPending", delivery.PhaseDecisionPending},
		{"PhaseMergePending", delivery.PhaseMergePending},
		{"PhaseConflicted", delivery.PhaseConflicted},
		{"PhaseMerged", delivery.PhaseMerged},
		{"PhaseAbandoned", delivery.PhaseAbandoned},
	}

	expected := map[string]string{
		"PhaseBuilding":        "building",
		"PhaseCIPending":       "ci-pending",
		"PhaseReviewPending":   "review-pending",
		"PhaseRework":          "rework",
		"PhaseDecisionPending": "decision-pending",
		"PhaseMergePending":    "merge-pending",
		"PhaseConflicted":      "conflicted",
		"PhaseMerged":          "merged",
		"PhaseAbandoned":       "abandoned",
	}

	for _, c := range cases {
		if c.value != expected[c.name] {
			t.Errorf("constant %s: got %q, want %q", c.name, c.value, expected[c.name])
		}
	}
}

func TestMetaKeyConstants(t *testing.T) {
	if delivery.MetaKeyPhase != "gc.phase" {
		t.Errorf("MetaKeyPhase: got %q, want %q", delivery.MetaKeyPhase, "gc.phase")
	}
	if delivery.MetaKeyPhaseHistory != "gc.phase_history" {
		t.Errorf("MetaKeyPhaseHistory: got %q, want %q", delivery.MetaKeyPhaseHistory, "gc.phase_history")
	}
	if delivery.MetaKeyPRURL != "gc.pr_url" {
		t.Errorf("MetaKeyPRURL: got %q, want %q", delivery.MetaKeyPRURL, "gc.pr_url")
	}
}

func TestIsTerminalPhase(t *testing.T) {
	terminal := []string{delivery.PhaseMerged, delivery.PhaseAbandoned}
	nonTerminal := []string{
		delivery.PhaseBuilding,
		delivery.PhaseCIPending,
		delivery.PhaseReviewPending,
		delivery.PhaseRework,
		delivery.PhaseDecisionPending,
		delivery.PhaseMergePending,
		delivery.PhaseConflicted,
	}

	for _, p := range terminal {
		if !delivery.IsTerminalPhase(p) {
			t.Errorf("IsTerminalPhase(%q): got false, want true", p)
		}
	}
	for _, p := range nonTerminal {
		if delivery.IsTerminalPhase(p) {
			t.Errorf("IsTerminalPhase(%q): got true, want false", p)
		}
	}
	// empty string is not terminal
	if delivery.IsTerminalPhase("") {
		t.Error("IsTerminalPhase empty string: got true, want false")
	}
}
