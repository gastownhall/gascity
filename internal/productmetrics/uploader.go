package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

const uploaderLockName = "uploader.lock"

type uploadRunOutcome uint8

const (
	uploadRunNoBatch uploadRunOutcome = iota
	uploadRunDeleted
	uploadRunRestored
	uploadRunStale
	uploadRunPausePending
	uploadRunPaused
)

type uploadRunResult struct {
	outcome uploadRunOutcome
	events  int
}

type uploaderOperation string

const (
	uploaderOperationBeforePreSendRevalidation uploaderOperation = "before-pre-send-revalidation"
	uploaderOperationAfterSend                 uploaderOperation = "after-send"
)

type uploadWaitFunc func() (uploadResponse, error)

// uploadStartFunc must initiate the upload without blocking on its response.
// A successful return linearizes request initiation; the returned function
// waits for the response after the caller releases the state lock.
type uploadStartFunc func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error)

type uploaderDependencies struct {
	now             func() time.Time
	start           uploadStartFunc
	budget          spoolWorkBudget
	beforeOperation func(uploaderOperation)
	authorizeLocked func(*lockedState) error
}

type lockedUploader struct {
	root   *storageRoot
	lock   *advisoryLock
	closed atomic.Bool
}

func (locked *lockedUploader) Close() error {
	if locked == nil || !locked.closed.CompareAndSwap(false, true) {
		return nil
	}
	if locked.lock == nil {
		return nil
	}
	return locked.lock.Release()
}

func (locked *lockedUploader) valid() bool {
	return locked != nil && locked.root != nil && locked.lock != nil && !locked.closed.Load()
}

func (locked *lockedUploader) lockState(ctx context.Context, service *Service) (*lockedState, error) {
	if !locked.valid() {
		return nil, errors.New("productmetrics: uploader lock is not held")
	}
	return service.lockState(ctx, locked.root)
}

func (service *Service) lockUploader(ctx context.Context, root *storageRoot) (*lockedUploader, error) {
	if service == nil {
		return nil, errors.New("productmetrics: service is nil")
	}
	if ctx == nil {
		return nil, errors.New("productmetrics: uploader-lock context is nil")
	}
	if root == nil {
		return nil, errStorageClosed
	}
	lock, err := root.acquireLock(ctx, uploaderLockName)
	if err != nil {
		return nil, err
	}
	return &lockedUploader{root: root, lock: lock}, nil
}

func (service *Service) uploadOneBatch(ctx context.Context, dependencies uploaderDependencies) (result uploadRunResult, returnErr error) {
	if service == nil {
		return result, errors.New("productmetrics: service is nil")
	}
	if ctx == nil {
		return result, errors.New("productmetrics: upload context is nil")
	}
	if dependencies.start == nil {
		return result, errors.New("productmetrics: upload starter is nil")
	}
	if dependencies.now == nil {
		dependencies.now = service.deps.now
	}
	if dependencies.now == nil {
		dependencies.now = time.Now
	}
	if dependencies.budget == (spoolWorkBudget{}) {
		dependencies.budget = defaultSpoolWorkBudget()
	}
	eligible, err := service.uploadNeedsMutableWork()
	if err != nil {
		return result, err
	}
	if !eligible {
		return uploadRunResult{outcome: uploadRunNoBatch}, nil
	}

	root, err := openStorageRootMutableWithHooks(service.deps.home, service.deps.storageHooks)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	uploader, err := service.lockUploader(ctx, root)
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, uploader.Close()) }()
	return service.uploadOneBatchLocked(ctx, root, uploader, dependencies)
}

// uploadOneBatchLocked performs one batch while retaining the caller's exact
// root and uploader-lock capabilities. Detached children use this boundary so
// root replacement cannot move token validation and upload onto different
// filesystem objects.
func (service *Service) uploadOneBatchLocked(
	ctx context.Context,
	root *storageRoot,
	uploader *lockedUploader,
	dependencies uploaderDependencies,
) (result uploadRunResult, returnErr error) {
	if service == nil || root == nil || !uploader.valid() || uploader.root != root {
		return result, errors.New("productmetrics: uploader lock is not held for the supplied root")
	}
	if ctx == nil {
		return result, errors.New("productmetrics: upload context is nil")
	}
	if dependencies.start == nil {
		return result, errors.New("productmetrics: upload starter is nil")
	}
	if dependencies.now == nil {
		dependencies.now = service.deps.now
	}
	if dependencies.now == nil {
		dependencies.now = time.Now
	}
	if dependencies.budget == (spoolWorkBudget{}) {
		dependencies.budget = defaultSpoolWorkBudget()
	}
	state, err := uploader.lockState(ctx, service)
	if err != nil {
		return result, err
	}
	if dependencies.authorizeLocked != nil {
		if err := dependencies.authorizeLocked(state); err != nil {
			return result, errors.Join(err, state.Close())
		}
	}
	pauseToken, _, pausePending, err := service.pauseCleanupLocked(state)
	if err != nil {
		return result, errors.Join(err, state.Close())
	}
	if pausePending {
		defer func() { returnErr = errors.Join(returnErr, pauseToken.Close()) }()
		complete, cleanupErr := service.finishPauseCleanupLocked(state, pauseToken, dependencies.budget)
		outcome := uploadRunPausePending
		if complete {
			outcome = uploadRunPaused
		}
		return uploadRunResult{outcome: outcome}, errors.Join(cleanupErr, state.Close())
	}
	permit, err := service.currentUploadPermitLocked(state)
	if err != nil {
		return result, errors.Join(err, state.Close())
	}
	defer func() { returnErr = errors.Join(returnErr, permit.Close()) }()
	if err := service.revalidatePermitLocked(state, permit); err != nil {
		return result, errors.Join(err, state.Close())
	}
	claim, err := claimSpoolBatch(root, permit, dependencies.now(), dependencies.budget)
	if err != nil {
		return result, errors.Join(err, state.Close())
	}
	if len(claim.records) == 0 {
		return uploadRunResult{outcome: uploadRunNoBatch}, state.Close()
	}
	prepared, err := prepareSpoolClaimForUpload(claim, permit)
	if err != nil {
		return result, errors.Join(err, state.Close())
	}
	if err := state.Close(); err != nil {
		return result, err
	}

	if dependencies.beforeOperation != nil {
		dependencies.beforeOperation(uploaderOperationBeforePreSendRevalidation)
	}
	state, err = uploader.lockState(ctx, service)
	if err != nil {
		return result, err
	}
	if err := service.revalidatePermitLocked(state, permit); err != nil {
		return uploadRunResult{outcome: uploadRunStale, events: len(claim.records)}, errors.Join(err, state.Close())
	}
	if err := state.Close(); err != nil {
		return result, err
	}
	if dependencies.beforeOperation != nil {
		dependencies.beforeOperation(uploaderOperation("before-send-start"))
	}
	state, err = uploader.lockState(ctx, service)
	if err != nil {
		return result, err
	}
	if err := service.revalidatePermitLocked(state, permit); err != nil {
		return uploadRunResult{outcome: uploadRunStale, events: len(claim.records)}, errors.Join(err, state.Close())
	}
	if dependencies.authorizeLocked != nil {
		if err := dependencies.authorizeLocked(state); err != nil {
			settleErr := restoreSpoolClaim(root, claim)
			return uploadRunResult{outcome: uploadRunRestored, events: len(claim.records)}, errors.Join(err, settleErr, state.Close())
		}
	}
	wait, startErr := dependencies.start(ctx, prepared, permit.metricsEpoch)
	if startErr != nil || wait == nil {
		if startErr == nil {
			startErr = errors.New("productmetrics: upload start returned a nil wait function")
		}
		settleErr := restoreSpoolClaim(root, claim)
		return uploadRunResult{outcome: uploadRunRestored, events: len(claim.records)}, errors.Join(startErr, settleErr, state.Close())
	}
	attemptedAt := dependencies.now()
	service.bestEffortUpdateDiagnosticStatusLocked(root, diagnosticStatusUpdate{
		lastUploadAttempt: attemptedAt,
	}, nil)
	stateCloseErr := state.Close()

	response, waitErr := wait()
	if waitErr != nil {
		response.kind = uploadResponseRetry
	}
	if dependencies.beforeOperation != nil {
		dependencies.beforeOperation(uploaderOperationAfterSend)
	}
	settlementContext, cancelSettlement := context.WithTimeout(context.Background(), stateLockTimeout)
	state, err = uploader.lockState(settlementContext, service)
	cancelSettlement()
	if err != nil {
		return result, errors.Join(waitErr, stateCloseErr, err)
	}
	if err := service.revalidatePermitLocked(state, permit); err != nil {
		return uploadRunResult{outcome: uploadRunStale, events: len(claim.records)}, errors.Join(waitErr, stateCloseErr, err, state.Close())
	}

	switch response.kind {
	case uploadResponseAccepted, uploadResponseDuplicate:
		settleErr := deleteSpoolClaim(root, claim)
		if settleErr == nil && waitErr == nil {
			service.bestEffortUpdateDiagnosticStatusLocked(root, diagnosticStatusUpdate{
				lastUploadSuccess: dependencies.now(),
				clearLastError:    true,
			}, nil)
		} else {
			class := diagnosticClassForUpload(response, waitErr)
			if settleErr != nil {
				class = diagnosticClassForStorageError(settleErr)
			}
			service.bestEffortUpdateDiagnosticStatusLocked(root, diagnosticStatusUpdate{lastErrorClass: class}, nil)
		}
		return uploadRunResult{outcome: uploadRunDeleted, events: len(claim.records)}, errors.Join(waitErr, stateCloseErr, settleErr, state.Close())
	case uploadResponseRetry:
		settleErr := restoreSpoolClaim(root, claim)
		class := diagnosticClassForUpload(response, waitErr)
		if settleErr != nil {
			class = diagnosticClassForStorageError(settleErr)
		}
		service.bestEffortUpdateDiagnosticStatusLocked(root, diagnosticStatusUpdate{lastErrorClass: class}, nil)
		return uploadRunResult{outcome: uploadRunRestored, events: len(claim.records)}, errors.Join(waitErr, stateCloseErr, settleErr, state.Close())
	case uploadResponsePause:
		if response.pause.releaseVersion != prepared.releaseVersion || response.pause.metricsEpoch != permit.metricsEpoch {
			settleErr := restoreSpoolClaim(root, claim)
			return uploadRunResult{outcome: uploadRunRestored, events: len(claim.records)}, errors.Join(
				stateCloseErr, settleErr, state.Close(), errors.New("productmetrics: signed pause does not match the claimed batch authority"))
		}
		token, pauseErr := service.applyPauseLocked(state, permit, response.pause.metricsEpoch)
		if pauseErr != nil {
			outcome := uploadRunStale
			if service.revalidatePermitLocked(state, permit) == nil {
				pauseErr = errors.Join(pauseErr, restoreSpoolClaim(root, claim))
				outcome = uploadRunRestored
			}
			return uploadRunResult{outcome: outcome, events: len(claim.records)}, errors.Join(stateCloseErr, pauseErr, state.Close())
		}
		defer func() { returnErr = errors.Join(returnErr, token.Close()) }()
		// Keep the diagnostic write behind the durable pause barrier but before
		// cleanup installs its clean successor. A crash in this best-effort
		// atomic write is then recoverable pause-cleanup residue; no diagnostic
		// write can introduce a fresh journal after cleanup has completed.
		service.bestEffortUpdateDiagnosticStatusLocked(root, diagnosticStatusUpdate{lastErrorClass: DiagnosticErrorServerPaused}, nil)
		complete, cleanupErr := service.finishPauseCleanupLocked(state, token, dependencies.budget)
		outcome := uploadRunPausePending
		if complete {
			outcome = uploadRunPaused
		}
		return uploadRunResult{outcome: outcome, events: len(claim.records)}, errors.Join(stateCloseErr, cleanupErr, state.Close())
	default:
		settleErr := restoreSpoolClaim(root, claim)
		class := DiagnosticErrorInvalidResponse
		if settleErr != nil {
			class = diagnosticClassForStorageError(settleErr)
		}
		service.bestEffortUpdateDiagnosticStatusLocked(root, diagnosticStatusUpdate{lastErrorClass: class}, nil)
		return uploadRunResult{outcome: uploadRunRestored, events: len(claim.records)}, errors.Join(waitErr, stateCloseErr, settleErr, state.Close(), errors.New("productmetrics: unknown upload response kind"))
	}
}

func (service *Service) pauseCleanupLocked(locked *lockedState) (cleanupToken, persistedState, bool, error) {
	if service == nil || !locked.valid() {
		return cleanupToken{}, persistedState{}, false, errors.New("productmetrics: state lock is not held")
	}
	loaded := loadStateFromDirectory(locked.root)
	defer func() { _ = loaded.Close() }()
	if loaded.err != nil || !loaded.present {
		return cleanupToken{}, persistedState{}, false, loaded.err
	}
	if loaded.state.CleanupKind != cleanupPause {
		return cleanupToken{}, loaded.state, false, nil
	}
	if loaded.state.Preference != preferenceEnabled || loaded.state.SpoolGeneration != "" || loaded.state.InstallationID == "" {
		return cleanupToken{}, persistedState{}, false, errors.New("productmetrics: invalid pause-cleanup state")
	}
	state := loaded.state
	return cleanupTokenFromLoaded(&loaded), state, true, nil
}

func (service *Service) finishPauseCleanupLocked(locked *lockedState, token cleanupToken, budget spoolWorkBudget) (bool, error) {
	if service == nil || !locked.valid() || token.kind != cleanupPause || token.recordLease == nil {
		return false, errors.New("productmetrics: invalid pause-cleanup authority")
	}
	if err := service.prepareCleanupLocked(locked, token); err != nil {
		return false, err
	}
	result, err := purgeSpoolWithinBudget(locked.root, budget)
	if err != nil || !result.complete {
		return false, err
	}
	if err := service.completeCleanupLockedWithJournalProof(locked, token, result.meter); err != nil {
		return false, err
	}
	return true, nil
}

func (service *Service) finishPauseCleanupAndResume(ctx context.Context) (transitioned bool, returnErr error) {
	if service == nil {
		return false, errors.New("productmetrics: service is nil")
	}
	if ctx == nil {
		return false, errors.New("productmetrics: pause-cleanup context is nil")
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, service.deps.storageHooks)
	if err != nil {
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	uploader, err := service.lockUploader(ctx, root)
	if err != nil {
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, uploader.Close()) }()
	state, err := uploader.lockState(ctx, service)
	if err != nil {
		return false, err
	}
	defer func() { returnErr = errors.Join(returnErr, state.Close()) }()
	token, barrier, pending, err := service.pauseCleanupLocked(state)
	if err != nil {
		return false, err
	}
	if barrier.PausedThroughMetricsEpoch == 0 || service.deps.release.metricsEpoch <= barrier.PausedThroughMetricsEpoch {
		return false, token.Close()
	}
	cleanupProved := false
	if pending {
		defer func() { returnErr = errors.Join(returnErr, token.Close()) }()
		complete, cleanupErr := service.finishPauseCleanupLocked(state, token, defaultSpoolWorkBudget())
		if cleanupErr != nil || !complete {
			return false, cleanupErr
		}
		cleanupProved = true
	} else if closeErr := token.Close(); closeErr != nil {
		return false, closeErr
	}
	if !cleanupProved {
		if err := proveCleanMetricsTreeAllowDiagnosticStatus(root, defaultSpoolWorkBudget()); err != nil {
			return false, err
		}
	}
	loaded := loadStateFromDirectory(root)
	defer func() { returnErr = errors.Join(returnErr, loaded.Close()) }()
	if loaded.err != nil || !loaded.present || loaded.lease == nil {
		return false, loaded.err
	}
	if err := service.resumeGreaterEpochLocked(state, stateVersionFromLoaded(loaded)); err != nil {
		return false, err
	}
	return true, nil
}

func (service *Service) uploadNeedsMutableWork() (bool, error) {
	if service == nil || service.deps.homeErr != nil {
		return false, nil
	}
	loaded := service.readStateReadOnlyWithHooks(service.deps.storageHooks)
	projection := service.project(InvocationContext{
		DoNotTrack:          service.deps.getenv(envDoNotTrack),
		DisableUsageMetrics: service.deps.getenv(envDisableUsageMetrics),
	}, loaded)
	eligible := projection.state == StateEnabled ||
		(projection.state == StateServerPaused && projection.reason == ReasonPauseCleanupPending)
	if err := loaded.Close(); err != nil {
		return false, err
	}
	return eligible, nil
}

func (service *Service) currentUploadPermitLocked(locked *lockedState) (RecordingPermit, error) {
	if service == nil || !locked.valid() {
		return RecordingPermit{}, errors.New("productmetrics: state lock is not held")
	}
	loaded := loadStateFromDirectory(locked.root)
	defer func() { _ = loaded.Close() }()
	if loaded.err != nil || !loaded.present || loaded.lease == nil {
		return RecordingPermit{}, ErrStateChangedConcurrently
	}
	projection := service.project(InvocationContext{
		DoNotTrack:          service.deps.getenv(envDoNotTrack),
		DisableUsageMetrics: service.deps.getenv(envDisableUsageMetrics),
	}, loaded)
	if projection.state != StateEnabled {
		return RecordingPermit{}, ErrStateChangedConcurrently
	}
	state := loaded.state
	permit := RecordingPermit{
		valid:            true,
		recordLease:      loaded.takeLease(),
		counterNamespace: state.CounterNamespace,
		stateGeneration:  state.StateGeneration,
		installationID:   state.InstallationID,
		spoolGeneration:  state.SpoolGeneration,
		releaseVersion:   service.deps.release.releaseVersion,
		metricsEpoch:     service.deps.release.metricsEpoch,
		requiredNotice:   state.RequiredNoticeVersion,
		acceptedNotice:   state.AcceptedNoticeVersion,
		operatingSystem:  operatingSystemForRuntime(),
	}
	if !permit.Valid() {
		_ = permit.Close()
		return RecordingPermit{}, ErrStateChangedConcurrently
	}
	return permit, nil
}

func prepareSpoolClaimForUpload(claim spoolClaim, permit RecordingPermit) (preparedUploadBatch, error) {
	if len(claim.records) == 0 || !permit.Valid() || claim.generation != permit.spoolGeneration {
		return preparedUploadBatch{}, errors.New("productmetrics: invalid spool claim upload authority")
	}
	releaseVersion := claim.records[0].event.ReleaseVersion
	files := make([]claimedEventFile, 0, len(claim.records))
	for _, record := range claim.records {
		if record.generation != claim.generation || record.name != eventFileName(record.event.EventID) ||
			record.event.ReleaseVersion != releaseVersion {
			return preparedUploadBatch{}, errors.New("productmetrics: spool claim identity mismatch")
		}
		body, err := EncodeEvent(record.event)
		if err != nil {
			return preparedUploadBatch{}, fmt.Errorf("productmetrics: encode claimed event: %w", err)
		}
		if uint64(len(body)) != record.bytes {
			return preparedUploadBatch{}, errors.New("productmetrics: claimed event byte count mismatch")
		}
		files = append(files, claimedEventFile{name: record.event.EventID, body: body})
	}
	return buildUploadBatch(files, uploadBatchIdentity{
		installationID: permit.installationID,
		releaseVersion: releaseVersion,
	})
}
