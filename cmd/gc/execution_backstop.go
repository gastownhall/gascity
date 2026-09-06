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
// The escalation is latched on the session bead so it fires once per stalled
// claim, not once per tick.

import (
	"encoding/json"
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
	// executionClaimNudgeStalledKey latches the one-shot escalation so the
	// typed event and the drain request fire once per stalled claim rather than
	// once per tick for as long as the claim is held.
	executionClaimNudgeStalledKey = "execution_claim_nudge_stalled"
)

// nudgeStalledPoolExecution re-delivers the configured claim nudge to a pool slot
// that HOLDS an in-progress claim it never started executing, and escalates once
// the bounded attempts are spent.
//
// work/workStores/workStoreRefs are the reconciler's index-aligned assigned-work
// snapshot; requestDrain is the existing drain request (drainOps.setDrain), taken
// as a function so this file needs no reconciler wiring of its own. A partial
// snapshot is not evidence of a stall, so it disables the predicate for that tick.
func nudgeStalledPoolExecution(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	work []beads.Bead,
	workStores []beads.Store,
	workStoreRefs []string,
	snapshotPartial bool,
	now time.Time,
	rec events.Recorder,
	requestDrain func(sessionBead beads.Bead) error,
	stdout io.Writer,
) {
	if sp == nil || cfg == nil || store == nil || snapshotPartial {
		return // hot reconcile path: never panic on a half-built dependency
	}
	if sess, ok := store.(beads.SessionStore); ok && sess.Store == nil {
		return
	}
	runNudgeBackstop(sp, store, sessionBeads, nil, now, stdout, "execution-claim-nudge", poolExecutionBackstop{
		cfg:          cfg,
		sp:           sp,
		now:          now,
		rec:          rec,
		requestDrain: requestDrain,
		claims:       newExecutionClaimSnapshot(work, workStores, workStoreRefs),
	})
}

// executionClaim is one in-progress claim from the assigned-work snapshot, kept
// with the store handle its live re-read must use.
type executionClaim struct {
	BeadID   string
	RootID   string
	StoreRef string
	Assignee string
	Store    beads.Store
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
			BeadID:   wb.ID,
			RootID:   strings.TrimSpace(wb.Metadata[beadmeta.RootBeadIDMetadataKey]),
			Assignee: assignee,
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
	requestDrain func(sessionBead beads.Bead) error
	claims       executionClaimSnapshot
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
	target := backstopTarget{
		ID:       claim.BeadID,
		RootID:   claim.RootID,
		StoreRef: claim.StoreRef,
		Assignee: claim.Assignee,
		Store:    claim.Store,
	}
	if p.sessionIsQuiet(sessName) {
		return target, backstopResolutionOutstanding
	}
	if p.sessionShowsRetryableOverload(sessName) {
		return target, backstopResolutionImmediate
	}
	return backstopTarget{}, backstopResolutionHold
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

// sessionShowsRetryableOverload distinguishes recent activity caused by the
// provider rejecting a turn from recent activity caused by the agent working.
// Peek failures are uncertainty and therefore hold through the ordinary path.
func (p poolExecutionBackstop) sessionShowsRetryableOverload(sessName string) bool {
	content, err := p.sp.Peek(sessName, rateLimitPeekLines)
	return err == nil && runtime.ContainsProviderRetryableOverloadScreen(content)
}

func (p poolExecutionBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	same = strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey]) == target.ID &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeStoreRefKey]) == target.StoreRef
	return same, atoiOr0(s.Metadata[executionClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[executionClaimNudgeAtKey])
}

func (p poolExecutionBackstop) content(s beads.Bead) string {
	return claimNudgeFor(p.cfg, s)
}

// revalidate re-reads the claim through the owning store's authoritative live
// handle immediately before delivery. Assigned-work snapshots are normally
// CachingStore-backed, so a plain read can still show a claim the agent finished
// seconds ago. A failed read HOLDS: it is not proof the claim went away.
func (p poolExecutionBackstop) revalidate(target backstopTarget) backstopResolution {
	if target.Store == nil {
		return backstopResolutionHold
	}
	live := beads.HandlesFor(target.Store).Live
	if live == nil {
		return backstopResolutionHold
	}
	current, err := live.Get(target.ID)
	if err != nil || current.ID != target.ID {
		return backstopResolutionHold
	}
	if !strings.EqualFold(strings.TrimSpace(current.Status), "in_progress") ||
		strings.TrimSpace(current.Assignee) != target.Assignee {
		return backstopResolutionClear
	}
	return backstopResolutionOutstanding
}

func (p poolExecutionBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	writeExecutionClaimMarker(store, s, target, 0, now, stdout)
}

func (p poolExecutionBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeExecutionClaimMarker(store, s, target, attempts, now, stdout)
}

// exhausted turns a spent attempt budget into one observable fact and one drain
// request, latched so both happen exactly once for this claim however many ticks
// the session survives.
func (p poolExecutionBackstop) exhausted(store beads.Store, s *beads.Bead, stdout io.Writer) {
	if strings.TrimSpace(s.Metadata[executionClaimNudgeStalledKey]) != "" {
		return
	}
	beadID := strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey])
	if beadID == "" {
		return
	}
	sessName := strings.TrimSpace(s.Metadata["session_name"])
	// Latch FIRST. A failed event write or a failed drain must not leave the
	// escalation armed to repeat on every subsequent tick; the marker itself is
	// the durable record that this claim was escalated, and the operator sees the
	// failure on stdout.
	if !writeSessionMetadata(store, s, map[string]string{
		executionClaimNudgeStalledKey: p.now.UTC().Format(time.RFC3339),
	}, "execution-claim-nudge", stdout) {
		return
	}
	// Report the attempts actually DELIVERED, not the cap. An agent with no
	// configured nudge escalates at 0/3 having never been contacted, and an
	// operator reading "after 3 attempts" there would go looking for three
	// nudges that were never sent.
	attempts := atoiOr0(s.Metadata[executionClaimNudgeCountKey])
	p.emitStepStalled(s, beadID, attempts)
	fmt.Fprintf(stdout, //nolint:errcheck // best-effort
		"execution-claim-nudge: %s still holds %s unexecuted after %d/%d nudge attempts; draining (%s)\n",
		sessName, beadID, attempts, idleClaimNudgeMaxAttempts, executionStalledDrainReason)
	if p.requestDrain == nil || sessName == "" {
		return
	}
	// The drain is the CONVERGENCE step, not a notification. Nothing else will
	// release this claim: the session is alive and awake, so no crash lane
	// touches it, and it holds in_progress work, so the wake machinery keeps it
	// alive by design. The tracked drain is what turns "we gave up nudging" into
	// stop -> close -> dead-assignee reopen -> the row is claimable again.
	if err := p.requestDrain(*s); err != nil {
		fmt.Fprintf(stdout, "execution-claim-nudge: draining %s failed: %v\n", sessName, err) //nolint:errcheck // best-effort
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
	clearExecutionClaimMarker(store, s, stdout)
}

func writeExecutionClaimMarker(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeSessionMetadata(store, s, map[string]string{
		executionClaimNudgeWorkKey:     target.ID,
		executionClaimNudgeRootKey:     target.RootID,
		executionClaimNudgeStoreRefKey: target.StoreRef,
		executionClaimNudgeCountKey:    strconv.Itoa(attempts),
		executionClaimNudgeAtKey:       now.UTC().Format(time.RFC3339),
	}, "execution-claim-nudge", stdout)
}

// clearExecutionClaimMarker wipes the state machine — including the escalation
// latch — so the next claim this slot takes starts a fresh window. No-op when
// there is nothing to clear, so steady-state ticks stay write-free.
func clearExecutionClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) {
	keys := []string{
		executionClaimNudgeWorkKey,
		executionClaimNudgeRootKey,
		executionClaimNudgeStoreRefKey,
		executionClaimNudgeCountKey,
		executionClaimNudgeAtKey,
		executionClaimNudgeStalledKey,
	}
	dirty := false
	for _, key := range keys {
		if s.Metadata[key] != "" {
			dirty = true
			break
		}
	}
	if !dirty {
		return
	}
	kvs := make(map[string]string, len(keys))
	for _, key := range keys {
		kvs[key] = ""
	}
	if !writeSessionMetadata(store, s, kvs, "execution-claim-nudge", stdout) {
		return
	}
	for _, key := range keys {
		delete(s.Metadata, key)
	}
}

// writeSessionMetadata persists a marker patch and mirrors it into the in-memory
// session bead so the rest of this tick reads the just-written values.
func writeSessionMetadata(store beads.Store, s *beads.Bead, kvs map[string]string, label string, stdout io.Writer) bool {
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
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
// backstop's snapshot: the drain carries the session's generation, and acting on
// a stale generation is how a drain lands on the wrong incarnation.
func (cr *CityRuntime) requestExecutionStalledDrain(sessionBead beads.Bead) error {
	if cr == nil || cr.sessionDrains == nil {
		return fmt.Errorf("no drain tracker configured for %q", sessionBead.ID)
	}
	info, err := sessionFrontDoor(cr.sessionsBeadStore()).Get(sessionBead.ID)
	if err != nil {
		return fmt.Errorf("reading session %q before draining: %w", sessionBead.ID, err)
	}
	beginSessionDrainInfo(info, cr.sp, cr.sessionDrains, executionStalledDrainReason, clock.Real{}, defaultDrainTimeout)
	return nil
}
