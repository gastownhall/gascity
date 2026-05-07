package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/telemetry"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// staleKeyDetectDelay is how long to wait after starting a session before
// checking if it died immediately (stale resume key detection).
const staleKeyDetectDelay = 2 * time.Second

const waitIdleNudgeTimeout = 30 * time.Second

// ErrStateSync reports that the runtime reached the requested lifecycle
// boundary but persisting the corresponding bead metadata failed.
var ErrStateSync = errors.New("session state sync failed")

// stripResumeFlag removes the resume flag and session key from a command
// string, returning a command suitable for a fresh start.
func stripResumeFlag(cmd, resumeFlag, sessionKey string) string {
	if resumeFlag == "" || sessionKey == "" {
		return cmd
	}
	// Remove "--resume <key>" or similar from the command.
	target := resumeFlag + " " + sessionKey
	result := strings.Replace(cmd, " "+target, "", 1)
	if result == cmd {
		// Try without the leading space (flag at start of args).
		result = strings.Replace(cmd, target+" ", "", 1)
	}
	return strings.TrimSpace(result)
}

func (m *Manager) clearStaleResumeMetadata(id string, b *beads.Bead) error {
	if err := m.store.SetMetadata(id, "session_key", ""); err != nil {
		return fmt.Errorf("clearing stale resume metadata session_key: %w", err)
	}
	if err := m.store.SetMetadata(id, "started_config_hash", ""); err != nil {
		return fmt.Errorf("clearing stale resume metadata started_config_hash: %w", err)
	}
	if err := m.store.SetMetadata(id, "continuation_reset_pending", "true"); err != nil {
		return fmt.Errorf("clearing stale resume metadata continuation_reset_pending: %w", err)
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	b.Metadata["session_key"] = ""
	b.Metadata["started_config_hash"] = ""
	b.Metadata["continuation_reset_pending"] = "true"
	return nil
}

func (m *Manager) retryFreshStartAfterStaleKey(
	ctx context.Context,
	id string,
	b *beads.Bead,
	sessName,
	resumeCommand string,
	cfg runtime.Config,
	unroute func(),
) (bool, error) {
	if b.Metadata["session_key"] == "" {
		return false, nil
	}
	freshCmd := stripResumeFlag(resumeCommand, b.Metadata["resume_flag"], b.Metadata["session_key"])
	if err := m.clearStaleResumeMetadata(id, b); err != nil {
		if unroute != nil {
			unroute()
		}
		return false, err
	}
	if freshCmd == resumeCommand {
		if unroute != nil {
			unroute()
		}
		return false, fmt.Errorf("fresh start after stale key: resume command could not be stripped")
	}
	cfg.Command = freshCmd
	if err := m.sp.Start(ctx, sessName, cfg); err != nil {
		if unroute != nil {
			unroute()
		}
		return false, fmt.Errorf("fresh start after stale key: %w", err)
	}
	return true, nil
}

var (
	// ErrNotSession reports that the requested bead is not a session bead.
	ErrNotSession = errors.New("bead is not a session")
	// ErrSessionClosed reports that the requested session has been closed.
	ErrSessionClosed = errors.New("session is closed")
	// ErrSessionInactive reports that the requested session has no live runtime.
	ErrSessionInactive = errors.New("session is not active")
	// ErrResumeRequired reports that the session cannot be resumed without an
	// explicit resume command.
	ErrResumeRequired = errors.New("session requires resume command")
	// ErrNoPendingInteraction reports that a session has nothing awaiting
	// user input or approval resolution.
	ErrNoPendingInteraction = errors.New("session has no pending interaction")
	// ErrInteractionUnsupported reports that the backing runtime cannot
	// surface or resolve structured pending interactions.
	ErrInteractionUnsupported = errors.New("session provider does not support interactive responses")
	// ErrInteractionMismatch reports that the response does not match the
	// currently pending interaction request.
	ErrInteractionMismatch = errors.New("pending interaction does not match request")
	// ErrPendingInteraction reports that the session is blocked on a pending
	// approval or question and cannot accept a new user turn.
	ErrPendingInteraction = errors.New("session has a pending interaction")
)

type sessionMutationLockEntry struct {
	mu   sync.Mutex
	refs int
}

var (
	sessionMutationLocksMu sync.Mutex
	sessionMutationLocks   = map[string]*sessionMutationLockEntry{}
)

func withSessionMutationLock(id string, fn func() error) error {
	lock := acquireSessionMutationLock(id)
	defer releaseSessionMutationLock(id, lock)
	return fn()
}

func acquireSessionMutationLock(id string) *sessionMutationLockEntry {
	sessionMutationLocksMu.Lock()
	lock := sessionMutationLocks[id]
	if lock == nil {
		lock = &sessionMutationLockEntry{}
		sessionMutationLocks[id] = lock
	}
	lock.refs++
	sessionMutationLocksMu.Unlock()

	lock.mu.Lock()
	return lock
}

func releaseSessionMutationLock(id string, lock *sessionMutationLockEntry) {
	lock.mu.Unlock()

	sessionMutationLocksMu.Lock()
	lock.refs--
	if lock.refs == 0 {
		delete(sessionMutationLocks, id)
	}
	sessionMutationLocksMu.Unlock()
}

func sessionName(id string, b beads.Bead) string {
	sessName := b.Metadata["session_name"]
	if sessName == "" {
		sessName = sessionNameFor(id)
	}
	return sessName
}

func (m *Manager) loadSessionBead(id string, allowClosed bool) (beads.Bead, string, error) {
	b, err := m.store.Get(id)
	if err != nil {
		return beads.Bead{}, "", fmt.Errorf("getting session: %w", err)
	}
	if !IsSessionBeadOrRepairable(b) {
		return beads.Bead{}, "", fmt.Errorf("%w: bead %s (type=%q)", ErrNotSession, id, b.Type)
	}
	RepairEmptyType(m.store, &b)
	if !allowClosed && b.Status == "closed" {
		return beads.Bead{}, "", fmt.Errorf("%w: %s", ErrSessionClosed, id)
	}
	sessName := sessionName(id, b)
	if b.Status != "closed" {
		transport, _ := m.transportForBead(b, sessName)
		_ = m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	}
	return b, sessName, nil
}

func (m *Manager) sessionBead(id string) (beads.Bead, string, error) {
	return m.loadSessionBead(id, false)
}

func (m *Manager) ensureRunning(ctx context.Context, id string, b beads.Bead, sessName, resumeCommand string, hints runtime.Config) error {
	transport, transportVerified := m.transportForBead(b, sessName)
	unroute := m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	if State(b.Metadata["state"]) != StateSuspended && m.sp.IsRunning(sessName) {
		if b.Metadata["transport"] == "" && transportVerified {
			m.persistTransport(id, b.Metadata["provider"], transport)
		}
		if err := m.confirmLiveSessionState(id, &b); err != nil {
			return err
		}
		return nil
	}
	if resumeCommand == "" {
		return fmt.Errorf("%w: %s", ErrResumeRequired, id)
	}

	cfg := hints
	cfg.Command = resumeCommand
	if cfg.WorkDir == "" {
		cfg.WorkDir = b.Metadata["work_dir"]
	}
	generation, err := strconv.Atoi(b.Metadata["generation"])
	if err != nil || generation <= 0 {
		generation = DefaultGeneration
	}
	continuationEpoch, err := strconv.Atoi(b.Metadata["continuation_epoch"])
	if err != nil || continuationEpoch <= 0 {
		continuationEpoch = DefaultContinuationEpoch
	}
	instanceToken := b.Metadata["instance_token"]
	if instanceToken == "" {
		instanceToken = NewInstanceToken()
		if err := m.store.SetMetadata(id, "instance_token", instanceToken); err != nil {
			return fmt.Errorf("storing instance token: %w", err)
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata["instance_token"] = instanceToken
	}
	cfg.Env = mergeEnv(cfg.Env, RuntimeEnvWithSessionContext(
		id,
		sessName,
		strings.TrimSpace(b.Metadata["alias"]),
		strings.TrimSpace(b.Metadata["template"]),
		strings.TrimSpace(b.Metadata["session_origin"]),
		generation,
		continuationEpoch,
		instanceToken,
	))
	if gcProvider := providerKind(b); gcProvider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
	}
	cfg = runtime.SyncWorkDirEnv(cfg)
	started := false
	if err := m.sp.Start(ctx, sessName, cfg); err != nil {
		if errors.Is(err, runtime.ErrSessionDiedDuringStartup) && b.Metadata["session_key"] != "" {
			retried, err := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, unroute)
			if err != nil {
				return err
			}
			started = retried
		} else if !errors.Is(err, runtime.ErrSessionExists) || !m.sp.IsRunning(sessName) {
			// Another caller may have resumed the same session after we loaded the
			// bead but before we reached Start. If the runtime is already up, treat
			// the resume as converged and only persist active state below.
			if unroute != nil {
				unroute()
			}
			return fmt.Errorf("resuming session: %w", err)
		}
	} else {
		started = true
	}

	// Stale session key detection: if we just started a session with a
	// resume flag but it died immediately, the session key is likely
	// invalid (e.g., "No conversation found"). Clear the key and retry
	// with a fresh start so the user isn't stuck with a dead pane.
	if started && b.Metadata["session_key"] != "" {
		if err := sleepWithContext(ctx, staleKeyDetectDelay); err != nil {
			// Context canceled during stale-key sleep: the runtime session
			// may already be running but we skip setting state="active".
			// This is self-healing via NDI — the next ensureRunning call
			// sees the suspended-state bead, attempts sp.Start, gets
			// ErrSessionExists (IsRunning=true), and persists "active".
			if unroute != nil {
				unroute()
			}
			return err
		}
		if !m.sp.IsRunning(sessName) {
			retried, err := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, unroute)
			if err != nil {
				return err
			}
			started = retried
		}
	}
	if b.Metadata["transport"] == "" && (started || transportVerified) {
		m.persistTransport(id, b.Metadata["provider"], transport)
	}
	if err := m.syncStoredMCPServers(id, &b, cfg.MCPServers); err != nil {
		return fmt.Errorf("%w: %w", ErrStateSync, err)
	}
	if err := m.confirmLiveSessionState(id, &b); err != nil {
		if started && !errors.Is(err, ErrStateSync) {
			_ = m.sp.Stop(sessName)
		}
		return err
	}
	return nil
}

func (m *Manager) ensureRunningRuntimeOnly(ctx context.Context, id string, b beads.Bead, sessName, resumeCommand string, hints runtime.Config) error {
	transport, _ := m.transportForBead(b, sessName)
	unroute := m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)
	if m.sp.IsRunning(sessName) {
		return nil
	}
	if resumeCommand == "" {
		return fmt.Errorf("%w: %s", ErrResumeRequired, id)
	}

	cfg := hints
	cfg.Command = resumeCommand
	if cfg.WorkDir == "" {
		cfg.WorkDir = b.Metadata["work_dir"]
	}
	generation, err := strconv.Atoi(b.Metadata["generation"])
	if err != nil || generation <= 0 {
		generation = DefaultGeneration
	}
	continuationEpoch, err := strconv.Atoi(b.Metadata["continuation_epoch"])
	if err != nil || continuationEpoch <= 0 {
		continuationEpoch = DefaultContinuationEpoch
	}
	instanceToken := b.Metadata["instance_token"]
	if instanceToken == "" {
		instanceToken = NewInstanceToken()
		if err := m.store.SetMetadata(id, "instance_token", instanceToken); err != nil {
			return fmt.Errorf("storing instance token: %w", err)
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata["instance_token"] = instanceToken
	}
	cfg.Env = mergeEnv(cfg.Env, RuntimeEnvWithSessionContext(
		id,
		sessName,
		strings.TrimSpace(b.Metadata["alias"]),
		strings.TrimSpace(b.Metadata["template"]),
		strings.TrimSpace(b.Metadata["session_origin"]),
		generation,
		continuationEpoch,
		instanceToken,
	))
	if gcProvider := strings.TrimSpace(b.Metadata["provider_kind"]); gcProvider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
	} else if provider := strings.TrimSpace(b.Metadata["provider"]); provider != "" {
		cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": provider})
	}
	cfg = runtime.SyncWorkDirEnv(cfg)
	started := false
	if err := m.sp.Start(ctx, sessName, cfg); err != nil {
		switch {
		case errors.Is(err, runtime.ErrSessionDiedDuringStartup) && b.Metadata["session_key"] != "":
			retried, err := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, unroute)
			if err != nil {
				return err
			}
			started = retried
		case errors.Is(err, runtime.ErrSessionExists) && m.sp.IsRunning(sessName):
			return err
		default:
			if unroute != nil {
				unroute()
			}
			return fmt.Errorf("resuming session: %w", err)
		}
	} else {
		started = true
	}
	if started && b.Metadata["session_key"] != "" {
		if err := sleepWithContext(ctx, staleKeyDetectDelay); err != nil {
			if unroute != nil {
				unroute()
			}
			return err
		}
		if !m.sp.IsRunning(sessName) {
			if _, err := m.retryFreshStartAfterStaleKey(ctx, id, &b, sessName, resumeCommand, cfg, unroute); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) confirmLiveSessionState(id string, b *beads.Bead) error {
	if b == nil {
		return nil
	}
	batch := make(map[string]string)
	switch State(b.Metadata["state"]) {
	case "", StateCreating, StateAsleep, StateSuspended:
		batch["state"] = string(StateActive)
		batch["state_reason"] = "creation_complete"
	}
	if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "" {
		batch["pending_create_claim"] = ""
		batch["pending_create_started_at"] = ""
	}
	if len(batch) == 0 {
		return nil
	}
	if err := m.store.SetMetadataBatch(id, batch); err != nil {
		return fmt.Errorf("%w: updating session state: %w", ErrStateSync, err)
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	for k, v := range batch {
		b.Metadata[k] = v
	}
	return nil
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx == nil {
		time.Sleep(d)
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func formatWaitIdleReminder(source, message string) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	sb.WriteString("You have a deferred reminder that was queued until a safe boundary:\n\n")
	fmt.Fprintf(&sb, "- [%s] %s\n", source, message)
	sb.WriteString("\nHandle them after this turn.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

func (m *Manager) nudgeSession(ctx context.Context, sessName, message string, immediate bool) error {
	content := runtime.TextContent(message)
	err := m.nudgeContent(sessName, content, immediate)
	recordCtx := ctx
	if recordCtx == nil || recordCtx.Err() != nil {
		recordCtx = context.Background()
	}
	telemetry.RecordNudge(recordCtx, sessName, err)
	if err != nil {
		return fmt.Errorf("sending message to session: %w", err)
	}
	return nil
}

func (m *Manager) nudgeContent(sessName string, content []runtime.ContentBlock, immediate bool) error {
	if immediate {
		if np, ok := m.sp.(runtime.ImmediateNudgeProvider); ok {
			return np.NudgeNow(sessName, content)
		}
	}
	return m.sp.Nudge(sessName, content)
}

func normalizeWaitIdleNudgeSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "session"
	}
	return source
}

func (m *Manager) tryWaitIdleNudgeLocked(ctx context.Context, id string, b beads.Bead, source, sessName, message, resumeCommand string, hints runtime.Config) (bool, error) {
	if transportFromMetadata(b) == "acp" {
		if err := m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints); err != nil {
			return false, err
		}
		if err := m.nudgeSession(ctx, sessName, message, false); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints); err != nil {
		return false, err
	}
	if providerKind(b) != "claude" {
		return false, nil
	}
	waiter, ok := m.sp.(runtime.IdleWaitProvider)
	if !ok {
		return false, nil
	}
	if err := waiter.WaitForIdle(ctx, sessName, waitIdleNudgeTimeout); err != nil {
		return false, nil
	}
	if err := m.nudgeSession(ctx, sessName, formatWaitIdleReminder(normalizeWaitIdleNudgeSource(source), message), true); err != nil {
		return false, nil
	}
	return true, nil
}

func (m *Manager) tryWaitIdleNudgeLiveOnlyLocked(ctx context.Context, b beads.Bead, source, sessName, message string) (bool, error) {
	if !m.sp.IsRunning(sessName) {
		return false, nil
	}
	if transportFromMetadata(b) == "acp" {
		if err := m.nudgeSession(ctx, sessName, message, false); err != nil {
			return false, err
		}
		return true, nil
	}
	if providerKind(b) != "claude" {
		return false, nil
	}
	waiter, ok := m.sp.(runtime.IdleWaitProvider)
	if !ok {
		return false, nil
	}
	if err := waiter.WaitForIdle(ctx, sessName, waitIdleNudgeTimeout); err != nil {
		return false, nil
	}
	if err := m.nudgeSession(ctx, sessName, formatWaitIdleReminder(normalizeWaitIdleNudgeSource(source), message), true); err != nil {
		return false, nil
	}
	return true, nil
}

func (m *Manager) pendingInteractionLocked(sessName string) error {
	if ip, ok := m.sp.(runtime.InteractionProvider); ok {
		pending, err := ip.Pending(sessName)
		if err != nil && !errors.Is(err, runtime.ErrInteractionUnsupported) {
			return fmt.Errorf("getting pending interaction: %w", err)
		}
		if pending != nil {
			return ErrPendingInteraction
		}
	}
	return nil
}

func (m *Manager) dismissKnownDialogsLocked(ctx context.Context, sessName string, timeout time.Duration) bool {
	dp, ok := m.sp.(runtime.DialogProvider)
	if !ok {
		return false
	}
	_ = dp.DismissKnownDialogs(ctx, sessName, timeout)
	return true
}

func (m *Manager) markStartupDialogsVerifiedLocked(id string, b *beads.Bead) {
	if err := m.store.SetMetadata(id, startupDialogVerifiedKey, "true"); err != nil {
		return
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	b.Metadata[startupDialogVerifiedKey] = "true"
}

func (m *Manager) sendLocked(ctx context.Context, id string, b beads.Bead, sessName, message, resumeCommand string, hints runtime.Config, immediate bool) error {
	if err := m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints); err != nil {
		return err
	}
	verifyDeferredDialogs := needsDeferredStartupDialogVerification(b)
	if verifyDeferredDialogs {
		m.dismissKnownDialogsLocked(ctx, sessName, codexDeferredDialogDelay)
	}
	if err := m.pendingInteractionLocked(sessName); err != nil {
		return err
	}
	if err := m.nudgeSession(ctx, sessName, message, immediate); err != nil {
		return err
	}
	if verifyDeferredDialogs && m.dismissKnownDialogsLocked(ctx, sessName, codexDeferredDialogDelay) {
		m.markStartupDialogsVerifiedLocked(id, &b)
	}
	return nil
}

func (m *Manager) send(ctx context.Context, id, message, resumeCommand string, hints runtime.Config, immediate bool) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		return m.sendLocked(ctx, id, b, sessName, message, resumeCommand, hints, immediate)
	})
}

func (m *Manager) sendLiveOnly(ctx context.Context, id, message string, immediate bool) (bool, error) {
	var delivered bool
	err := withSessionMutationLock(id, func() error {
		_, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		if !m.sp.IsRunning(sessName) {
			delivered = false
			return nil
		}
		if err := m.nudgeSession(ctx, sessName, message, immediate); err != nil {
			return err
		}
		delivered = true
		return nil
	})
	return delivered, err
}

// Start ensures the session runtime is live without sending a message.
// It is the canonical manager-level bring-up path for worker handles and
// other callers that need bounded startup without attaching a terminal.
func (m *Manager) Start(ctx context.Context, id, resumeCommand string, hints runtime.Config) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		return m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints)
	})
}

// StartRuntimeOnly brings the runtime live for a bead-backed session without
// mutating persisted lifecycle metadata. Legacy reconciler callers use this
// bridge while they still own commit/rollback bookkeeping above the worker
// boundary.
func (m *Manager) StartRuntimeOnly(ctx context.Context, id, resumeCommand string, hints runtime.Config) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		return m.ensureRunningRuntimeOnly(ctx, id, b, sessName, resumeCommand, hints)
	})
}

// Send resumes a suspended session if needed, then nudges the runtime with a
// new user message.
func (m *Manager) Send(ctx context.Context, id, message, resumeCommand string, hints runtime.Config) error {
	return m.send(ctx, id, message, resumeCommand, hints, false)
}

// SendImmediate resumes a suspended session if needed, then injects the new
// user message without waiting for an idle boundary when the runtime supports
// immediate nudges. Falls back to Send semantics on runtimes without the
// optional immediate nudge capability.
func (m *Manager) SendImmediate(ctx context.Context, id, message, resumeCommand string, hints runtime.Config) error {
	return m.send(ctx, id, message, resumeCommand, hints, true)
}

// SendLiveOnly nudges the runtime only when the current session is already
// running. It never resumes or restarts the session.
func (m *Manager) SendLiveOnly(ctx context.Context, id, message string) (bool, error) {
	return m.sendLiveOnly(ctx, id, message, false)
}

// SendImmediateLiveOnly is like SendLiveOnly but uses the immediate nudge path
// when the runtime supports it. It never resumes or restarts the session.
func (m *Manager) SendImmediateLiveOnly(ctx context.Context, id, message string) (bool, error) {
	return m.sendLiveOnly(ctx, id, message, true)
}

// TryWaitIdleNudge delivers a best-effort session nudge at a provider-defined
// safe boundary. It resumes supported runtimes if needed, then reports whether
// live delivery actually happened. Unsupported providers return (false, nil)
// so higher layers can fall back to queue semantics without treating that as
// an operational error.
func (m *Manager) TryWaitIdleNudge(ctx context.Context, id, source, message, resumeCommand string, hints runtime.Config) (bool, error) {
	var delivered bool
	err := withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		delivered, err = m.tryWaitIdleNudgeLocked(ctx, id, b, source, sessName, message, resumeCommand, hints)
		return err
	})
	return delivered, err
}

// TryWaitIdleNudgeLiveOnly delivers a best-effort nudge at a safe boundary
// only when the runtime is already live. It never resumes or restarts the
// session.
func (m *Manager) TryWaitIdleNudgeLiveOnly(ctx context.Context, id, source, message string) (bool, error) {
	var delivered bool
	err := withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		delivered, err = m.tryWaitIdleNudgeLiveOnlyLocked(ctx, b, source, sessName, message)
		return err
	})
	return delivered, err
}

// StopTurn issues a provider-appropriate interrupt for the currently running
// turn. For providers that need post-interrupt idle settlement (e.g. Claude),
// it waits for the session to return to an idle prompt before returning.
func (m *Manager) StopTurn(id string) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		interruptStartedAt := time.Now()
		if err := m.stopTurnLocked(b, sessName); err != nil {
			return err
		}
		if err := m.waitForInterruptIdleLocked(context.Background(), b, sessName); err != nil {
			return fmt.Errorf("waiting for stopped session to become idle: %w", err)
		}
		if err := m.waitForInterruptBoundaryLocked(context.Background(), b, sessName, interruptStartedAt); err != nil {
			return fmt.Errorf("waiting for stopped session interrupt boundary: %w", err)
		}
		return nil
	})
}

// Pending returns the provider's current structured pending interaction, if
// the provider supports that capability.
func (m *Manager) Pending(id string) (*runtime.PendingInteraction, bool, error) {
	_, sessName, err := m.sessionBead(id)
	if err != nil {
		return nil, false, err
	}
	ip, ok := m.sp.(runtime.InteractionProvider)
	if !ok {
		return nil, false, nil
	}
	pending, err := ip.Pending(sessName)
	if err != nil {
		if errors.Is(err, runtime.ErrInteractionUnsupported) {
			return nil, false, nil
		}
		if errors.Is(err, runtime.ErrSessionNotFound) {
			log.Printf("session: pending interaction runtime session gone for %q: %v", sessName, err)
			return nil, true, nil
		}
		return nil, true, fmt.Errorf("getting pending interaction: %w", err)
	}
	return pending, true, nil
}

// Respond resolves the current pending interaction for a session.
func (m *Manager) Respond(id string, response runtime.InteractionResponse) error {
	return withSessionMutationLock(id, func() error {
		_, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		ip, ok := m.sp.(runtime.InteractionProvider)
		if !ok {
			return ErrInteractionUnsupported
		}
		pending, err := ip.Pending(sessName)
		if err != nil {
			if errors.Is(err, runtime.ErrInteractionUnsupported) {
				return ErrInteractionUnsupported
			}
			if errors.Is(err, runtime.ErrSessionNotFound) {
				log.Printf("session: respond pending probe runtime session gone for %q: %v", sessName, err)
				return ErrNoPendingInteraction
			}
			return fmt.Errorf("getting pending interaction: %w", err)
		}
		if pending == nil {
			return ErrNoPendingInteraction
		}
		if response.RequestID == "" {
			response.RequestID = pending.RequestID
		}
		if response.Action == "" {
			return fmt.Errorf("interaction action is required")
		}
		if pending.RequestID != "" && response.RequestID != pending.RequestID {
			return fmt.Errorf("%w: pending interaction %q does not match request %q", ErrInteractionMismatch, pending.RequestID, response.RequestID)
		}
		if err := ip.Respond(sessName, response); err != nil {
			if errors.Is(err, runtime.ErrInteractionUnsupported) {
				return ErrInteractionUnsupported
			}
			if errors.Is(err, runtime.ErrSessionNotFound) {
				log.Printf("session: respond runtime session gone for %q: %v", sessName, err)
				return ErrNoPendingInteraction
			}
			return fmt.Errorf("responding to pending interaction: %w", err)
		}
		return nil
	})
}

// TranscriptPath resolves the best available session transcript file.
// It prefers session-key-specific lookup and falls back to workdir-based
// discovery for providers that do not expose a stable session key.
func (m *Manager) TranscriptPath(id string, searchPaths []string) (string, error) {
	b, _, err := m.loadSessionBead(id, true)
	if err != nil {
		return "", err
	}
	workDir := b.Metadata["work_dir"]
	if workDir == "" {
		return "", nil
	}
	provider := strings.TrimSpace(b.Metadata["provider_kind"])
	if provider == "" {
		provider = strings.TrimSpace(b.Metadata["provider"])
	}
	if len(searchPaths) == 0 {
		searchPaths = sessionlog.DefaultSearchPaths()
	}
	if path := workertranscript.DiscoverKeyedPath(searchPaths, provider, workDir, b.Metadata["session_key"]); path != "" {
		return path, nil
	}

	all, err := m.store.List(beads.ListQuery{
		Label:         LabelSession,
		IncludeClosed: b.Status == "closed",
	})
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}
	matches := 0
	for _, other := range all {
		if !IsSessionBeadOrRepairable(other) {
			continue
		}
		// For a live target, closed historical sessions should not make the
		// lookup ambiguous. For a closed target, historical siblings sharing
		// the same workdir are the ambiguity we need to preserve.
		if b.Status != "closed" && other.Status == "closed" {
			continue
		}
		otherProvider := strings.TrimSpace(other.Metadata["provider_kind"])
		if otherProvider == "" {
			otherProvider = strings.TrimSpace(other.Metadata["provider"])
		}
		if provider != "" && otherProvider != provider {
			continue
		}
		if other.Metadata["work_dir"] == workDir {
			matches++
			if matches > 1 {
				// Without a stable session key, multiple sessions sharing the
				// same workdir cannot be mapped safely to a single transcript.
				return "", nil
			}
		}
	}
	return workertranscript.DiscoverPath(searchPaths, provider, workDir, ""), nil
}
