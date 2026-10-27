package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/nudgepoller"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

const (
	defaultQueuedSubmitTTL    = 24 * time.Hour
	interruptClearDelay       = 100 * time.Millisecond
	interruptBoundaryWait     = 10 * time.Second
	codexDeferredDialogDelay  = 2 * time.Second
	softInterruptFallbackWait = 2 * time.Second
	startupDialogVerifiedKey  = "startup_dialog_verified"
)

// SubmitIntent is the semantic delivery choice for a user message.
type SubmitIntent string

const (
	// SubmitIntentDefault asks the session runtime to deliver the message using
	// its normal provider-specific behavior.
	SubmitIntentDefault SubmitIntent = "default"
	// SubmitIntentFollowUp asks the session runtime to hold the message until
	// the current run reaches its follow-up boundary.
	SubmitIntentFollowUp SubmitIntent = "follow_up"
	// SubmitIntentInterruptNow asks the session runtime to interrupt the current
	// run and deliver the replacement message immediately.
	SubmitIntentInterruptNow SubmitIntent = "interrupt_now"
)

// SubmissionCapabilities describes which submit intents a session can honor.
type SubmissionCapabilities struct {
	SupportsFollowUp     bool `json:"supports_follow_up"`
	SupportsInterruptNow bool `json:"supports_interrupt_now"`
	// SupportsSteer reports that a steer reaches the session's running turn
	// ([SupportsSteerForInfo]): the runtime's steer mode combined with the
	// session's provider family, whichever host delivers the steer. No
	// submit intent steers yet: the capability is read by conversation-lane
	// producers, whose steers only the conversation lane delivers. The
	// lane's Caps.Steer is this capability only while the lane runs the
	// conversation live; it is false while another host serves the
	// conversation or the runtime cancels turns by protocol and its own
	// protocol does not report its turns, starts included, which the lane
	// does not run live, and there a steer waits not_live whatever its
	// on_unsupported policy. On a live lane, a steer the session cannot take
	// follows that policy: boundary waits for the turn's boundary, fail
	// closes the op failed{unsupported}.
	SupportsSteer bool `json:"supports_steer"`
}

// SubmitOutcome reports whether a submit was delivered now or queued.
type SubmitOutcome struct {
	Queued bool
	// Deferred marks a queue the actor caused: the call may not start
	// or resume the session (CONTRACT v5.9 D8 rule 1), so a controller should.
	Deferred bool
	// Interrupt is what an interrupt_now did about the turn it targeted when
	// the session's runtime reports turns (SESSION-RUNTIME-020). It is
	// set once the fence ran, a failed send or queue write included; nil when
	// the submit took no turn-fenced path or was refused before the fence (a
	// stopping or kill-pending row, the runtime lease not taken).
	Interrupt *TurnInterruptReport
}

// SubmissionCapabilitiesForMetadata derives runtime submit affordances from
// persisted session metadata and whether deferred queueing is available. It
// knows no runtime, so it never reports SupportsSteer.
func SubmissionCapabilitiesForMetadata(metadata map[string]string, hasDeferredQueue bool) SubmissionCapabilities {
	transport := transportFromMetadata(beads.Bead{Metadata: metadata})
	return SubmissionCapabilities{
		SupportsFollowUp:     hasDeferredQueue && transport != "acp",
		SupportsInterruptNow: supportsInterruptNowForMetadata(metadata),
	}
}

// SubmissionCapabilitiesForRuntime is [SubmissionCapabilitiesForMetadata] for
// a session whose runtime cancels turns in mode, the session's
// [runtime.TurnInterruptModeFor], and steers in steer, its
// [runtime.SteerModeFor]: a runtime that cannot interrupt the session's turn
// withdraws interrupt_now, and steer is the runtime's mode combined with the
// session's provider family ([SupportsSteerForInfo]).
func SubmissionCapabilitiesForRuntime(metadata map[string]string, hasDeferredQueue bool, mode runtime.TurnInterruptMode, steer runtime.SteerMode) SubmissionCapabilities {
	caps := SubmissionCapabilitiesForMetadata(metadata, hasDeferredQueue)
	if refusesTurnInterrupt(beads.Bead{Metadata: metadata}, mode) {
		caps.SupportsInterruptNow = false
	}
	caps.SupportsSteer = supportsSteerForMetadata(metadata, steer)
	return caps
}

func supportsInterruptNowForMetadata(metadata map[string]string) bool {
	return ProviderFamilyFromMetadata(metadata, "") != "antigravity"
}

// SubmissionCapabilities reports which semantic submit intents the session can
// currently support. A runtime that cannot interrupt the session's turn
// withdraws interrupt_now; steer is the runtime's steer mode combined with the
// session's provider family.
func (m *Manager) SubmissionCapabilities(id string) (SubmissionCapabilities, error) {
	b, sessName, err := m.loadSessionBead(id, true)
	if err != nil {
		return SubmissionCapabilities{}, err
	}
	return SubmissionCapabilitiesForRuntime(b.Metadata, m.deferredQueue() != nil,
		runtime.TurnInterruptModeFor(m.sp, sessName), runtime.SteerModeFor(m.sp, sessName)), nil
}

// refusesTurnInterrupt reports whether a runtime with mode cannot replace the
// session's running turn: it can interrupt it neither by keys nor by
// protocol, and the session's family does not replace turns by restarting.
func refusesTurnInterrupt(b beads.Bead, mode runtime.TurnInterruptMode) bool {
	return mode == runtime.TurnInterruptUnsupported && !requiresHardRestartInterrupt(b)
}

// Submit delivers a user message according to the requested semantic intent.
// A dormant session it may not resume as its actor (CONTRACT v5.9 D8) gets
// the message queued.
func (m *Manager) Submit(ctx context.Context, by Actor, id, message, resumeCommand string, hints runtime.Config, intent SubmitIntent) (SubmitOutcome, error) {
	switch intent {
	case "", SubmitIntentDefault, SubmitIntentFollowUp, SubmitIntentInterruptNow:
	default:
		return SubmitOutcome{}, fmt.Errorf("invalid submit intent %q", intent)
	}
	return m.submit(ctx, by, id, message, resumeCommand, hints, intent)
}

func (m *Manager) submit(ctx context.Context, by Actor, id, message, resumeCommand string, hints runtime.Config, intent SubmitIntent) (SubmitOutcome, error) {
	if err := m.checkStarter(by); err != nil {
		return SubmitOutcome{}, err
	}
	var seen turnSighting // lock waits and lease retries fall after it
	if intent == SubmitIntentInterruptNow {
		seen = m.sightTurn(ctx, id)
	}
	var outcome SubmitOutcome
	err := withSessionStartLock(ctx, id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		if m.queueFor(b.Metadata, sessName, by) {
			outcome.Queued, outcome.Deferred = true, true
			return m.enqueueDeferredSubmitLocked(b, sessName, message, nudgequeue.SourceSubmit)
		}
		switch intent {
		case SubmitIntentFollowUp:
			if !m.supportsFollowUpLocked(b) {
				return ErrInteractionUnsupported
			}
			if State(b.Metadata["state"]) == StateSuspended || !m.sp.IsRunning(sessName) {
				outcome.Queued, err = m.sendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints, true, nudgequeue.SourceSubmit)
				outcome.Deferred = outcome.Queued
				return err
			}
			if err := m.pendingInteractionLocked(sessName); err != nil {
				return err
			}
			if err := m.enqueueDeferredSubmitLocked(b, sessName, message, nudgequeue.SourceSubmit); err != nil {
				return err
			}
			outcome.Queued = true
			return nil
		case SubmitIntentInterruptNow:
			if !supportsInterruptNowForMetadata(b.Metadata) {
				return ErrInteractionUnsupported
			}
			// The interrupt keystroke would dismiss a pending permission
			// prompt as a side effect; make the caller answer it explicitly.
			if State(b.Metadata["state"]) != StateSuspended && m.sp.IsRunning(sessName) {
				if err := m.pendingInteractionLocked(sessName); err != nil {
					return err
				}
			}
			if tf := m.turnFenceFor(ctx, by, id, b, sessName, seen); tf != nil {
				outcome, err = m.fencedInterruptAndSubmitLocked(ctx, by, id, b, sessName, message, resumeCommand, hints, tf, seen)
				return err
			}
			outcome.Queued, err = m.interruptAndSubmitLocked(ctx, by, id, b, sessName, message, resumeCommand, hints)
			outcome.Deferred = outcome.Queued
			return err
		default:
			running := m.sp.IsRunning(sessName)
			if pendingConversationRestart(b) && !running {
				// A reset or restart is recorded but the replacement runtime is
				// not up yet. Delivering now would start the pane just to carry
				// this message and, for a provider that keys its own
				// conversation identity off the session's epoch, would do it
				// against a conversation the operator has already discarded.
				// Queue it for the incarnation the controller is about to bring
				// up instead.
				if err := m.enqueueDeferredSubmitLocked(b, sessName, message, nudgequeue.SourceSubmit); err != nil {
					return err
				}
				outcome.Queued = true
				return nil
			}
			if (State(b.Metadata["state"]) == StateStartPending || State(b.Metadata["state"]) == StateCreating) && !running {
				if err := m.enqueueDeferredSubmitLocked(b, sessName, message, nudgequeue.SourceSubmit); err != nil {
					return err
				}
				outcome.Queued = true
				return nil
			}
			resuming := State(b.Metadata["state"]) == StateSuspended || !running
			outcome.Queued, err = m.sendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints, usesImmediateDefaultSubmit(b, resuming), nudgequeue.SourceSubmit)
			outcome.Deferred = outcome.Queued
			return err
		}
	})
	return outcome, err
}

// pendingConversationRestart reports whether a fresh restart has been recorded
// for this session but not yet carried out, so its live runtime — if any — is
// the outgoing incarnation.
func pendingConversationRestart(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata["continuation_reset_pending"]) != "" ||
		strings.TrimSpace(b.Metadata["restart_requested"]) != ""
}

func (m *Manager) supportsFollowUpLocked(b beads.Bead) bool {
	return SubmissionCapabilitiesForMetadata(
		b.Metadata,
		m.deferredQueue() != nil,
	).SupportsFollowUp
}

// interruptAndSubmitLocked runs after submit's actor check. It reports
// whether its send queued the message: a dead agent's (sendLocked, on
// ensureDeliverable's fresh read) takes no interrupt keys and queues it for
// the incarnation the controller's heal brings up.
func (m *Manager) interruptAndSubmitLocked(ctx context.Context, by Actor, id string, b beads.Bead, sessName, message, resumeCommand string, hints runtime.Config) (bool, error) {
	// A row whose drain-ack stop is pending belongs to the controller's stop:
	// no interrupt keys, no hard-restart stop (as ensureRunning refuses it).
	if drainAckStopPendingMetadata(b.Metadata) {
		return false, fmt.Errorf("%w: %s", ErrSessionStopping, id)
	}
	running := State(b.Metadata["state"]) != StateSuspended && m.liveAgent(sessName, hints.ProcessNames)
	if !running {
		return m.sendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints, true, nudgequeue.SourceSubmit)
	}
	mode := runtime.TurnInterruptModeFor(m.sp, sessName)
	// A protocol runtime cancels the turn itself; its Stop may leave the
	// runtime running, so the hard restart would interrupt nothing.
	hardRestart := requiresHardRestartInterrupt(b) && mode != runtime.TurnInterruptProtocol
	if refusesTurnInterrupt(b, mode) {
		// Refuse before taking the lease: waiting on a busy lease would
		// hide this reason behind ErrSessionStarting.
		return false, fmt.Errorf("%w: %s", ErrTurnInterruptUnsupported, sessName)
	}
	// Every interrupt may end in a stop and a restart: take the runtime lease
	// before sending anything, and carry it to the restart (D8D-5).
	by, release, err := m.leaseForRestartLocked(ctx, by, id, sessName)
	if err != nil {
		return false, err
	}
	defer release()
	if hardRestart {
		piTranscriptPath, err := piPendingTurnPath(b, hints)
		if err != nil {
			return false, err
		}
		// Replacement only needs the outgoing incarnation gone, so a session
		// that is already gone — including one whose tmux server died — is
		// cleared to restart rather than reported as a failed stop.
		if err := runtime.StopForCleanup(m.sp, sessName); err != nil {
			return false, fmt.Errorf("stopping session for interrupt replacement: %w", err)
		}
		if err := discardPiPendingTurn(piTranscriptPath, hints); err != nil {
			// Pi must be stopped before transcript truncation so it cannot race the
			// write. If truncation fails, restart on the dirty transcript; the
			// caller's retry will repeat this stop-and-discard sequence.
			if restartErr := m.restoreAfterHardRestartFailureLocked(ctx, by, id, b, sessName, resumeCommand, hints); restartErr != nil {
				return false, fmt.Errorf("%w; additionally failed to restore session: %w", err, restartErr)
			}
			return false, err
		}
		return false, m.restartAndSendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints)
	}
	interruptStartedAt := time.Now()
	if err := m.stopTurnLocked(ctx, b, sessName, mode); err != nil {
		return false, err
	}
	if err := m.waitForInterruptIdleLocked(ctx, b, sessName, mode, nil); err != nil {
		if mode == runtime.TurnInterruptProtocol {
			// A protocol runtime's Stop may leave it running, so a restart
			// would interrupt nothing and nudge the message into the turn.
			return false, fmt.Errorf("waiting for interrupted session to become idle: %w", err)
		}
		// Idle wait failed (e.g. timeout). Fall back to hard
		// restart so the session isn't left in limbo.
		if stopErr := runtime.StopForCleanup(m.sp, sessName); stopErr != nil {
			return false, fmt.Errorf("stopping session after idle timeout: %w", stopErr)
		}
		return false, m.restartAndSendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints)
	}
	if err := m.waitForInterruptBoundaryLocked(ctx, b, sessName, interruptStartedAt); err != nil {
		if mode == runtime.TurnInterruptProtocol {
			return false, err
		}
		if stopErr := runtime.StopForCleanup(m.sp, sessName); stopErr != nil {
			return false, fmt.Errorf("stopping session after interrupt boundary timeout: %w", stopErr)
		}
		return false, m.restartAndSendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints)
	}
	if err := m.resetInterruptedTurnLocked(ctx, b, sessName); err != nil {
		return false, err
	}
	if shouldClearInterruptedInputBeforeSubmit(b) && mode == runtime.TurnInterruptKeystroke {
		if err := m.clearInterruptedInputLocked(ctx, b, sessName); err != nil {
			return false, err
		}
	}
	return m.sendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints, true, nudgequeue.SourceSubmit)
}

// restartAndSendLocked replaces the runtime the interrupt just stopped. The
// hold was decided before the stop, so the restart does not re-read it: the
// dead runtime would make a heartbeat or unreadable hold refuse, losing the
// session and the message. Only an operator's resume consumes a user hold
// (Actor.consumesHold).
func (m *Manager) restartAndSendLocked(ctx context.Context, by Actor, id string, b beads.Bead, sessName, message, resumeCommand string, hints runtime.Config) error {
	if err := m.ensureRunning(ctx, by, id, b, sessName, resumeCommand, hints, true); err != nil {
		return err
	}
	if err := m.waitUntilRunningLocked(ctx, id, sessName, 2*time.Second); err != nil {
		return err
	}
	if waiter, ok := m.sp.(runtime.IdleWaitProvider); ok && waitsForIdleAfterHardRestart(b) {
		if err := waiter.WaitForIdle(ctx, sessName, 15*time.Second); err != nil && !errors.Is(err, runtime.ErrInteractionUnsupported) {
			return fmt.Errorf("waiting for restarted session to become idle: %w", err)
		}
	}
	// This is a fresh replacement turn after a hard restart. The previous run's
	// pending-interaction state is irrelevant, and probing tmux immediately after
	// the restart is race-prone for Claude-backed sessions.
	return m.nudgeSession(ctx, sessName, message, true)
}

func (m *Manager) waitUntilRunningLocked(ctx context.Context, id, sessName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if m.sp.IsRunning(sessName) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s", ErrSessionInactive, id)
		}
		if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

// stopTurnLocked sends the turn interrupt for mode, the session runtime's
// [runtime.TurnInterruptModeFor]. Only keystroke mode sends the
// provider-specific keys (soft Escape, Ctrl-C fallback, input clear): a
// protocol runtime drops keys and is interrupted through Interrupt alone.
// Every input goes under the fence ctx carries, if any. A session with no
// turn to interrupt (stopFindsNoTurn) is sent nothing: under a fence its
// fresh read decides the miss, as it may have stopped since its caller read
// it running.
func (m *Manager) stopTurnLocked(ctx context.Context, b beads.Bead, sessName string, mode runtime.TurnInterruptMode) error {
	_, err := m.sendTurnInterruptLocked(ctx, b, sessName, mode, false)
	return err
}

// sendTurnInterruptLocked is stopTurnLocked, also reporting none when it
// found no turn to interrupt and sent nothing. fresh decides whether the
// runtime runs from a fresh liveness read, as stopFindsNoTurn documents.
func (m *Manager) sendTurnInterruptLocked(ctx context.Context, b beads.Bead, sessName string, mode runtime.TurnInterruptMode, fresh bool) (none bool, err error) {
	if none, err := m.stopFindsNoTurn(ctx, b, sessName, fresh); none {
		return true, err
	}
	switch mode {
	case runtime.TurnInterruptProtocol:
		if err := runtime.InterruptWithContext(ctx, m.sp, sessName); err != nil {
			return false, fmt.Errorf("interrupting session: %w", err)
		}
		return false, nil
	case runtime.TurnInterruptUnsupported:
		return false, fmt.Errorf("%w: %s", ErrTurnInterruptUnsupported, sessName)
	}
	if usesSoftEscapeInterrupt(b) {
		if err := runtime.SendKeysWithContext(ctx, m.sp, sessName, "Escape"); err != nil {
			return false, fmt.Errorf("interrupting session: %w", err)
		}
		return false, nil
	}
	if err := runtime.InterruptWithContext(ctx, m.sp, sessName); err != nil {
		return false, fmt.Errorf("interrupting session: %w", err)
	}
	return false, nil
}

// waitForInterruptIdleLocked waits for a family that settles to an idle
// prompt after its interrupt to read idle, sending the keystroke sequence's
// follow-up interrupt on the way: a soft-escape family's Ctrl-C fallback once
// the Escape has not settled it within softInterruptFallbackWait, or
// Gemini's immediate confirm. The follow-up interrupts whatever turn runs
// when it lands, so a checked stop (StopTurnIf) passes its check as still:
// the follow-up goes only once still passes again, and a refusal sends
// nothing more and fails the wait with a *followUpRefusedError, which wraps
// none of the refusal, since the first key was sent: StopTurnWith reads the
// refusal from it.
func (m *Manager) waitForInterruptIdleLocked(ctx context.Context, b beads.Bead, sessName string, mode runtime.TurnInterruptMode, still func() error) error {
	if !waitsForIdleAfterInterrupt(b) {
		return nil
	}
	waiter, ok := m.sp.(runtime.IdleWaitProvider)
	if !ok {
		return nil
	}
	waitForIdle := func(timeout time.Duration) error {
		err := waiter.WaitForIdle(ctx, sessName, timeout)
		if errors.Is(err, runtime.ErrInteractionUnsupported) {
			return nil
		}
		return err
	}
	// waited is the error of the idle wait before the follow-up, nil when
	// none ran (followUpSendError).
	followUp := func(what string, waited error) error {
		if still != nil {
			if err := still(); err != nil {
				return &followUpRefusedError{what: what, refusal: err}
			}
		}
		if err := runtime.InterruptWithContext(ctx, m.sp, sessName); err != nil {
			return followUpSendError(what, waited, err)
		}
		return nil
	}
	if usesSoftEscapeInterrupt(b) && mode == runtime.TurnInterruptKeystroke {
		if requiresImmediateInterruptConfirm(b) {
			if err := followUp("confirming interrupt after soft escape", nil); err != nil {
				return err
			}
			return waitForIdle(15 * time.Second)
		}
		waited := waitForIdle(softInterruptFallbackWait)
		if waited == nil {
			return nil
		}
		if err := followUp("interrupting session with control-c fallback", waited); err != nil {
			return err
		}
	}
	return waitForIdle(15 * time.Second)
}

func (m *Manager) waitForInterruptBoundaryLocked(ctx context.Context, b beads.Bead, sessName string, since time.Time) error {
	_, err := m.readInterruptBoundaryLocked(ctx, b, sessName, since)
	return err
}

// readInterruptBoundaryLocked is waitForInterruptBoundaryLocked, also
// reporting whether a boundary wait ran: false when the family has none or
// the runtime cannot wait for one (runtime.ErrInteractionUnsupported), which
// passes without reading the turn.
func (m *Manager) readInterruptBoundaryLocked(ctx context.Context, b beads.Bead, sessName string, since time.Time) (waited bool, err error) {
	if !requiresInterruptBoundaryWait(b) {
		return false, nil
	}
	waiter, ok := m.sp.(runtime.InterruptBoundaryWaitProvider)
	if !ok {
		return false, nil
	}
	err = waiter.WaitForInterruptBoundary(ctx, sessName, since, interruptBoundaryWait)
	switch {
	case errors.Is(err, runtime.ErrInteractionUnsupported):
		return false, nil
	case err != nil:
		return true, fmt.Errorf("waiting for interrupt boundary: %w", err)
	}
	return true, nil
}

// ProviderFamilyFromMetadata returns the canonical provider family for session
// metadata. It prefers metadata stamped at session-bead creation over raw
// provider names so wrapped custom aliases inherit their built-in family.
// Preference order:
//  1. builtin_ancestor — stamped from ResolvedProvider.BuiltinAncestor
//     at session-bead creation for custom providers with explicit
//     `base = "builtin:..."` (see cmd/gc session-bead creation sites).
//  2. provider_kind — stamped for command-matched custom aliases
//     (legacy Phase A auto-inheritance path).
//  3. provider — normalized through sessionlog.ProviderFamily as a
//     last-resort fallback.
//
// Callers that branch on Claude/Codex/Gemini-family behavior
// (idle-wait-after-interrupt, soft-escape interrupt, default submit,
// etc.) consume this helper so wrapped custom aliases inherit the
// correct family behavior without every call site re-deriving it.
func ProviderFamilyFromMetadata(meta map[string]string, fallback string) string {
	if ancestor := strings.TrimSpace(meta["builtin_ancestor"]); ancestor != "" {
		return sessionlog.ProviderFamily(ancestor)
	}
	if kind := strings.TrimSpace(meta["provider_kind"]); kind != "" {
		return sessionlog.ProviderFamily(kind)
	}
	if provider := strings.TrimSpace(meta["provider"]); provider != "" {
		return sessionlog.ProviderFamily(provider)
	}
	return sessionlog.ProviderFamily(fallback)
}

func providerKind(b beads.Bead) string {
	return ProviderFamilyFromMetadata(b.Metadata, "")
}

// ProviderFamilyFromInfo is the session.Info sibling of ProviderFamilyFromMetadata:
// it walks the same builtin_ancestor → provider_kind → provider precedence ladder,
// reading the raw mirrors Info carries (BuiltinAncestor, ProviderKind, Provider)
// instead of the bead metadata map. Byte-identical to the metadata form for any
// bead b (ProviderFamilyFromInfo(infoFromPersistedBead(b), fallback) ==
// ProviderFamilyFromMetadata(b.Metadata, fallback)), so a caller holding a typed
// Info can resolve the provider family without the raw bead.
func ProviderFamilyFromInfo(info Info, fallback string) string {
	if ancestor := strings.TrimSpace(info.BuiltinAncestor); ancestor != "" {
		return sessionlog.ProviderFamily(ancestor)
	}
	if kind := strings.TrimSpace(info.ProviderKind); kind != "" {
		return sessionlog.ProviderFamily(kind)
	}
	if provider := strings.TrimSpace(info.Provider); provider != "" {
		return sessionlog.ProviderFamily(provider)
	}
	return sessionlog.ProviderFamily(fallback)
}

func wrappedProviderFamily(b beads.Bead, family string) bool {
	ancestor := sessionlog.ProviderFamily(b.Metadata["builtin_ancestor"])
	// Leave provider raw: normalizing it would collapse wrapped aliases such as
	// "my-pi" onto their family and hide that the provider is wrapped.
	provider := strings.TrimSpace(b.Metadata["provider"])
	return ancestor == family && provider != "" && provider != family
}

func usesSoftEscapeInterrupt(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	if wrappedProviderFamily(b, "gemini") {
		return true
	}
	switch providerKind(b) {
	case "codex", "kimi":
		return true
	default:
		return false
	}
}

func waitsForIdleAfterInterrupt(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	if wrappedProviderFamily(b, "codex") || wrappedProviderFamily(b, "gemini") {
		return false
	}
	switch providerKind(b) {
	case "claude", "codex", "gemini", "kimi":
		return true
	default:
		return false
	}
}

func shouldClearInterruptedInputBeforeSubmit(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	switch providerKind(b) {
	case "claude", "gemini":
		return true
	default:
		return false
	}
}

func (m *Manager) resetInterruptedTurnLocked(ctx context.Context, b beads.Bead, sessName string) error {
	if !requiresInterruptedTurnReset(b) {
		return nil
	}
	resetter, ok := m.sp.(runtime.InterruptedTurnResetProvider)
	if !ok {
		return nil
	}
	if err := resetter.ResetInterruptedTurn(ctx, sessName); err != nil && !errors.Is(err, runtime.ErrInteractionUnsupported) {
		return fmt.Errorf("discarding interrupted turn: %w", err)
	}
	return nil
}

// claudeInputRestoreWindow is how long StopTurn and interrupt_now give Claude
// Code to put an interrupted prompt back into its input box before clearing
// it. In the lost-turn repro (Claude Code 2.1.288) the prompt reappeared
// within about 0.3 s of the idle prompt.
const claudeInputRestoreWindow = 2 * time.Second

// restoresInterruptedInput reports whether the provider puts a prompt that was
// interrupted before its first response chunk back into its input box. Claude
// Code does: the next message is appended to it unless the input box is
// cleared first.
func restoresInterruptedInput(b beads.Bead) bool {
	return transportFromMetadata(b) != "acp" && providerKind(b) == "claude"
}

// clearRestoredInputLocked waits briefly for Claude to restore an interrupted
// prompt, then clears the input box and verifies it is empty. handled is false
// when the runtime cannot do a verified clear. When a client is attached the
// runtime leaves the input box alone (a human may be typing in it, #5192);
// that is logged and handled, so no caller falls back to a raw Ctrl-U.
func (m *Manager) clearRestoredInputLocked(ctx context.Context, sessName string) (handled bool, err error) {
	clearer, ok := m.sp.(runtime.InputClearProvider)
	if !ok {
		return false, nil
	}
	err = clearer.ClearInput(ctx, sessName, claudeInputRestoreWindow)
	if errors.Is(err, runtime.ErrInteractionUnsupported) {
		return false, nil
	}
	if errors.Is(err, runtime.ErrInputClearSkipped) {
		log.Printf("session: left the input box of %q as it is after an interrupt: %v", sessName, err)
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("clearing the interrupted prompt from the input box: %w", err)
	}
	return true, nil
}

func (m *Manager) clearInterruptedInputLocked(ctx context.Context, b beads.Bead, sessName string) error {
	if restoresInterruptedInput(b) {
		handled, err := m.clearRestoredInputLocked(ctx, sessName)
		if err != nil || handled {
			return err
		}
	}
	if err := runtime.SendKeysWithContext(ctx, m.sp, sessName, "C-u"); err != nil {
		return fmt.Errorf("clearing interrupted input: %w", err)
	}
	if err := sleepWithContext(ctx, interruptClearDelay); err != nil {
		return err
	}
	return nil
}

func requiresImmediateInterruptConfirm(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	switch providerKind(b) {
	case "gemini":
		return true
	default:
		return false
	}
}

func requiresInterruptedTurnReset(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	return providerKind(b) == "gemini"
}

func requiresInterruptBoundaryWait(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	return providerKind(b) == "codex"
}

// stopsBackgroundWork reports whether an explicit stop must also stop work
// the provider keeps running after its turn is interrupted. Codex does: it
// runs a turn's commands in background terminals that outlive the turn.
// Claude does too: a command the model ran in the background outlives the
// turn, and its completion notice starts another one.
func stopsBackgroundWork(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	switch providerKind(b) {
	case "codex", "claude":
		return true
	}
	return false
}

// stopBackgroundWorkLocked has the runtime stop the work a stopped turn left
// running, when the runtime can (see [runtime.BackgroundWorkStopper]). Only
// StopTurn calls it: an interrupt_now submit replaces the turn and leaves
// earlier work, such as a dev server, running. The turn is already stopped,
// so a failure is logged, not returned.
func (m *Manager) stopBackgroundWorkLocked(ctx context.Context, sessName string) {
	stopper, ok := m.sp.(runtime.BackgroundWorkStopper)
	if !ok {
		return
	}
	if err := stopper.StopBackgroundWork(ctx, sessName); err != nil && !errors.Is(err, runtime.ErrInteractionUnsupported) {
		log.Printf("session: stopped the turn of %q but not its background work: %v", sessName, err)
	}
}

func requiresHardRestartInterrupt(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	return providerKind(b) == "pi"
}

func waitsForIdleAfterHardRestart(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	return providerKind(b) != "pi"
}

// restoreAfterHardRestartFailureLocked restarts the runtime the interrupt
// stopped; like restartAndSendLocked it does not re-read the hold.
func (m *Manager) restoreAfterHardRestartFailureLocked(ctx context.Context, by Actor, id string, b beads.Bead, sessName, resumeCommand string, hints runtime.Config) error {
	if err := m.ensureRunning(ctx, by, id, b, sessName, resumeCommand, hints, true); err != nil {
		return err
	}
	return m.waitUntilRunningLocked(ctx, id, sessName, 2*time.Second)
}

func piPendingTurnPath(b beads.Bead, hints runtime.Config) (string, error) {
	searchPaths := piSessionSearchPaths(hints)
	workDir := b.Metadata["work_dir"]
	if sessionKey := strings.TrimSpace(b.Metadata["session_key"]); sessionKey != "" {
		if path := strings.TrimSpace(sessionlog.FindPiSessionFileByID(searchPaths, workDir, sessionKey)); path != "" {
			return path, nil
		}
		fallback, err := sessionlog.FindPiSessionFileStrict(searchPaths, workDir)
		if err != nil {
			if errors.Is(err, sessionlog.ErrAmbiguousPiSessionFile) {
				return "", fmt.Errorf("resolving pi interrupted transcript for session_key %q workdir %q: %w", sessionKey, workDir, err)
			}
			return "", err
		}
		if fallback != "" {
			return "", fmt.Errorf("resolving pi interrupted transcript for session_key %q workdir %q: keyed transcript not found; refusing same-workdir fallback %q", sessionKey, workDir, fallback)
		}
		return "", nil
	}
	path, err := sessionlog.FindPiSessionFileStrict(searchPaths, workDir)
	if err != nil {
		if errors.Is(err, sessionlog.ErrAmbiguousPiSessionFile) {
			return "", fmt.Errorf("resolving pi interrupted transcript for workdir %q: %w", workDir, err)
		}
		return "", err
	}
	return strings.TrimSpace(path), nil
}

func discardPiPendingTurn(path string, hints runtime.Config) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := sessionlog.ResetPiInterruptedTurn(path, strings.TrimSpace(hints.Env["GC_PI_TRANSCRIPT_DIR"])); err != nil {
		return fmt.Errorf("discarding pi interrupted turn: %w", err)
	}
	return nil
}

func piSessionSearchPaths(hints runtime.Config) []string {
	var paths []string
	if dir := strings.TrimSpace(hints.Env["PI_CODING_AGENT_SESSION_DIR"]); dir != "" {
		paths = append(paths, dir)
	}
	if dir := strings.TrimSpace(hints.Env["PI_CODING_AGENT_DIR"]); dir != "" {
		paths = append(paths, filepath.Join(dir, "sessions"))
	}
	paths = append(paths, sessionlog.DefaultPiSearchPaths()...)
	return paths
}

func usesImmediateDefaultSubmit(b beads.Bead, resuming ...bool) bool {
	isResuming := len(resuming) > 0 && resuming[0]
	if transportFromMetadata(b) == "acp" {
		return false
	}
	switch providerKind(b) {
	case "codex":
		return true
	case "gemini":
		return isResuming
	default:
		return false
	}
}

func needsDeferredStartupDialogVerification(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	if providerKind(b) != "codex" {
		return false
	}
	return strings.TrimSpace(b.Metadata[startupDialogVerifiedKey]) != "true"
}

// deferredNudgeSource is the Source of a nudge sent through Send that the
// session queued: it is delivered later as a deferred reminder. A user's
// submit is queued as nudgequeue.SourceSubmit instead, and delivered as the
// user's prompt.
const deferredNudgeSource = "session"

// enqueueDeferredSubmitLocked queues message for sessName's next delivery
// boundary under source (nudgequeue.SourceSubmit for a user's submit,
// deferredNudgeSource for a nudge).
func (m *Manager) enqueueDeferredSubmitLocked(b beads.Bead, sessName, message, source string) error {
	queue := m.deferredQueue()
	if queue == nil {
		return errors.New("deferred submit is unavailable without a city path")
	}
	now := time.Now().UTC()
	item := nudgequeue.Item{
		ID:                "nudge-" + NewInstanceToken()[:12],
		Agent:             deferredSubmitAgentKey(b),
		SessionID:         b.ID,
		ContinuationEpoch: deferredSubmitEpoch(b),
		Source:            source,
		Message:           message,
		CreatedAt:         now,
		DeliverAfter:      now,
		ExpiresAt:         now.Add(defaultQueuedSubmitTTL),
	}
	if err := queue.EnqueueDeferred(item); err != nil {
		return fmt.Errorf("queueing deferred submit: %w", err)
	}
	if m.supportsFollowUpLocked(b) {
		// The item is queued either way: the supervisor's dispatcher also
		// delivers it. A poller that did not start only loses the fast path,
		// so say so instead of dropping the error.
		if err := startSessionSubmitPoller(m.cityPath, deferredSubmitPollerKey(b), sessName); err != nil {
			log.Printf("session: %q: queued a deferred submit but could not start its poller: %v", sessName, err)
		}
	}
	return nil
}

// deferredSubmitEpoch returns the continuation epoch a deferred submit is
// fenced against.
//
// A submit deferred while a conversation reset is pending is fenced by its
// session alone (an empty epoch): commitPendingContinuationReset advances
// continuation_epoch N->N+1 when the replacement incarnation starts, after
// this item is queued. Any other defer keeps the current epoch's stamp. An
// epoch rotated since (a reset, or a crash heal's) does not lose the item:
// since #5775 delivery re-stamps an item for the same session with the live
// epoch (splitQueuedNudgesForTarget) and delivers it, so the stamp records
// the conversation the message was written for, not a gate on delivery.
func deferredSubmitEpoch(b beads.Bead) string {
	if strings.TrimSpace(b.Metadata["continuation_reset_pending"]) != "" {
		return ""
	}
	return strings.TrimSpace(b.Metadata["continuation_epoch"])
}

func deferredSubmitAgentKey(b beads.Bead) string {
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
		return alias
	}
	if b.ID != "" {
		return b.ID
	}
	if template := strings.TrimSpace(b.Metadata["template"]); template != "" {
		return template
	}
	if sessName := strings.TrimSpace(b.Metadata["session_name"]); sessName != "" {
		return sessName
	}
	return b.Title
}

func deferredSubmitPollerKey(b beads.Bead) string {
	return PollerKeyFromBead(b)
}

var (
	startSessionSubmitPoller      = ensureSessionSubmitPoller
	sessionSubmitPollerExecutable = os.Executable
)

func ensureSessionSubmitPoller(cityPath, agentName, sessionName string) error {
	pidPath := sessionSubmitPollerPIDPath(cityPath, sessionName, agentName)
	return withSessionSubmitPollerPIDLock(pidPath, func() error {
		if running, _ := existingSessionSubmitPollerPID(pidPath, cityPath, sessionName, agentName); running {
			return nil
		}
		exe, err := sessionSubmitPollerExecutable()
		if err != nil {
			return err
		}
		if isGoTestExecutable(exe) {
			return fmt.Errorf("refusing to start nudge poller with Go test binary %q", exe)
		}
		cmd := newSessionSubmitPollerCommand(exe, os.Environ(), cityPath, sessionName, agentName)
		logFile, err := os.OpenFile(sessionSubmitPollerLogPath(cityPath, sessionName, agentName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer logFile.Close() //nolint:errcheck
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		if err := writeSessionSubmitPollerPID(pidPath, cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Process.Release()
			return err
		}
		return cmd.Process.Release()
	})
}

// newSessionSubmitPollerCommand builds the detached poller child over
// environ, without the spawning session's identity (nudgepoller.ChildEnv),
// so the poller never reads as that session's leaked process.
func newSessionSubmitPollerCommand(exe string, environ []string, cityPath, sessionName, agentName string) *exec.Cmd {
	cmd := exec.Command(exe, nudgepoller.CommandArgs(cityPath, sessionName, agentName)...)
	cmd.Env = execenv.WithUsageMetricsDisabled(nudgepoller.ChildEnv(environ))
	return cmd
}

func isGoTestExecutable(path string) bool {
	argv0 := ""
	if len(os.Args) > 0 {
		argv0 = os.Args[0]
	}
	return isGoTestExecutableFor(path, argv0, os.Getenv("TEST_SRCDIR"))
}

// isGoTestExecutableFor reports whether path names a Go test binary, given the
// running process's argv0 and TEST_SRCDIR.
func isGoTestExecutableFor(path, argv0, testSrcdir string) bool {
	// The running bazel test binary drops the .test suffix but keeps the
	// <target>_test name and exports TEST_SRCDIR; probes that exec a renamed
	// spy binary must not match. Sibling gates such as internal/testenv and
	// cmd/gc/test_guard.go need no symlink comparison: they classify only the
	// running process, by an argv0 basename that is <target>_test whether or
	// not bazel exec'd it through a symlink.
	if testSrcdir != "" && strings.HasSuffix(filepath.Base(path), "_test") && isRunningBinary(path, argv0) {
		return true
	}
	return strings.HasSuffix(filepath.Base(path), ".test")
}

// isRunningBinary reports whether path, as reported by os.Executable, is the
// binary argv0 names. Bazel execs a test through its runfiles path, which is a
// symlink under local execution; on Linux os.Executable reads /proc/self/exe,
// which names the symlink's resolved target, so argv0 is also compared after
// resolving symlinks.
func isRunningBinary(path, argv0 string) bool {
	if path == argv0 {
		return true
	}
	resolved, err := filepath.EvalSymlinks(argv0)
	return err == nil && resolved == path
}

func sessionSubmitPollerPIDPath(cityPath, sessionName, agentName string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "pollers", nudgepoller.PollerFileStem(sessionName, agentName)+".pid")
}

func sessionSubmitPollerLogPath(cityPath, sessionName, agentName string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "pollers", nudgepoller.PollerFileStem(sessionName, agentName)+".log")
}

func existingSessionSubmitPollerPID(pidPath, cityPath, sessionName, agentName string) (bool, error) {
	data, err := os.ReadFile(pidPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pidText := strings.TrimSpace(string(data))
	if pidText == "" {
		return false, nil
	}
	var pid int
	if _, err := fmt.Sscanf(pidText, "%d", &pid); err != nil || pid <= 0 {
		return false, nil
	}
	if cityPath == "" || sessionName == "" || agentName == "" {
		return false, nil
	}
	if pidutil.AliveWithCmdline(pid, nudgepoller.CmdlineMatcher(cityPath, sessionName, agentName)) {
		return true, nil
	}
	return false, nil
}

func writeSessionSubmitPollerPID(pidPath string, pid int) error {
	data := []byte(fmt.Sprintf("%d\n", pid))
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, pidPath, data, 0o644); err != nil {
		return fmt.Errorf("write nudge poller pid: %w", err)
	}
	return nil
}

func withSessionSubmitPollerPIDLock(pidPath string, fn func() error) error {
	lockPath := pidPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return fmt.Errorf("creating nudge poller dir: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening nudge poller lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking nudge poller: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}
