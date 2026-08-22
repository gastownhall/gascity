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
		events.ExecutionWorkAssociated,
		events.ExecutionRunAnchored,
		events.ExecutionStepDefined,
		events.ExecutionStepStarted,
		events.ExecutionStepCompleted,
		events.SessionDrainAckedWithAssignedWork,
		events.SessionResetStalled,
		events.ProjectIdentityStamped,
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

// TestClaimFenceEventsAreDeliberatelyNotExported pins the DENY side of the
// allowlist for the turn-binding events.
//
// The list above is exhaustive, so anything absent is already denied — but
// denied-by-omission and denied-on-purpose look identical in a diff, and the
// question "should this new event leave the machine?" is the one an export
// allowlist exists to force someone to answer. Stating it here makes a later
// addition a deliberate edit to a test that explains the reasoning, rather than
// a silent widening of what gc ships off-box.
//
// Both are operational telemetry about a worker's own process lifecycle —
// invocation ages, parent liveness, claims given back — with no cross-machine
// consumer. execution.step_stalled (deploy lanes only) is the same shape and is
// denied for the same reason. If a hosted consumer ever needs them, add them to
// the allowlist AND to the want list above, and delete the entry here.
func TestClaimFenceEventsAreDeliberatelyNotExported(t *testing.T) {
	for _, typ := range []string{
		events.ExecutionClaimWindowExpired,
		events.BeadClaimReleased,
	} {
		if eventexport.IsAllowed(typ) {
			t.Errorf("IsAllowed(%q) = true; this event is local operational telemetry and was not reviewed for export — if that changed, say so in the allowlist and in this test", typ)
		}
	}
}
