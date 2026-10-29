package main

import (
	"io"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// hookCity is the city one `gc hook` invocation works in: resolved once, its
// config loaded once, and the city store behind its session front door opened at
// most once, however many of the invocation's steps reach it.
//
// Before it, each step reached the city on its own. The session fence opened
// the city store; the drain-pending probe, the session work-dir lookup and the
// current-claim stamp (and its clear) each resolved the city again, reloaded its
// config and opened the store again (sessionCurrentClaimFrontDoor); and every
// claim event resolved the city again to open its event log. On maintainer-city
// that was ~0.5 s per front-door open and ~0.15 s per event. The store handle
// itself was already shared: `gc hook` runs under the invocation store memo
// (invocationStoreMemoCommands), which answers a repeat open with the handle
// already open, so the cost was resolving the city and loading its config. The
// invocation's other store opens still rely on that memo.
//
// Sharing changes no answer. Nothing here caches a read: every session read and
// write goes to the store live, exactly as through a freshly opened one, and the
// session class routes the same way whichever config the front door is built
// from (resolveClassStore does not read it). Only a successful open is kept, so
// a step whose open fails is retried by the next step, as each step's own open
// used to be.
type hookCity struct {
	path string
	cfg  *config.City

	mu    sync.Mutex
	front *session.Store
}

// hookCityStoreOpener opens the city store behind a hook invocation's session
// front door. A variable so tests can count the opens.
var hookCityStoreOpener = openCityStoreAtWithConfig

// newHookCity is the city a `gc hook` invocation resolved at path and loaded
// as cfg.
func newHookCity(path string, cfg *config.City) *hookCity {
	return &hookCity{path: path, cfg: cfg}
}

// sessionFront returns the invocation's session front door, opening the city
// store on first use.
func (h *hookCity) sessionFront() (*session.Store, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.front != nil {
		return h.front, nil
	}
	store, err := hookCityStoreOpener(h.path, h.cfg)
	if err != nil {
		return nil, err
	}
	h.front = cliSessionFrontDoor(store, h.cfg, h.path)
	return h.front, nil
}

// recorder opens the city's event log, as openCityRecorder does, without
// resolving the city again. Lifecycle events are best-effort: a log that cannot
// be opened is events.Discard.
func (h *hookCity) recorder() events.Recorder {
	return openCityRecorderAt(h.path, io.Discard)
}

// claimOps returns claim ops whose session seams (the drain-pending probe, the
// session work-dir lookup, the current-claim stamp and its clear, and the
// work_started write, removal and read) answer through the invocation's one
// session front door, whose claim store refs are read off its loaded config, and whose events and continuation
// nudge reach the invocation's city without resolving it again. Every other
// seam is left to its default (hookClaimOps.applyDefaults).
func (h *hookCity) claimOps() hookClaimOps {
	var ops hookClaimOps
	ops.DrainPending = func(sessionID string) (bool, error) {
		return sessionDrainPendingVia(h.sessionFront, sessionID)
	}
	ops.ResolveSessionWorkDir = func(sessionID string) string {
		return resolveSessionWorkDirVia(h.sessionFront, sessionID)
	}
	ops.StampSessionClaim = func(sessionID, beadID string) error {
		return stampSessionCurrentClaimVia(h.sessionFront, sessionID, beadID)
	}
	ops.EmitClaimRejected = func(beadID, existingClaimant, attemptedClaimant string) {
		emitClaimRejected(h.recorder(), beadID, existingClaimant, attemptedClaimant)
	}
	ops.EmitClaimFailed = func(beadID string, opts hookClaimOptions, err error) {
		emitClaimFailedTo(h.recorder, beadID, opts, err)
	}
	ops.EmitClaimReleased = func(release hookClaimReleaseRecord) {
		emitClaimReleased(h.recorder(), release)
	}
	ops.EmitClaimWindowExpired = func(expiry hookClaimWindowExpiry) {
		emitClaimWindowExpired(h.recorder(), expiry)
	}
	ops.EmitHookClaimReclaimedStale = func(beadID, previousOwner, newAssignee string) {
		emitClaimReclaimedStale(h.recorder(), beadID, previousOwner, newAssignee)
	}
	ops.EmitExecutionStepStarted = func(step beads.Bead, dir string, env []string, assignee string) {
		emitExecutionStepStarted(h.recorder(), step, dir, env, assignee)
	}
	ops.EnqueueContinuationNudge = func(assignee string) {
		continuationNudgeEnqueueAt(h.path, assignee)
	}
	ops.RecordWorkStarted = func(sessionID string, fence hookStartedFence, key hookStartedKey, via string) (hookStartedResult, error) {
		return recordWorkStartedVia(h.sessionFront, sessionID, fence, key, via)
	}
	ops.ForgetWorkStarted = func(sessionID string, key hookStartedKey) error {
		return forgetWorkStartedVia(h.sessionFront, sessionID, key)
	}
	ops.ReadWorkStarted = func(sessionID string) (string, error) {
		return sessionWorkStartedVia(h.sessionFront, sessionID)
	}
	ops.ReadSessionTrigger = func(sessionID string) (string, error) {
		return sessionTriggerBeadVia(h.sessionFront, sessionID)
	}
	ops.StoreRef = func(dir string) string {
		return claimStoreRefAt(h.path, h.cfg, dir)
	}
	ops.EmitWorkStartedUnrecorded = func(sessionID, template, beadID string, cause error) {
		emitWorkStartedUnrecordedTo(h.recorder(), sessionID, template, beadID, cause)
	}
	return ops
}
