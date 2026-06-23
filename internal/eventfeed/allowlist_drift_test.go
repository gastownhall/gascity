package eventfeed

import (
	"reflect"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// TestAllowedTypesMatchEventConstants is the drift guard that lets
// pkg/eventexport keep its allowlist as raw wire-string literals (so it never
// imports internal/events) while staying in lockstep with the canonical event
// constants. If the supervisor renames/removes a constant's value or the pkg
// literal is mistyped, this fails CI. It queries the published API
// (IsAllowed/AllowedTypeList) since the allowlist map is unexported.
func TestAllowedTypesMatchEventConstants(t *testing.T) {
	want := []string{
		events.BeadCreated,
		events.BeadClosed,
		events.OrderFired,
		events.OrderCompleted,
		events.OrderFailed,
		events.SessionWoke,
		events.SessionStopped,
		events.SessionDraining,
		events.SessionStranded,
		events.ConvoyClosed,
		events.ControllerStarted,
		events.EventsRotated,
		events.SessionDrainAckedWithAssignedWork,
		events.SessionResetStalled,
		events.ProjectIdentityStamped,
		events.StoreMaintenanceDone,
		events.MailSent,
	}

	// Every events constant must be allowed.
	for _, typ := range want {
		if !eventexport.IsAllowed(typ) {
			t.Errorf("IsAllowed(%q) = false; events constant not on the allowlist", typ)
		}
	}

	// The published sorted list must equal exactly the set of events constants —
	// catches both an extra literal in pkg and a missing one.
	got := eventexport.AllowedTypeList()
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(got, wantSorted) {
		t.Fatalf("AllowedTypeList drift:\n got  %v\n want %v", got, wantSorted)
	}
}
