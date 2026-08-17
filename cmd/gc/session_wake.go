package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessions "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/worker"
)

var (
	// errTokenMismatch indicates the running session's instance token does not
	// match the expected one — the session was re-woken by a different
	// incarnation and this drain/stop is stale.
	errTokenMismatch = errors.New("instance token mismatch")
	// errDrainLifecycleSuperseded means a durable lifecycle transition (including
	// a same-token start with a newer awake epoch) committed after the caller's
	// drain snapshot. The stale drain must be retired without touching runtime or
	// session state.
	errDrainLifecycleSuperseded = errors.New("drain lifecycle superseded")
	// errDrainLifecycleBusy is a fail-closed retry signal: a start, wake, suspend,
	// or another finalizer currently owns the per-session lifecycle lock.
	errDrainLifecycleBusy = errors.New("session lifecycle busy")
	// errRuntimeIdentityUnavailable is distinct from a positive mismatch. Missing
	// live identity is not authority to kill a name-scoped runtime; retain the
	// drain and retry after metadata/provider recovery.
	errRuntimeIdentityUnavailable = errors.New("runtime identity unavailable")
)

type drainCompletionOutcome uint8

const (
	drainCompletionDeferred drainCompletionOutcome = iota
	drainCompletionApplied
	drainCompletionSuperseded
)

// tryWithCurrentDrainLifecycle acquires the non-blocking lifecycle fence and
// re-reads the authoritative persisted epoch before fn may mutate state or stop
// a runtime. instance_token alone is insufficient because Manager.Start
// intentionally preserves it; awake_started_at and the rest of
// drainAckLifecycleVersion distinguish the newer interval.
func tryWithCurrentDrainLifecycle(
	cityPath string,
	expected sessions.Info,
	sessFront *sessions.Store,
	allowOperatorPark bool,
	fn func(sessions.Info) error,
) (acquired, superseded bool, err error) {
	if sessFront == nil || strings.TrimSpace(expected.ID) == "" {
		return false, false, errors.New("session lifecycle store unavailable")
	}
	acquired, attachBusy, err := sessions.TryWithCitySessionDestructiveLock(cityPath, expected.ID, func() error {
		latest, getErr := sessFront.GetLive(expected.ID)
		if getErr != nil {
			return getErr
		}
		if sessions.HasExecutionClaimNudgeStalled(latest) {
			superseded = true
			return nil
		}
		if drainAckVersionOf(latest) != drainAckVersionOf(expected) &&
			(!allowOperatorPark || !operatorParkPreservesDrainStopIdentity(expected, latest)) {
			superseded = true
			return nil
		}
		return fn(latest)
	})
	return acquired && !attachBusy, superseded, err
}

// operatorParkPreservesDrainStopIdentity permits one intentional lifecycle
// delta: Suspend persists its hold before provider Stop. If that Stop fails, a
// previously queued drain still owns teardown of the same runtime. Normalize
// only the three fields Suspend changes; every identity/start discriminator and
// all other lifecycle fields must remain byte-identical. Explicit wake is never
// compatible.
func operatorParkPreservesDrainStopIdentity(expected, latest sessions.Info) bool {
	return operatorParkVersionsPreserveDrainStopIdentity(drainAckVersionOf(expected), drainAckVersionOf(latest))
}

func operatorParkVersionsPreserveDrainStopIdentity(expectedVersion, latestVersion drainAckLifecycleVersion) bool {
	if latestVersion.closed ||
		strings.TrimSpace(latestVersion.state) != string(sessions.StateSuspended) ||
		strings.TrimSpace(latestVersion.sleepIntent) != string(sessions.SleepReasonUserHold) ||
		strings.TrimSpace(latestVersion.wakeRequest) == string(sessions.WakeCauseExplicit) {
		return false
	}
	latestVersion.state = expectedVersion.state
	latestVersion.sleepReason = expectedVersion.sleepReason
	latestVersion.sleepIntent = expectedVersion.sleepIntent
	return latestVersion == expectedVersion
}

// verifyLiveDrainRuntimeIdentity proves a name-scoped provider target belongs
// to the exact durable session before a destructive stop. ID and token are
// mandatory. Runtime and continuation epochs are compared whenever the
// provider exposes them; the durable awake epoch remains protected by the
// lifecycle-lock re-read because it is not exported to runtimes.
func verifyLiveDrainRuntimeIdentity(sp runtime.Provider, name string, expected sessions.Info) error {
	if err := verifyLiveDrainRuntimeNameOwnership(sp, name, expected); err != nil {
		return err
	}
	for _, epoch := range []struct {
		key      string
		expected string
	}{
		{key: "GC_RUNTIME_EPOCH", expected: expected.Generation},
		{key: "GC_CONTINUATION_EPOCH", expected: expected.ContinuationEpoch},
	} {
		live, epochErr := sp.GetMeta(name, epoch.key)
		live = strings.TrimSpace(live)
		want := strings.TrimSpace(epoch.expected)
		if epochErr == nil && live != "" && want != "" && live != want {
			return fmt.Errorf("%w for session %s: %s=%s", errTokenMismatch, expected.ID, epoch.key, live)
		}
	}
	return nil
}

// verifyLiveDrainRuntimeNameOwnership is the non-destructive metadata-mutation
// fence. Clearing an obsolete ack requires positive ID+token ownership, while a
// deliberately stale runtime epoch is itself one of the reasons the ack needs
// clearing. Destructive stops use verifyLiveDrainRuntimeIdentity above and also
// require every available runtime epoch to match.
func verifyLiveDrainRuntimeNameOwnership(sp runtime.Provider, name string, expected sessions.Info) error {
	if sp == nil || strings.TrimSpace(name) == "" ||
		strings.TrimSpace(expected.ID) == "" || strings.TrimSpace(expected.InstanceToken) == "" {
		return fmt.Errorf("%w for session %s", errRuntimeIdentityUnavailable, expected.ID)
	}
	liveID, idErr := sp.GetMeta(name, "GC_SESSION_ID")
	liveID = strings.TrimSpace(liveID)
	if idErr != nil || liveID == "" {
		return fmt.Errorf("%w for session %s: missing GC_SESSION_ID", errRuntimeIdentityUnavailable, expected.ID)
	}
	if liveID != strings.TrimSpace(expected.ID) {
		return fmt.Errorf("%w for session %s: GC_SESSION_ID=%s", errTokenMismatch, expected.ID, liveID)
	}
	liveToken, tokenErr := sp.GetMeta(name, "GC_INSTANCE_TOKEN")
	liveToken = strings.TrimSpace(liveToken)
	if tokenErr != nil || liveToken == "" {
		return fmt.Errorf("%w for session %s: missing GC_INSTANCE_TOKEN", errRuntimeIdentityUnavailable, expected.ID)
	}
	if liveToken != strings.TrimSpace(expected.InstanceToken) {
		return fmt.Errorf("%w for session %s", errTokenMismatch, expected.ID)
	}
	return nil
}

// preWakeCommit persists a new incarnation (generation + token) BEFORE
// starting the process. This is Phase 1 of the two-phase wake protocol.
// Returns the new generation, instance token, and the PreWakePatch batch it
// persisted so the caller can fold it onto its coherent typed snapshot
// (write-returns-Info) instead of re-projecting the bead. It reads the current
// persisted state off the caller's typed Info (session_name, generation,
// continuation epoch, sleep_reason, wake_mode, and the continuation-reset
// signals) — every field a verbatim raw mirror — so no raw bead crosses in.
func preWakeCommit(
	info sessions.Info,
	sessFront *sessions.Store,
	clk clock.Clock,
) (newGen int, token string, fold sessions.MetadataPatch, err error) {
	name := info.SessionNameMetadata
	if !sessions.IsSessionNameSyntaxValid(name) {
		return 0, "", nil, fmt.Errorf("invalid session_name %q", name)
	}

	gen, _ := strconv.Atoi(info.Generation)
	newGen = gen + 1
	token = sessions.NewInstanceToken()
	continuationEpoch, _ := strconv.Atoi(info.ContinuationEpoch)
	if continuationEpoch <= 0 {
		continuationEpoch = sessions.DefaultContinuationEpoch
	}
	if shouldBumpContinuationEpoch(info) {
		continuationEpoch++
	}

	sleepReason := ""
	if info.SleepReason == string(sessions.SleepReasonIdleTimeout) {
		// Preserve the idle-timeout wake override until the replacement
		// session has actually started. Failed starts must retry next tick.
		sleepReason = string(sessions.SleepReasonIdleTimeout)
	}

	freshWake := info.WakeMode == "fresh" || pendingContinuationResetNeedsFreshStart(info)
	batch := sessions.PreWakePatch(sessions.PreWakePatchInput{
		Generation:        newGen,
		InstanceToken:     token,
		ContinuationEpoch: continuationEpoch,
		Now:               clk.Now(),
		SleepReason:       sleepReason,
		FreshWake:         freshWake,
	})
	if writeErr := sessFront.ApplyPatch(info.ID, batch); writeErr != nil {
		return 0, "", nil, fmt.Errorf("pre-wake metadata commit: %w", writeErr)
	}
	traceFreshWakeMetadataReset(name, freshWakeResetPriorValues(info), batch, freshWake)

	return newGen, token, batch, nil
}

// freshWakeResetPriorValues reconstructs the pre-reset values of the fresh-wake
// conversation-reset keys off the typed Info so traceFreshWakeMetadataReset can
// report which durable provider markers a fresh wake cleared without the raw
// bead. The keys mirror sessions.FreshWakeConversationResetKeys().
func freshWakeResetPriorValues(info sessions.Info) map[string]string {
	return map[string]string{
		"session_key":             info.SessionKey,
		"started_config_hash":     info.StartedConfigHash,
		"started_live_hash":       info.StartedLiveHash,
		"live_hash":               info.LiveHash,
		"startup_dialog_verified": info.StartupDialogVerified,
		// Priming markers share the fresh-wake reset (S19 Stage 2), so their prior
		// values come off the verbatim raw Info mirrors — otherwise the trace's
		// before[key] lookup reads "" and the cleared list omits them even though
		// FreshWakeConversationResetKeys() clears them. Written as raw string keys
		// (matching the sibling entries) so this read-only prior-value map is not
		// mistaken for a store write by the compared-key write-site gate.
		"primed_at":            info.PrimedAtMetadata,
		"priming_attempted_at": info.PrimingAttemptedAtMetadata,
		"prompt_hash":          info.PromptHashMetadata,
	}
}

func traceFreshWakeMetadataReset(name string, before map[string]string, batch sessions.MetadataPatch, freshWake bool) {
	if !freshWake || os.Getenv("GC_TMUX_TRACE") != "1" {
		return
	}
	cleared := make([]string, 0, len(sessions.FreshWakeConversationResetKeys()))
	for _, key := range sessions.FreshWakeConversationResetKeys() {
		if strings.TrimSpace(before[key]) == "" || batch[key] != "" {
			continue
		}
		cleared = append(cleared, key)
	}
	if len(cleared) == 0 {
		return
	}
	log.Printf(
		"[WAKE-TRACE] preWakeCommit session=%s wake_mode=fresh cleared_provider_metadata=%s",
		name,
		strings.Join(cleared, ","),
	)
}

func shouldBumpContinuationEpoch(info sessions.Info) bool {
	if info.ContinuationResetPending != "" {
		return true
	}
	return info.WakeMode == "fresh" && info.LastWokeAt != ""
}

func pendingContinuationResetNeedsFreshStart(info sessions.Info) bool {
	switch sessions.State(strings.TrimSpace(info.MetadataState)) {
	case sessions.StateStartPending, sessions.StateCreating:
		return false
	}
	return strings.TrimSpace(info.ContinuationResetPending) != "" &&
		strings.TrimSpace(info.StartedConfigHash) != ""
}

// validateWorkDir ensures the path is safe to use as a working directory.
func validateWorkDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if abs != filepath.Clean(abs) {
		return fmt.Errorf("non-canonical path")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	return nil
}

// beginSessionDrainInfo initiates an async drain. Returns immediately.
// The drainTracker stores in-memory state; advanceSessionDrainsWithSessionsTraced progresses it.
//
// Returns true when this call enqueued a new drain (a state transition) and
// false when a drain was already enqueued for this session (no-op). Callers
// that emit user-visible log lines or convergence events tied to the drain
// MUST gate on the return value — otherwise those emissions fire every
// reconciler tick for the life of a stuck drain.
//
// The interrupt signal (Ctrl-C) is NOT sent immediately. It is deferred to
// the next reconciler tick via advanceSessionDrainsWithSessionsTraced. This gives the drain
// one full tick to be canceled (e.g., if the session was falsely orphaned
// due to a transient store failure) before any signal reaches the process.
// Without this, a single bad tick can interrupt a working agent mid-tool-call.
//
// It reads only session_name, generation, and id — all carried verbatim on Info.
func beginSessionDrainInfo(
	info sessions.Info,
	_ runtime.Provider, // kept for caller compatibility; interrupt deferred to advanceSessionDrainsWithSessionsTraced
	dt *drainTracker,
	reason string,
	clk clock.Clock,
	timeout time.Duration,
) bool {
	return beginSessionDrainInfoWithActionGuard(info, dt, reason, clk, timeout, nil)
}

// beginSessionDrainInfoWithActionGuard is the execution-stalled variant. The
// caller invokes guard around this enqueue, and the tracker retains the same
// guard so every later stop/close boundary re-proves the exact session and work
// incarnations. Keeping the generic entry point above guard-free avoids changing
// any ordinary drain semantics.
func beginSessionDrainInfoWithActionGuard(
	info sessions.Info,
	dt *drainTracker,
	reason string,
	clk clock.Clock,
	timeout time.Duration,
	guard drainActionGuard,
) bool {
	name := info.SessionNameMetadata
	if dt.get(info.ID) != nil {
		if os.Getenv("GC_TMUX_TRACE") == "1" {
			log.Printf("[DRAIN-TRACE] beginSessionDrain session=%s reason=%s noop=already-draining", name, reason)
		}
		return false
	}
	gen, _ := strconv.Atoi(info.Generation)

	dt.set(info.ID, &drainState{
		startedAt:       clk.Now(),
		deadline:        clk.Now().Add(timeout),
		reason:          reason,
		generation:      gen,
		lifecycle:       drainAckVersionOf(info),
		lifecyclePinned: true,
		actionGuard:     guard,
	})

	if os.Getenv("GC_TMUX_TRACE") == "1" {
		log.Printf("[DRAIN-TRACE] beginSessionDrain session=%s reason=%s", name, reason)
	}
	telemetry.RecordDrainTransition(context.Background(), name, reason, "begin")
	return true
}

// executionStalledDrainReason drains a pool seat that claimed work and then
// never executed it, after the execution backstop spent its bounded nudges
// (execution_backstop.go).
//
// It is NOT cancelable, by any of the three cancel lenses, and that is the whole
// point of having its own reason. The session it drains is — by construction —
// alive, awake, and holding an in_progress claim, which is exactly the shape
// every keep-alive guard is built to protect: the assigned-work cancel would
// cancel it on the same claim that justified it, and the plain cancel would
// cancel it the moment any wake reason reappeared. A cancelable drain here is
// not a drain at all; the session stays wedged holding work no one else can
// take, which is the failure this whole lane exists to end.
//
// Convergence chain once it fires: tracked drain -> one-tick deferral ->
// authority-guarded stop -> session bead closed -> the claim released by the
// dead-assignee reopen lane -> the row is demand again -> a fresh seat claims it.
const executionStalledDrainReason = "execution-stalled"

func drainReasonCancelable(reason string) bool {
	return reason != "config-drift" && reason != "orphaned" && reason != "suspended" &&
		reason != executionStalledDrainReason
}

func pendingDrainReasonCancelable(reason string) bool {
	return reason != "orphaned" && reason != "suspended" && reason != executionStalledDrainReason
}

const (
	reconcilerDrainAckSourceKey     = runtime.DrainAckSourceMetadataKey
	reconcilerDrainAckSourceValue   = "reconciler"
	drainAckSourceAgentValue        = "agent"
	reconcilerDrainAckReasonKey     = runtime.DrainAckReasonMetadataKey
	reconcilerDrainAckGenerationKey = runtime.DrainAckGenerationMetadataKey
	reconcilerDrainAckAwakeEpochKey = runtime.DrainAckAwakeEpochMetadataKey
)

func setReconcilerDrainAckMetadata(sp runtime.Provider, name string, ds *drainState) error {
	if ds == nil {
		return nil
	}
	if err := sp.SetMeta(name, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue); err != nil {
		return err
	}
	if err := sp.SetMeta(name, reconcilerDrainAckReasonKey, ds.reason); err != nil {
		_ = clearReconcilerDrainAckMetadata(sp, name)
		return err
	}
	if err := sp.SetMeta(name, reconcilerDrainAckGenerationKey, strconv.Itoa(ds.generation)); err != nil {
		_ = clearReconcilerDrainAckMetadata(sp, name)
		return err
	}
	// A generation is not a complete runtime incarnation discriminator:
	// Manager.Start can preserve both generation and instance_token. Pin the
	// acknowledgement to the durable awake interval as well, so an ack left by
	// the preceding same-token interval cannot stop its replacement.
	if err := sp.SetMeta(name, reconcilerDrainAckAwakeEpochKey, strings.TrimSpace(ds.lifecycle.awakeStartedAt)); err != nil {
		_ = clearReconcilerDrainAckMetadata(sp, name)
		return err
	}
	if err := sp.SetMeta(name, "GC_DRAIN_ACK", "1"); err != nil {
		_ = clearReconcilerDrainAckMetadata(sp, name)
		return err
	}
	return nil
}

func clearReconcilerDrainAckMetadata(sp runtime.Provider, name string) error {
	if sp == nil {
		return fmt.Errorf("session provider is nil")
	}
	var errs []error
	for _, key := range []string{runtime.DrainAckMetadataKey, reconcilerDrainAckSourceKey, reconcilerDrainAckReasonKey, reconcilerDrainAckGenerationKey, reconcilerDrainAckAwakeEpochKey} {
		if err := sp.RemoveMeta(name, key); err != nil {
			log.Printf("session wake: clearing reconciler drain ack metadata %s for %s: %v", key, name, err)
			errs = append(errs, fmt.Errorf("removing %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

func trySetReconcilerDrainAckCurrent(
	cityPath string,
	info sessions.Info,
	sessFront *sessions.Store,
	sp runtime.Provider,
	ds *drainState,
) (applied, superseded bool, err error) {
	acquired, superseded, err := tryWithCurrentDrainLifecycle(cityPath, info, sessFront, false, func(latest sessions.Info) error {
		name := strings.TrimSpace(latest.SessionNameMetadata)
		if identityErr := verifyLiveDrainRuntimeIdentity(sp, name, latest); identityErr != nil {
			return identityErr
		}
		return setReconcilerDrainAckMetadata(sp, name, ds)
	})
	if err != nil || !acquired || superseded {
		return false, superseded, err
	}
	return true, false, nil
}

// cancelSessionDrainIfCurrent retires an in-memory drain and, only when the
// provider metadata is positively proven to be that exact reconciler drain,
// clears its GC_DRAIN_* keys under the lifecycle fence. A newer lifecycle or a
// different provider ack is never name-cleared.
func cancelSessionDrainIfCurrent(
	cityPath string,
	info sessions.Info,
	sessFront *sessions.Store,
	sp runtime.Provider,
	dt *drainTracker,
	canCancel func(string) bool,
) bool {
	return cancelSessionDrainIfCurrentCore(cityPath, info, sessFront, sp, dt, canCancel, nil)
}

func cancelSessionDrainAndOpsIfCurrent(
	cityPath string,
	info sessions.Info,
	sessFront *sessions.Store,
	sp runtime.Provider,
	dt *drainTracker,
	dops drainOps,
	canCancel func(string) bool,
) bool {
	if dops == nil {
		return false
	}
	return cancelSessionDrainIfCurrentCore(cityPath, info, sessFront, sp, dt, canCancel, dops.clearDrain)
}

func cancelSessionDrainIfCurrentCore(
	cityPath string,
	info sessions.Info,
	sessFront *sessions.Store,
	sp runtime.Provider,
	dt *drainTracker,
	canCancel func(string) bool,
	clearDrain func(string) error,
) bool {
	if dt == nil || sp == nil {
		return false
	}
	ds := dt.get(info.ID)
	if ds == nil || !canCancel(ds.reason) {
		return false
	}
	gen, _ := strconv.Atoi(info.Generation)
	if gen != ds.generation {
		return false
	}
	if !ds.ackSet && clearDrain == nil {
		// No provider or durable state is touched: this is only retirement of a
		// process-local intent after the caller's coherent snapshot restored a
		// wake reason. It remains safe for store-less unit/unmanaged callers and
		// does not need to contend on the lifecycle lock.
		if ds.lifecyclePinned && drainAckVersionOf(info) != ds.lifecycle {
			dt.clearIdleProbe(info.ID)
			dt.remove(info.ID)
			return false
		}
		dt.clearIdleProbe(info.ID)
		dt.remove(info.ID)
		telemetry.RecordDrainTransition(context.Background(), info.SessionNameMetadata, ds.reason, "cancel")
		return true
	}
	ackCleared := false
	acquired, superseded, err := tryWithCurrentDrainLifecycle(cityPath, info, sessFront, false, func(latest sessions.Info) error {
		name := strings.TrimSpace(latest.SessionNameMetadata)
		if clearDrain != nil {
			// The reconciler has independently established that current work should
			// cancel whichever drain signal owns this exact live lifecycle. Clear
			// both provider provenance and drainOps state while the same lifecycle
			// lock is held; never follow a fenced clear with a racy name-only clear.
			if identityErr := verifyLiveDrainRuntimeNameOwnership(sp, name, latest); identityErr != nil {
				return identityErr
			}
			if clearErr := clearReconcilerDrainAckMetadata(sp, name); clearErr != nil {
				return clearErr
			}
			if clearErr := clearDrain(name); clearErr != nil {
				return clearErr
			}
			ackCleared = true
			return nil
		}
		source, sourceErr := sp.GetMeta(name, reconcilerDrainAckSourceKey)
		if sourceErr != nil {
			return sourceErr
		}
		if source != reconcilerDrainAckSourceValue {
			return nil
		}
		reason, reasonErr := sp.GetMeta(name, reconcilerDrainAckReasonKey)
		if reasonErr != nil {
			return reasonErr
		}
		generation, generationErr := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
		if generationErr != nil {
			return generationErr
		}
		awakeEpoch, awakeEpochErr := sp.GetMeta(name, reconcilerDrainAckAwakeEpochKey)
		if awakeEpochErr != nil {
			return awakeEpochErr
		}
		if reason != ds.reason ||
			strings.TrimSpace(generation) != strconv.Itoa(ds.generation) ||
			strings.TrimSpace(awakeEpoch) != strings.TrimSpace(ds.lifecycle.awakeStartedAt) {
			// A different reconciler drain now owns the provider metadata.
			return nil
		}
		if identityErr := verifyLiveDrainRuntimeNameOwnership(sp, name, latest); identityErr != nil {
			return identityErr
		}
		if clearErr := clearReconcilerDrainAckMetadata(sp, name); clearErr != nil {
			return clearErr
		}
		ackCleared = true
		return nil
	})
	if err != nil || !acquired {
		return false
	}
	// Supersession retires only local state and returns false so callers never
	// follow it with a name-scoped drain-ops clear. A mismatched provider ack is
	// left in place and keeps the tracker retryable.
	if superseded {
		dt.clearIdleProbe(info.ID)
		dt.remove(info.ID)
		return false
	}
	if !ackCleared {
		return false
	}
	dt.clearIdleProbe(info.ID)
	dt.remove(info.ID)
	telemetry.RecordDrainTransition(context.Background(), info.SessionNameMetadata, ds.reason, "cancel")
	return true
}

type reconcilerDrainAckMetadata struct {
	source     string
	reason     string
	generation string
	awakeEpoch string
	acked      string
}

func readReconcilerDrainAckMetadata(sp runtime.Provider, name string) (reconcilerDrainAckMetadata, error) {
	if sp == nil {
		return reconcilerDrainAckMetadata{}, fmt.Errorf("session provider is nil")
	}
	var metadata reconcilerDrainAckMetadata
	var err error
	if metadata.source, err = sp.GetMeta(name, reconcilerDrainAckSourceKey); err != nil {
		return reconcilerDrainAckMetadata{}, err
	}
	if metadata.reason, err = sp.GetMeta(name, reconcilerDrainAckReasonKey); err != nil {
		return reconcilerDrainAckMetadata{}, err
	}
	if metadata.generation, err = sp.GetMeta(name, reconcilerDrainAckGenerationKey); err != nil {
		return reconcilerDrainAckMetadata{}, err
	}
	if metadata.awakeEpoch, err = sp.GetMeta(name, reconcilerDrainAckAwakeEpochKey); err != nil {
		return reconcilerDrainAckMetadata{}, err
	}
	if metadata.acked, err = sp.GetMeta(name, "GC_DRAIN_ACK"); err != nil {
		return reconcilerDrainAckMetadata{}, err
	}
	return metadata, nil
}

// tryClearReconcilerDrainAckCurrent is the reconciler path for clearing a
// recovered/stale acknowledgement without an in-memory drain owner. It
// serializes with start/suspend/finalize, re-reads the complete lifecycle epoch,
// positively proves the live name belongs to that durable session, and checks
// the exact provider metadata again while holding the fence.
func tryClearReconcilerDrainAckCurrent(
	cityPath string,
	info sessions.Info,
	sessFront *sessions.Store,
	sp runtime.Provider,
	shouldClear func(sessions.Info, reconcilerDrainAckMetadata) bool,
	afterClear func(string) error,
) (bool, error) {
	cleared := false
	acquired, superseded, err := tryWithCurrentDrainLifecycle(cityPath, info, sessFront, false, func(latest sessions.Info) error {
		name := strings.TrimSpace(latest.SessionNameMetadata)
		metadata, metadataErr := readReconcilerDrainAckMetadata(sp, name)
		if metadataErr != nil {
			return metadataErr
		}
		if !shouldClear(latest, metadata) {
			return nil
		}
		if identityErr := verifyLiveDrainRuntimeNameOwnership(sp, name, latest); identityErr != nil {
			return identityErr
		}
		if clearErr := clearReconcilerDrainAckMetadata(sp, name); clearErr != nil {
			return clearErr
		}
		if afterClear != nil {
			if clearErr := afterClear(name); clearErr != nil {
				return clearErr
			}
		}
		cleared = true
		return nil
	})
	if err != nil || !acquired || superseded {
		return false, err
	}
	return cleared, nil
}

func reconcilerDrainAckMetadataMatchesInfo(info sessions.Info, metadata reconcilerDrainAckMetadata) bool {
	return strings.TrimSpace(metadata.source) == reconcilerDrainAckSourceValue &&
		strings.TrimSpace(metadata.reason) != "" &&
		strings.TrimSpace(metadata.generation) == strings.TrimSpace(info.Generation) &&
		strings.TrimSpace(metadata.awakeEpoch) == strings.TrimSpace(info.AwakeStartedAt)
}

func clearRecoveredReconcilerDrainAckCurrent(
	cityPath string,
	info sessions.Info,
	sessFront *sessions.Store,
	sp runtime.Provider,
	canCancel func(string) bool,
) bool {
	cleared, err := tryClearReconcilerDrainAckCurrent(cityPath, info, sessFront, sp, func(latest sessions.Info, metadata reconcilerDrainAckMetadata) bool {
		return reconcilerDrainAckMetadataMatchesInfo(latest, metadata) && canCancel(metadata.reason)
	}, nil)
	if err != nil || !cleared {
		return false
	}
	telemetry.RecordDrainTransition(context.Background(), info.SessionNameMetadata, "recovered", "cancel")
	return true
}

func clearRecoveredReconcilerDrainAckAndOpsCurrent(
	cityPath string,
	info sessions.Info,
	sessFront *sessions.Store,
	sp runtime.Provider,
	dops drainOps,
	canCancel func(string) bool,
) bool {
	if dops == nil {
		return false
	}
	cleared, err := tryClearReconcilerDrainAckCurrent(cityPath, info, sessFront, sp, func(latest sessions.Info, metadata reconcilerDrainAckMetadata) bool {
		return reconcilerDrainAckMetadataMatchesInfo(latest, metadata) && canCancel(metadata.reason)
	}, dops.clearDrain)
	return err == nil && cleared
}

func clearStaleOrLegacyReconcilerDrainAckCurrent(
	cityPath string,
	info sessions.Info,
	sessFront *sessions.Store,
	sp runtime.Provider,
	allowLegacy bool,
) bool {
	cleared, err := tryClearReconcilerDrainAckCurrent(cityPath, info, sessFront, sp, func(latest sessions.Info, metadata reconcilerDrainAckMetadata) bool {
		if strings.TrimSpace(metadata.acked) != "1" {
			return false
		}
		if strings.TrimSpace(metadata.source) == "" {
			return allowLegacy
		}
		if strings.TrimSpace(metadata.source) != reconcilerDrainAckSourceValue {
			return false
		}
		return !reconcilerDrainAckMetadataMatchesInfo(latest, metadata)
	}, nil)
	return err == nil && cleared
}

// cancelSessionDrainInfo removes a cancelable drain if wake reasons reappeared
// for the same generation. If GC_DRAIN_ACK was already set by the reconciler
// (deferred drain signal), it is cleared so the Phase 1 drain-ack check doesn't
// kill the session. It reads the session id/generation/name off the Info snapshot.
func cancelSessionDrainInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker) bool {
	return cancelSessionDrainIfInfo(info, sp, dt, drainReasonCancelable)
}

// cancelSessionDrainForPendingInfo is the snapshot-only form used by focused
// convergence checks. Production reconciliation additionally revalidates the
// current durable session through cancelSessionDrainIfCurrent before mutating
// runtime drain metadata.
func cancelSessionDrainForPendingInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker) bool {
	return cancelSessionDrainIfInfo(info, sp, dt, pendingDrainReasonCancelable)
}

// cancelSessionDrainForAssignedWorkInfo is the snapshot-only form used by
// focused convergence checks. Production reconciliation uses the current-row
// lifecycle fence before clearing an assigned-work-cancelable drain.
func cancelSessionDrainForAssignedWorkInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker) bool {
	return cancelSessionDrainIfInfo(info, sp, dt, assignedWorkDrainReasonCancelable)
}

func assignedWorkDrainReasonCancelable(reason string) bool {
	switch reason {
	case "orphaned", "no-wake-reason":
		return true
	default:
		return false
	}
}

// cancelSessionConfigDriftDrainInfo cancels a config-drift drain off the Info
// snapshot, threading Info straight into the typed drain-cancel core.
func cancelSessionConfigDriftDrainInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker) bool {
	if dt == nil {
		return false
	}
	return cancelSessionDrainIfInfo(info, sp, dt, func(reason string) bool {
		return reason == "config-drift"
	})
}

// cancelSessionDrainIfInfo is the typed core of the drain-cancel helpers. It
// reads only the session id, generation, and session_name — all carried raw and
// verbatim on Info — so it is byte-identical to the raw-bead form it backs.
func cancelSessionDrainIfInfo(info sessions.Info, sp runtime.Provider, dt *drainTracker, canCancel func(string) bool) bool {
	ds := dt.get(info.ID)
	if ds == nil {
		return false
	}
	if !canCancel(ds.reason) {
		return false
	}
	gen, _ := strconv.Atoi(info.Generation)
	if gen == ds.generation {
		dt.clearIdleProbe(info.ID)
		dt.remove(info.ID)
		name := info.SessionNameMetadata
		// Clear GC_DRAIN_ACK if it was set — prevents stale ack from
		// killing the session on the next Phase 1 drain-ack check.
		if ds.ackSet {
			_ = clearReconcilerDrainAckMetadata(sp, name)
		}
		telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "cancel")
		return true
	}
	return false
}

func reconcilerDrainAckMatchesSession(session beads.Bead, sp runtime.Provider, name string) (string, bool) {
	if sp == nil || name == "" {
		return "", false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil || source != reconcilerDrainAckSourceValue {
		return "", false
	}
	reason, err := sp.GetMeta(name, reconcilerDrainAckReasonKey)
	if err != nil || reason == "" {
		return "", false
	}
	expectedGeneration, err := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
	if err != nil || expectedGeneration == "" {
		return "", false
	}
	currentGeneration := strings.TrimSpace(session.Metadata["generation"])
	if currentGeneration == "" || currentGeneration != expectedGeneration {
		return "", false
	}
	expectedAwakeEpoch, err := sp.GetMeta(name, reconcilerDrainAckAwakeEpochKey)
	if err != nil || strings.TrimSpace(expectedAwakeEpoch) != strings.TrimSpace(session.Metadata["awake_started_at"]) {
		return "", false
	}
	return reason, true
}

// reconcilerDrainAckMatchesSessionInfo is the session.Info sibling of
// reconcilerDrainAckMatchesSession for the reconciler forward pass. The only
// session-bead read is the generation (Info.Generation); everything else is
// provider metadata (sp) and the caller-supplied name, shared verbatim with the
// raw form — so it is byte-identical, pinned by the sessionGeneration oracle row.
func reconcilerDrainAckMatchesSessionInfo(info sessions.Info, sp runtime.Provider, name string) (string, bool) {
	if sp == nil || name == "" {
		return "", false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil || source != reconcilerDrainAckSourceValue {
		return "", false
	}
	reason, err := sp.GetMeta(name, reconcilerDrainAckReasonKey)
	if err != nil || reason == "" {
		return "", false
	}
	expectedGeneration, err := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
	if err != nil || expectedGeneration == "" {
		return "", false
	}
	currentGeneration := strings.TrimSpace(info.Generation)
	if currentGeneration == "" || currentGeneration != expectedGeneration {
		return "", false
	}
	expectedAwakeEpoch, err := sp.GetMeta(name, reconcilerDrainAckAwakeEpochKey)
	if err != nil || strings.TrimSpace(expectedAwakeEpoch) != strings.TrimSpace(info.AwakeStartedAt) {
		return "", false
	}
	return reason, true
}

func staleReconcilerDrainAck(session beads.Bead, sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil || source != reconcilerDrainAckSourceValue {
		return false
	}
	expectedGeneration, err := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
	if err != nil || expectedGeneration == "" {
		return true
	}
	currentGeneration := strings.TrimSpace(session.Metadata["generation"])
	if currentGeneration == "" || currentGeneration != expectedGeneration {
		return true
	}
	expectedAwakeEpoch, err := sp.GetMeta(name, reconcilerDrainAckAwakeEpochKey)
	return err != nil || strings.TrimSpace(expectedAwakeEpoch) != strings.TrimSpace(session.Metadata["awake_started_at"])
}

// staleReconcilerDrainAckInfo is the session.Info sibling of
// staleReconcilerDrainAck: the only session-bead read is the generation
// (Info.Generation), matching the raw form byte-for-byte (sessionGeneration
// oracle row).
func staleReconcilerDrainAckInfo(info sessions.Info, sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err != nil || source != reconcilerDrainAckSourceValue {
		return false
	}
	expectedGeneration, err := sp.GetMeta(name, reconcilerDrainAckGenerationKey)
	if err != nil || expectedGeneration == "" {
		return true
	}
	currentGeneration := strings.TrimSpace(info.Generation)
	if currentGeneration == "" || currentGeneration != expectedGeneration {
		return true
	}
	expectedAwakeEpoch, err := sp.GetMeta(name, reconcilerDrainAckAwakeEpochKey)
	return err != nil || strings.TrimSpace(expectedAwakeEpoch) != strings.TrimSpace(info.AwakeStartedAt)
}

func staleOrLegacyDrainAckBeforeStart(session beads.Bead, sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err == nil && source == drainAckSourceAgentValue {
		return false
	}
	if err == nil && source == reconcilerDrainAckSourceValue {
		return staleReconcilerDrainAck(session, sp, name)
	}
	acked, err := sp.GetMeta(name, "GC_DRAIN_ACK")
	return err == nil && acked == "1"
}

// staleOrLegacyDrainAckBeforeStartInfo is the session.Info sibling of
// staleOrLegacyDrainAckBeforeStart: it defers to staleReconcilerDrainAckInfo for
// the reconciler-owned branch (the only session-bead read, Info.Generation) and
// otherwise reads provider metadata only, so it is byte-identical to the raw form.
func staleOrLegacyDrainAckBeforeStartInfo(info sessions.Info, sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	source, err := sp.GetMeta(name, reconcilerDrainAckSourceKey)
	if err == nil && source == drainAckSourceAgentValue {
		return false
	}
	if err == nil && source == reconcilerDrainAckSourceValue {
		return staleReconcilerDrainAckInfo(info, sp, name)
	}
	acked, err := sp.GetMeta(name, "GC_DRAIN_ACK")
	return err == nil && acked == "1"
}

func advanceSessionDrainsWithSessionsTraced(
	cityPath string,
	dt *drainTracker,
	sp runtime.Provider,
	store beads.Store,
	infoLookup func(id string) (sessions.Info, bool),
	wakeEvals map[string]wakeEvaluation,
	cfg *config.City,
	clk clock.Clock,
	trace *sessionReconcilerTraceCycle,
) {
	// wakeEvals is required. The reconciler builds it from the coherent infoByID
	// snapshot via ComputeAwakeSet -> awakeSetToWakeEvals; tests supply explicit
	// wakeEvals encoding the premise they exercise. Step 5d dropped the raw-bead
	// wakeEvals==nil fallback and its now-unused sessionBeads/poolDesired/workSet/
	// readyWaitSet inputs from this prod core — the scan runs entirely off infoLookup.
	// Session front door constructed once from the same store; nil when store is
	// nil so completeDrain keeps its store==nil short-circuit.
	sessFront := sessionFrontDoor(store)
	if store == nil {
		sessFront = nil
	}
	for id, ds := range dt.all() {
		info, ok := infoLookup(id)
		if !ok {
			dt.clearIdleProbe(id)
			dt.remove(id)
			continue
		}
		// The whole scan runs off the typed Info: decision reads (session_name,
		// generation, template), the drain-complete write (completeDrain → store),
		// the cancel checks (cancelSessionDrainFor*Info), verifiedStop, and the
		// process-running probe (by info.ID). Nothing reads the raw bead.
		name := info.SessionNameMetadata
		currentLifecycle := drainAckVersionOf(info)
		if !ds.lifecyclePinned {
			// Compatibility for recovered/tests-created in-memory drains. Every
			// production beginSessionDrainInfo pins this at enqueue time.
			ds.lifecycle = currentLifecycle
			ds.lifecyclePinned = true
		}
		operatorParkedAfterBegin := operatorParkVersionsPreserveDrainStopIdentity(ds.lifecycle, currentLifecycle)
		if currentLifecycle != ds.lifecycle && !operatorParkedAfterBegin {
			// A newer lifecycle owns both the durable row and any provider
			// metadata under the reused name. Retire only the in-memory drain;
			// never clear name-scoped GC_DRAIN_* from a stale snapshot.
			dt.clearIdleProbe(id)
			dt.remove(id)
			if trace != nil {
				trace.RecordDecision(TraceSiteDrainStale, TraceReasonStaleGeneration, TraceOutcomeCancel, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
					"drain_reason": ds.reason,
					"superseded":   "lifecycle-epoch",
				})
			}
			continue
		}

		// Stale check: if session was re-woken (generation changed), cancel drain.
		gen, _ := strconv.Atoi(info.Generation)
		if gen != ds.generation {
			dt.clearIdleProbe(id)
			dt.remove(id)
			if trace != nil {
				trace.RecordDecision(TraceSiteDrainStale, TraceReasonStaleGeneration, TraceOutcomeCancel, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
					"drain_reason":       ds.reason,
					"drain_generation":   ds.generation,
					"session_generation": gen,
				})
			}
			continue
		}

		// Check if process exited.
		var running bool
		var runtimeProbeErr error
		if store == nil {
			// Unmanaged/store-less callers cannot resolve a worker handle by durable
			// ID, but they can still safely cancel an in-memory, unacknowledged drain
			// from their coherent name snapshot. Production always supplies a store.
			running = sp != nil && sp.IsRunning(name)
		} else {
			running, runtimeProbeErr = workerSessionTargetRunningWithConfig("", store, sp, cfg, info.ID)
			if runtimeProbeErr != nil {
				running = false
			}
		}

		// Execution-stalled drains carry a live authority guard instead of
		// entering the generic GC_DRAIN_ACK path. The general ack is consumed on
		// the next reconciler tick, after the tracker (and therefore its exact work
		// validator) may have been cleared; that gap would let a completed or
		// reassigned claim trigger a non-cancelable async stop. This rare lane
		// performs the already-deferred stop directly, with the retained guard
		// wrapped around both the stop and the terminal metadata write. A positive
		// mismatch retires the tracker; an unavailable live read holds for retry.
		if ds.reason == executionStalledDrainReason {
			// This reason is non-cancelable only because its retained guard proves
			// one exact session+work incarnation at every action boundary. A
			// malformed or partially reconstructed tracker without that authority
			// must never fall through to the generic drain-ack/timeout path.
			if ds.actionGuard == nil {
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainTimeout, TraceReasonCode(ds.reason), TraceOutcomeRetry, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
						"authority_guard": false,
						"error":           "execution-stalled authority guard unavailable",
					})
				}
				continue
			}
			retire := func() {
				dt.clearIdleProbe(id)
				dt.remove(id)
			}
			completeAuthorized := func() {
				resolution, guardErr := ds.actionGuard(func(latest sessions.Info) error {
					return closeExecutionStalledSessionChecked(latest, sessFront, clk)
				})
				switch resolution {
				case backstopResolutionClear:
					retire()
				case backstopResolutionHold:
				case backstopResolutionOutstanding:
					if guardErr != nil {
						return
					}
					retire()
					telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "complete")
					if trace != nil {
						trace.RecordDecision(TraceSiteDrainComplete, TraceReasonCode(ds.reason), TraceOutcomeComplete, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
							"drain_started_at": ds.startedAt,
							"authority_guard":  true,
						})
					}
				}
			}
			// An ambiguous observation is not proof that the runtime exited. Keep
			// the exact authority tracker and retry instead of writing terminal
			// metadata for a process that may still be alive.
			if runtimeProbeErr != nil {
				continue
			}
			if !running {
				completeAuthorized()
				continue
			}

			// Prove the live runtime token before entering the final action guard.
			// Provider metadata may itself be remote I/O; doing it first ensures the
			// guard's authoritative session+work reads are the last observations
			// before the direct destructive Stop call.
			if tokenErr := verifyLiveDrainRuntimeIdentity(sp, name, info); tokenErr != nil {
				if errors.Is(tokenErr, errTokenMismatch) {
					retire()
				}
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainTimeout, TraceReasonCode(ds.reason), TraceOutcomeRetry, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
						"authority_guard": true,
						"error":           tokenErr.Error(),
					})
				}
				continue
			}
			resolution, stopErr := ds.actionGuard(func(latest sessions.Info) error {
				// Attachment is provider state, not part of the durable lifecycle
				// fingerprint. Re-read it inside the final lifecycle-locked action
				// boundary so a user attachment suppresses the destructive stop.
				if sp.IsAttached(name) {
					return errors.New("execution-stalled session is attached")
				}
				return stopExecutionStalledRuntime(latest, sp)
			})
			switch resolution {
			case backstopResolutionClear:
				retire()
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainStale, TraceReasonCode(ds.reason), TraceOutcomeCancel, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
						"authority_guard": true,
					})
				}
				continue
			case backstopResolutionHold:
				continue
			case backstopResolutionOutstanding:
				if stopErr != nil {
					if errors.Is(stopErr, errTokenMismatch) {
						retire()
					}
					if trace != nil {
						trace.RecordDecision(TraceSiteDrainTimeout, TraceReasonCode(ds.reason), TraceOutcomeRetry, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
							"authority_guard": true,
							"error":           stopErr.Error(),
						})
					}
					continue
				}
			default:
				continue
			}

			running, runtimeProbeErr = workerSessionTargetRunningWithConfig("", store, sp, cfg, info.ID)
			if runtimeProbeErr != nil {
				// The stop may have succeeded, but terminal metadata and tracker
				// retirement require a positive observation that it is no longer
				// running. Retry the probe on a later tick.
				continue
			}
			if !running {
				completeAuthorized()
			}
			continue
		}
		if !running {
			if operatorParkedAfterBegin {
				// Suspend already owns the durable terminal state. The runtime is
				// gone, so retire the old drain without applying CompleteDrainPatch.
				dt.clearIdleProbe(id)
				dt.remove(id)
				continue
			}
			// Process exited — drain complete.
			outcome, completeErr := completeDrain(cityPath, info, sessFront, ds, clk)
			if completeErr != nil || outcome == drainCompletionDeferred {
				// A lifecycle owner or transient store failure won this tick. Keep
				// the drain so a later pass can re-observe and retry safely.
				continue
			}
			dt.clearIdleProbe(id)
			dt.remove(id)
			if outcome == drainCompletionSuperseded {
				continue
			}
			telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "complete")
			if trace != nil {
				trace.RecordDecision(TraceSiteDrainComplete, TraceReasonCode(ds.reason), TraceOutcomeComplete, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
					"drain_started_at": ds.startedAt,
				})
			}
			continue
		}

		if eval, ok := wakeEvals[info.ID]; ok &&
			containsWakeReason(eval.Reasons, WakePending) &&
			pendingDrainReasonCancelable(ds.reason) {
			if cancelSessionDrainIfCurrent(cityPath, info, sessFront, sp, dt, pendingDrainReasonCancelable) {
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ds.reason), TraceOutcomeCancelPending, normalizedSessionTemplateInfo(info, cfg), name, nil)
				}
				continue
			}
		}

		if eval, ok := wakeEvals[info.ID]; ok &&
			eval.Reason == "assigned-work" &&
			containsWakeReason(eval.Reasons, WakeWork) &&
			assignedWorkDrainReasonCancelable(ds.reason) {
			if cancelSessionDrainIfCurrent(cityPath, info, sessFront, sp, dt, assignedWorkDrainReasonCancelable) {
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ds.reason), TraceOutcomeCancelAssignedWork, normalizedSessionTemplateInfo(info, cfg), name, nil)
				}
				continue
			}
		}

		// Cancellation check: if wake reasons reappeared, cancel the in-memory
		// drain. Orphaned, suspended, and ordinary config-drift drains are not
		// canceled here.
		if drainReasonCancelable(ds.reason) {
			if eval, ok := wakeEvals[info.ID]; ok && len(eval.Reasons) > 0 {
				if !cancelSessionDrainIfCurrent(cityPath, info, sessFront, sp, dt, drainReasonCancelable) {
					continue
				}
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainCancel, TraceReasonCode(ds.reason), TraceOutcomeCancel, normalizedSessionTemplateInfo(info, cfg), name, nil)
				}
				continue
			}
		}

		// Deferred drain signal: set GC_DRAIN_ACK after the drain has survived
		// at least one full tick without being canceled. This prevents a
		// single transient store failure from interrupting a working agent
		// — the false-orphan drain is canceled on the next tick when the
		// store recovers, before any signal is set.
		//
		// Uses the same GC_DRAIN_ACK env var that agents set via
		// `gc runtime drain-ack`. The reconciler's Phase 1 drain-ack check
		// sees it on the next tick and calls sp.Stop() for a clean
		// SIGTERM/SIGKILL — no Ctrl-C keystroke injection into the pane.
		if !ds.ackSet {
			if os.Getenv("GC_TMUX_TRACE") == "1" {
				log.Printf("[DRAIN-TRACE] advanceSessionDrainsWithSessionsTraced: setting GC_DRAIN_ACK session=%s reason=%s", name, ds.reason)
			}
			applied, superseded, err := trySetReconcilerDrainAckCurrent(cityPath, info, sessFront, sp, ds)
			if superseded {
				dt.clearIdleProbe(id)
				dt.remove(id)
				continue
			}
			if applied {
				ds.ackSet = true
				ds.followUp = true
			}
			if trace != nil {
				outcome := TraceOutcomeSuccess
				fields := traceRecordPayload{
					"reason":          ds.reason,
					"deferred_signal": true,
				}
				if !applied {
					outcome = TraceOutcomeDeferredBusy
				}
				if err != nil {
					outcome = TraceOutcomeFailed
					fields["error"] = err.Error()
				}
				fields["template"] = normalizedSessionTemplateInfo(info, cfg)
				fields["before"] = ""
				fields["after"] = "1"
				fields["field"] = "GC_DRAIN_ACK"
				trace.RecordMutation(TraceSiteMutationRuntimeMeta, TraceReasonUnknown, outcome, "provider_meta", name, "GC_DRAIN_ACK", fields)
			}
		}

		// Pending-interaction guards and wake-based cancellation run before this
		// timeout path. Preserve that ordering if this block is refactored.
		if clk.Now().After(ds.deadline) {
			// Drain timed out — force stop.
			if err := verifiedStop(cityPath, info, store, sp, cfg); err != nil {
				if errors.Is(err, errTokenMismatch) || errors.Is(err, errDrainLifecycleSuperseded) {
					// Session was re-woken or otherwise advanced to a newer
					// lifecycle (including a same-token awake interval).
					// This drain is stale — cancel it.
					dt.clearIdleProbe(id)
					dt.remove(id)
				}
				// Other errors (transient stop failure): keep drain
				// active for retry on next tick.
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainTimeout, TraceReasonCode(ds.reason), TraceOutcomeRetry, normalizedSessionTemplateInfo(info, cfg), name, traceRecordPayload{
						"error": err.Error(),
					})
				}
				continue
			}
			// Re-probe after stop to confirm process actually exited
			// before marking metadata as asleep.
			running, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, info.ID)
			if err != nil {
				running = false
			}
			if !running {
				if operatorParkedAfterBegin {
					dt.clearIdleProbe(id)
					dt.remove(id)
					continue
				}
				outcome, completeErr := completeDrain(cityPath, info, sessFront, ds, clk)
				if completeErr != nil || outcome == drainCompletionDeferred {
					continue
				}
				dt.clearIdleProbe(id)
				dt.remove(id)
				if outcome == drainCompletionSuperseded {
					continue
				}
				telemetry.RecordDrainTransition(context.Background(), name, ds.reason, "timeout")
				if trace != nil {
					trace.RecordDecision(TraceSiteDrainTimeout, TraceReasonCode(ds.reason), TraceOutcomeComplete, normalizedSessionTemplateInfo(info, cfg), name, nil)
				}
			}
			// If still running after stop, keep drain for next tick.
		}
		// Else: still draining, check again next tick.
	}
}

// completeDrain writes drain-complete metadata to the store for the drained
// session. It reads only the typed Info (id + raw wake_mode); the raw-bead
// mirror the reconciler used to keep is dropped. Nothing reads a drained
// session's metadata later in the tick — the awake scan runs before
// advanceSessionDrainsWithSessionsTraced, and completeDrain is always followed by dt.remove +
// continue — so the store write is the sole observable effect (all completeDrain
// tests assert on store.Get). With no store there is nothing to persist.
func completeDrain(cityPath string, info sessions.Info, sessFront *sessions.Store, ds *drainState, clk clock.Clock) (drainCompletionOutcome, error) {
	if sessFront == nil {
		return drainCompletionDeferred, nil
	}
	acquired, superseded, err := tryWithCurrentDrainLifecycle(cityPath, info, sessFront, false, func(latest sessions.Info) error {
		batch := sessions.CompleteDrainPatch(clk.Now(), ds.reason, latest.WakeMode == "fresh")
		return sessFront.ApplyPatch(latest.ID, batch)
	})
	if err != nil {
		return drainCompletionDeferred, err
	}
	if !acquired {
		return drainCompletionDeferred, nil
	}
	if superseded {
		return drainCompletionSuperseded, nil
	}
	return drainCompletionApplied, nil
}

// closeExecutionStalledSessionChecked is called only while an
// execution-stalled actionGuard holds the exact city lifecycle and work
// authority fence. A stopped session that still owns in-progress work must be
// terminal, not merely asleep: assigned-work demand would immediately wake an
// open/asleep bead and recreate the same wedged claim loop. closeBeadDetailed
// atomically stamps the terminal record and closes the bead, then performs the
// ordinary same-store release cleanup. Split-store work is recovered by the
// next tick's dead-assignee lane once it observes the closed owner.
func closeExecutionStalledSessionChecked(info sessions.Info, sessFront *sessions.Store, clk clock.Clock) error {
	if sessFront == nil {
		return errors.New("session store unavailable while closing execution-stalled drain")
	}
	result := closeExecutionStalledBeadDetailed(sessFront.Store().Store, info.ID, clk.Now().UTC())
	switch result.status {
	case sessionCloseClosed:
		return nil
	case sessionCloseAlreadyClosed:
		authoritative, err := sessFront.GetLive(info.ID)
		if err != nil {
			return fmt.Errorf("witnessing closed execution-stalled session %s: %w", info.ID, err)
		}
		if authoritative.Closed {
			return nil
		}
		return fmt.Errorf("execution-stalled session %s reported already closed but remains open", info.ID)
	case sessionCloseFailed:
		// A sequential backend can persist ClosePatch before Close fails. Adopt a
		// visible terminal witness; otherwise restore every pre-close lifecycle
		// value so this retained tracker can retry from an honest open state.
		authoritative, err := sessFront.GetLive(info.ID)
		if err != nil {
			return fmt.Errorf("resolving failed execution-stalled close for %s: %w", info.ID, err)
		}
		if authoritative.Closed {
			return nil
		}
		if len(result.rollbackPatch) > 0 {
			rollback := make(sessions.MetadataPatch, len(result.rollbackPatch))
			for key, value := range result.rollbackPatch {
				rollback[key] = value
			}
			rollback["state"] = info.MetadataState
			if _, restoreErr := sessFront.UpdateMetadataInfo(authoritative, rollback); restoreErr != nil {
				return fmt.Errorf("restoring execution-stalled session %s after failed close: %w", info.ID, restoreErr)
			}
		}
		return fmt.Errorf("closing execution-stalled session %s failed", info.ID)
	default:
		return fmt.Errorf("closing execution-stalled session %s returned status %d", info.ID, result.status)
	}
}

// closeExecutionStalledBeadDetailed deliberately uses patch-first ordering.
// Store.Update is the repository's one-call atomic metadata seam, including on
// exec-backed stores whose SetMetadataBatch implementation may decompose into
// per-key commands. If the following status Close fails, the complete,
// auditable ClosePatch tuple remains recoverable by the early stalled finalizer;
// a crash can never leave only state=execution-stalled without its timestamps
// and canonical reason.
func closeExecutionStalledBeadDetailed(store beads.Store, id string, now time.Time) sessionCloseResult {
	snapshot, err := beads.HandlesFor(store).Live.Get(id)
	if err == nil && snapshot.Status == "closed" {
		return sessionCloseResult{status: sessionCloseAlreadyClosed}
	}
	closePatch := sessions.ClosePatch(now, executionStalledDrainReason)
	rollbackPatch := make(sessions.MetadataPatch, len(closePatch))
	for key := range closePatch {
		if err == nil {
			rollbackPatch[key] = snapshot.Metadata[key]
		} else {
			rollbackPatch[key] = ""
		}
	}
	if err := store.Update(id, beads.UpdateOpts{Metadata: map[string]string(closePatch)}); err != nil {
		return sessionCloseResult{status: sessionCloseFailed, rollbackPatch: rollbackPatch}
	}
	if err := store.Close(id); err != nil {
		return sessionCloseResult{status: sessionCloseFailed, rollbackPatch: rollbackPatch}
	}
	cancelStateAssignedToRetiredSessionBead(store, id, now, io.Discard)
	if snapshot.ID != "" {
		releaseWorkFromClosedSessionBead(store, snapshot, io.Discard)
		// The terminal close committed: the durable shared-store stalled fence
		// has no remaining owner. Best-effort exact release; a residue whose
		// release fails here is recovered when the row re-enters an active
		// lane (reopen releases an unlatched residue; the next stall/hook
		// acquire evicts only provably-stale transient kinds).
		releaseExecutionStalledFenceResidue(store, snapshot)
	}
	return sessionCloseResult{status: sessionCloseClosed}
}

// verifiedStop stops a session after verifying the instance_token matches.
// Prevents stale drain operations from targeting a re-woken session.
// Returns errTokenMismatch if the running process has a different token.
//
// NOTE: On composite providers (auto/hybrid), GetMeta and Stop may route
// to different backends if the route table is stale. This is a pre-existing
// routing limitation — when the reconciler is wired in, consider a
// provider-level VerifiedStop that atomically verifies+stops on the same backend.
func verifiedStop(cityPath string, info sessions.Info, store beads.Store, sp runtime.Provider, cfg *config.City) error {
	if store == nil {
		return errors.New("session lifecycle store unavailable")
	}
	acquired, superseded, err := tryWithCurrentDrainLifecycle(cityPath, info, sessionFrontDoor(store), true, func(latest sessions.Info) error {
		name := latest.SessionNameMetadata
		if identityErr := verifyLiveDrainRuntimeIdentity(sp, name, latest); identityErr != nil {
			return identityErr
		}
		handle, handleErr := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, latest.ID)
		if handleErr != nil {
			return handleErr
		}
		return handle.Kill(context.Background())
	})
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("%w for session %s", errDrainLifecycleBusy, info.ID)
	}
	if superseded {
		return fmt.Errorf("%w for session %s", errDrainLifecycleSuperseded, info.ID)
	}
	return nil
}

// stopExecutionStalledRuntime is intentionally only the destructive provider
// call. Full live runtime identity proof runs before actionGuard, and the guard
// then performs the final session/work reads before invoking this callback.
// Manager.Kill would add session-store reads here and reopen the
// claim-completion gap.
func stopExecutionStalledRuntime(info sessions.Info, sp runtime.Provider) error {
	return sp.Stop(info.SessionNameMetadata)
}

// verifiedInterrupt sends an interrupt signal after verifying instance_token.
func verifiedInterrupt(session beads.Bead, store beads.Store, sp runtime.Provider, cfg *config.City) error {
	name := session.Metadata["session_name"]
	expectedToken := session.Metadata["instance_token"]
	if expectedToken != "" {
		actualToken, _ := sp.GetMeta(name, "GC_INSTANCE_TOKEN")
		if actualToken != "" && actualToken != expectedToken {
			return fmt.Errorf("%w for session %s", errTokenMismatch, session.ID)
		}
	}
	handle, err := workerHandleForSessionWithConfig("", store, sp, cfg, session.ID)
	if err != nil {
		return err
	}
	return handle.Interrupt(context.Background(), worker.InterruptRequest{})
}
