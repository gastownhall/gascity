package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// healExactSessionActiveAlias converges legacy's `active` alias to the
// reconciler's canonical `awake` on a live row the detector-family seam just
// claimed.
//
// It exists because claiming a key ends the keyed pass: every family returns
// from the seam (session_start_reconcile.go) well above the ordinary lane's
// status heal, so a row a family owns never reaches the only heal the keyed
// lane has. Legacy cannot cover it either — withLegacyStatusHealExclusion
// stands its desired-site heal down for exactly the rows that classify
// keyed-owned, and a wake-demanded row does — so the row is left with no heal
// owner at all and keeps the stored alias forever (ga-f7v2ft.140).
//
// The scope is deliberately the alias and nothing else. `active` and `awake`
// both project to BaseStateActive, so this write is decision-neutral: no arm
// that already ran against the stored value could have decided differently
// against the healed one, which is what makes it safe to apply after the
// family's own effect rather than before it. Every other heal — start-pending
// or creating to awake, active to asleep — moves the base state and therefore
// belongs to the lane that owns that decision, so this refuses anything but the
// neutral patch.
//
// It reads the row itself rather than trusting the caller's pre-family
// snapshot, so the conditional write is fenced on the revision the family left
// behind and a family that already moved the row simply loses the guard and
// writes nothing.
func healExactSessionActiveAlias(params exactSessionStartParams, sessionID string, clk clock.Clock) bool {
	if params.Store == nil || params.Config == nil || params.Provider == nil {
		return false
	}
	if strings.TrimSpace(sessionID) == "" {
		return false
	}
	if clk == nil {
		clk = clock.Real{}
	}
	info, response, err := getAuthoritativeSessionStartPersistedRecord(params.Store, sessionID)
	if err != nil || info.ID != sessionID || info.Closed || !beads.RevisionKnown(response.Revision) {
		return false
	}
	if strings.TrimSpace(info.MetadataState) != string(sessionpkg.StateActive) {
		return false
	}
	// Same row-backed fence the ordinary lane's heal pays: labels persist outside
	// the revision, so only identity that resolves in the current config may
	// authorize a whole-row conditional update.
	if !exactSessionStatusHealInputsAreRowBacked(info, params.Config) {
		return false
	}
	plan := planSessionLifecycleStatus(sessionLifecycleShadowInput{
		Info:              info,
		RuntimeObserved:   true,
		RuntimeAlive:      true,
		ObservedAt:        clk.Now().UTC(),
		StartupTimeout:    params.Config.Session.StartupTimeoutDuration(),
		RollbackAvailable: true,
	})
	if plan.Outcome != sessionLifecycleStatusHeal || !exactSessionAliasHealIsProjectionNeutral(plan.Patch) {
		return false
	}
	// Liveness is the whole premise: the planner was asked what a LIVE row owes,
	// so anything short of a POSITIVE observation means the answer does not
	// apply. A dead `active` row projects to asleep, which is not neutral and not
	// this heal's to write, so it keeps the stored alias.
	//
	// Scan completeness is deliberately not asked for. It proves ABSENCE, and
	// this arm never infers absence — it acts only on presence. Demanding it
	// meant the alias could only ever be normalized on a host quiet enough to
	// license an alive target's sweep, which is no host running agents: a live
	// pane withholds the very tmux-absence license the /proc scan needs
	// (ga-bxa8r).
	liveness := runtime.ObserveFreshLiveness(params.Provider, runtime.LivenessTarget{
		SessionID:            info.ID,
		SessionName:          info.SessionNameMetadata,
		ProcessNames:         drainAckStopPendingProcessNames(params.Config, info),
		IncarnationStartedAt: drainAckIncarnationStartedAt(info),
	})
	if !liveness.Running && !liveness.Alive {
		return false
	}
	applied, err := applyHealPatchFenced(sessionFrontDoor(params.Store), info.ID, response.Revision, plan.Patch)
	if err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: healing the active alias for %s: %v\n", info.ID, err) //nolint:errcheck // an advisory heal is level-triggered and re-detected
	}
	return applied
}

// exactSessionAliasHealIsProjectionNeutral reports whether a planned heal is the
// alias normalization and nothing else. It is what keeps healExactSessionActiveAlias
// from growing into a second, unowned copy of the status heal.
func exactSessionAliasHealIsProjectionNeutral(patch sessionpkg.MetadataPatch) bool {
	return len(patch) == 1 && patch["state"] == string(sessionpkg.StateAwake)
}
