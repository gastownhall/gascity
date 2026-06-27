package coordclass

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestClassifyGoldenTable pins the work-vs-infrastructure boundary with one row
// per census taxon. It is BOTH a characterization of the current
// cmd/gc/bead_policy_store.go behavior (so the extraction is provably faithful)
// AND the explicit pin for the two net-new arms (messaging, synthetic convoy)
// that the policy classifier does not have today. Any change to the boundary
// must change a row here in the same diff — that is the point.
func TestClassifyGoldenTable(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
		want Class
	}{
		// ---- WORK (the real backlog; stays in bd) ----
		{"plain task", beads.Bead{Type: "task"}, ClassWork},
		{"empty type defaults work", beads.Bead{}, ClassWork},
		{"epic", beads.Bead{Type: "epic"}, ClassWork},
		{"bug", beads.Bead{Type: "bug"}, ClassWork},
		{"feature", beads.Bead{Type: "feature"}, ClassWork},
		{"merge-request", beads.Bead{Type: "merge-request"}, ClassWork},
		{"user convoy (not synthetic)", beads.Bead{Type: "convoy", Labels: []string{"owned"}}, ClassWork},
		{"spec doc bead", beads.Bead{Type: "spec"}, ClassWork},

		// ---- GRAPH (formula-v2 topology + control lane; the explosion) ----
		// Convergence roots carry no graph metadata, so this is a deliberate
		// net-new arm (policyNameForBead does NOT route them today): the
		// convergence engine's state folds in with the graph it pours.
		{"convergence root folds into graph", beads.Bead{Type: "convergence"}, ClassGraph},
		{"workflow root", beads.Bead{Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}, ClassGraph},
		{"wisp root via gc.kind", beads.Bead{Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}}, ClassGraph},
		{"wisp via label", beads.Bead{Type: "task", Labels: []string{"gc:wisp"}}, ClassGraph},
		{"graph.v2 contract bead", beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: "graph.v2"}}, ClassGraph},
		{"graph child by root_bead_id", beads.Bead{Type: "step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-100"}}, ClassGraph},
		{"control bead (fanout) by root_bead_id", beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: "fanout", beadmeta.RootBeadIDMetadataKey: "gc-100"}}, ClassGraph},
		{"embedded work-typed step stays with graph", beads.Bead{Type: "bug", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-100"}}, ClassGraph},
		{"synthetic input convoy", beads.Bead{Type: "convoy", Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"}}, ClassGraph},
		{"drain-unit convoy", beads.Bead{Type: "convoy", Metadata: map[string]string{beadmeta.SyntheticKindMetadataKey: "drain-unit-convoy"}}, ClassGraph},

		// ---- MESSAGING (net-new arm: was work under policy store) ----
		{"mail message", beads.Bead{Type: "message"}, ClassMessaging},
		{"extmsg binding (type=task + label)", beads.Bead{Type: "task", Labels: []string{"gc:extmsg-binding"}}, ClassMessaging},
		{"extmsg transcript (type=task + label)", beads.Bead{Type: "task", Labels: []string{"gc:extmsg-transcript"}}, ClassMessaging},

		// ---- SESSIONS (session lifecycle + durable session waits) ----
		{"session bead by type", beads.Bead{Type: "session"}, ClassSessions},
		{"session bead by label", beads.Bead{Type: "task", Labels: []string{"gc:session"}}, ClassSessions},
		{"durable session wait bead", beads.Bead{Type: "gate", Labels: []string{"gc:wait"}}, ClassSessions},
		// A real wait bead carries BOTH gc:wait and the per-entity session:<id>
		// label; it classifies via the gc:wait class signal.
		{"wait bead with per-entity session label", beads.Bead{Type: "gate", Labels: []string{"gc:wait", "session:gc-7"}}, ClassSessions},
		// Federation-correctness guard: the per-entity session:<id> label is NOT a
		// class signal. ListSessionWaitBeads queries by Label="session:<id>", which a
		// route-by-query adapter would mis-route to the session store — but the
		// federating Router classifies by the class-level signal (gc:wait/gc:session/
		// type=session), so a bead carrying ONLY session:<id> stays ClassWork. This
		// is why sessions ride the federating Router, not a query-shape adapter.
		{"per-entity session label alone is NOT a session class signal", beads.Bead{Type: "task", Labels: []string{"session:gc-7"}}, ClassWork},

		// ---- ORDERS (order-dispatch tracking) ----
		{"order-tracking bead", beads.Bead{Type: "task", Labels: []string{"order-run:rig/agent", "order-tracking"}, NoHistory: true}, ClassOrders},

		// ---- NUDGES (nudge-queue durability mirror) ----
		{"nudge bead (type=chore + label)", beads.Bead{Type: "chore", Labels: []string{"gc:nudge"}}, ClassNudges},

		// ---- PRECEDENCE (tracker/session arms win over the broad workflow arm) ----
		{"order-tracking with stray root_bead_id stays orders", beads.Bead{Type: "task", Labels: []string{"order-tracking"}, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassOrders},
		{"session with stray root_bead_id stays sessions", beads.Bead{Type: "session", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassSessions},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.bead); got != tc.want {
				t.Errorf("Classify(%s) = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

func TestClassifyGraphPlan(t *testing.T) {
	t.Run("nil plan is work", func(t *testing.T) {
		if got := ClassifyGraphPlan(nil); got != ClassWork {
			t.Errorf("got %s, want work", got)
		}
	})
	t.Run("empty plan is work", func(t *testing.T) {
		if got := ClassifyGraphPlan(&beads.GraphApplyPlan{}); got != ClassWork {
			t.Errorf("got %s, want work", got)
		}
	})
	t.Run("plan with a workflow root is graph", func(t *testing.T) {
		plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
			{Key: "root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
			{Key: "s1", Type: "step", ParentKey: "root", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}},
		}}
		if got := ClassifyGraphPlan(plan); got != ClassGraph {
			t.Errorf("got %s, want graph", got)
		}
	})
	t.Run("plan routed wholesale even with embedded work-typed node", func(t *testing.T) {
		plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
			{Key: "root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
			{Key: "s1", Type: "bug", ParentKey: "root", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}},
		}}
		if got := ClassifyGraphPlan(plan); got != ClassGraph {
			t.Errorf("got %s, want graph (embedded bug step must not split the plan)", got)
		}
	})
}

func TestClassStringStable(t *testing.T) {
	want := map[Class]string{
		ClassWork:      "work",
		ClassGraph:     "graph",
		ClassMessaging: "messaging",
		ClassSessions:  "sessions",
		ClassOrders:    "orders",
		ClassNudges:    "nudges",
	}
	for c, name := range want {
		if c.String() != name {
			t.Errorf("Class(%d).String() = %q, want %q", c, c.String(), name)
		}
		if c.IsInfrastructure() != (c != ClassWork) {
			t.Errorf("%s.IsInfrastructure() wrong", name)
		}
	}
	if len(Classes()) != len(want) {
		t.Errorf("Classes() has %d entries, want %d", len(Classes()), len(want))
	}
}
