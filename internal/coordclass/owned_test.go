package coordclass

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestControllerOwned pins the one predicate the generic bead API and the
// workflow teardowns refuse by (architecture B): a session row by type or by
// the session label (an empty type included), a startup-health episode, a
// convergence root and an operation-journal row (type op: op and fact rows),
// in any case and spacing; nothing else, the coordination classes v1 leaves
// to their routes included.
func TestControllerOwned(t *testing.T) {
	for _, c := range []struct {
		name string
		b    beads.Bead
		want bool
	}{
		{"session", beads.Bead{Type: "session"}, true},
		{"' Session '", beads.Bead{Type: " Session "}, true},
		{"the session label, no type", beads.Bead{Labels: []string{"gc:session"}}, true},
		{"a task with ' GC:Session '", beads.Bead{Type: "task", Labels: []string{"note", " GC:Session "}}, true},
		{"startup-health episode", beads.Bead{Type: "startup-health-episode"}, true},
		{"'STARTUP-HEALTH-EPISODE '", beads.Bead{Type: "STARTUP-HEALTH-EPISODE "}, true},
		{"convergence", beads.Bead{Type: "Convergence"}, true},
		{"op row", beads.Bead{Type: "op", Labels: []string{"gc:op", "gc:op-conv:gc-7"}}, true},
		{"fact row", beads.Bead{Type: "op", Status: "closed", Labels: []string{"gc:fact", "gc:fact-conv:gc-7"}}, true},
		{"' OP '", beads.Bead{Type: " OP "}, true},
		{"a task carrying gc:op", beads.Bead{Type: "task", Labels: []string{"gc:op"}}, false},
		{"task", beads.Bead{Type: "task"}, false},
		{"no type", beads.Bead{}, false},
		{"message", beads.Bead{Type: "message"}, false},
		{"a wait", beads.Bead{Type: "task", Labels: []string{"gc:wait"}}, false},
		{"a nudge", beads.Bead{Type: "task", Labels: []string{"gc:nudge"}}, false},
		{"order tracking", beads.Bead{Type: "task", Labels: []string{"order-tracking"}}, false},
		{"a wisp", beads.Bead{Type: "task", Labels: []string{"gc:wisp"}}, false},
		{"an extmsg row", beads.Bead{Type: "task", Labels: []string{"gc:extmsg-binding"}}, false},
		{"a label naming a session", beads.Bead{Type: "task", Labels: []string{"gc:session-x"}}, false},
	} {
		if got := ControllerOwned(c.b); got != c.want {
			t.Errorf("%s: ControllerOwned = %t, want %t", c.name, got, c.want)
		}
	}
	if i := ControllerOwnedLabel([]string{"a", "b", "gc:session"}); i != 2 {
		t.Errorf("ControllerOwnedLabel = %d, want 2", i)
	}
}
