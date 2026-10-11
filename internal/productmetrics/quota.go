package productmetrics

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"

	"github.com/BurntSushi/toml"
)

var (
	errRecordDecisionWindowExpired = errors.New("productmetrics: foreground record decision window expired")
	errSpoolQuotaFull              = errors.New("productmetrics: root-global spool quota is full")
)

const (
	quotaFileName                = "quota.toml"
	spoolControlDirectoryName    = ".pm-control"
	retiredControlDirectoryName  = ".pm-control-retired"
	fallbackRelocationCursorName = ".pm-fallback-relocation.toml"
	quotaStagingFileName         = "quota.next"
	relocationCursorFileName     = "relocation.toml"
	currentQuotaSchema           = uint64(1)
	currentRelocationSchema      = uint64(1)
	maximumControlFileBytes      = 4 * 1024
	maximumQuotaBytes            = maximumControlFileBytes
	maximumRelocationBytes       = maximumControlFileBytes
	maximumRelocationSequence    = uint64(math.MaxInt64)

	maximumSpoolBytes          = uint64(4 * 1024 * 1024)
	maximumSpoolEvents         = uint64(5000)
	maximumEventBytes          = uint64(4 * 1024)
	maximumBatchEvents         = MaxBatchEvents
	maximumRequestBytes        = 64 * 1024
	maximumEventAgeHours       = 7 * 24
	maximumStorageNameBytes    = 128
	maximumStorageTempAttempts = 64
	maximumRelocationSlots     = 8

	// Reconciliation may persist one conservative overflow marker. Foreground
	// reservation treats either marker as over-cap and never increments it.
	maximumQuotaEventMarker = maximumSpoolEvents + 1
	maximumQuotaByteMarker  = maximumSpoolBytes + 1
)

type spoolQuota struct {
	Events uint64
	Bytes  uint64
}

type quotaWire struct {
	QuotaSchema    uint64 `toml:"quota_schema"`
	ReservedEvents uint64 `toml:"reserved_events"`
	ReservedBytes  uint64 `toml:"reserved_bytes"`
}

type relocationCursor struct {
	Next uint64
}

type relocationCursorWire struct {
	CursorSchema   uint64 `toml:"cursor_schema"`
	RelocationNext uint64 `toml:"relocation_next"`
}

func encodeRelocationCursor(cursor relocationCursor) ([]byte, error) {
	if cursor.Next > maximumRelocationSequence {
		return nil, errors.New("productmetrics: relocation cursor is exhausted")
	}
	var output bytes.Buffer
	if err := toml.NewEncoder(&output).Encode(relocationCursorWire{
		CursorSchema: currentRelocationSchema, RelocationNext: cursor.Next,
	}); err != nil {
		return nil, fmt.Errorf("productmetrics: encode relocation cursor: %w", err)
	}
	if output.Len() > maximumRelocationBytes {
		return nil, fmt.Errorf("productmetrics: encoded relocation cursor exceeds %d bytes", maximumRelocationBytes)
	}
	return output.Bytes(), nil
}

func decodeRelocationCursor(data []byte) (relocationCursor, error) {
	if len(data) == 0 || len(data) > maximumRelocationBytes {
		return relocationCursor{}, errors.New("productmetrics: invalid relocation cursor size")
	}
	var wire relocationCursorWire
	metadata, err := toml.Decode(string(data), &wire)
	if err != nil {
		return relocationCursor{}, fmt.Errorf("productmetrics: decode relocation cursor TOML: %w", err)
	}
	required := map[string]bool{"cursor_schema": false, "relocation_next": false}
	for _, key := range metadata.Keys() {
		parts := []string(key)
		if len(parts) != 1 {
			return relocationCursor{}, fmt.Errorf("productmetrics: nested relocation cursor key %q is not allowed", key.String())
		}
		if _, ok := required[parts[0]]; !ok {
			return relocationCursor{}, fmt.Errorf("productmetrics: unknown relocation cursor field %q", parts[0])
		}
		required[parts[0]] = true
	}
	for key, present := range required {
		if !present {
			return relocationCursor{}, fmt.Errorf("productmetrics: required relocation cursor field %q is absent", key)
		}
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		return relocationCursor{}, fmt.Errorf("productmetrics: unrecognized relocation cursor field %q", undecoded[0].String())
	}
	if wire.CursorSchema != currentRelocationSchema {
		return relocationCursor{}, fmt.Errorf("productmetrics: relocation cursor schema is %d, want %d", wire.CursorSchema, currentRelocationSchema)
	}
	cursor := relocationCursor{Next: wire.RelocationNext}
	if cursor.Next > maximumRelocationSequence {
		return relocationCursor{}, errors.New("productmetrics: relocation cursor is exhausted")
	}
	return cursor, nil
}

func encodeSpoolQuota(quota spoolQuota) ([]byte, error) {
	if err := validateSpoolQuota(quota); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	err := toml.NewEncoder(&output).Encode(quotaWire{
		QuotaSchema:    currentQuotaSchema,
		ReservedEvents: quota.Events,
		ReservedBytes:  quota.Bytes,
	})
	if err != nil {
		return nil, fmt.Errorf("productmetrics: encode spool quota: %w", err)
	}
	if output.Len() > maximumQuotaBytes {
		return nil, fmt.Errorf("productmetrics: encoded quota exceeds %d bytes", maximumQuotaBytes)
	}
	return output.Bytes(), nil
}

func decodeSpoolQuota(data []byte) (spoolQuota, error) {
	if len(data) == 0 {
		return spoolQuota{}, errors.New("productmetrics: empty quota record")
	}
	if len(data) > maximumQuotaBytes {
		return spoolQuota{}, fmt.Errorf("productmetrics: quota exceeds %d bytes", maximumQuotaBytes)
	}
	var wire quotaWire
	metadata, err := toml.Decode(string(data), &wire)
	if err != nil {
		return spoolQuota{}, fmt.Errorf("productmetrics: decode quota TOML: %w", err)
	}
	required := map[string]bool{
		"quota_schema":    false,
		"reserved_events": false,
		"reserved_bytes":  false,
	}
	for _, key := range metadata.Keys() {
		parts := []string(key)
		if len(parts) != 1 {
			return spoolQuota{}, fmt.Errorf("productmetrics: nested quota key %q is not allowed", key.String())
		}
		if _, ok := required[parts[0]]; !ok {
			return spoolQuota{}, fmt.Errorf("productmetrics: unknown quota field %q", parts[0])
		}
		required[parts[0]] = true
	}
	for key, present := range required {
		if !present {
			return spoolQuota{}, fmt.Errorf("productmetrics: required quota field %q is absent", key)
		}
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		return spoolQuota{}, fmt.Errorf("productmetrics: unrecognized quota field %q", undecoded[0].String())
	}
	if wire.QuotaSchema != currentQuotaSchema {
		return spoolQuota{}, fmt.Errorf("productmetrics: quota schema is %d, want %d", wire.QuotaSchema, currentQuotaSchema)
	}
	quota := spoolQuota{Events: wire.ReservedEvents, Bytes: wire.ReservedBytes}
	if err := validateSpoolQuota(quota); err != nil {
		return spoolQuota{}, err
	}
	return quota, nil
}

func validateSpoolQuota(quota spoolQuota) error {
	if quota.Events > maximumQuotaEventMarker {
		return errors.New("productmetrics: quota event counter exceeds its conservative marker")
	}
	if quota.Bytes > maximumQuotaByteMarker {
		return errors.New("productmetrics: quota byte counter exceeds its conservative marker")
	}
	return nil
}

func (quota spoolQuota) reserve(eventBytes uint64) (spoolQuota, error) {
	if eventBytes == 0 || eventBytes > maximumEventBytes {
		return spoolQuota{}, fmt.Errorf("productmetrics: event size %d is outside the spool limit", eventBytes)
	}
	if quota.Events >= maximumSpoolEvents || quota.Bytes > maximumSpoolBytes-eventBytes {
		return spoolQuota{}, errSpoolQuotaFull
	}
	events, ok := checkedAddUint64(quota.Events, 1)
	if !ok {
		return spoolQuota{}, errors.New("productmetrics: quota event counter overflow")
	}
	bytes, ok := checkedAddUint64(quota.Bytes, eventBytes)
	if !ok {
		return spoolQuota{}, errors.New("productmetrics: quota byte counter overflow")
	}
	return spoolQuota{Events: events, Bytes: bytes}, nil
}

func (quota spoolQuota) release(events, bytes uint64) (spoolQuota, error) {
	if events > quota.Events || bytes > quota.Bytes {
		return spoolQuota{}, errors.New("productmetrics: quota release would underflow")
	}
	return spoolQuota{Events: quota.Events - events, Bytes: quota.Bytes - bytes}, nil
}

func checkedAddUint64(left, right uint64) (uint64, bool) {
	if right > math.MaxUint64-left {
		return 0, false
	}
	return left + right, true
}

func loadSpoolQuota(root *storageRoot) (spoolQuota, bool, error) {
	return loadSpoolQuotaWithGate(root, nil)
}

func loadSpoolQuotaClockFree(root *storageRoot) (spoolQuota, bool, error) {
	if root == nil || root.storageDir == nil {
		return spoolQuota{}, false, errStorageClosed
	}
	data, err := root.readFileClockFree(quotaFileName, maximumQuotaBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return spoolQuota{}, false, nil
	}
	if err != nil {
		return spoolQuota{}, false, err
	}
	quota, err := decodeSpoolQuota(data)
	return quota, err == nil, err
}

func loadSpoolQuotaWithGate(root *storageRoot, canStart func(recordOperation) bool) (spoolQuota, bool, error) {
	return loadSpoolQuotaForOperation(root, canStart, recordOperationQuotaRead)
}

func loadSpoolQuotaForOperation(root *storageRoot, canStart func(recordOperation) bool, operation recordOperation) (spoolQuota, bool, error) {
	if root == nil || root.storageDir == nil {
		return spoolQuota{}, false, errStorageClosed
	}
	if !recordOperationCanStart(canStart, operation) {
		return spoolQuota{}, false, errRecordDecisionWindowExpired
	}
	data, err := root.readFile(quotaFileName, maximumQuotaBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return spoolQuota{}, false, nil
	}
	if err != nil {
		return spoolQuota{}, false, err
	}
	quota, err := decodeSpoolQuota(data)
	return quota, err == nil, err
}

func persistSpoolQuota(root *storageRoot, quota spoolQuota) error {
	return persistSpoolQuotaWithMode(root, quota, false, nil)
}

func persistSpoolQuotaDirect(root *storageRoot, quota spoolQuota) error {
	if root == nil || root.storageDir == nil {
		return errStorageClosed
	}
	data, err := encodeSpoolQuota(quota)
	if err != nil {
		return err
	}
	return root.writeFileAtomic(quotaFileName, data)
}

func persistInitialSpoolQuota(root *storageRoot, quota spoolQuota) error {
	return persistSpoolQuotaWithMode(root, quota, true, nil)
}

func persistForegroundSpoolQuota(root *storageRoot, quota spoolQuota, noReplace bool, canStart func(recordOperation) bool) error {
	return persistSpoolQuotaWithMode(root, quota, noReplace, canStart)
}

func persistSpoolQuotaWithMode(root *storageRoot, quota spoolQuota, noReplace bool, canStart func(recordOperation) bool) error {
	if root == nil || root.storageDir == nil {
		return errStorageClosed
	}
	if !recordOperationCanStart(canStart, recordOperationControlOpen) {
		return errRecordDecisionWindowExpired
	}
	control, err := root.openDir([]string{spoolControlDirectoryName}, true)
	if err != nil {
		return err
	}
	if err := persistSpoolQuotaFromControlWithGate(root, control, quota, noReplace, canStart); err != nil {
		return errors.Join(err, control.Close())
	}
	closeErr := control.Close()
	if !recordOperationCanStart(canStart, recordOperationControlClean) {
		return errors.Join(closeErr, errRecordDecisionWindowExpired)
	}
	entry, lookupErr := root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(lookupErr, fs.ErrNotExist) {
		lookupErr = nil
	} else if lookupErr == nil {
		if !recordOperationCanStart(canStart, recordOperationControlRemove) {
			return errors.Join(closeErr, errRecordDecisionWindowExpired)
		}
		removeErr := root.removeEnumeratedCleanupDirectory(entry)
		if errors.Is(removeErr, errStorageDirectoryNotEmpty) || errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		lookupErr = removeErr
	}
	return errors.Join(closeErr, lookupErr)
}

func persistSpoolQuotaFromControl(root *storageRoot, control *storageDir, quota spoolQuota, noReplace bool) error {
	return persistSpoolQuotaFromControlWithGate(root, control, quota, noReplace, nil)
}

func persistSpoolQuotaFromControlWithGate(root *storageRoot, control *storageDir, quota spoolQuota, noReplace bool, canStart func(recordOperation) bool) error {
	if root == nil || root.storageDir == nil || control == nil || control.backend == nil {
		return errStorageClosed
	}
	data, err := encodeSpoolQuota(quota)
	if err != nil {
		return err
	}
	if !recordOperationCanStart(canStart, recordOperationQuotaStage) {
		return errRecordDecisionWindowExpired
	}
	staged, stageErr := control.writeFileAtomicOutcome(quotaStagingFileName, data)
	if staged.state != storageWriteAppliedDurable {
		if stageErr == nil {
			stageErr = errors.New("productmetrics: quota staging write was not durably applied")
		}
		return stageErr
	}
	if !recordOperationCanStart(canStart, recordOperationQuotaInstall) {
		return errRecordDecisionWindowExpired
	}
	var result storageRenameResult
	var renameErr error
	if noReplace {
		result, renameErr = control.renameFile(quotaStagingFileName, root.storageDir, quotaFileName)
	} else {
		result, renameErr = control.replaceFile(quotaStagingFileName, root.storageDir, quotaFileName)
	}
	if noReplace && result.state == storageRenameNotApplied && errors.Is(renameErr, errStorageDestinationExists) {
		installed, present, loadErr := loadSpoolQuotaClockFree(root)
		if loadErr != nil || !present || installed != quota {
			return errors.Join(renameErr, loadErr)
		}
		if syncErr := root.syncDirectory(); syncErr != nil {
			return syncErr
		}
		if removeErr := control.removeFileClockFree(quotaStagingFileName); removeErr != nil {
			return removeErr
		}
		result.state = storageRenameAppliedDurable
		renameErr = nil
	}
	if result.state != storageRenameAppliedDurable {
		if renameErr == nil {
			renameErr = errors.New("productmetrics: quota install was not durably applied")
		}
		return renameErr
	}
	return renameErr
}

func recordOperationCanStart(canStart func(recordOperation) bool, operation recordOperation) bool {
	return canStart == nil || canStart(operation)
}
