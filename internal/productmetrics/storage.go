package productmetrics

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/gastownhall/gascity/internal/gchome"
)

var (
	errStorageDestinationExists   = errors.New("productmetrics: rename destination already exists")
	errStorageEntryExists         = errors.New("productmetrics: storage entry already exists")
	errStorageEntryChanged        = errors.New("productmetrics: enumerated storage entry changed")
	errStorageEntryIsDirectory    = errors.New("productmetrics: storage entry is a directory")
	errStorageDirectoryNotEmpty   = errors.New("productmetrics: storage directory is not empty")
	errStorageExchangeAncestor    = errors.New("productmetrics: exchange target contains its source")
	errStorageExchangeSameEntry   = errors.New("productmetrics: exchange source and target are the same entry")
	errStorageExchangeUnsupported = errors.New("productmetrics: atomic directory exchange is unsupported")
	errStorageClosed              = errors.New("productmetrics: storage handle is closed")
	errStorageReadLimit           = errors.New("productmetrics: storage read limit exceeded")
	errStorageUnsafeRecordShape   = errors.New("productmetrics: storage record has an unsafe filesystem shape")
)

const (
	rootTempJournalDirectoryName      = ".pm-root-temp-journal"
	rootTempJournalMarkerMagic        = "GCPMRTJ1"
	rootTempJournalBoundState         = byte(0x02)
	rootTempJournalMarkerHeaderBytes  = 32
	maximumRootTempMarkerNameBytes    = 128
	maximumRootTempJournalMarkerBytes = rootTempJournalMarkerHeaderBytes + maximumRootTempMarkerNameBytes
	rootTempJournalMarkerReadLimit    = maximumRootTempJournalMarkerBytes + 1
)

type rootTempJournalMarkerState uint8

const (
	rootTempJournalMarkerInvalid rootTempJournalMarkerState = iota
	rootTempJournalMarkerIntent
	rootTempJournalMarkerBound
)

type rootTempJournalMarkerEvidence struct {
	state rootTempJournalMarkerState
	name  string
	temp  recordIncarnation
}

func encodeBoundRootTempJournalMarker(name string, temp recordIncarnation) ([]byte, error) {
	if !canonicalStorageTempName(name) || len(name) == 0 || len(name) > maximumRootTempMarkerNameBytes ||
		temp.dev == 0 || temp.ino == 0 {
		return nil, errors.New("productmetrics: invalid root temporary-file marker binding")
	}
	data := make([]byte, rootTempJournalMarkerHeaderBytes+len(name))
	copy(data[:8], rootTempJournalMarkerMagic)
	data[8] = rootTempJournalBoundState
	data[9] = byte(len(name))
	binary.BigEndian.PutUint64(data[16:24], temp.dev)
	binary.BigEndian.PutUint64(data[24:32], temp.ino)
	copy(data[rootTempJournalMarkerHeaderBytes:], name)
	return data, nil
}

func decodeRootTempJournalMarker(name string, data []byte) (rootTempJournalMarkerEvidence, error) {
	if !canonicalStorageTempName(name) || len(name) == 0 || len(name) > maximumRootTempMarkerNameBytes {
		return rootTempJournalMarkerEvidence{}, errors.New("productmetrics: invalid root temporary-file marker name")
	}
	if len(data) == 0 {
		return rootTempJournalMarkerEvidence{state: rootTempJournalMarkerIntent, name: name}, nil
	}
	if len(data) < rootTempJournalMarkerHeaderBytes || len(data) > maximumRootTempJournalMarkerBytes ||
		string(data[:8]) != rootTempJournalMarkerMagic || data[8] != rootTempJournalBoundState {
		return rootTempJournalMarkerEvidence{}, errors.New("productmetrics: malformed root temporary-file marker")
	}
	nameBytes := int(data[9])
	if nameBytes == 0 || nameBytes > maximumRootTempMarkerNameBytes ||
		len(data) != rootTempJournalMarkerHeaderBytes+nameBytes {
		return rootTempJournalMarkerEvidence{}, errors.New("productmetrics: malformed root temporary-file marker length")
	}
	for _, reserved := range data[10:16] {
		if reserved != 0 {
			return rootTempJournalMarkerEvidence{}, errors.New("productmetrics: malformed root temporary-file marker reserved bytes")
		}
	}
	boundName := string(data[rootTempJournalMarkerHeaderBytes:])
	temp := recordIncarnation{
		dev: binary.BigEndian.Uint64(data[16:24]),
		ino: binary.BigEndian.Uint64(data[24:32]),
	}
	if boundName != name || !canonicalStorageTempName(boundName) || temp.dev == 0 || temp.ino == 0 {
		return rootTempJournalMarkerEvidence{}, errors.New("productmetrics: invalid root temporary-file marker binding")
	}
	return rootTempJournalMarkerEvidence{state: rootTempJournalMarkerBound, name: boundName, temp: temp}, nil
}

type storageStep string

const (
	storageStepFileSync      storageStep = "file-sync"
	storageStepWrite         storageStep = "write"
	storageStepRename        storageStep = "rename"
	storageStepDelete        storageStep = "delete"
	storageStepEnumerate     storageStep = "enumerate"
	storageStepEntryStat     storageStep = "entry-stat"
	storageStepUnlink        storageStep = "unlink"
	storageStepRmdir         storageStep = "rmdir"
	storageStepDirectorySync storageStep = "directory-sync"
	storageStepMarkerCreate  storageStep = "marker-create"
	storageStepMarkerBind    storageStep = "marker-bind"
	storageStepLock          storageStep = "lock"
)

type storageEntryKind uint8

const (
	storageEntryOther storageEntryKind = iota
	storageEntryRegular
	storageEntryDirectory
)

type storageMetadata struct {
	uid               uint32
	mode              uint32
	nlink             uint64
	dev               uint64
	ino               uint64
	size              int64
	mtimeSeconds      int64
	mtimeNanoseconds  int64
	kind              storageEntryKind
	ownerOnly         bool
	physicalReadBytes uint64
}

type storageEntry struct {
	name      string
	nameBytes int
	metadata  storageMetadata
}

type storageRenameState uint8

const (
	storageRenameNotApplied storageRenameState = iota
	storageRenameAppliedSyncPending
	storageRenameAppliedDurable
)

type storageRenameResult struct {
	state storageRenameState
}

type storageWriteState uint8

const (
	storageWriteNotApplied storageWriteState = iota
	storageWriteAppliedSyncPending
	storageWriteAppliedDurable
)

type storageWriteResult struct {
	state storageWriteState
}

type recordIncarnation struct {
	dev uint64
	ino uint64
}

type storageRecordBackend interface {
	close() error
	metadata() (storageMetadata, error)
}

// storageRecordLease retains the validated descriptor for one exact atomic
// config record. Keeping that descriptor open prevents its inode from being
// reused while stale in-process authority still exists.
type storageRecordLease struct {
	mu                sync.Mutex
	backend           storageRecordBackend
	record            recordIncarnation
	physicalReadBytes uint64
}

func newStorageRecordLease(backend storageRecordBackend, metadata storageMetadata) *storageRecordLease {
	if backend == nil {
		return nil
	}
	lease := &storageRecordLease{
		backend:           backend,
		record:            recordIncarnation{dev: metadata.dev, ino: metadata.ino},
		physicalReadBytes: metadata.physicalReadBytes,
	}
	runtime.SetFinalizer(lease, func(retained *storageRecordLease) { _ = retained.Close() })
	return lease
}

func (lease *storageRecordLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	backend := lease.backend
	lease.backend = nil
	lease.mu.Unlock()
	if backend == nil {
		return nil
	}
	runtime.SetFinalizer(lease, nil)
	return backend.close()
}

func (lease *storageRecordLease) Valid() bool {
	if lease == nil {
		return false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.backend != nil
}

func (lease *storageRecordLease) incarnation() recordIncarnation {
	if lease == nil {
		return recordIncarnation{}
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.backend == nil {
		return recordIncarnation{}
	}
	return lease.record
}

func (lease *storageRecordLease) Matches(other *storageRecordLease) bool {
	if lease == nil || other == nil {
		return false
	}
	left := lease.incarnation()
	right := other.incarnation()
	return left != (recordIncarnation{}) && left == right
}

// storageTestHooks is deliberately package-private. No external construction
// path can weaken validation or inject filesystem behavior.
type storageTestHooks struct {
	beforeStep            func(storageStep) error
	beforeDirectoryOpen   func(string) error
	afterDirectoryAttempt func(string, error)
	beforeTempFileCreate  func(string)
	beforeMutation        func(storageStep, string)
	afterComponentOpen    func(string)
	afterDirectoryOpen    func(string)
	afterFileOpen         func(string)
	afterAtomicWrite      func(string, storageWriteState)
	afterRename           func(string, string, storageRenameState)
	beforeExchange        func() error
	afterExchange         func() error
	decisionGate          func() bool
	beforeMetadataAttempt func(string) error
	beforeRead            func(string)
	afterRead             func(string, int, int, error)
	metadata              func(string, storageMetadata) storageMetadata
}

func (hooks storageTestHooks) run(step storageStep) error {
	if hooks.beforeStep == nil {
		return nil
	}
	return hooks.beforeStep(step)
}

func (hooks storageTestHooks) markerBindingHooks() storageTestHooks {
	original := hooks.beforeStep
	if original == nil {
		return hooks
	}
	hooks.beforeStep = func(step storageStep) error {
		if step == storageStepWrite {
			step = storageStepMarkerBind
		}
		return original(step)
	}
	return hooks
}

func (hooks storageTestHooks) openedComponent(path string) {
	if hooks.afterComponentOpen != nil {
		hooks.afterComponentOpen(path)
	}
}

func (hooks storageTestHooks) openingDirectory(path string) error {
	if hooks.beforeDirectoryOpen == nil {
		return nil
	}
	return hooks.beforeDirectoryOpen(path)
}

func (hooks storageTestHooks) observedDirectoryAttempt(path string, err error) {
	if hooks.afterDirectoryAttempt != nil {
		hooks.afterDirectoryAttempt(path, err)
	}
}

func (hooks storageTestHooks) creatingTempFile(path string) {
	if hooks.beforeTempFileCreate != nil {
		hooks.beforeTempFileCreate(path)
	}
}

func (hooks storageTestHooks) openedDirectory(path string) {
	if hooks.afterDirectoryOpen != nil {
		hooks.afterDirectoryOpen(path)
	}
}

func (hooks storageTestHooks) openedFile(path string) {
	if hooks.afterFileOpen != nil {
		hooks.afterFileOpen(path)
	}
}

func (hooks storageTestHooks) wroteAtomic(path string, state storageWriteState) {
	if hooks.afterAtomicWrite != nil {
		hooks.afterAtomicWrite(path, state)
	}
}

func (hooks storageTestHooks) renamed(sourcePath, targetPath string, state storageRenameState) {
	if hooks.afterRename != nil {
		hooks.afterRename(sourcePath, targetPath, state)
	}
}

func (hooks storageTestHooks) inspect(path string, metadata storageMetadata) storageMetadata {
	if hooks.metadata != nil {
		return hooks.metadata(path, metadata)
	}
	return metadata
}

func (hooks storageTestHooks) canStartStorageWork() error {
	if hooks.decisionGate != nil && !hooks.decisionGate() {
		return errRecordDecisionWindowExpired
	}
	return nil
}

func (hooks storageTestHooks) observedRead(path string, requested, read int, err error) {
	if hooks.afterRead != nil {
		hooks.afterRead(path, requested, read, err)
	}
}

func (hooks storageTestHooks) startingRead(path string) {
	if hooks.beforeRead != nil {
		hooks.beforeRead(path)
	}
}

func (hooks storageTestHooks) preparingMutation(step storageStep, path string) {
	if hooks.beforeMutation != nil {
		hooks.beforeMutation(step, path)
	}
}

type storageDirectoryBackend interface {
	close() error
	openDir([]string, bool) (storageDirectoryBackend, error)
	readFile(string, int64) ([]byte, error)
	readFileLease(string, int64) ([]byte, storageRecordBackend, storageMetadata, error)
	readFileLeaseClockFree(string, int64) ([]byte, storageRecordBackend, storageMetadata, error)
	writeFileAtomic(string, []byte) error
	writeFileAtomicOutcome(string, []byte) (storageWriteResult, error)
	writeFileAtomicNoReplace(string, []byte) error
	removeFile(string) error
	removeFileClockFree(string) error
	removeFileMatching(string, recordIncarnation) error
	removeFileMatchingGuarded(string, recordIncarnation, func() error) error
	confirmEntryAbsent(string) error
	renameFile(string, storageDirectoryBackend, string) (storageRenameResult, error)
	replaceFile(string, storageDirectoryBackend, string) (storageRenameResult, error)
	renameEnumeratedEntry(storageEntry, storageDirectoryBackend, string) (storageRenameResult, error)
	renameEnumeratedDirectory(storageEntry, storageDirectoryBackend, string) (storageRenameResult, error)
	exchangeEnumeratedEntries(storageEntry, storageDirectoryBackend, storageEntry) (storageRenameResult, error)
	exchangeFilesMatching(string, recordIncarnation, storageDirectoryBackend, string, recordIncarnation) (storageRenameResult, error)
	syncDirectory() error
	iterateEntries() (storageIteratorBackend, error)
	firstEntryFromRetainedHandle() (storageEntry, error)
	lookupEntry(string) (storageEntry, error)
	validateFileMatching(string, recordIncarnation) error
	openEnumeratedCleanupDirectory(storageEntry) (storageDirectoryBackend, error)
	unlinkEnumeratedEntry(storageEntry) error
	removeEnumeratedDirectory(storageEntry) error
	removeEnumeratedCleanupDirectory(storageEntry) error
	acquireLock(context.Context, string) (storageLockBackend, error)
	cleanupOnlyHandle() bool
}

type storageDirectoryOpenHookInstaller interface {
	installDirectoryOpenHooks(func(string) error, func(string)) func()
}

type storageFileDescriptorLimitBackend interface {
	fileDescriptorSoftLimit() (uint64, error)
}

type storageIteratorBackend interface {
	next() (storageEntry, error)
	close() error
}

type storageLockBackend interface {
	release() error
}

type storageRoot struct {
	*storageDir
}

type storageDir struct {
	backend storageDirectoryBackend
}

type advisoryLock struct {
	backend storageLockBackend
}

type storageIterator struct {
	backend storageIteratorBackend
}

func openStorageRootReadOnly(home gchome.ProductUsageHome) (*storageRoot, error) {
	return openStorageRoot(home, false, storageTestHooks{})
}

func openStorageRootReadOnlyWithHooks(home gchome.ProductUsageHome, hooks storageTestHooks) (*storageRoot, error) {
	return openStorageRoot(home, false, hooks)
}

func openStorageRootMutable(home gchome.ProductUsageHome) (*storageRoot, error) {
	return openStorageRoot(home, true, storageTestHooks{})
}

func openStorageRootMutableWithHooks(home gchome.ProductUsageHome, hooks storageTestHooks) (*storageRoot, error) {
	return openStorageRoot(home, true, hooks)
}

func openStorageRoot(home gchome.ProductUsageHome, mutable bool, hooks storageTestHooks) (*storageRoot, error) {
	backend, err := platformOpenStorageRoot(home, mutable, hooks)
	if err != nil {
		return nil, err
	}
	return &storageRoot{storageDir: &storageDir{backend: backend}}, nil
}

func (directory *storageDir) Close() error {
	if directory == nil || directory.backend == nil {
		return nil
	}
	return directory.backend.close()
}

func (directory *storageDir) cleanupOnly() bool {
	return directory != nil && directory.backend != nil && directory.backend.cleanupOnlyHandle()
}

func (root *storageRoot) installDirectoryOpenHooks(before func(string) error, after func(string)) func() {
	if root == nil || root.backend == nil {
		return func() {}
	}
	installer, ok := root.backend.(storageDirectoryOpenHookInstaller)
	if !ok {
		return func() {}
	}
	return installer.installDirectoryOpenHooks(before, after)
}

func (directory *storageDir) openDir(components []string, create bool) (*storageDir, error) {
	if directory == nil || directory.backend == nil {
		return nil, errStorageClosed
	}
	for _, component := range components {
		if err := validateStorageName(component); err != nil {
			return nil, fmt.Errorf("productmetrics: invalid directory component: %w", err)
		}
	}
	backend, err := directory.backend.openDir(components, create)
	if err != nil {
		return nil, err
	}
	return &storageDir{backend: backend}, nil
}

func (directory *storageDir) readFile(name string, maximumBytes int64) ([]byte, error) {
	data, _, lease, err := directory.readFileMeasured(name, maximumBytes)
	if lease != nil {
		err = errors.Join(err, lease.Close())
	}
	return data, err
}

func (directory *storageDir) readFileMeasured(name string, maximumBytes int64) ([]byte, uint64, *storageRecordLease, error) {
	data, lease, err := directory.readFileLease(name, maximumBytes)
	if lease == nil {
		return data, 0, nil, err
	}
	return data, lease.physicalReadBytes, lease, err
}

func (directory *storageDir) readFileClockFree(name string, maximumBytes int64) ([]byte, error) {
	if directory == nil || directory.backend == nil {
		return nil, errStorageClosed
	}
	if err := validateStorageName(name); err != nil {
		return nil, err
	}
	if maximumBytes <= 0 {
		return nil, errors.New("productmetrics: read size limit must be positive")
	}
	data, backend, _, err := directory.backend.readFileLeaseClockFree(name, maximumBytes)
	if backend != nil {
		err = errors.Join(err, backend.close())
	}
	return data, err
}

func (directory *storageDir) readFileLease(name string, maximumBytes int64) ([]byte, *storageRecordLease, error) {
	if directory == nil || directory.backend == nil {
		return nil, nil, errStorageClosed
	}
	if err := validateStorageName(name); err != nil {
		return nil, nil, err
	}
	if maximumBytes <= 0 {
		return nil, nil, errors.New("productmetrics: read size limit must be positive")
	}
	data, backend, metadata, err := directory.backend.readFileLease(name, maximumBytes)
	return data, newStorageRecordLease(backend, metadata), err
}

func (directory *storageDir) writeFileAtomic(name string, data []byte) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	return directory.backend.writeFileAtomic(name, data)
}

func (directory *storageDir) writeFileAtomicOutcome(name string, data []byte) (storageWriteResult, error) {
	if directory == nil || directory.backend == nil {
		return storageWriteResult{state: storageWriteNotApplied}, errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return storageWriteResult{state: storageWriteNotApplied}, err
	}
	return directory.backend.writeFileAtomicOutcome(name, data)
}

func (directory *storageDir) writeFileAtomicNoReplace(name string, data []byte) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	return directory.backend.writeFileAtomicNoReplace(name, data)
}

func (directory *storageDir) removeFile(name string) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	return directory.backend.removeFile(name)
}

func (directory *storageDir) removeFileClockFree(name string) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	return directory.backend.removeFileClockFree(name)
}

func (directory *storageDir) removeFileMatchingLease(name string, lease *storageRecordLease) error {
	return directory.removeFileMatchingLeaseGuarded(name, lease, nil)
}

func (directory *storageDir) validateFileMatchingLease(name string, lease *storageRecordLease) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	incarnation := lease.incarnation()
	if incarnation == (recordIncarnation{}) {
		return errors.New("productmetrics: closed or invalid record lease for identity validation")
	}
	return directory.backend.validateFileMatching(name, incarnation)
}

func (directory *storageDir) removeFileMatchingLeaseGuarded(name string, lease *storageRecordLease, guard func() error) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	if lease == nil {
		return errors.New("productmetrics: missing record lease for identity-bound deletion")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.backend == nil || lease.record == (recordIncarnation{}) {
		return errors.New("productmetrics: closed or invalid record lease for identity-bound deletion")
	}
	if err := directory.backend.removeFileMatchingGuarded(name, lease.record, guard); err != nil {
		return err
	}
	metadata, err := lease.backend.metadata()
	if err != nil {
		return fmt.Errorf("productmetrics: inspect unlinked record lease: %w", err)
	}
	if metadata.dev != lease.record.dev || metadata.ino != lease.record.ino || metadata.nlink != 0 {
		return fmt.Errorf("%w: identity-bound deletion did not unlink the leased record", errStorageEntryChanged)
	}
	return nil
}

func (directory *storageDir) confirmEntryAbsent(name string) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return err
	}
	return directory.backend.confirmEntryAbsent(name)
}

func (directory *storageDir) renameFile(name string, target *storageDir, targetName string) (storageRenameResult, error) {
	if directory == nil || directory.backend == nil || target == nil || target.backend == nil {
		return storageRenameResult{state: storageRenameNotApplied}, errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	if err := validateMutableStorageName(targetName); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	return directory.backend.renameFile(name, target.backend, targetName)
}

func (directory *storageDir) replaceFile(name string, target *storageDir, targetName string) (storageRenameResult, error) {
	if directory == nil || directory.backend == nil || target == nil || target.backend == nil {
		return storageRenameResult{state: storageRenameNotApplied}, errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	if err := validateMutableStorageName(targetName); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	return directory.backend.replaceFile(name, target.backend, targetName)
}

func (directory *storageDir) renameEnumeratedDirectory(entry storageEntry, target *storageDir, targetName string) (storageRenameResult, error) {
	if directory == nil || directory.backend == nil || target == nil || target.backend == nil {
		return storageRenameResult{state: storageRenameNotApplied}, errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	if err := validateMutableStorageName(targetName); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	return directory.backend.renameEnumeratedDirectory(entry, target.backend, targetName)
}

func (directory *storageDir) renameEnumeratedEntry(entry storageEntry, target *storageDir, targetName string) (storageRenameResult, error) {
	if directory == nil || directory.backend == nil || target == nil || target.backend == nil {
		return storageRenameResult{state: storageRenameNotApplied}, errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	if err := validateMutableStorageName(targetName); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	return directory.backend.renameEnumeratedEntry(entry, target.backend, targetName)
}

func (directory *storageDir) exchangeEnumeratedEntries(source storageEntry, target *storageDir, targetEntry storageEntry) (storageRenameResult, error) {
	if directory == nil || directory.backend == nil || target == nil || target.backend == nil {
		return storageRenameResult{state: storageRenameNotApplied}, errStorageClosed
	}
	if err := validateEnumeratedEntry(source); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	if err := validateEnumeratedEntry(targetEntry); err != nil {
		return storageRenameResult{state: storageRenameNotApplied}, err
	}
	return directory.backend.exchangeEnumeratedEntries(source, target.backend, targetEntry)
}

// exchangeFilesMatchingLeases atomically swaps two exact leased private files.
// Both descriptors remain retained for the whole exchange, preventing either
// inode from being reused while its name-bound authority is revalidated.
func (directory *storageDir) exchangeFilesMatchingLeases(
	name string,
	lease *storageRecordLease,
	target *storageDir,
	targetName string,
	targetLease *storageRecordLease,
) (storageRenameResult, error) {
	notApplied := storageRenameResult{state: storageRenameNotApplied}
	if directory == nil || directory.backend == nil || target == nil || target.backend == nil {
		return notApplied, errStorageClosed
	}
	if err := validateMutableStorageName(name); err != nil {
		return notApplied, err
	}
	if err := validateMutableStorageName(targetName); err != nil {
		return notApplied, err
	}
	if lease == nil || targetLease == nil || lease == targetLease {
		return notApplied, errors.New("productmetrics: distinct source and target leases are required for file exchange")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	targetLease.mu.Lock()
	defer targetLease.mu.Unlock()
	if lease.backend == nil || targetLease.backend == nil ||
		lease.record == (recordIncarnation{}) || targetLease.record == (recordIncarnation{}) ||
		lease.record == targetLease.record {
		return notApplied, errors.New("productmetrics: invalid file leases for exact exchange")
	}
	return directory.backend.exchangeFilesMatching(name, lease.record, target.backend, targetName, targetLease.record)
}

func (directory *storageDir) syncDirectory() error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	return directory.backend.syncDirectory()
}

func (directory *storageDir) iterateEntries() (*storageIterator, error) {
	if directory == nil || directory.backend == nil {
		return nil, errStorageClosed
	}
	backend, err := directory.backend.iterateEntries()
	if err != nil {
		return nil, err
	}
	return &storageIterator{backend: backend}, nil
}

func (directory *storageDir) firstEntryFromRetainedHandle() (storageEntry, error) {
	if directory == nil || directory.backend == nil {
		return storageEntry{}, errStorageClosed
	}
	return directory.backend.firstEntryFromRetainedHandle()
}

func (iterator *storageIterator) Next() (storageEntry, error) {
	if iterator == nil || iterator.backend == nil {
		return storageEntry{}, errStorageClosed
	}
	return iterator.backend.next()
}

func (iterator *storageIterator) Close() error {
	if iterator == nil || iterator.backend == nil {
		return nil
	}
	return iterator.backend.close()
}

func (directory *storageDir) lookupEntry(name string) (storageEntry, error) {
	if directory == nil || directory.backend == nil {
		return storageEntry{}, errStorageClosed
	}
	if err := validateStorageName(name); err != nil {
		return storageEntry{}, err
	}
	return directory.backend.lookupEntry(name)
}

func (directory *storageDir) openEnumeratedCleanupDirectory(entry storageEntry) (*storageDir, error) {
	if directory == nil || directory.backend == nil {
		return nil, errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return nil, err
	}
	backend, err := directory.backend.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		return nil, err
	}
	return &storageDir{backend: backend}, nil
}

func (directory *storageDir) unlinkEnumeratedEntry(entry storageEntry) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return err
	}
	return directory.backend.unlinkEnumeratedEntry(entry)
}

func (directory *storageDir) removeEnumeratedDirectory(entry storageEntry) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return err
	}
	return directory.backend.removeEnumeratedDirectory(entry)
}

func (directory *storageDir) removeEnumeratedCleanupDirectory(entry storageEntry) error {
	if directory == nil || directory.backend == nil {
		return errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return err
	}
	return directory.backend.removeEnumeratedCleanupDirectory(entry)
}

func validateEnumeratedEntry(entry storageEntry) error {
	if entry.name == "" || entry.name == "." || entry.name == ".." || entry.nameBytes != len(entry.name) {
		return errors.New("productmetrics: invalid enumerated entry name")
	}
	for index := range len(entry.name) {
		if entry.name[index] == 0 || entry.name[index] == '/' {
			return errors.New("productmetrics: invalid enumerated entry name")
		}
	}
	return nil
}

func (directory *storageDir) acquireLock(ctx context.Context, name string) (*advisoryLock, error) {
	if directory == nil || directory.backend == nil {
		return nil, errStorageClosed
	}
	if ctx == nil {
		return nil, errors.New("productmetrics: lock context is nil")
	}
	if !isStorageLockName(name) {
		return nil, fmt.Errorf("productmetrics: unrecognized lock name %q", name)
	}
	backend, err := directory.backend.acquireLock(ctx, name)
	if err != nil {
		return nil, err
	}
	return &advisoryLock{backend: backend}, nil
}

func (lock *advisoryLock) Release() error {
	if lock == nil || lock.backend == nil {
		return nil
	}
	return lock.backend.release()
}

func validateStorageName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("productmetrics: invalid empty or relative storage name %q", name)
	}
	if len(name) > maximumStorageNameBytes {
		return fmt.Errorf("productmetrics: storage name exceeds 128 bytes")
	}
	for index := range len(name) {
		if name[index] < 0x21 || name[index] > 0x7e || name[index] == '/' || name[index] == '\\' {
			return fmt.Errorf("productmetrics: storage name contains a forbidden byte")
		}
	}
	return nil
}

func validateMutableStorageName(name string) error {
	if err := validateStorageName(name); err != nil {
		return err
	}
	if isStorageLockName(name) {
		return fmt.Errorf("productmetrics: stable lock inode %q cannot be replaced or removed", name)
	}
	return nil
}

func isStorageLockName(name string) bool {
	return name == "state.lock" || name == "uploader.lock"
}

func storagePathError(operation, path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("productmetrics: %s %q: %w", operation, path, fs.ErrNotExist)
	}
	return fmt.Errorf("productmetrics: %s %q: %w", operation, path, err)
}

func isCleanAbsoluteProductRoot(home gchome.ProductUsageHome) bool {
	path := home.Home().Path()
	return home.Home().Provenance().Stable() && filepath.IsAbs(path) &&
		filepath.Clean(path) == path && home.Root() == filepath.Join(path, "product-usage")
}
