package main

import (
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The keyed drain-ack fence asked the durable row for one literal spelling of
// "this session is running": state == "active". Nothing keeps a live pool
// member on that spelling. The status heal rewrites every active row to
// "awake" within a tick of it reaching the runtime
// (session_status_alias_heal.go:52), and both spellings project to
// BaseStateActive — so from the heal onward the keyed family refused its own
// members with lease_invalid, handed the acknowledgement to legacy, and legacy
// applied the drain effect the row-scoped purity assertion is watching for.
//
// ga-f7v2ft.147: `keyed_drain_ack_owner` fired ZERO times in eight journey
// runs and the :1993 signature reproduced byte-for-byte in a citable one; the
// instrumented run reads the refusal as `lease_invalid/lifecycle_shape` at the
// pre-commit authorize. The unit fixtures never caught it because they stamp
// state=active by hand and no test ever ran the heal over them.

// TestAuthorizeRoutedWorkPoolDrainAckHoldsAStatusHealedMember drains a member
// in the state production actually leaves it in.
func TestAuthorizeRoutedWorkPoolDrainAckHoldsAStatusHealedMember(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)

	// Exactly what the status heal writes: state active -> awake, nothing else
	// (healSessionStatusPatch asserts len(patch) == 1).
	if err := fixture.store.SetMetadataBatch(fixture.info.ID, map[string]string{
		"state": string(sessionpkg.StateAwake),
	}); err != nil {
		t.Fatalf("apply status heal: %v", err)
	}
	healed, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read healed pool session: %v", err)
	}
	if healed.MetadataState != string(sessionpkg.StateAwake) {
		t.Fatalf("healed session state = %q, want awake", healed.MetadataState)
	}

	authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, healed, fixture.lease)
	if err != nil || !authorized || refusal != drainAckRefusalNone {
		t.Fatalf("status-healed member authorization = (%t, %q, %v), want the keyed owner to hold the drain",
			authorized, refusal, err)
	}
}

// TestRoutedWorkPoolDrainAckLifecycleShapeMatchesTheProjection keeps the gate
// honest in both directions: it widens by exactly the second spelling of a
// running row and by nothing else. A dormant, terminal or drained row is still
// outside the shape, and a legacy-draining row without the acknowledgement
// stamp still is too.
func TestRoutedWorkPoolDrainAckLifecycleShapeMatchesTheProjection(t *testing.T) {
	shape := func(state sessionpkg.State) sessionpkg.Info {
		return sessionpkg.Info{MetadataState: string(state)}
	}
	for _, tc := range []struct {
		name string
		info sessionpkg.Info
		want bool
	}{
		{name: "active", info: shape(sessionpkg.StateActive), want: true},
		{name: "awake", info: shape(sessionpkg.StateAwake), want: true},
		{name: "closed row", info: sessionpkg.Info{MetadataState: string(sessionpkg.StateAwake), Closed: true}, want: false},
		{name: "asleep", info: shape(sessionpkg.StateAsleep), want: false},
		{name: "suspended", info: shape(sessionpkg.StateSuspended), want: false},
		{name: "drained", info: shape(sessionpkg.StateDrained), want: false},
		{name: "creating", info: shape(sessionpkg.StateCreating), want: false},
		{name: "draining without the ack stamp", info: shape(sessionpkg.StateDraining), want: false},
		{
			name: "draining with the ack stamp",
			info: sessionpkg.Info{
				MetadataState: string(sessionpkg.StateDraining),
				StateReason:   sessionpkg.DrainAckStopPendingReason,
			},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRoutedWorkPoolDrainAckLifecycleShape(tc.info); got != tc.want {
				t.Fatalf("isRoutedWorkPoolDrainAckLifecycleShape(%+v) = %t, want %t", tc.info, got, tc.want)
			}
		})
	}
}
