package coordclass

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCoordinationOwned pins the coordination-class predicate (architecture B
// v2, mc-v8g4p): a message, a wait by the gc:wait label or by its typed
// session:<session_id> shape (session.IsWaitBead's), a queued nudge or its
// shadow, an order's tracking or run label and an extmsg row, in any case and
// spacing; not a wisp, a work bead, a session-labeled task or a wait-shaped
// bead whose session label names another session.
func TestCoordinationOwned(t *testing.T) {
	for _, c := range []struct {
		name string
		b    beads.Bead
		want bool
	}{
		{"message", beads.Bead{Type: "message"}, true},
		{"' Message '", beads.Bead{Type: " Message "}, true},
		{"gc:wait on a task", beads.Bead{Type: "task", Labels: []string{"gc:wait"}}, true},
		{"a typed wait by its session label", beads.Bead{Type: "gate", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}, true},
		{"a legacy wait by its session label", beads.Bead{Type: "Wait", Labels: []string{" Session:S-1 "}, Metadata: map[string]string{"session_id": "S-1"}}, true},
		{"a gate naming another session", beads.Bead{Type: "gate", Labels: []string{"session:s-2"}, Metadata: map[string]string{"session_id": "s-1"}}, false},
		{"a task with a session label", beads.Bead{Type: "task", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}}, false},
		{"a nudge shadow", beads.Bead{Type: "chore", Labels: []string{"GC:Nudge"}}, true},
		{"a queued nudge", beads.Bead{Type: "task", Labels: []string{"gc:nudge-queue", "agent:a"}}, true},
		{"order tracking", beads.Bead{Type: "task", Labels: []string{"order-tracking"}}, true},
		{"an order run alone", beads.Bead{Type: "task", Labels: []string{"note", "order-run:digest"}}, true},
		{"an extmsg binding", beads.Bead{Type: "task", Labels: []string{"gc:extmsg-binding"}}, true},
		{"an extmsg transcript state", beads.Bead{Type: "task", Labels: []string{" gc:extmsg-transcript-state"}}, true},
		{"a wisp", beads.Bead{Type: "task", Labels: []string{"gc:wisp"}}, false},
		{"a task", beads.Bead{Type: "task", Labels: []string{"note"}}, false},
		{"a gate step", beads.Bead{Type: "gate"}, false},
		{"an extmsg locator alone", beads.Bead{Type: "task", Labels: []string{"extmsg:binding:conv:v1:x"}}, false},
	} {
		if got := CoordinationOwned(c.b); got != c.want {
			t.Errorf("%s: CoordinationOwned = %t, want %t", c.name, got, c.want)
		}
		if c.want && !session.IsWaitBead(c.b) && c.b.Type == "gate" {
			t.Errorf("%s: owned as a wait, but session.IsWaitBead disagrees", c.name)
		}
	}
	if i := CoordinationOwnedLabel("task", []string{"a", "order-run:x"}, nil); i != 1 {
		t.Errorf("CoordinationOwnedLabel = %d, want 1", i)
	}
}
