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
)

// Session-bead metadata keys for the stalled-claim backstop. The state machine
// is PERSISTED on the pool slot's own session bead so it survives a controller
// restart — the in-memory grace map of the reverted #312 nudger did not, which
// is precisely why that one re-nudge-stormed on every restart (test-5il).
const (
	idleClaimNudgeTriggerKey = "idle_claim_nudge_trigger" // trigger bead id last acted on
	idleClaimNudgeCountKey   = "idle_claim_nudge_count"   // nudges delivered for that trigger
	idleClaimNudgeAtKey      = "idle_claim_nudge_at"      // RFC3339 of last attempt / first observation
)

// Backstop pacing. Deliberately slow: this only rescues a pool slot that was
// handed work but never began it, so a couple of minutes of latency is fine and
// keeps the backstop nowhere near anything that could read as churn.
const (
	idleClaimNudgeGrace       = 90 * time.Second // observe-before-first-nudge; lets a normal claim land
	idleClaimNudgeBackoff     = 3 * time.Minute  // between retries when a delivered nudge didn't take
	idleClaimNudgeMaxAttempts = 3                // then give up and log (manual re-nudge remains)
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
//     claims, its trigger bead flips to in_progress and stops matching.
//   - State is persisted on the session bead, so a restart cannot replay it.
//   - Bounded per assignment: observe (grace) → nudge → backoff retries → give
//     up. It never spams a tick and never loops forever.
//   - Pool slots only.
//
// This is a thin predicate wrapper (poolClaimBackstop) over the shared
// grace→nudge→backoff→give-up engine in nudge_backstop.go; the pacing,
// looping, and delivery mechanics live there so a second predicate (e.g. for
// named/direct startup kickoff) can reuse them without duplicating this state
// machine.
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

// poolClaimBackstop is the backstopPredicate for pool-managed slots: it
// re-delivers the claim nudge to a slot whose assigned trigger bead is still
// unclaimed. See nudgeStalledPoolClaims for the full rationale and scope.
type poolClaimBackstop struct {
	cfg  *config.City
	work idleClaimWorkSnapshot
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
func (p poolClaimBackstop) outstandingID(s beads.Bead, _ map[string]beads.Bead, sessName string) (string, bool) {
	triggerID := strings.TrimSpace(s.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	if triggerID == "" {
		return "", false
	}
	w, ok := p.work.lookup(triggerID, s.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey])
	if !ok || !isUnclaimedTrigger(w, sessName) {
		return "", false
	}
	return triggerID, true
}

func (p poolClaimBackstop) state(s beads.Bead, id string) (same bool, attempts int, last time.Time) {
	marked := strings.TrimSpace(s.Metadata[idleClaimNudgeTriggerKey])
	return marked == id, atoiOr0(s.Metadata[idleClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[idleClaimNudgeAtKey])
}

func (p poolClaimBackstop) content(s beads.Bead) string {
	return claimNudgeFor(p.cfg, s)
}

func (p poolClaimBackstop) observe(store beads.Store, s *beads.Bead, id string, now time.Time, stdout io.Writer) {
	writeIdleClaimMarker(store, s, id, 0, now, stdout)
}

func (p poolClaimBackstop) record(store beads.Store, s *beads.Bead, id string, attempts int, now time.Time, stdout io.Writer) {
	writeIdleClaimMarker(store, s, id, attempts, now, stdout)
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
	template := normalizedSessionTemplate(session, cfg)
	if template == "" {
		return ""
	}
	agent := findAgentByTemplate(cfg, template)
	if agent == nil {
		return ""
	}
	return strings.TrimSpace(agent.Nudge)
}

// writeIdleClaimMarker persists the backstop state machine onto the session
// bead and mirrors it into the in-memory snapshot so the rest of this tick
// reads the just-written values.
func writeIdleClaimMarker(store beads.Store, s *beads.Bead, triggerID string, attempts int, now time.Time, stdout io.Writer) {
	kvs := map[string]string{
		idleClaimNudgeTriggerKey: triggerID,
		idleClaimNudgeCountKey:   strconv.Itoa(attempts),
		idleClaimNudgeAtKey:      now.UTC().Format(time.RFC3339),
	}
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "idle-claim-nudge: marking %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
		return
	}
	if s.Metadata == nil {
		s.Metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		s.Metadata[k] = v
	}
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
