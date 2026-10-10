package session

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The session field registry (CONTRACT R1; ARCH-RESTRUCTURE R1): per session
// metadata key, its class and, per mode, the sites (file and function) that
// write, consume and clear it. Tests enforce it where rows are written: a
// MemStore write of an unregistered key from production code fails the test
// (GuardSessionKeys), and cmd/gc's flow and clear-site tests run the declared
// sites. A Site with Pending is a contract site not yet built or doing its
// job, on the bead that owns it. Nothing reads this table in production.

// FieldClass is what a key means to a decision.
type FieldClass uint8

// The classes, CONTRACT R1's field groups.
const (
	ClassIncarnation        FieldClass = iota + 1 // which runtime incarnation the row owns
	ClassLifecycle                                // the lifecycle state machine's own facts
	ClassOperatorIntent                           // holds, pins and settings an operator owns
	ClassRequest                                  // one-shot requests a consumer clears
	ClassStopRequest                              // the v2 stop request (D1, D5); new keys
	ClassCounter                                  // failure accounting
	ClassMarker                                   // decision stamps and latches
	ClassBaseline                                 // the started-config hashes (M2)
	ClassAssignmentIdentity                       // the identities a claim is held under
	ClassLaunchConfig                             // desired identity and launch configuration
	ClassLease                                    // the cross-process runtime lease (L1)
	ClassAdvisory                                 // diagnostics and pacing stamps
)

// classInPremise is whether a fence compares a class's keys. Baseline keys are
// compared by the drift arms, assignment identity only by an effect that reads
// L5, the lease by its own CAS, and advisory keys never.
var classInPremise = map[FieldClass]bool{
	ClassIncarnation: true, ClassLifecycle: true, ClassOperatorIntent: true, ClassRequest: true,
	ClassStopRequest: true, ClassCounter: true, ClassMarker: true,
	ClassBaseline: false, ClassAssignmentIdentity: false, ClassLaunchConfig: false, ClassLease: false, ClassAdvisory: false,
}

// Mode is who runs a site. ModeOperator is the CLI, the API, the worker
// boundary and the session Manager, which run under either controller.
type Mode uint8

// The modes.
const (
	ModeLegacy Mode = iota + 1
	ModeV2
	ModeOperator
)

// Site is one function's role for a key.
type Site struct {
	Mode    Mode
	File    string // repo-relative
	Func    string // "Func" or "Type.Method"; empty while unbuilt
	Pending string // the bead that builds or fixes the site
	Note    string // what the site must do, when Pending
}

// Field is one key's registry entry.
type Field struct {
	Key     string
	Family  bool // Key is a prefix: the open-world opt_<option> keys
	Class   FieldClass
	Writers []Site
	Readers []Site // the consumers a Request needs; others list the decisive ones
	Clears  []Site
	// NoWriter is the ruling for a key with no production writer, NoV2Writer
	// for a premise key v2 reads and only legacy writes.
	NoWriter, NoV2Writer string
	// NewKey is CONTRACT R1's rollback rule: legacy ignores it and never
	// writes it.
	NewKey bool
	// Premise puts a key of a non-premise class in the premise: one that
	// LifecycleInput, today's fence, already compares. PerTick keeps a key
	// written every pass out of it, or every CAS on the row would lose.
	Premise, PerTick bool
}

// InPremise reports whether a fence compares f.
func (f Field) InPremise() bool { return !f.PerTick && (f.Premise || classInPremise[f.Class]) }

// Fields returns a copy of the registry.
func Fields() []Field {
	out := make([]Field, len(fields))
	for i, f := range fields {
		out[i] = cloneField(f)
	}
	return out
}

// LookupField returns a copy of key's entry, a family's for an open-world
// key. It copies only that entry: a write guard calls it per key.
func LookupField(key string) (Field, bool) {
	if i, ok := fieldIndex[key]; ok {
		return cloneField(fields[i]), true
	}
	for _, i := range familyIndex {
		if strings.HasPrefix(key, fields[i].Key) {
			return cloneField(fields[i]), true
		}
	}
	return Field{}, false
}

func cloneField(f Field) Field {
	f.Writers, f.Readers, f.Clears = slices.Clone(f.Writers), slices.Clone(f.Readers), slices.Clone(f.Clears)
	return f
}

func legacy(file, fn string) Site { return Site{Mode: ModeLegacy, File: file, Func: fn} }
func v2(file, fn string) Site     { return Site{Mode: ModeV2, File: file, Func: fn} }
func op(file, fn string) Site     { return Site{Mode: ModeOperator, File: file, Func: fn} }

func (s Site) pending(bead, note string) Site { s.Pending, s.Note = bead, note; return s }

func sites(s ...Site) []Site { return s }

// same is one entry per key for keys that share a class and their writers.
func same(class FieldClass, perTick bool, writers []Site, keys ...string) []Field {
	out := make([]Field, len(keys))
	for i, k := range keys {
		out[i] = Field{Key: k, Class: class, Writers: writers, PerTick: perTick}
	}
	return out
}

// The source files the table names most.
const (
	srcTransition = "internal/session/lifecycle_transition.go"
	srcManager    = "internal/session/manager.go"
	srcWaitStore  = "internal/session/wait_store.go"
	srcHeals      = "cmd/gc/reconcile_arms_heals.go"
	srcStopKeys   = "cmd/gc/reconcile_stop_request.go"
	srcReconciler = "cmd/gc/session_reconciler.go"
	srcReconcile  = "cmd/gc/session_reconcile.go"
	srcBeads      = "cmd/gc/session_beads.go"
	srcSleep      = "cmd/gc/session_sleep.go"
	srcPoolNames  = "cmd/gc/session_name_lookup.go"
)

// Shared sites. The pending ones' beads: mc-cfmtd (S1's PreWake, the C5a1
// replay), mc-yeiz3 (NEW-1) and mc-fmhlc (NEW-2), both the C5d replay,
// mc-h28bx (C5a3), mc-7je1h (the SESS-537/538 accrual owner).
var (
	legacyPreWake = legacy(srcTransition, "PreWakePatch")
	legacyCommit  = legacy(srcTransition, "CommitStartedPatch")
	v2PreWake     = Site{Mode: ModeV2, File: "cmd/gc/reconcile_effect_start.go"}.pending("mc-cfmtd",
		"S1's PreWake stamps the incarnation and consumes and clears the row's requests")
	v2AwakeHeal = v2(srcHeals, "armAwakeHeal").pending("mc-yeiz3",
		"A6's asleep-to-awake heal clears the wake request (D7 rule 2)")
	v2SatisfiedWake = Site{Mode: ModeV2, File: srcHeals}.pending("mc-fmhlc", "A6's satisfied-wake clear (D7)")
	v2AdoptCommit   = Site{Mode: ModeV2, File: "cmd/gc/reconcile_effect_start.go"}.pending("mc-h28bx",
		"S2's adopt commit clears the wake request (D7)")
	v2Accrual = Site{Mode: ModeV2, File: "cmd/gc/reconcile_effect_start.go"}.pending("mc-7je1h",
		"S3/S5 accrue a failed or dying start (SESS-537/538)")
	opCreate     = op(srcManager, "Manager.createBeadOnly")
	legacyCreate = legacy(srcBeads, "syncCreateMetadata")
	waitHold     = v2("cmd/gc/reconcile_steps_waits.go", "clearSessionWaitHoldFenced")
	sleepPolicy  = sites(legacy(srcSleep, "persistSleepPolicyMetadataInfo"))
	leaseAcquire = legacy("internal/session/runtime_lease.go", "RuntimeLease.acquireRecord").pending("mc-x9ygp",
		"L1b's starters, legacy and v2, take the lease (until then nothing in a mode calls it)")
)

// with adds site to the writers (asClear false) or the clears of keys.
func with(fields []Field, asClear bool, site Site, keys ...string) []Field {
	for _, k := range keys {
		f := &fields[slices.IndexFunc(fields, func(f Field) bool { return f.Key == k })]
		if asClear {
			f.Clears = append(slices.Clone(f.Clears), site)
		} else {
			f.Writers = append(slices.Clone(f.Writers), site)
		}
	}
	return fields
}

// fields is the registry, grouped by class, then the writers of v2's create
// (C1) and named reopen effects, which stamp a row's whole starting state.
var fields = withObserved(with(with(with(with(registry, false, v2("cmd/gc/reconcile_steps_nudges.go", "externalReadsLane.nudges"),
	"idle_claim_nudge_trigger", "idle_claim_nudge_count", "idle_claim_nudge_at", "continuation_claim_nudge_work",
	"continuation_claim_nudge_root", "continuation_claim_nudge_store_ref", "continuation_claim_nudge_generation",
	"continuation_claim_nudge_count", "continuation_claim_nudge_at", "execution_claim_nudge_work", "execution_claim_nudge_root",
	"execution_claim_nudge_store_ref", "execution_claim_nudge_count", "execution_claim_nudge_at", "execution_claim_nudge_stalled",
	"execution_claim_nudge_stalled_token", "execution_claim_hold"), true, v2(srcHeals, "asleepHealPatch"), "session_key", "started_config_hash",
	PrimedAtMetadataKey, PrimingAttemptedAtMetadataKey, PromptHashMetadataKey),
	false, v2("cmd/gc/allocator_create_named.go", "createEffects.writeNamed"), "agent_name", CanonicalInstanceNameMetadata, CanonicalPoolSlotMetadata,
	"command", NamedSessionIdentityMetadata, NamedSessionModeMetadata, NamedSessionMetadataKey, "continuation_epoch", "dependency_only",
	beadmeta.BoundStepIDMetadataKey, "generation", "instance_token", "live_hash", "pending_create_claim", "pending_create_started_at",
	"pool_managed", "pool_slot", "resume_command", "resume_flag", "resume_style", "session_id_flag", "session_key", "session_name",
	"session_origin", "startup_kickoff_attempts", "startup_kickoff_started_at", "startup_kickoff_state", "state", "synced_at", "template",
	"wake_mode", "work_dir", "provider", "provider_kind", "builtin_ancestor"),
	false, v2("cmd/gc/allocator_create_named.go", "reopenNamed"), "closed_at", "close_reason", "creation_complete_at", "last_woke_at", "live_hash",
	"pending_create_claim", "pending_create_started_at", PrimedAtMetadataKey, PrimingAttemptedAtMetadataKey, PromptHashMetadataKey,
	"sleep_reason", "started_config_hash", "started_live_hash", "startup_dialog_verified", "state", "state_reason", "synced_at",
	"churn_count", "held_until", "quarantined_until", "sleep_intent", "wait_hold", "wake_attempts", "wake_refused_event_at",
	beadmeta.BoundStepIDMetadataKey, "startup_kickoff_state", "startup_kickoff_started_at", "startup_kickoff_attempts", "startup_kickoff_last_nudge_at"))

// fieldIndex finds an entry by exact key, familyIndex the open-world ones.
var fieldIndex, familyIndex = func() (map[string]int, []int) {
	idx, fams := make(map[string]int, len(fields)), []int(nil)
	for i, f := range fields {
		idx[f.Key] = i
		if f.Family {
			fams = append(fams, i)
		}
	}
	return idx, fams
}()

// observed is what F1c's flows watched each site write beyond the entries'
// own columns: the site, whether it writes "" (a clear), and the keys.
var observed = []struct {
	site    Site
	asClear bool
	keys    []string
}{
	{legacy(srcReconciler, "finalizeDrainAckStoppedSession"), false, []string{"last_woke_at", "pending_create_claim", "pending_create_started_at", "slept_at", "state", "state_reason"}},
	{op("internal/session/kill_fence.go", "KillPendingPatch"), false, []string{"last_woke_at", "pending_create_claim", "pending_create_started_at", "sleep_intent", "sleep_reason", "state", "suspended_at", "synced_at"}},
	{op("internal/session/kill_fence.go", "KillPendingPatch"), true, []string{"wake_request", "wake_requested_at"}},
	{legacy(srcTransition, "CommitStartedPatch"), true, []string{"reset_committed_at"}},
	{legacy(srcTransition, "ConfigDriftResetPatch"), false, []string{"continuation_reset_pending", "last_woke_at", "live_hash", "pending_create_claim", "pending_create_started_at", "primed_at", "priming_attempted_at", "prompt_hash", "session_key", "started_config_hash", "started_live_hash", "startup_dialog_verified", "state"}},
	{legacy(srcTransition, "ConfigDriftResetPatch"), true, []string{"restart_requested"}},
	{legacy(srcTransition, "PreWakePatch"), false, []string{"sleep_reason"}},
	{legacy(srcTransition, "PreWakePatch"), true, []string{"sleep_intent"}},
	{legacy(srcTransition, "RestartRequestPatch"), false, []string{"continuation_reset_pending", "last_woke_at", "pending_create_claim", "pending_create_started_at", "primed_at", "priming_attempted_at", "prompt_hash", "session_key", "started_config_hash"}},
	{legacy(srcTransition, "RetireNamedSessionPatch"), false, []string{"pending_create_claim", "pending_create_started_at", "state", "state_reason"}},
	{op(srcManager, "Manager.createBeadOnly"), false, []string{"continuation_epoch", "instance_token", "pending_create_claim", "pending_create_started_at", "provider", "resume_command", "resume_flag", "resume_style", "session_id_flag", "session_origin", "state", "work_dir"}},
	{op(srcManager, "Manager.suspend"), false, []string{"sleep_reason", "slept_at", "state"}},
	{op(srcManager, "Manager.suspend"), true, []string{"wake_request", "wake_requested_at"}},
	{op("internal/session/store.go", "Store.SetState"), false, []string{"state", "state_reason"}},
	{op(srcWaitStore, "Store.wakeSessionFromBead"), false, []string{"churn_count", "held_until", "quarantined_until", "sleep_intent", "wait_hold", "wake_attempts", "wake_refused_event_at"}},
	{v2("cmd/gc/allocator_create_named.go", "reopenNamed"), true, []string{RuntimeLeaseHolderKey, RuntimeLeaseExpiresKey, RuntimeLeaseTTLKey, RuntimeLeaseFlockKey}},
	{op(srcManager, "Manager.Archive"), false, []string{"archived_at", "continuity_eligible", "pending_create_claim", "pending_create_started_at", "state", "state_reason"}},
}

func withObserved(fields []Field) []Field {
	for _, o := range observed {
		fields = with(fields, o.asClear, o.site, o.keys...)
	}
	return fields
}

var registry = slices.Concat([]Field{
	// Incarnation.
	{Key: "generation", Class: ClassIncarnation, Writers: sites(legacyPreWake, opCreate, v2PreWake)},
	{Key: "instance_token", Class: ClassIncarnation, Writers: sites(legacyPreWake,
		v2("cmd/gc/reconcile_arms_identity.go", "armIdentity"), v2("cmd/gc/allocator_create_named.go", "recordRowToken"))},

	// Lifecycle.
	{Key: "state", Class: ClassLifecycle, Writers: sites(legacyPreWake, v2(srcHeals, "armAwakeHeal"), v2(srcHeals, "asleepHealPatch"))},
	{Key: "state_reason", Class: ClassLifecycle, Writers: sites(legacyCommit, op("internal/session/kill_fence.go", "KillPendingPatch"))},
	{Key: "sleep_reason", Class: ClassLifecycle, Writers: sites(legacy(srcTransition, "SleepPatch"), v2(srcHeals, "asleepHealPatch")), Clears: sites(waitHold)},
	{Key: "slept_at", Class: ClassLifecycle, Writers: sites(legacy(srcTransition, "SleepPatch"),
		op("internal/session/kill_fence.go", "KillPendingPatch"))},
	{Key: "last_woke_at", Class: ClassLifecycle, Writers: sites(legacyPreWake, v2PreWake)},
	{Key: "drain_at", Class: ClassLifecycle, Writers: sites(legacy(srcReconciler, "markDrainAckStopPending")), NoV2Writer: "v2 reads legacy residue: its stop request (D1) replaces drain_at"},
	{Key: "pending_create_claim", Class: ClassLifecycle, Writers: sites(legacy(srcPoolNames, "createPoolSessionBeadWithIdentifiers")), Clears: sites(v2(srcHeals, "armClaimClear"))},
	{Key: "pending_create_started_at", Class: ClassLifecycle, Writers: sites(legacyPreWake), Clears: sites(v2(srcHeals, "armClaimClear"))},
	{Key: "creation_complete_at", Class: ClassLifecycle, Writers: sites(legacyCommit)},
	{Key: "archived_at", Class: ClassLifecycle, Writers: sites(legacy(srcTransition, "RetireNamedSessionPatch"), op(srcWaitStore, "Store.wakeSessionFromBead"))},
	{Key: "continuity_eligible", Class: ClassLifecycle, Writers: sites(legacy(srcTransition, "RetireNamedSessionPatch"), op(srcWaitStore, "Store.wakeSessionFromBead"))},
	{Key: "close_reason", Class: ClassLifecycle, Writers: sites(legacy(srcTransition, "ClosePatch"))},
	{Key: "closed_at", Class: ClassLifecycle, Writers: sites(legacy(srcTransition, "ClosePatch"))},

	// Operator intent.
	{Key: "held_until", Class: ClassOperatorIntent, Writers: sites(op("cmd/gc/cmd_session.go", "managedSuspendPatch")), Clears: sites(v2(srcTransition, "ClearExpiredHoldPatch"))},
	{Key: "quarantined_until", Class: ClassOperatorIntent, Writers: sites(legacy(srcReconcile, "recordRateLimitQuarantine"), v2Accrual), Clears: sites(v2(srcHeals, "armStabilityClear"))},
	{Key: "sleep_intent", Class: ClassOperatorIntent, Writers: sites(op("cmd/gc/cmd_session.go", "managedSuspendPatch"), legacy(srcSleep, "markIdleSleepPendingInfo")), Clears: sites(waitHold)},
	{Key: "wait_hold", Class: ClassOperatorIntent, Writers: sites(op("cmd/gc/cmd_wait.go", "doSessionWait")), Clears: sites(waitHold)},
	{Key: "suspended_at", Class: ClassOperatorIntent, Writers: sites(op(srcManager, "Manager.suspend"))},
	{Key: "pin_awake", Class: ClassOperatorIntent, Writers: sites(op("cmd/gc/cmd_session_pin.go", "cmdSessionSetPin")), Readers: sites(v2("cmd/gc/allocator_decide.go", "decidePass.awake"))},
	// template_overrides is a per-session setting the start reads and nothing
	// clears, so it is operator intent, not a request.
	{Key: "template_overrides", Class: ClassOperatorIntent, Writers: sites(op(srcManager, "Manager.UpdateTemplateOverrides"))},

	// Requests (D7 for the wake request).
	{Key: "wake_request", Class: ClassRequest, Writers: sites(op(srcWaitStore, "Store.wakeSessionFromBead")), Readers: sites(legacy("internal/session/lifecycle_projection.go", "projectWakeCauses"), v2("internal/session/lifecycle_projection.go", "projectWakeCauses")), Clears: sites(legacyPreWake, v2PreWake, v2AwakeHeal, v2SatisfiedWake, v2AdoptCommit)},
	{Key: "wake_requested_at", Class: ClassRequest, Writers: sites(op(srcWaitStore, "Store.wakeSessionFromBead")), Readers: sites(legacy(srcReconciler, "explicitWakePendingInfo"), v2(srcReconciler, "explicitWakePendingInfo"), v2SatisfiedWake), Clears: sites(legacyPreWake, v2PreWake, v2AwakeHeal, v2SatisfiedWake, v2AdoptCommit)},
	{Key: "restart_requested", Class: ClassRequest, Writers: sites(op(srcManager, "Manager.RequestFreshRestart")), Readers: sites(legacy(srcReconciler, "reconcileSessionBeadsTracedWithNamedDemand"), Site{Mode: ModeV2, File: "cmd/gc/reconcile_arms_lifecycle.go"}.pending("mc-029ln", "A13 stops a restart-requested row")), Clears: sites(legacy(srcTransition, "RestartRequestPatch"), legacy(srcReconciler, "finalizeDrainAckStoppedSession"), Site{Mode: ModeV2, File: "cmd/gc/reconcile_finalize.go"}.pending("mc-029ln", "D4's finalizePatch always clears it"))},
	{Key: "continuation_reset_pending", Class: ClassRequest, Writers: sites(v2(srcHeals, "asleepHealPatch"), op(srcManager, "Manager.RequestFreshRestart")), Readers: sites(legacy("cmd/gc/session_wake.go", "pendingContinuationResetNeedsFreshStart"), v2PreWake), Clears: sites(legacyCommit, v2PreWake)},

	// The stop request: the controller half, then the CLI's ack (E3), which
	// writes only in a v2 city.
	{Key: DrainIntentReasonKey, Class: ClassStopRequest, NewKey: true, Writers: sites(v2(srcStopKeys, "stopBeginPatch")), Readers: sites(v2(srcStopKeys, "readStopKeys")), Clears: sites(v2(srcStopKeys, "stopCancelPatch"))},
	{Key: DrainIntentAtKey, Class: ClassStopRequest, NewKey: true, Writers: sites(v2(srcStopKeys, "stopBeginPatch")), Readers: sites(v2(srcStopKeys, "readStopKeys")), Clears: sites(v2(srcStopKeys, "stopCancelPatch"))},
	{Key: DrainIntentIncarnationKey, Class: ClassStopRequest, NewKey: true, Writers: sites(v2(srcStopKeys, "stopBeginPatch")), Readers: sites(v2(srcStopKeys, "readStopKeys")), Clears: sites(v2(srcStopKeys, "stopCancelPatch"))},
	{Key: DrainAckIncarnationKey, Class: ClassStopRequest, NewKey: true, Writers: sites(op("cmd/gc/cmd_runtime_drain.go", "drainAckRowPatch")), Readers: sites(v2(srcStopKeys, "readStopKeys")), Clears: sites(v2(srcStopKeys, "stopVoidResiduePatch"))},
	{Key: DrainAckAtKey, Class: ClassStopRequest, NewKey: true, Writers: sites(op("cmd/gc/cmd_runtime_drain.go", "drainAckRowPatch")), Readers: sites(v2(srcStopKeys, "readStopKeys")), Clears: sites(v2(srcStopKeys, "stopVoidResiduePatch"))},

	// Counters.
	{Key: "wake_attempts", Class: ClassCounter, Writers: sites(legacy(srcReconcile, "recordWakeFailure"), v2(srcHeals, "armStabilityClear"), v2Accrual)},
	{Key: "churn_count", Class: ClassCounter, Writers: sites(legacy(srcReconcile, "recordChurn"), v2(srcHeals, "armStabilityClear"), v2Accrual)},
	{Key: "crash_count", Class: ClassCounter, NoWriter: "dead: only Manager.Reactivate writes it, and nothing calls that"},
	{Key: "quarantine_cycle", Class: ClassCounter, NoWriter: "dead: only Manager.Quarantine writes it, and nothing calls that"},
	{Key: "idle_respawn_attempts", Class: ClassCounter, Writers: sites(legacy(srcReconciler, "beginIdleRespawnDrainIfIdle"))},

	// Markers.
	{Key: "detached_at", Class: ClassMarker, Writers: sites(legacy(srcSleep, "reconcileDetachedAtInfo"), v2(srcHeals, "armDetachedAt")), Clears: sites(legacyPreWake)},
	{Key: "stranded_event_emitted_at", Class: ClassMarker, Writers: sites(legacy(srcReconciler, "emitSessionStrandedDiagnostic")), Clears: sites(v2(srcHeals, "armStrandedClear"))},
	// D2's idle latch: v2 reads it through configWakeSuppressedInfo and has
	// no writer yet (DIF-M2).
	{Key: "sleep_policy_fingerprint", Class: ClassMarker, Writers: sites(sleepPolicy[0], Site{Mode: ModeV2, File: srcStopKeys}.pending("mc-v3jws", "D2: the idle drain begin's CAS writes the latch")), Readers: sites(v2(srcSleep, "configWakeSuppressedInfoWithError"))},
	{Key: CurrentBeadIDKey, Class: ClassMarker, Writers: sites(legacy("cmd/gc/session_bead_cycle.go", "recordCurrentBeadIDOnWake"), v2(srcHeals, "armCurrentBead"))},
	{Key: beadmeta.CurrentClaimBeadIDMetadataKey, Class: ClassMarker, Writers: sites(op("internal/session/store.go", "Store.SetCurrentClaim"))},
	{Key: ResetCommittedAtKey, Class: ClassMarker, Writers: sites(legacy(srcTransition, "RestartRequestPatch"))},
	{Key: "drain_finalize", Class: ClassMarker, Writers: sites(legacy("cmd/gc/session_pool_drain_deadline.go", "retirePoolSlotAtDrainDeadline"))},
	{Key: "startup_dialog_verified", Class: ClassMarker, Writers: sites(op("internal/session/chat.go", "Manager.markStartupDialogsVerifiedLocked"))},
	{Key: "unknown_state_first_seen", Class: ClassMarker, Writers: sites(legacy(srcReconciler, "emitSessionUnknownStateDiagnostic")), Clears: sites(v2("cmd/gc/reconcile_session_decide.go", "timerHealPatch"))},
	{Key: "unknown_state_value", Class: ClassMarker, Writers: sites(legacy(srcReconciler, "emitSessionUnknownStateDiagnostic")), Clears: sites(v2("cmd/gc/reconcile_session_decide.go", "timerHealPatch"))},
	{Key: "unknown_state_escalated_at", Class: ClassMarker, Writers: sites(legacy(srcReconciler, "emitSessionUnknownStateDiagnostic")), Clears: sites(v2("cmd/gc/reconcile_session_decide.go", "timerHealPatch"))},

	// Baseline (M2).
	{Key: "started_config_hash", Class: ClassBaseline, Premise: true, Writers: sites(legacyCommit)},

	// Assignment identity (sessionAssignmentIdentifiersForConfig's reads).
	{Key: NamedSessionIdentityMetadata, Class: ClassAssignmentIdentity, Premise: true, Writers: sites(op("cmd/gc/cmd_session.go", "cmdSessionNew"))},

	// Launch configuration.
	{Key: "session_key", Class: ClassLaunchConfig, Premise: true, Writers: sites(op(srcManager, "Manager.PersistSessionKey"))},
	{Key: "alias", Class: ClassAssignmentIdentity, Writers: sites(legacy(srcPoolNames, "createPoolSessionBeadWithIdentifiers"), v2("cmd/gc/allocator_create_named.go", "createEffects.writeNamed"))},
	{Key: aliasHistoryMetadataKey, Class: ClassLaunchConfig, Writers: sites(legacy("internal/session/alias.go", "UpdatedAliasMetadata"))},
	{Key: "common_name", Class: ClassLaunchConfig, NoWriter: "retired: no production writer; name lookups still query it"},
	{Key: "work_dir", Class: ClassLaunchConfig, Writers: sites(legacyCreate, v2("cmd/gc/allocator_create.go", "createEffects.verifiedMetadata"))},
	{Key: beadmeta.WorkDirMetadataKey, Class: ClassLaunchConfig, Writers: sites(legacy("cmd/gc/build_desired_state.go", "computePoolTriggerBindingPatch"), v2("cmd/gc/allocator_create.go", "createEffects.verifiedMetadata"))},
	{Key: "continuation_epoch", Class: ClassLaunchConfig, Writers: sites(legacyPreWake)},
	{Key: "transport", Class: ClassLaunchConfig, Writers: sites(op(srcManager, "Manager.persistTransport"))},
	{Key: "manual_session", Class: ClassAssignmentIdentity, NoWriter: "retired: no production writer; legacy rows keep it and isManualSessionBead still reads it"},
	{Key: beadmeta.WorkerDirMetadataKey, Class: ClassLaunchConfig, NoWriter: "no production writer: contract.SetWorkerDir has no caller yet; WorkerDirFromMetadata reads it first"},
	{Key: CanonicalInstanceNameMetadata, Class: ClassLaunchConfig, Writers: sites(legacy("cmd/gc/session_identity.go", "desiredSessionIdentity"))},
	{Key: CanonicalPoolSlotMetadata, Class: ClassLaunchConfig, Writers: sites(legacy("cmd/gc/session_identity.go", "desiredSessionIdentity"))},
	{Key: "real_world_app_project_id", Class: ClassLaunchConfig, Writers: sites(op("internal/api/handler_session_create.go", "Server.persistSessionMeta"))},
	{Key: beadmeta.OptionMetadataPrefix, Family: true, Class: ClassLaunchConfig, Writers: sites(op(srcManager, "Manager.UpdateTemplateOverrides"))},
	{Key: beadmeta.LabelRevisionMetadataKey, Class: ClassAdvisory, NewKey: true, NoWriter: "written by internal/beads' native Dolt label CAS (LL1), outside the census dirs; a version stamp no decision reads"},
	{Key: beadmeta.InfraMigratedFromMetadataKey, Class: ClassAdvisory, Writers: sites(legacy("cmd/gc/infra_class_migrate.go", "infraMigrationRow"))},
	// Usage compute attributes a row by its run chain (beadmeta's runIDChainKeys).
	{Key: beadmeta.MoleculeIDMetadataKey, Class: ClassAdvisory, NoWriter: "no session-row writer: work beads carry it; usage compute reads a row's run chain", Readers: sites(legacy("cmd/gc/usage_compute.go", "emitComputeFactForBead"))},
	// External clients' open-world family; gc itself writes only the project id.
	{Key: "real_world_app_", Family: true, Class: ClassLaunchConfig, NoWriter: "external clients stamp real_world_app_* keys; the API exposes them, and the worker reads real_world_app_session_kind", Readers: sites(op("internal/worker/factory.go", "Factory.sessionFromRecord"))},
	{Key: "mc_session_kind", Class: ClassAdvisory, NoWriter: "retired: legacy provider sessions carry it; nothing reads or writes it now"},
	// The fixture-only family: a test that needs a key no production code
	// writes spells test_*; the universe check fails a production write of one.
	{Key: "test_", Family: true, Class: ClassAdvisory, NoWriter: "fixture-only: tests write test_* keys; production never does"},
	{Key: beadmeta.PerDispatchModelMetadataKey, Class: ClassAdvisory, NoWriter: "retired auto-stamp: no production writer; the work-option doctor clears the residue"},
},
	same(ClassBaseline, false, sites(legacyCommit), "started_provision_hash", "started_launch_hash", "started_live_hash", "live_hash", "core_hash_breakdown"),
	same(ClassAssignmentIdentity, false, sites(legacy(srcPoolNames, "createPoolSessionBeadWithIdentifiers"), opCreate), "template", "session_name"),
	same(ClassLaunchConfig, false, sites(legacyCreate), NamedSessionModeMetadata),
	same(ClassAssignmentIdentity, false, sites(op("cmd/gc/cmd_session.go", "cmdSessionNew")), NamedSessionMetadataKey),
	same(ClassAssignmentIdentity, false, sites(legacy(srcPoolNames, "createPoolSessionBeadWithIdentifiers")), "agent_name"),
	same(ClassLaunchConfig, false, sites(opCreate), "session_name_explicit"),
	same(ClassLaunchConfig, false, sites(legacyCreate, opCreate), "command"),
	same(ClassAssignmentIdentity, false, sites(legacyCreate), "session_origin", "dependency_only"),
	same(ClassLaunchConfig, false, sites(legacyCreate), "resume_flag", "resume_style", "resume_command", "session_id_flag", "wake_mode",
		beadmeta.BoundStepIDMetadataKey),
	same(ClassLaunchConfig, false, sites(legacy(srcBeads, "stampResolvedProviderSessionMetadata")), "provider", "provider_kind", "builtin_ancestor"),
	same(ClassAssignmentIdentity, false, sites(legacy(srcBeads, "syncDesiredPoolSlots")), "pool_slot", "pool_managed"),
	same(ClassLaunchConfig, false, sites(legacy(srcTransition, "RetireNamedSessionPatch")), "retired_named_identity"),
	same(ClassLaunchConfig, false, sites(legacy("internal/session/mcp_metadata.go", "WithStoredMCPMetadata")),
		MCPIdentityMetadataKey, MCPServersSnapshotMetadataKey),
	same(ClassLaunchConfig, false, sites(legacy("cmd/gc/build_desired_state.go", "computePoolTriggerBindingPatch")), beadmeta.TriggerBeadIDMetadataKey,
		beadmeta.TriggerBeadStoreRefMetadataKey, beadmeta.BrainParentSIDMetadataKey, beadmeta.PackMetadataKey, beadmeta.PackWorkspaceMetadataKey),
	same(ClassLaunchConfig, false, sites(op("internal/worker/handle_construct.go", "applyCanonicalProfileIdentityMetadata")), "worker_profile_provider_family",
		"worker_profile_transport_class", "worker_profile_behavior_version", "worker_profile_transcript_adapter_version",
		"worker_profile_compatibility_version", "worker_profile_certification_fingerprint"),
	same(ClassLaunchConfig, false, sites(op("internal/worker/handle_construct.go", "NewSessionHandle")), "worker_profile"),

	// Counters and markers of the legacy circuit breaker, and the health stamps
	// a terminal provider error writes.
	same(ClassCounter, false, sites(legacy("cmd/gc/session_circuit_breaker.go", "sessionCircuitBreaker.metadataLocked")), "session_circuit_restarts",
		"session_circuit_opened_at", "session_circuit_open_restart_count"),
	same(ClassMarker, false, sites(legacy("cmd/gc/session_circuit_breaker.go", "sessionCircuitBreaker.metadataLocked")), SessionCircuitStateMetadataKey,
		"session_circuit_reset_generation"),
	same(ClassAdvisory, true, sites(legacy("cmd/gc/session_circuit_breaker.go", "sessionCircuitBreaker.metadataLocked")), "session_circuit_last_restart",
		"session_circuit_last_progress", "session_circuit_last_observed", "session_circuit_progress_signature"),
	same(ClassMarker, false, sites(legacy(srcReconcile, "providerTerminalErrorPatch")), "provider_terminal_error",
		"provider_terminal_error_at", "session_health", "session_health_reason", "session_drainable"),
	same(ClassMarker, false, sites(legacy(srcReconciler, "emitSessionWakeRefused")), "wake_refused_event_at"),
	same(ClassMarker, false, sites(legacy(srcReconciler, "beginIdleRespawnDrainIfIdle")), "idle_respawn_bead_id"),

	// Advisory: diagnostics, pacing and usage stamps. The execution backstop's
	// are written every pass, under either controller.
	same(ClassAdvisory, true, sites(legacy("cmd/gc/execution_backstop.go", "writeExecutionClaimMarker")), "execution_claim_nudge_work",
		"execution_claim_nudge_root", "execution_claim_nudge_store_ref", "execution_claim_nudge_count", "execution_claim_nudge_at"),
	same(ClassAdvisory, true, sites(legacy("cmd/gc/execution_backstop.go", "poolExecutionBackstop.exhausted")), "execution_claim_nudge_stalled",
		"execution_claim_nudge_stalled_token"),
	same(ClassAdvisory, true, sites(legacy("cmd/gc/execution_backstop.go", "poolExecutionBackstop.observeHold")), "execution_claim_hold"),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/drain_reminder.go", "writeDrainReminderMarker")), "drain_reminder_at", "drain_reminder_count",
		"drain_reminder_drain", "drain_reminder_failed"),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/drain_reminder.go", "noteDrainReminderHold")), "drain_reminder_hold"),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/drain_ack_escalation.go", "recordDrainAckEscalationAttempt")), "drain_escalation_at",
		"drain_escalation_count", "drain_escalation_drain"),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/idle_nudge.go", "writeIdleClaimMarker")), "idle_claim_nudge_trigger", "idle_claim_nudge_count",
		"idle_claim_nudge_at"),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/idle_nudge.go", "writeContinuationClaimMarker")), "continuation_claim_nudge_work",
		"continuation_claim_nudge_root", "continuation_claim_nudge_store_ref", "continuation_claim_nudge_generation",
		"continuation_claim_nudge_count", "continuation_claim_nudge_at"),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/build_desired_state_pool_info.go", "recordDeferredNonExpandingPoolAliasConflictInfo")), "pool_alias_conflict",
		"pool_alias_conflict_count", "pool_alias_conflict_at"),
	same(ClassAdvisory, false, sites(op("internal/session/waits.go", "StampWaitLookupCapMetadata")),
		"wait_lookup_capped_at", "wait_lookup_capped_label", "wait_lookup_capped_limit", "wait_lookup_capped_source"),
	same(ClassAdvisory, false, sites(legacyCommit), "awake_started_at", PrimedAtMetadataKey, PromptHashMetadataKey),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/session_wake.go", "freshWakeResetPriorValues")), PrimingAttemptedAtMetadataKey),
	same(ClassAdvisory, false, sites(legacyCreate), "synced_at", "startup_kickoff_state", "startup_kickoff_started_at", "startup_kickoff_attempts"),
	same(ClassAdvisory, false, sites(legacy(srcBeads, "startupKickoffReopenMetadata")), "startup_kickoff_last_nudge_at"),
	same(ClassAdvisory, false, sites(op("cmd/gc/cmd_nudge.go", "stampLastNudgeDeliveredAt")), MetadataLastNudgeDeliveredAt),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/usage_compute.go", "emitComputeFactForBead")), "usage_compute_emitted_at"),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/usage_compute.go", "CityRuntime.emitDueComputeFacts")), "usage_model_swept_at"),
	same(ClassAdvisory, false, sites(op(srcManager, "Manager.PersistInvocationUsageCursor")), "invocation_usage_cursor"),
	same(ClassAdvisory, false, sites(legacy(srcReconciler, "recordNamedSessionConfigDriftDeferredAt")), "config_drift_deferred_at", "config_drift_deferred_key"),
	same(ClassAdvisory, false, sites(legacy(srcReconciler, "recordSessionAttachedConfigDriftDeferral")), "attached_config_drift_deferred_at",
		"attached_config_drift_deferred_key"),
	same(ClassAdvisory, false, sleepPolicy, "requested_sleep_after_idle", "effective_sleep_after_idle", "sleep_policy_source",
		"sleep_capability", "sleep_policy_adjustment_reason", "config_wake_suppressed"),
	same(ClassAdvisory, false, sites(legacy(srcReconciler, "mirrorStartupHealthEpisodeMetadata")), "startup_health_active_count", "startup_health_active_kind"),
	same(ClassAdvisory, false, sites(legacy("cmd/gc/warm_bind_nudge.go", "deliverWarmBindClaimNudge")), "warm_bind_nudged_for_trigger"),
	// The runtime lease (L1). Legacy writes it too, so no key is new.
	same(ClassLease, false, sites(leaseAcquire, v2("cmd/gc/reconcile_fenced_store.go", "fencedWriter.closeWithTerminalPatch")),
		RuntimeLeaseHolderKey, RuntimeLeaseExpiresKey, RuntimeLeaseTTLKey, RuntimeLeaseFlockKey),
	same(ClassLease, false, sites(leaseAcquire), RuntimeLeaseEpochKey),
)
