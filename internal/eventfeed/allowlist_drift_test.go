package eventfeed

import (
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// TestAllowedTypesMatchEventConstants is the drift guard that lets
// pkg/eventexport keep its AllowedTypes as raw wire-string literals (so it never
// imports internal/events) while staying in lockstep with the canonical event
// constants. If the supervisor renames/removes a constant's value or the pkg
// literal is mistyped, this fails CI.
func TestAllowedTypesMatchEventConstants(t *testing.T) {
	want := map[string]bool{
		events.BeadCreated:                       true,
		events.BeadClosed:                        true,
		events.OrderFired:                        true,
		events.OrderCompleted:                    true,
		events.OrderFailed:                       true,
		events.SessionWoke:                       true,
		events.SessionStopped:                    true,
		events.SessionDraining:                   true,
		events.SessionStranded:                   true,
		events.ConvoyClosed:                      true,
		events.ControllerStarted:                 true,
		events.EventsRotated:                     true,
		events.SessionDrainAckedWithAssignedWork: true,
		events.SessionResetStalled:               true,
		events.ProjectIdentityStamped:            true,
		events.StoreMaintenanceDone:              true,
		events.MailSent:                          true,
	}
	got := eventexport.AllowedTypes

	if len(got) != len(want) {
		t.Fatalf("AllowedTypes has %d entries, want %d (drift between pkg literals and events constants)", len(got), len(want))
	}
	for typ := range want {
		if !got[typ] {
			t.Errorf("AllowedTypes missing %q (present as an events constant)", typ)
		}
	}
	for typ := range got {
		if !want[typ] {
			t.Errorf("AllowedTypes has %q with no matching events constant", typ)
		}
	}
}
