package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"io"
)

type noticeDefinition struct {
	testOnly bool
	version  uint64
	text     []byte
}

// activationBasis is the exact persisted consent record an activation call
// observed before it began waiting for state.lock. Keeping the complete basis
// private makes the generation check resistant to a same-generation state
// replacement while avoiding any mutation token in user-visible status.
type activationBasis struct {
	present bool
	state   persistedState
	lease   *storageRecordLease
}

func activationBasisFrom(loaded loadedState) activationBasis {
	return activationBasis{present: loaded.present, state: loaded.state, lease: loaded.lease}
}

func (basis activationBasis) matches(loaded loadedState) bool {
	return loaded.err == nil && loaded.present == basis.present &&
		(!loaded.present || loaded.state == basis.state && basis.lease.Matches(loaded.lease))
}

// NoticeOutcome is the closed result of an automatic notice attempt.
type NoticeOutcome string

const (
	// NoticeNotNeeded means another state or invocation gate suppressed notice.
	NoticeNotNeeded NoticeOutcome = "not-needed"
	// NoticeActivated means the complete notice was written and one whole
	// enabled identity record became the logical committed state.
	NoticeActivated NoticeOutcome = "activated"
	// NoticeFailed means no logical activation occurred.
	NoticeFailed NoticeOutcome = "failed"
)

// NoticeResult reports a bounded outcome. Err is non-nil only when activation
// failed; ordinary callers keep it isolated from command output.
type NoticeResult struct {
	Outcome NoticeOutcome
	Err     error
}

// MaybeActivateNotice prints and accepts the notice only for an eligible TTY
// invocation whose state is pending or stale. Its caller must have captured
// the sticky recording permit before invoking it.
func (service *Service) MaybeActivateNotice(invocation InvocationContext, writer io.Writer) NoticeResult {
	if !service.noticeInvocationEligible(invocation, writer) {
		return NoticeResult{Outcome: NoticeNotNeeded}
	}
	loaded := service.readStateReadOnly()
	defer func() { _ = loaded.Close() }()
	projection := service.project(invocation, loaded)
	if projection.state != StatePendingNotice && projection.state != StateNoticeUpdateRequired {
		return NoticeResult{Outcome: NoticeNotNeeded}
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateLockTimeout)
	defer cancel()
	if projection.state == StateNoticeUpdateRequired {
		projection, err := service.rebaseStaleNoticeForActivation(ctx, invocation, &loaded)
		if err != nil {
			return NoticeResult{Outcome: NoticeFailed, Err: err}
		}
		if projection.state == StateEnabled || projection.state != StateNoticeUpdateRequired {
			return NoticeResult{Outcome: NoticeNotNeeded}
		}
	}
	activated, err := service.activateNotice(ctx, invocation, writer, false, activationBasisFrom(loaded))
	if err != nil {
		return NoticeResult{Outcome: NoticeFailed, Err: err}
	}
	if !activated {
		return NoticeResult{Outcome: NoticeNotNeeded}
	}
	return NoticeResult{Outcome: NoticeActivated}
}

// Enable explicitly accepts the notice for a verified human TTY. It is
// idempotent while already enabled and never rotates an existing identity.
func (service *Service) Enable(ctx context.Context, invocation InvocationContext, writer io.Writer) error {
	if ctx == nil {
		return errors.New("productmetrics: enable context is nil")
	}
	if !service.noticeInvocationEligible(invocation, writer) {
		return errors.New("productmetrics: enable requires an eligible verified TTY notice writer")
	}
	loaded := service.readStateReadOnly()
	defer func() { _ = loaded.Close() }()
	projection := service.project(invocation, loaded)
	switch projection.state {
	case StateEnabled:
		return nil
	case StatePendingNotice, StateNoticeUpdateRequired, StateDisabled:
		// Continue under state.lock and re-evaluate the exact record.
	default:
		return fmt.Errorf("productmetrics: enable is blocked by %s", projection.reason)
	}
	if projection.state == StateNoticeUpdateRequired {
		projection, err := service.rebaseStaleNoticeForActivation(ctx, invocation, &loaded)
		if err != nil {
			return err
		}
		switch projection.state {
		case StateEnabled:
			return nil
		case StateNoticeUpdateRequired:
			// Continue from the exact invalidated record.
		default:
			return ErrStateChangedConcurrently
		}
	}
	_, err := service.activateNotice(ctx, invocation, writer, true, activationBasisFrom(loaded))
	return err
}

// rebaseStaleNoticeForActivation installs the monotonic notice floor before
// notice output, entropy, or acceptance. It then replaces the caller's basis
// with the exact post-barrier record for the second activation phase.
func (service *Service) rebaseStaleNoticeForActivation(ctx context.Context, invocation InvocationContext, loaded *loadedState) (stateProjection, error) {
	if loaded == nil {
		return stateProjection{StateFailClosed, ReasonConfigUnreadable}, errors.New("productmetrics: stale-notice basis is nil")
	}
	err := service.invalidateNotice(ctx, stateVersionFromLoaded(*loaded))
	_ = loaded.Close()
	*loaded = service.readStateReadOnly()
	projection := service.project(invocation, *loaded)
	if err != nil {
		return projection, err
	}
	if loaded.err != nil {
		return projection, loaded.err
	}
	return projection, nil
}

func (service *Service) noticeInvocationEligible(invocation InvocationContext, writer io.Writer) bool {
	return invocation.NoticeEligible && !invocation.ManagedAutomation && writer != nil && service.deps.verifyTTY(writer) &&
		!doNotTrackTruthy(invocation.DoNotTrack) && !gcDisableTruthy(invocation.DisableUsageMetrics)
}

func (service *Service) activateNotice(ctx context.Context, invocation InvocationContext, writer io.Writer, explicit bool, basis activationBasis) (bool, error) {
	if service.deps.homeErr != nil {
		return false, service.deps.homeErr
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, service.deps.storageHooks)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	lock, err := root.acquireLock(ctx, stateLockName)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Release() }()

	loaded := loadStateFromDirectory(root)
	defer func() { _ = loaded.Close() }()
	if loaded.err == nil && loaded.present && loaded.state.CounterNamespace == terminalCounterNamespace {
		return false, errors.New("productmetrics: counter namespace exhausted")
	}
	if loaded.err == nil && loaded.present && loaded.state.Preference == preferenceEnabled {
		if loaded.state.CleanupKind != cleanupNone {
			return false, errors.New("productmetrics: notice activation is blocked by cleanup")
		}
		if loaded.state.PausedThroughMetricsEpoch >= service.deps.release.metricsEpoch {
			return false, errors.New("productmetrics: notice activation is blocked by server pause")
		}
	}
	projection := service.project(invocation, loaded)
	if projection.state == StateEnabled {
		return false, nil
	}
	if !basis.matches(loaded) {
		return false, ErrStateChangedConcurrently
	}
	allowed := projection.state == StatePendingNotice || projection.state == StateNoticeUpdateRequired
	if explicit && projection.state == StateDisabled {
		allowed = true
	}
	if !allowed {
		return false, fmt.Errorf("productmetrics: notice activation is blocked by %s", projection.reason)
	}
	if len(service.deps.notice.text) == 0 || service.deps.notice.version == 0 || !service.deps.notice.testOnly {
		return false, errors.New("productmetrics: no approved notice is compiled")
	}
	written, writeErr := writer.Write(service.deps.notice.text)
	if writeErr != nil {
		return false, fmt.Errorf("productmetrics: write complete notice: %w", writeErr)
	}
	if written != len(service.deps.notice.text) {
		return false, fmt.Errorf("productmetrics: write complete notice: %w", io.ErrShortWrite)
	}

	state := persistedState{
		StateSchema:           currentStateSchema,
		CounterNamespace:      initialCounterNamespace,
		Preference:            preferenceUnset,
		RequiredNoticeVersion: service.deps.notice.version,
		CleanupKind:           cleanupNone,
	}
	if loaded.present {
		state = loaded.state
	}
	installationID := state.InstallationID
	if state.Preference != preferenceEnabled || installationID == "" {
		installationID, err = service.deps.newUUID()
		if err != nil {
			return false, fmt.Errorf("productmetrics: generate installation ID: %w", err)
		}
		if err := validateCanonicalUUIDv4(installationID); err != nil {
			return false, fmt.Errorf("productmetrics: generated installation ID: %w", err)
		}
	}
	spoolGeneration, err := service.deps.newUUID()
	if err != nil {
		return false, fmt.Errorf("productmetrics: generate spool generation: %w", err)
	}
	if err := validateCanonicalUUIDv4(spoolGeneration); err != nil {
		return false, fmt.Errorf("productmetrics: generated spool generation: %w", err)
	}

	state.Preference = preferenceEnabled
	state.RequiredNoticeVersion = service.deps.notice.version
	state.AcceptedNoticeVersion = service.deps.notice.version
	state.InstallationID = installationID
	state.SpoolGeneration = spoolGeneration
	state.CleanupKind = cleanupNone
	if state.StateGeneration == 0 {
		state.StateGeneration = 1
	} else if err := incrementStateGeneration(&state); err != nil {
		return false, err
	}
	persisted, err := persistStateMutation(root, state, true)
	_ = persisted.Close()
	if err != nil {
		return false, err
	}
	return true, nil
}
