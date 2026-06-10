package state_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads/state"
)

// stubBead is the minimal BeadView implementation used by table tests.
type stubBead struct {
	id     string
	status string
	typ    string
	title  string
	labels []string
	meta   map[string]string
}

func (s stubBead) ID() string        { return s.id }
func (s stubBead) Status() string    { return s.status }
func (s stubBead) IssueType() string { return s.typ }
func (s stubBead) Title() string     { return s.title }
func (s stubBead) Labels() []string  { return s.labels }
func (s stubBead) Meta(key string) string {
	return s.meta[key]
}

// defaults fills in sensible zero values so test cases only need to set
// fields that matter.
func b(id, status, typ, title string, labels []string, meta map[string]string) stubBead {
	if status == "" {
		status = "open"
	}
	if typ == "" {
		typ = "task"
	}
	if title == "" {
		title = "t"
	}
	if id == "" {
		id = "x-1"
	}
	if meta == nil {
		meta = map[string]string{}
	}
	if labels == nil {
		labels = []string{}
	}
	return stubBead{id: id, status: status, typ: typ, title: title, labels: labels, meta: meta}
}

func TestClassify(t *testing.T) {
	ready := map[string]bool{"R": true}
	blocked := map[string]bool{"B": true}
	live := map[string]bool{"sess-live": true}
	liveRigs := map[string]bool{"rig": true} // "rig" is the prefix in gc.routed_to="rig/agent"

	cases := []struct {
		want state.EffectiveState
		bead stubBead
		// override sets; nil means use the shared vars above
		readyOverride    map[string]bool
		blockedOverride  map[string]bool
		liveRigsOverride map[string]bool
	}{
		// 1. terminal: status=closed
		{want: state.StateDone, bead: b("x-1", "closed", "", "", nil, nil)},
		// 2. terminal: delivery phase merged
		{want: state.StateDone, bead: b("", "", "", "", nil, map[string]string{"gc.phase": "merged"})},
		// 3. deferred
		{want: state.StateDeferred, bead: b("", "deferred", "", "", nil, nil)},
		// 4. pinned
		{want: state.StatePinned, bead: b("", "pinned", "", "", nil, nil)},
		// 5. orchestration: non-work type (session)
		{want: state.StateOrchestration, bead: b("", "", "session", "", nil, nil)},
		// 6. orchestration: non-work type (convoy)
		{want: state.StateOrchestration, bead: b("", "", "convoy", "", nil, nil)},
		// 7. orchestration: wisp id pattern
		{want: state.StateOrchestration, bead: b("vc-wisp-1", "", "task", "", nil, nil)},
		// 8. orchestration: internal title nudge:
		{want: state.StateOrchestration, bead: b("", "", "chore", "nudge:nudge-abc", nil, nil)},
		// 9. orchestration: internal title order:
		{want: state.StateOrchestration, bead: b("", "", "task", "order:foo", nil, nil)},
		// 10. orchestration: gc.kind set
		{want: state.StateOrchestration, bead: b("", "", "task", "", nil, map[string]string{"gc.kind": "scope"})},
		// 11. waiting-review: delivery phase review-pending
		{want: state.StateWaitingReview, bead: b("", "", "", "", nil, map[string]string{"gc.phase": "review-pending"})},
		// 12. waiting-decision: delivery phase decision-pending
		{want: state.StateWaitingDecision, bead: b("", "", "", "", nil, map[string]string{"gc.phase": "decision-pending"})},
		// 13. delivering: delivery phase building
		{want: state.StateDelivering, bead: b("", "", "", "", nil, map[string]string{"gc.phase": "building"})},
		// 14. in-progress: status=in_progress
		{want: state.StateInProgress, bead: b("", "in_progress", "", "", nil, nil)},
		// 15. in-progress: bound to a live session
		{want: state.StateInProgress, bead: b("", "", "", "", nil, map[string]string{"gc.session_name": "sess-live"})},
		// 16. orphaned: bound to a dead session
		{want: state.StateOrphaned, bead: b("", "", "", "", nil, map[string]string{"gc.session_name": "sess-dead"})},
		// 17. blocked-deps: id in blocked set
		{want: state.StateBlockedDeps, bead: b("B", "", "", "", nil, nil)},
		// 18. waiting-human: label "human"
		{want: state.StateWaitingHuman, bead: b("", "", "", "", []string{"human"}, nil)},
		// 19. waiting-human: type=decision
		{want: state.StateWaitingHuman, bead: b("", "", "decision", "", nil, nil)},
		// 20. waiting-human: gc.do_not_auto_route=1
		{want: state.StateWaitingHuman, bead: b("", "", "", "", nil, map[string]string{"gc.do_not_auto_route": "1"})},
		// 21. epic-triage: type=epic
		{want: state.StateEpicTriage, bead: b("", "", "epic", "", nil, nil)},
		// 22. epic-triage: title starts with EPIC:
		{want: state.StateEpicTriage, bead: b("", "", "task", "EPIC: big thing", nil, nil)},
		// 22b. NOT epic-triage: "epic" substring not at word boundary
		{want: state.StateUnknown, bead: b("", "", "task", "Epically important task", nil, nil)},
		// 23. routed-waiting: routed to a live rig
		{want: state.StateRoutedWaiting, bead: b("R", "", "", "", nil, map[string]string{"gc.routed_to": "rig/agent"})},
		// 24. ready-unrouted: open, in ready set, plannable, not routed
		{want: state.StateReadyUnrouted, bead: b("R", "", "", "", nil, nil)},
		// 25. unknown: work-typed, not ready/blocked/routed
		{want: state.StateUnknown, bead: b("", "", "bug", "", nil, nil)},
		// 26. precedence: orchestration beats in_progress status
		{want: state.StateOrchestration, bead: b("", "in_progress", "session", "", nil, nil)},
		// 27. precedence: delivery phase beats in_progress status
		{want: state.StateWaitingReview, bead: b("", "in_progress", "", "", nil, map[string]string{"gc.phase": "review-pending"})},
		// 28. precedence: blocked beats waiting-human (decision type)
		{want: state.StateBlockedDeps, bead: b("B", "", "decision", "", nil, nil)},
		// 29. routed-stalled-dispatch: routed but target rig has no live sessions
		{
			want:             state.StateRoutedStalledDispatch,
			bead:             b("R", "", "", "", nil, map[string]string{"gc.routed_to": "dead-rig/agent"}),
			liveRigsOverride: map[string]bool{}, // empty → dead-rig has no live sessions
		},
	}

	for i, tc := range cases {
		readySet := ready
		if tc.readyOverride != nil {
			readySet = tc.readyOverride
		}
		blockedSet := blocked
		if tc.blockedOverride != nil {
			blockedSet = tc.blockedOverride
		}
		liveRigsSet := liveRigs
		if tc.liveRigsOverride != nil {
			liveRigsSet = tc.liveRigsOverride
		}
		got := state.Classify(tc.bead, readySet, blockedSet, live, liveRigsSet)
		if got != tc.want {
			t.Errorf("case %d: Classify() = %q, want %q (bead id=%q type=%q status=%q title=%q meta=%v)",
				i+1, got, tc.want, tc.bead.id, tc.bead.typ, tc.bead.status, tc.bead.title, tc.bead.meta)
		}
	}
}
