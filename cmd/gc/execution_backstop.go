package main

// The execution backstop: a claim that never becomes execution.
//
// Every backstop that came before this one ends at the CLAIM. nudgeStalledPoolClaims
// clears the instant its trigger bead flips to in_progress ("the slot is doing
// its job"), and nudgeStalledPoolContinuations only ever looks at the OPEN
// successor of a step that already completed. So the one state neither covers is
// the one the incident produced: a pool slot holding an in-progress bead, idle at
// its prompt, having claimed and then ended its turn without starting the work.
// From there nothing in the fleet converges — the bead is not open, so no claim
// probe wants it; the session is alive, so no crash lane touches it — until the
// session is recycled 15-85 minutes later and the dead-assignee reopen releases
// the claim.
//
// This predicate closes that with the same bounded shape as its two siblings:
// observe, nudge, back off, give up. It re-delivers the agent's OWN configured
// claim nudge, which is idempotent by construction — re-running `gc hook --claim`
// on a bead this session already owns returns action=work
// reason=existing_assignment (the ga-i44k invariant), so a slot that was merely
// slow re-reads its assignment instead of being handed a second one.
//
// # The churn guard is provider idleness, and it is the predicate's job
//
// The shared engine does not consult the runtime; it keys purely on bead state.
// That is enough for the sibling predicates, because their outstanding condition
// (a bead still OPEN) is structurally invisible to a working agent — the moment
// it claims, the predicate stops matching. This one's condition is the exact
// opposite: an in-progress bead is what a working agent looks like. So the
// predicate holds unless the runtime itself reports no activity for at least the
// grace window, and an UNKNOWN activity signal (an error, or a runtime that
// reports none) holds too. That is the #312 lesson stated in the one place it
// applies: never nudge on "we cannot tell".
//
// # Exhaustion hands the session to a lane that already converges
//
// At the attempt cap the stall becomes a typed event and the session is handed
// to the ordinary drain path, whose recycle -> dead-assignee-reopen chain is the
// one that converges today — only now in a bounded ~11 minutes rather than on
// recycle roulette. Restart-with-backoff is established framework liveness (the
// same shape as health patrol); the decision about the WORK is still the agent's.
// The observable escalation is latched on the session bead so it fires once per
// stalled claim, not once per tick. The tracked drain remains level-triggered:
// a controller restart loses its in-memory drain tracker, so later ticks for the
// same claim must reconstruct it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessions "github.com/gastownhall/gascity/internal/session"
)

// Session-bead metadata keys for the execution backstop. Persisted for the same
// reason the sibling backstops persist theirs: a controller restart must resume
// the state machine, never replay it (test-5il).
const (
	executionClaimNudgeWorkKey     = "execution_claim_nudge_work"
	executionClaimNudgeRootKey     = "execution_claim_nudge_root"
	executionClaimNudgeStoreRefKey = "execution_claim_nudge_store_ref"
	executionClaimNudgeCountKey    = "execution_claim_nudge_count"
	executionClaimNudgeAtKey       = "execution_claim_nudge_at"
	// The stalled latch is authority over one exact session incarnation and one
	// exact ownership incarnation of its claim. These coordinates make a
	// controller restart reconstruct only the drain that was actually earned;
	// a re-wake or close/reopen/re-claim starts a fresh bounded window.
	executionClaimNudgeGenerationKey     = "execution_claim_nudge_generation"
	executionClaimNudgeInstanceTokenKey  = "execution_claim_nudge_instance_token"
	executionClaimNudgeAwakeStartedAtKey = "execution_claim_nudge_awake_started_at"
	executionClaimNudgeAssigneeKey       = "execution_claim_nudge_assignee"
	executionClaimNudgeRevisionKey       = "execution_claim_nudge_work_revision"
	executionClaimNudgeClaimFenceKey     = "execution_claim_nudge_work_claim_fence"
	// The lifecycle authority is a versioned SHA-256 of the complete canonical
	// drain lifecycle version read immediately before the stalled latch lands.
	// Marker metadata is excluded by construction, so atomically writing this
	// key beside the latch cannot invalidate the authority it records.
	executionClaimNudgeLifecycleAuthorityKey = "execution_claim_nudge_lifecycle_authority"
	// The close authority is the fingerprint expected after the sole lifecycle
	// mutation made by ClosePatch (state=execution-stalled). Keeping it beside
	// the original authority makes a patch-first close crash-recoverable without
	// guessing which preterminal state preceded the patch.
	executionClaimNudgeCloseAuthorityKey = "execution_claim_nudge_close_authority"
	// executionClaimNudgeStalledKey latches the one-shot observable escalation
	// so the typed event fires once per stalled claim. The drain request is
	// intentionally replayed because its tracker is in-memory and its enqueue
	// operation is idempotent.
	executionClaimNudgeStalledKey = sessions.ExecutionClaimNudgeStalledMetadataKey
)

var executionClaimMarkerKeys = []string{
	executionClaimNudgeWorkKey,
	executionClaimNudgeRootKey,
	executionClaimNudgeStoreRefKey,
	executionClaimNudgeGenerationKey,
	executionClaimNudgeInstanceTokenKey,
	executionClaimNudgeAwakeStartedAtKey,
	executionClaimNudgeAssigneeKey,
	executionClaimNudgeRevisionKey,
	executionClaimNudgeClaimFenceKey,
	executionClaimNudgeCountKey,
	executionClaimNudgeAtKey,
	executionClaimNudgeStalledKey,
	executionClaimNudgeLifecycleAuthorityKey,
	executionClaimNudgeCloseAuthorityKey,
}

// nudgeStalledPoolExecution re-delivers the configured claim nudge to a pool slot
// that HOLDS an in-progress claim it never started executing, and escalates once
// the bounded attempts are spent.
//
// work/workStores/workStoreRefs are the reconciler's index-aligned assigned-work
// snapshot; requestDrain is the existing drain request (drainOps.setDrain), taken
// as a function so this file needs no reconciler wiring of its own. A partial
// snapshot is not evidence of a stall, so it disables the predicate for that tick.
type executionStalledDrainRequester func(beads.Bead, backstopTarget) (backstopResolution, error)

func nudgeStalledPoolExecution(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	work []beads.Bead,
	workStores []beads.Store,
	workStoreRefs []string,
	snapshotPartial bool,
	cityPath string,
	now time.Time,
	rec events.Recorder,
	requestDrain executionStalledDrainRequester,
	stdout io.Writer,
) {
	if sp == nil || cfg == nil || store == nil || snapshotPartial {
		return // hot reconcile path: never panic on a half-built dependency
	}
	// The work row, owning store handle, and durable store reference are one
	// authority tuple. A short companion slice cannot prove which store owns a
	// row, and treating a missing ref as HQ would let restart reconstruction bind
	// a same-ID row from the wrong store.
	if len(work) != len(workStores) || len(work) != len(workStoreRefs) {
		return
	}
	if sess, ok := store.(beads.SessionStore); ok && sess.Store == nil {
		return
	}
	pred := poolExecutionBackstop{
		cfg:          cfg,
		sp:           sp,
		now:          now,
		rec:          rec,
		requestDrain: requestDrain,
		cityPath:     cityPath,
		claims:       newExecutionClaimSnapshot(work, workStores, workStoreRefs),
	}
	runNudgeBackstop(sp, store, sessionBeads, nil, now, stdout, "execution-claim-nudge", pred)

	// The shared engine intentionally ignores stopped runtimes. A durable
	// execution-stalled latch is different: after a controller restart its
	// in-memory tracker is gone, and the runtime may already have stopped. Scan
	// only those preterminal latches and reconstruct/retire them immediately,
	// without consuming pacing attempts or delivering another nudge. A row whose
	// ClosePatch already landed is finalized by the reconciler's earlier
	// terminal-close-pending lane and must never return to work validation here.
	for i := range sessionBeads {
		s := &sessionBeads[i]
		if s.Status == "closed" || !pred.governs(*s) ||
			!sessions.HasExecutionClaimNudgeStalledMetadata(s.Metadata) ||
			strings.TrimSpace(s.Metadata["state"]) == executionStalledDrainReason {
			continue
		}
		sessName := strings.TrimSpace(s.Metadata["session_name"])
		if sessName == "" || sp.IsRunning(sessName) {
			continue
		}
		_, resolution := pred.resolve(*s, nil, sessName)
		switch resolution {
		case backstopResolutionClear:
			pred.clear(store, s, stdout)
		case backstopResolutionOutstanding:
			pred.exhausted(store, s, stdout)
		case backstopResolutionHold:
		}
	}
}

// executionClaim is one in-progress claim from the assigned-work snapshot, kept
// with the store handle its live re-read must use.
type executionClaim struct {
	BeadID     string
	RootID     string
	StoreRef   string
	Assignee   string
	Revision   int64
	ClaimFence int64
	Store      beads.Store
}

// executionClaimSnapshot indexes in-progress claims by their exact assignee
// string. Resolution is by identity rather than by bead id because the question
// this predicate asks is "what does THIS session hold", and the same bead id can
// exist in independent stores.
type executionClaimSnapshot struct {
	byAssignee map[string][]executionClaim
}

func newExecutionClaimSnapshot(work []beads.Bead, stores []beads.Store, storeRefs []string) executionClaimSnapshot {
	snapshot := executionClaimSnapshot{byAssignee: make(map[string][]executionClaim)}
	for i, wb := range work {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" || !strings.EqualFold(strings.TrimSpace(wb.Status), "in_progress") {
			continue
		}
		if strings.TrimSpace(wb.ID) == "" {
			continue
		}
		claim := executionClaim{
			BeadID:     wb.ID,
			RootID:     strings.TrimSpace(wb.Metadata[beadmeta.RootBeadIDMetadataKey]),
			Assignee:   assignee,
			Revision:   wb.Revision,
			ClaimFence: wb.ClaimFence,
		}
		if i < len(storeRefs) {
			claim.StoreRef = normalizeIdleClaimStoreRef(storeRefs[i])
		}
		if i < len(stores) {
			claim.Store = stores[i]
		}
		snapshot.byAssignee[assignee] = append(snapshot.byAssignee[assignee], claim)
	}
	return snapshot
}

// forIdentities returns the deduped claims held by any of the session's current
// identities, in a stable order so a session with several claims resolves the
// same way on every tick.
func (s executionClaimSnapshot) forIdentities(identities []string) []executionClaim {
	seen := make(map[string]struct{})
	var out []executionClaim
	for _, identity := range identities {
		for _, claim := range s.byAssignee[identity] {
			key := claim.StoreRef + "\x00" + claim.BeadID
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, claim)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StoreRef != out[j].StoreRef {
			return out[i].StoreRef < out[j].StoreRef
		}
		return out[i].BeadID < out[j].BeadID
	})
	return out
}

// poolExecutionBackstop is the backstopPredicate for a pool slot that claimed a
// bead and never executed it.
type poolExecutionBackstop struct {
	cfg          *config.City
	sp           runtime.Provider
	now          time.Time
	rec          events.Recorder
	requestDrain executionStalledDrainRequester
	cityPath     string
	claims       executionClaimSnapshot
}

func (poolExecutionBackstop) handlesExecutionStalledLatch() bool { return true }

func (p poolExecutionBackstop) backstopLifecycleCityPath() string { return p.cityPath }

func executionIncarnationFromBead(s beads.Bead) sessions.Info {
	return sessions.Info{
		Generation:     strings.TrimSpace(s.Metadata["generation"]),
		InstanceToken:  strings.TrimSpace(s.Metadata["instance_token"]),
		AwakeStartedAt: strings.TrimSpace(s.Metadata["awake_started_at"]),
	}
}

func executionStalledLifecycleAuthority(info sessions.Info) string {
	return sessions.ExecutionStalledLifecycleAuthority(info)
}

func validExecutionStalledLifecycleAuthority(authority string) bool {
	return sessions.ValidExecutionStalledLifecycleAuthority(authority)
}

func executionIncarnationProven(info sessions.Info) bool {
	inc := drainAckIncarnationOf(info)
	return inc.generation != "" && inc.instanceToken != "" && inc.awakeStartedAt != ""
}

func executionWorkAuthorityCoordinatesProven(revision, claimFence int64) bool {
	return revision != 0 || claimFence != 0
}

func executionClaimAuthorityProven(claim executionClaim) bool {
	return strings.TrimSpace(claim.BeadID) != "" &&
		strings.TrimSpace(claim.Assignee) != "" &&
		claim.Store != nil &&
		executionWorkAuthorityCoordinatesProven(claim.Revision, claim.ClaimFence)
}

func executionTargetAuthorityProven(target backstopTarget) bool {
	return strings.TrimSpace(target.ID) != "" &&
		strings.TrimSpace(target.Assignee) != "" &&
		target.Store != nil &&
		executionWorkAuthorityCoordinatesProven(target.WorkRevision, target.WorkClaimFence)
}

func executionAssigneeIsCurrent(info sessions.Info, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	for _, identity := range []string{
		info.ID,
		info.SessionNameMetadata,
		info.ConfiguredNamedIdentity,
		info.Alias,
	} {
		if strings.TrimSpace(identity) == assignee {
			return true
		}
	}
	return false
}

func executionSessionLifecycleDrainable(info sessions.Info) bool {
	if info.Closed {
		return false
	}
	switch info.State {
	case sessions.StateNone, sessions.StateActive, sessions.StateAwake:
		return true
	default:
		return false
	}
}

func executionTarget(claim executionClaim, incarnation sessions.Info) backstopTarget {
	inc := drainAckIncarnationOf(incarnation)
	return backstopTarget{
		ID:             claim.BeadID,
		RootID:         claim.RootID,
		StoreRef:       claim.StoreRef,
		Generation:     inc.generation,
		InstanceToken:  inc.instanceToken,
		AwakeStartedAt: inc.awakeStartedAt,
		WorkRevision:   claim.Revision,
		WorkClaimFence: claim.ClaimFence,
		Assignee:       claim.Assignee,
		Store:          claim.Store,
	}
}

func executionMarkerIncarnationMatches(s beads.Bead, incarnation sessions.Info) bool {
	inc := drainAckIncarnationOf(incarnation)
	return strings.TrimSpace(s.Metadata[executionClaimNudgeGenerationKey]) == inc.generation &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeInstanceTokenKey]) == inc.instanceToken &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeAwakeStartedAtKey]) == inc.awakeStartedAt
}

func executionMarkerClaimAuthorityMatches(s beads.Bead, target backstopTarget) bool {
	revision, revisionErr := strconv.ParseInt(strings.TrimSpace(s.Metadata[executionClaimNudgeRevisionKey]), 10, 64)
	claimFence, claimFenceErr := strconv.ParseInt(strings.TrimSpace(s.Metadata[executionClaimNudgeClaimFenceKey]), 10, 64)
	return revisionErr == nil && claimFenceErr == nil &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeAssigneeKey]) == target.Assignee &&
		revision == target.WorkRevision && claimFence == target.WorkClaimFence
}

func executionDurableMarkerMatchesTarget(s beads.Bead, target backstopTarget) bool {
	return strings.TrimSpace(target.StalledAt) != "" &&
		s.Metadata[executionClaimNudgeStalledKey] == target.StalledAt &&
		validExecutionStalledLifecycleAuthority(target.LifecycleAuthority) &&
		s.Metadata[executionClaimNudgeLifecycleAuthorityKey] == target.LifecycleAuthority &&
		validExecutionStalledLifecycleAuthority(target.CloseAuthority) &&
		s.Metadata[executionClaimNudgeCloseAuthorityKey] == target.CloseAuthority &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey]) == target.ID &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeRootKey]) == target.RootID &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeStoreRefKey]) == target.StoreRef &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeGenerationKey]) == target.Generation &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeInstanceTokenKey]) == target.InstanceToken &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeAwakeStartedAtKey]) == target.AwakeStartedAt &&
		executionMarkerClaimAuthorityMatches(s, target)
}

func (p poolExecutionBackstop) governs(s beads.Bead) bool {
	return strings.TrimSpace(s.Metadata["pool_managed"]) == "true"
}

// resolve reports an outstanding stall only when all of it holds: the session
// holds exactly ONE in-progress claim under a current identity, and the runtime
// says the session has been quiet for at least the grace window.
//
// Exactly one, because the pacing state names a single bead: a session juggling
// several claims is demonstrably doing something, and picking one of them to
// nudge about would make the persisted marker a lie. Ambiguity holds rather than
// clears, so a transient multi-claim tick cannot reset a window already running.
func (p poolExecutionBackstop) resolve(s beads.Bead, _ map[string]beads.Bead, sessName string) (backstopTarget, backstopResolution) {
	incarnation := executionIncarnationFromBead(s)
	if !executionIncarnationProven(incarnation) {
		return backstopTarget{}, backstopResolutionHold
	}
	// Once an exact claim exhausted its bounded nudge budget, that durable
	// identity — not the cardinality of a later assigned-work snapshot — owns
	// convergence. The session may acquire another claim before a restarted
	// controller reconstructs the drain; find the latched claim directly so
	// ambiguity cannot strand it. If that exact claim is gone, clear the latch:
	// completion or reassignment already achieved the work-side convergence.
	if strings.TrimSpace(s.Metadata[executionClaimNudgeStalledKey]) != "" {
		// Legacy or corrupt latches have no proof of which complete lifecycle
		// earned them. Hold them fail-closed: clearing would rearm both the nudge
		// budget and one-shot event, while reconstructing could stop a successor.
		authority := s.Metadata[executionClaimNudgeLifecycleAuthorityKey]
		if !validExecutionStalledLifecycleAuthority(authority) {
			return backstopTarget{}, backstopResolutionHold
		}
		// ClosePatch is the irreversible boundary: once its terminal state lands,
		// claim completion/reassignment is expected cleanup progress and can no
		// longer retire the latch. The early reconciler finalizer validates the
		// exact permitted lifecycle delta and closes without work revalidation.
		if strings.TrimSpace(s.Metadata["state"]) == executionStalledDrainReason {
			return backstopTarget{}, backstopResolutionHold
		}
		if !executionMarkerIncarnationMatches(s, incarnation) {
			return backstopTarget{}, backstopResolutionClear
		}
		claims := p.claims.forIdentities(currentSessionAssigneeIdentities(s))
		markedID := strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey])
		markedStoreRef := strings.TrimSpace(s.Metadata[executionClaimNudgeStoreRefKey])
		for _, claim := range claims {
			if claim.BeadID != markedID || claim.StoreRef != markedStoreRef {
				continue
			}
			if !executionClaimAuthorityProven(claim) {
				return backstopTarget{}, backstopResolutionHold
			}
			target := executionTarget(claim, incarnation)
			target.LifecycleAuthority = authority
			target.CloseAuthority = s.Metadata[executionClaimNudgeCloseAuthorityKey]
			target.StalledAt = s.Metadata[executionClaimNudgeStalledKey]
			if !executionMarkerClaimAuthorityMatches(s, target) {
				return backstopTarget{}, backstopResolutionClear
			}
			return target, backstopResolutionOutstanding
		}
		return backstopTarget{}, backstopResolutionClear
	}
	claims := p.claims.forIdentities(currentSessionAssigneeIdentities(s))
	switch len(claims) {
	case 0:
		return backstopTarget{}, backstopResolutionClear
	case 1:
		// Continue below.
	default:
		return backstopTarget{}, backstopResolutionHold
	}
	claim := claims[0]
	if !executionClaimAuthorityProven(claim) {
		return backstopTarget{}, backstopResolutionHold
	}
	target := executionTarget(claim, incarnation)
	target.LifecycleAuthority = executionStalledLifecycleAuthority(sessions.InfoFromPersistedBead(s))
	if !validExecutionStalledLifecycleAuthority(target.LifecycleAuthority) {
		return backstopTarget{}, backstopResolutionHold
	}
	if !p.sessionIsQuiet(sessName) {
		return backstopTarget{}, backstopResolutionHold
	}
	return target, backstopResolutionOutstanding
}

// executionStalledClosePendingAuthorityMatches proves that current is exactly
// the preterminal lifecycle captured by the durable fingerprint plus the sole
// lifecycle-field mutation in session.ClosePatch: state=execution-stalled.
// ClosePatch's close_reason/closed_at/synced_at audit keys are intentionally not
// part of drainAckLifecycleVersion. The preterminal predicate admits only the
// same raw states requestExecutionStalledDrain accepted before stopping.
func executionStalledClosePendingAuthorityMatches(s beads.Bead, current sessions.Info) bool {
	if s.Status != "open" || current.Closed ||
		strings.TrimSpace(s.Metadata["pool_managed"]) != "true" || !current.PoolManaged ||
		s.Metadata["state"] != executionStalledDrainReason ||
		current.MetadataState != executionStalledDrainReason {
		return false
	}
	originalAuthority := s.Metadata[executionClaimNudgeLifecycleAuthorityKey]
	closeAuthority := s.Metadata[executionClaimNudgeCloseAuthorityKey]
	if !validExecutionStalledLifecycleAuthority(originalAuthority) ||
		!validExecutionStalledLifecycleAuthority(closeAuthority) ||
		!executionMarkerIncarnationMatches(s, current) {
		return false
	}
	if executionStalledLifecycleAuthority(current) != closeAuthority {
		return false
	}
	// Prove that the stored preterminal authority is the exact lifecycle now
	// persisted, modulo the one state mutation ClosePatch is allowed to make.
	// Merely accepting any syntactically-valid original digest would let a
	// forged/stale marker authorize terminal close of an unrelated lifecycle.
	originalMatched := false
	for _, preterminalState := range []string{"", string(sessions.StateActive), string(sessions.StateAwake)} {
		preterminal := current
		preterminal.MetadataState = preterminalState
		if executionStalledLifecycleAuthority(preterminal) == originalAuthority {
			originalMatched = true
			break
		}
	}
	if !originalMatched {
		return false
	}
	closedAt := s.Metadata["closed_at"]
	parsedClosedAt, err := time.Parse(time.RFC3339, closedAt)
	return err == nil && closedAt != "" &&
		closedAt == parsedClosedAt.UTC().Format(time.RFC3339) &&
		s.Metadata["synced_at"] == closedAt &&
		s.Metadata["close_reason"] == sessions.CanonicalCloseReason(executionStalledDrainReason)
}

// finalizeExecutionStalledClosePending recognizes the crash residue produced
// when ClosePatch commits but the following status close fails. That patch was
// written only after a positively observed stop under the original exact
// session+work guard, so the row is terminal-close-pending: a later claim
// disappearance cannot revoke close authority. The durable lifecycle latch
// fences every managed re-wake while this idempotent close retries.
func finalizeExecutionStalledClosePending(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	info sessions.Info,
	clk clock.Clock,
	stderr io.Writer,
) (handled, closed bool) {
	if !sessions.HasExecutionClaimNudgeStalled(info) {
		return false, false
	}
	handled = true
	if store == nil || sp == nil || strings.TrimSpace(info.MetadataState) != executionStalledDrainReason {
		return handled, false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if clk == nil {
		clk = clock.Real{}
	}
	acquired, _, lockErr := sessions.TryWithCitySessionDestructiveLock(cityPath, info.ID, func() error {
		// The raw audit tuple must come from the cache-bypassing live handle.
		// ClosePatch may have landed in the backing store while an active-row
		// cache still exposes the pre-close snapshot.
		snapshot, err := beads.HandlesFor(store).Live.Get(info.ID)
		if err != nil {
			return nil
		}
		if snapshot.Status == "closed" {
			// Terminal residue: a close that won between fence release and
			// this pass can leave nothing, but a crash after Close and before
			// the owning drain released its stalled fence leaves exactly that.
			// The closed row is inert; clear the exact leftover quietly.
			releaseExecutionStalledFenceResidue(store, snapshot)
			return nil
		}
		latest := sessions.InfoFromPersistedBead(snapshot)
		if !executionStalledClosePendingAuthorityMatches(snapshot, latest) {
			return nil
		}
		running, err := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, latest.ID)
		if err != nil || running {
			return nil
		}
		if err := store.Close(latest.ID); err != nil {
			fmt.Fprintf(stderr, "session reconciler: completing execution-stalled close for %s: %v\n", latest.ID, err) //nolint:errcheck
			return nil
		}
		cancelStateAssignedToRetiredSessionBead(store, latest.ID, clk.Now().UTC(), stderr)
		releaseWorkFromClosedSessionBead(store, snapshot, stderr)
		// The terminal close is committed; the durable shared-store fence has
		// no remaining owner. Its exact value cannot erase a successor lease.
		releaseExecutionStalledFenceResidue(store, snapshot)
		closed = true
		return nil
	})
	if lockErr != nil {
		fmt.Fprintf(stderr, "session reconciler: locking execution-stalled close for %s: %v\n", info.ID, lockErr) //nolint:errcheck
	}
	return handled, acquired && lockErr == nil && closed
}

// releaseExecutionStalledFenceResidue clears the durable shared-store stalled
// fence after a terminal close, or a closed-row leftover from a crash between
// Close and the owning drain's release. It is best-effort by design: a failed
// exact-value release is retried by the next finalizer pass, and a value that
// no longer decodes to this row's stalled fence is not ours to touch.
func releaseExecutionStalledFenceResidue(store beads.Store, snapshot beads.Bead) {
	gate := strings.TrimSpace(snapshot.Metadata[sessions.SessionHookActivityGateMetadataKey])
	if !sessions.IsExecutionStalledActivityFence(gate) {
		return
	}
	_ = sessions.ReleaseExecutionStalledActivityFenceValue(store, snapshot.ID, gate)
}

// sessionIsQuiet reports whether the runtime has observed no activity for at
// least the grace window. An unreadable or unset activity signal is NOT quiet: a
// backstop that treats "unknown" as "idle" nudges working agents, which is
// exactly how the reverted idle-session nudger produced restart storms.
func (p poolExecutionBackstop) sessionIsQuiet(sessName string) bool {
	last, err := p.sp.GetLastActivity(sessName)
	if err != nil || last.IsZero() {
		return false
	}
	return p.now.Sub(last) >= idleClaimNudgeGrace
}

func (p poolExecutionBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	lifecycleSame := strings.TrimSpace(s.Metadata[executionClaimNudgeStalledKey]) == "" ||
		s.Metadata[executionClaimNudgeLifecycleAuthorityKey] == target.LifecycleAuthority
	same = strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey]) == target.ID &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeRootKey]) == target.RootID &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeStoreRefKey]) == target.StoreRef &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeGenerationKey]) == target.Generation &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeInstanceTokenKey]) == target.InstanceToken &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeAwakeStartedAtKey]) == target.AwakeStartedAt &&
		lifecycleSame &&
		executionMarkerClaimAuthorityMatches(s, target)
	return same, atoiOr0(s.Metadata[executionClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[executionClaimNudgeAtKey])
}

func (p poolExecutionBackstop) content(s beads.Bead) string {
	return claimNudgeFor(p.cfg, s)
}

// revalidate re-reads the claim through the owning store's authoritative live
// handle immediately before delivery. Assigned-work snapshots are normally
// CachingStore-backed, so a plain read can still show a claim the agent finished
// seconds ago. An authoritative not-found CLEARS; ambiguous read failures HOLD
// because they are not proof the claim went away.
func (p poolExecutionBackstop) revalidate(target backstopTarget) backstopResolution {
	return revalidateExecutionClaim(target)
}

func revalidateExecutionClaim(target backstopTarget) backstopResolution {
	if !executionTargetAuthorityProven(target) {
		return backstopResolutionHold
	}
	live := beads.HandlesFor(target.Store).Live
	if live == nil {
		return backstopResolutionHold
	}
	current, err := live.Get(target.ID)
	if errors.Is(err, beads.ErrNotFound) {
		return backstopResolutionClear
	}
	if err != nil || current.ID != target.ID {
		return backstopResolutionHold
	}
	if !strings.EqualFold(strings.TrimSpace(current.Status), "in_progress") ||
		strings.TrimSpace(current.Assignee) != target.Assignee {
		return backstopResolutionClear
	}
	if strings.TrimSpace(current.Metadata[beadmeta.RootBeadIDMetadataKey]) != target.RootID {
		return backstopResolutionClear
	}
	if target.WorkRevision != 0 {
		if current.Revision == 0 {
			return backstopResolutionHold
		}
		if current.Revision != target.WorkRevision {
			return backstopResolutionClear
		}
	}
	if target.WorkClaimFence != 0 {
		if current.ClaimFence == 0 {
			return backstopResolutionHold
		}
		if current.ClaimFence != target.WorkClaimFence {
			return backstopResolutionClear
		}
	}
	return backstopResolutionOutstanding
}

func (p poolExecutionBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	p.writeMarkerIfCurrent(store, s, target, 0, now, stdout)
}

func (p poolExecutionBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	// runNudgeBackstop owns the city/session lifecycle lock across this durable
	// reservation and provider delivery. Re-locking here would recurse on the
	// non-reentrant cross-process flock.
	return writeExecutionClaimMarker(store, s, target, attempts, now, stdout)
}

func executionClaimMarkerSnapshotMatches(expected, current beads.Bead) bool {
	if expected.ID != current.ID || expected.Status != current.Status {
		return false
	}
	for _, key := range executionClaimMarkerKeys {
		if expected.Metadata[key] != current.Metadata[key] {
			return false
		}
	}
	return true
}

func (p poolExecutionBackstop) writeMarkerIfCurrent(
	store beads.Store,
	s *beads.Bead,
	target backstopTarget,
	attempts int,
	now time.Time,
	stdout io.Writer,
) bool {
	if store == nil || s == nil {
		return false
	}
	written := false
	acquired, err := sessions.TryWithCitySessionLifecycleLock(p.cityPath, s.ID, func() error {
		current, getErr := beads.HandlesFor(store).Live.Get(s.ID)
		if getErr != nil || current.Status == "closed" ||
			strings.TrimSpace(current.Metadata[executionClaimNudgeStalledKey]) != "" ||
			strings.TrimSpace(current.Metadata["state"]) == executionStalledDrainReason ||
			!executionClaimMarkerSnapshotMatches(*s, current) {
			return nil
		}
		currentInfo := sessions.InfoFromPersistedBead(current)
		if !validExecutionStalledLifecycleAuthority(target.LifecycleAuthority) ||
			executionStalledLifecycleAuthority(currentInfo) != target.LifecycleAuthority {
			return nil
		}
		written = writeExecutionClaimMarker(store, &current, target, attempts, now, stdout)
		if written {
			s.Metadata = current.Metadata
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stdout, "execution-claim-nudge: locking %s before marker write failed: %v\n", s.ID, err) //nolint:errcheck
	}
	return acquired && err == nil && written
}

// exhausted turns a spent attempt budget into one observable fact and a tracked
// drain. The fact is latched exactly once; the idempotent drain request is
// level-triggered so a controller restart can reconstruct its in-memory tracker.
func (p poolExecutionBackstop) exhausted(store beads.Store, s *beads.Bead, stdout io.Writer) {
	sessName := strings.TrimSpace(s.Metadata["session_name"])
	if sessName == "" {
		return
	}
	// Resolve again at the destructive boundary, then live-read the exact claim.
	// The reconciler snapshots passed to the backstop can be stale, and a finished
	// or reassigned claim is convergence, not a reason to recycle its old holder.
	target, resolution := p.resolve(*s, nil, sessName)
	switch resolution {
	case backstopResolutionHold:
		return
	case backstopResolutionClear:
		p.clear(store, s, stdout)
		return
	case backstopResolutionOutstanding:
		// Continue below.
	default:
		return
	}
	if same, _, _ := p.state(*s, target); !same {
		return
	}
	switch p.revalidate(target) {
	case backstopResolutionHold:
		return
	case backstopResolutionClear:
		p.clear(store, s, stdout)
		return
	case backstopResolutionOutstanding:
		// Continue below.
	default:
		return
	}

	if strings.TrimSpace(s.Metadata[executionClaimNudgeStalledKey]) == "" {
		// Capture and latch while holding the same city/session lifecycle lock as
		// every managed wake/start path. Whichever side wins has an unambiguous
		// order: a completed wake changes the fingerprint before this write, while
		// a completed latch is visible to the start path's in-lock re-read.
		latched := false
		acquired, _, lockErr := sessions.TryWithCitySessionDestructiveLock(p.cityPath, s.ID, func() error {
			liveSnapshot, err := beads.HandlesFor(store).Live.Get(s.ID)
			if err != nil {
				return nil
			}
			info := sessions.InfoFromPersistedBead(liveSnapshot)
			clearCurrent := func() {
				if liveSnapshot.Status == "closed" ||
					strings.TrimSpace(liveSnapshot.Metadata["state"]) == executionStalledDrainReason ||
					!executionClaimMarkerSnapshotMatches(*s, liveSnapshot) {
					return
				}
				if clearExecutionClaimMarker(store, &liveSnapshot, stdout) {
					s.Metadata = liveSnapshot.Metadata
				}
			}
			if !executionIncarnationProven(info) ||
				!p.governs(liveSnapshot) ||
				!sameProvenDrainAckIncarnation(sessions.Info{
					Generation:     target.Generation,
					InstanceToken:  target.InstanceToken,
					AwakeStartedAt: target.AwakeStartedAt,
				}, info) ||
				!executionSessionLifecycleDrainable(info) ||
				!executionAssigneeIsCurrent(info, target.Assignee) {
				clearCurrent()
				return nil
			}
			// The snapshot that exhausted the budget is the authority. A wake,
			// restart, or other lifecycle mutation that won the lock first must
			// retire this attempt, never be silently adopted as a new baseline.
			if !validExecutionStalledLifecycleAuthority(target.LifecycleAuthority) ||
				executionStalledLifecycleAuthority(info) != target.LifecycleAuthority {
				clearCurrent()
				return nil
			}
			// Provider activity and attachment do not mutate the lifecycle row.
			// Recheck both inside the serialized latch boundary so an active user
			// or message delivery cannot be stopped on a stale quiet observation.
			if !p.sessionIsQuiet(sessName) || p.sp.IsAttached(sessName) {
				return nil
			}
			expectedClose := info
			expectedClose.MetadataState = executionStalledDrainReason
			closeAuthority := executionStalledLifecycleAuthority(expectedClose)
			if !validExecutionStalledLifecycleAuthority(closeAuthority) {
				return nil
			}
			// Install the shared-store stalled fence before the durable latch.
			// Remote hooks (K8s/SSH) share Dolt but not this host's lifecycle
			// flock, so the CAS gate — not the flock — orders hook output
			// against the terminal fence. A blocked acquire defers the latch to
			// a later level-triggered tick rather than stealing the window.
			if !p.installExecutionStalledActivityFence(store, info, target) {
				return nil
			}
			// Latch FIRST. A failed event write must not leave the observable
			// escalation armed to repeat on every subsequent tick. Drain failures
			// are different: the durable latch makes their retry safe and necessary.
			latched = writeSessionMetadata(store, s, map[string]string{
				executionClaimNudgeStalledKey:            p.now.UTC().Format(time.RFC3339),
				executionClaimNudgeLifecycleAuthorityKey: target.LifecycleAuthority,
				executionClaimNudgeCloseAuthorityKey:     closeAuthority,
			}, "execution-claim-nudge", stdout)
			if latched {
				target.CloseAuthority = closeAuthority
				target.StalledAt = s.Metadata[executionClaimNudgeStalledKey]
			}
			return nil
		})
		if lockErr != nil {
			fmt.Fprintf(stdout, "execution-claim-nudge: locking %s before stalled latch failed: %v\n", sessName, lockErr) //nolint:errcheck // best-effort
		}
		if !acquired || lockErr != nil || !latched {
			return
		}
		p.emitStepStalled(s, target.ID, atoiOr0(s.Metadata[executionClaimNudgeCountKey]))
		fmt.Fprintf(stdout, //nolint:errcheck // best-effort
			"execution-claim-nudge: %s still holds %s unexecuted after %d attempts; draining (%s)\n",
			sessName, target.ID, idleClaimNudgeMaxAttempts, executionStalledDrainReason)
	}
	if p.requestDrain == nil {
		return
	}
	// The drain is the CONVERGENCE step, not a notification. Nothing else will
	// release this claim: the session is alive and awake, so no crash lane
	// touches it, and it holds in_progress work, so the wake machinery keeps it
	// alive by design. The tracked drain is what turns "we gave up nudging" into
	// stop -> close -> dead-assignee reopen -> the row is claimable again.
	resolution, err := p.requestDrain(*s, target)
	if err != nil {
		fmt.Fprintf(stdout, "execution-claim-nudge: draining %s failed: %v\n", sessName, err) //nolint:errcheck // best-effort
		return
	}
	switch resolution {
	case backstopResolutionClear:
		p.clear(store, s, stdout)
	case backstopResolutionHold, backstopResolutionOutstanding:
		// A hold is retried from the durable latch; outstanding means the exact
		// drain authority was accepted (possibly as an idempotent re-enqueue).
	}
}

// installExecutionStalledActivityFence installs (or adopts) the durable
// shared-store fence for one exact session/work authority inside the caller's
// destructive-lock boundary. A blocked acquire is resolved only through
// provably dead windows: a released hook tombstone whose seat is already quiet
// past the idle grace may be acknowledged, and a stalled fence left behind by
// an aborted attempt whose durable latch never landed may be cleared. Anything
// else — an active remote hook lease, a foreign stalled fence, an incomplete
// authority — defers to a later tick without writing.
func (p poolExecutionBackstop) installExecutionStalledActivityFence(store beads.Store, info sessions.Info, target backstopTarget) bool {
	coordinates := sessions.ExecutionStalledActivityFenceCoordinates{
		HookActivityCoordinates: sessions.HookActivityCoordinates{
			SessionID:         info.ID,
			Generation:        info.Generation,
			ContinuationEpoch: info.ContinuationEpoch,
			InstanceToken:     info.InstanceToken,
		},
		AwakeStartedAt:     target.AwakeStartedAt,
		LifecycleAuthority: target.LifecycleAuthority,
		WorkID:             target.ID,
		WorkStoreRef:       target.StoreRef,
		WorkRevision:       target.WorkRevision,
		WorkClaimFence:     target.WorkClaimFence,
		Assignee:           target.Assignee,
	}
	for attempt := 0; ; attempt++ {
		_, current, err := sessions.AcquireExecutionStalledActivityFence(store, coordinates)
		if err == nil {
			return true
		}
		if !errors.Is(err, sessions.ErrSessionHookActivityBlocked) || attempt > 0 {
			return false
		}
		gate := strings.TrimSpace(current.SessionHookActivityGate)
		// Window: a hook lease its owner already released. The quiet check
		// above this boundary already proved no provider activity for the
		// whole grace window, which is the documented released-tombstone
		// boundary; elapsed time alone never clears an active lease. Any other
		// occupant — an active hook lease, or a stalled fence for a different
		// authority — is not ours to clear: our own same-authority leftover is
		// adopted by Acquire without reaching this branch.
		if sessions.IsReleasedSessionHookActivityTombstone(gate) {
			if _, ackErr := sessions.AcknowledgeSessionHookActivityAfterProviderBoundary(store, current.ID, gate); ackErr == nil {
				continue
			}
		}
		return false
	}
}

func (p poolExecutionBackstop) emitStepStalled(s *beads.Bead, beadID string, attempts int) {
	if p.rec == nil {
		return
	}
	rootID := strings.TrimSpace(s.Metadata[executionClaimNudgeRootKey])
	payload, err := json.Marshal(events.ExecutionStepStalledPayload{
		BeadID:     beadID,
		RootBeadID: rootID,
		SessionID:  s.ID,
		Attempts:   attempts,
	})
	if err != nil {
		return
	}
	p.rec.Record(events.Event{
		Type:      events.ExecutionStepStalled,
		Actor:     eventActor(),
		Subject:   beadID,
		RunID:     rootID,
		SessionID: s.ID,
		Payload:   payload,
	})
}

func (p poolExecutionBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) {
	if store == nil || s == nil {
		return
	}
	_, err := sessions.TryWithCitySessionLifecycleLock(p.cityPath, s.ID, func() error {
		current, getErr := beads.HandlesFor(store).Live.Get(s.ID)
		if getErr != nil || current.Status == "closed" ||
			strings.TrimSpace(current.Metadata["state"]) == executionStalledDrainReason ||
			!executionClaimMarkerSnapshotMatches(*s, current) {
			return nil
		}
		if clearExecutionClaimMarker(store, &current, stdout) {
			s.Metadata = current.Metadata
		}
		// A preterminal clear retires the whole stall regime, including a
		// durable shared-store fence installed by an attempt whose latch never
		// landed (crash window) or whose claim resolved. The exact-value
		// release cannot erase a successor lease installed after this read.
		releaseExecutionStalledFenceResidue(store, current)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stdout, "execution-claim-nudge: locking %s before marker clear failed: %v\n", s.ID, err) //nolint:errcheck
	}
}

func writeExecutionClaimMarker(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeSessionMetadata(store, s, map[string]string{
		executionClaimNudgeWorkKey:           target.ID,
		executionClaimNudgeRootKey:           target.RootID,
		executionClaimNudgeStoreRefKey:       target.StoreRef,
		executionClaimNudgeGenerationKey:     target.Generation,
		executionClaimNudgeInstanceTokenKey:  target.InstanceToken,
		executionClaimNudgeAwakeStartedAtKey: target.AwakeStartedAt,
		executionClaimNudgeAssigneeKey:       target.Assignee,
		executionClaimNudgeRevisionKey:       strconv.FormatInt(target.WorkRevision, 10),
		executionClaimNudgeClaimFenceKey:     strconv.FormatInt(target.WorkClaimFence, 10),
		executionClaimNudgeCountKey:          strconv.Itoa(attempts),
		executionClaimNudgeAtKey:             now.UTC().Format(time.RFC3339),
		// A directly replaced assignment may never produce a no-claim tick that
		// calls clear. Observing/reserving the new target must still give it a
		// fresh one-shot escalation budget.
		executionClaimNudgeStalledKey:            "",
		executionClaimNudgeLifecycleAuthorityKey: "",
		executionClaimNudgeCloseAuthorityKey:     "",
	}, "execution-claim-nudge", stdout)
}

// clearExecutionClaimMarker wipes the state machine — including the escalation
// latch — so the next claim this slot takes starts a fresh window. No-op when
// there is nothing to clear, so steady-state ticks stay write-free.
func clearExecutionClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) bool {
	dirty := false
	for _, key := range executionClaimMarkerKeys {
		if s.Metadata[key] != "" {
			dirty = true
			break
		}
	}
	if !dirty {
		return false
	}
	kvs := make(map[string]string, len(executionClaimMarkerKeys))
	for _, key := range executionClaimMarkerKeys {
		kvs[key] = ""
	}
	if !writeSessionMetadata(store, s, kvs, "execution-claim-nudge", stdout) {
		return false
	}
	for _, key := range executionClaimMarkerKeys {
		delete(s.Metadata, key)
	}
	return true
}

// writeSessionMetadata persists a marker patch and mirrors it into the in-memory
// session bead so the rest of this tick reads the just-written values.
func writeSessionMetadata(store beads.Store, s *beads.Bead, kvs map[string]string, label string, stdout io.Writer) bool {
	if err := store.Update(s.ID, beads.UpdateOpts{Metadata: kvs}); err != nil {
		fmt.Fprintf(stdout, "%s: marking %s failed: %v\n", label, s.ID, err) //nolint:errcheck // best-effort
		return false
	}
	if s.Metadata == nil {
		s.Metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		s.Metadata[k] = v
	}
	return true
}

// requestExecutionStalledDrain begins a TRACKED drain of a seat that claimed
// work and never executed it.
//
// Tracked, not a bare runtime flag: the drainTracker is the machinery that
// actually converges a session — it defers the interrupt one tick so a
// false positive can still be canceled, then advances through stop, close, and
// the dead-assignee reopen that puts the claim back in the demand set. Setting
// the runtime's GC_DRAIN meta alone announces an intention that nothing drives.
//
// The reason is executionStalledDrainReason precisely because this session looks
// exactly like one every keep-alive guard exists to protect (awake, running,
// holding an in_progress claim); a cancelable reason would be canceled by the
// very claim that justified the drain.
//
// It re-reads the session through the front door rather than trusting the
// backstop's snapshot, and binds the tracker to both the exact session
// incarnation and exact claim revision/fence. The same guard is retained on the
// drain state and rerun immediately around every later stop/close action. This is
// the narrowest safe answer to the cross-store TOCTOU: the stores cannot share a
// transaction, so each destructive boundary must independently re-prove both
// sides and retire on a positive mismatch.
func (cr *CityRuntime) requestExecutionStalledDrain(sessionBead beads.Bead, target backstopTarget) (backstopResolution, error) {
	if cr == nil || cr.sessionDrains == nil {
		return backstopResolutionHold, fmt.Errorf("no drain tracker configured for %q", sessionBead.ID)
	}
	sessionStore := cr.sessionsBeadStore().Store
	if sessionStore == nil {
		return backstopResolutionHold, fmt.Errorf("session store unavailable while draining %q", sessionBead.ID)
	}
	sessFront := sessionFrontDoor(sessionStore)
	raw, err := beads.HandlesFor(sessionStore).Live.Get(sessionBead.ID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) || errors.Is(err, sessions.ErrSessionNotFound) {
			return backstopResolutionClear, nil
		}
		return backstopResolutionHold, fmt.Errorf("reading session %q before draining: %w", sessionBead.ID, err)
	}
	info := sessions.InfoFromPersistedBead(raw)
	if strings.TrimSpace(raw.Metadata[executionClaimNudgeStalledKey]) == "" {
		return backstopResolutionClear, nil
	}
	if strings.TrimSpace(raw.Metadata["pool_managed"]) != "true" || !info.PoolManaged {
		return backstopResolutionClear, nil
	}
	if !validExecutionStalledLifecycleAuthority(raw.Metadata[executionClaimNudgeLifecycleAuthorityKey]) ||
		!validExecutionStalledLifecycleAuthority(raw.Metadata[executionClaimNudgeCloseAuthorityKey]) {
		return backstopResolutionHold, nil
	}
	if !executionDurableMarkerMatchesTarget(raw, target) {
		return backstopResolutionClear, nil
	}
	if !validExecutionStalledLifecycleAuthority(target.LifecycleAuthority) {
		return backstopResolutionHold, nil
	}
	if executionStalledLifecycleAuthority(info) != target.LifecycleAuthority {
		return backstopResolutionClear, nil
	}
	markerIncarnation := sessions.Info{
		Generation:     target.Generation,
		InstanceToken:  target.InstanceToken,
		AwakeStartedAt: target.AwakeStartedAt,
	}
	if !executionIncarnationProven(markerIncarnation) {
		return backstopResolutionHold, nil
	}
	if !executionIncarnationProven(info) || !sameProvenDrainAckIncarnation(markerIncarnation, info) {
		return backstopResolutionClear, nil
	}

	guard := executionStalledDrainActionGuard(cr.cityPath, sessionStore, sessFront, info, target)
	resolution, err := guard(func(latest sessions.Info) error {
		beginSessionDrainInfoWithActionGuard(
			latest,
			cr.sessionDrains,
			executionStalledDrainReason,
			clock.Real{},
			defaultDrainTimeout,
			guard,
		)
		return nil
	})
	return resolution, err
}

// executionStalledDrainActionGuard serializes with every managed lifecycle
// transition through the city/session lock, refreshes the authoritative live
// session Info, and live-reads the exact work row before invoking action. A busy
// lifecycle holds without blocking the controller. A cross-store claim writer
// cannot join this lock, so callers retain and rerun this guard at each action
// boundary rather than treating one successful check as a lease.
func executionStalledDrainActionGuard(
	cityPath string,
	store beads.Store,
	sessFront *sessions.Store,
	expected sessions.Info,
	target backstopTarget,
) drainActionGuard {
	return func(action func(sessions.Info) error) (resolution backstopResolution, err error) {
		resolution = backstopResolutionHold
		if store == nil || sessFront == nil || strings.TrimSpace(expected.ID) == "" {
			return resolution, errors.New("execution-stalled lifecycle store unavailable")
		}
		acquired, _, lockErr := sessions.TryWithCitySessionDestructiveLock(cityPath, expected.ID, func() error {
			raw, getErr := beads.HandlesFor(store).Live.Get(expected.ID)
			if getErr != nil {
				if errors.Is(getErr, beads.ErrNotFound) || errors.Is(getErr, sessions.ErrSessionNotFound) {
					resolution = backstopResolutionClear
					return nil
				}
				return fmt.Errorf("reading live session %q at drain boundary: %w", expected.ID, getErr)
			}
			latest := sessions.InfoFromPersistedBead(raw)
			if strings.TrimSpace(raw.Metadata[executionClaimNudgeStalledKey]) == "" {
				resolution = backstopResolutionClear
				return nil
			}
			if strings.TrimSpace(raw.Metadata["pool_managed"]) != "true" || !latest.PoolManaged {
				resolution = backstopResolutionClear
				return nil
			}
			if !validExecutionStalledLifecycleAuthority(raw.Metadata[executionClaimNudgeLifecycleAuthorityKey]) ||
				!validExecutionStalledLifecycleAuthority(raw.Metadata[executionClaimNudgeCloseAuthorityKey]) {
				resolution = backstopResolutionHold
				return nil
			}
			if !executionDurableMarkerMatchesTarget(raw, target) {
				resolution = backstopResolutionClear
				return nil
			}
			if executionStalledLifecycleAuthority(latest) != target.LifecycleAuthority {
				resolution = backstopResolutionClear
				return nil
			}
			if drainAckVersionOf(latest) != drainAckVersionOf(expected) {
				resolution = backstopResolutionClear
				return nil
			}
			if !executionIncarnationProven(latest) || !sameProvenDrainAckIncarnation(expected, latest) {
				resolution = backstopResolutionClear
				return nil
			}
			// Suspend, close, restart, and other lifecycle transitions may retain
			// generation/token/awake coordinates. They supersede this backstop;
			// never overwrite their terminal or in-flight state with asleep.
			if !executionSessionLifecycleDrainable(latest) {
				resolution = backstopResolutionClear
				return nil
			}
			// Alias history is intentionally excluded: only an identity the latest
			// session currently answers to may authorize a stop. Alias rotation can
			// happen without changing the runtime-incarnation coordinates.
			if !executionAssigneeIsCurrent(latest, target.Assignee) {
				resolution = backstopResolutionClear
				return nil
			}
			resolution = revalidateExecutionClaim(target)
			if resolution != backstopResolutionOutstanding || action == nil {
				return nil
			}
			return action(latest)
		})
		if lockErr != nil {
			return resolution, lockErr
		}
		if !acquired {
			return backstopResolutionHold, nil
		}
		return resolution, nil
	}
}
