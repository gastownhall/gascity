package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Priming markers record that a session's launch path attempted delivery of
// the rendered startup prompt (S19 §2 confirmation signal 1). They share the
// exact lifetime of started_config_hash: written only by CommitStartedPatch
// (both-or-neither, launch-confirmed) and cleared at every started_config_hash
// clear site, so a fresh incarnation re-primes and a resumed/churned
// incarnation keeps its markers. S19 Stage 2 is WRITE-ONLY: they are
// stamped/cleared but read by no decision path yet.
const (
	// PrimedAtMetadataKey records when this incarnation's startup-prompt
	// delivery was attempted (RFC3339): a delivery mechanism was selected and
	// the runtime start returned success. It is not proof the runtime received
	// the prompt — herdr's Start records GC_HERDR_STARTUP_DELIVERY_UNCONFIRMED
	// and still returns nil when first-turn submission cannot be confirmed
	// (internal/runtime/herdr/provider.go) — and certainly not that the agent
	// consumed or began acting on it. A live worker can still be
	// idle if the provider drops submission after this stamp; the durable
	// signal that work actually began is the trigger bead becoming
	// assigned/in-progress, not this key (gastownhall/gascity#5236). Written
	// only by CommitStartedPatch (and, from Stage 4, the post-Nudge stamp) —
	// never a write-ahead attempt marker.
	PrimedAtMetadataKey = "primed_at"
	// PrimingAttemptedAtMetadataKey is the write-ahead attempt marker. Defined
	// (constant + clear sites) in Stage 2 but NEVER written here; its writer is
	// the Stage-4 awake-scan path.
	PrimingAttemptedAtMetadataKey = "priming_attempted_at"
	// PromptHashMetadataKey records the sha256 of the rendered startup *template*
	// prompt (tp.Prompt), so a later hash mismatch — the template/config the
	// session would be re-launched with changed — marks the session re-eligible.
	// It deliberately excludes the one-shot initial_message override, which is
	// appended to the delivered payload only on a first start / fresh wake and is
	// never replayed on a later re-launch; folding it in would make the stored
	// hash never match a re-derivation from the template, re-priming forever.
	PromptHashMetadataKey = "prompt_hash"
)

// primingResetKeys are the three priming markers cleared wherever
// started_config_hash is cleared (S19 Stage 2 priming-key lifetime rule). Kept
// as a slice so the six clear sites share one vocabulary.
var primingResetKeys = []string{
	PrimedAtMetadataKey,
	PrimingAttemptedAtMetadataKey,
	PromptHashMetadataKey,
}

// clearPrimingMarkers clears the three priming markers on a patch. Clearing a
// key that was never set is a no-op at the store layer (empty values clear), so
// this is behavior-preserving in a write-only stage.
func clearPrimingMarkers(patch MetadataPatch) {
	for _, k := range primingResetKeys {
		patch[k] = ""
	}
}

// resumeRunKeys are the resume backstop's run (mc-bm1af): its count of
// resume-shaped failures, and whether the last committed launch resumed.
// They belong to one conversation, so every conversation reset clears them
// (ClearResumeRun): a new conversation must not inherit the old one's run.
var resumeRunKeys = []string{"resume_failures", "resume_launch"}

// ClearResumeRun adds the resume backstop's run to patch, cleared.
func ClearResumeRun(patch MetadataPatch) {
	for _, k := range resumeRunKeys {
		patch[k] = ""
	}
}

// PromptHash returns the sha256 hex digest of the exact rendered startup
// prompt. The empty prompt hashes to "" (not the sha256 of the empty string),
// so it is one of the two independent gates — alongside promptDelivery("")
// being undelivered — that keep an empty prompt from ever stamping a priming
// marker (S19 P5).
func PromptHash(prompt string) string {
	if prompt == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// CurrentBeadIDKey records the work bead a session is currently processing.
// The reconciler writes it whenever a session is brought up for a specific
// work bead. ComputeAwakeSet uses it to detect when an alive session has been
// reassigned to a different bead — the trigger for a fresh-wake conversation
// cycle under wake_mode=fresh.
//
// Usage attribution is a second consumer: the worker reads it to find the
// formula a session's facts belong to. It is a single-valued pointer meaning
// "the last bead the controller stamped", so it survives that bead's close and
// a sleep.
const CurrentBeadIDKey = "currently_processing_bead_id"

var freshWakeConversationResetKeys = []string{
	"session_key",
	"started_config_hash",
	"started_live_hash",
	"live_hash",
	startupDialogVerifiedKey,
	// Priming markers share started_config_hash's lifetime (S19 Stage 2): a
	// fresh wake re-primes. This list and applyFreshWakeConversationReset must
	// stay aligned — TestFreshWakeResetKeysAlignWithApply enforces it.
	PrimedAtMetadataKey,
	PrimingAttemptedAtMetadataKey,
	PromptHashMetadataKey,
	// The resume backstop's run belongs to the conversation (ClearResumeRun).
	"resume_failures",
	"resume_launch",
}

// ResetCommittedAtKey records when a restart handoff durably committed.
const ResetCommittedAtKey = "reset_committed_at"

// MetadataPatch is an atomic set of metadata key updates for one lifecycle
// transition. Empty values intentionally clear metadata keys in existing store
// implementations.
type MetadataPatch map[string]string

// FreshWakeConversationResetKeys returns the metadata fields that define
// provider-conversation identity and are reset when a wake or restart must
// start fresh.
func FreshWakeConversationResetKeys() []string {
	return append([]string(nil), freshWakeConversationResetKeys...)
}

// Apply returns a merged copy of meta with the patch applied.
func (p MetadataPatch) Apply(meta map[string]string) map[string]string {
	merged := make(map[string]string, len(meta)+len(p))
	for k, v := range meta {
		merged[k] = v
	}
	for k, v := range p {
		merged[k] = v
	}
	return merged
}

// applyFreshWakeConversationReset keeps all fresh-wake paths aligned on the
// same provider-identity field set. PreWakePatch remains the authoritative
// final reset immediately before command preparation; drain/config-drift paths
// reuse the same cleared fields so fresh-wake semantics do not drift.
func applyFreshWakeConversationReset(patch MetadataPatch) {
	patch["started_config_hash"] = ""
	patch["started_live_hash"] = ""
	patch["live_hash"] = ""
	patch[startupDialogVerifiedKey] = ""
	clearPrimingMarkers(patch)
	ClearResumeRun(patch)
}

func pendingCreateStartedAt(now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.UTC().Format(time.RFC3339)
}

// awakeIntervalStartedAt formats the immutable start-of-awake-interval marker.
// Nanosecond precision lets two intervals that begin within the same wall-clock
// second still receive distinct epochs: the compute usage fact keys both its
// per-interval emit marker and its idempotency key on this value, so a coarser
// timestamp would silently drop the second interval (gastownhall/gascity#3513).
func awakeIntervalStartedAt(now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.UTC().Format(time.RFC3339Nano)
}

// RequestExplicitWakePatch records durable wake intent without claiming a
// concrete start. The reconciler consumes the request when it prepares the
// runtime start.
func RequestExplicitWakePatch(reason string, now time.Time) MetadataPatch {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return MetadataPatch{
		"wake_request":      reason,
		"wake_requested_at": now.UTC().Format(time.RFC3339),
	}
}

// ClearWakeRequestPatch drops a pending wake request (CONTRACT v5.7 D7). Its
// consumers write it in the CAS that satisfies the request, and `gc session
// suspend` and `gc session kill` in their own write, as newer operator intent.
func ClearWakeRequestPatch() MetadataPatch {
	return MetadataPatch{"wake_request": "", "wake_requested_at": ""}
}

// IndefiniteHoldDuration is the "suspended indefinitely" held_until horizon:
// 100 years is effectively forever without risking time arithmetic overflow.
const IndefiniteHoldDuration = 100 * 365 * 24 * time.Hour

// RequestWakePatch records a controller-owned one-shot create claim.
func RequestWakePatch(reason string, now time.Time) MetadataPatch {
	return MetadataPatch{
		"state":                     string(StateStartPending),
		"state_reason":              reason,
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAt(now),
		"sleep_reason":              "",
		"wake_attempts":             "0",
		"churn_count":               "0",
	}
}

// PreWakePatchInput records the metadata transition for a concrete runtime
// wake attempt. The caller computes generation, token, and continuation epoch;
// the patch owns keeping all persisted lifecycle fields consistent for that
// transition.
type PreWakePatchInput struct {
	Generation        int
	InstanceToken     string
	ContinuationEpoch int
	Now               time.Time
	SleepReason       string
	FreshWake         bool
	// EpisodePendingCreateStartedAt is the pending_create_started_at of the
	// pending-create episode this wake continues, or empty when the wake opens
	// a new episode. See PreWakePatch.
	EpisodePendingCreateStartedAt string
}

// PreWakePatch records the metadata transition for a concrete runtime wake
// attempt. It intentionally owns the StateStartPending to StateCreating
// provider-start boundary outside Transition because this patch is the atomic
// reconciler commit made immediately before runtime start.
//
// pending_create_started_at marks the start of a pending-create episode, and
// the stale-create bounds measure from it. This patch runs before every start
// attempt, so stamping it unconditionally reset that clock on each retry: a
// pending create that retried every tick never aged out and kept its alias
// indefinitely. A wake that continues an episode passes the episode's marker
// in EpisodePendingCreateStartedAt and the patch keeps it. Every path that
// sets pending_create_claim stamps a fresh marker in the same write, so a
// caller that carries the marker only while the claim is held always carries
// the start of the current episode. A marker that does not parse is replaced.
func PreWakePatch(input PreWakePatchInput) MetadataPatch {
	startedAt := input.Now.UTC().Format(time.RFC3339) // every caller passes Now (R12: no clock fallback)
	if episode := strings.TrimSpace(input.EpisodePendingCreateStartedAt); episode != "" {
		if t, err := time.Parse(time.RFC3339, episode); err == nil && !t.IsZero() {
			startedAt = episode
		}
	}
	patch := MetadataPatch{
		"instance_token":             input.InstanceToken,
		"continuation_epoch":         fmt.Sprintf("%d", input.ContinuationEpoch),
		"continuation_reset_pending": "",
		"detached_at":                "",
		"state":                      string(StateCreating),
		"pending_create_started_at":  startedAt,
		"last_woke_at":               input.Now.UTC().Format(time.RFC3339),
		"sleep_reason":               input.SleepReason,
		"generation":                 fmt.Sprintf("%d", input.Generation),
		"wake_request":               "",
		"wake_requested_at":          "",
	}
	if input.FreshWake {
		patch["session_key"] = ""
		applyFreshWakeConversationReset(patch)
	}
	return patch
}

// ContinuationResetWakePatch records a controller-owned fresh wake for a
// session whose pending continuation reset was observed while a stale runtime
// was still alive.
func ContinuationResetWakePatch(now time.Time) MetadataPatch {
	patch := RequestWakePatch("continuation-reset", now)
	patch["session_key"] = ""
	applyFreshWakeConversationReset(patch)
	patch["continuation_reset_pending"] = "true"
	return patch
}

// ClearWakeBlockersPatch is the lifecycle of clearing a dormant session's
// blockers so the normal wake path may select it; the holds themselves are
// the Wake (or Retire) cell's.
func ClearWakeBlockersPatch(state State, sleepReason string, now time.Time) MetadataPatch {
	patch := MetadataPatch{
		"wake_attempts":         "0",
		"wake_refused_event_at": "",
		"churn_count":           "0",
	}
	switch state {
	case StateSuspended, StateDrained:
		patch["state"] = string(StateAsleep)
		patch["slept_at"] = now.UTC().Format(time.RFC3339)
	}
	switch SleepReason(sleepReason) {
	case SleepReasonUserHold, SleepReasonWaitHold, SleepReasonQuarantine,
		SleepReasonContextChurn, SleepReasonRateLimit, SleepReasonDrained:
		patch["sleep_reason"] = ""
	}
	return patch
}

// ClearExpiredHoldPatch is the lifecycle of an expired hold's clear: it drops
// the displayed hold reason only when that reason came from the expired
// timer. The timer itself is the TimerTick cell's.
func ClearExpiredHoldPatch(sleepReason string) MetadataPatch {
	patch := MetadataPatch{}
	if SleepReason(sleepReason) == SleepReasonUserHold {
		patch["sleep_reason"] = ""
	}
	return patch
}

// ClearExpiredQuarantinePatch is the lifecycle of an expired quarantine-like
// timer's clear: it resets the retry counters associated with that blocker.
// The timer itself is the TimerTick cell's.
func ClearExpiredQuarantinePatch(sleepReason string) MetadataPatch {
	patch := MetadataPatch{
		"wake_attempts": "0",
		"churn_count":   "0",
	}
	switch SleepReason(sleepReason) {
	case SleepReasonQuarantine, SleepReasonContextChurn, SleepReasonRateLimit:
		patch["sleep_reason"] = ""
	}
	return patch
}

// ConfirmStartedPatch records a confirmed runtime start. The timestamp pins
// the "creation_complete" transition so downstream readers (e.g. the pool
// bead sweep) can distinguish a just-committed start from a long-stable
// bead whose last_woke_at was later cleared by crash/churn recovery.
func ConfirmStartedPatch(now time.Time) MetadataPatch {
	return MetadataPatch{
		"state":                string(StateActive),
		"state_reason":         "creation_complete",
		"creation_complete_at": now.UTC().Format(time.RFC3339),
		// awake_started_at is the immutable start of this awake interval. Unlike
		// last_woke_at (a wake-attempt lease cleared by many teardown paths) it
		// is never cleared, so a compute usage fact can recover the interval
		// start and key idempotency on it at teardown. Every confirmed start
		// stamps a fresh epoch so each awake interval bills exactly once.
		"awake_started_at":          awakeIntervalStartedAt(now),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
		"sleep_reason":              "",
	}
}

// CommitStartedPatchInput describes metadata persisted after a runtime start
// has completed. Hashes describe the runtime configuration that actually
// launched; ConfirmState controls whether this start should stamp lifecycle
// state active. Now is used to stamp creation_complete_at whenever it is
// non-zero (independent of ConfirmState, so the recovery path that commits
// a fresh start on an already-active bead still refreshes the sweep's
// post-create marker). ClearPendingCreateClaim folds the
// pending_create_claim clear into the same atomic batch so downstream
// readers (e.g. the pool bead sweep) never observe a transient state
// where the claim is gone but the post-create marker hasn't landed yet.
type CommitStartedPatchInput struct {
	CoreHash                string
	LiveHash                string
	ProvisionHash           string
	LaunchHash              string
	CoreBreakdown           string
	ConfirmState            bool
	ClearSleepReason        bool
	ClearPendingCreateClaim bool
	// StartsAwakeInterval marks a commit that begins a new awake interval: a
	// first start, a wake from a dormant state, or a start (or its recovery)
	// committed onto a bead already awake or active (heal can mark it awake
	// while the start is in flight) that has no interval still open (no epoch,
	// or one the compute lane has already accounted). A commit onto a bead
	// whose interval is still open leaves it false, so that interval keeps its
	// epoch. When true the patch stamps a fresh awake_started_at epoch so each
	// awake interval emits exactly one compute usage fact; the controller wake
	// path does not otherwise refresh it (gastownhall/gascity#3513).
	StartsAwakeInterval bool
	Now                 time.Time
	// PrimedAt, when non-zero and PromptHash is non-empty, records that this
	// start's launch path attempted delivery of the rendered startup prompt
	// (S19 §2 confirmation signal 1). Emitted atomically with
	// started_config_hash so priming inherits the start path's crash
	// semantics. Zero PrimedAt (or an empty PromptHash) ⇒ no priming keys, so
	// a resume/recovery that delivered nothing stamps nothing.
	// priming_attempted_at is never emitted here.
	PrimedAt   time.Time
	PromptHash string
	// RuntimeBinding is the patch recording the thread the runtime hosts
	// the session in, read after the start (RuntimeBindingPatch); empty
	// writes no binding keys.
	RuntimeBinding MetadataPatch
}

// CommitStartedPatch records a successful runtime start atomically with the
// configuration hashes that future drift checks use.
func CommitStartedPatch(input CommitStartedPatchInput) MetadataPatch {
	patch := MetadataPatch{
		"started_config_hash":        input.CoreHash,
		"live_hash":                  input.LiveHash,
		"started_live_hash":          input.LiveHash,
		"started_provision_hash":     input.ProvisionHash,
		"started_launch_hash":        input.LaunchHash,
		"continuation_reset_pending": "",
		ResetCommittedAtKey:          "",
	}
	if input.CoreBreakdown != "" {
		patch["core_hash_breakdown"] = input.CoreBreakdown
	}
	if input.ConfirmState {
		patch["state"] = string(StateActive)
		patch["state_reason"] = "creation_complete"
		patch["pending_create_started_at"] = ""
	}
	// creation_complete_at tracks when the runtime was last confirmed started.
	// Stamp it whenever Now is non-zero — the ConfirmState path marks the
	// fresh transition from creating/asleep; the recovery path (already-
	// active bead with pending_create_claim=true) re-confirms an existing
	// start, so it needs the same marker so the post-create sweep guard
	// doesn't treat the healed bead as stale on subsequent ticks.
	if !input.Now.IsZero() {
		patch["creation_complete_at"] = input.Now.UTC().Format(time.RFC3339)
	}
	if input.ClearSleepReason {
		patch["sleep_reason"] = ""
	}
	if input.ClearPendingCreateClaim {
		patch["pending_create_claim"] = ""
		patch["pending_create_started_at"] = ""
	}
	// A genuine (re)start opens a new awake interval. Stamp a fresh, immutable
	// epoch so the compute usage fact for this interval is distinct from any
	// prior interval on a reused session bead; a recovery re-confirmation of an
	// already-running runtime leaves the in-flight interval's epoch untouched.
	if input.StartsAwakeInterval {
		patch["awake_started_at"] = input.Now.UTC().Format(time.RFC3339Nano) // every caller passes Now (R12: no clock fallback)
	}
	// Priming confirmation pair (both-or-neither). Stamped atomically with
	// started_config_hash so priming inherits its crash semantics and lifetime.
	if !input.PrimedAt.IsZero() && input.PromptHash != "" {
		patch[PrimedAtMetadataKey] = input.PrimedAt.UTC().Format(time.RFC3339)
		patch[PromptHashMetadataKey] = input.PromptHash
	}
	maps.Copy(patch, input.RuntimeBinding)
	return patch
}

// RuntimeBindingPatch reads the thread sp hosts the session name in and
// returns the patch that records it on the session row. A runtime that is
// not a [runtime.SessionBindingProvider], or that does not host name in a
// thread (a zero binding), writes nothing. When no thread is
// bound ([runtime.ErrSessionNotFound]), the patch clears both keys, so the
// row never names a previous incarnation's thread as current. Any other read
// error writes nothing and is returned: the keys then stay as of the last
// successful read.
func RuntimeBindingPatch(sp runtime.Provider, name string) (MetadataPatch, error) {
	bp, ok := sp.(runtime.SessionBindingProvider)
	if !ok {
		return MetadataPatch{}, nil
	}
	binding, err := bp.SessionBinding(name)
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		binding = runtime.SessionBinding{}
	case err != nil:
		return MetadataPatch{}, fmt.Errorf("reading the runtime binding of %q: %w", name, err)
	case binding.ThreadID == "":
		return MetadataPatch{}, nil
	}
	return MetadataPatch{
		MetadataRuntimeThreadID:  binding.ThreadID,
		MetadataRuntimeProjectID: binding.ProjectID,
	}, nil
}

// BeginDrainPatch transitions a live session into draining.
func BeginDrainPatch(now time.Time, reason string) MetadataPatch {
	return MetadataPatch{
		"state":        string(StateDraining),
		"state_reason": reason,
		"drain_at":     now.UTC().Format(time.RFC3339),
	}
}

// DrainAckStopPendingReason marks a drain-acked runtime whose provider stop is
// running asynchronously and waiting for controller finalization.
const DrainAckStopPendingReason = "drain-ack-stop-pending"

// DrainAckStopPendingPatch records that a drain-acked session has moved into
// durable stop-pending state. The provider stop itself is asynchronous; the
// controller finalizes the bead with the normal drain completion patches after
// observing the runtime stopped.
func DrainAckStopPendingPatch(now time.Time) MetadataPatch {
	patch := BeginDrainPatch(now, DrainAckStopPendingReason)
	patch["pending_create_claim"] = ""
	patch["pending_create_started_at"] = ""
	return patch
}

// RevertDrainAckStopPendingPatch undoes DrainAckStopPendingPatch on a row
// whose stop was never queued, restoring the lifecycle fields prior — the row
// as read before the stop-pending mark — carried. The caller commits it
// fenced on the stop-pending decision, so it lands only on the row that mark
// produced and never over a later writer's state that its read sees; only a
// store with conditional writes also refuses a writer that lands between that
// read and the write (Store.Commit).
func RevertDrainAckStopPendingPatch(prior Info) MetadataPatch {
	return MetadataPatch{
		"state":                     prior.MetadataState,
		"state_reason":              prior.StateReason,
		"drain_at":                  prior.DrainAt,
		"pending_create_claim":      prior.PendingCreateClaimMetadata,
		"pending_create_started_at": prior.PendingCreateStartedAt,
	}
}

// SleepPatch records a non-terminal sleep/drain result. The holds are the
// Sleep cell's (Store.ApplySleep).
func SleepPatch(now time.Time, reason string) MetadataPatch {
	return MetadataPatch{
		"state":                     string(StateAsleep),
		"state_reason":              "",
		"sleep_reason":              reason,
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
		"slept_at":                  now.UTC().Format(time.RFC3339),
	}
}

// KeepQuarantineReason drops patch's sleep_reason while the row's quarantine
// is in force at now and its reason is the quarantine's (quarantine,
// rate_limit, context-churn): a finalize landing after a late quarantine
// never overwrites its reason, as the quarantine dominates (mc-slkxd), nor
// does a crash or churn accrual during it (ExitAccrual.Over, v5.9z). It
// returns patch, edited in place.
func KeepQuarantineReason(patch MetadataPatch, d Disposition, reason string, now time.Time) MetadataPatch {
	if _, ok := patch["sleep_reason"]; !ok || !d.Quarantine.After(now) {
		return patch
	}
	switch SleepReason(strings.TrimSpace(reason)) {
	case SleepReasonQuarantine, SleepReasonRateLimit, SleepReasonContextChurn:
		delete(patch, "sleep_reason")
	}
	return patch
}

// AcknowledgeDrainPatch records an agent-acknowledged drain. Drained is a
// compatibility state distinct from ordinary asleep: demand alone does not
// reselect it, but explicit attach or work can. Like SleepPatch, it stamps
// slept_at alongside clearing last_woke_at so a same-tick drain-ack falls
// back to this fairness key instead of collapsing straight to CreatedAt
// (#2574) — drain-ack is the dominant real-world drain path for
// wake_mode=fresh roles.
func AcknowledgeDrainPatch(now time.Time, freshWake bool) MetadataPatch {
	patch := MetadataPatch{
		"state":                     string(StateDrained),
		"state_reason":              "",
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
		"slept_at":                  now.UTC().Format(time.RFC3339),
	}
	if freshWake {
		patch["session_key"] = ""
		applyFreshWakeConversationReset(patch)
		patch["continuation_reset_pending"] = "true"
	}
	return patch
}

// CompleteDrainPatch records a completed controller drain as ordinary asleep.
func CompleteDrainPatch(now time.Time, reason string, freshWake bool) MetadataPatch {
	patch := SleepPatch(now, reason)
	patch["state_reason"] = ""
	if freshWake {
		patch["session_key"] = ""
		applyFreshWakeConversationReset(patch)
		patch["continuation_reset_pending"] = "true"
	}
	return patch
}

// RestartRequestPatch records a controller handoff to a fresh provider
// conversation. It intentionally clears only the fields that force the next
// wake onto a first-start path; started_live_hash/live_hash remain intact until
// the next successful start rewrites them so restart-in-flight drift readers do
// not observe an empty-hash backfill state. The caller owns stopping any
// currently running runtime.
func RestartRequestPatch(sessionKey string, now time.Time) MetadataPatch {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return RestartRequestPatchAt(sessionKey, now)
}

// RestartRequestPatchAt is RestartRequestPatch at now, reading no clock:
// v2's restart handoff (A13).
func RestartRequestPatchAt(sessionKey string, now time.Time) MetadataPatch {
	patch := MetadataPatch{
		"restart_requested":          "",
		"started_config_hash":        "",
		"continuation_reset_pending": "true",
		ResetCommittedAtKey:          now.UTC().Format(time.RFC3339),
		"last_woke_at":               "",
		"pending_create_claim":       "",
		"pending_create_started_at":  "",
	}
	// A restart handoff clears started_config_hash to force the next wake onto a
	// first-start path, so the priming markers share that clear (S19 Stage 2
	// priming-key lifetime rule): the fresh conversation must re-prime rather
	// than inherit the previous incarnation's confirmation pair. The fresh
	// conversation starts the resume backstop's run anew too.
	clearPrimingMarkers(patch)
	ClearResumeRun(patch)
	if sessionKey != "" {
		patch["session_key"] = sessionKey
	}
	return patch
}

// ConfigDriftResetPatch records an in-place named-session repair after core
// config drift. Creating claims a new runtime start; asleep stays dormant
// until the next normal wake reason.
func ConfigDriftResetPatch(nextState State, sessionKey string, now time.Time) MetadataPatch {
	patch := configDriftReset(nextState, sessionKey)
	if nextState == StateCreating || nextState == StateStartPending {
		patch["pending_create_claim"] = "true"
		patch["pending_create_started_at"] = pendingCreateStartedAt(now)
	}
	return patch
}

// ConfigDriftAsleepResetPatch is ConfigDriftResetPatch(StateAsleep, ...),
// which reads no clock: the v2 asleep named repair's (SESS-568).
func ConfigDriftAsleepResetPatch(sessionKey string) MetadataPatch {
	return configDriftReset(StateAsleep, sessionKey)
}

// ConfigDriftRestartPatch is ConfigDriftResetPatch(StateStartPending, ...)
// at now, which it reads from its caller: the v2 named restart in place's
// (SESS-557), written after its stop.
func ConfigDriftRestartPatch(sessionKey string, now time.Time) MetadataPatch {
	patch := configDriftReset(StateStartPending, sessionKey)
	patch["pending_create_claim"] = "true"
	patch["pending_create_started_at"] = now.UTC().Format(time.RFC3339)
	return patch
}

// configDriftReset is a config-drift reset into nextState, its pending
// create claim (a start's) aside.
func configDriftReset(nextState State, sessionKey string) MetadataPatch {
	patch := MetadataPatch{
		"state":                      string(nextState),
		"last_woke_at":               "",
		"restart_requested":          "",
		"continuation_reset_pending": "true",
		"pending_create_claim":       "",
		"pending_create_started_at":  "",
	}
	applyFreshWakeConversationReset(patch)
	if sessionKey != "" {
		patch["session_key"] = sessionKey
	}
	return patch
}

// ArchivePatch transitions a retired session into archived history.
func ArchivePatch(now time.Time, reason string, continuityEligible bool) MetadataPatch {
	continuity := "false"
	if continuityEligible {
		continuity = "true"
	}
	return MetadataPatch{
		"state":                     string(StateArchived),
		"state_reason":              reason,
		"archived_at":               now.UTC().Format(time.RFC3339),
		"continuity_eligible":       continuity,
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}
}

// ClosePatch records terminal close metadata before the bead status is closed.
//
// state retains the canonical short stateCode (used by reconciler logic and
// closedNamedSessionReopenEligible). close_reason is expanded via
// CanonicalCloseReason so cities running with validation.on-close=error
// (which rejects close reasons under 20 characters) accept the close.
// Without the expansion, every short-code session-close call (gc_swept,
// orphaned, drained, failed-create, ...) is silently rejected by the
// validator, leaving close_reason/closed_at stamped on an OPEN bead and
// triggering reconciler flap as the next tick clears them.
//
// Every close except a failed create also carries UsageClosePatch: the close
// commits status=closed with it, so the row leaves the open set the usage lane
// scans, and the marker is what keeps the interval it ended accounted. A failed
// create never confirmed a start, so it has no interval and writes neither key.
func ClosePatch(now time.Time, stateCode string) MetadataPatch {
	ts := now.UTC().Format(time.RFC3339)
	patch := MetadataPatch{
		"state":        stateCode,
		"close_reason": CanonicalCloseReason(stateCode),
		"closed_at":    ts,
		"synced_at":    ts,
	}
	if stateCode != string(StateFailedCreate) {
		maps.Copy(patch, UsageClosePatch(now))
	}
	return patch
}

// UsageClosePatch marks a closing row as possibly holding an awake interval
// the usage lane has not accounted. usage_close_pending is the equality the
// lane lists closed rows by; usage_closed_at is the interval's end, kept apart
// from closed_at because a named-session reopen clears closed_at before the
// lane may have run. It drops the start and key an earlier close's reopen kept
// (UsageReopenPatch): this close ended the row's current interval. The lane
// clears every key once the interval is settled (UsageCloseClearPatch).
func UsageClosePatch(now time.Time) MetadataPatch {
	return MetadataPatch{
		MetadataKeyUsageClosePending:         "true",
		MetadataKeyUsageClosedAt:             now.UTC().Format(time.RFC3339Nano),
		MetadataKeyUsageClosedAwakeStartedAt: "",
		MetadataKeyUsageClosedSessionKey:     "",
	}
}

// UsageReopenPatch keeps the interval a pending usage close ended identifiable
// across the reopen of its closed row, from closedMeta, the row's metadata.
// A closed row never starts and never changes its session_key, but the reopen
// lets it: its next start stamps a new awake_started_at, a fresh wake clears
// the key, and both can land before the usage lane settles the closed
// interval. The patch copies both onto the close marker, once per close: a
// reopen of a row whose kept values are already set (its start failed and it
// closed again as a failed create, which leaves the marker) keeps the first.
// It is empty when no usage close is pending or the row has no interval.
func UsageReopenPatch(closedMeta map[string]string) MetadataPatch {
	start := strings.TrimSpace(closedMeta["awake_started_at"])
	if closedMeta[MetadataKeyUsageClosePending] != "true" || start == "" ||
		strings.TrimSpace(closedMeta[MetadataKeyUsageClosedAwakeStartedAt]) != "" {
		return nil
	}
	return MetadataPatch{
		MetadataKeyUsageClosedAwakeStartedAt: start,
		MetadataKeyUsageClosedSessionKey:     closedMeta["session_key"],
	}
}

// UsageClosedIntervalMetadata returns meta as of the awake interval its
// pending usage close ended: the awake_started_at and session_key a reopen
// kept (UsageReopenPatch) stand in for the row's current ones, so that
// interval is accounted the same whatever the row did after the reopen. It
// returns meta itself when the reopen kept nothing.
func UsageClosedIntervalMetadata(meta map[string]string) map[string]string {
	start := strings.TrimSpace(meta[MetadataKeyUsageClosedAwakeStartedAt])
	if start == "" {
		return meta
	}
	interval := maps.Clone(meta)
	interval["awake_started_at"] = start
	interval["session_key"] = meta[MetadataKeyUsageClosedSessionKey]
	return interval
}

// UsageCloseClearPatch clears a settled usage close: the marker and the
// interval start and key a reopen kept.
func UsageCloseClearPatch() MetadataPatch {
	return MetadataPatch{
		MetadataKeyUsageClosePending:         "",
		MetadataKeyUsageClosedAt:             "",
		MetadataKeyUsageClosedAwakeStartedAt: "",
		MetadataKeyUsageClosedSessionKey:     "",
	}
}

// UsageIntervalEnd returns where the awake interval that began at start ended,
// for compute wall time and the model-usage sweep window. A usage close marker
// later than start bounds the interval: slept_at wins when it falls inside
// (start, usage_closed_at], since the session slept before the close. Without
// one, slept_at ends the interval when it is after start (one carried over
// from a prior interval is not), else now.
func UsageIntervalEnd(meta map[string]string, start, now time.Time) time.Time {
	slept, sleptErr := time.Parse(time.RFC3339, strings.TrimSpace(meta["slept_at"]))
	sleptInside := sleptErr == nil && slept.After(start)
	if closedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(meta[MetadataKeyUsageClosedAt])); err == nil && closedAt.After(start) {
		if sleptInside && !slept.After(closedAt) {
			return slept
		}
		return closedAt
	}
	if sleptInside {
		return slept
	}
	return now
}

// CanonicalCloseReason maps a short session stateCode to a human-readable
// close reason of at least 20 characters, suitable for use as
// `bd close --reason` under validation.on-close=error.
//
// Unknown non-empty codes fall back to "session terminated: <code>".
// Codes already 20+ characters pass through unchanged.
func CanonicalCloseReason(stateCode string) string {
	switch stateCode {
	case "":
		return "session terminated: unknown state"
	case "gc_swept":
		return "session swept: no assigned work in any rig"
	case "orphaned":
		return "session orphaned: configured agent removed"
	case "drained":
		return "session drained: pool slot retired by reconciler"
	case "failed-create":
		return "session create failed: aborted before creation_complete"
	case "stale-session":
		return "session stale: no liveness signal beyond threshold"
	case "duplicate":
		return "session retired: duplicate of canonical bead"
	case "duplicate-repair":
		return "session retired: duplicate-repair canonicalization"
	case "reconfigured":
		return "session reconfigured: superseded by new agent config"
	case "suspended":
		return "session suspended: agent disabled in city config"
	}
	if len(stateCode) >= 20 {
		return stateCode
	}
	return fmt.Sprintf("session terminated: %s", stateCode)
}

// RetireNamedSessionPatch archives a named-session bead without closing it so
// historical references can be reassigned while canonical identifiers are freed.
func RetireNamedSessionPatch(now time.Time, reason, identity string) MetadataPatch {
	patch := ArchivePatch(now, reason, false)
	patch["alias"] = ""
	patch["session_name"] = ""
	patch["session_name_explicit"] = ""
	// Free the durable canonical-identity record (S19) alongside the legacy
	// alias/session_name identifiers, so an archived duplicate/removed named
	// session no longer carries a live canonical instance name or pool slot —
	// matching this patch's contract that canonical identifiers are freed.
	freeCanonicalIdentityMetadata(patch)
	patch["synced_at"] = now.UTC().Format(time.RFC3339)
	patch["sleep_reason"] = ""
	if identity != "" {
		patch["retired_named_identity"] = identity
	}
	return patch
}

// QuarantinePatch is the lifecycle of a crash-loop quarantine; its
// quarantined_until is the Quarantine cell's.
func QuarantinePatch(cycle int) MetadataPatch {
	return MetadataPatch{
		"state":            string(StateQuarantined),
		"state_reason":     "crash-loop",
		"quarantine_cycle": fmt.Sprintf("%d", cycle),
		"last_woke_at":     "",
	}
}

// ReactivatePatch clears quarantine/archive metadata and makes the session
// eligible for normal wake machinery when continuityEligible is true. It does
// not claim that a runtime is already alive.
func ReactivatePatch(continuityEligible bool) MetadataPatch {
	continuity := "false"
	if continuityEligible {
		continuity = "true"
	}
	return MetadataPatch{
		"state":                     string(StateAsleep),
		"state_reason":              "reactivated",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
		"continuity_eligible":       continuity,
		"crash_count":               "0",
		"archived_at":               "",
	}
}
