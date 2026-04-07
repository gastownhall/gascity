package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	defaultQueuedSubmitTTL = 24 * time.Hour
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
}

// SubmitOutcome reports whether a submit was delivered now or queued.
type SubmitOutcome struct {
	Queued bool
}

// SubmissionCapabilitiesForMetadata derives runtime submit affordances from
// persisted session metadata and whether deferred queueing is available.
func SubmissionCapabilitiesForMetadata(metadata map[string]string, hasDeferredQueue bool) SubmissionCapabilities {
	poolManaged := metadata["pool_managed"] == "true" || strings.TrimSpace(metadata["pool_slot"]) != ""
	return SubmissionCapabilities{
		SupportsFollowUp:     hasDeferredQueue && !poolManaged && transportFromMetadata(beads.Bead{Metadata: metadata}) != "acp",
		SupportsInterruptNow: !poolManaged,
	}
}

// SubmissionCapabilities reports which semantic submit intents the session can
// currently support.
func (m *Manager) SubmissionCapabilities(id string) (SubmissionCapabilities, error) {
	b, _, err := m.loadSessionBead(id, true)
	if err != nil {
		return SubmissionCapabilities{}, err
	}
	return SubmissionCapabilitiesForMetadata(b.Metadata, strings.TrimSpace(m.cityPath) != ""), nil
}

// Submit delivers a user message according to the requested semantic intent.
func (m *Manager) Submit(ctx context.Context, id, message, resumeCommand string, hints runtime.Config, intent SubmitIntent) (SubmitOutcome, error) {
	switch intent {
	case "", SubmitIntentDefault, SubmitIntentFollowUp, SubmitIntentInterruptNow:
	default:
		return SubmitOutcome{}, fmt.Errorf("invalid submit intent %q", intent)
	}
	return m.submit(ctx, id, message, resumeCommand, hints, intent)
}

func (m *Manager) submit(ctx context.Context, id, message, resumeCommand string, hints runtime.Config, intent SubmitIntent) (SubmitOutcome, error) {
	var outcome SubmitOutcome
	err := withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		switch intent {
		case SubmitIntentFollowUp:
			if err := m.pendingInteractionLocked(sessName); err != nil {
				return err
			}
			if !m.supportsFollowUpLocked(b) {
				return ErrInteractionUnsupported
			}
			if State(b.Metadata["state"]) == StateSuspended || !m.sp.IsRunning(sessName) {
				return m.sendLocked(ctx, id, b, sessName, message, resumeCommand, hints, true)
			}
			if err := m.enqueueDeferredSubmitLocked(b, sessName, message); err != nil {
				return err
			}
			outcome.Queued = true
			return nil
		case SubmitIntentInterruptNow:
			return m.interruptAndSubmitLocked(ctx, id, b, sessName, message, resumeCommand, hints)
		default:
			return m.sendLocked(ctx, id, b, sessName, message, resumeCommand, hints, usesImmediateDefaultSubmit(b))
		}
	})
	return outcome, err
}

func (m *Manager) supportsFollowUpLocked(b beads.Bead) bool {
	return SubmissionCapabilitiesForMetadata(
		b.Metadata,
		strings.TrimSpace(m.cityPath) != "",
	).SupportsFollowUp
}

func (m *Manager) interruptAndSubmitLocked(ctx context.Context, id string, b beads.Bead, sessName, message, resumeCommand string, hints runtime.Config) error {
	running := State(b.Metadata["state"]) != StateSuspended && m.sp.IsRunning(sessName)
	if !running {
		return m.sendLocked(ctx, id, b, sessName, message, resumeCommand, hints, true)
	}
	if err := m.stopTurnLocked(b, sessName); err != nil {
		return err
	}
	if usesHardRestartSubmit(b) {
		if err := m.sp.Stop(sessName); err != nil {
			return fmt.Errorf("stopping session before submit: %w", err)
		}
	}
	return m.sendLocked(ctx, id, b, sessName, message, resumeCommand, hints, true)
}

func (m *Manager) stopTurnLocked(b beads.Bead, sessName string) error {
	if State(b.Metadata["state"]) == StateSuspended || !m.sp.IsRunning(sessName) {
		return nil
	}
	if b.Metadata["pool_managed"] == "true" || strings.TrimSpace(b.Metadata["pool_slot"]) != "" {
		return fmt.Errorf("%w: %s", ErrPoolManaged, sessName)
	}
	if usesSoftEscapeInterrupt(b) {
		if err := m.sp.SendKeys(sessName, "Escape"); err != nil {
			return fmt.Errorf("interrupting session: %w", err)
		}
		return nil
	}
	if err := m.sp.Interrupt(sessName); err != nil {
		return fmt.Errorf("interrupting session: %w", err)
	}
	return nil
}

func usesSoftEscapeInterrupt(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	switch strings.TrimSpace(b.Metadata["provider"]) {
	case "codex", "gemini":
		return true
	default:
		return false
	}
}

func usesHardRestartSubmit(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	return strings.TrimSpace(b.Metadata["provider"]) == "claude"
}

func usesImmediateDefaultSubmit(b beads.Bead) bool {
	if transportFromMetadata(b) == "acp" {
		return false
	}
	switch strings.TrimSpace(b.Metadata["provider"]) {
	case "codex", "gemini":
		return true
	default:
		return false
	}
}

func (m *Manager) enqueueDeferredSubmitLocked(b beads.Bead, sessName, message string) error {
	if strings.TrimSpace(m.cityPath) == "" {
		return errors.New("deferred submit is unavailable without a city path")
	}
	now := time.Now().UTC()
	item := nudgequeue.Item{
		ID:                "nudge-" + NewInstanceToken()[:12],
		Agent:             deferredSubmitAgentKey(b),
		SessionID:         b.ID,
		ContinuationEpoch: strings.TrimSpace(b.Metadata["continuation_epoch"]),
		Source:            "session",
		Message:           message,
		CreatedAt:         now,
		DeliverAfter:      now,
		ExpiresAt:         now.Add(defaultQueuedSubmitTTL),
	}
	if err := nudgequeue.WithState(m.cityPath, func(state *nudgequeue.State) error {
		state.Pending = append(state.Pending, item)
		nudgequeue.SortState(state)
		return nil
	}); err != nil {
		return fmt.Errorf("queueing deferred submit: %w", err)
	}
	if m.supportsFollowUpLocked(b) {
		_ = startSessionSubmitPoller(m.cityPath, deferredSubmitAgentKey(b), sessName)
	}
	return nil
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

var startSessionSubmitPoller = ensureSessionSubmitPoller

func ensureSessionSubmitPoller(cityPath, agentName, sessionName string) error {
	pidPath := sessionSubmitPollerPIDPath(cityPath, sessionName)
	return withSessionSubmitPollerPIDLock(pidPath, func() error {
		if running, _ := existingSessionSubmitPollerPID(pidPath); running {
			return nil
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "nudge", "poll", "--city", cityPath, "--session", sessionName, agentName)
		cmd.Env = os.Environ()
		logFile, err := os.OpenFile(sessionSubmitPollerLogPath(cityPath, sessionName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
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

func sessionSubmitPollerPIDPath(cityPath, sessionName string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "pollers", sessionName+".pid")
}

func sessionSubmitPollerLogPath(cityPath, sessionName string) string {
	return citylayout.RuntimePath(cityPath, "nudges", "pollers", sessionName+".log")
}

func existingSessionSubmitPollerPID(pidPath string) (bool, error) {
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
	if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
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
