package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

const defaultDisableUploaderWait = 12 * time.Second

// PurgeOutcome is the closed result of a DisableAndPurge attempt.
type PurgeOutcome string

const (
	// PurgeCompleted means this call durably disabled collection and proved the
	// local metrics tree clean after crossing the uploader barrier.
	PurgeCompleted PurgeOutcome = "completed"
	// PurgeAlreadyDisabled means the call began from clean disabled state but
	// still installed cleanup ownership and crossed the uploader barrier.
	PurgeAlreadyDisabled PurgeOutcome = "already-disabled"
	// PurgeCleanupPending means quiescence or exact local cleanup was not proven;
	// DisabledDurable distinguishes a proven opt-out from sync uncertainty.
	PurgeCleanupPending PurgeOutcome = "cleanup-pending"
	// PurgeFailed means durable opt-out itself or the final barrier failed.
	PurgeFailed PurgeOutcome = "failed"
)

// PurgeIncompletePhase is the closed stage at which an opt-out attempt could
// not finish. The zero value means no incomplete work was reported.
type PurgeIncompletePhase string

// Purge incomplete phases identify the bounded stage that did not finish.
const (
	PurgeIncompleteNone               PurgeIncompletePhase = ""
	PurgeIncompleteDisableWrite       PurgeIncompletePhase = "disable-write"
	PurgeIncompleteUploaderQuiescence PurgeIncompletePhase = "uploader-quiescence"
	PurgeIncompleteLocalCleanup       PurgeIncompletePhase = "local-cleanup"
	PurgeIncompleteFinalProof         PurgeIncompletePhase = "final-proof"
)

// PurgeManualCleanupReason is the closed, path-free reason that same-UID
// manual inspection/removal is required before a later opt-out can finish.
type PurgeManualCleanupReason string

// Purge manual-cleanup reasons describe preserved local residue without paths.
const (
	PurgeManualCleanupNone                     PurgeManualCleanupReason = ""
	PurgeManualCleanupUnsettledRootTempJournal PurgeManualCleanupReason = "unsettled-root-temp-journal"
	PurgeManualCleanupUnrecognizedRootEntry    PurgeManualCleanupReason = "unrecognized-root-entry"
)

// PurgeResult contains only bounded aggregate facts about local cleanup.
type PurgeResult struct {
	Outcome               PurgeOutcome
	RemovedEvents         uint64
	RemovedBytes          uint64
	RecoveredState        bool
	DisabledDurable       bool
	IncompletePhase       PurgeIncompletePhase
	ManualCleanupRequired bool
	ManualCleanupReason   PurgeManualCleanupReason
}

// PurgeErrorClass is a closed, path-free failure classification.
type PurgeErrorClass string

const (
	// PurgeErrorInvalidRequest identifies a nil service or context.
	PurgeErrorInvalidRequest PurgeErrorClass = "invalid-request"
	// PurgeErrorDisableWrite identifies failure before durable opt-out is proven.
	PurgeErrorDisableWrite PurgeErrorClass = "disable-write-failed"
	// PurgeErrorUploaderQuiescence identifies the bounded uploader-lock timeout.
	PurgeErrorUploaderQuiescence PurgeErrorClass = "uploader-quiescence-timeout"
	// PurgeErrorCleanupIncomplete identifies bounded local cleanup without proof.
	PurgeErrorCleanupIncomplete PurgeErrorClass = "cleanup-incomplete"
	// PurgeErrorStateChanged identifies a lost exact-record comparison.
	PurgeErrorStateChanged PurgeErrorClass = "state-changed-concurrently"
	// PurgeErrorStorage identifies a bounded filesystem or lock failure.
	PurgeErrorStorage PurgeErrorClass = "storage-failure"
)

// PurgeError exposes a bounded class while retaining its cause for errors.Is.
type PurgeError struct {
	Class PurgeErrorClass
	cause error
}

// Error returns only the bounded public class and never a filesystem path.
func (err *PurgeError) Error() string {
	if err == nil {
		return "productmetrics: purge failed"
	}
	return fmt.Sprintf("productmetrics: %s", err.Class)
}

// Unwrap retains the private cause for programmatic errors.Is checks.
func (err *PurgeError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func newPurgeError(class PurgeErrorClass, cause error) error {
	if cause == nil {
		cause = errors.New("productmetrics: purge operation failed")
	}
	return &PurgeError{Class: class, cause: cause}
}

type controlCloseTarget string

const (
	controlCloseFinalState controlCloseTarget = "final-state-lock"
	controlCloseUploader   controlCloseTarget = "uploader-lock"
	controlCloseRoot       controlCloseTarget = "storage-root"
)

// DisableAndPurge durably opts out, crosses the uploader barrier, and proves
// exact local cleanup before returning a successful result.
func (service *Service) DisableAndPurge(ctx context.Context) (result PurgeResult, returnErr error) {
	result.Outcome = PurgeFailed
	incompletePhase := PurgeIncompleteNone
	defer func() {
		if returnErr != nil {
			markPurgeIncomplete(&result, incompletePhase, returnErr)
		}
	}()
	if service == nil || ctx == nil {
		return result, newPurgeError(PurgeErrorInvalidRequest, errors.New("productmetrics: invalid disable-and-purge request"))
	}
	incompletePhase = PurgeIncompleteDisableWrite
	if service.deps.homeErr != nil {
		return result, newPurgeError(PurgeErrorStorage, service.deps.homeErr)
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, service.deps.storageHooks)
	if err != nil {
		return result, newPurgeError(PurgeErrorStorage, err)
	}
	defer func() {
		closeErr := errors.Join(root.Close(), service.controlCloseFailure(controlCloseRoot))
		if closeErr != nil {
			markPurgeCloseFailure(&result, &returnErr, closeErr)
		}
	}()
	observed := loadStateFromDirectory(root)
	if observed.err != nil && (!observed.present || observed.lease == nil) {
		return result, newPurgeError(PurgeErrorStorage, errors.Join(observed.err, observed.Close()))
	}
	alreadyDisabled := observed.err == nil && observed.present && cleanDisabledState(observed.state)
	recoveringState := observed.present && observed.err != nil
	expected := stateVersionFromLoaded(observed)
	pendingBasis := cleanupToken{}
	if observed.err == nil && observed.present && observed.lease != nil &&
		observed.state.Preference == preferenceDisabled && observed.state.CleanupKind == cleanupDisable {
		pendingBasis = cleanupTokenFromLoaded(&observed)
	}
	stateWait := service.deps.disableStateWait
	if stateWait <= 0 {
		stateWait = stateLockTimeout
	}
	disableContext, cancelDisable := context.WithTimeout(ctx, stateWait)
	token, disableErr := service.beginDisableAtRoot(disableContext, expected, root)
	cancelDisable()
	observedCloseErr := observed.Close()
	pendingFallback := false
	if disableErr != nil {
		if errors.Is(disableErr, ErrStateChangedConcurrently) && pendingBasis.recordLease != nil && observedCloseErr == nil {
			token = pendingBasis
			pendingBasis = cleanupToken{}
			pendingFallback = true
		} else {
			basisCloseErr := pendingBasis.Close()
			if errors.Is(disableErr, errStateAppliedSyncPending) {
				result.Outcome = PurgeCleanupPending
			}
			class := PurgeErrorDisableWrite
			if errors.Is(disableErr, ErrStateChangedConcurrently) {
				class = PurgeErrorStateChanged
			}
			return result, newPurgeError(class, errors.Join(disableErr, observedCloseErr, basisCloseErr))
		}
	} else {
		observedCloseErr = errors.Join(observedCloseErr, pendingBasis.Close())
		result.DisabledDurable = true
		result.RecoveredState = recoveringState
	}
	incompletePhase = PurgeIncompleteLocalCleanup
	if observedCloseErr != nil {
		tokenCloseErr := token.Close()
		result.Outcome = PurgeCleanupPending
		return result, newPurgeError(PurgeErrorStorage, errors.Join(observedCloseErr, tokenCloseErr))
	}
	defer func() {
		if closeErr := token.Close(); closeErr != nil {
			markPurgeCloseFailure(&result, &returnErr, closeErr)
		}
	}()

	wait := service.deps.disableUploaderWait
	if wait <= 0 {
		wait = defaultDisableUploaderWait
	}
	waitContext, cancel := context.WithTimeout(ctx, wait)
	incompletePhase = PurgeIncompleteUploaderQuiescence
	if service.deps.beforeDisableUploaderLock != nil {
		service.deps.beforeDisableUploaderLock()
	}
	uploader, err := service.lockUploader(waitContext, root)
	cancel()
	if err != nil {
		result.Outcome = PurgeCleanupPending
		class := PurgeErrorStorage
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			class = PurgeErrorUploaderQuiescence
		}
		return result, newPurgeError(class, err)
	}
	defer func() {
		closeErr := errors.Join(uploader.Close(), service.controlCloseFailure(controlCloseUploader))
		if closeErr != nil {
			markPurgeCloseFailure(&result, &returnErr, closeErr)
		}
	}()
	incompletePhase = PurgeIncompleteLocalCleanup
	stateContext, cancelState := context.WithTimeout(ctx, stateWait)
	state, err := uploader.lockState(stateContext, service)
	cancelState()
	if err != nil {
		result.Outcome = PurgeCleanupPending
		return result, newPurgeError(PurgeErrorStorage, err)
	}
	defer func() {
		closeErr := errors.Join(state.Close(), service.controlCloseFailure(controlCloseFinalState))
		if closeErr != nil {
			markPurgeCloseFailure(&result, &returnErr, closeErr)
		}
	}()

	budget := service.deps.disableCleanupBudget
	if budget == (spoolWorkBudget{}) {
		budget = defaultSpoolWorkBudget()
	}
	if err := service.prepareCleanupLocked(state, token); err != nil {
		if !errors.Is(err, ErrStateChangedConcurrently) {
			result.Outcome = PurgeCleanupPending
			return result, newPurgeError(PurgeErrorCleanupIncomplete, err)
		}
		peer, peerErr := service.loadDurableCleanupSuccessorLocked(state, token)
		if peerErr != nil {
			result.Outcome = PurgeCleanupPending
			class := PurgeErrorStorage
			if errors.Is(peerErr, ErrStateChangedConcurrently) {
				class = PurgeErrorStateChanged
			}
			return result, newPurgeError(class, errors.Join(err, peerErr))
		}
		peerCloseErr := peer.Close()
		incompletePhase = PurgeIncompleteFinalProof
		if proofErr := proveCleanMetricsTree(root, budget); proofErr != nil || peerCloseErr != nil {
			result.Outcome = PurgeCleanupPending
			class := PurgeErrorStorage
			if peerCloseErr == nil && errors.Is(proofErr, ErrStateChangedConcurrently) {
				class = PurgeErrorStateChanged
			}
			return result, newPurgeError(class, errors.Join(proofErr, peerCloseErr))
		}
		finalPeer, finalPeerErr := loadDurableExactStateLocked(state, cleanupSuccessorState(token.barrier))
		if finalPeerErr != nil {
			result.Outcome = PurgeCleanupPending
			class := PurgeErrorStorage
			if errors.Is(finalPeerErr, ErrStateChangedConcurrently) {
				class = PurgeErrorStateChanged
			}
			return result, newPurgeError(class, finalPeerErr)
		}
		if finalPeerCloseErr := finalPeer.Close(); finalPeerCloseErr != nil {
			result.Outcome = PurgeCleanupPending
			return result, newPurgeError(PurgeErrorStorage, finalPeerCloseErr)
		}
		result.DisabledDurable = true
		result.Outcome = completedPurgeOutcome(alreadyDisabled)
		return result, nil
	}
	if pendingFallback {
		result.DisabledDurable = true
	}

	sweep, sweepErr := purgeSpoolWithinBudget(root, budget)
	result.RemovedEvents = sweep.removedEvents
	result.RemovedBytes = sweep.removedBytes
	if sweepErr != nil || !sweep.complete {
		result.Outcome = PurgeCleanupPending
		if sweepErr == nil {
			sweepErr = errors.New("productmetrics: bounded cleanup did not prove the metrics tree empty")
		}
		return result, newPurgeError(PurgeErrorCleanupIncomplete, sweepErr)
	}
	incompletePhase = PurgeIncompleteFinalProof
	if err := service.completeCleanupLockedWithJournalProof(state, token, sweep.meter); err != nil {
		result.Outcome = PurgeCleanupPending
		class := PurgeErrorCleanupIncomplete
		if errors.Is(err, ErrStateChangedConcurrently) {
			class = PurgeErrorStateChanged
		}
		return result, newPurgeError(class, err)
	}
	clean, err := loadDurableExactStateLocked(state, cleanupSuccessorState(token.barrier))
	if err != nil {
		result.Outcome = PurgeCleanupPending
		class := PurgeErrorStorage
		if errors.Is(err, ErrStateChangedConcurrently) {
			class = PurgeErrorStateChanged
		}
		return result, newPurgeError(class, err)
	}
	if err := clean.Close(); err != nil {
		result.Outcome = PurgeFailed
		return result, newPurgeError(PurgeErrorStorage, err)
	}
	result.Outcome = completedPurgeOutcome(alreadyDisabled)
	return result, nil
}

func completedPurgeOutcome(alreadyDisabled bool) PurgeOutcome {
	if alreadyDisabled {
		return PurgeAlreadyDisabled
	}
	return PurgeCompleted
}

func (service *Service) controlCloseFailure(target controlCloseTarget) error {
	if service == nil || service.deps.controlCloseError == nil {
		return nil
	}
	return service.deps.controlCloseError(target)
}

func markPurgeCloseFailure(result *PurgeResult, returnErr *error, closeErr error) {
	if result == nil || returnErr == nil || closeErr == nil {
		return
	}
	if result.Outcome == PurgeCompleted || result.Outcome == PurgeAlreadyDisabled {
		result.Outcome = PurgeFailed
	}
	*returnErr = newPurgeError(PurgeErrorStorage, errors.Join(*returnErr, closeErr))
}

func markPurgeIncomplete(result *PurgeResult, phase PurgeIncompletePhase, err error) {
	if result == nil || err == nil {
		return
	}
	result.IncompletePhase = phase
	switch {
	case errors.Is(err, errUnsettledRootTempJournal):
		result.ManualCleanupRequired = true
		result.ManualCleanupReason = PurgeManualCleanupUnsettledRootTempJournal
	case errors.Is(err, errUnrecognizedMetricsRootEntry):
		result.ManualCleanupRequired = true
		result.ManualCleanupReason = PurgeManualCleanupUnrecognizedRootEntry
	default:
		result.ManualCleanupRequired = false
		result.ManualCleanupReason = PurgeManualCleanupNone
	}
}

func cleanDisabledState(state persistedState) bool {
	return state.Preference == preferenceDisabled && state.CleanupKind == cleanupNone &&
		state.InstallationID == "" && state.SpoolGeneration == "" && state.PausedThroughMetricsEpoch == 0
}

func (service *Service) loadDurableCleanupSuccessorLocked(locked *lockedState, token cleanupToken) (loadedState, error) {
	if service == nil || !locked.valid() || token.recordLease == nil || token.kind != cleanupDisable {
		return loadedState{}, ErrStateChangedConcurrently
	}
	expected := cleanupSuccessorState(token.barrier)
	if !cleanDisabledState(expected) {
		return loadedState{}, ErrStateChangedConcurrently
	}
	return loadDurableExactStateLocked(locked, expected)
}

func loadDurableExactStateLocked(locked *lockedState, expected persistedState) (loadedState, error) {
	if !locked.valid() || !cleanDisabledState(expected) {
		return loadedState{}, ErrStateChangedConcurrently
	}
	first := loadStateFromDirectory(locked.root)
	if first.err != nil || !first.present || first.lease == nil || first.state != expected {
		err := errors.Join(first.err, ErrStateChangedConcurrently)
		_ = first.Close()
		return loadedState{}, err
	}
	lease := first.takeLease()
	_ = first.Close()
	if err := locked.root.syncDirectory(); err != nil {
		_ = lease.Close()
		return loadedState{}, err
	}
	reloaded := loadStateFromDirectory(locked.root)
	if reloaded.err != nil || !reloaded.present || reloaded.lease == nil || reloaded.state != expected ||
		!lease.Matches(reloaded.lease) {
		err := errors.Join(reloaded.err, ErrStateChangedConcurrently, lease.Close())
		_ = reloaded.Close()
		return loadedState{}, err
	}
	if err := lease.Close(); err != nil {
		_ = reloaded.Close()
		return loadedState{}, err
	}
	return reloaded, nil
}

func proveCleanMetricsTree(root *storageRoot, budget spoolWorkBudget) error {
	return proveCleanMetricsTreeWithOptions(root, budget, false)
}

func proveCleanMetricsTreeAllowDiagnosticStatus(root *storageRoot, budget spoolWorkBudget) error {
	return proveCleanMetricsTreeWithOptions(root, budget, true)
}

func proveCleanMetricsTreeWithOptions(root *storageRoot, budget spoolWorkBudget, allowDiagnosticStatus bool) error {
	if root == nil || root.storageDir == nil || root.backend == nil {
		return errStorageClosed
	}
	budget = constrainSpoolDirectoryBudget(root, budget)
	meter := newSpoolWorkMeter(budget)
	meter.physicalDirectories = true
	if err := proveRootTempJournalReadOnlyWithMeter(root, meter, false); err != nil {
		if errors.Is(err, errUnsettledRootTempJournal) {
			return errors.Join(ErrStateChangedConcurrently, err)
		}
		return err
	}
	restore := root.installDirectoryOpenHooks(meter.beforePhysicalDirectoryOpen, meter.afterPhysicalDirectoryOpen)
	defer restore()
	if !meter.claimFixedWorkEnvelope() {
		return ErrStateChangedConcurrently
	}
	if allowDiagnosticStatus {
		if err := proveDiagnosticStatusReadOnly(root, meter); err != nil {
			return err
		}
	}
	quota, present, err := loadSpoolQuota(root)
	if err != nil {
		return err
	}
	if !present || quota != (spoolQuota{}) {
		return ErrStateChangedConcurrently
	}
	for _, name := range []string{spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName} {
		if _, err := root.lookupEntry(name); err == nil {
			return ErrStateChangedConcurrently
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, treeName := range []string{queueDirectoryName, inflightDirectoryName} {
		if !meter.chargeDirectory() {
			return ErrStateChangedConcurrently
		}
		entry, lookupErr := root.lookupEntry(treeName)
		if errors.Is(lookupErr, fs.ErrNotExist) {
			continue
		}
		if lookupErr != nil {
			return lookupErr
		}
		tree, openErr := root.openEnumeratedCleanupDirectory(entry)
		if openErr != nil {
			return openErr
		}
		if tree.cleanupOnly() {
			_ = tree.Close()
			return ErrStateChangedConcurrently
		}
		iterator, iterateErr := tree.iterateEntries()
		if iterateErr != nil {
			_ = tree.Close()
			return iterateErr
		}
		_, hasEntry := meter.next(iterator)
		closeErr := errors.Join(iterator.Close(), tree.syncDirectory(), tree.Close())
		if closeErr != nil {
			return closeErr
		}
		if hasEntry || meter.exhausted {
			return ErrStateChangedConcurrently
		}
		if meter.traversalError != nil {
			return meter.traversalError
		}
	}
	if !meter.chargeDirectory() {
		return ErrStateChangedConcurrently
	}
	iterator, err := root.iterateEntries()
	if err != nil {
		return err
	}
	defer func() {
		if iterator != nil {
			_ = iterator.Close()
		}
	}()
	for {
		entry, ok := meter.next(iterator)
		if !ok {
			break
		}
		if peerCleanRootEntry(entry.name, allowDiagnosticStatus) {
			continue
		}
		return ErrStateChangedConcurrently
	}
	if meter.exhausted {
		return ErrStateChangedConcurrently
	}
	if meter.traversalError != nil {
		return meter.traversalError
	}
	err = iterator.Close()
	iterator = nil
	return err
}

func peerCleanRootEntry(name string, allowDiagnosticStatus bool) bool {
	if isStorageLockName(name) {
		return true
	}
	switch name {
	case configFileName, quotaFileName, queueDirectoryName, inflightDirectoryName, rootTempJournalDirectoryName:
		return true
	case statusFileName:
		return allowDiagnosticStatus
	default:
		return false
	}
}
