package main

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The K1 waits step (CONTRACT v5.5 R1 wait-hold owner and §13 K1 waits row,
// C8 ruling): legacy's wait wake-state pass, then its ready-wait nudge
// dispatch, in the external-reads lane. Its wait-bead writes are idempotent
// (STALE-READ R22) and its lookup-cap stamp is advisory (R1 exception 4);
// only the wait-hold clear writes the session row, and that is fenced
// (clearSessionWaitHoldFenced). Legacy starts a nudge poller when the nudge
// dispatcher is not the supervisor, and so does this step.
//
// It publishes the ready-wait set (I10), which gather reads into
// allocInputs.ReadyWaits, and the dependencies of the deps waits it left
// pending, whose close runs it alone (GUAR-010).

// waitHoldClearAttempts bounds the fenced wait-hold clear's re-decides.
const waitHoldClearAttempts = 3

// addWaitsStep registers the waits step. Call it before start. It runs every
// pass, as legacy runs it every tick.
func (l *externalReadsLane) addWaitsStep() {
	l.waitsStep = &laneStep{name: "waits", legs: sourceNone, run: l.waits}
	l.steps = append(l.steps, l.waitsStep)
}

// waits is the waits step. Like legacy, a failed wake-state pass publishes
// no ready wait.
func (l *externalReadsLane) waits(_ context.Context, env externalReadsEnv) {
	if env.Cfg == nil || env.CityStore == nil || env.Nudges.Store == nil {
		return
	}
	now := l.now()
	sessFront := sessionpkg.NewStore(beads.SessionStore{Store: env.CityStore})
	work := env.WorkStore
	if work == nil {
		work = env.CityStore
	}
	// Dependencies resolve over the serving rigs only (MAINT-052).
	topo := residencyTopologyForCity(env.CityPath, env.Cfg, work, servingRigStores(env.Cfg, env.RigStores, env.SuspendedRigPaths))
	pending := make(map[string]bool)
	ready, err := prepareWaitWakeStateHooked(sessFront, newWaitDependencyPlanReader(topo, len(env.SuspendedRigPaths) > 0), env.Nudges, now, env.Sessions, waitWakeHooks{
		clearHold: clearSessionWaitHoldFenced,
		pending: func(w sessionpkg.WaitInfo) {
			for _, id := range w.DepIDs {
				pending[id] = true
			}
		},
	})
	if err != nil {
		fmt.Fprintf(l.stderr, "external reads: preparing waits: %v\n", err) //nolint:errcheck // best-effort stderr
		ready = nil
	}
	l.waitDeps.Store(&pending)
	if prev := l.readyWaits.Swap(&ready); prev == nil || !maps.Equal(*prev, ready) {
		l.onChange()
	}
	if err := dispatchReadyWaitNudgesWithSnapshot(env.CityPath, env.Cfg, sessFront, env.Nudges, now, nil); err != nil {
		fmt.Fprintf(l.stderr, "external reads: dispatching wait nudges: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// readyWaitSet is the waits step's last ready-wait set (I10), nil before its
// first run.
func (l *externalReadsLane) readyWaitSet() map[string]bool {
	if p := l.readyWaits.Load(); p != nil {
		return *p
	}
	return nil
}

// clearSessionWaitHoldFenced is legacy's clearSessionWaitHoldIfIdle decided
// on the fresh row and written through fencedWriter, never blind: with no
// non-terminal wait left it clears wait_hold, and sleep_intent and
// sleep_reason while each still reads wait-hold, so an operator's hold that
// replaced ours survives. Otherwise it writes nothing.
func clearSessionWaitHoldFenced(sessFront *sessionpkg.Store, sessionID string) error {
	return clearSessionWaitHoldWith(sessFront, sessionID, (fencedWriter{store: sessFront.Store()}).updateMetadataFenced)
}

// errWaitHoldClearContended reports a wait-hold clear whose every CAS lost to
// another writer; the hold still stands and the caller should retry.
var errWaitHoldClearContended = errors.New("wait-hold clear lost to concurrent writes; retry")

// clearSessionWaitHoldWith is the wait-hold clear over a fenced update verb:
// fencedWriter's for v2 (never blind), the session store's for legacy.
func clearSessionWaitHoldWith(sessFront *sessionpkg.Store, sessionID string, update func(string, int, func(sessionpkg.Info, sessionpkg.PersistedResponse) sessionpkg.MetadataPatch) (bool, error)) error {
	if sessionID == "" {
		return nil
	}
	var waitsErr error
	var wanted bool
	ok, err := update(sessionID, waitHoldClearAttempts, func(row sessionpkg.Info, _ sessionpkg.PersistedResponse) sessionpkg.MetadataPatch {
		wanted = false
		held, err := hasNonTerminalWaits(sessFront, sessionID)
		if waitsErr = err; err != nil || held {
			return nil
		}
		patch := sessionpkg.MetadataPatch{}
		if row.WaitHold != "" {
			patch["wait_hold"] = ""
		}
		if row.SleepIntent == string(sessionpkg.SleepReasonWaitHold) {
			patch["sleep_intent"] = ""
		}
		if row.SleepReason == string(sessionpkg.SleepReasonWaitHold) {
			patch["sleep_reason"] = ""
		}
		wanted = len(patch) > 0
		return patch
	})
	switch {
	case err != nil:
		err = fmt.Errorf("clearing wait hold on %s: %w", sessionID, err)
	case wanted && !ok:
		err = fmt.Errorf("%w: %s", errWaitHoldClearContended, sessionID)
	}
	return errors.Join(err, waitsErr)
}
