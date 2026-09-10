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
)

// Backstop pacing. Deliberately slow: this only rescues a pool slot that was
// handed work but never began it, so a couple of minutes of latency is fine and
// keeps the backstop nowhere near anything that could read as churn.
const (
	idleClaimNudgeGrace       = 90 * time.Second // observe-before-first-nudge; lets a normal claim land
	idleClaimNudgeBackoff     = 3 * time.Minute  // between retries when a delivered nudge didn't take
	idleClaimNudgeMaxAttempts = 3                // then give up and log (manual re-nudge remains)
)

const defaultPoolClaimNudge = "Run gc hook --claim --drain-ack --json now; if it returns work, execute it immediately."

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
//     claims, its trigger bead flips to in_progress and stops matching.
//   - State is persisted on the session bead, so a restart cannot replay it.
//   - Bounded per assignment: observe (grace) → nudge → backoff retries → give
//     up. It never spams a tick and never loops forever.
//   - Pool slots only.
//
// This is a thin predicate wrapper (poolClaimBackstop) over the shared
// grace→nudge→backoff→give-up engine in nudge_backstop.go; the pacing,
// looping, and delivery mechanics live there so continuation delivery can
// reuse them without duplicating this state machine.
func nudgeStalledPoolClaims(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	claimWork []beads.Bead,
	claimWorkStoreRefs []string,
	now time.Time,
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
	runNudgeBackstop(sp, store, sessionBeads, nil, now, stdout, "idle-claim-nudge", poolClaimBackstop{
		cfg:  cfg,
		work: newIdleClaimWorkSnapshot(claimWork, claimWorkStoreRefs),
	})
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
		"continuation-claim-nudge",
		poolContinuationBackstop{
			cfg:        cfg,
			candidates: newPoolContinuationCandidateSnapshot(sessionBeads, candidates),
		},
	)
}

// poolClaimBackstop is the backstopPredicate for pool-managed slots: it
// re-delivers the claim nudge to a slot whose assigned trigger bead is still
// unclaimed. See nudgeStalledPoolClaims for the full rationale and scope.
type poolClaimBackstop struct {
	cfg  *config.City
	work idleClaimWorkSnapshot
}

// poolContinuationBackstop is the backstopPredicate for a graph-v2 successor
// preassigned to a live pool session after that session completed the preceding
// step. The snapshot is keyed by the session bead's durable ID, not by a
// mutable alias or runtime name.
type poolContinuationBackstop struct {
	cfg        *config.City
	candidates poolContinuationCandidateSnapshot
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

func (p poolContinuationBackstop) revalidate(target backstopTarget) backstopResolution {
	if target.Store == nil {
		return backstopResolutionHold
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
		return backstopResolutionHold
	}
	current, err := live.Get(target.ID)
	if err != nil || current.ID != target.ID {
		return backstopResolutionHold
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
		return backstopResolutionClear
	}
	root, err := live.Get(target.RootID)
	if err != nil || root.ID != target.RootID {
		return backstopResolutionHold
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
		return backstopResolutionClear
	}
	return backstopResolutionOutstanding
}

func (p poolContinuationBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	writeContinuationClaimMarker(store, s, target, 0, now, stdout)
}

func (p poolContinuationBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeContinuationClaimMarker(store, s, target, attempts, now, stdout)
}

func (p poolContinuationBackstop) exhausted(_ beads.Store, _ *beads.Bead, _ io.Writer) {
}

func (p poolContinuationBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) {
	clearContinuationClaimMarker(store, s, stdout)
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
func (p poolClaimBackstop) resolve(s beads.Bead, _ map[string]beads.Bead, sessName string) (backstopTarget, backstopResolution) {
	triggerID := strings.TrimSpace(s.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	if triggerID == "" {
		return backstopTarget{}, backstopResolutionClear
	}
	w, ok := p.work.lookup(triggerID, s.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey])
	if !ok || !isUnclaimedTrigger(w, sessName) {
		return backstopTarget{}, backstopResolutionClear
	}
	return backstopTarget{ID: triggerID}, backstopResolutionOutstanding
}

func (p poolClaimBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	marked := strings.TrimSpace(s.Metadata[idleClaimNudgeTriggerKey])
	return marked == target.ID, atoiOr0(s.Metadata[idleClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[idleClaimNudgeAtKey])
}

func (p poolClaimBackstop) content(s beads.Bead) string {
	return stalledPoolClaimNudgeFor(p.cfg, s)
}

func (p poolClaimBackstop) revalidate(_ backstopTarget) backstopResolution {
	return backstopResolutionOutstanding
}

func (p poolClaimBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	writeIdleClaimMarker(store, s, target.ID, 0, now, stdout)
}

func (p poolClaimBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeIdleClaimMarker(store, s, target.ID, attempts, now, stdout)
}

// exhausted is a deliberate no-op: manual re-nudge remains the pool escape
// hatch, and leaving the marker untouched at the cap (rather than clearing or
// rewriting it) is what keeps this predicate silent on every subsequent tick.
func (p poolClaimBackstop) exhausted(_ beads.Store, _ *beads.Bead, _ io.Writer) {
}

func (p poolClaimBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) {
	clearIdleClaimMarker(store, s, stdout)
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
// waiting to be claimed: status open and not already assigned to this slot
// (a non-empty assignee equal to the session means the claim is mid-flight).
func isUnclaimedTrigger(w beads.Bead, sessName string) bool {
	if !strings.EqualFold(strings.TrimSpace(w.Status), "open") {
		return false // in_progress / closed / blocked → not ours to nudge
	}
	if assignee := strings.TrimSpace(w.Assignee); assignee != "" && assignee == sessName {
		return false
	}
	return true
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

// clearIdleClaimMarker wipes the marker once the slot no longer has unclaimed
// work, so the next assignment starts its grace clock fresh. No-op (no store
// write) when there is nothing to clear, so steady-state ticks stay silent.
func clearIdleClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) {
	if s.Metadata[idleClaimNudgeTriggerKey] == "" &&
		s.Metadata[idleClaimNudgeCountKey] == "" &&
		s.Metadata[idleClaimNudgeAtKey] == "" {
		return
	}
	kvs := map[string]string{
		idleClaimNudgeTriggerKey: "",
		idleClaimNudgeCountKey:   "",
		idleClaimNudgeAtKey:      "",
	}
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "idle-claim-nudge: clearing %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
		return
	}
	for k := range kvs {
		delete(s.Metadata, k)
	}
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

func clearContinuationClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) {
	if s.Metadata[continuationClaimNudgeWorkKey] == "" &&
		s.Metadata[continuationClaimNudgeRootKey] == "" &&
		s.Metadata[continuationClaimNudgeStoreRefKey] == "" &&
		s.Metadata[continuationClaimNudgeGenerationKey] == "" &&
		s.Metadata[continuationClaimNudgeCountKey] == "" &&
		s.Metadata[continuationClaimNudgeAtKey] == "" {
		return
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
		return
	}
	for key := range kvs {
		delete(s.Metadata, key)
	}
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
