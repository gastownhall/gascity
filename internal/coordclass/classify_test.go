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
		// A convoy is a work bead, synthetic ones included. It holds `tracks`
		// edges to work members, and TrackItemIn refuses an edge across a class
		// boundary, so a synthetic convoy owned by any other class is a convoy
		// that can never track the members it was minted for.
		{"synthetic input convoy", beads.Bead{Type: "convoy", Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"}}, ClassWork},
		{"drain-unit convoy", beads.Bead{Type: "convoy", Metadata: map[string]string{beadmeta.SyntheticKindMetadataKey: "drain-unit-convoy"}}, ClassWork},

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

		// ---- MESSAGING (net-new arm: was work under policy store) ----
		{"mail message", beads.Bead{Type: "message"}, ClassMessaging},
		{"extmsg binding (type=task + label)", beads.Bead{Type: "task", Labels: []string{"gc:extmsg-binding"}}, ClassMessaging},
		{"extmsg transcript (type=task + label)", beads.Bead{Type: "task", Labels: []string{"gc:extmsg-transcript"}}, ClassMessaging},

		// ---- SESSIONS (session lifecycle + durable session waits) ----
		{"session bead by type", beads.Bead{Type: "session"}, ClassSessions},
		{"session bead by label", beads.Bead{Type: "task", Labels: []string{"gc:session"}}, ClassSessions},
		{"durable session wait bead", beads.Bead{Type: "gate", Labels: []string{"gc:wait"}}, ClassSessions},
		// Conversation operation journal rows live with the conversation rows
		// (critic C1). A stray gc.root_bead_id must not move one to the graph.
		{"op row open", beads.Bead{Type: "op", Status: "open", Labels: []string{"gc:op", "gc:op-conv:gc-7"}}, ClassSessions},
		{"op row closed", beads.Bead{Type: "op", Status: "closed", Labels: []string{"gc:op", "gc:op-conv:gc-7"}}, ClassSessions},
		{"fact row", beads.Bead{Type: "op", Status: "closed", Labels: []string{"gc:fact", "gc:fact-conv:gc-7"}}, ClassSessions},
		{"op row with stray root_bead_id", beads.Bead{Type: "op", Labels: []string{"gc:op"}, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-100"}}, ClassSessions},
		{"op label on another type", beads.Bead{Type: "task", Labels: []string{"gc:op"}}, ClassSessions},
		{"runtime row", beads.Bead{Type: "op", Status: "open", Labels: []string{"gc:rt", "gc:rt-conv:gc-7"}}, ClassSessions},
		{"runtime label on another type", beads.Bead{Type: "task", Labels: []string{"gc:rt"}}, ClassSessions},
		{"runtime conversation label alone stays work", beads.Bead{Type: "task", Labels: []string{"gc:rt-conv:gc-7"}}, ClassWork},
		{"conversation key label alone stays work", beads.Bead{Type: "task", Labels: []string{"gc:op-conv:gc-7"}}, ClassWork},
		// A real wait bead carries BOTH gc:wait and the per-entity session:<id>
		// label; it classifies via the gc:wait class signal.
		{"wait bead with per-entity session label", beads.Bead{Type: "gate", Labels: []string{"gc:wait", "session:gc-7"}}, ClassSessions},
		// Federation-correctness guard: the per-entity session:<id> label is NOT a
		// class signal. ListSessionWaits queries by Label="session:<id>", which a
		// route-by-query adapter would mis-route to the session store — but the
		// federating Router classifies by the class-level signal (gc:wait/gc:session/
		// type=session), so a bead carrying ONLY session:<id> stays ClassWork. This
		// is why sessions ride the federating Router, not a query-shape adapter.
		{"per-entity session label alone is NOT a session class signal", beads.Bead{Type: "task", Labels: []string{"session:gc-7"}}, ClassWork},

		// A startup-health episode is the session class's quarantine state
		// (session.SaveStartupHealthEpisode writes it through the sessions
		// store). It carries no session label, so before its own arm it fell
		// to the work default: on maintainer-city 35 episodes sat in the
		// binding answering to work, and a migration left every city's
		// episodes in the work store, silently resetting quarantine at cutover.
		{"startup-health episode", beads.Bead{Type: "startup-health-episode", Metadata: map[string]string{"session_name": "worker-1"}}, ClassSessions},
		{"startup-health episode with stray root_bead_id stays sessions", beads.Bead{Type: "startup-health-episode", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassSessions},
		// A durable session wait written without gc:wait (session.IsWaitBead):
		// a wait-typed bead whose session:<session_id> label names its own
		// session_id. The per-entity label on any OTHER shape is still not a
		// class signal (row above, and the two rows below).
		{"typed wait by its own session label", beads.Bead{Type: "gate", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}, ClassSessions},
		{"legacy-typed wait by its own session label", beads.Bead{Type: "wait", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}, ClassSessions},
		{"gate naming another session stays work", beads.Bead{Type: "gate", Labels: []string{"session:s-2"}, Metadata: map[string]string{"session_id": "s-1"}}, ClassWork},
		{"graph gate step with a session label stays graph", beads.Bead{Type: "gate", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1", beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassGraph},

		// ---- ORDERS (order-dispatch tracking) ----
		// An order's run label alone (no order-tracking, no graph marker) is
		// WORK: agents file real bugs, handoffs and tasks that inherit it from
		// the patrol that found them (150 such beads in production single-store
		// cities, e.g. or-l50geh, pl-vm0tw, tr-b6glw). Run records are matched
		// by order-tracking; order-poured roots are graph (rows below).
		{"order run label alone stays work", beads.Bead{Type: "task", Labels: []string{"order-run:rig/agent"}}, ClassWork},
		{"patrol-filed bug with an inherited order-run label stays work", beads.Bead{Type: "bug", Labels: []string{"order-run:technical-health-patrol", "technical-health"}}, ClassWork},
		{"order-poured workflow root stays graph", beads.Bead{Type: "molecule", Labels: []string{"order-run:rig/agent"}, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}, ClassGraph},
		{"order-poured wisp root stays graph", beads.Bead{Type: "molecule", Labels: []string{"order-run:rig/agent"}, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}}, ClassGraph},
		{"order-tracking bead", beads.Bead{Type: "task", Labels: []string{"order-run:rig/agent", "order-tracking"}, NoHistory: true}, ClassOrders},

		// ---- NUDGES (nudge-queue durability mirror + the durable queue itself) ----
		{"nudge shadow bead (type=chore + label)", beads.Bead{Type: "chore", Labels: []string{"gc:nudge"}}, ClassNudges},
		// The queue's own family. gc:nudge-queue is a DIFFERENT label from
		// gc:nudge, and the exact-match nudge arm alone routed it to work — a
		// bead the nudges class store physically holds, answering to work. See
		// labelNudgeQueue for the defect this row pins closed.
		{"nudge queue bead (type=chore + queue label)", beads.Bead{Type: "chore", Labels: []string{"gc:nudge-queue", "agent:worker-1"}}, ClassNudges},
		{"terminal nudge queue bead", beads.Bead{Type: "chore", Status: "closed", Labels: []string{"gc:nudge-queue"}, Metadata: map[string]string{"queue_state": "terminal"}}, ClassNudges},

		// ---- PRECEDENCE: order-tracking wins over the WISP arm ----
		// The wisp arm is checked first for everything EXCEPT an order-tracking
		// bead, and that exclusion is the one deliberate divergence from
		// policyNameForBead. orders.RunOutcomeWisp / WispFailed / WispCanceled
		// stamp the BARE "wisp" label on the tracking bead as an OUTCOME marker: it
		// says the run dispatched a wisp, not that the bead IS one. The dispatched
		// wisp is a separate bead and still classifies as graph.
		//
		// Before the exclusion these two rows returned ClassGraph, and the
		// consequence was live: ListTracking and RecentRunsAll read the orders leg
		// only, so on a class-split city a wisp-outcome run was invisible to them.
		{"order run with a wisp outcome classifies as orders", beads.Bead{Type: "task", Labels: []string{"order-run:rig/agent", "order-tracking", "wisp"}, NoHistory: true}, ClassOrders},
		{"order run with a wisp-failed outcome classifies as orders", beads.Bead{Type: "task", Labels: []string{"order-run:rig/agent", "order-tracking", "wisp", "wisp-failed"}, NoHistory: true}, ClassOrders},
		{"a real wisp bead is still graph", beads.Bead{Type: "task", Labels: []string{"gc:wisp"}}, ClassGraph},
		{"a bare-wisp bead without order-tracking is still graph", beads.Bead{Type: "task", Labels: []string{"wisp"}}, ClassGraph},
		{"order run with an exec outcome stays orders", beads.Bead{Type: "task", Labels: []string{"order-run:rig/agent", "order-tracking", "exec-failed"}, NoHistory: true}, ClassOrders},

		// ---- PRECEDENCE (tracker/session arms win over the broad workflow arm) ----
		{"order-tracking with stray root_bead_id stays orders", beads.Bead{Type: "task", Labels: []string{"order-tracking"}, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassOrders},
		{"session with stray root_bead_id stays sessions", beads.Bead{Type: "session", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassSessions},

		// ---- PRECEDENCE (the synthetic-convoy arm wins over the workflow arm) ----
		// A synthetic convoy is glue for one specific graph and carries that
		// graph's markers. If the workflow arm reached it first, the convoy would
		// answer to the graph class while its members stayed in work — and
		// TrackItemIn refuses that edge, so the convoy could never track them.
		// This is why the arm is explicit rather than left to the default.
		{"synthetic convoy with root_bead_id stays work", beads.Bead{Type: "convoy", Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true", beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassWork},
		{"drain-unit convoy with graph.v2 contract stays work", beads.Bead{Type: "convoy", Metadata: map[string]string{beadmeta.SyntheticKindMetadataKey: "drain-unit-convoy", beadmeta.FormulaContractMetadataKey: "graph.v2"}}, ClassWork},
		// The arm is scoped to convoys: a non-convoy bead carrying gc.synthetic
		// is not exempted from the workflow arm by it.
		{"non-convoy synthetic bead is unaffected by the convoy arm", beads.Bead{Type: "task", Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true", beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassGraph},
		// And the wisp arm still runs first, so no bead a wisp marker reaches can
		// be pulled into work by the convoy arm. This is what bounds the
		// reclassification for a converged city: the only beads whose class moves
		// are type=convoy beads carrying a synthetic marker, so an existing
		// city's wisp population — and everything the strand detector says about
		// it — is untouched.
		{"a wisp carrying a synthetic marker is still graph", beads.Bead{Type: "convoy", Labels: []string{"gc:wisp"}, Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"}}, ClassGraph},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.bead); got != tc.want {
				t.Errorf("Classify(%s) = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestClassifyAtLegacyDiffersOnlyInTheOwnedSessionArms pins the generation
// boundary. A copy manifest written by an earlier build is re-read with
// GenerationLegacy, so that generation must be today's classifier minus
// exactly the two GenerationOwnedSessions arms: one more difference, and an
// upgrade would strand (or hide) rows the earlier copy classified otherwise.
func TestClassifyAtLegacyDiffersOnlyInTheOwnedSessionArms(t *testing.T) {
	ownSessionWait := beads.Bead{Type: "gate", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}
	cases := []struct {
		name   string
		bead   beads.Bead
		legacy Class
	}{
		// The two arms GenerationOwnedSessions added.
		{"startup-health episode", beads.Bead{Type: "startup-health-episode", Metadata: map[string]string{"session_name": "worker-1"}}, ClassWork},
		{"startup-health episode with stray root_bead_id", beads.Bead{Type: "startup-health-episode", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassGraph},
		{"typed wait by its own session label", ownSessionWait, ClassWork},
		{"legacy-typed wait by its own session label", beads.Bead{Type: "wait", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}, ClassWork},
		// Every other arm answers the same in both generations.
		{"plain task", beads.Bead{Type: "task"}, ClassWork},
		{"session bead", beads.Bead{Type: "session"}, ClassSessions},
		{"wait with gc:wait", beads.Bead{Type: "gate", Labels: []string{"gc:wait", "session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}, ClassSessions},
		{"episode carrying gc:wait", beads.Bead{Type: "startup-health-episode", Labels: []string{"gc:wait"}}, ClassSessions},
		{"graph gate step with a session label", beads.Bead{Type: "gate", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1", beadmeta.RootBeadIDMetadataKey: "gc-9"}}, ClassGraph},
		{"gate naming another session", beads.Bead{Type: "gate", Labels: []string{"session:s-2"}, Metadata: map[string]string{"session_id": "s-1"}}, ClassWork},
		{"mail message", beads.Bead{Type: "message"}, ClassMessaging},
		{"op row", beads.Bead{Type: "op", Labels: []string{"gc:op"}}, ClassSessions},
		{"nudge queue bead", beads.Bead{Type: "chore", Labels: []string{"gc:nudge-queue"}}, ClassNudges},
		{"order-tracking bead", beads.Bead{Type: "task", Labels: []string{"order-tracking"}}, ClassOrders},
		{"wisp", beads.Bead{Type: "task", Labels: []string{"gc:wisp"}}, ClassGraph},
		{"convergence root", beads.Bead{Type: "convergence"}, ClassGraph},
		{"synthetic convoy", beads.Bead{Type: "convoy", Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"}}, ClassWork},
		{"order run label alone", beads.Bead{Type: "task", Labels: []string{"order-run:rig/agent"}}, ClassWork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAt(tc.bead, GenerationLegacy); got != tc.legacy {
				t.Errorf("ClassifyAt(%s, legacy) = %s, want %s", tc.name, got, tc.legacy)
			}
			if got, want := ClassifyAt(tc.bead, CurrentGeneration), Classify(tc.bead); got != want {
				t.Errorf("ClassifyAt(%s, current) = %s, want Classify's %s", tc.name, got, want)
			}
		})
	}
	if got := ClassifyAt(ownSessionWait, CurrentGeneration+1); got != ClassSessions {
		t.Errorf("a generation newer than this build's classified an own-session wait as %s, want the current generation's sessions", got)
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
	// graph.v2 step nodes carry their only gc.root_bead_id marker in
	// MetadataRefs (molecule/graph_apply.go writes it there, guarded by an
	// empty Metadata marker), not in Metadata. ClassifyGraphPlan must consult
	// MetadataRefs the same way the storage-tier policyNameForGraphPlan does,
	// otherwise such nodes are invisible to routing and the whole-plan result
	// depends on an unstated root-Metadata-marker invariant.
	t.Run("step node workflow marker in MetadataRefs routes the plan to graph", func(t *testing.T) {
		plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
			{Key: "root", Type: "molecule"},
			{Key: "s1", Type: "step", ParentKey: "root", MetadataRefs: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}},
		}}
		if got := ClassifyGraphPlan(plan); got != ClassGraph {
			t.Errorf("got %s, want graph (gc.root_bead_id in MetadataRefs must route to graph)", got)
		}
	})
	t.Run("realistic graph.v2 pour: workflow root in Metadata, steps refs-only", func(t *testing.T) {
		plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
			{Key: "root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
			{Key: "s1", Type: "step", ParentKey: "root", MetadataRefs: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}},
			{Key: "s2", Type: "step", ParentKey: "root", MetadataRefs: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}},
		}}
		if got := ClassifyGraphPlan(plan); got != ClassGraph {
			t.Errorf("got %s, want graph", got)
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
