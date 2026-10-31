package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Session-bead metadata keys for the stalled-claim backstop. The state machine
// is PERSISTED on the pool slot's own session bead so it survives a controller
// restart — the in-memory grace map of the reverted #312 nudger did not, which
// is precisely why that one re-nudge-stormed on every restart (test-5il).
const (
	idleClaimNudgeTriggerKey = "idle_claim_nudge_trigger" // trigger bead id last acted on
	idleClaimNudgeCountKey   = "idle_claim_nudge_count"   // delivery attempts reserved for that trigger
	idleClaimNudgeAtKey      = "idle_claim_nudge_at"      // RFC3339 of last attempt / first observation
	idleClaimNudgeTokenKey   = "idle_claim_nudge_token"   // the incarnation (instance_token) the marker paces
	// The terminal drain's latch (exhausted): when it was requested and for
	// which incarnation, so it is requested once per incarnation.
	idleClaimNudgeStalledKey      = "idle_claim_nudge_stalled"
	idleClaimNudgeStalledTokenKey = "idle_claim_nudge_stalled_token"
	// idleClaimNudgeHoldKey is the claim backstop's hold breadcrumb
	// (observeClaimHold).
	idleClaimNudgeHoldKey = "idle_claim_nudge_hold"
)

// Session-bead metadata keys for the post-step continuation-claim backstop.
// Work, root, store, and pool generation are persisted separately so a
// recycled graph root, same-ID bead in another store, or recycled session
// process always starts a fresh grace window.
const (
	continuationClaimNudgeWorkKey       = "continuation_claim_nudge_work"
	continuationClaimNudgeRootKey       = "continuation_claim_nudge_root"
	continuationClaimNudgeStoreRefKey   = "continuation_claim_nudge_store_ref"
	continuationClaimNudgeGenerationKey = "continuation_claim_nudge_generation"
	continuationClaimNudgeCountKey      = "continuation_claim_nudge_count"
	continuationClaimNudgeAtKey         = "continuation_claim_nudge_at"
	// continuationClaimNudgeHoldKey is the continuation backstop's hold
	// breadcrumb (observeClaimHold).
	continuationClaimNudgeHoldKey = "continuation_claim_nudge_hold"
)

// Backstop pacing. Deliberately slow: this only rescues a pool slot that was
// handed work but never began it, so a couple of minutes of latency is fine and
// keeps the backstop nowhere near anything that could read as churn.
//
// idleClaimNudgeGrace is also a cross-package floor: a pane-owning adapter that
// is silent for the whole grace reads as a stalled seat and gets drained, so
// such adapters redraw their busy line on an interval strictly below it (the
// zcode adapter beats every 30 s by default, operator-overridable via
// ZCODE_REPL_HEARTBEAT_SECS; see internal/worker/adapters/zcode/zcode-repl).
// Keep this grace above every adapter's heartbeat interval — lowering it below
// one re-introduces the drain regression that heartbeat exists to fix.
const (
	idleClaimNudgeGrace       = 90 * time.Second // observe-before-first-nudge; lets a normal claim land
	idleClaimNudgeBackoff     = 3 * time.Minute  // between retries when a delivered nudge didn't take
	idleClaimNudgeMaxAttempts = 3                // then give up and log (manual re-nudge remains)
	// idleClaimNudgeBusyHoldLimit bounds a run of holds on a nudge the claim
	// backstops find their slot too busy for (boundedBusyHold): past it the
	// nudge goes anyway, and the runtime's own turn guard decides. It is about
	// the backstop's whole nudge budget (the grace and its backoffs), well past
	// a claim and its startup turn.
	idleClaimNudgeBusyHoldLimit = 10 * time.Minute
)

const defaultPoolClaimNudge = "Run gc hook --claim --drain-ack --json now; if it returns work, execute it immediately."

// defaultHeldClaimNudge is the default for a seat whose claim (the %s) has
// started its step, the execution backstop's started seat: PHASE1-2 §6.6's
// started-rung text. It carries no --drain-ack, which on a hook answer of no
// work, the started step gate- or hold-blocked, acked a drain and gave the
// claim back (mc-ilr3b).
const defaultHeldClaimNudge = "You hold %s and its step has started. Continue it now (run gc hook --claim --json to re-read your assignment). If you are waiting on something, say what, then stop."

// idleClaimNudgeLabel and continuationClaimNudgeLabel name the claim and
// continuation backstops in their log lines and their planner notices.
const (
	idleClaimNudgeLabel         = "idle-claim-nudge"
	continuationClaimNudgeLabel = "continuation-claim-nudge"
)

// nudgeStalledPoolClaims is a reconcile-tick backstop that runs for every
// runtime (herdr AND tmux). It re-delivers the claim nudge to a pool slot that
// is running but whose assigned trigger bead is still UNCLAIMED (open, not
// in_progress). The startup nudge can be missed — a freshly-spawned slot whose
// submit-CR was swallowed, or a warm slot that survived a `gc restart` and was
// never re-Started — leaving the worker session idle at its prompt with work it never
// began. tmux's relaunch/respawn path only heals a session that DIED; a live
// idle slot needs this demand-driven wake exactly as herdr does (activity
// reporting makes the controller SEE the slot but never nudges it to claim).
//
// This keys on the slot's own gc.trigger_bead_id. The work snapshot includes
// both actionable assigned work and ready, routed, unassigned work selected as
// concrete default pool demand, so a warm slot rebound after its startup turn
// is still visible here without widening the predicate to blocked open work.
//
// Churn-free by construction — it inverts every failure mode that got the #312
// idle-session nudger reverted:
//   - Keys on bead state (trigger bead == open), never "idle for N minutes", so
//     it is structurally invisible to a working agent: the instant a pool slot
//     claims, its trigger bead flips to in_progress and stops matching. The
//     tick's work snapshot can predate that flip — on a slow store by minutes
//     (factory-lifecycle oct14b: a slot told to re-claim the step it had been
//     executing for two minutes) — so the trigger is re-read live before every
//     delivery (poolClaimBackstop.revalidate).
//   - Waits for a quiet slot: a nudge typed into a running turn can abort it,
//     so delivery also waits until the runtime shows the slot quiet
//     (claimNudgeBusyHold). A slot that is busy without having claimed is most
//     often running the claim itself. The wait is bounded (boundedBusyHold):
//     past it the runtime's own turn guard decides.
//   - State is persisted on the session bead, so a restart cannot replay it.
//   - Bounded per assignment: observe (grace) → nudge → backoff retries → give
//     up. It never spams a tick and never loops forever.
//   - Pool slots only.
//
// This is a thin predicate wrapper (poolClaimBackstop) over the shared
// grace→nudge→backoff→give-up engine in nudge_backstop.go; the pacing,
// looping, and delivery mechanics live there so continuation delivery can
// reuse them without duplicating this state machine.
//
// When the bounded nudges are spent, or there is no nudge to send, and the
// slot's own trigger still reads open and unclaimed, the slot is drained and
// recycled (poolClaimBackstop.exhausted) through drain, the execution
// backstop's non-cancelable drain: an agent that cannot answer a claim nudge
// (a provider refusing every turn) would otherwise hold its pool seat forever.
func nudgeStalledPoolClaims(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	claimWork []beads.Bead,
	claimWorkStoreRefs []string,
	now time.Time,
	drain poolClaimDrain,
	stdout io.Writer,
) {
	if sp == nil || cfg == nil || store == nil {
		return // hot reconcile path: never panic on a half-built dependency
	}
	// beads.SessionStore embeds the Store interface, so a wrapper holding a nil
	// store is still a non-nil beads.Store. Unwrap it so the half-built
	// dependency guard also covers the SessionStore the reconciler passes.
	if sess, ok := store.(beads.SessionStore); ok && sess.Store == nil {
		return
	}
	// The shared engine keys work by bead ID alone, which cannot tell two
	// same-ID beads in different stores apart, so this predicate carries its own
	// store-scoped snapshot and leaves the engine's ID map empty.
	runNudgeBackstop(sp, store, sessionBeads, nil, now, stdout, idleClaimNudgeLabel, poolClaimBackstop{
		cfg:    cfg,
		work:   newIdleClaimWorkSnapshot(claimWork, claimWorkStoreRefs),
		now:    now,
		sp:     sp,
		drain:  drain,
		stdout: stdout,
	})
}

// poolClaimDrain is what the claim backstop needs from its caller for the
// decisions a snapshot must not make (mc-zndi7.80): readTrigger, the slot's
// trigger read live from the store that owns it by residency (found false on
// a clean miss); and request, the drain request. readTrigger gates every
// nudge delivery as well as the terminal drain, so nil readTrigger holds both;
// nil request disables only the drain.
type poolClaimDrain struct {
	readTrigger func(id string) (bead beads.Bead, found bool, err error)
	request     func(sessionBead beads.Bead) error
}

// poolClaimDrainOver is the terminal drain over legs, the tick's work legs,
// requesting through request.
func poolClaimDrainOver(legs WorkLegs, request func(beads.Bead) error) poolClaimDrain {
	return poolClaimDrain{readTrigger: legs.readLive, request: request}
}

// nudgeStalledPoolContinuations is the later-stage complement to the
// hook-claim continuation nudge: after a pool worker completes one graph-v2
// step, the control dispatcher can make exactly one preassigned successor
// ready without a new root claim. If the provider ends its turn instead of
// running gc hook --claim again, this persisted backstop re-delivers the
// configured claim nudge after the shared grace window.
//
// Candidate qualification proves ready/open state and exact graph root/store
// provenance in build_desired_state.go. This final lane re-resolves the
// candidate's assignee against CURRENT session identities, requires exactly
// one candidate for one running pool session, and re-reads the step and root
// immediately before reserving delivery. Any incomplete or ambiguous evidence
// is silent and write-free.
func nudgeStalledPoolContinuations(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	candidates []ContinuationClaimCandidate,
	snapshotPartial bool,
	now time.Time,
	stdout io.Writer,
) {
	if sp == nil || cfg == nil || store == nil || snapshotPartial {
		return
	}
	if sess, ok := store.(beads.SessionStore); ok && sess.Store == nil {
		return
	}
	runNudgeBackstop(
		sp,
		store,
		sessionBeads,
		nil,
		now,
		stdout,
		continuationClaimNudgeLabel,
		poolContinuationBackstop{
			cfg:        cfg,
			candidates: newPoolContinuationCandidateSnapshot(sessionBeads, candidates),
			sp:         sp,
			now:        now,
			stdout:     stdout,
		},
	)
}

// poolClaimBackstop is the backstopPredicate for pool-managed slots: it
// re-delivers the claim nudge to a slot whose assigned trigger bead is still
// unclaimed. See nudgeStalledPoolClaims for the full rationale and scope.
type poolClaimBackstop struct {
	cfg   *config.City
	work  idleClaimWorkSnapshot
	now   time.Time
	sp    runtime.Provider
	drain poolClaimDrain
	// stdout logs a nudge that goes past a busy hold (boundedBusyHold).
	stdout io.Writer
}

// poolContinuationBackstop is the backstopPredicate for a graph-v2 successor
// preassigned to a live pool session after that session completed the preceding
// step. The snapshot is keyed by the session bead's durable ID, not by a
// mutable alias or runtime name.
type poolContinuationBackstop struct {
	cfg        *config.City
	candidates poolContinuationCandidateSnapshot
	sp         runtime.Provider
	now        time.Time
	// stdout logs a nudge that goes past a busy hold (boundedBusyHold).
	stdout io.Writer
}

func (p poolContinuationBackstop) governs(s beads.Bead) bool {
	return strings.TrimSpace(s.Metadata["pool_managed"]) == "true"
}

func (p poolContinuationBackstop) resolve(s beads.Bead, _ map[string]beads.Bead, _ string) (backstopTarget, backstopResolution) {
	if p.candidates.holdBySessionID[s.ID] {
		return backstopTarget{}, backstopResolutionHold
	}
	generation := strings.TrimSpace(s.Metadata["generation"])
	if generation == "" {
		return backstopTarget{}, backstopResolutionHold
	}
	candidates := p.candidates.bySessionID[s.ID]
	switch len(candidates) {
	case 0:
		return backstopTarget{}, backstopResolutionClear
	case 1:
		// Continue below.
	default:
		return backstopTarget{}, backstopResolutionHold
	}
	candidate := candidates[0]
	return backstopTarget{
		ID:         candidate.WorkBeadID,
		RootID:     candidate.RootBeadID,
		StoreRef:   candidate.StoreRef,
		Generation: generation,
		Assignee:   candidate.Assignee,
		Store:      candidate.Store,
	}, backstopResolutionOutstanding
}

func (p poolContinuationBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	same = strings.TrimSpace(s.Metadata[continuationClaimNudgeWorkKey]) == target.ID &&
		strings.TrimSpace(s.Metadata[continuationClaimNudgeRootKey]) == target.RootID &&
		strings.TrimSpace(s.Metadata[continuationClaimNudgeStoreRefKey]) == target.StoreRef &&
		strings.TrimSpace(s.Metadata[continuationClaimNudgeGenerationKey]) == target.Generation
	return same, atoiOr0(s.Metadata[continuationClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[continuationClaimNudgeAtKey])
}

func (p poolContinuationBackstop) content(s beads.Bead) string {
	return claimNudgeFor(p.cfg, s)
}

func (p poolContinuationBackstop) revalidate(s beads.Bead, sessName string, target backstopTarget) (backstopResolution, string) {
	// The runtime look comes first: it is the cheaper read, and a busy session
	// holds whatever the store says, for as long as boundedBusyHold lets it.
	if p.sp == nil {
		return backstopResolutionHold, ""
	}
	hold, overdue := boundedBusyHold(p.sp, s, sessName, continuationClaimNudgeHoldKey, p.now)
	if hold != "" {
		return backstopResolutionHold, hold
	}
	res, _ := revalidateContinuation(target)
	if res == backstopResolutionOutstanding {
		logOverdueBusyHold(p.stdout, continuationClaimNudgeLabel, s, sessName, continuationClaimNudgeHoldKey, overdue)
	}
	return res, ""
}

// observeHold implements backstopHoldObserver with the continuation
// backstop's breadcrumb (observeClaimHold).
func (p poolContinuationBackstop) observeHold(store beads.Store, s *beads.Bead, reason string, stdout io.Writer) {
	observeClaimHold(store, s, continuationClaimNudgeHoldKey, reason, p.now, continuationClaimNudgeLabel, stdout)
}

// clearHold implements backstopHoldObserver (clearClaimHold).
func (p poolContinuationBackstop) clearHold(store beads.Store, s *beads.Bead, stdout io.Writer) {
	clearClaimHold(store, s, continuationClaimNudgeHoldKey, continuationClaimNudgeLabel, stdout)
}

// revalidateContinuation is revalidate's verdict on target, with the step as
// the live read found it (the zero bead when it held).
func revalidateContinuation(target backstopTarget) (backstopResolution, beads.Bead) {
	if target.Store == nil {
		return backstopResolutionHold, beads.Bead{}
	}
	// Assigned-work snapshots normally carry a CachingStore. A plain Get can
	// therefore return the pre-claim row after another process has already
	// claimed it, so both revalidation reads must go through the live handle
	// rather than the snapshot's cached view. That handle belongs to
	// target.Store — the leg the row was read from, which is not necessarily
	// the scope target.StoreRef names (see the owner note below). planClass
	// (internal/storeref/resolve.go) is the PLACEMENT contract, not a residency
	// one: `gc storage migrate` preserves ids and never deletes back
	// (cmd/gc/census_residency.go), so a relocated row stays co-resident in the
	// work ledger beside its binding. Both legs canonicalize to city:<name>, so
	// the copies share a group, and — sameContinuationClaimCandidate not
	// comparing Store — the fold keeps the first leg in census order, which is
	// the work ledger. This guard therefore re-reads that leg, and on a
	// pre-relocation residue it can still pass on a stale row. Pre-existing and
	// unchanged by the owner-ref split; tracked with the rest of the
	// leg-vs-owner grouping work in ga-m4sj2.
	live := beads.HandlesFor(target.Store).Live
	if live == nil {
		return backstopResolutionHold, beads.Bead{}
	}
	current, err := live.Get(target.ID)
	if err != nil || current.ID != target.ID {
		return backstopResolutionHold, beads.Bead{}
	}
	// target.StoreRef is the OWNER scope selectReadyContinuationClaimCandidates
	// proved for this row, not the leg it was read from: inside a class binding a
	// rig-scoped workflow's steps carry gc.root_store_ref=rig:<name> (ga-erfca).
	// Both re-reads below compare against that owner, so this mirror of the
	// evaluator cannot disqualify a row the evaluator admitted.
	if !strings.EqualFold(strings.TrimSpace(current.Status), "open") ||
		!strings.EqualFold(strings.TrimSpace(current.Type), "task") ||
		strings.TrimSpace(current.Assignee) != target.Assignee ||
		strings.TrimSpace(current.Metadata[beadmeta.RootBeadIDMetadataKey]) != target.RootID ||
		strings.TrimSpace(current.Metadata[beadmeta.RootStoreRefMetadataKey]) != target.StoreRef ||
		strings.TrimSpace(current.Metadata[beadmeta.ContinuationGroupMetadataKey]) == "" ||
		strings.TrimSpace(current.Metadata[beadmeta.SessionAffinityMetadataKey]) != "require" {
		return backstopResolutionClear, current
	}
	root, err := live.Get(target.RootID)
	if err != nil || root.ID != target.RootID {
		return backstopResolutionHold, beads.Bead{}
	}
	// Mirrors evaluateReadyContinuationClaimCandidate: the root proves the run is
	// live, not who owns this step. gc.session_name on the root is a dashboard
	// stamp, last-writer-wins across the molecule's steps, so requiring it to
	// equal target.Assignee is unsatisfiable for any formula that routes steps to
	// more than one agent template. current.Assignee above is the authoritative
	// pin.
	if !strings.EqualFold(strings.TrimSpace(root.Status), "in_progress") ||
		!strings.EqualFold(strings.TrimSpace(root.Type), "task") ||
		strings.TrimSpace(root.Metadata[beadmeta.RootStoreRefMetadataKey]) != target.StoreRef ||
		strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]) != "graph.v2" ||
		strings.TrimSpace(root.Metadata[beadmeta.KindMetadataKey]) != "workflow" {
		return backstopResolutionClear, current
	}
	return backstopResolutionOutstanding, current
}

func (p poolContinuationBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	writeContinuationClaimMarker(store, s, target, 0, now, stdout)
}

func (p poolContinuationBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeContinuationClaimMarker(store, s, target, attempts, now, stdout)
}

func (p poolContinuationBackstop) exhausted(_ beads.Store, _ *beads.Bead, _ io.Writer) {
}

func (p poolContinuationBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) bool {
	return clearContinuationClaimMarker(store, s, stdout)
}

type poolContinuationCandidateSnapshot struct {
	bySessionID     map[string][]ContinuationClaimCandidate
	holdBySessionID map[string]bool
}

type continuationCandidateIdentity struct {
	WorkBeadID string
	RootBeadID string
	StoreRef   string
	Assignee   string
}

func newPoolContinuationCandidateSnapshot(
	sessionBeads []beads.Bead,
	candidates []ContinuationClaimCandidate,
) poolContinuationCandidateSnapshot {
	snapshot := poolContinuationCandidateSnapshot{
		bySessionID:     make(map[string][]ContinuationClaimCandidate),
		holdBySessionID: make(map[string]bool),
	}
	if len(sessionBeads) == 0 || len(candidates) == 0 {
		return snapshot
	}

	identityOwners := make(map[string]map[string]struct{})
	for _, sessionBead := range sessionBeads {
		if strings.EqualFold(strings.TrimSpace(sessionBead.Status), "closed") ||
			!isSessionBead(sessionBead) ||
			strings.TrimSpace(sessionBead.ID) == "" {
			continue
		}
		for _, identity := range currentSessionAssigneeIdentities(sessionBead) {
			if identityOwners[identity] == nil {
				identityOwners[identity] = make(map[string]struct{})
			}
			identityOwners[identity][sessionBead.ID] = struct{}{}
		}
	}

	seen := make(map[string]map[continuationCandidateIdentity]struct{})
	for _, candidate := range candidates {
		assignee := strings.TrimSpace(candidate.Assignee)
		owners := identityOwners[assignee]
		if len(owners) == 0 {
			continue
		}
		if len(owners) != 1 {
			for sessionID := range owners {
				snapshot.holdBySessionID[sessionID] = true
			}
			continue
		}
		sessionID := ""
		for owner := range owners {
			sessionID = owner
		}
		if strings.TrimSpace(candidate.WorkBeadID) == "" ||
			strings.TrimSpace(candidate.RootBeadID) == "" ||
			strings.TrimSpace(candidate.StoreRef) == "" ||
			candidate.Store == nil {
			snapshot.holdBySessionID[sessionID] = true
			continue
		}
		identity := continuationCandidateIdentity{
			WorkBeadID: candidate.WorkBeadID,
			RootBeadID: candidate.RootBeadID,
			StoreRef:   candidate.StoreRef,
			Assignee:   candidate.Assignee,
		}
		if seen[sessionID] == nil {
			seen[sessionID] = make(map[continuationCandidateIdentity]struct{})
		}
		if _, duplicate := seen[sessionID][identity]; duplicate {
			continue
		}
		seen[sessionID][identity] = struct{}{}
		snapshot.bySessionID[sessionID] = append(snapshot.bySessionID[sessionID], candidate)
		if len(snapshot.bySessionID[sessionID]) > 1 {
			snapshot.holdBySessionID[sessionID] = true
		}
	}
	return snapshot
}

// currentSessionAssigneeIdentities excludes alias_history deliberately. A
// historical alias is useful for orphan recovery but is not a CURRENT identity
// that may authorize a new claim nudge.
func currentSessionAssigneeIdentities(sessionBead beads.Bead) []string {
	values := []string{
		sessionBead.ID,
		sessionBead.Metadata["session_name"],
		sessionBead.Metadata["configured_named_identity"],
		sessionBead.Metadata["alias"],
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (p poolClaimBackstop) governs(s beads.Bead) bool {
	return strings.TrimSpace(s.Metadata["pool_managed"]) == "true"
}

// outstandingID acts only while the trigger bead is genuinely unclaimed. A
// claimed bead is in_progress (or closed) — either way the slot is doing its
// job and must not be disturbed. If the bead is absent from the work snapshot
// it's been claimed/closed/moved.
//
// The engine's ID-keyed map is ignored: resolution goes through the
// store-scoped snapshot so a slot bound to a rig bead is matched against that
// rig's copy, not a same-ID bead in another store.
func (p poolClaimBackstop) resolve(s beads.Bead, _ map[string]beads.Bead, _ string) (backstopTarget, backstopResolution) {
	triggerID := strings.TrimSpace(s.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	if triggerID == "" {
		return backstopTarget{}, backstopResolutionClear
	}
	w, ok := p.work.lookup(triggerID, s.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey])
	if !ok || !isUnclaimedTrigger(p.cfg, w) {
		return backstopTarget{}, backstopResolutionClear
	}
	return backstopTarget{ID: triggerID}, backstopResolutionOutstanding
}

// state's pacing is the current incarnation's: a marker or a latch another
// incarnation left reads as a new assignment, so each incarnation gets its
// own grace and nudges before its own drain, and a slot re-woken for the same
// trigger is not drained on sight, whether or not its predecessor's drain
// was latched.
func (p poolClaimBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	token := strings.TrimSpace(s.Metadata["instance_token"])
	if strings.TrimSpace(s.Metadata[idleClaimNudgeTokenKey]) != token {
		return false, 0, time.Time{}
	}
	if tok := strings.TrimSpace(s.Metadata[idleClaimNudgeStalledTokenKey]); tok != "" && tok != token {
		return false, 0, time.Time{}
	}
	marked := strings.TrimSpace(s.Metadata[idleClaimNudgeTriggerKey])
	return marked == target.ID, atoiOr0(s.Metadata[idleClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[idleClaimNudgeAtKey])
}

func (p poolClaimBackstop) content(s beads.Bead) string {
	return stalledPoolClaimNudgeFor(p.cfg, s)
}

// revalidate confirms, immediately before an attempt is reserved, what resolve
// read from the tick's snapshot. That snapshot is the planner's work view and
// can be well behind the store: on a contended store a slot that claimed its
// trigger minutes ago can still read open there. So, in order:
//
//   - The slot must be quiet (claimNudgeBusyHold): a nudge typed into a running
//     turn can abort it, and a slot that is busy without having claimed is
//     most often running the claim. A busy slot holds, and the hold names its
//     gate; a run of holds is bounded (boundedBusyHold).
//   - Its trigger, re-read live through the store that owns it by residency
//     (drain.readTrigger, the read the terminal drain uses), must still be
//     open and unclaimed. Claimed since the snapshot, by this slot or any
//     other session, clears; a failed read, a clean miss, or no reader at all
//     holds, since none of them proves the trigger still waits.
//
// A hold spends no attempt, so a slot that really stalled is nudged on the
// first tick it is quiet and its trigger reads open. The live read costs one
// owner resolution and one Live Get per would-be delivery, never per tick.
func (p poolClaimBackstop) revalidate(s beads.Bead, sessName string, target backstopTarget) (backstopResolution, string) {
	if p.sp == nil {
		return backstopResolutionHold, ""
	}
	hold, overdue := boundedBusyHold(p.sp, s, sessName, idleClaimNudgeHoldKey, p.now)
	if hold != "" {
		return backstopResolutionHold, hold
	}
	if p.drain.readTrigger == nil {
		return backstopResolutionHold, ""
	}
	trigger, found, err := p.drain.readTrigger(target.ID)
	switch {
	case err != nil, !found:
		return backstopResolutionHold, ""
	case !isUnclaimedTrigger(p.cfg, trigger):
		return backstopResolutionClear, ""
	default:
		logOverdueBusyHold(p.stdout, idleClaimNudgeLabel, s, sessName, idleClaimNudgeHoldKey, overdue)
		return backstopResolutionOutstanding, ""
	}
}

// observeHold implements backstopHoldObserver with the claim backstop's
// breadcrumb (observeClaimHold).
func (p poolClaimBackstop) observeHold(store beads.Store, s *beads.Bead, reason string, stdout io.Writer) {
	observeClaimHold(store, s, idleClaimNudgeHoldKey, reason, p.now, idleClaimNudgeLabel, stdout)
}

// clearHold implements backstopHoldObserver (clearClaimHold).
func (p poolClaimBackstop) clearHold(store beads.Store, s *beads.Bead, stdout io.Writer) {
	clearClaimHold(store, s, idleClaimNudgeHoldKey, idleClaimNudgeLabel, stdout)
}

// claimNudgeBusyHold names what keeps a claim nudge from being typed into
// sp's session sessName (its row s) at now, or returns "" when nothing does.
// Every runtime delivers a nudge by typing into the session, and the tmux
// provider's own wait for idle sends anyway once its timeout passes; for Codex
// the submit sequence then leads with Escape, which aborts the running turn.
// So the claim backstops wait for quiet themselves:
//
//   - activity the runtime observed within idleClaimNudgeGrace holds, as does
//     an activity read that fails;
//   - for a busy-readable provider family off ACP (executionSnapshotVetoApplies)
//     a turn the screen shows running holds, as does a look that fails
//     (turnHoldFor).
//
// Unlike sessionQuietHold, a runtime that reports no activity at all (zero
// time) does not hold by itself: holding on it would leave a slot on an
// activity-blind runtime that really missed its claim with no nudge, ever.
// There the live trigger read is the remaining guard. The execution backstop,
// whose in-progress claim is exactly what a working agent looks like, keeps
// the stricter rule.
func claimNudgeBusyHold(sp runtime.Provider, s beads.Bead, sessName string, now time.Time) string {
	last, err := sp.GetLastActivity(sessName)
	if err != nil || (!last.IsZero() && now.Sub(last) < idleClaimNudgeGrace) {
		return executionHoldNotQuiet
	}
	if !executionSnapshotVetoApplies(s.Metadata) {
		return ""
	}
	return turnHoldFor(sp, sessName)
}

// claimHoldTurnRunning names the live turn reading (turnHoldFor) holding a
// nudge: the screen shows a turn running now.
const claimHoldTurnRunning = "turn_running"

// turnHoldFor asks the backend serving sp's session sessName whether its agent
// is running a turn now, the reading its own nudge takes before it types
// (runtime.TurnRunningProvider): a running turn holds (claimHoldTurnRunning),
// as does a look that fails (executionHoldSnapshotError). That reading counts
// only the live screen where it can place the turn's status (a Codex
// composer's status rows), so a busy row an earlier turn left in the
// scrollback holds nothing. A backend that takes no such reading answers with
// its idle snapshot's busy reading (snapshotHoldFor).
func turnHoldFor(sp runtime.Provider, sessName string) string {
	tr, ok := runtime.TurnRunningFor(sp, sessName)
	if !ok {
		return snapshotHoldFor(sp, sessName)
	}
	running, err := tr.TurnRunning(sessName)
	switch {
	case err != nil:
		return executionHoldSnapshotError
	case running:
		return claimHoldTurnRunning
	default:
		return ""
	}
}

// boundedBusyHold is claimNudgeBusyHold bounded by s's hold breadcrumb key:
// the gate that holds a nudge to sp's session sessName (its row s) at now, or
// "" when none does. A run of holds that has stood for
// idleClaimNudgeBusyHoldLimit (observeClaimHold keeps when it began) holds no
// longer: a screen that reads busy for good, a frozen pane or an indicator the
// reading cannot tell from a live one, would otherwise hold the slot's nudges,
// and with them its terminal drain, forever. The nudge then goes, the
// runtime's own turn guard deciding: a running turn its submit would
// interrupt defers it (runtime.ErrSessionBusy, refunded), and the next tick's
// hold starts a new run. overdue names the gate the nudge goes past, "" when
// none.
func boundedBusyHold(sp runtime.Provider, s beads.Bead, sessName, key string, now time.Time) (hold, overdue string) {
	busy := claimNudgeBusyHold(sp, s, sessName, now)
	if busy == "" {
		return "", ""
	}
	if _, since := parseClaimHold(s.Metadata[key]); !since.IsZero() && now.Sub(since) >= idleClaimNudgeBusyHoldLimit {
		return "", busy
	}
	return busy, ""
}

// logOverdueBusyHold logs the backstop behind label nudging sp's session
// sessName (its row s) past the busy gate overdue (boundedBusyHold) that has
// held it since its hold breadcrumb key says. It logs nothing when overdue is
// "".
func logOverdueBusyHold(stdout io.Writer, label string, s beads.Bead, sessName, key, overdue string) {
	if overdue == "" || stdout == nil {
		return
	}
	_, since := parseClaimHold(s.Metadata[key])
	fmt.Fprintf(stdout, "%s: %s has read busy (%s) since %s; nudging anyway, its runtime's turn guard decides\n", //nolint:errcheck // best-effort
		label, sessName, overdue, since.UTC().Format(time.RFC3339))
}

// observeClaimHold records on s's hold breadcrumb key the busy gate
// (claimNudgeBusyHold's) that held the claim backstop behind label, as
// "<reason> <RFC3339 time>": the gate that held it on its latest evaluation,
// and when the run of holds it belongs to began (now, for a run's first
// hold). It writes only when the reason changes, and keeps that time, so a
// standing hold costs one write and the run's age survives a gate that
// changes (boundedBusyHold bounds it). The engine clears the breadcrumb
// wherever the backstop advances. A hold on a read that could not settle the
// target (backstopHoldRevalidate) writes nothing, as these backstops' every
// hold on incomplete evidence does, and leaves a standing breadcrumb as it
// is. A session whose runtime is not running is no hold to bound: a standing
// breadcrumb is cleared, and nothing is written for a slot merely asleep.
func observeClaimHold(store beads.Store, s *beads.Bead, key, reason string, now time.Time, label string, stdout io.Writer) {
	if store == nil || s == nil {
		return
	}
	switch reason {
	case "", backstopHoldRevalidate:
		return
	case backstopHoldRuntimeNotRunning:
		clearClaimHold(store, s, key, label, stdout)
		return
	}
	held, since := parseClaimHold(s.Metadata[key])
	if held == reason {
		return
	}
	if since.IsZero() {
		since = now
	}
	writeSessionMetadata(store, s, claimHoldPatch(key, reason+" "+since.UTC().Format(time.RFC3339)), label, stdout)
}

// clearClaimHold removes s's hold breadcrumb key, so a reason never outlives
// the run of holds it named. No-op when there is nothing to clear.
func clearClaimHold(store beads.Store, s *beads.Bead, key, label string, stdout io.Writer) {
	if store == nil || s == nil || strings.TrimSpace(s.Metadata[key]) == "" {
		return
	}
	writeSessionMetadata(store, s, claimHoldPatch(key, ""), label, stdout)
}

// claimHoldPatch is the patch that sets the hold breadcrumb key, one of
// idleClaimNudgeHoldKey and continuationClaimNudgeHoldKey, to value: the
// breadcrumb observeClaimHold records, or "" to clear it (clearClaimHold).
// It is the keys' writer site in the session field registry.
func claimHoldPatch(key, value string) map[string]string {
	return map[string]string{key: value}
}

// parseClaimHold reads a hold breadcrumb (observeClaimHold): its reason and
// since when, the zero time when either is missing or unreadable.
func parseClaimHold(v string) (string, time.Time) {
	reason, at, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok {
		return reason, time.Time{}
	}
	return reason, parseRFC3339OrZero(at)
}

func (p poolClaimBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	// A new assignment, or a new incarnation, starts without the old latch.
	clearIdleClaimLatch(store, s, stdout)
	writeIdleClaimMarker(store, s, target.ID, 0, now, stdout)
}

func (p poolClaimBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeIdleClaimMarker(store, s, target.ID, attempts, now, stdout)
}

// exhausted drains and recycles a slot that never claimed its own trigger
// (mc-zndi7.80): its nudges are spent, or there is no nudge to send (its
// template resolves to no agent). The engine calls it only while that trigger
// reads open and unclaimed, and exhausted proves both again just before it
// latches: the slot is quiet (sessionQuietHold, the execution backstop's
// gate, so a busy agent with a slow first claim is never drained), and its
// trigger, re-read live through its store, is still open and unassigned.
// Without it the slot holds its pool seat forever:
// it is alive and awake, so no crash lane touches it, and its trigger is a
// wake reason, so every cancelable drain would be canceled. So it takes the
// execution backstop's drain (executionStalledDrainReason), which no lens
// cancels: stop, close, and the trigger is demand for a fresh slot. The
// request is latched per incarnation (stalledLatchHolds) before it is made,
// and the attempt marker is left at the cap, so the next ticks stay silent.
func (p poolClaimBackstop) exhausted(store beads.Store, s *beads.Bead, stdout io.Writer) {
	if stalledLatchHolds(s, idleClaimNudgeStalledKey, idleClaimNudgeStalledTokenKey, p.now) {
		return
	}
	sessName := strings.TrimSpace(s.Metadata["session_name"])
	if p.drain.readTrigger == nil || p.drain.request == nil || sessName == "" || p.sp == nil {
		return
	}
	if sessionQuietHold(p.sp, *s, sessName, p.now, turnHoldFor) != "" {
		return // not proven quiet: hold, the next tick looks again
	}
	trigger, found, err := p.drain.readTrigger(s.Metadata[idleClaimNudgeTriggerKey])
	if err != nil || !found || !isUnclaimedTrigger(p.cfg, trigger) {
		return // claimed since the snapshot, or unreadable: never drain on stale evidence
	}
	if !writeSessionMetadata(store, s, map[string]string{
		idleClaimNudgeStalledKey:      p.now.UTC().Format(time.RFC3339),
		idleClaimNudgeStalledTokenKey: strings.TrimSpace(s.Metadata["instance_token"]),
	}, idleClaimNudgeLabel, stdout) {
		return
	}
	fmt.Fprintf(stdout, //nolint:errcheck // best-effort
		"idle-claim-nudge: %s never claimed %s after %d/%d nudge attempts; draining (%s)\n",
		sessName, strings.TrimSpace(s.Metadata[idleClaimNudgeTriggerKey]), atoiOr0(s.Metadata[idleClaimNudgeCountKey]), idleClaimNudgeMaxAttempts, executionStalledDrainReason)
	if err := p.drain.request(*s); err != nil {
		fmt.Fprintf(stdout, "idle-claim-nudge: draining %s failed: %v\n", sessName, err) //nolint:errcheck // best-effort
	}
}

// clear wipes the claim backstop's pacing marker, and withdraws the slot's
// warm-bind claim notice (withdrawWarmBindNotice) whether or not there was a
// marker to wipe: the backstop clears once the slot's trigger is claimed,
// gone or unbound, which leaves that notice nothing to claim, and a trigger
// claimed before the backstop first saw it left no marker.
func (p poolClaimBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) bool {
	wiped := clearIdleClaimMarker(store, s, stdout)
	withdrawWarmBindNotice(store, s, "the claim backstop found no work outstanding", stdout)
	return wiped
}

type idleClaimWorkSnapshot struct {
	byScope map[storeScopedBeadKey]beads.Bead
	byID    map[string][]storeScopedBeadKey
}

func newIdleClaimWorkSnapshot(work []beads.Bead, storeRefs []string) idleClaimWorkSnapshot {
	snapshot := idleClaimWorkSnapshot{
		byScope: make(map[storeScopedBeadKey]beads.Bead, len(work)),
		byID:    make(map[string][]storeScopedBeadKey, len(work)),
	}
	for i, bead := range work {
		storeRef := ""
		if i < len(storeRefs) {
			storeRef = normalizeIdleClaimStoreRef(storeRefs[i])
		}
		key := storeScopedBeadKey{StoreRef: storeRef, ID: bead.ID}
		if _, exists := snapshot.byScope[key]; !exists {
			snapshot.byID[bead.ID] = append(snapshot.byID[bead.ID], key)
		}
		snapshot.byScope[key] = bead
	}
	return snapshot
}

func (s idleClaimWorkSnapshot) lookup(id, storeRef string) (beads.Bead, bool) {
	id = strings.TrimSpace(id)
	storeRef = strings.TrimSpace(storeRef)
	if id == "" {
		return beads.Bead{}, false
	}
	if storeRef != "" {
		bead, ok := s.byScope[storeScopedBeadKey{StoreRef: normalizeIdleClaimStoreRef(storeRef), ID: id}]
		return bead, ok
	}
	keys := s.byID[id]
	if len(keys) != 1 {
		return beads.Bead{}, false
	}
	bead, ok := s.byScope[keys[0]]
	return bead, ok
}

func normalizeIdleClaimStoreRef(storeRef string) string {
	storeRef = strings.TrimSpace(storeRef)
	switch {
	case storeRef == "", storeRef == "city", strings.HasPrefix(storeRef, "city:"):
		return "city"
	// A class binding is city scope: it is the same store the leading arm used
	// to record under the city ref, now named as a leg of its own.
	case storeref.IsClassRef(storeRef):
		return "city"
	case strings.HasPrefix(storeRef, "rig:"):
		return "rig:" + strings.TrimSpace(strings.TrimPrefix(storeRef, "rig:"))
	case !strings.Contains(storeRef, ":"):
		// AssignedWorkStoreRefs uses a bare rig name; ready-routed refs are
		// already canonical.
		return "rig:" + storeRef
	default:
		return storeRef
	}
}

// isUnclaimedTrigger reports whether the pool slot's trigger bead is still
// waiting to be claimed: open, and assigned to no session. An assignee that
// is a session claims it: this slot under any of its identities (session
// name, bead ID, alias: the claim mid-flight, or a continuation pin), or
// another session, whose work is no reason to nudge or drain this slot. An
// assignee that is a pool template of cfg (template-assigned pool demand,
// isKnownPoolTemplate) claims nothing: it is any of the pool's slots' to
// take.
func isUnclaimedTrigger(cfg *config.City, w beads.Bead) bool {
	if !strings.EqualFold(strings.TrimSpace(w.Status), "open") {
		return false // in_progress / closed / blocked → not ours to nudge
	}
	assignee := strings.TrimSpace(w.Assignee)
	return assignee == "" || isKnownPoolTemplate(assignee, cfg)
}

// claimNudgeFor resolves the slot's configured startup nudge (the worker's
// `gc hook --claim` line) from the agent template behind this session bead.
func claimNudgeFor(cfg *config.City, session beads.Bead) string {
	nudge, _ := configuredClaimNudgeFor(cfg, session)
	return nudge
}

func stalledPoolClaimNudgeFor(cfg *config.City, session beads.Bead) string {
	nudge, known := configuredClaimNudgeFor(cfg, session)
	if !known {
		return ""
	}
	if nudge == "" {
		return defaultPoolClaimNudge
	}
	return nudge
}

// heldClaimNudgeFor is stalledPoolClaimNudgeFor for a seat whose claim beadID
// has started: its configured nudge, else defaultHeldClaimNudge naming the
// claim, and "" when its template names no configured agent. A configured
// nudge that carries --drain-ack is not sent to a started seat either: the
// default stands in for it, since an ack on the nudge's no-work answer is how
// started work was lost (mc-ilr3b), and the hook's fence holds only a claim
// whose start was recorded.
func heldClaimNudgeFor(cfg *config.City, session beads.Bead, beadID string) string {
	nudge, known := configuredClaimNudgeFor(cfg, session)
	if known && (nudge == "" || strings.Contains(nudge, "--drain-ack")) {
		return fmt.Sprintf(defaultHeldClaimNudge, strings.TrimSpace(beadID))
	}
	return nudge
}

func configuredClaimNudgeFor(cfg *config.City, session beads.Bead) (string, bool) {
	template := normalizedSessionTemplate(session, cfg)
	if template == "" {
		return "", false
	}
	agent := findAgentByTemplate(cfg, template)
	if agent == nil {
		return "", false
	}
	return strings.TrimSpace(agent.Nudge), true
}

// writeIdleClaimMarker persists the backstop state machine onto the session
// bead and mirrors it into the in-memory snapshot so the rest of this tick
// reads the just-written values.
func writeIdleClaimMarker(store beads.Store, s *beads.Bead, triggerID string, attempts int, now time.Time, stdout io.Writer) bool {
	kvs := map[string]string{
		idleClaimNudgeTriggerKey: triggerID,
		idleClaimNudgeCountKey:   strconv.Itoa(attempts),
		idleClaimNudgeAtKey:      now.UTC().Format(time.RFC3339),
		idleClaimNudgeTokenKey:   strings.TrimSpace(s.Metadata["instance_token"]),
	}
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "idle-claim-nudge: marking %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
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

// clearIdleClaimLatch clears the terminal drain's latch from s, when set.
func clearIdleClaimLatch(store beads.Store, s *beads.Bead, stdout io.Writer) {
	if s.Metadata[idleClaimNudgeStalledKey] == "" && s.Metadata[idleClaimNudgeStalledTokenKey] == "" {
		return
	}
	writeSessionMetadata(store, s, map[string]string{idleClaimNudgeStalledKey: "", idleClaimNudgeStalledTokenKey: ""}, idleClaimNudgeLabel, stdout)
}

// clearIdleClaimMarker wipes the marker once the slot no longer has unclaimed
// work, so the next assignment starts its grace clock fresh, and reports
// whether it wiped one. No-op (no store write) when there is nothing to clear,
// so steady-state ticks stay silent.
func clearIdleClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) bool {
	if s.Metadata[idleClaimNudgeTriggerKey] == "" &&
		s.Metadata[idleClaimNudgeCountKey] == "" &&
		s.Metadata[idleClaimNudgeAtKey] == "" &&
		s.Metadata[idleClaimNudgeTokenKey] == "" &&
		s.Metadata[idleClaimNudgeStalledKey] == "" &&
		s.Metadata[idleClaimNudgeStalledTokenKey] == "" {
		return false
	}
	kvs := map[string]string{
		idleClaimNudgeTriggerKey:      "",
		idleClaimNudgeCountKey:        "",
		idleClaimNudgeAtKey:           "",
		idleClaimNudgeTokenKey:        "",
		idleClaimNudgeStalledKey:      "",
		idleClaimNudgeStalledTokenKey: "",
	}
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "idle-claim-nudge: clearing %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
		return false
	}
	for k := range kvs {
		delete(s.Metadata, k)
	}
	return true
}

func writeContinuationClaimMarker(
	store beads.Store,
	s *beads.Bead,
	target backstopTarget,
	attempts int,
	now time.Time,
	stdout io.Writer,
) bool {
	kvs := map[string]string{
		continuationClaimNudgeWorkKey:       target.ID,
		continuationClaimNudgeRootKey:       target.RootID,
		continuationClaimNudgeStoreRefKey:   target.StoreRef,
		continuationClaimNudgeGenerationKey: target.Generation,
		continuationClaimNudgeCountKey:      strconv.Itoa(attempts),
		continuationClaimNudgeAtKey:         now.UTC().Format(time.RFC3339),
	}
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "continuation-claim-nudge: marking %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
		return false
	}
	if s.Metadata == nil {
		s.Metadata = make(map[string]string, len(kvs))
	}
	for key, value := range kvs {
		s.Metadata[key] = value
	}
	return true
}

// clearContinuationClaimMarker wipes the continuation marker and reports
// whether it wiped one. No-op (no store write) when there is nothing to clear.
func clearContinuationClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) bool {
	if s.Metadata[continuationClaimNudgeWorkKey] == "" &&
		s.Metadata[continuationClaimNudgeRootKey] == "" &&
		s.Metadata[continuationClaimNudgeStoreRefKey] == "" &&
		s.Metadata[continuationClaimNudgeGenerationKey] == "" &&
		s.Metadata[continuationClaimNudgeCountKey] == "" &&
		s.Metadata[continuationClaimNudgeAtKey] == "" {
		return false
	}
	kvs := map[string]string{
		continuationClaimNudgeWorkKey:       "",
		continuationClaimNudgeRootKey:       "",
		continuationClaimNudgeStoreRefKey:   "",
		continuationClaimNudgeGenerationKey: "",
		continuationClaimNudgeCountKey:      "",
		continuationClaimNudgeAtKey:         "",
	}
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "continuation-claim-nudge: clearing %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
		return false
	}
	for key := range kvs {
		delete(s.Metadata, key)
	}
	return true
}

func atoiOr0(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func parseRFC3339OrZero(s string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}
