package main

// The seat-claim backstop: ready work a seat owns and never picked up.
//
// The three lanes that came before this one each key on something OTHER than
// "what work does this seat own right now". nudgeStalledPoolClaims keys on the
// slot's bound gc.trigger_bead_id; nudgeStalledPoolContinuations keys on the one
// preassigned graph-v2 successor of a step that already completed; and
// nudgeStalledPoolExecution (the execution backstop) starts at the CLAIM and
// only ever looks at in_progress rows. So the state none of them covers is the
// one ga-evxqd reproduced live: a configured named seat, awake and quiet, with
// an OPEN bead routed to its own identity, no assignee, and zero unmet
// dependencies — sitting untouched for three hours until a human assigned it by
// hand and nudged the pane. The trigger-bead keying has the same blind spot for
// pool slots (a slot idle for 22 hours next to its own open assigned bead), and
// this lane closes both, because "the seat's own open work" is an IDENTITY
// question rather than a binding one.
//
// # The escalation is a nudge, and it stays a nudge
//
// The execution backstop ends at a drain, and it has to: the seat there holds an
// in_progress row that nothing else will release, so the stop -> close ->
// dead-assignee reopen chain is the only convergence available. Nothing is
// stranded here. The row is still OPEN, every claim probe in the fleet can still
// see it, and any other seat may take it. Draining a live seat over work it
// merely has not started would destroy a healthy session to fix nothing. So this
// lane re-nudges on the same bounded ladder, reports the first exhausted budget
// as one typed event, and then RE-ARMS instead of latching silent — a one-shot
// latch here would mean a seat that ignores three nudges is never contacted
// again, which is exactly the permanent silence the incident produced.
//
// # Why it cannot double-nudge the lanes it sits beside
//
// Two structural exclusions, both in resolve/snapshot rather than in timing:
// any in_progress row under the seat's identities hands the whole seat to the
// execution backstop for that tick, and the slot's own bound trigger bead is
// skipped because nudgeStalledPoolClaims already re-delivers on exactly this
// cadence for exactly that bead.

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
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Session-bead metadata keys for the seat-claim backstop. Persisted for the
// same reason its siblings persist theirs: a controller restart must RESUME the
// state machine, never replay it (test-5il).
const (
	seatClaimNudgeWorkKey     = "seat_claim_nudge_work"
	seatClaimNudgeRootKey     = "seat_claim_nudge_root"
	seatClaimNudgeStoreRefKey = "seat_claim_nudge_store_ref"
	seatClaimNudgeCountKey    = "seat_claim_nudge_count"
	seatClaimNudgeAtKey       = "seat_claim_nudge_at"
	// seatClaimNudgeCycleKey counts how many full attempt ladders this one
	// unclaimed bead has cost. It is observability, not a bound: the ladder
	// re-arms indefinitely because its only action is a nudge.
	seatClaimNudgeCycleKey = "seat_claim_nudge_cycles"
	// seatClaimNudgeStalledKey latches the typed escalation so it is reported
	// ONCE per unclaimed bead rather than once per re-armed ladder.
	seatClaimNudgeStalledKey = "seat_claim_nudge_stalled"
)

// seatClaimNudgeLabel prefixes this lane's stdout diagnostics.
const seatClaimNudgeLabel = "seat-claim-nudge"

// maxSeatClaimReadinessProbes bounds the live dependency reads one seat may cost
// on one tick while the lane looks for its first genuinely-ready row.
//
// Readiness is settled from each row's own dependency edges rather than from
// bd's denormalized is_blocked projection, which BdStore leaves unset on every
// read (see beadHasUnmetPlainBlocksDep). That is a store round-trip per row, and
// a seat parked next to a long queue of blocked work would otherwise pay one per
// row per tick forever. Exhausting the budget reports HOLD, not "nothing ready":
// the lane did not prove absence, it ran out of evidence.
const maxSeatClaimReadinessProbes = 8

// nudgeStalledSeatClaims re-delivers the configured claim nudge to a seat — a
// pool slot or a configured named interactive seat (see seatClaimBackstop.governs)
// — that is awake, quiet, and sitting on its own ready work without claiming it.
//
// The two work snapshots are the reconciler's index-aligned triples: the
// assignee-keyed assigned-work view and the broad open/unassigned/routed view.
// Both are needed because a seat can own a row either way — assigned to its
// session name, or merely routed to its identity — and neither view alone sees
// both. A partial read of either is not evidence that work is absent, so it
// disables the lane for that tick.
func nudgeStalledSeatClaims(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	assignedWork []beads.Bead,
	assignedWorkStores []beads.Store,
	assignedWorkStoreRefs []string,
	routedWork []beads.Bead,
	routedWorkStores []beads.Store,
	routedWorkStoreRefs []string,
	snapshotPartial bool,
	now time.Time,
	rec events.Recorder,
	stdout io.Writer,
) {
	if sp == nil || cfg == nil || store == nil || snapshotPartial {
		return // hot reconcile path: never panic on a half-built dependency
	}
	// beads.SessionStore embeds the Store interface, so a wrapper holding a nil
	// store is still a non-nil beads.Store.
	if sess, ok := store.(beads.SessionStore); ok && sess.Store == nil {
		return
	}
	runNudgeBackstop(sp, store, sessionBeads, nil, now, stdout, seatClaimNudgeLabel, seatClaimBackstop{
		cfg: cfg,
		sp:  sp,
		now: now,
		rec: rec,
		work: newSeatOpenWorkSnapshot(
			now,
			assignedWork, assignedWorkStores, assignedWorkStoreRefs,
			routedWork, routedWorkStores, routedWorkStoreRefs,
		),
	})
}

// seatOpenWork is one open row a seat owns, kept with the store handle its live
// re-read must use. Identity is the exact seat identity the row matched, and
// Assigned records WHICH way it matched, because the two arms have different
// ownership invariants to re-check before delivery.
type seatOpenWork struct {
	BeadID   string
	RootID   string
	StoreRef string
	Identity string
	Assigned bool
	Store    beads.Store
}

// seatOpenWorkSnapshot indexes a tick's work views by seat identity. Resolution
// is by identity rather than by bead id because the question this lane asks is
// "what does THIS seat own", and the same bead id can exist in independent
// stores.
type seatOpenWorkSnapshot struct {
	openByIdentity map[string][]seatOpenWork
	// inProgressIdentities marks every identity holding at least one in_progress
	// row. Such a seat belongs to the execution backstop for this tick.
	inProgressIdentities map[string]bool
	byKey                map[storeScopedBeadKey]seatOpenWork
}

func newSeatOpenWorkSnapshot(
	now time.Time,
	assignedWork []beads.Bead,
	assignedStores []beads.Store,
	assignedStoreRefs []string,
	routedWork []beads.Bead,
	routedStores []beads.Store,
	routedStoreRefs []string,
) seatOpenWorkSnapshot {
	snapshot := seatOpenWorkSnapshot{
		openByIdentity:       make(map[string][]seatOpenWork),
		inProgressIdentities: make(map[string]bool),
		byKey:                make(map[storeScopedBeadKey]seatOpenWork),
	}
	for i, wb := range assignedWork {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(wb.Status), "in_progress") {
			snapshot.inProgressIdentities[assignee] = true
			continue
		}
		snapshot.add(now, wb, assignee, true, storeAt(assignedStores, i), storeRefAt(assignedStoreRefs, i))
	}
	for i, wb := range routedWork {
		// The routed arm is the unassigned one by construction
		// (collectOpenUnassignedRoutedWork); an assignee here means the row
		// already belongs to the assigned arm above.
		if strings.TrimSpace(wb.Assignee) != "" {
			continue
		}
		routedTo := strings.TrimSpace(wb.Metadata[beadmeta.RoutedToMetadataKey])
		if routedTo == "" {
			continue
		}
		snapshot.add(now, wb, routedTo, false, storeAt(routedStores, i), storeRefAt(routedStoreRefs, i))
	}
	for identity := range snapshot.openByIdentity {
		rows := snapshot.openByIdentity[identity]
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].StoreRef != rows[j].StoreRef {
				return rows[i].StoreRef < rows[j].StoreRef
			}
			return rows[i].BeadID < rows[j].BeadID
		})
	}
	return snapshot
}

func (s seatOpenWorkSnapshot) add(now time.Time, wb beads.Bead, identity string, assigned bool, store beads.Store, storeRef string) {
	if !claimableSeatWork(wb, now) {
		return
	}
	row := seatOpenWork{
		BeadID:   strings.TrimSpace(wb.ID),
		RootID:   strings.TrimSpace(wb.Metadata[beadmeta.RootBeadIDMetadataKey]),
		StoreRef: storeRef,
		Identity: identity,
		Assigned: assigned,
		Store:    store,
	}
	key := storeScopedBeadKey{StoreRef: row.StoreRef, ID: row.BeadID}
	if _, duplicate := s.byKey[key]; duplicate {
		return
	}
	s.byKey[key] = row
	s.openByIdentity[identity] = append(s.openByIdentity[identity], row)
}

func storeAt(stores []beads.Store, i int) beads.Store {
	if i < len(stores) {
		return stores[i]
	}
	return nil
}

func storeRefAt(storeRefs []string, i int) string {
	if i < len(storeRefs) {
		return normalizeIdleClaimStoreRef(storeRefs[i])
	}
	return normalizeIdleClaimStoreRef("")
}

// holdsInProgress reports whether any of the seat's identities holds a claim.
func (s seatOpenWorkSnapshot) holdsInProgress(identities []string) bool {
	for _, identity := range identities {
		if s.inProgressIdentities[identity] {
			return true
		}
	}
	return false
}

// openForIdentities returns the deduped open rows the seat owns, in a stable
// order so a seat with several of them resolves the same way on every tick.
// skip is the seat's own bound trigger bead, which another lane already covers.
func (s seatOpenWorkSnapshot) openForIdentities(identities []string, skip storeScopedBeadKey) []seatOpenWork {
	// "This seat owns nothing" is the answer on almost every tick for almost
	// every seat, and this runs per governed seat per reconcile tick — so settle
	// it with a few map lookups rather than allocating to discover it.
	owned := 0
	for _, identity := range identities {
		owned += len(s.openByIdentity[identity])
	}
	if owned == 0 {
		return nil
	}
	seen := make(map[storeScopedBeadKey]struct{}, owned)
	var out []seatOpenWork
	for _, identity := range identities {
		for _, row := range s.openByIdentity[identity] {
			key := storeScopedBeadKey{StoreRef: row.StoreRef, ID: row.BeadID}
			if key == skip {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, row)
		}
	}
	return out
}

// claimableSeatWork is the pure-bead half of "this row is the seat's to start
// now". Everything here is readable from the snapshot alone; the dependency half
// costs a store round-trip and runs only on the rows that survive this.
//
// The kind exclusions are the structural ones: graphroute already states that
// routing never lands on workflow-topology kinds and that control kinds — drain
// controls among them — belong to the control dispatcher, so a seat must never
// be told to claim one even when the row carries its identity.
func claimableSeatWork(b beads.Bead, now time.Time) bool {
	if strings.TrimSpace(b.ID) == "" || b.Type == sessionBeadType {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(b.Status), "open") {
		return false
	}
	if beads.IsDeferred(b, now) || beads.HasReadyExcludedLabel(b) || hasHoldLabel(b.Labels) {
		return false
	}
	kind := strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])
	return !graphroute.IsControlDispatcherKind(kind) && !graphroute.IsWorkflowTopologyKind(kind)
}

// hasHoldLabel reports whether any label puts the bead in the hold: dimension.
// A hold:<value> label means "paused pending a specific actor or condition"
// (engdocs/contributors/hold-label-conventions.md), which is the bead's own
// statement that it is not waiting on the seat. The PREFIX is the whole test —
// the values name actors, and naming one here would put a role in Go.
//
// Deliberately not holdLabelValue (doctor_hold_label_routed_to.go): that one
// answers "which actor is this bead routed to", so it skips hold:external
// precisely because no actor is named. Here every hold value disqualifies the
// row — an externally-blocked bead is the LEAST claimable of all.
func hasHoldLabel(labels []string) bool {
	for _, label := range labels {
		if strings.HasPrefix(strings.TrimSpace(label), "hold:") {
			return true
		}
	}
	return false
}

// boundTriggerKey is the seat's own bound trigger bead, store-scoped.
func boundTriggerKey(s beads.Bead) storeScopedBeadKey {
	triggerID := strings.TrimSpace(s.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	if triggerID == "" {
		return storeScopedBeadKey{}
	}
	return storeScopedBeadKey{
		StoreRef: normalizeIdleClaimStoreRef(s.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]),
		ID:       triggerID,
	}
}

// seatClaimBackstop is the backstopPredicate for a seat sitting on its own ready
// work. See the file header for the full rationale and scope.
type seatClaimBackstop struct {
	cfg  *config.City
	sp   runtime.Provider
	now  time.Time
	rec  events.Recorder
	work seatOpenWorkSnapshot
}

// governs covers pool slots and configured named interactive seats.
//
// Manual seats are excluded because a manual seat is a human's own session
// rather than an orchestration slot, so nudging one would act against a session
// this lane has no business driving.
//
// Dependency-only floors are excluded too, and this is where the scope departs
// from the execution backstop's. There, a floor holds a real in_progress claim
// that nothing but a drain will release, so leaving it out would strand it. Here
// there is nothing to release: a floor is minted to satisfy someone else's
// dependency gate (ensureDependencyOnlyTemplate, build_desired_state.go) rather
// than to serve a work binding, so an open row that merely carries its slot
// identity is not evidence it was handed anything to start.
func (p seatClaimBackstop) governs(s beads.Bead) bool {
	if isManualSessionBead(s) {
		return false
	}
	if strings.TrimSpace(s.Metadata["dependency_only"]) == "true" {
		return false
	}
	return strings.TrimSpace(s.Metadata["pool_managed"]) == "true" || isNamedSessionBead(s)
}

// resolve reports an outstanding stall only when all of it holds: the seat holds
// NO in-progress claim, it owns at least one open row that is genuinely ready,
// no human is attached, and the runtime says it has been quiet for at least the
// grace window.
//
// The order is cost-ordered and guard-ordered at once. The in-progress check and
// the snapshot lookup are in-memory and settle the overwhelmingly common
// answers; the runtime probes come next; and only a seat that is quiet,
// unattended, and demonstrably sitting on its own work pays for the live
// dependency reads.
func (p seatClaimBackstop) resolve(s beads.Bead, _ map[string]beads.Bead, sessName string) (backstopTarget, backstopResolution) {
	// An in_progress row is the execution backstop's turf. HOLD rather than
	// clear: our work did not go away, this seat just belongs to another lane
	// for now, and both lanes nudging it on one tick is the churn neither is
	// allowed to cause.
	identities := currentSessionAssigneeIdentities(s)
	if p.work.holdsInProgress(identities) {
		return backstopTarget{}, backstopResolutionHold
	}
	candidates := p.work.openForIdentities(identities, boundTriggerKey(s))
	if len(candidates) == 0 {
		return backstopTarget{}, backstopResolutionClear
	}
	// Never act under a human's hands. While a terminal is attached a nudge
	// would inject keystrokes into the operator's own session, and a quiet
	// attached seat is "we cannot tell", not "idle" — so HOLD, which lets a
	// grace window already running survive the human detaching.
	if p.sp.IsAttached(sessName) {
		return backstopTarget{}, backstopResolutionHold
	}
	if !p.sessionIsQuiet(sessName) {
		return backstopTarget{}, backstopResolutionHold
	}
	row, resolution := p.firstReadyRow(candidates)
	if resolution != backstopResolutionOutstanding {
		return backstopTarget{}, resolution
	}
	return backstopTarget{
		ID:       row.BeadID,
		RootID:   row.RootID,
		StoreRef: row.StoreRef,
		Assignee: row.Identity,
		Store:    row.Store,
	}, backstopResolutionOutstanding
}

// firstReadyRow walks the seat's own open rows in their stable order and returns
// the first whose blocking dependencies are all satisfied.
//
// It settles readiness from live dependency edges rather than from the
// is_blocked projection, which is absent on every store class production
// actually hands this lane. A read that FAILS holds, and so does a spent probe
// budget: neither is proof that the seat has nothing ready.
func (p seatClaimBackstop) firstReadyRow(candidates []seatOpenWork) (seatOpenWork, backstopResolution) {
	for i, row := range candidates {
		if i >= maxSeatClaimReadinessProbes {
			return seatOpenWork{}, backstopResolutionHold
		}
		if row.Store == nil {
			return seatOpenWork{}, backstopResolutionHold
		}
		blocked, err := beadHasUnmetPlainBlocksDep(row.Store, row.BeadID)
		if err != nil {
			return seatOpenWork{}, backstopResolutionHold
		}
		if blocked {
			continue
		}
		return row, backstopResolutionOutstanding
	}
	return seatOpenWork{}, backstopResolutionClear
}

// sessionIsQuiet reports whether the runtime has observed no activity for at
// least the grace window. An unreadable or unset activity signal is NOT quiet: a
// backstop that treats "unknown" as "idle" nudges working agents, which is
// exactly how the reverted idle-session nudger produced restart storms.
func (p seatClaimBackstop) sessionIsQuiet(sessName string) bool {
	last, err := p.sp.GetLastActivity(sessName)
	if err != nil || last.IsZero() {
		return false
	}
	return p.now.Sub(last) >= idleClaimNudgeGrace
}

func (p seatClaimBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	same = strings.TrimSpace(s.Metadata[seatClaimNudgeWorkKey]) == target.ID &&
		strings.TrimSpace(s.Metadata[seatClaimNudgeStoreRefKey]) == target.StoreRef
	return same, atoiOr0(s.Metadata[seatClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[seatClaimNudgeAtKey])
}

// content resolves the seat's claim nudge, falling back to defaultPoolClaimNudge
// when the agent is known but configures no nudge — the same fallback both
// sibling claim lanes use. The named seats this lane rescues configure no
// [agent] nudge, so reading the raw configured value would make it inert for
// exactly the population it exists for.
func (p seatClaimBackstop) content(s beads.Bead) string {
	return stalledPoolClaimNudgeFor(p.cfg, s)
}

// revalidate re-reads the row through its owning store's authoritative live
// handle immediately before delivery. Work snapshots are normally
// CachingStore-backed, so a plain read can still show a row another seat claimed
// seconds ago. A failed read HOLDS: it is not proof the row went away.
func (p seatClaimBackstop) revalidate(target backstopTarget) backstopResolution {
	row, ok := p.work.byKey[storeScopedBeadKey{StoreRef: target.StoreRef, ID: target.ID}]
	if !ok || row.Store == nil {
		return backstopResolutionHold
	}
	live := beads.HandlesFor(row.Store).Live
	if live == nil {
		return backstopResolutionHold
	}
	current, err := live.Get(target.ID)
	if err != nil || current.ID != target.ID {
		return backstopResolutionHold
	}
	if !claimableSeatWork(current, p.now) || !seatStillOwns(current, row) {
		return backstopResolutionClear
	}
	blocked, err := beadHasUnmetPlainBlocksDep(row.Store, target.ID)
	if err != nil {
		return backstopResolutionHold
	}
	if blocked {
		return backstopResolutionClear
	}
	return backstopResolutionOutstanding
}

// seatStillOwns re-checks ownership through the arm the row matched on. The two
// arms are not interchangeable: an assigned row must still name the identity,
// and a routed row must still carry NO assignee, since an assignee appearing on
// it means some seat — possibly another one — has taken it.
func seatStillOwns(current beads.Bead, row seatOpenWork) bool {
	assignee := strings.TrimSpace(current.Assignee)
	if row.Assigned {
		return assignee == row.Identity
	}
	return assignee == "" &&
		strings.TrimSpace(current.Metadata[beadmeta.RoutedToMetadataKey]) == row.Identity
}

// observe starts a new row's window: a fresh grace clock, a fresh ladder count,
// and a cleared escalation latch, since both are spent per unclaimed bead.
func (p seatClaimBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	writeSeatClaimMarker(store, s, target, 0, 0, "", now, stdout)
}

// reserve records a delivery attempt, carrying the ladder count forward: an
// attempt spends attempt budget, never ladder budget.
func (p seatClaimBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeSeatClaimMarker(
		store, s, target, attempts,
		atoiOr0(s.Metadata[seatClaimNudgeCycleKey]),
		strings.TrimSpace(s.Metadata[seatClaimNudgeStalledKey]),
		now, stdout,
	)
}

// exhausted reports a spent attempt budget once and then re-arms the ladder.
//
// There is deliberately no terminal action beyond the report. The row is still
// open and still claimable by anyone; a drain would kill a live seat to release
// nothing. And the report is latched per bead while the LADDER is not, because
// the two answer different questions: an operator needs to hear about this stall
// once, and the seat needs to keep being asked. Latching both — the shape that
// makes a backstop go permanently quiet after three tries — is what left the
// ga-evxqd row sitting for three hours.
func (p seatClaimBackstop) exhausted(store beads.Store, s *beads.Bead, stdout io.Writer) {
	beadID := strings.TrimSpace(s.Metadata[seatClaimNudgeWorkKey])
	if beadID == "" {
		return
	}
	sessName := strings.TrimSpace(s.Metadata["session_name"])
	attempts := atoiOr0(s.Metadata[seatClaimNudgeCountKey])
	if strings.TrimSpace(s.Metadata[seatClaimNudgeStalledKey]) == "" {
		// Latch FIRST. A failed event write must not leave the escalation armed
		// to repeat on every re-armed ladder; the marker is the durable record
		// that this bead was reported, and the operator sees failures on stdout.
		if !writeSessionMetadata(store, s, map[string]string{
			seatClaimNudgeStalledKey: p.now.UTC().Format(time.RFC3339),
		}, seatClaimNudgeLabel, stdout) {
			return
		}
		p.emitClaimStalled(s, beadID, attempts)
		// Report the attempts actually DELIVERED, not the cap: a seat whose agent
		// resolves no nudge at all reports 0 having never been contacted, and an
		// operator reading the cap there would hunt for nudges that never went out.
		fmt.Fprintf(stdout, //nolint:errcheck // best-effort
			"%s: %s left %s open and unclaimed after %d/%d nudge attempts; re-arming (no drain — the bead is still claimable)\n",
			seatClaimNudgeLabel, sessName, beadID, attempts, idleClaimNudgeMaxAttempts)
	}
	if attempts == 0 {
		// Nothing was deliverable for this seat, so there is no nudge to re-arm
		// and re-arming would be a pure write loop. The escalation above is the
		// whole output; a changed row starts a fresh window through observe.
		return
	}
	cycles := atoiOr0(s.Metadata[seatClaimNudgeCycleKey]) + 1
	// Patch only the pacing fields: the work/root/store-ref identity and the
	// escalation latch must survive the re-arm, or the next ladder would look
	// like a new bead and report itself all over again.
	if !writeSessionMetadata(store, s, map[string]string{
		seatClaimNudgeCountKey: "0",
		seatClaimNudgeCycleKey: strconv.Itoa(cycles),
		seatClaimNudgeAtKey:    p.now.UTC().Format(time.RFC3339),
	}, seatClaimNudgeLabel, stdout) {
		return
	}
	fmt.Fprintf(stdout, //nolint:errcheck // best-effort
		"%s: %s re-arming the claim ladder for %s (ladder %d)\n",
		seatClaimNudgeLabel, sessName, beadID, cycles)
}

func (p seatClaimBackstop) emitClaimStalled(s *beads.Bead, beadID string, attempts int) {
	if p.rec == nil {
		return
	}
	rootID := strings.TrimSpace(s.Metadata[seatClaimNudgeRootKey])
	payload, err := json.Marshal(events.ExecutionClaimStalledPayload{
		BeadID:     beadID,
		RootBeadID: rootID,
		SessionID:  s.ID,
		Attempts:   attempts,
	})
	if err != nil {
		return
	}
	p.rec.Record(events.Event{
		Type:      events.ExecutionClaimStalled,
		Actor:     eventActor(),
		Subject:   beadID,
		RunID:     rootID,
		SessionID: s.ID,
		Payload:   payload,
	})
}

func (p seatClaimBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) {
	clearSeatClaimMarker(store, s, stdout)
}

func writeSeatClaimMarker(
	store beads.Store,
	s *beads.Bead,
	target backstopTarget,
	attempts, cycles int,
	stalledAt string,
	now time.Time,
	stdout io.Writer,
) bool {
	return writeSessionMetadata(store, s, map[string]string{
		seatClaimNudgeWorkKey:     target.ID,
		seatClaimNudgeRootKey:     target.RootID,
		seatClaimNudgeStoreRefKey: target.StoreRef,
		seatClaimNudgeCountKey:    strconv.Itoa(attempts),
		seatClaimNudgeCycleKey:    strconv.Itoa(cycles),
		seatClaimNudgeAtKey:       now.UTC().Format(time.RFC3339),
		seatClaimNudgeStalledKey:  stalledAt,
	}, seatClaimNudgeLabel, stdout)
}

// clearSeatClaimMarker wipes the state machine — escalation latch included — so
// the seat's next row starts a fresh window. No-op when there is nothing to
// clear, so steady-state ticks stay write-free.
func clearSeatClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) {
	keys := []string{
		seatClaimNudgeWorkKey,
		seatClaimNudgeRootKey,
		seatClaimNudgeStoreRefKey,
		seatClaimNudgeCountKey,
		seatClaimNudgeCycleKey,
		seatClaimNudgeAtKey,
		seatClaimNudgeStalledKey,
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
	if !writeSessionMetadata(store, s, kvs, seatClaimNudgeLabel, stdout) {
		return
	}
	for _, key := range keys {
		delete(s.Metadata, key)
	}
}
