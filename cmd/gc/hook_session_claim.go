package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionCurrentClaimFrontDoor opens the routed session-class write front door
// for the city this process is running in. It is the shared root for the two
// halves of the claim back-channel: `gc hook --claim` stamps the claimed bead id
// onto the calling session's bead through it, and `gc hook current` reads that
// stamp back through it.
//
// It routes through cliSessionFrontDoor so a [beads.classes.sessions] relocation
// reaches both halves — a raw work-store front door would stamp the claim onto
// the work store while the real session bead lives in the relocated store, and
// `gc hook current` would then read back nothing forever. The no-refresh config
// loader matches the other hook-path roots (cmd_prime.go's
// persistPrimeHookProviderSessionKey): this runs on every claim, and a nil cfg
// leaves cliSessionStore identity to the input store.
func sessionCurrentClaimFrontDoor() (*session.Store, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil, err
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	return cliSessionFrontDoor(store, cfg, cityPath), nil
}

// hookStampSessionCurrentClaim records beadID as the work bead the session
// identified by sessionID is currently running. It is the production
// implementation of the hookClaimOps.StampSessionClaim seam.
//
// The write goes through session.Store.SetCurrentClaim, which resolves the id
// EXACTLY and refuses a non-session bead before writing anything: bd's fuzzy id
// resolver would otherwise let a post-claim update land on a prefix-colliding
// session if the intended one disappeared concurrently, which is why the claim
// path decorates the session bead only through this guarded seam (see
// publishHookClaimRunMap, which stays a file-based sidecar for exactly that
// reason). SetCurrentClaim also compare-and-skips, so the per-tick adoption
// re-run issues no write once the value is current.
func hookStampSessionCurrentClaim(sessionID, beadID string) error {
	return stampSessionCurrentClaimVia(sessionCurrentClaimFrontDoor, sessionID, beadID)
}

// sessionFrontGetter yields the session front door a session seam reads or
// writes through: sessionCurrentClaimFrontDoor opens one per call, a
// hookCity's sessionFront shares one across an invocation.
type sessionFrontGetter func() (*session.Store, error)

// stampSessionCurrentClaimVia is hookStampSessionCurrentClaim through front.
func stampSessionCurrentClaimVia(front sessionFrontGetter, sessionID, beadID string) error {
	sessFront, err := front()
	if err != nil {
		return err
	}
	_, err = sessFront.SetCurrentClaim(sessionID, beadID)
	return err
}

// hookSessionDrainPending reports whether the session identified by sessionID is
// already draining. It is the production implementation of the
// hookClaimOps.DrainPending seam — the F-D claim fence's only input.
//
// The SESSION ROW is the source, not provider meta. Provider meta carries only
// GC_DRAIN, which reconciler-tracked drains never set, so a provider-meta
// probe would miss the whole keyed population this fence exists for. It reads
// through the same routed front door the claim back-channel writes through, so a
// [beads.classes.sessions] relocation reaches the fence too.
//
// Any state OTHER than draining — including a closed row, whose runtime state
// GetState reports as empty — is not this fence's business. A closed or
// superseded incarnation is the runtime-identity fence's stale-session lane, and
// answering false here leaves that lane's verdict intact rather than relabelling
// it. Errors are returned rather than swallowed: the caller fails OPEN on them,
// and it can only make that choice if it can see them.
func hookSessionDrainPending(sessionID string) (bool, error) {
	return sessionDrainPendingVia(sessionCurrentClaimFrontDoor, sessionID)
}

// sessionDrainPendingVia is hookSessionDrainPending through front.
func sessionDrainPendingVia(front sessionFrontGetter, sessionID string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, nil
	}
	sessFront, err := front()
	if err != nil {
		return false, err
	}
	state, _, err := sessFront.GetState(sessionID)
	if err != nil {
		return false, err
	}
	return state == session.StateDraining, nil
}

// hookResolveSessionWorkDir returns the checkout the session identified by
// sessionID is running in, or "" when that is not knowable. It is the production
// implementation of the hookClaimOps.ResolveSessionWorkDir seam.
//
// A bead routed to a POOL records no checkout of its own: cliBeadRouter.Route
// normalizes a pool target through agentutil.NormalizePoolRouteTarget, so
// gc.routed_to names a pool identity and no slot exists to name a worktree until
// a claim happens (gc-2n4c). Stamping a work_dir at route time would bind a
// pool-label path to the bead, which is the gc-j0cfh defect. The claiming session
// is the first actor that knows the concrete tree, and the session bead is the
// only place that records it: GC_WORK_DIR is not in the env of a pool session.
//
// The id is resolved EXACTLY, for the same reason SetCurrentClaim does: the bd
// fuzzy resolver would otherwise let a prefix collision hand back the checkout of
// a different session, and this value is stamped onto the work bead as durable
// execution identity. Every failure yields "" -- an unset gc.work_dir is the
// honest outcome, and the caller stamps nothing rather than guessing a path.
func hookResolveSessionWorkDir(sessionID string) string {
	return resolveSessionWorkDirVia(sessionCurrentClaimFrontDoor, sessionID)
}

// resolveSessionWorkDirVia is hookResolveSessionWorkDir through front.
func resolveSessionWorkDirVia(front sessionFrontGetter, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	sessFront, err := front()
	if err != nil {
		// Each failure below returns the same "" as a session that simply has no
		// stampable checkout, so without naming the step a lookup fault is
		// indistinguishable in production from the ordinary refusal. The caller
		// behavior is unchanged; only the diagnosis is.
		log.Printf("hookResolveSessionWorkDir: opening the session front door for %q: %v; stamping no %s",
			sessionID, err, beadmeta.WorkDirMetadataKey)
		return ""
	}
	resolved, err := sessFront.ResolveIDByExactID(strings.TrimSpace(sessionID))
	if err != nil {
		log.Printf("hookResolveSessionWorkDir: resolving session id %q exactly: %v; stamping no %s",
			sessionID, err, beadmeta.WorkDirMetadataKey)
		return ""
	}
	info, err := sessFront.Get(resolved)
	if err != nil {
		log.Printf("hookResolveSessionWorkDir: reading session bead %q: %v; stamping no %s",
			resolved, err, beadmeta.WorkDirMetadataKey)
		return ""
	}
	return sessionStampableWorkDir(info)
}

// sessionStampableWorkDir returns the checkout of info that may be stamped onto a
// work bead as gc.work_dir, or "" when info has none that qualifies. It is the
// decidable half of hookResolveSessionWorkDir, split out so the pool refusal is
// testable without a live city.
//
// A POOL-MANAGED session is refused outright: its WorkDir is the slot label
// (worker-slots/worker-N), a SHARED directory rather than an isolated worktree,
// so stamping it would turn a slot label into worktree-ownership evidence on the
// bead (gc-j0cfh). The reconciler side refuses the same value for the same reason
// (workDirStampHasOwnershipEvidence), and this keeps the claim path from becoming
// a second way to mint it.
//
// The classifier is isPoolManagedSessionInfo, this package's canonical one, not
// the raw PoolManaged flag: a session carrying a pool_slot, or one whose origin is
// ephemeral, is running in a shared slot directory for exactly the same gc-j0cfh
// reason even when pool_managed was never stamped on its bead.
func sessionStampableWorkDir(info session.Info) string {
	if isPoolManagedSessionInfo(info) {
		return ""
	}
	return strings.TrimSpace(info.WorkDir)
}

// The started write (PHASE1-2 §6.4, W1). At hand-over, `gc hook --claim`
// records the claim in its own session row's work_started cell, by a CAS on
// the whole row (UpdateRowFenced), so it and the controller's release of an
// unstarted claim (work_ladder.release, §6.5) are linearizable on the row's
// revision: whichever lands first, the other's decide sees it and refuses.

// hookStartedFence is the incarnation a started write must still be the
// row's: the runtime's own instance token and, when it carries one, its
// generation.
type hookStartedFence struct {
	Token  string
	Gen    int64
	HasGen bool
}

// hookStartedFenceOf reads the fence from env, the process env in
// production. An unparsable generation is no generation: the token alone
// fences the write.
func hookStartedFenceOf(env []string) hookStartedFence {
	f := hookStartedFence{Token: strings.TrimSpace(hookClaimEnvValue(env, runtime.InstanceTokenEnv))}
	if raw := strings.TrimSpace(hookClaimEnvValue(env, runtime.RuntimeEpochEnv)); raw != "" {
		if gen, err := strconv.ParseInt(raw, 10, 64); err == nil {
			f.Gen, f.HasGen = gen, true
		}
	}
	return f
}

// hookStartedKey is a claim as the work cells name it: its store ref
// (normalizeIdleClaimStoreRef's form, "" when unknown) and bead ID.
type hookStartedKey struct{ Store, Bead string }

// hookStartedResult is what the started write found.
type hookStartedResult uint8

const (
	// hookStartedUnrecorded: no write was attempted or it failed; the claim
	// is delivered anyway (fail open).
	hookStartedUnrecorded hookStartedResult = iota
	hookStartedRecorded
	hookStartedAlreadyRecorded
	// hookStartedReleasing: the controller's work_ladder.release names the
	// claim; it must not be handed out.
	hookStartedReleasing
	// hookStartedStale: the row is another incarnation's.
	hookStartedStale
	// hookStartedClosed: the row is closed; no turn of it can run the claim.
	hookStartedClosed
)

// hookStartedWriteAttempts bounds the CAS retries against the controller's
// writes to the same row.
const hookStartedWriteAttempts = 5

var errWorkStartedContended = errors.New("work_started: the row moved on every attempt")

// errWorkStartedReset marks a started write that replaced an unreadable cell:
// the claim is recorded, but the claims the old cell held are not.
var errWorkStartedReset = errors.New("work_started was unreadable and was reset to this claim; the claims it held are no longer recorded")

// decideWorkStarted is the started write's decision on a fresh read of the
// row: refuse a closed row and another incarnation's (stale), refuse a claim
// the controller is releasing, skip one already recorded, else record it
// first in the cell. A cell it cannot read is replaced by one holding just
// this claim, and reset reports the read error: the entries it held are lost
// (they read as "every claim started" while unreadable, and the hook reports
// the reset off the pane), but the cell is readable again, so a drain-ack and
// the ladder judge each claim by its own record from here on.
func decideWorkStarted(fresh session.Info, fence hookStartedFence, key hookStartedKey, via string, now time.Time) (res hookStartedResult, patch session.MetadataPatch, reset error) {
	rowGen, _ := strconv.ParseInt(strings.TrimSpace(fresh.Generation), 10, 64)
	switch {
	case fresh.Closed:
		return hookStartedClosed, nil, nil
	case strings.TrimSpace(fresh.InstanceToken) != fence.Token || (fence.HasGen && rowGen != fence.Gen):
		return hookStartedStale, nil, nil
	}
	if session.WorkLadderReleases(fresh.WorkLadderRaw, key.Store, key.Bead) {
		return hookStartedReleasing, nil, nil
	}
	cell, err := session.DecodeWorkStarted(fresh.WorkStartedRaw)
	if err != nil {
		cell, reset = session.WorkStarted{}, err
	}
	if cell.Names(key.Store, key.Bead) {
		return hookStartedAlreadyRecorded, nil, nil
	}
	gen := rowGen
	if fence.HasGen {
		gen = fence.Gen
	}
	cell = cell.With(session.WorkStartedEntry{Store: key.Store, Bead: key.Bead, Gen: gen, At: now.UTC(), Via: via})
	return hookStartedRecorded, session.MetadataPatch{session.WorkStartedKey: session.EncodeWorkStarted(cell)}, reset
}

// hookRecordWorkStarted is the production RecordWorkStarted seam, through the
// same routed session front door as the current-claim stamp.
func hookRecordWorkStarted(sessionID string, fence hookStartedFence, key hookStartedKey, via string) (hookStartedResult, error) {
	return recordWorkStartedVia(sessionCurrentClaimFrontDoor, sessionID, fence, key, via)
}

// recordWorkStartedVia is hookRecordWorkStarted through front.
func recordWorkStartedVia(front sessionFrontGetter, sessionID string, fence hookStartedFence, key hookStartedKey, via string) (hookStartedResult, error) {
	sessFront, err := front()
	if err != nil {
		return hookStartedUnrecorded, err
	}
	return recordWorkStarted(sessFront, sessionID, fence, key, via, time.Now())
}

// recordWorkStarted runs decideWorkStarted under UpdateRowFenced. A CAS lost
// on every attempt is an error, as is a store that cannot fence: the caller
// fails open. A write that replaced an unreadable cell returns Recorded with
// an error naming the reset, which the caller reports as it reports a
// failure: the claims the old cell held are no longer recorded.
func recordWorkStarted(front *session.Store, sessionID string, fence hookStartedFence, key hookStartedKey, via string, now time.Time) (hookStartedResult, error) {
	var res hookStartedResult
	var reset error
	wrote, err := front.UpdateRowFenced(sessionID, hookStartedWriteAttempts, func(fresh session.Info, _ session.PersistedResponse) (session.RowPatch, bool) {
		var patch session.MetadataPatch
		res, patch, reset = decideWorkStarted(fresh, fence, key, via, now)
		return session.RowPatch{Metadata: patch}, len(patch) > 0
	})
	switch {
	case err != nil:
		return hookStartedUnrecorded, err
	case res == hookStartedRecorded && !wrote:
		return hookStartedUnrecorded, errWorkStartedContended
	case reset != nil:
		return res, fmt.Errorf("%w: %w", errWorkStartedReset, reset)
	}
	return res, nil
}

// hookForgetWorkStarted is the production ForgetWorkStarted seam.
func hookForgetWorkStarted(sessionID string, key hookStartedKey) error {
	return forgetWorkStartedVia(sessionCurrentClaimFrontDoor, sessionID, key)
}

// forgetWorkStartedVia is hookForgetWorkStarted through front.
func forgetWorkStartedVia(front sessionFrontGetter, sessionID string, key hookStartedKey) error {
	sessFront, err := front()
	if err != nil {
		return err
	}
	return forgetWorkStarted(sessFront, sessionID, key)
}

// forgetWorkStarted drops key from the row's work_started cell by a CAS, for
// an unwind that gives the claim back: a released claim must not read as
// started. A closed row, a cell that does not name it, and a cell it cannot
// read are left alone.
func forgetWorkStarted(front *session.Store, sessionID string, key hookStartedKey) error {
	_, err := front.UpdateRowFenced(sessionID, hookStartedWriteAttempts, func(fresh session.Info, _ session.PersistedResponse) (session.RowPatch, bool) {
		cell, err := session.DecodeWorkStarted(fresh.WorkStartedRaw)
		if fresh.Closed || err != nil || !cell.Names(key.Store, key.Bead) {
			return session.RowPatch{}, false
		}
		return session.RowPatch{Metadata: session.MetadataPatch{session.WorkStartedKey: session.EncodeWorkStarted(cell.Without(key.Store, key.Bead))}}, true
	})
	return err
}

// hookSessionWorkStarted is the production ReadWorkStarted seam: the session
// row's raw work_started cell, read through the routed front door.
func hookSessionWorkStarted(sessionID string) (string, error) {
	return sessionWorkStartedVia(sessionCurrentClaimFrontDoor, sessionID)
}

// sessionWorkStartedVia is hookSessionWorkStarted through front.
func sessionWorkStartedVia(front sessionFrontGetter, sessionID string) (string, error) {
	sessFront, err := front()
	if err != nil {
		return "", err
	}
	info, err := sessFront.Get(strings.TrimSpace(sessionID))
	if err != nil {
		return "", err
	}
	return info.WorkStartedRaw, nil
}

// hookClaimStoreRefForDir is the production StoreRef seam: the store a claim
// made in dir lives in, as the work cells key it ("city" or "rig:<name>"),
// or "" when the city or its config cannot be read or dir is no store root.
func hookClaimStoreRefForDir(dir string) string {
	cityPath, err := resolveCity()
	if err != nil {
		return ""
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	return claimStoreRefAt(cityPath, cfg, dir)
}

// claimStoreRefAt is hookClaimStoreRefForDir in the city at cityPath, loaded
// as cfg.
func claimStoreRefAt(cityPath string, cfg *config.City, dir string) string {
	if cfg == nil {
		return ""
	}
	ref := workflowStoreRefForDir(dir, cityPath, loadedCityName(cfg, cityPath), cfg)
	if ref == "" {
		return ""
	}
	return normalizeIdleClaimStoreRef(ref)
}

// emitWorkStartedUnrecorded is the production EmitWorkStartedUnrecorded seam:
// a claim was delivered without a work_started record. Best-effort, like
// emitDrainFenceUnavailable, whose posture it shares: the hook failed open,
// and this record is the only sign of it off the pane.
func emitWorkStartedUnrecorded(sessionID, template, beadID string, cause error) {
	emitWorkStartedUnrecordedTo(openCityRecorder(io.Discard), sessionID, template, beadID, cause)
}

// emitWorkStartedUnrecordedTo is emitWorkStartedUnrecorded into rec, which it
// closes.
func emitWorkStartedUnrecordedTo(rec events.Recorder, sessionID, template, beadID string, cause error) {
	if closer, ok := rec.(interface{ Close() error }); ok {
		defer closer.Close() //nolint:errcheck // best-effort event recorder cleanup
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || cause == nil || rec == nil {
		return
	}
	template = strings.TrimSpace(template)
	subject := template
	if subject == "" {
		subject = sessionID
	}
	reason := fmt.Sprintf("claim %s: %v", beadID, cause)
	rec.Record(events.Event{
		Type:    events.WorkStartedUnrecorded,
		Actor:   eventActor(),
		Subject: subject,
		Message: reason,
		Payload: api.SessionLifecyclePayloadJSON(sessionID, template, reason),
	})
}
