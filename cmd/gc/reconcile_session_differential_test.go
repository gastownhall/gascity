package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The session differential (EFFECT-STRUCTURE §2 item 3; it generalizes the
// allocation differential, P3-5c, to rows and effects). Each fixture builds
// twin worlds over the simulator's provider from the same rows, work and
// runtimes. Copy A runs legacy ticks until one changes nothing: each is the
// desired-state build, the session-bead sync, then the session reconciler
// (reconcileSessionBeadsAtPath's named-demand form, as the controller's
// ticks call it). Copy B runs v2 on the simulator: passes with every effect
// released synchronously until a pass admits nothing and writes nothing.
// Both then compare row status, type and metadata, work assignees and
// status, the provider's action log, and the recorded events by type,
// subject and payload keys (the plan's event-parity rule, §2.3).
//
// A difference is never whitelisted by shape. Each one a fixture shows must
// be accounted to an entry in one of three tables, and an entry that no
// longer shows fails as loudly as an unaccounted difference:
//   - parityAccepted: a CONTRACT v5 §12.2 item, by number;
//   - parityFindings: a divergence the contract does not explain, pinned
//     while the owner decides or its fix lands (a finding is not an
//     explanation);
//   - parityUnported: legacy behavior of an arm or effect kind not yet
//     registered on main. An entry fails once its arm or kind registers, so
//     the registering PR turns it into parity, a §12.2 entry or a finding.
//
// Fixtures are quiescent shapes. A behavior that is a race (INC-024's stale
// heal, POOL-051's locked re-census) is pinned here in its settled form; the
// simulator's scenarios own the interleaving (R5) until effect seams let a
// fixture inject one (EFFECT-STRUCTURE §2 item 5).
//
// TestEveryKeptBehaviorOfARegisteredArmHasAParityFixture gates the fixtures
// on BEHAVIORS.md: every KEPT row whose owner names only reachable arms and
// effect kinds must be named by a fixture. An arm is reachable once it is in
// rowArms; a kind once it is registered and a registered arm, or the
// allocation's createIntent, can propose it (proposedKinds).

// parityFixture is one twin-world fixture. Rows and Work seed the city leg
// on both sides; each runtime runs under the session name of the row whose
// ID it carries. City is workerCity(3) when nil. Explain maps each aspect the
// fixture differs on to the entry that accounts for it.
type parityFixture struct {
	Name      string
	Behaviors []string
	Rows      []parityRow
	Work      []parityRow
	// Rig seeds the rig leg: census-only rows, which legacy's ticks read as a
	// rig work store and v2's census as a leg (timeline fixtures only).
	Rig      []parityRow
	Runtimes []simRuntime
	City     func() *config.City
	// Drains is a drain requested at generation 1, by row ID to reason:
	// legacy's drain tracker holds it, and the row its v2 stop request
	// (CONTRACT v5 D1), which legacy ignores. Legacy's outcome projects its
	// tracker back onto those keys, so the two compare.
	Drains map[string]string
	// Pending names the rows whose runtime has a pending interaction:
	// legacy's provider answers it, and v2's observation cache holds the
	// probe's fact.
	Pending []string
	// Idle names the rows whose runtime was last active an hour before the
	// fixture and answers WaitForIdle idle: legacy's idle probe and v2's
	// activity fact (I3) and fresh idle proof read the same provider.
	Idle []string
	// StartErr fails each copy's first provider Start (S3's abandon).
	StartErr error
	Explain  map[string]string
}

// parityRow is a bead; a type alias keeps fixtures short.
type parityRow = beads.Bead

// parityEntry is one accounted difference: its clause or owning PR, why,
// and for an unported arm the keys it writes.
type parityEntry struct {
	ref, reason string
	keys        []string // the row fields a wildcard "row:<id>:*<label>" Explain accounts to it
}

// parityAccepted are CONTRACT v5 §12.2's items the fixtures show; the rest
// come with the arms that own them (#6 with A3, #15-#17 with A21).
var parityAccepted = map[string]parityEntry{
	"§12.2#51 a7-lag": {
		"CONTRACT v5 §12.2 #51 (A7; C7d)", "legacy refreshes a row's identity every tick, beside its heals and under a live kill fence; v2's A7 writes it when it decides the row, a pass later when an earlier arm (an A6 heal) takes the row's first pass, or once A2's kill fence lets the row through, so its write, the synced_at it stamps and its runtime alias sync land later",
		[]string{"synced_at", "alias", "alias_history", "labels", "pool_slot", "agent_name", "work_dir", "wake_mode", "session_key", "continuation_epoch", "command", "provider", "provider_kind", "builtin_ancestor", "resume_flag", "resume_style", "resume_command", "session_id_flag"},
	},
	"§12.2#17 orphan-close-reason": {
		"CONTRACT v5 §12.2 #17 (A21, C5c1 review)", "an unwanted dead row closes with its drain reason (orphaned, or suspended for an agent-suspended seat that legacy closes as orphaned), not MAINT-051's gc_swept (v2 does not run the sweep) nor the suspended legacy writes for a row squatting a configured name",
		[]string{"state", "close_reason"},
	},
	"§12.2#37 current-bead-lag": {
		"CONTRACT v5 §12.2 #37 (A6; mc-d4atw)", "v2 stamps currently_processing_bead_id one pass after legacy: A6 decides on the pass's read and stamps only rows AL1 wants awake, so a row whose quarantine just cleared is stamped a pass later",
		[]string{session.CurrentBeadIDKey},
	},
	"§12.2#35 fenced-timer-heal": {
		"CONTRACT v5 §12.2 #35 (A2 before A6; mc-uyclc)", "an expired quarantine is not cleared while a kill fence is live: v2 holds the row until the fence expires; legacy clears it (and zeroes wake_attempts and churn_count) under the live fence",
		[]string{"quarantined_until", "wake_attempts", "churn_count"},
	},
	"§12.2#40 held-start-bind":     {"CONTRACT v5 §12.2 #40 (C5b; PreWake binds, S1)", "legacy's realize binds a wanted seat's trigger and stamps its origin when a start gate then holds it; v2 binds in the PreWake, so a held seat carries neither until it launches (the trigger is presence only, claims are pull, and a work item binds once per pass: no double claim or spawn)", []string{"gc.trigger_bead_id", "gc.trigger_bead_store_ref", "gc.work_dir", "session_origin", "continuation_epoch"}},
	"§12.2#39 commit-writes-awake": {"CONTRACT v5 §12.2 #39 (C5b)", "v2's start and adopt commits write awake, the state legacy's next tick heals its committed active row to; for that one tick legacy reads active", []string{"state"}},
	"§12.2#38 start-meta-clears":   {"CONTRACT v5 §12.2 #38 (A18, C5a1)", "legacy's start clears stale runtime metadata (RemoveMeta) on the runtime it starts; v2's FreshOnly start creates a runtime without any and removes nothing", nil},
	"§12.2#27 held-slot-kept": {
		"CONTRACT v5 §12.2 #27 (A10 v5.7, A21; C5c1 review ruling)", "an undesired pool row parked by a deliberate hold (user-hold, wait-hold, a future held_until or quarantined_until) keeps its slot until the hold ends; legacy's MAINT-051 sweep closes it held or not",
		[]string{"status", "state", "close_reason", "closed_at", "synced_at"},
	},
	"§12.2#43 close-after-hold-heal": {
		"CONTRACT v5 §12.2 #43 (A6, A21)", "v2 heals an unwanted workless pool row's expired hold one pass before A21 closes it; legacy's MAINT-051 sweep closes it first and leaves the expired held_until and sleep_reason on the closed row",
		[]string{"held_until", "sleep_reason"},
	},
	"§12.2#41 close-removes-corpse": {"CONTRACT v5 §12.2 #41 (C3 step 1, M1; C5c1)", "A21's close removes the corpse it counts as stopped, by exact session object; legacy's sweep closes the row and leaves the dead pane listed", nil},
	"§12.2#42 orphan-claim-release-unconfigured": {
		"CONTRACT v5 §12.2 #42 (C3 step 3, SESS-082, NEW-5; C5c1)", "an unwanted row whose agent was removed and which holds a served claim: legacy's sync close (SESS-722) refuses on the work and the reconciler's release (SESS-082) never runs for it, so seat and claim wedge; v2's A21 releases the claim (bead.dead_assignee_reopened), closes the row, and the freed demand creates a fresh seat",
		[]string{"status", "state", "close_reason", "closed_at", "synced_at"},
	},
	"§12.2#3 R16": {
		"CONTRACT v5 §12.2 #3 (ruling 4)", "a resume voids a suspended drain; legacy signals, stops and restarts the row",
		append([]string{"slept_at"}, parityStartKeys...),
	},
}

// parityStartKeys are the row keys a legacy start writes.
var parityStartKeys = []string{
	"state", "state_reason", "generation", "instance_token", "last_woke_at", "awake_started_at", "creation_complete_at", "primed_at", "prompt_hash",
	"core_hash_breakdown", "live_hash", "started_config_hash", "started_launch_hash", "started_live_hash", "started_provision_hash", "pending_create_claim", "pending_create_started_at",
}

// parityFindings are divergences between legacy and merged v2 that CONTRACT
// v5 does not explain. Delete an entry when its fix lands; its fixture then
// fails until the fixture's Explain drops it too.
var parityFindings = map[string]parityEntry{
	"sleep-policy-keys": {
		"BEHAVIORS SESS-601 (KEPT: AL1 allocate)", "legacy persists the seven sleep-policy keys on every visited row; the allocate is pure and no v2 arm or effect writes them, and neither CONTRACT v5 nor the plan names a writer",
		[]string{"requested_sleep_after_idle", "effective_sleep_after_idle", "sleep_policy_source", "sleep_capability", "sleep_policy_adjustment_reason", "sleep_policy_fingerprint", "config_wake_suppressed"},
	},
	"unknown-state-diagnostic": {
		"CONTRACT v5 A5; ORCH-NOTES C2c1 (sync SESS-045/046)", "legacy stamps the unknown_state_* markers and emits session.unknown_state (escalating at 30 minutes); v2 writes no marker and records reconciler.alert instead. Not a §12.2 item, and against the plan's event-parity rule (§2.3)",
		[]string{"unknown_state_first_seen", "unknown_state_value", "unknown_state_escalated_at"},
	},
	"named-trigger-clear": {"POOL-056 follow-up (ORCH-NOTES C5d ruling)", "legacy clears a preserved named row's stale trigger stamp; v2's A6 does not yet", []string{"gc.trigger_bead_id", "gc.trigger_bead_store_ref", "brain_parent_sid"}},
	"drain-cancel-on-probe-error": {
		"CONTRACT v5 O3, BEHAVIORS DRAIN-042", "legacy skips a drain whose running probe errors; v2's inventory still classifies the row, so A19's wake lens cancels the drain. No completion either way, but §12.2 does not list the cancel",
		[]string{session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey},
	},
	"idle-respawn-void": {
		"C6d, BEHAVIORS DRAIN-044", "legacy cancels an idle-respawn drain no longer eligible; A19 holds it until C6d completes its policy row",
		[]string{session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey},
	},
	"suspended-row-heal":   {"mc-92clf (ruled: v2 matches legacy; R7 Disposition owns the v2 side), BEHAVIORS SESS-531", "once an operator's suspend (the hold, #7417) has stopped a row's runtime, legacy's state heal turns the suspended row asleep under its hold; v2 keeps it suspended until R7 matches legacy, so its later restart replaces the suspend stamp", []string{"state", "suspended_at", "slept_at"}},
	"suspended-named-heal": {"mc-92clf (ruled: v2 matches legacy; R7 Disposition owns the v2 side), BEHAVIORS SESS-009, SESS-531", "in a suspended city, legacy heals a suspended named row's expired hold from a fresh snapshot (SESS-009), then its state heal (SESS-531) turns the row asleep; v2 heals the hold and keeps it suspended until R7 matches legacy", []string{"state"}},
	"episode-mirror":       {"C5b follow-up PR (owner; SESS-607 mirror, the gate is A18's)", "legacy mirrors a quarantined startup-health episode's count and kind onto the row every pass; v2's A18 holds the row on the episode but does not mirror it yet (the keys are not on session.Info)", []string{startupHealthActiveCountMetadataKey, startupHealthActiveKindMetadataKey}},

	"pending-create-swept": {
		"CONTRACT v5 A10, C3; BEHAVIORS MAINT-051, SESS-050..052", "an undesired pool pending create past its never-started deadline: legacy's MAINT-051 sweep closes it first as gc_swept, its claim keys left on the closed row, so its rollback never runs; v2 does not run the sweep, and A10 rolls it back (failed-create, the claim cleared). §12.2 #17 covers only the orphan close's reason",
		[]string{"state", "close_reason", "pending_create_claim", "pending_create_started_at"},
	},
	"rollback-stops-nothing": {"CONTRACT v5 C3, C8.8; BEHAVIORS SESS-052", "legacy stops a bead-scoped pool name before its rollback whether or not anything runs under it (releaseBeadScopedPoolRuntime); v2 stops only a runtime its fresh read finds, whose absence that read has confirmed. §12.2 does not list it", nil},
	"rollback-replacement-slot": {
		"CONTRACT v5 AL1, A10", "v2 creates a rolled-back seat's replacement in the rollback's pass, while the rolled-back row still occupies its slot, so the replacement takes the next slot; legacy closes the row first and creates the replacement the next tick in the freed slot. §12.2 does not list it",
		[]string{"agent_name", "canonical_instance_name", "canonical_pool_slot", "pool_slot", "prompt_hash"},
	},
	"rollback-leaves-lease": {
		"v5.9b §12.5 (runtime lease); BEHAVIORS START-033", "legacy's failed start rolls its pending create back while it still holds the row's runtime lease record, which stays on the closed row; v2's effect releases its lease record with the close. §12.2 does not list it",
		[]string{session.RuntimeLeaseExpiresKey, session.RuntimeLeaseFlockKey, session.RuntimeLeaseHolderKey, session.RuntimeLeaseTTLKey},
	},
	"dead-corpse-close": {
		"C5c2 (MAINT-031; CONTRACT v5 A21, C3)", "legacy closes an unwanted row that claims a live runtime once its runtime is a corpse (here MAINT-051's sweep reaches it first); A21 never closes a row that claims a live runtime, and C5c2's dead-runtime close is not built",
		[]string{"status", "state", "close_reason", "closed_at", "synced_at"},
	},
	"drift-deferral-skips": {"C7b1 (A14 runs after A6's heals and the allocation's writes, CONTRACT v5 §4)", "legacy's config-drift deferral (SESS-553/558/559) continues past the row, so that tick it skips the row's later writes, its current-bead stamp (SESS-613) and sleep-policy keys among them; v2's A6 and the allocation write them before A14 decides", []string{"currently_processing_bead_id"}},
	"mislabelled-close":    {"CONTRACT v5 AL1, A1", "legacy closes a row with no template and no session name as orphaned; v2's A1 leaves it open as None, which §12.2 does not list", []string{"status", "state", "close_reason", "closed_at"}},
	"idle-respawn-drain":   {"C6d (D2's idle-respawn row: its begin and the claim-revalidating signal, signal-fresh), BEHAVIORS SESS-614", "legacy drains a pool row awake only for ready, unclaimed assigned work, idle past its sleep time, and retires its slot so a fresh seat takes the work; v2's A20 begins a plain idle drain there (its stop request), and registers no idle-respawn begin or signal yet", []string{"sleep_intent", "drain_at", "state", "state_reason", "status", "close_reason", "closed_at", "synced_at", session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey}},
}

// parityUnported is legacy behavior that belongs to an arm or effect kind
// not yet registered. The key's arm ("A18") or kind must stay unregistered.
var parityUnported = map[string]parityEntry{
	"C7b2 drift-restart": {
		"C7b2 (A14's named restart in place, SESS-557)", "legacy kills a drifted named row's runtime and restarts it in place in the tick (ConfigDriftResetPatch, the start and its commit); v2's A14 leaves the row alone until C7b2 brings the restart with its effect",
		append([]string{"continuation_epoch", "generation", "state_reason", "creation_complete_at", "awake_started_at", "last_woke_at", "primed_at", "prompt_hash", "started_config_hash"}, parityStartKeys...),
	},
	// Unported with no fixture, since neither twin reads a provider-health
	// registry (legacy's gate is nil there): legacy's ADR-0013 episode
	// bookkeeping around A18's red skip, recordRedSkip, recordGreenTick and
	// the once-per-episode emitProviderHealthGateAlert (stdout and
	// provider.health_gate_alert). v2's A18 skips a red provider's start
	// (TestDecideRowStartGates) but keeps no episode and records no alert.
	// Owner: the C5b follow-up PR, with episode-mirror.
	"A4 idle-sleep": {"C6b1, C6b2 (A4 + D4)", "legacy signals an idle drain in the tick it begins it and finishes the sleep: SleepPatch(idle) with the drain's sleep-policy fingerprint, the idle latch's input, and starts it again when its policy changes; v2's begin (A20) writes the stop request and the fingerprint, and registers no signal or finalize yet, so its runtime stays up", append([]string{"sleep_intent", "sleep_reason", "slept_at", "drain_at", "detached_at", session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey}, parityStartKeys...)},
	"A4 managed-suspend": {
		"C6b1, C6b2 (A4 + D3, D4)", "legacy drains a managed suspend (the operator hold on a live runtime) to a stop, so a later wake restarts the row and its start consumes the request (D7); v2 writes no stop request on an operator-dormant row (D1 rule 2), and the direct stop verb of the operator-dormant path (D3, C6b2) is not registered yet, so the runtime stays up and the wake neither restarts the row nor is consumed",
		append([]string{"state", "wake_request", "wake_requested_at", session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey}, parityStartKeys...),
	},
	"A4 drain-stop": {"C6b1, C6b2 (A4 + D3, D4)", "legacy signals and stops a drain in the tick it begins it (its drain-ack mark, the provider's meta and Stop); v2's begin (A20) writes only the stop request (D1), which legacy never writes, and registers no signal or stop verb yet", []string{session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey, "drain_at", "sleep_intent", "slept_at", "state_reason"}},
	"A4 wait-sleep": {
		"C6b1, C6b2 (A4 + D3, D4)", "legacy drains a waiter to a sleep on its wait hold and, once the wait turns ready, starts it again through the idle latch (a wait's own intent is no explicit sleep intent) and delivers the wait's nudge with the start; v2's begin (A20) writes the stop request and registers no signal or stop verb yet, so the waiter's runtime stays up and its waits step queues the nudge to it live",
		append([]string{"sleep_intent", "sleep_reason", "slept_at", "drain_at", "detached_at", "stranded_event_emitted_at", session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey}, parityStartKeys...),
	},
}

// parityPinned are KEPT rows a registered arm implements whose owner cell
// also names an arm not yet registered, which exempts them from the gate;
// they need a fixture all the same. SESS-531's cell names A17 (the heartbeat
// hold) and SESS-539's A20, beside A6's items 4 and 6.
var parityPinned = []string{"SESS-531", "SESS-539"}

// parityKindOwners maps each reachable effect kind to the BEHAVIORS owner
// phrases it implements; the PR that makes a kind reachable adds its phrases.
var parityKindOwners = map[string]*regexp.Regexp{
	intentRowHeal: regexp.MustCompile(`\brow write\b`),
	// A6's fresh heals: the creating-row heal (SESS-062) and the
	// dead-runtime heal (SESS-531).
	intentRowHealFresh: regexp.MustCompile("\\bA6 heal of a `creating` row\\b|\\bA6 dead-runtime heal\\b"),
	intentCreate:       regexp.MustCompile(`\bC1 create\b|\bC2 named reopen\b`),
	// A19's two kinds own exactly its rows.
	intentDrainCancel: regexp.MustCompile(`\bA19\b`),
	intentDrainVoid:   regexp.MustCompile(`\bA19\b`),
	// A20's two begin kinds own its rows: the begin, its grace, its keeps
	// and the D2 policy table it shares with A19.
	intentDrainBegin:      regexp.MustCompile(`\bA20\b`),
	intentDrainBeginFresh: regexp.MustCompile(`\bA20\b`),
	// A20's signal half (C6b1) owns the same rows: the D2 table's
	// authorization and grace columns.
	intentSignal:      regexp.MustCompile(`\bA20\b`),
	intentSignalFresh: regexp.MustCompile(`\bA20\b`),
	// S4 rekeys a StaleSelf row; A3, the arm proposing it, is gated
	// through parityArmRef.
	intentRekey:       regexp.MustCompile(`\bS4\b`),
	intentLiveApply:   regexp.MustCompile(`\bA14 config drift\b`), // SESS-565's RunLive
	intentDriftRepair: regexp.MustCompile(`\bA14 config drift\b`), // SESS-566/568's repair
	// S1's start (C5a1's effect, A17/A18's kind) owns its Launch and
	// PreWake rows; S1's adopt owns no row yet.
	intentStart: regexp.MustCompile(`\bS1 start\b|\bS1 PreWake\b`),
	intentAdopt: regexp.MustCompile(`\bS1 adopt\b`),
	// A7's row-metadata verb (C7d) owns the M1 verb's rows.
	intentRowMetadata:  regexp.MustCompile(`\bM1 row-metadata verb \(A7\)`),
	intentRuntimeAlias: regexp.MustCompile(`\bM1 row-metadata verb \(A7\)`), // SESS-716's runtime alias sync
	// A8's baseline (C7c) owns the M2 builder's rows.
	intentBaseline: regexp.MustCompile(`\bM2 baseline builder \(A8\)`),
	// A10's rollback (C5a2) owns its C3 rows.
	intentRollback: regexp.MustCompile(`\bA10 \+ C3 rollback\b`),
	// A21's closes own their C3 rows: the failed-create, pool-slot and
	// orphan closes, and the dead-corpse fence.
	intentClose: regexp.MustCompile(`\bA21\b`),
}

var parityArmRef = regexp.MustCompile(`\bA(\d{1,2})\b`)

// twinProvider is the simulator's provider with the reads legacy makes
// through runtime.Fake's own session table answered from the simulator's
// runtimes instead, so both copies see one runtime model.
type twinProvider struct{ *simProvider }

func (p twinProvider) IsRunning(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rts[name] != nil
}

func (p twinProvider) ProcessAlive(name string, names []string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rts[name]
	return rt != nil && (len(names) == 0 || !rt.corpse && !rt.zombie)
}

func (p twinProvider) GetMeta(name, key string) (string, error) {
	p.mu.Lock()
	rt := p.rts[name]
	p.mu.Unlock()
	if rt != nil {
		switch key {
		case "GC_SESSION_ID":
			return rt.id, nil
		case "GC_RUNTIME_EPOCH":
			return rt.epoch, nil
		case "GC_INSTANCE_TOKEN":
			return rt.token, nil
		}
	}
	return p.Fake.GetMeta(name, key)
}

func (p twinProvider) IsAttachedWithError(name string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt := p.rts[name]; rt != nil && rt.probeErr {
		return false, fmt.Errorf("sim: attach probe of %s: %w", name, runtime.ErrRuntimeUnavailable)
	}
	return p.rts[name] != nil && p.rts[name].attached, nil
}

// parityMutators are runtime.Fake's recorded calls that change a runtime;
// Start and the destructive calls are the simulator's own records.
var parityMutators = map[string]bool{
	"Interrupt": true, "Nudge": true, "NudgeNow": true, "SetMeta": true, "RemoveMeta": true, "SendKeys": true,
	"Relaunch": true, "TerminateRuntime": true, "ClearScrollback": true, "RunLive": true, "Respond": true, "CopyTo": true,
}

// actions is the provider's action log as a multiset of "Method name".
func (p twinProvider) actions() map[string]int {
	out := map[string]int{}
	p.mu.Lock()
	for _, s := range p.starts {
		out["Start "+s.Name]++
	}
	for _, k := range p.kills {
		out[k.Method+" "+k.Name]++
	}
	p.mu.Unlock()
	for _, c := range p.SnapshotCalls() {
		if parityMutators[c.Method] {
			out[c.Method+" "+c.Name]++
		}
	}
	return out
}

// place puts f's runtimes on p, each under its row's session name.
func (f parityFixture) place(p *simProvider) {
	p.nextStart = f.StartErr
	for _, rt := range f.Runtimes {
		for _, b := range append(slices.Clone(f.Rows), f.Rig...) {
			if b.ID == rt.id {
				p.put(b.Metadata["session_name"], rt.id, rt.epoch, rt.token)
				placed := *p.rts[b.Metadata["session_name"]]
				placed.corpse, placed.zombie, placed.attached, placed.probeErr = rt.corpse, rt.zombie, rt.attached, rt.probeErr
				*p.rts[b.Metadata["session_name"]] = placed
				if slices.Contains(f.Pending, rt.id) {
					p.SetPendingInteraction(b.Metadata["session_name"], &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval"})
				}
				if slices.Contains(f.Idle, rt.id) {
					p.SetActivity(b.Metadata["session_name"], parityNow.Add(-time.Hour))
					p.WaitForIdleErrors[b.Metadata["session_name"]] = nil
				}
			}
		}
	}
}

func (f parityFixture) city() *config.City {
	if f.City == nil {
		return workerCity(3)
	}
	return f.City()
}

func (f parityFixture) beads() []beads.Bead {
	return cloneDiffBeads(append(slices.Clone(f.Rows), f.Work...))
}

// parityOutcome is one copy's end state, normalized for comparison.
type parityOutcome struct {
	rows    map[string]beads.Bead // session rows by ID
	work    map[string]beads.Bead // other beads by ID
	actions map[string]int
	events  map[string]int // "type subject [payload keys]"
	// payloads are the events with their payloads' values, "type subject
	// {json}", for the timeline differential.
	payloads map[string]int
}

// parityRandom are metadata keys whose values each side mints at random; only
// whether they are set is compared.
var parityRandom = map[string]bool{"instance_token": true}

func newParityOutcome(all []beads.Bead, sp twinProvider, rec *memRecorder) parityOutcome {
	out := parityOutcome{rows: map[string]beads.Bead{}, work: map[string]beads.Bead{}, actions: sp.actions(), events: map[string]int{}, payloads: map[string]int{}}
	for _, b := range all {
		b.Metadata = maps.Clone(b.Metadata)
		for k, v := range b.Metadata {
			if parityRandom[k] && v != "" {
				b.Metadata[k] = "<set>"
			}
			// The runtime lease record's epoch is a holder's bookkeeping
			// (SESSION-RUNTIME-012): legacy's start takes one, and v2 takes
			// its own per effect (PR B). A released record's cleared keys
			// are too; a held one still differs, which is a leaked lease.
			if strings.HasPrefix(k, "runtime_lease_") && (k == session.RuntimeLeaseEpochKey || v == "") {
				delete(b.Metadata, k)
			}
		}
		if slices.Contains(b.Labels, session.LabelSession) || b.Type == session.BeadType {
			out.rows[b.ID] = b
		} else {
			out.work[b.ID] = b
		}
	}
	rec.mu.Lock()
	for _, e := range rec.events {
		var payload map[string]any
		_ = json.Unmarshal(e.Payload, &payload)
		out.events[fmt.Sprintf("%s %s %v", e.Type, e.Subject, slices.Sorted(maps.Keys(payload)))]++
		values, _ := json.Marshal(payload) // map keys marshal sorted
		out.payloads[fmt.Sprintf("%s %s %s", e.Type, e.Subject, values)]++
	}
	rec.mu.Unlock()
	return out
}

// mismatches lists the aspects on which legacy and v2 differ, each as
// "<aspect>" with both sides' values.
func (legacy parityOutcome) mismatches(v2 parityOutcome) map[string]string {
	out := map[string]string{}
	differ := func(aspect, l, v string) {
		if l != v {
			out[aspect] = fmt.Sprintf("legacy %q, v2 %q", l, v)
		}
	}
	for _, id := range unionKeys(legacy.rows, v2.rows) {
		l, lok := legacy.rows[id]
		v, vok := v2.rows[id]
		if lok != vok {
			differ("row:"+id, fmt.Sprint(lok), fmt.Sprint(vok))
			continue
		}
		differ("row:"+id+":status", l.Status, v.Status)
		differ("row:"+id+":type", l.Type, v.Type)
		for _, k := range unionKeys(l.Metadata, v.Metadata) {
			differ("row:"+id+":"+k, l.Metadata[k], v.Metadata[k])
		}
	}
	for _, id := range unionKeys(legacy.work, v2.work) {
		l, v := legacy.work[id], v2.work[id]
		differ("work:"+id+":assignee", l.Assignee, v.Assignee)
		differ("work:"+id+":status", l.Status, v.Status)
	}
	for _, a := range unionKeys(legacy.actions, v2.actions) {
		differ("provider:"+a, fmt.Sprint(legacy.actions[a]), fmt.Sprint(v2.actions[a]))
	}
	for _, e := range unionKeys(legacy.events, v2.events) {
		differ("event:"+e, fmt.Sprint(legacy.events[e]), fmt.Sprint(v2.events[e]))
	}
	return out
}

// parityTicks bounds both copies' runs to a fixed point.
const parityTicks = 6

// parityNow is both copies' clock: the simulator's after its first second.
var parityNow = plannerT0.Add(time.Second)

// legacyWorld is copy A: a store, the provider, and the state legacy's
// ticks carry between them.
type legacyWorld struct {
	cfg      *config.City
	cityPath string
	store    *beads.MemStore
	sp       twinProvider
	clk      *clock.Fake
	rec      *memRecorder
	dt       *drainTracker
	rigs     map[string]beads.Store // the rig work stores a timeline fixture seeds; nil otherwise
	dops     drainOps
	stops    asyncStartTracker // the drain-ack's async stops, as the controller's asyncStops
	pokes    *atomic.Int64     // the async stops' completion pokes, when the timeline counts them
}

func newLegacyWorld(t testing.TB, f parityFixture, cityPath string, rows []beads.Bead) *legacyWorld {
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
	clk := &clock.Fake{Time: parityNow}
	sim := &simProvider{Fake: runtime.NewFake(), rts: make(map[string]*simRuntime), changed: make(map[string]uint64), now: clk.Now}
	f.place(sim)
	w := &legacyWorld{
		cfg: f.city(), cityPath: cityPath, store: beads.NewMemStoreFrom(0, rows, nil), sp: twinProvider{sim},
		clk: clk, rec: &memRecorder{}, dt: newDrainTracker(),
	}
	w.dops = newDrainOps(w.sp)
	for id, reason := range f.Drains {
		// The drain's basis is the seeded row, as beginSessionDrainInfo records it.
		var basis session.Decided
		if i := slices.IndexFunc(rows, func(b beads.Bead) bool { return b.ID == id }); i >= 0 {
			basis = session.Decide(sessionInfoFromBead(rows[i]), session.FactsLegacyStopPending)
		}
		w.dt.set(id, &drainState{startedAt: parityDrainAt, reason: reason, generation: 1, basis: basis})
	}
	return w
}

// parityDrainAt is when every fixture's drain began.
var parityDrainAt = parityNow.Add(-time.Minute)

// projectDrains rewrites each row's stop-request keys from legacy's drain
// tracker: the drain it still holds, or none.
func (w *legacyWorld) projectDrains(all []beads.Bead) {
	for i, b := range all {
		if !slices.Contains(b.Labels, session.LabelSession) {
			continue
		}
		meta := maps.Clone(b.Metadata)
		meta[session.DrainIntentReasonKey], meta[session.DrainIntentAtKey], meta[session.DrainIntentIncarnationKey] = "", "", ""
		if ds := w.dt.get(b.ID); ds != nil {
			meta[session.DrainIntentReasonKey], meta[session.DrainIntentAtKey] = ds.reason, ds.startedAt.UTC().Format(time.RFC3339)
			meta[session.DrainIntentIncarnationKey] = strconv.Itoa(ds.generation)
		}
		all[i].Metadata = meta
	}
}

// list is the city store's beads, then each rig store's.
func (w *legacyWorld) list(t *testing.T) []beads.Bead {
	all, err := w.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, rig := range slices.Sorted(maps.Keys(w.rigs)) {
		rows, err := w.rigs[rig].List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
	}
	return all
}

// tick is one legacy tick, as the controller's: the desired-state build
// (which also creates pool rows), the session-bead sync, and, when
// reconcile is set, the session reconciler over the synced rows.
func (w *legacyWorld) tick(t *testing.T, reconcile bool) {
	cfg, now := w.cfg, w.clk.Now()
	// The tick's one read of the work legs, as runTickPhases mints it.
	sw := testSeatWork(w.cityPath, cfg, w.store, w.rigs)
	snap, err := loadSessionBeadSnapshot(w.store)
	if err != nil {
		t.Fatal(err)
	}
	if reconcile {
		// tickFinalizeDrainAckStopPending runs before the demand build.
		finalizeDrainAckStopPendingSessions(w.cityPath, cfg, w.sp, beads.SessionStore{Store: w.store}, sw, snap.OpenInfos(), w.dops, w.dt, &w.stops, w.clk, w.rec, io.Discard)
		w.stops.wg.Wait()
		if snap, err = loadSessionBeadSnapshot(w.store); err != nil {
			t.Fatal(err)
		}
	}
	result := buildDesiredStateWithSessionBeadsAt(cfg.Workspace.Name, w.cityPath, now, now, cfg, w.sp, w.store, w.rigs, snap, nil, io.Discard)
	cfgNames := configuredSessionNamesWithSnapshot(cfg, cfg.Workspace.Name, snap)
	_, updated := syncSessionBeadsWithSnapshotAndRigStores(w.cityPath, beads.SessionStore{Store: w.store}, sw, result.State, w.sp, cfgNames, cfg, w.clk, io.Discard, true, snap, nil)
	if !reconcile {
		return
	}
	// beadReconcileTick (city_runtime.go), in its order: the orphan release
	// over the wake candidates, the pool demand, the undesired-pool sweep,
	// then the reconciler with the controller's inputs (the assigned work
	// filtered for wake with its ready flags and legs, real drain ops, and
	// the drain-ack's async stop tracker).
	open := updated.OpenInfos()
	assigned, refs, stores := result.AssignedWorkBeads, result.AssignedWorkStoreRefs, result.AssignedWorkStores
	preWake, preWakeRefs := filterAssignedWorkBeadsForSessionWake(cfg, w.cityPath, w.store, open, assigned, refs)
	result.AssignedWorkBeads = assigned
	if released := releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(w.store, beads.SessionStore{Store: w.store}, cfg, w.cityPath, open, result, w.rigs, protectedWakeWorkKeys(preWake, preWakeRefs), nil, nil); len(released) > 0 {
		emitDeadAssigneeReopenedEvents(w.rec, assigned, released, now)
		assigned, refs, stores = filterReleasedAssignedWorkSnapshot(assigned, refs, stores, released)
	}
	poolDesired := result.PoolDesiredCounts
	if poolDesired == nil {
		work := filterAssignedWorkBeadsForPoolDemand(cfg, w.cityPath, w.store, open, assigned, refs)
		poolDesired = retainScaleCheckPartialPoolDesired(cfg, PoolDesiredCounts(ComputePoolDesiredStatesAt(cfg, work, open, result.ScaleCheckCounts, now)),
			updated, effectivePoolPartialRetentionTemplates(result))
	}
	if poolDesired == nil {
		poolDesired = map[string]int{}
	}
	mergeNamedSessionDemand(poolDesired, result.NamedSessionDemand, cfg)
	if sweepUndesiredPoolSessionBeads(beads.SessionStore{Store: w.store}, sw, updated, result.State, cfg, w.sp, result.snapshotQueryPartial(), w.clk) > 0 {
		if updated, err = loadSessionBeadSnapshot(w.store); err != nil {
			t.Fatal(err)
		}
		open = updated.OpenInfos()
	}
	// The wait wake state (prepareWaitWakeStateWithSnapshot), as the tick
	// prepares it: ready waits wake their sleeping sessions.
	deps := waitDependencyReaderFunc(func(id string) (beads.Bead, error) { return loadWaitDependencyBead("", w.store, id) })
	readyWaits, err := prepareWaitWakeStateWithSnapshot(sessionFrontDoor(w.store), deps, beads.NudgesStore{Store: w.store}, now, updated)
	if err != nil {
		t.Fatalf("legacy prepare waits: %v", err)
	}
	gates := w.gateIdleProbes()
	awake, awakeRefs, awakeStores := filterAssignedWorkBeadsForSessionWakeWithStores(cfg, w.cityPath, w.store, open, assigned, refs, stores)
	reconcileSessionBeadsAtPathWithNamedDemand(context.Background(), w.cityPath, updated.OpenForReconcile(), updated, result.State, cfgNames, cfg, w.sp, w.store, sw,
		w.dops, awake, w.rigs, readyWaits, w.dt, nil, poolDesired, result.NamedSessionDemand, result.NamedSessionRoutedDemand,
		result.snapshotQueryPartial(), map[string]bool{}, cfg.Workspace.Name, nil, w.clk, w.rec, cfg.Session.StartupTimeoutDuration(), cfg.Daemon.DriftDrainTimeoutDuration(),
		io.Discard, io.Discard,
		withReadyAssignedFlags(readyAssignedFlagsForBeads(result.ReadyAssigned, awake, awakeRefs)), withAssignedWorkStores(awakeStores),
		withAsyncDrainAckStopTracker(&w.stops),
		withStartStabilityWaiter(immediateStartStabilityWaiter), withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter))
	for _, gate := range gates {
		close(gate)
	}
	w.awaitIdleProbes(t)
	w.stops.wg.Wait()
}

// followUp reports, and consumes, the controller's reasons for an
// immediate follow-up tick: a deferred drain signal
// (requestDeferredDrainFollowUpTick), or a drain-ack async stop's
// completion poke (drainAckAsyncStopPokeController), which pokes counts.
func (w *legacyWorld) followUp(pokesBefore int64) bool {
	return w.dt.consumeFollowUpTick() || w.pokes != nil && w.pokes.Load() != pokesBefore
}

// pokesNow is the async stops' completion pokes so far.
func (w *legacyWorld) pokesNow() int64 {
	if w.pokes == nil {
		return 0
	}
	return w.pokes.Load()
}

// gateIdleProbes holds every idle probe the fake answers until the tick
// ends, as a real probe outlasts the tick that launched it; a probe that
// finished inside its own tick would make the drain's begin tick a race.
// The gate is deliberate: the probe's verdict lands a tick (a patrol in the
// timeline) after its launch, so a timeline's legacy idle drain begins a
// patrol late against an idle probe that answered at once. C6a2's
// comparison of idle-drain begin times allows ±1 patrol for it.
func (w *legacyWorld) gateIdleProbes() []chan struct{} {
	// No probe runs between ticks, so the maps need no lock here.
	var gates []chan struct{}
	for name := range w.sp.WaitForIdleErrors {
		gate := make(chan struct{})
		w.sp.WaitForIdleGates[name] = gate
		gates = append(gates, gate)
	}
	return gates
}

// awaitIdleProbes waits out the idle probes a tick launched, their gates
// open: they finish on goroutines of the tick's, so the next tick sees each
// probe finished whatever the scheduler does. It yields rather than sleeps;
// simGuard turns a hang into a failure.
func (w *legacyWorld) awaitIdleProbes(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(simGuard); ; goruntime.Gosched() {
		w.dt.mu.Lock()
		pending := slices.ContainsFunc(slices.Collect(maps.Values(w.dt.idleProbes)), func(p *idleProbeState) bool { return p != nil && !p.ready })
		w.dt.mu.Unlock()
		if !pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("an idle probe never finished")
		}
	}
}

// sameParityBead is sameBead plus the type and assignee.
func sameParityBead(a, b beads.Bead) bool {
	return a.ID == b.ID && a.Type == b.Type && a.Assignee == b.Assignee && sameBead(a, b)
}

// converged is f's beads with each row's metadata as legacy's session-bead
// sync leaves it, the fixture's own keys kept: a city at cutover has
// converged rows, so a fixture shows the arms under test rather than the
// metadata convergence that arm A7 (C7d) owns. The rows the build creates are
// dropped; the copies' runs create their own.
func (f parityFixture) converged(t *testing.T, cityPath string) []beads.Bead {
	w := newLegacyWorld(t, f, cityPath, f.beads())
	w.tick(t, false)
	synced := map[string]beads.Bead{}
	for _, b := range w.list(t) {
		synced[b.ID] = b
	}
	out := f.beads()
	for i, b := range out {
		if !slices.Contains(b.Labels, session.LabelSession) {
			continue
		}
		meta := maps.Clone(synced[b.ID].Metadata)
		maps.Copy(meta, b.Metadata)
		if reason, ok := f.Drains[b.ID]; ok {
			meta[session.DrainIntentReasonKey], meta[session.DrainIntentAtKey], meta[session.DrainIntentIncarnationKey] = reason, parityDrainAt.Format(time.RFC3339), "1"
		}
		out[i].Metadata = meta
	}
	return out
}

// legacyParity runs copy A from rows until a tick changes no bead and calls
// no provider verb.
func legacyParity(t *testing.T, f parityFixture, cityPath string, rows []beads.Bead) parityOutcome {
	t.Helper()
	return newLegacyWorld(t, f, cityPath, cloneDiffBeads(rows)).settle(t, f.Name)
}

// settle runs legacy ticks until one changes no bead and calls no provider
// verb, and returns the outcome: the city store's beads, then each rig
// store's.
func (w *legacyWorld) settle(t *testing.T, name string) parityOutcome {
	t.Helper()
	for tick := 0; w.once(t); tick++ {
		if tick == parityTicks {
			t.Fatalf("%s: legacy reached no fixed point in %d ticks", name, parityTicks)
		}
	}
	return w.outcome(t)
}

// once runs one tick and reports whether it changed a bead on any leg,
// called a provider verb, or moved the drain tracker.
func (w *legacyWorld) once(t *testing.T) bool {
	before, acted, drains := w.list(t), w.sp.actions(), w.drains()
	for i := 0; ; i++ {
		pokes := w.pokesNow()
		w.tick(t, true)
		if !w.followUp(pokes) {
			break
		}
		if i == parityTicks {
			t.Fatalf("legacy asked for a follow-up tick %d times at %s", parityTicks, w.clk.Now())
		}
	}
	return !slices.EqualFunc(before, w.list(t), sameParityBead) || !maps.Equal(acted, w.sp.actions()) || !maps.Equal(drains, w.drains())
}

// drains is the drain tracker's drains, rendered. Its idle probes are not:
// a steady tick relaunches and consumes them.
func (w *legacyWorld) drains() map[string]string {
	w.dt.mu.Lock()
	defer w.dt.mu.Unlock()
	out := map[string]string{}
	for id, ds := range w.dt.drains {
		out[id] = fmt.Sprintf("%+v", *ds)
	}
	return out
}

// outcome is legacy's outcome now, its drain tracker projected onto the rows.
func (w *legacyWorld) outcome(t *testing.T) parityOutcome {
	all := w.list(t)
	w.projectDrains(all)
	return newParityOutcome(all, w.sp, w.rec)
}

// v2Parity runs copy B from rows on the simulator, every effect released at
// once, until a pass admits nothing and writes nothing. The simulator's
// invariants hold throughout.
func v2Parity(t *testing.T, f parityFixture, cityPath string, rows []beads.Bead) parityOutcome {
	t.Helper()
	return newV2World(t, f, cityPath, rows, nil, "", nil).settle(t, f.Name)
}

// v2World is copy B: the simulator, its provider and its recorder.
type v2World struct {
	s       *sim
	sp      twinProvider
	rec     *memRecorder
	pending []string
	legs    int // the legs an outcome lists: the city leg, or every leg once a rig is seeded
	// lane runs K1's steps the timeline needs after each pass, as the
	// controller's lane does: orphan release every pass, and the demand
	// repairs every externalReadsRepairInterval since repaired.
	lane     *externalReadsLane
	repaired time.Time
}

// newV2World seeds rows on the city leg and rig on the rig leg, whose path is
// rigPath when set, and moves the clock to parityNow. arms, when set, edits
// the row arms (a v2-only mutant).
func newV2World(t *testing.T, f parityFixture, cityPath string, rows, rig []beads.Bead, rigPath string, arms func([]rowArm) []rowArm) *v2World {
	t.Helper()
	s := newSim(t, 1, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
		f.place(s.sp)
		return cloneDiffBeads(rows), cloneDiffBeads(rig)
	}, arms: arms})
	s.lag, s.env.CityPath = false, cityPath
	sp := twinProvider{s.sp}
	cfg := f.city()
	cfg.Daemon.PatrolInterval, cfg.Daemon.ProbeConcurrency, cfg.Rigs = s.cfg.Daemon.PatrolInterval, s.cfg.Daemon.ProbeConcurrency, s.cfg.Rigs
	if rigPath != "" {
		cfg.Rigs[0].Path = rigPath
	}
	*s.cfg = *cfg
	s.env.Env().SP = sp
	w := &v2World{s: s, sp: sp, rec: &memRecorder{}, pending: f.Pending, legs: 1}
	if rig != nil {
		w.legs = len(s.legs)
	}
	s.p.rec = w.rec
	creates, err := newCreateEffects(createEffectHost{cityPath: cityPath, cityName: s.env.CityName, lookPath: s.env.LookPath, now: s.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	s.p.creates = creates
	w.lane = newExternalReadsLane(simPatrol, s.env.externalReadsEnv, nil, nil, w.rec, io.Discard)
	w.lane.now = s.clk.Now
	w.lane.addPoolSteps(s.p.out.summary.Load, nil)
	// K1's waits step, whose ready waits the allocation reads (I10).
	w.lane.addWaitsStep()
	s.env.Nudges = func() beads.NudgesStore { return beads.NudgesStore{Store: s.legs[0].cache} }
	s.env.ReadyWaits = w.lane.readyWaitSet
	s.advance(parityNow.Sub(s.clk.Now()))
	return w
}

// settle runs passes, every effect released at once, until one admits
// nothing and writes nothing. The simulator's invariants hold throughout.
func (w *v2World) settle(t *testing.T, name string) parityOutcome {
	t.Helper()
	for pass := 0; w.once(t); pass++ {
		if pass == parityTicks {
			t.Fatalf("%s: v2 reached no fixed point in %d passes", name, parityTicks)
		}
	}
	return w.outcome(t)
}

// once runs one pass, every effect released at once, and reports whether it
// admitted an intent or changed a bead.
func (w *v2World) once(t *testing.T) bool {
	s := w.s
	before := s.snapshot()
	s.notIdle = nil
	s.inventory()
	for _, id := range w.pending {
		s.lane.cache.Note("s-"+id, FactPending, ObsYes, s.clk.Now(), SourceProbe, "")
	}
	env, err := s.env.externalReadsEnv()
	if err != nil {
		t.Fatalf("v2 lane env: %v", err)
	}
	w.lane.waits(context.Background(), env)
	s.pass()
	admitted := intentKeys(s.p.out.record.Load().Admitted)
	for len(s.parked) > 0 {
		s.release(0)
	}
	for _, st := range s.p.settlements.drain() { // the facts the next pass records first, recorded at their tick
		s.p.applyFacts(st.Facts, s.clk.Now())
		st.Facts = effectFacts{}
		s.p.settlements.post(st)
	}
	if env, err = s.env.externalReadsEnv(); err != nil {
		t.Fatalf("v2 lane env: %v", err)
	}
	w.lane.orphanRelease(context.Background(), env)
	if now := s.clk.Now(); w.repaired.IsZero() || now.Sub(w.repaired) >= externalReadsRepairInterval {
		runBackstopDemandRepairs(context.Background(), env, io.Discard)
		w.repaired = now
	}
	s.audit("v2")
	// An idle probe that found the agent busy acts on nothing, as legacy's
	// probes, which its drains leave out.
	admitted = slices.DeleteFunc(admitted, func(k string) bool { return slices.Contains(s.notIdle, k) })
	return len(admitted) > 0 || !maps.EqualFunc(before, s.snapshot(), sameParityBead)
}

// outcome is v2's outcome now; the simulator's invariants must hold.
func (w *v2World) outcome(t *testing.T) parityOutcome {
	t.Helper()
	s := w.s
	s.noViolations(t)
	var all []beads.Bead
	for _, l := range s.legs[:w.legs] {
		rows, err := l.backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
	}
	return newParityOutcome(all, w.sp, w.rec)
}

// parityEntryOf finds id in the three tables.
func parityEntryOf(id string) (table string, e parityEntry, ok bool) {
	for name, tbl := range map[string]map[string]parityEntry{"accepted": parityAccepted, "finding": parityFindings, "unported": parityUnported} {
		if e, ok := tbl[id]; ok {
			return name, e, true
		}
	}
	return "", parityEntry{}, false
}

// explains is the Explain key that accounts for aspect: the aspect itself,
// or a wildcard "row:<id>:*<label>" whose entry lists the aspect's field. A
// timeline fixture's aspect and key may lead with "@<step> ".
func (f parityFixture) explains(aspect string) (string, bool) {
	if _, ok := f.Explain[aspect]; ok {
		return aspect, true
	}
	i := strings.LastIndex(aspect, ":")
	for _, key := range slices.Sorted(maps.Keys(f.Explain)) {
		if prefix, _, wild := strings.Cut(key, "*"); wild && (strings.Contains(prefix, "row:") || strings.Contains(prefix, "work:")) && prefix == aspect[:i+1] {
			field := aspect[i+1:]
			if strings.HasPrefix(field, "core_hash_breakdown.") { // a breakdown field is the breakdown's
				field = "core_hash_breakdown"
			}
			if _, e, ok := parityEntryOf(f.Explain[key]); ok && slices.Contains(e.keys, field) {
				return key, true
			}
		}
	}
	return "", false
}

// checkParity compares f's two copies: f must account for every mismatch
// with an entry, and every aspect it accounts for must show.
func checkParity(t *testing.T, f parityFixture) {
	t.Helper()
	cityPath := t.TempDir()
	rows := f.converged(t, cityPath)
	got := legacyParity(t, f, cityPath, rows).mismatches(v2Parity(t, f, cityPath, rows))
	shown := map[string]bool{}
	for _, aspect := range slices.Sorted(maps.Keys(got)) {
		key, ok := f.explains(aspect)
		if !ok {
			t.Errorf("%s: unexplained difference %s (%s)", f.Name, aspect, got[aspect])
			continue
		}
		shown[key] = true
		switch table, e, ok := parityEntryOf(f.Explain[key]); {
		case !ok:
			t.Errorf("%s: %s accounted to %q, which no table lists", f.Name, aspect, f.Explain[key])
		case table == "finding":
			t.Logf("%s: %s is open finding %s (%s): %s", f.Name, aspect, f.Explain[key], e.ref, e.reason)
		}
	}
	for key, id := range f.Explain {
		if !shown[key] {
			t.Errorf("%s: %s, accounted to %s, no longer shows: legacy and v2 agree", f.Name, key, id)
		}
	}
}

// parityFixtures are the fixtures, one or more per KEPT row of a registered
// arm or effect kind.
func parityFixtures() []parityFixture {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	live := simRuntime{id: "gc-1", epoch: "1", token: "tok-gc-1"}
	awake := func(meta ...string) parityRow {
		return poolRow("gc-1", "worker", 1, "awake", append([]string{"instance_token", "tok-gc-1", "last_woke_at", at(-time.Minute)}, meta...)...)
	}
	assigned := []parityRow{{ID: "gw-1", Title: "gw-1", Type: "task", Status: "in_progress", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}}
	napping := func() *config.City {
		cfg := workerCity(3)
		cfg.Agents[0].SleepAfterIdle = "5m"
		return cfg
	}
	// chat is a city with an always named session; legacy preserves its
	// row outside the desired state only while the city is suspended.
	chat := func(suspended bool) func() *config.City {
		return func() *config.City {
			cfg := workerCity(3)
			cfg.Workspace.Suspended = suspended
			cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat", StartCommand: "true"})
			cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
			return cfg
		}
	}
	// chatNapping is chat's city, its named session idle-sleeping after five
	// minutes.
	chatNapping := func() *config.City {
		cfg := chat(false)()
		cfg.Agents[1].SleepAfterIdle = "5m"
		return cfg
	}
	// onDemand is chat's on-demand named row, awake, with meta; chatLive its
	// runtime; chatStanding its standing findings.
	onDemand := func(meta ...string) parityRow {
		return chatRow("gc-c", "1", append([]string{"state", "awake", "session_name", "chat", "configured_named_mode", "on_demand", "instance_token", "tok-gc-c"}, meta...)...)
	}
	chatLive := simRuntime{id: "gc-c", epoch: "1", token: "tok-gc-c"}
	chatStanding := map[string]string{"row:gc-c:*policy": "sleep-policy-keys"}
	// stopsInTick is legacy's same-tick signal, stop and sleep of the named
	// session after its begin, which v2 leaves to A4 (C6b1, C6b2); v2's begin
	// writes the stop request.
	stopsInTick := func(entry string) map[string]string {
		return map[string]string{
			"row:gc-c:*policy": "sleep-policy-keys", "row:gc-c:*stop": entry,
			"row:gc-c:state": entry, "row:gc-c:last_woke_at": entry, "row:gc-c:detached_at": entry,
			"provider:SetMeta chat": entry, "provider:RemoveMeta chat": entry, "provider:Stop chat": entry,
			"event:session.stopped chat [reason session_id template]": entry,
		}
	}
	// latchCompared replaces explain's sleep-policy wildcard for gc-c with
	// the policy keys that differ there, by name, so the latch value
	// (sleep_policy_fingerprint) is compared.
	latchCompared := func(explain map[string]string) map[string]string {
		out := maps.Clone(explain)
		delete(out, "row:gc-c:*policy")
		for _, k := range []string{"requested_sleep_after_idle", "effective_sleep_after_idle", "sleep_policy_source", "sleep_capability"} {
			out["row:gc-c:"+k] = "sleep-policy-keys"
		}
		return out
	}
	// chatOnDemand is chat's city with its named session on demand, idle
	// after five minutes.
	chatOnDemand := func() *config.City {
		cfg := chat(false)()
		cfg.Agents[1].SleepAfterIdle = "5m"
		cfg.NamedSessions[0].Mode = "on_demand"
		return cfg
	}
	// suspendedWorker is the worker city with its agent suspended in config.
	suspendedWorker := func() *config.City {
		cfg := workerCity(3)
		cfg.Agents[0].Suspended = true
		return cfg
	}
	// floor keeps one worker seat desired with no work; drifted is a stored
	// config hash this generation's never equals, under its version.
	floor := func() *config.City {
		cfg := workerCity(3)
		cfg.Agents[0].MinActiveSessions = intPtr(1)
		return cfg
	}
	drifted := runtime.FingerprintVersion + ":drifted"
	// driftDrained is legacy finishing the drift drain in its ticks (the
	// signal, the stop, the drained seat's retirement and its replacement),
	// where v2's A14 writes the stop request and has no signal or stop yet.
	driftDrained := map[string]string{"row:gc-1:*stop": "A4 drain-stop", "row:gc-2": "A4 drain-stop"}
	for _, k := range []string{
		"row:gc-1:state", "row:gc-1:status", "row:gc-1:closed_at", "row:gc-1:close_reason",
		"provider:SetMeta s-gc-1", "provider:RemoveMeta s-gc-1", "provider:Stop s-gc-1", "provider:Start worker-gc-2", "provider:RemoveMeta worker-gc-2",
		"event:session.stopped worker [reason session_id template]", "event:session.woke worker-1 []",
	} {
		driftDrained[k] = "A4 drain-stop"
	}
	// explain merges the alive wanted row gc-1's standing findings with more.
	explain := func(more map[string]string) map[string]string {
		out := map[string]string{"row:gc-1:*policy": "sleep-policy-keys"}
		maps.Copy(out, more)
		return out
	}
	// started is a row id both copies start under name, with legacy's
	// sleep-policy keys on it and its runtime metadata clears (§12.2 #38).
	started := func(id, name, _ string, more map[string]string) map[string]string {
		out := map[string]string{"row:" + id + ":*policy": "sleep-policy-keys", "provider:RemoveMeta " + name: "§12.2#38 start-meta-clears"}
		maps.Copy(out, more)
		return out
	}
	return []parityFixture{
		{
			// INC-024: a heal never reverts a durable suspend; the row write
			// re-decides in its CAS (R2). (Legacy's own state heal turns the
			// stopped suspended row asleep under its hold: mc-92clf.)
			Name: "an expired quarantine heals on a suspended row, which stays suspended", Behaviors: []string{"INC-024"},
			City:    chat(false),
			Rows:    []parityRow{chatRow("gc-c", "1", "session_name", "chat", "state", "suspended", "sleep_intent", "user-hold", "held_until", at(time.Hour), "quarantined_until", at(-time.Minute))},
			Explain: map[string]string{"row:gc-c:*policy": "sleep-policy-keys", "row:gc-c:state": "suspended-row-heal"},
		},
		{
			Name: "an expired hold heals on an awake row with assigned work", Behaviors: []string{"SESS-009", "SESS-613"},
			Rows: []parityRow{awake("held_until", at(-time.Minute), "sleep_reason", "user-hold")}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "an expired hold, then an expired quarantine against the post-hold reason", Behaviors: []string{"SESS-010"},
			Rows: []parityRow{awake("held_until", at(-2*time.Minute), "quarantined_until", at(-time.Minute), "sleep_reason", "user-hold", "wake_attempts", "3")}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			// The undesired-pool sweep (MAINT-051) closes a workless dead pool
			// row before the reconciler heals its expired hold; v2 heals the
			// hold, then A21 closes the row (no deliberate hold is in force).
			Name: "a workless suspended pool row is swept before its hold heals", Behaviors: []string{"MAINT-051"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "suspended", "held_until", at(-time.Minute), "sleep_reason", "user-hold", "suspended_at", at(-time.Hour))},
			Explain: map[string]string{
				"row:gc-1:*reason": "§12.2#17 orphan-close-reason", "row:gc-1:held_until": "§12.2#43 close-after-hold-heal", "row:gc-1:sleep_reason": "§12.2#43 close-after-hold-heal",
			},
		},
		{
			// In a suspended city: legacy heals the expired hold from a fresh
			// snapshot (SESS-009), then its state heal (SESS-531) turns the
			// row asleep; v2 heals the hold and keeps the suspend.
			Name: "a suspended named row's expired hold heals, and legacy turns it asleep", Behaviors: []string{"SESS-009", "SESS-531"}, City: chat(true),
			Rows:    []parityRow{chatRow("gc-c", "1", "session_name", "chat", "state", "suspended", "held_until", at(-time.Minute), "sleep_reason", "user-hold", "suspended_at", at(-time.Hour))},
			Explain: map[string]string{"row:gc-c:*policy": "sleep-policy-keys", "row:gc-c:state": "suspended-named-heal"},
		},
		{
			Name: "an unknown state is skipped", Behaviors: []string{"SESS-044", "SESS-045"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "archived-v9")}, Work: assigned,
			Explain: map[string]string{"row:gc-1:*marker": "unknown-state-diagnostic", "event:session.unknown_state gc-1 [escalated first_seen session_id session_name state]": "unknown-state-diagnostic", "event:reconciler.alert gc-1 [alert]": "unknown-state-diagnostic"},
		},
		{
			Name: "an unknown state first seen 31 minutes ago", Behaviors: []string{"SESS-046"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "archived-v9", "unknown_state_first_seen", at(-31*time.Minute), "unknown_state_value", "archived-v9")}, Work: assigned,
			Explain: map[string]string{"row:gc-1:*marker": "unknown-state-diagnostic", "event:session.unknown_state gc-1 [escalated first_seen session_id session_name state]": "unknown-state-diagnostic", "event:reconciler.alert gc-1 [alert]": "unknown-state-diagnostic"},
		},
		{
			Name: "a known state clears the unknown-state markers", Behaviors: []string{"SESS-047"},
			Rows: []parityRow{awake("unknown_state_first_seen", at(-time.Hour), "unknown_state_value", "archived-v9")}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "a creating row with no claim whose runtime is gone", Behaviors: []string{"SESS-062"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-gc-1", "quarantined_until", at(time.Hour))}, Work: assigned,
			Explain: map[string]string{"row:gc-1:*policy": "sleep-policy-keys"},
		},
		{
			Name: "an awake row whose runtime is gone", Behaviors: []string{"SESS-531", "SESS-624"},
			Rows: []parityRow{awake("last_woke_at", at(-2*time.Hour), "quarantined_until", at(time.Hour), "session_key", "conversation-1", "started_config_hash", "v6:abc")}, Work: assigned,
			Explain: map[string]string{"row:gc-1:*policy": "sleep-policy-keys"},
		},
		{
			Name: "a stranded row past its confirmation window is repaired", Behaviors: []string{"SESS-625"},
			Rows: []parityRow{awake("last_woke_at", at(-2*time.Hour), "quarantined_until", at(time.Hour), "stranded_event_emitted_at", at(-time.Hour))}, Work: assigned,
			Explain: map[string]string{"row:gc-1:*policy": "sleep-policy-keys"},
		},
		{
			Name: "a workless idle pool slot whose runtime is gone closes", Behaviors: []string{"SESS-626"},
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour))},
			Explain: map[string]string{"row:gc-1:*reason": "§12.2#17 orphan-close-reason"},
		},
		{
			// SESS-060's half, no close on a partial pass, has no fixture
			// shape (the harness injects no partial read): TestArmCloseDecides
			// pins it ("nothing on a partial pass").
			Name: "a failed create closes", Behaviors: []string{"SESS-060", "SESS-061"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "failed-create")},
		},
		{
			Name: "a configured named row's failed create closes", Behaviors: []string{"SESS-702"}, City: chat(false),
			Rows:    []parityRow{chatRow("gc-c", "1", "session_name", "chat", "state", "failed-create")},
			Explain: started("gc-1", "chat", "chat", nil),
		},
		{
			Name: "an orphan whose runtime is gone closes", Behaviors: []string{"SESS-081", "SESS-083", "SESS-722"},
			Rows: []parityRow{poolRow("gc-1", "gone", 1, "asleep", "slept_at", at(-time.Hour))},
		},
		{
			Name: "an orphan releases its claim, then closes", Behaviors: []string{"SESS-082"},
			Rows: []parityRow{poolRow("gc-1", "gone", 1, "awake", "instance_token", "tok-gc-1", "last_woke_at", at(-time.Hour))}, Work: assigned,
			Explain: map[string]string{
				"row:gc-1:*close": "§12.2#42 orphan-claim-release-unconfigured", "row:gc-1:state": "§12.2#42 orphan-claim-release-unconfigured", "work:gw-1:assignee": "§12.2#42 orphan-claim-release-unconfigured", "work:gw-1:status": "§12.2#42 orphan-claim-release-unconfigured",
				"event:bead.dead_assignee_reopened gw-1 [bead_id dead_assignee routed_to]": "§12.2#42 orphan-claim-release-unconfigured", "row:gc-2": "§12.2#42 orphan-claim-release-unconfigured",
				"provider:Start worker-gc-2": "§12.2#42 orphan-claim-release-unconfigured", "event:session.woke worker-1 []": "§12.2#42 orphan-claim-release-unconfigured",
			},
		},
		{
			// v2 drains the seat as suspended where legacy classifies it
			// orphaned; both release the served claim and close (§12.2 #17).
			Name: "an agent-suspended pool seat releases its claim, then closes", Behaviors: []string{"SESS-082"},
			City: func() *config.City { cfg := workerCity(3); cfg.Agents[0].Suspended = true; return cfg },
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "awake", "instance_token", "tok-gc-1", "last_woke_at", at(-time.Hour))}, Work: assigned,
			Explain: map[string]string{"row:gc-1:*reason": "§12.2#17 orphan-close-reason"},
		},
		{
			Name: "an undesired pool row under a live user hold keeps its slot", Behaviors: []string{"MAINT-051"},
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_intent", "user-hold", "sleep_reason", "user-hold", "held_until", at(time.Hour), "slept_at", at(-time.Hour))},
			Explain: map[string]string{"row:gc-1:*kept": "§12.2#27 held-slot-kept"},
		},
		{
			Name: "an undesired pool row in quarantine keeps its slot", Behaviors: []string{"MAINT-051"},
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "quarantine", "quarantined_until", at(time.Hour), "slept_at", at(-time.Hour))},
			Explain: map[string]string{"row:gc-1:*kept": "§12.2#27 held-slot-kept"},
		},
		{
			Name: "a desired floor seat asleep with no wake reason stays open", Behaviors: []string{"SESS-626"},
			City:    func() *config.City { cfg := workerCity(3); cfg.Agents[0].MinActiveSessions = intPtr(1); return cfg },
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "no-wake-reason", "slept_at", at(-time.Hour))},
			Explain: started("gc-2", "worker-gc-2", "worker-2", map[string]string{"row:gc-1:*policy": "sleep-policy-keys"}),
		},
		{
			Name: "a desired floor seat asleep on config drift stays open", Behaviors: []string{"SESS-626"},
			City:    func() *config.City { cfg := workerCity(3); cfg.Agents[0].MinActiveSessions = intPtr(1); return cfg },
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "config-drift", "slept_at", at(-time.Hour))},
			Explain: started("gc-2", "worker-gc-2", "worker-2", map[string]string{"row:gc-1:*policy": "sleep-policy-keys"}),
		},
		{
			Name: "a dead phantom of a configured name closes and keeps the identity's work", Behaviors: []string{"SESS-080"}, City: chat(false),
			Rows:    []parityRow{sessionRow("gc-p", "template", "keeper", "session_name", "chat", "configured_named_session", "true", "state", "asleep", "generation", "1")},
			Work:    []parityRow{{ID: "gw-1", Title: "gw-1", Type: "task", Status: "open", Assignee: "chat", Metadata: map[string]string{}}},
			Explain: started("gc-1", "chat", "chat", map[string]string{"row:gc-p:*reason": "§12.2#17 orphan-close-reason"}),
		},
		{
			Name: "an idle killed pool seat closes", Behaviors: []string{"SESS-623"},
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "killed", "slept_at", at(-time.Hour))},
			Explain: map[string]string{"row:gc-1:*reason": "§12.2#17 orphan-close-reason"},
		},
		{
			// MAINT-031 is C5c2's: A21 never closes a row that claims a live
			// runtime (C3), so the corpse stays and the row with it.
			Name: "a corpse on an unwanted row that claims a live runtime", Behaviors: []string{"MAINT-031"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", at(-time.Hour))}, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", corpse: true}},
			Explain: map[string]string{"row:gc-1:*close": "dead-corpse-close"},
		},
		{
			Name: "a corpse on an unwanted row is removed, and the row closes", Behaviors: []string{"GUAR-022", "INC-025"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-gc-1", "slept_at", at(-time.Hour))}, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", corpse: true}},
			Explain: map[string]string{"row:gc-1:*reason": "§12.2#17 orphan-close-reason", "provider:KillCorpseObject s-gc-1": "§12.2#41 close-removes-corpse"},
		},
		{
			Name: "detached_at clears when idle sleep is off", Behaviors: []string{"SESS-533"},
			Rows: []parityRow{awake("detached_at", at(-time.Hour))}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "detached_at clears while attached", Behaviors: []string{"SESS-535"}, City: napping,
			Rows: []parityRow{awake("detached_at", at(-time.Hour))}, Work: assigned, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", attached: true}},
			Explain: explain(nil),
		},
		{
			Name: "detached_at is stamped on detach", Behaviors: []string{"SESS-536"}, City: napping,
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "an ambiguous probe defers the lifecycle and writes nothing else", Behaviors: []string{"SESS-534", "GUAR-011"}, City: napping,
			Rows: []parityRow{awake("held_until", at(-time.Minute), "sleep_reason", "user-hold", "last_woke_at", at(-2*time.Hour), "wake_attempts", "2")}, Work: assigned, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", probeErr: true}},
		},
		{
			Name: "a row alive past the thresholds clears wake failures and churn", Behaviors: []string{"SESS-539", "SESS-540"},
			Rows: []parityRow{awake("last_woke_at", at(-2*time.Hour), "wake_attempts", "2", "quarantined_until", at(time.Hour), "churn_count", "2")}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "an alive row clears its stranded marker", Behaviors: []string{"SESS-603"},
			Rows: []parityRow{awake("stranded_event_emitted_at", at(-time.Hour))}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "wake reasons cancel an idle drain", Behaviors: []string{"SESS-615", "SESS-632", "DRAIN-047"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Drains: map[string]string{"gc-1": "idle"}, Explain: explain(nil),
		},
		{
			Name: "assigned work cancels an orphaned drain", Behaviors: []string{"DRAIN-046"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Drains: map[string]string{"gc-1": "orphaned"}, Explain: explain(nil),
		},
		{
			Name: "a pending interaction cancels a config-drift drain", Behaviors: []string{"DRAIN-045"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Pending: []string{"gc-1"}, Drains: map[string]string{"gc-1": "config-drift"}, Explain: explain(nil),
		},
		{
			Name: "a probe error keeps a drain", Behaviors: []string{"DRAIN-042"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", probeErr: true}}, Drains: map[string]string{"gc-1": "idle"},
			Explain: map[string]string{"row:gc-1:*drain": "drain-cancel-on-probe-error"},
		},
		{
			Name: "an idle-respawn drain no longer eligible", Behaviors: []string{"DRAIN-044"},
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Drains: map[string]string{"gc-1": "idle-respawn"},
			Explain: explain(map[string]string{"row:gc-1:*drain": "idle-respawn-void"}),
		},
		{
			Name: "a resume voids a suspended drain (R16)",
			Rows: []parityRow{awake()}, Work: assigned, Runtimes: []simRuntime{live}, Drains: map[string]string{"gc-1": "suspended"},
			Explain: explain(map[string]string{
				"row:gc-1:*r16": "§12.2#3 R16", "provider:SetMeta s-gc-1": "§12.2#3 R16", "provider:Stop s-gc-1": "§12.2#3 R16",
				"provider:Start s-gc-1": "§12.2#3 R16", "provider:RemoveMeta s-gc-1": "§12.2#3 R16", "event:session.woke worker-1 []": "§12.2#3 R16",
				"event:session.drain_acked_with_assigned_work worker [bead_id bead_status reason session_id template]": "§12.2#3 R16", "event:session.stopped worker [reason session_id template]": "§12.2#3 R16",
			}),
		},
		{
			// An unwanted row woken a minute ago: both keep it for INC-003's
			// grace.
			Name: "an unwanted row inside the wake grace is kept", Behaviors: []string{"INC-003"},
			Rows: []parityRow{awake()}, Runtimes: []simRuntime{live},
		},
		{
			// A suspended agent's row past the grace, with in-progress work:
			// both keep it open (the undesired-drain work keep).
			Name: "an undesired row with assigned work is kept open", Behaviors: []string{"SESS-074"}, City: suspendedWorker,
			Rows: []parityRow{awake("last_woke_at", at(-time.Hour))}, Work: assigned, Runtimes: []simRuntime{live},
		},
		{
			// An always named session idle past sleep_after_idle, its
			// activity an hour old as the observation lane reads it (C6a2b):
			// both begin its idle drain and write the latch, which is
			// compared; legacy also signals and stops it in the tick (A4).
			Name: "an idle drain begin writes the sleep policy fingerprint", Behaviors: []string{"SESS-601"}, City: chatNapping,
			Rows:     []parityRow{chatRow("gc-c", "1", "state", "awake", "session_name", "chat", "instance_token", "tok-gc-c", "last_woke_at", at(-time.Hour), "detached_at", at(-time.Hour))},
			Runtimes: []simRuntime{chatLive}, Idle: []string{"gc-c"},
			Explain: tlMerged(latchCompared(stopsInTick("A4 idle-sleep")), map[string]string{"row:gc-c:config_wake_suppressed": "sleep-policy-keys"}),
		},
		{
			// A pool row awake only for ready, unclaimed assigned work, idle
			// past its sleep time: legacy begins an idle-respawn drain.
			Name: "an idle pool row awake only for unclaimed work", Behaviors: []string{"SESS-614"}, City: napping,
			Rows: []parityRow{awake("last_woke_at", at(-time.Hour), "detached_at", at(-time.Hour))}, Work: []parityRow{routedDemandBead("gw-1")}, Runtimes: []simRuntime{live}, Idle: []string{"gc-1"},
			// Legacy also creates and starts the replacement seat gc-2.
			Explain: explain(map[string]string{
				"row:gc-1:*respawn": "idle-respawn-drain", "provider:SetMeta s-gc-1": "idle-respawn-drain", "provider:RemoveMeta s-gc-1": "idle-respawn-drain",
				"provider:Stop s-gc-1": "idle-respawn-drain", "event:session.stopped worker [reason session_id template]": "idle-respawn-drain",
				"row:gc-2": "idle-respawn-drain", "provider:Start worker-gc-2": "idle-respawn-drain", "provider:RemoveMeta worker-gc-2": "idle-respawn-drain",
				"event:session.woke worker-1 []": "idle-respawn-drain",
			}),
		},
		{
			// A woken row's leftover idle-stop-pending mark is cleared.
			Name: "a woken row clears its idle-stop-pending mark", Behaviors: []string{"SESS-616"},
			Rows: []parityRow{awake("sleep_intent", "idle-stop-pending")}, Work: assigned, Runtimes: []simRuntime{live}, Explain: explain(nil),
		},
		{
			// A heartbeat hold (held_until, no intent) keeps a live Sleep row
			// running: no drain begins.
			Name: "a heartbeat hold keeps a live row undrained", Behaviors: []string{"SESS-617"}, City: chatOnDemand,
			Rows:     []parityRow{onDemand("last_woke_at", at(-time.Hour), "held_until", at(time.Hour))},
			Runtimes: []simRuntime{chatLive},
			Explain:  chatStanding,
		},
		{
			// Reason selection: a live quarantined Sleep row with no intent
			// and no work drains for no wake reason; the lenses do not cancel
			// it (DRAIN-022, DRAIN-501).
			Name: "a live quarantined row drains for no wake reason", Behaviors: []string{"SESS-618", "DRAIN-022", "DRAIN-501"}, City: chatOnDemand,
			Rows:     []parityRow{onDemand("last_woke_at", at(-10*time.Second), "quarantined_until", at(time.Hour), "sleep_reason", "quarantine")},
			Runtimes: []simRuntime{chatLive},
			Explain:  stopsInTick("A4 drain-stop"),
		},
		{
			// An idle on-demand named session holding a live claim (its
			// in-progress work) is not idle-drained: legacy's live-claim and
			// claimed-work vetoes, v2's AL1 claimed-work exemption.
			Name: "an idle session holding a live claim is not drained", Behaviors: []string{"SESS-619"}, City: chatOnDemand,
			Rows:     []parityRow{onDemand("last_woke_at", at(-time.Hour), "detached_at", at(-time.Hour))},
			Work:     []parityRow{{ID: "gw-1", Title: "gw-1", Type: "task", Status: "in_progress", Assignee: "gc-c", Metadata: map[string]string{}}},
			Runtimes: []simRuntime{chatLive}, Idle: []string{"gc-c"},
			Explain: chatStanding,
		},
		{
			// An idle on-demand named session proved idle: both begin its
			// idle drain (SESS-621's gate passed).
			// Its latch is compared: every other sleep-policy key is explained
			// by name, the fingerprint is not (SESS-601, D2's idle latch).
			Name: "an idle on-demand session proved idle begins its idle drain", Behaviors: []string{"SESS-621", "SESS-601"}, City: chatOnDemand,
			Rows:     []parityRow{onDemand("last_woke_at", at(-time.Hour), "detached_at", at(-time.Hour))},
			Runtimes: []simRuntime{chatLive}, Idle: []string{"gc-c"},
			Explain: latchCompared(stopsInTick("A4 idle-sleep")),
		},
		{
			// An idle stop pending whose runtime is gone finishes the sleep
			// (legacy's SleepPatch(idle), keeping the latch): A6's
			// dead-runtime heal writes it (C6a2 review 1c).
			Name: "an idle stop pending with its runtime gone finishes the sleep", Behaviors: []string{"SESS-532"}, City: chatOnDemand,
			// A keyed conversation woken 10s ago: a continuation reset or a
			// rapid-crash accrual would show.
			Rows:    []parityRow{onDemand("last_woke_at", at(-10*time.Second), "session_key", "k-1", "sleep_intent", "idle-stop-pending", "sleep_policy_fingerprint", "pre-drain")},
			Explain: latchCompared(chatStanding),
		},
		{
			Name: "an empty row type is repaired", Behaviors: []string{"SESS-701"},
			Rows: []parityRow{func() parityRow { b := awake(); b.Type = ""; return b }()}, Work: assigned, Runtimes: []simRuntime{live},
			Explain: explain(nil),
		},
		{
			Name: "a preserved named row's closed trigger is cleared", Behaviors: []string{"POOL-056"}, City: chat(true),
			Rows:    []parityRow{chatRow("gc-c", "1", "session_name", "chat", "gc.trigger_bead_id", "gw-9")},
			Work:    []parityRow{{ID: "gw-9", Title: "gw-9", Type: "task", Status: "closed", Metadata: map[string]string{}}},
			Explain: map[string]string{"row:gc-c:*trigger": "named-trigger-clear", "row:gc-c:*policy": "sleep-policy-keys"},
		},
		{
			Name: "routed demand creates a pool row", Behaviors: []string{"POOL-051", "GUAR-042", "INC-018", "SESS-711"},
			Work: []parityRow{routedDemandBead("gw-1")}, Explain: started("gc-1", "worker-gc-1", "worker-1", nil),
		},
		{
			Name: "no second create beside an unconfirmed create", Behaviors: []string{"SESS-710"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "creating", "pending_create_claim", "true", "pending_create_started_at", at(-10*time.Second))},
			Work: []parityRow{routedDemandBead("gw-1")}, Explain: started("gc-1", "s-gc-1", "worker-1", nil),
		},
		{Name: "an always named session is created", Behaviors: []string{"SESS-711", "START-014", "GUAR-017"}, City: chat(false), Explain: started("gc-1", "chat", "chat", nil)},
		{
			Name: "a closed named row is reopened", Behaviors: []string{"SESS-709"}, City: chat(false),
			Rows:    []parityRow{func() parityRow { b := chatRow("gc-c", "1", "session_name", "chat"); b.Status = "closed"; return b }()},
			Explain: started("gc-c", "chat", "chat", nil),
		},
		{
			Name: "a mislabelled row is left alone", Rows: []parityRow{sessionRow("gc-1", "state", "asleep")},
			Explain: map[string]string{"row:gc-1:*close": "mislabelled-close", "row:gc-1:*a7": "§12.2#51 a7-lag"},
		},
		{Name: "a kill fence holds its row while demand creates another", Rows: []parityRow{func() parityRow {
			b := poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-gc-1")
			maps.Copy(b.Metadata, session.KillPendingPatch(parityNow.Add(-time.Minute)))
			return b
		}()}, Work: []parityRow{routedDemandBead("gw-1")}, Explain: started("gc-2", "worker-gc-2", "worker-2", nil)},
		// A18's gates (C5b). The twin has no provider-health registry, so
		// health reads unknown and fails open in both copies; the red skip
		// is TestDecideRowStartGates', and the episode and its alert are
		// unported (parityUnported's note).
		// S1's start end to end: the PreWake's incarnation bump, one Start
		// per admission, the commit; legacy's runtime meta clears are §12.2
		// #38. The start's deadline, stale-key wait, panic recovery and
		// session-name guard (START-016/019/038, SESS-640) are inside the
		// effect, which the twin runs unbounded and over valid names (legacy
		// replaces an invalid-name seat every tick); TestLaunchDeadlines,
		// TestResumedStartWaitsStaleKeyDetectDelay, the executor's panic
		// recovery and TestLaunchRefusesEmptySessionName pin them.
		{
			Name: "a wanted row whose provider health is unknown starts", Behaviors: []string{"GUAR-034", "SESS-610", "SESS-630", "START-807", "START-808", "START-809", "SESS-641", "START-012", "START-026", "GUAR-002", "START-025", "START-016", "START-019", "START-038", "SESS-640"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-gc-1")}, Work: assigned,
			Explain: started("gc-1", "s-gc-1", "worker-1", nil),
		},
		{
			Name: "a failed-create always named row is never started", Behaviors: []string{"SESS-605", "START-040"}, City: chat(false),
			Rows:    []parityRow{chatRow("gc-c", "1", "session_name", "chat", "state", "failed-create")},
			Explain: started("gc-1", "chat", "chat", nil),
		},
		{
			Name: "a quarantined wanted row is not started", Behaviors: []string{"SESS-606", "START-041"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-gc-1", "quarantined_until", at(time.Hour))}, Work: assigned,
			Explain: explain(nil),
		},
		{
			Name: "a startup-health episode holds a wanted row", Behaviors: []string{"SESS-607", "START-042"},
			Rows: func() []parityRow {
				row := poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-gc-1")
				m := beads.NewMemStore()
				ep := session.StartupHealthEpisode{SessionName: startupHealthEpisodeKey(sessionInfoFromBead(row), "s-gc-1"), ConsecutiveCount: 3, QuarantinedUntil: parityNow.Add(time.Hour)}
				if err := session.NewStore(beads.SessionStore{Store: m}).SaveStartupHealthEpisode(ep); err != nil {
					panic(err)
				}
				eps, _ := m.List(beads.ListQuery{Type: session.StartupHealthEpisodeType})
				return append([]parityRow{row}, eps...)
			}(), Work: assigned,
			Explain: explain(map[string]string{"row:gc-1:*a7": "§12.2#51 a7-lag", "row:gc-1:*bind": "§12.2#40 held-start-bind", "row:gc-1:*mirror": "episode-mirror"}),
		},
		{
			Name: "a heartbeat-held seat whose runtime died respawns for its work", Behaviors: []string{"SESS-602"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "held_until", at(time.Hour), "last_woke_at", at(-time.Hour))}, Work: assigned,
			Explain: started("gc-1", "s-gc-1", "worker-1", map[string]string{"row:gc-1:currently_processing_bead_id": "§12.2#37 current-bead-lag"}),
		},
		// The replay's mc-p2 (C5b-2) with the demand its export lacks: a seat
		// whose user hold expired, its in-progress work assigned to it,
		// wakes in both copies (resume tier).
		{
			Name: "a seat whose user hold expired resumes for its work", Behaviors: []string{"SESS-009"},
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-gc-1", "held_until", at(-time.Hour), "sleep_reason", "user-hold")}, Work: assigned,
			Explain: started("gc-1", "s-gc-1", "worker-1", nil),
		},
		// A8's version rebaseline of an asleep named row (C7c), as legacy's
		// asleep named drift block does; a live seat's is the timeline's.
		{
			Name: "an asleep named row's v5 hash is rebaselined", Behaviors: []string{"SESS-567"}, City: func() *config.City {
				cfg := chat(false)()
				cfg.NamedSessions[0].Mode = "on_demand"
				return cfg
			},
			Rows:    []parityRow{chatRow("gc-c", "1", "session_name", "chat", "configured_named_mode", "on_demand", "started_config_hash", "v5:core")},
			Explain: map[string]string{"row:gc-c:*policy": "sleep-policy-keys"},
		},
		// A wanted asleep named row whose start a gate holds (a quarantine)
		// is rebaselined too, as legacy's asleep named branch does: A8
		// leaves only a row the start arms start this pass, whose commit
		// writes the baseline.
		{
			Name: "a wanted asleep named row its start holds is rebaselined", Behaviors: []string{"SESS-567"}, City: chat(false),
			Rows:    []parityRow{chatRow("gc-c", "1", "session_name", "chat", "started_config_hash", "v5:core", "quarantined_until", at(time.Hour), "sleep_reason", "quarantine")},
			Explain: map[string]string{"row:gc-c:*policy": "sleep-policy-keys"},
		},
		// A14's config drift (C7b1) on a live seat, legacy's drift block in
		// its order. Its fixtures carry no launch-only drift, so the relaunch
		// (SESS-556/560), C7b2's, and an attach read error (SESS-552) are
		// TestDecideRowConfigDrift's.
		{
			// The execution-stalled drain (NUDGE-022) is A16's through the same
			// begin; the twin runs no execution backstop, so
			// TestDecideRowExecutionStalled pins it.
			Name: "a drifted floor seat drains for config drift", Behaviors: []string{"SESS-550", "SESS-561", "SESS-556", "SESS-560", "NUDGE-022"}, City: floor,
			Rows: []parityRow{awake("started_config_hash", drifted)}, Runtimes: []simRuntime{live}, Explain: driftDrained,
		},
		{
			Name: "an attached drifted seat records its deferral and stays", Behaviors: []string{"SESS-552", "SESS-553", "GUAR-006"}, City: floor,
			Rows: []parityRow{awake("started_config_hash", drifted)}, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", attached: true}}, Explain: map[string]string{},
		},
		{
			Name: "a drifted seat with a pending interaction waits", Behaviors: []string{"SESS-558"}, City: floor,
			Rows: []parityRow{awake("started_config_hash", drifted)}, Runtimes: []simRuntime{live}, Pending: []string{"gc-1"}, Explain: map[string]string{},
		},
		{
			Name: "a drifted seat with assigned work waits", Behaviors: []string{"SESS-559"},
			Rows: []parityRow{awake("started_config_hash", drifted)}, Work: assigned, Runtimes: []simRuntime{live}, Explain: map[string]string{"row:gc-1:currently_processing_bead_id": "drift-deferral-skips"},
		},
		// A named row's drift: a pinned one is never restarted, only its
		// deferral stamped (SESS-555); another restarts in place (SESS-557)
		// and an asleep one is repaired (SESS-566/568), both C7b2's, which
		// A14 leaves alone until then.
		{
			Name: "a pinned named row's drift is deferred", Behaviors: []string{"SESS-555"}, City: chat(false),
			Rows: []parityRow{onDemand("configured_named_mode", "always", "started_config_hash", drifted, "pin_awake", "true")}, Runtimes: []simRuntime{chatLive}, Explain: map[string]string{},
		},
		{
			Name: "a named row's drift restarts it in place", Behaviors: []string{"SESS-557"}, City: chat(false),
			Rows: []parityRow{onDemand("configured_named_mode", "always", "started_config_hash", drifted)}, Runtimes: []simRuntime{chatLive},
			Explain: tlMerged(chatStanding, map[string]string{
				"row:gc-c:*restart": "C7b2 drift-restart", "provider:Stop chat": "C7b2 drift-restart", "provider:Start chat": "C7b2 drift-restart", "provider:RemoveMeta chat": "C7b2 drift-restart",
				"event:session.draining chat []": "C7b2 drift-restart", "event:session.woke chat []": "C7b2 drift-restart",
			}),
		},
		{
			Name: "an asleep named row's drift is repaired", Behaviors: []string{"SESS-566", "SESS-568"}, City: chatOnDemand,
			Rows:    []parityRow{onDemand("state", "asleep", "started_config_hash", drifted, "session_key", "conversation-1")},
			Explain: map[string]string{"row:gc-c:*policy": "sleep-policy-keys"},
		},
		// A wanted one (always): legacy repairs it, its key rotated, then
		// starts it on a fresh conversation; v2's start carries the repair in
		// its PreWake (C7b2-1). A v5 hash is only rebaselined (SESS-567).
		{
			Name: "a wanted asleep named row's drift is repaired and started", Behaviors: []string{"SESS-566", "SESS-568"}, City: chat(false),
			Rows:    []parityRow{onDemand("configured_named_mode", "always", "state", "asleep", "started_config_hash", drifted, "session_key", "conversation-1")},
			Explain: map[string]string{"row:gc-c:*policy": "sleep-policy-keys", "provider:RemoveMeta chat": "§12.2#38 start-meta-clears"},
		},
		{
			Name: "a wanted asleep named row's v5 hash is rebaselined and started", Behaviors: []string{"SESS-567"}, City: chat(false),
			Rows:    []parityRow{onDemand("configured_named_mode", "always", "state", "asleep", "started_config_hash", "v5:core")}, // a keyed start waits out a stale-key window the twin's clock never reaches
			Explain: map[string]string{"row:gc-c:*policy": "sleep-policy-keys", "provider:RemoveMeta chat": "§12.2#38 start-meta-clears"},
		},
		// A16 (C7b1): C8's execution-stalled request, a drain no lens cancels.
		// The twin runs no execution backstop: TestDecideRowExecutionStalled.
		// A live drift (SESS-565), a resolved one (SESS-562) and a recent
		// attached deferral (SESS-554) are the timeline's.
		// A10's rollback (C5a2): an undesired pending create past its
		// deadline, which legacy's sweep closes first.
		{
			Name: "an undesired pending create past its deadline closes", Behaviors: []string{"SESS-050", "SESS-051", "SESS-052", "SESS-501", "MAINT-034"},
			Rows:    []parityRow{poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-gc-1", "pending_create_claim", "true", "pending_create_started_at", at(-11*time.Minute))},
			Explain: map[string]string{"row:gc-1:*swept": "pending-create-swept"},
		},
		// S3's abandon: a pending create whose start fails rolls back and
		// accrues #46 (the episode is a bead both copies compare). A seat's
		// failure retries the next legacy tick but backs off in v2 (P4), so
		// its classes are pinned against legacy's writers instead
		// (TestAbandonStepClasses, TestComposedUndoMatchesLegacyRestore).
		{
			Name: "a pending create whose start fails rolls back", Behaviors: []string{"START-031", "START-033"},
			City: floor, StartErr: errors.New("boom"),
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-gc-1", "pending_create_claim", "true", "pending_create_started_at", at(-time.Minute))},
			Explain: started("gc-3", "worker-gc-3", "", map[string]string{
				"row:gc-1:*policy": "sleep-policy-keys", "row:gc-1:*lease": "rollback-leaves-lease", "provider:Stop s-gc-1": "rollback-stops-nothing",
			}),
		},
		// A desired one (a floor seat), which no sweep reaches: both roll
		// it back, then replace it.
		{
			Name: "a desired pending create past its deadline rolls back", Behaviors: []string{"SESS-092", "SESS-093", "SESS-502", "GUAR-026", "GUAR-028", "INC-021"},
			City: floor,
			Rows: []parityRow{poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-gc-1", "pending_create_claim", "true", "pending_create_started_at", at(-11*time.Minute))},
			Explain: started("gc-2", "worker-gc-2", "", map[string]string{
				"row:gc-2:*slot": "rollback-replacement-slot", "provider:Stop s-gc-1": "rollback-stops-nothing",
				"event:session.woke worker-1 []": "rollback-replacement-slot", "event:session.woke worker-2 []": "rollback-replacement-slot",
			}),
		},
	}
}

// Kills drift between v2's merged arms and effects and legacy's tick on the
// same rows: any row write, assignee, provider call or event the two
// disagree on that no entry accounts for.
func TestSessionDifferentialFixtures(t *testing.T) {
	for _, f := range parityFixtures() {
		t.Run(f.Name, func(t *testing.T) { checkParity(t, f) })
	}
}

// Kills an entry that accounts for nothing, and an unported entry whose
// arm or kind has registered: every entry is shown by a fixture, and every
// unported entry's owner is still unregistered.
func TestSessionDifferentialEntriesHaveFixtures(t *testing.T) {
	shown := map[string]bool{}
	fixtures := parityFixtures()
	for _, f := range timelineFixtures() {
		fixtures = append(fixtures, f.parityFixture)
	}
	for _, f := range fixtures {
		for _, id := range f.Explain {
			shown[id] = true
		}
	}
	for _, tbl := range []map[string]parityEntry{parityAccepted, parityFindings, parityUnported} {
		for _, id := range slices.Sorted(maps.Keys(tbl)) {
			if !shown[id] {
				t.Errorf("entry %s has no fixture", id)
			}
		}
	}
	arms, kinds := parityRegistered()
	for _, id := range slices.Sorted(maps.Keys(parityUnported)) {
		owner, _, _ := strings.Cut(id, " ")
		if arms[owner] || kinds[owner] {
			t.Errorf("unported entry %s: %s has registered; make it parity, a §12.2 entry or a finding", id, owner)
		}
	}
}

// parityRegistered is the arms in rowArms and the kinds in effectSpecs that
// have an effect.
func parityRegistered() (arms, kinds map[string]bool) {
	arms, kinds = map[string]bool{}, map[string]bool{}
	for _, a := range rowArms {
		arms[a.name] = true
	}
	for k, spec := range effectSpecs {
		if spec.runs() {
			kinds[k] = true
		}
	}
	return arms, kinds
}

// keptOwners reads the vendored KEPT owner cells: owner → row IDs.
func keptOwners(t *testing.T) map[string][]string {
	t.Helper()
	file, err := os.Open("testdata/session_differential/kept_owners.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // read-only
	out := map[string][]string{}
	sc := bufio.NewScanner(file)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			owner, ids, ok := strings.Cut(line, "\t")
			if !ok {
				t.Fatalf("kept_owners.tsv: malformed line %q", line)
			}
			out[owner] = strings.Split(ids, ",")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// keptOwnerUnits is the arms and effect kinds an owner cell names, the
// kinds by their phrases.
func keptOwnerUnits(owner string, phrases map[string]*regexp.Regexp) []string {
	var units []string
	for _, m := range parityArmRef.FindAllStringSubmatch(owner, -1) {
		units = append(units, "A"+m[1])
	}
	for _, kind := range slices.Sorted(maps.Keys(phrases)) {
		if phrases[kind].MatchString(owner) {
			units = append(units, kind)
		}
	}
	return units
}

// keptGate is the KEPT rows the gate binds, by ID to owner: those whose owner
// names at least one arm or kind, and only reachable ones.
func keptGate(owners map[string][]string, phrases map[string]*regexp.Regexp, reachable func(unit string) bool) map[string]string {
	gated := map[string]string{}
	for owner, ids := range owners {
		units := keptOwnerUnits(owner, phrases)
		if len(units) == 0 || slices.ContainsFunc(units, func(u string) bool { return !reachable(u) }) {
			continue
		}
		for _, id := range ids {
			gated[id] = owner
		}
	}
	return gated
}

// parityReachable is reachability: an arm in rowArms, or a registered kind
// a proposer can return.
func parityReachable(arms, kinds, proposed map[string]bool) func(unit string) bool {
	return func(u string) bool { return arms[u] || kinds[u] && proposed[u] }
}

// parityProposers are where a pass proposes intents (tracePass): decideRow,
// whose arms it reaches through rowArms, and the allocation's creates.
var parityProposers = []string{"decideRow", "createIntent"}

// parityKindTables are the kind tables a walk from a proposer must not
// enter: they name every kind and propose none.
var parityKindTables = map[string]bool{"effectSpecs": true}

// proposedKinds is the value of every intent-kind constant (a key of
// effectSpecs, the kind table) the proposers in dir's v2 files (reconcile_*, allocator_*)
// reference, following the package functions and variables they name and
// the methods they select, never a local, so a kind an arm returns counts
// once the arm is in rowArms. A kind code only compares against (==, !=, a
// switch case) proposes nothing and does not count; any other reference
// does, so it still over-approximates.
func proposedKinds(t *testing.T, dir string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	funcs, methods := map[string][]ast.Node{}, map[string][]ast.Node{}
	consts, top := map[string]ast.Expr{}, map[any]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(base, "_test.go") || !strings.HasPrefix(base, "reconcile_") && !strings.HasPrefix(base, "allocator_") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if top[d] = true; d.Recv != nil {
					methods[d.Name.Name] = append(methods[d.Name.Name], d)
				} else {
					funcs[d.Name.Name] = append(funcs[d.Name.Name], d)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					top[vs] = true
					for i, name := range vs.Names {
						if d.Tok == token.CONST && i < len(vs.Values) {
							consts[name.Name] = vs.Values[i]
						} else if d.Tok == token.VAR {
							funcs[name.Name] = append(funcs[name.Name], vs)
						}
					}
				}
			}
		}
	}
	var value func(name string) (string, bool)
	value = func(name string) (string, bool) {
		switch e := consts[name].(type) {
		case *ast.BasicLit:
			v, err := strconv.Unquote(e.Value)
			return v, err == nil && e.Kind == token.STRING
		case *ast.Ident:
			return value(e.Name)
		}
		return "", false
	}
	kindNames := map[string]bool{}
	for _, d := range funcs["effectSpecs"] {
		for _, v := range d.(*ast.ValueSpec).Values {
			for _, elt := range v.(*ast.CompositeLit).Elts {
				if id, ok := elt.(*ast.KeyValueExpr).Key.(*ast.Ident); ok {
					kindNames[id.Name] = true
				}
			}
		}
	}
	out, seen := map[string]bool{}, map[string]bool{}
	var walk func(method bool, name string)
	var inspect func(root ast.Node)
	compared := func(e ast.Expr) bool { // a kind constant only compared against
		id, ok := ast.Unparen(e).(*ast.Ident)
		return ok && kindNames[id.Name]
	}
	inspect = func(root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BinaryExpr:
				if n.Op != token.EQL && n.Op != token.NEQ {
					return true
				}
				for _, side := range []ast.Expr{n.X, n.Y} {
					if !compared(side) {
						inspect(side)
					}
				}
				return false
			case *ast.CaseClause:
				for _, e := range n.List {
					if !compared(e) {
						inspect(e)
					}
				}
				for _, st := range n.Body {
					inspect(st)
				}
				return false
			case *ast.SelectorExpr:
				walk(true, n.Sel.Name)
				inspect(n.X)
				return false
			case *ast.Ident:
				if v, ok := value(n.Name); ok && kindNames[n.Name] {
					out[v] = true
				}
				if n.Obj == nil || top[n.Obj.Decl] { // a package name, never a local
					walk(false, n.Name)
				}
			}
			return true
		})
	}
	walk = func(method bool, name string) {
		key, decls := "func "+name, funcs
		if method {
			key, decls = "method "+name, methods
		}
		if seen[key] || parityKindTables[name] {
			return
		}
		seen[key] = true
		for _, d := range decls[name] {
			inspect(d)
		}
	}
	for _, p := range parityProposers {
		walk(false, p)
	}
	return out
}

// Kills an arm or effect kind becoming reachable without parity fixtures
// for the KEPT behaviors it owns: every KEPT row whose owner cell names only
// reachable arms and kinds is named by a fixture. A row naming an arm not yet
// registered, or a kind nothing registered proposes, is exempt until then.
func TestEveryKeptBehaviorOfARegisteredArmHasAParityFixture(t *testing.T) {
	arms, kinds := parityRegistered()
	reachable := parityReachable(arms, kinds, proposedKinds(t, "."))
	var reached []string
	for _, kind := range slices.Sorted(maps.Keys(kinds)) {
		switch {
		case !reachable(kind):
		case parityKindOwners[kind] == nil:
			t.Errorf("effect kind %s is registered and proposed with no BEHAVIORS owner phrase in parityKindOwners", kind)
		default:
			reached = append(reached, kind)
		}
	}
	covered := map[string]bool{}
	fixtures := parityFixtures()
	for _, f := range timelineFixtures() { // a timeline's behaviors are shown through time
		fixtures = append(fixtures, f.parityFixture)
	}
	for _, f := range fixtures {
		for _, id := range f.Behaviors {
			covered[id] = true
		}
	}
	owners := keptOwners(t)
	gated := keptGate(owners, parityKindOwners, reachable)
	for _, id := range slices.Sorted(maps.Keys(gated)) {
		if !covered[id] {
			t.Errorf("KEPT %s (owner %q) has no parity fixture", id, gated[id])
		}
	}
	kept := map[string]bool{}
	total := 0
	for _, ids := range owners {
		total += len(ids)
		for _, id := range ids {
			kept[id] = true
		}
	}
	for _, id := range parityPinned {
		if !kept[id] || !covered[id] {
			t.Errorf("pinned KEPT %s: kept %t, has a parity fixture %t", id, kept[id], covered[id])
		}
	}
	t.Logf("KEPT rows gated %d, exempt %d; reachable kinds %v", len(gated), total-len(gated), reached)
}

// Kills a gate that binds a kind before anything proposes it, and one that
// stays open after: a kind registered with no proposing arm gates no row; a
// registered arm that proposes it gates the row, which then needs a fixture.
func TestKeptGateBindsAKindOnceARegisteredArmProposesIt(t *testing.T) {
	dir := t.TempDir()
	write := func(src string) {
		if err := os.WriteFile(filepath.Join(dir, "reconcile_fake.go"), []byte("package main\n"+src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const kinds = `
const (
	intentFake = "fake"
	intentLaunch = intentFake
)
var effectSpecs = map[string]int{intentLaunch: 1}
func decideRow() { _ = rowArms }
func createIntent() {}
`
	owners := map[string][]string{"S1 fake start (PreWake)": {"FAKE-1"}, "A99 fake gate": {"FAKE-2"}}
	phrases := map[string]*regexp.Regexp{"fake": regexp.MustCompile(`\bS1\b`)}
	gate := func() map[string]string {
		reachable := parityReachable(map[string]bool{"A1": true}, map[string]bool{"fake": true}, proposedKinds(t, dir))
		return keptGate(owners, phrases, reachable)
	}
	// armOther names the kind table, which names every kind and proposes none.
	write(kinds + "var rowArms = []rowArm{{\"A1\", armOther}}\nfunc armOther() { _ = effectSpecs }\n")
	if got := gate(); len(got) != 0 {
		t.Fatalf("a registered kind no arm proposes gated %v", got)
	}
	// An arm that only compares against the kind proposes nothing.
	write(kinds + "var rowArms = []rowArm{{\"A1\", armOther}}\nfunc armOther(k string) bool { switch k { case intentLaunch: return true }; return k == intentLaunch }\n")
	if got := gate(); len(got) != 0 {
		t.Fatalf("a kind an arm only compares against gated %v", got)
	}
	write(kinds + "var rowArms = []rowArm{{\"A1\", armFake}}\nfunc armFake() { launch() }\nfunc launch() { _ = intentLaunch }\n")
	if got := gate(); !maps.Equal(got, map[string]string{"FAKE-1": "S1 fake start (PreWake)"}) {
		t.Fatalf("with a proposing arm the gate binds %v, want FAKE-1 only", got)
	}
}

// Kills a step-blind Explain lookup: a key scoped to one step accounts for
// its aspect at that step only.
func TestTimelineExplainsAreStepScoped(t *testing.T) {
	f := parityFixture{Explain: map[string]string{"@crash row:gc-1:*start": "§12.2#3 R16", "@crash runtime:s-gc-1": "§12.2#3 R16"}}
	for aspect, want := range map[string]bool{
		"@crash row:gc-1:generation": true, "@crash runtime:s-gc-1": true, "@crash row:gc-1:core_hash_breakdown.Command": true,
		"@seed row:gc-1:generation": false, "@seed runtime:s-gc-1": false, "@crash row:gc-1:alias": false, "row:gc-1:generation": false,
	} {
		if _, got := f.explains(aspect); got != want {
			t.Errorf("explains(%q) = %t, want %t", aspect, got, want)
		}
	}
}

// TestFreshDrainBeginsReachAdmission (C6a2 review must-fix 1). Kills a fresh
// begin (idle, no-wake-reason) deferred before admission, which never runs:
// through the real gather, allocation and admission, an idle on-demand named
// session's begin and a live quarantined one's no-wake-reason begin are
// admitted, and no pass defers an intent for its rests.
func TestFreshDrainBeginsReachAdmission(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	chatNapping := func() *config.City {
		cfg := workerCity(3)
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat", StartCommand: "true", SleepAfterIdle: "5m"})
		cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "on_demand"}}
		return cfg
	}
	for _, c := range []struct {
		name, want string
		f          parityFixture
	}{
		{"idle", intentDrainBeginFresh + ":gc-c", parityFixture{
			City:     chatNapping,
			Rows:     []parityRow{chatRow("gc-c", "1", "state", "awake", "session_name", "chat", "configured_named_mode", "on_demand", "instance_token", "tok-gc-c", "last_woke_at", at(-time.Hour), "detached_at", at(-time.Hour), "alias", "chat", "agent_name", "chat", "session_origin", "named", "command", "true", "continuation_epoch", "1")}, // its identity converged but its work_dir: one A7 pass
			Runtimes: []simRuntime{{id: "gc-c", epoch: "1", token: "tok-gc-c"}},
		}},
		{"no-wake-reason", intentDrainBeginFresh + ":gc-c", parityFixture{
			City:     chatNapping,
			Rows:     []parityRow{chatRow("gc-c", "1", "state", "awake", "session_name", "chat", "configured_named_mode", "on_demand", "instance_token", "tok-gc-c", "last_woke_at", at(-10*time.Second), "quarantined_until", at(time.Hour), "sleep_reason", "quarantine", "alias", "chat", "agent_name", "chat", "session_origin", "named", "command", "true", "continuation_epoch", "1")}, // its identity converged: A7 takes no pass
			Runtimes: []simRuntime{{id: "gc-c", epoch: "1", token: "tok-gc-c"}},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cityPath := t.TempDir()
			w := newV2World(t, c.f, cityPath, c.f.beads(), nil, "", nil)
			var admitted []string
			for pass := 0; pass < 3; pass++ {
				w.s.inventory()
				w.s.pass()
				rec := w.s.p.out.record.Load()
				admitted = append(admitted, intentKeys(rec.Admitted)...)
				for _, d := range rec.Deferred {
					if strings.HasPrefix(d.Cause, "rests-") {
						t.Fatalf("pass %d deferred %s:%s for its rests (%s)", pass, d.Kind, d.Key.ID, d.Cause)
					}
				}
				for len(w.s.parked) > 0 {
					w.s.release(0)
				}
			}
			if !slices.Contains(admitted, c.want) {
				t.Fatalf("admitted %v, want %s", admitted, c.want)
			}
		})
	}
}

// TestIdleProbesPaceAPatrolApart (C6a2 review 1a). Kills a busy agent's
// idle begin probed again on every settlement's pass, 4x the pass duration
// apart, taking the probe cap from other rows' heals and stops: after a
// probe that finds the agent busy, the passes inside the patrol defer the
// begin before admission (idle-paced), and the first pass a patrol later
// probes again. A new incarnation is not paced.
func TestIdleProbesPaceAPatrolApart(t *testing.T) {
	dir := t.TempDir()
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	cfg := func() *config.City {
		cfg := workerCity(3)
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat", StartCommand: "true", SleepAfterIdle: "5m"})
		cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "on_demand"}}
		return cfg
	}
	f := parityFixture{
		City:     cfg,
		Rows:     []parityRow{chatRow("gc-c", "1", "state", "awake", "session_name", "chat", "configured_named_mode", "on_demand", "instance_token", "tok-gc-c", "last_woke_at", at(-time.Hour), "detached_at", at(-time.Hour), "alias", "chat", "agent_name", "chat", "session_origin", "named", "command", "true", "continuation_epoch", "1", "work_dir", dir)}, // its identity converged: A7 has nothing to write
		Runtimes: []simRuntime{{id: "gc-c", epoch: "1", token: "tok-gc-c"}},
	}
	w := newV2World(t, f, dir, f.beads(), nil, "", nil)
	w.s.sp.WaitForIdleErrors["chat"] = errors.New("busy")
	begin := intentDrainBeginFresh + ":gc-c"
	pass := func() (admitted bool, cause string) {
		w.s.inventory()
		w.s.pass()
		rec := w.s.p.out.record.Load()
		for len(w.s.parked) > 0 {
			w.s.release(0)
		}
		for _, d := range rec.Deferred {
			if d.Kind+":"+d.Key.ID == begin {
				cause = d.Cause
			}
		}
		return slices.Contains(intentKeys(rec.Admitted), begin), cause
	}
	if ok, cause := pass(); !ok || !slices.Contains(w.s.notIdle, begin) {
		t.Fatalf("first pass: admitted %t (deferred %q), probes %v; want the begin probed and found busy", ok, cause, w.s.notIdle)
	}
	for i, step := range []time.Duration{0, simPatrol / 2} {
		w.s.advance(step)
		if ok, cause := pass(); ok || cause != causeIdlePaced {
			t.Fatalf("pass %d inside the patrol: admitted %t, deferred %q; want it paced", i, ok, cause)
		}
	}
	w.s.advance(simPatrol / 2)
	if ok, cause := pass(); !ok {
		t.Fatalf("a patrol after the probe: deferred %q, want the begin probed again", cause)
	}
	// The pace is the incarnation's: a probe recorded for another basis
	// holds nothing.
	probe := w.s.p.idleProbes[rowKeyOf("gc-c")]
	probe.Basis.Incarnation++
	w.s.p.idleProbes[rowKeyOf("gc-c")] = probe
	if ok, cause := pass(); !ok {
		t.Fatalf("a probe of another incarnation: deferred %q, want the begin probed", cause)
	}
}

// TestIdlePaceHoldsOnlyIdleProbes (C6a2 review 1a). Kills a pace that holds
// a row's begin that would not probe: once the row's reason is no longer
// idle (a no-wake-reason begin), its begin passes, though the row's last
// idle probe is inside the patrol.
func TestIdlePaceHoldsOnlyIdleProbes(t *testing.T) {
	dir := t.TempDir()
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	cfg := func() *config.City {
		cfg := workerCity(3)
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat", StartCommand: "true", SleepAfterIdle: "5m"})
		cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "on_demand"}}
		return cfg
	}
	f := parityFixture{
		City:     cfg,
		Rows:     []parityRow{chatRow("gc-c", "1", "state", "awake", "session_name", "chat", "configured_named_mode", "on_demand", "instance_token", "tok-gc-c", "last_woke_at", at(-10*time.Second), "quarantined_until", at(time.Hour), "sleep_reason", "quarantine", "alias", "chat", "agent_name", "chat", "session_origin", "named", "command", "true", "continuation_epoch", "1", "work_dir", dir)}, // its identity converged: A7 has nothing to write
		Runtimes: []simRuntime{{id: "gc-c", epoch: "1", token: "tok-gc-c"}},
	}
	w := newV2World(t, f, dir, f.beads(), nil, "", nil)
	k := rowKeyOf("gc-c")
	var admitted []string
	for pass := 0; pass < 3; pass++ {
		w.s.p.idleProbes = map[rowKey]idleProbe{k: {Basis: rowBasis{Incarnation: 1, InstanceToken: "tok-gc-c"}, At: w.s.clk.Now()}}
		w.s.inventory()
		w.s.pass()
		admitted = append(admitted, intentKeys(w.s.p.out.record.Load().Admitted)...)
		for len(w.s.parked) > 0 {
			w.s.release(0)
		}
	}
	if !slices.Contains(admitted, intentDrainBeginFresh+":gc-c") {
		t.Fatalf("a no-wake-reason begin after an idle probe: admitted %v; want the begin", admitted)
	}
}
