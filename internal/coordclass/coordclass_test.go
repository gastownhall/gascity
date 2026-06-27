package coordclass

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestClassifyMatchesPolicyTaxonomy pins that Classify returns the same class
// string the pre-lift cmd/gc policyNameForBead returned for a per-class
// corpus. The want values are the literal policy-name strings; if the lift
// drifted, the strings would differ. The wisp-ROOT inheritance (a child of a
// wisp root inheriting the wisp class) is intentionally NOT covered here: it
// stays a store-Get in policyForCreate, outside the pure classifier.
func TestClassifyMatchesPolicyTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
		want Class
	}{
		{
			name: "wisp via metadata kind",
			bead: beads.Bead{Title: "wisp", Metadata: map[string]string{"gc.kind": "wisp"}},
			want: "wisp",
		},
		{
			name: "wisp via type",
			bead: beads.Bead{Title: "wisp", Type: "wisp"},
			want: "wisp",
		},
		{
			name: "wisp via gc:wisp label",
			bead: beads.Bead{Title: "wisp", Labels: []string{"gc:wisp"}},
			want: "wisp",
		},
		{
			name: "wisp via bare wisp label",
			bead: beads.Bead{Title: "wisp", Labels: []string{"wisp"}},
			want: "wisp",
		},
		{
			name: "order tracking via label",
			bead: beads.Bead{Title: "order:daily", Labels: []string{"order-run:daily", MarkerOrderTrackingLabel}},
			want: "order_tracking",
		},
		{
			name: "session via type and label",
			bead: beads.Bead{Title: "session", Type: session.BeadType, Labels: []string{session.LabelSession}},
			want: "session",
		},
		{
			name: "session via label only",
			bead: beads.Bead{Title: "session", Labels: []string{session.LabelSession}},
			want: "session",
		},
		{
			name: "session via type only",
			bead: beads.Bead{Title: "session", Type: session.BeadType},
			want: "session",
		},
		{
			name: "wait via label",
			bead: beads.Bead{Title: "wait", Type: session.WaitBeadType, Labels: []string{session.WaitBeadLabel}},
			want: "wait",
		},
		{
			name: "nudge via label",
			bead: beads.Bead{Title: "nudge", Type: "chore", Labels: []string{MarkerNudgeLabel}},
			want: "nudge",
		},
		{
			name: "workflow root",
			bead: beads.Bead{Title: "workflow", Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			}},
			want: "workflow",
		},
		{
			name: "workflow child via root_bead_id",
			bead: beads.Bead{Title: "workflow child", Metadata: map[string]string{
				"gc.root_bead_id": "bd-root",
			}},
			want: "workflow",
		},
		{
			name: "ordinary work",
			bead: beads.Bead{Title: "source work", Type: "task"},
			want: "",
		},
		{
			name: "bare bead with no markers",
			bead: beads.Bead{Title: "bare"},
			want: "",
		},
		{
			name: "wisp wins over order tracking (switch order)",
			bead: beads.Bead{
				Title:    "wisp+tracking",
				Metadata: map[string]string{"gc.kind": "wisp"},
				Labels:   []string{MarkerOrderTrackingLabel},
			},
			want: "wisp",
		},
		{
			name: "order tracking wins over session (switch order)",
			bead: beads.Bead{
				Title:  "tracking+session",
				Type:   session.BeadType,
				Labels: []string{MarkerOrderTrackingLabel, session.LabelSession},
			},
			want: "order_tracking",
		},
		{
			name: "session wins over nudge (switch order)",
			bead: beads.Bead{
				Title:  "session+nudge",
				Labels: []string{session.LabelSession, MarkerNudgeLabel},
			},
			want: "session",
		},
		{
			name: "session wins over workflow metadata (switch order)",
			bead: beads.Bead{
				Title:    "session+workflow",
				Type:     session.BeadType,
				Metadata: map[string]string{"gc.kind": "workflow"},
			},
			want: "session",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.bead); got != tc.want {
				t.Fatalf("Classify(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestClassifyGraphPlanMatchesPolicyTaxonomy pins ClassifyGraphPlan against
// the pre-lift policyNameForGraphPlan: wisp nodes win, then workflow nodes,
// else default work.
func TestClassifyGraphPlanMatchesPolicyTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		plan *beads.GraphApplyPlan
		want Class
	}{
		{
			name: "wisp node via metadata",
			plan: &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
				{Key: "root", Metadata: map[string]string{"gc.kind": "wisp"}},
			}},
			want: "wisp",
		},
		{
			name: "wisp node via gc:wisp label",
			plan: &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
				{Key: "root", Labels: []string{"gc:wisp"}},
			}},
			want: "wisp",
		},
		{
			name: "workflow node via metadata",
			plan: &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
				{Key: "root", Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}},
			}},
			want: "workflow",
		},
		{
			name: "workflow node via metadata refs",
			plan: &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
				{Key: "root", MetadataRefs: map[string]string{"gc.root_bead_id": "bd-root"}},
			}},
			want: "workflow",
		},
		{
			name: "wisp wins over workflow across nodes",
			plan: &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
				{Key: "work", Metadata: map[string]string{"gc.kind": "workflow"}},
				{Key: "wisp", Metadata: map[string]string{"gc.kind": "wisp"}},
			}},
			want: "wisp",
		},
		{
			name: "no markers is work",
			plan: &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
				{Key: "root", Title: "plain"},
			}},
			want: "",
		},
		{
			name: "empty plan is work",
			plan: &beads.GraphApplyPlan{},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyGraphPlan(tc.plan); got != tc.want {
				t.Fatalf("ClassifyGraphPlan(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
