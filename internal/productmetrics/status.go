package productmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	statusFileName                = "status.toml"
	currentDiagnosticStatusSchema = uint64(1)
	maximumDiagnosticStatusBytes  = 4 * 1024
	maximumDroppedEvents          = uint64(math.MaxInt64)

	edgeLogRetentionDays     = uint64(7)
	rawEventRetentionDays    = uint64(90)
	aggregateRetentionMonths = uint64(13)
)

// DiagnosticErrorClass is the closed, path-free class of the most recent
// product-metrics failure that was safe to persist for local diagnostics.
type DiagnosticErrorClass string

// Diagnostic error classes are bounded and contain no path or response text.
const (
	DiagnosticErrorLockTimeout     DiagnosticErrorClass = "lock-timeout"
	DiagnosticErrorDiskFull        DiagnosticErrorClass = "disk-full"
	DiagnosticErrorStorageFailure  DiagnosticErrorClass = "storage-failure"
	DiagnosticErrorNetworkTimeout  DiagnosticErrorClass = "network-timeout"
	DiagnosticErrorNetworkFailure  DiagnosticErrorClass = "network-failure"
	DiagnosticErrorServer4xx       DiagnosticErrorClass = "server-4xx"
	DiagnosticErrorServer5xx       DiagnosticErrorClass = "server-5xx"
	DiagnosticErrorInvalidResponse DiagnosticErrorClass = "invalid-response"
	DiagnosticErrorServerPaused    DiagnosticErrorClass = "server-paused"
)

type diagnosticStatus struct {
	droppedEvents            uint64
	lastUploadAttemptHourUTC string
	lastUploadSuccessHourUTC string
	lastErrorClass           DiagnosticErrorClass
}

type diagnosticStatusWire struct {
	StatusSchema             uint64 `toml:"status_schema"`
	DroppedEvents            uint64 `toml:"dropped_events"`
	LastUploadAttemptHourUTC string `toml:"last_upload_attempt_hour_utc"`
	LastUploadSuccessHourUTC string `toml:"last_upload_success_hour_utc"`
	LastErrorClass           string `toml:"last_error_class"`
}

func encodeDiagnosticStatus(status diagnosticStatus) ([]byte, error) {
	if err := validateDiagnosticStatus(status); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	err := toml.NewEncoder(&output).Encode(diagnosticStatusWire{
		StatusSchema:             currentDiagnosticStatusSchema,
		DroppedEvents:            status.droppedEvents,
		LastUploadAttemptHourUTC: status.lastUploadAttemptHourUTC,
		LastUploadSuccessHourUTC: status.lastUploadSuccessHourUTC,
		LastErrorClass:           string(status.lastErrorClass),
	})
	if err != nil {
		return nil, fmt.Errorf("productmetrics: encode diagnostic status: %w", err)
	}
	if output.Len() > maximumDiagnosticStatusBytes {
		return nil, fmt.Errorf("productmetrics: encoded diagnostic status exceeds %d bytes", maximumDiagnosticStatusBytes)
	}
	return output.Bytes(), nil
}

func decodeDiagnosticStatus(data []byte) (diagnosticStatus, error) {
	if len(data) == 0 || len(data) > maximumDiagnosticStatusBytes {
		return diagnosticStatus{}, errors.New("productmetrics: invalid diagnostic status size")
	}
	var wire diagnosticStatusWire
	metadata, err := toml.Decode(string(data), &wire)
	if err != nil {
		return diagnosticStatus{}, fmt.Errorf("productmetrics: decode diagnostic status TOML: %w", err)
	}
	required := map[string]bool{
		"status_schema":                false,
		"dropped_events":               false,
		"last_upload_attempt_hour_utc": false,
		"last_upload_success_hour_utc": false,
		"last_error_class":             false,
	}
	for _, key := range metadata.Keys() {
		parts := []string(key)
		if len(parts) != 1 {
			return diagnosticStatus{}, fmt.Errorf("productmetrics: nested diagnostic status key %q is not allowed", key.String())
		}
		if _, ok := required[parts[0]]; !ok {
			return diagnosticStatus{}, fmt.Errorf("productmetrics: unknown diagnostic status field %q", parts[0])
		}
		required[parts[0]] = true
	}
	for key, present := range required {
		if !present {
			return diagnosticStatus{}, fmt.Errorf("productmetrics: required diagnostic status field %q is absent", key)
		}
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		return diagnosticStatus{}, fmt.Errorf("productmetrics: unrecognized diagnostic status field %q", undecoded[0].String())
	}
	if wire.StatusSchema != currentDiagnosticStatusSchema {
		return diagnosticStatus{}, fmt.Errorf("productmetrics: diagnostic status schema is %d, want %d", wire.StatusSchema, currentDiagnosticStatusSchema)
	}
	status := diagnosticStatus{
		droppedEvents:            wire.DroppedEvents,
		lastUploadAttemptHourUTC: wire.LastUploadAttemptHourUTC,
		lastUploadSuccessHourUTC: wire.LastUploadSuccessHourUTC,
		lastErrorClass:           DiagnosticErrorClass(wire.LastErrorClass),
	}
	if err := validateDiagnosticStatus(status); err != nil {
		return diagnosticStatus{}, err
	}
	return status, nil
}

func validateDiagnosticStatus(status diagnosticStatus) error {
	if status.droppedEvents > maximumDroppedEvents {
		return errors.New("productmetrics: dropped-event counter is exhausted")
	}
	for name, hour := range map[string]string{
		"last upload attempt": status.lastUploadAttemptHourUTC,
		"last upload success": status.lastUploadSuccessHourUTC,
	} {
		if hour == "" {
			continue
		}
		if _, err := parseCanonicalHourUTC(hour); err != nil {
			return fmt.Errorf("productmetrics: %s hour is invalid: %w", name, err)
		}
	}
	if !validDiagnosticErrorClass(status.lastErrorClass) {
		return fmt.Errorf("productmetrics: invalid diagnostic error class %q", status.lastErrorClass)
	}
	return nil
}

func validDiagnosticErrorClass(class DiagnosticErrorClass) bool {
	switch class {
	case "", DiagnosticErrorLockTimeout, DiagnosticErrorDiskFull, DiagnosticErrorStorageFailure,
		DiagnosticErrorNetworkTimeout, DiagnosticErrorNetworkFailure, DiagnosticErrorServer4xx,
		DiagnosticErrorServer5xx, DiagnosticErrorInvalidResponse, DiagnosticErrorServerPaused:
		return true
	default:
		return false
	}
}

// PolicyMetadata contains only compiled, non-secret product-metrics policy
// facts suitable for the user-facing status command.
type PolicyMetadata struct {
	EndpointHostname         string
	PrivacyURL               string
	EdgeLogRetentionDays     uint64
	RawEventRetentionDays    uint64
	AggregateRetentionMonths uint64
}

// PolicyMetadata returns the immutable product-metrics endpoint hostname and
// retention policy compiled into this service. It never returns URL path,
// query, fragment, or credential material.
func (service *Service) PolicyMetadata() PolicyMetadata {
	if service == nil {
		return PolicyMetadata{
			EdgeLogRetentionDays:     edgeLogRetentionDays,
			RawEventRetentionDays:    rawEventRetentionDays,
			AggregateRetentionMonths: aggregateRetentionMonths,
		}
	}
	return PolicyMetadata{
		EndpointHostname:         service.deps.release.endpointHostname,
		PrivacyURL:               service.deps.release.privacyURL,
		EdgeLogRetentionDays:     edgeLogRetentionDays,
		RawEventRetentionDays:    rawEventRetentionDays,
		AggregateRetentionMonths: aggregateRetentionMonths,
	}
}

// InstallationIDForDisclosure returns the current installation ID only from
// a valid exact config record. It is deliberately separate from Status so a
// caller must opt into handling this stable linkable pseudonym.
func (service *Service) InstallationIDForDisclosure(_ context.Context) (string, bool) {
	if service == nil {
		return "", false
	}
	loaded := service.readStateReadOnly()
	defer func() { _ = loaded.Close() }()
	if loaded.err != nil || !loaded.present || loaded.state.InstallationID == "" ||
		!validCanonicalUUIDv4(loaded.state.InstallationID) {
		return "", false
	}
	return loaded.state.InstallationID, true
}

func nonnegativeAge(now, then time.Time) time.Duration {
	if then.IsZero() || now.Before(then) {
		return 0
	}
	return now.Sub(then)
}

type readOnlyDiagnostics struct {
	queueEvents              uint64
	queueBytes               uint64
	queueAvailable           bool
	oldestQueuedAt           time.Time
	oldestQueuedPresent      bool
	status                   diagnosticStatus
	statusAvailable          bool
	spawnThrottleAttemptedAt time.Time
	spawnThrottlePresent     bool
}

type diagnosticStatusUpdate struct {
	incrementDroppedEvents bool
	lastUploadAttempt      time.Time
	lastUploadSuccess      time.Time
	lastErrorClass         DiagnosticErrorClass
	clearLastError         bool
}

func (service *Service) bestEffortUpdateDiagnosticStatusLocked(
	root *storageRoot,
	update diagnosticStatusUpdate,
	canStart func(recordOperation) bool,
) {
	if service == nil || root == nil || !recordOperationCanStart(canStart, recordOperationStatusRead) {
		return
	}
	var status diagnosticStatus
	data, lease, err := root.readFileLease(statusFileName, maximumDiagnosticStatusBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case lease == nil:
		return
	case err != nil && !errors.Is(err, errStorageReadLimit):
		_ = lease.Close()
		return
	default:
		if err == nil {
			decoded, decodeErr := decodeDiagnosticStatus(data)
			if decodeErr == nil {
				status = decoded
			}
		}
		if closeErr := lease.Close(); closeErr != nil {
			return
		}
	}
	if update.incrementDroppedEvents && status.droppedEvents < maximumDroppedEvents {
		status.droppedEvents++
	}
	if !update.lastUploadAttempt.IsZero() {
		status.lastUploadAttemptHourUTC = depsHourUTC(update.lastUploadAttempt)
	}
	if !update.lastUploadSuccess.IsZero() {
		status.lastUploadSuccessHourUTC = depsHourUTC(update.lastUploadSuccess)
	}
	if update.clearLastError {
		status.lastErrorClass = ""
	} else if update.lastErrorClass != "" && validDiagnosticErrorClass(update.lastErrorClass) {
		status.lastErrorClass = update.lastErrorClass
	}
	encoded, encodeErr := encodeDiagnosticStatus(status)
	if encodeErr != nil || !recordOperationCanStart(canStart, recordOperationStatusWrite) {
		return
	}
	_ = root.writeFileAtomic(statusFileName, encoded)
}

func diagnosticClassForStorageError(err error) DiagnosticErrorClass {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errSpoolQuotaFull), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return DiagnosticErrorDiskFull
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), errors.Is(err, errRecordDecisionWindowExpired):
		return DiagnosticErrorLockTimeout
	default:
		return DiagnosticErrorStorageFailure
	}
}

func diagnosticClassForUpload(response uploadResponse, err error) DiagnosticErrorClass {
	if response.diagnosticError != "" {
		return response.diagnosticError
	}
	if response.kind == uploadResponsePause {
		return DiagnosticErrorServerPaused
	}
	if response.statusCode >= 500 && response.statusCode <= 599 {
		return DiagnosticErrorServer5xx
	}
	if response.statusCode >= 400 && response.statusCode <= 499 {
		return DiagnosticErrorServer4xx
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return DiagnosticErrorNetworkTimeout
	}
	if err != nil {
		return DiagnosticErrorNetworkFailure
	}
	return DiagnosticErrorInvalidResponse
}

func (service *Service) readDiagnosticsReadOnly() readOnlyDiagnostics {
	diagnostics := readOnlyDiagnostics{queueAvailable: true, statusAvailable: true}
	if service == nil || service.deps.homeErr != nil {
		diagnostics.queueAvailable = false
		diagnostics.statusAvailable = false
		return diagnostics
	}
	root, err := openStorageRootReadOnly(service.deps.home)
	if errors.Is(err, fs.ErrNotExist) {
		return diagnostics
	}
	if err != nil {
		diagnostics.queueAvailable = false
		diagnostics.statusAvailable = false
		return diagnostics
	}

	statusData, statusErr := root.readFile(statusFileName, maximumDiagnosticStatusBytes)
	switch {
	case errors.Is(statusErr, fs.ErrNotExist):
	case statusErr != nil:
		diagnostics.statusAvailable = false
	default:
		decoded, decodeErr := decodeDiagnosticStatus(statusData)
		if decodeErr != nil {
			diagnostics.statusAvailable = false
		} else {
			diagnostics.status = decoded
		}
	}

	throttleData, throttleErr := root.readFile(spawnThrottleFileName, maximumSpawnThrottleBytes)
	if throttleErr == nil {
		throttle, decodeErr := decodeSpawnThrottle(throttleData)
		if decodeErr == nil {
			diagnostics.spawnThrottleAttemptedAt = throttle.attemptedAt
			diagnostics.spawnThrottlePresent = true
		}
	}

	quota, present, quotaErr := loadSpoolQuota(root)
	switch {
	case quotaErr != nil:
		diagnostics.queueAvailable = false
	case !present:
		for _, name := range []string{queueDirectoryName, inflightDirectoryName} {
			if _, lookupErr := root.lookupEntry(name); !errors.Is(lookupErr, fs.ErrNotExist) {
				diagnostics.queueAvailable = false
			}
		}
	default:
		diagnostics.queueEvents = quota.Events
		diagnostics.queueBytes = quota.Bytes
		oldest, oldestPresent, scannedEvents, scannedBytes, scanErr := scanOldestQueuedEventReadOnly(root, defaultSpoolWorkBudget())
		if scanErr != nil || scannedEvents > quota.Events || scannedBytes > quota.Bytes {
			diagnostics.queueAvailable = false
			diagnostics.queueEvents = 0
			diagnostics.queueBytes = 0
		} else {
			diagnostics.oldestQueuedAt = oldest
			diagnostics.oldestQueuedPresent = oldestPresent
		}
	}
	if closeErr := root.Close(); closeErr != nil {
		diagnostics.queueAvailable = false
		diagnostics.statusAvailable = false
		diagnostics.spawnThrottlePresent = false
	}
	return diagnostics
}

func scanOldestQueuedEventReadOnly(root *storageRoot, budget spoolWorkBudget) (
	oldest time.Time,
	present bool,
	events uint64,
	bytesRead uint64,
	returnErr error,
) {
	if root == nil {
		return time.Time{}, false, 0, 0, errStorageClosed
	}
	meter := newSpoolWorkMeter(budget)
	meter.physicalDirectories = true
	restoreDirectoryOpenHooks := root.installDirectoryOpenHooks(
		meter.beforePhysicalDirectoryOpen,
		meter.afterPhysicalDirectoryOpen,
	)
	defer restoreDirectoryOpenHooks()
	for _, treeName := range []string{queueDirectoryName, inflightDirectoryName} {
		if !meter.chargeNamedEntry(treeName) {
			return time.Time{}, false, 0, 0, errors.New("productmetrics: diagnostic spool budget exhausted")
		}
		treeEntry, err := root.lookupEntry(treeName)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || treeEntry.metadata.kind != storageEntryDirectory || !meter.chargeDirectory() {
			return time.Time{}, false, 0, 0, errors.Join(err, errors.New("productmetrics: diagnostic spool tree is unavailable"))
		}
		tree, err := root.openDir([]string{treeName}, false)
		if err != nil {
			return time.Time{}, false, 0, 0, err
		}
		iterator, err := tree.iterateEntries()
		if err != nil {
			_ = tree.Close()
			return time.Time{}, false, 0, 0, err
		}
		for {
			generationEntry, ok := meter.next(iterator)
			if !ok {
				break
			}
			if generationEntry.metadata.kind != storageEntryDirectory || !validCanonicalUUIDv4(generationEntry.name) ||
				!meter.chargeDirectory() {
				returnErr = errors.New("productmetrics: diagnostic spool generation is unavailable")
				break
			}
			generation, openErr := tree.openDir([]string{generationEntry.name}, false)
			if openErr != nil {
				returnErr = openErr
				break
			}
			generationIterator, iterateErr := generation.iterateEntries()
			if iterateErr != nil {
				returnErr = errors.Join(iterateErr, generation.Close())
				break
			}
			for {
				eventEntry, eventOK := meter.next(generationIterator)
				if !eventOK {
					break
				}
				if !meter.chargeEventEntry() || eventEntry.metadata.kind != storageEntryRegular ||
					!eventEntry.metadata.ownerOnly || eventEntry.metadata.nlink != 1 {
					returnErr = errors.New("productmetrics: diagnostic event entry is unavailable")
					break
				}
				if _, ok := eventIDFromFileName(eventEntry.name); !ok {
					returnErr = errors.New("productmetrics: diagnostic event name is invalid")
					break
				}
				const readReservation = maximumEventBytes + 1
				if !meter.chargeRead(readReservation) {
					returnErr = errors.New("productmetrics: diagnostic spool read budget exhausted")
					break
				}
				data, physicalReadBytes, lease, readErr := generation.readFileMeasured(eventEntry.name, int64(maximumEventBytes))
				meter.refundRead(readReservation, physicalReadBytes)
				if readErr != nil || lease == nil {
					returnErr = errors.Join(readErr, errors.New("productmetrics: diagnostic event read is unavailable"))
					if lease != nil {
						returnErr = errors.Join(returnErr, lease.Close())
					}
					break
				}
				_, decodeErr := DecodeEvent(data)
				incarnation := lease.incarnation()
				leaseErr := lease.Close()
				if decodeErr != nil || leaseErr != nil || incarnation != (recordIncarnation{
					dev: eventEntry.metadata.dev,
					ino: eventEntry.metadata.ino,
				}) {
					returnErr = errors.Join(decodeErr, leaseErr)
					if returnErr == nil {
						returnErr = errors.New("productmetrics: diagnostic event incarnation changed")
					}
					break
				}
				events++
				bytesRead += uint64(len(data))
				queuedAt := time.Unix(eventEntry.metadata.mtimeSeconds, eventEntry.metadata.mtimeNanoseconds).UTC()
				if !present || queuedAt.Before(oldest) {
					oldest = queuedAt
					present = true
				}
			}
			returnErr = errors.Join(returnErr, generationIterator.Close(), generation.Close())
			if returnErr != nil {
				break
			}
		}
		returnErr = errors.Join(returnErr, iterator.Close(), tree.Close())
		returnErr = errors.Join(returnErr, meter.traversalError)
		if meter.exhausted && returnErr == nil {
			returnErr = errors.New("productmetrics: diagnostic spool budget exhausted")
		}
		if returnErr != nil {
			return time.Time{}, false, 0, 0, returnErr
		}
	}
	return oldest, present, events, bytesRead, nil
}

func proveDiagnosticStatusReadOnly(root *storageRoot, meter *spoolWorkMeter) error {
	if root == nil || meter == nil || !meter.chargeFixedEntry(statusFileName) ||
		!meter.chargeFixedRead(maximumDiagnosticStatusBytes+1) {
		return ErrStateChangedConcurrently
	}
	data, _, lease, err := root.readFileMeasured(statusFileName, maximumDiagnosticStatusBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || lease == nil {
		if lease != nil {
			err = errors.Join(err, lease.Close())
		}
		return errors.Join(err, ErrStateChangedConcurrently)
	}
	decodeErr := func() error {
		_, err := decodeDiagnosticStatus(data)
		return err
	}()
	return errors.Join(decodeErr, lease.Close())
}
