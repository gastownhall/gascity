package productmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	queueDirectoryName    = "queue"
	inflightDirectoryName = "inflight"
	eventFileSuffix       = ".json"

	maximumEnumerationEvents        = uint64(5001)
	maximumCleanupEntries           = uint64(6000)
	maximumCleanupDirectories       = uint64(512)
	maximumCleanupReadBytes         = uint64(5 * 1024 * 1024)
	maximumCleanupNameBytes         = uint64(1024 * 1024)
	maximumFilesystemName           = uint64(255)
	spoolTraversalDirectoryEnvelope = uint64(2)
	// Reserve the post-traversal worst case: quota staging, post-quota journal
	// proof, final config write, journal replay, and mutation-free proof.
	spoolFixedDirectoryReserve  = uint64(5)
	spoolFileDescriptorHeadroom = uint64(4)
	spoolFallbackDirectoryLimit = uint64(32)
	// Three traversal envelopes cover the fixed root journal plus enough
	// ordinary descent to reach one nested child and mutate it. The remaining
	// reserve preserves post-traversal control and proof work.
	spoolMinimumDirectoryProgress = spoolFixedDirectoryReserve + 3*spoolTraversalDirectoryEnvelope
	// A root temp collision currently needs at most eight named operations:
	// journal open/revalidation, marker create/revalidation, temp create, and
	// marker inspect/revalidation/unlink. Reserve nine per possible attempt so
	// one additional operation cannot silently escape the shared cap, plus
	// 32 per relocation candidate and 64 for control/quota/cursor bookkeeping.
	// The root-temp journal proof reserves one enumeration sentinel entry.
	// Spawn-throttle and diagnostic-status cleanup each spend two additional
	// exact-name operations: one bounded identity-leased read and one
	// identity-bound unlink.
	spoolFixedEntryEnvelope = uint64(9*maximumStorageTempAttempts + 32*maximumRelocationSlots + 64 + 1 + 2 + 2)
	spoolFixedNameEnvelope  = spoolFixedEntryEnvelope * maximumStorageNameBytes
	// The relocation envelope covers quota read + conflict replay + quota
	// stage, followed by the control and root-fallback cursor read/stage pairs.
	// Writes share the same byte dimension as reads so neither direction can
	// escape the cap. The final terms cover bounded spawn-throttle and
	// diagnostic-status reads, including their one-byte limit probes.
	spoolFixedReadEnvelope = uint64(3*maximumQuotaBytes+4*maximumRelocationBytes+4) +
		3*maximumStorageTempAttempts*rootTempJournalMarkerReadLimit +
		maximumSpawnThrottleBytes + maximumDiagnosticStatusBytes + 2

	defaultRecordDecisionBudget = 50 * time.Millisecond
	canonicalHourLayout         = "2006-01-02T15:04:05Z"
)

var errUnrecognizedMetricsRootEntry = errors.New("productmetrics: metrics root contains an unrecognized entry")

var errUnsettledRootTempJournal = errors.New("productmetrics: root temporary-file journal is not settled")

// RecordResult is the deliberately small outcome of a best-effort recording
// attempt. Metrics failures never surface as command failures.
type RecordResult uint8

const (
	// RecordDropped means the first attempt was ineligible or could not be
	// made durable within the fixed bounds.
	RecordDropped RecordResult = iota
	// RecordStored means exactly one immutable event file is durable.
	RecordStored
)

type recordDecisionWindow struct {
	started time.Time
	now     func() time.Time
	limit   time.Duration
}

type recordOperation string

const (
	recordOperationQuotaRead          recordOperation = "quota-read"
	recordOperationControlOpen        recordOperation = "control-open"
	recordOperationQuotaStage         recordOperation = "quota-stage"
	recordOperationQuotaInstall       recordOperation = "quota-install"
	recordOperationQuotaReplay        recordOperation = "quota-replay-read"
	recordOperationQuotaSync          recordOperation = "quota-replay-sync"
	recordOperationStageCleanup       recordOperation = "quota-stage-cleanup"
	recordOperationControlClean       recordOperation = "control-cleanup"
	recordOperationControlRemove      recordOperation = "control-remove"
	recordOperationQueueOpen          recordOperation = "queue-open"
	recordOperationGenerationOpen     recordOperation = "generation-open"
	recordOperationEventWrite         recordOperation = "event-write"
	recordOperationStatusRead         recordOperation = "status-read"
	recordOperationStatusWrite        recordOperation = "status-write"
	recordOperationSpawnThrottleRead  recordOperation = "spawn-throttle-read"
	recordOperationSpawnToken         recordOperation = "spawn-token"
	recordOperationSpawnThrottleWrite recordOperation = "spawn-throttle-write"
	recordOperationSpawnPrepare       recordOperation = "spawn-prepare"
	recordOperationSpawnStart         recordOperation = "spawn-start"
)

func recordLookupOperation(name string) recordOperation {
	return recordOperation("lookup:" + name)
}

func (window recordDecisionWindow) remaining() (time.Duration, bool) {
	current := window.now()
	if current.Before(window.started) {
		return 0, false
	}
	elapsed := current.Sub(window.started)
	if elapsed < 0 || elapsed >= window.limit {
		return 0, false
	}
	return window.limit - elapsed, true
}

func depsHourUTC(value time.Time) string {
	return value.UTC().Truncate(time.Hour).Format(canonicalHourLayout)
}

func parseCanonicalHourUTC(value string) (time.Time, error) {
	parsed, err := time.Parse(canonicalHourLayout, value)
	if err != nil || parsed.Format(canonicalHourLayout) != value || parsed.Minute() != 0 || parsed.Second() != 0 || parsed.Nanosecond() != 0 {
		return time.Time{}, errors.New("productmetrics: occurrence is not a canonical UTC hour")
	}
	return parsed, nil
}

func operatingSystemForRuntime() OperatingSystem {
	switch runtime.GOOS {
	case "linux":
		return OSLinux
	case "darwin":
		return OSDarwin
	default:
		return ""
	}
}

// RecordOnce consumes the invocation's first recording attempt regardless of
// whether it succeeds. It revalidates the exact config record under state.lock,
// durably reserves root-global quota, and then installs one immutable file.
func (service *Service) RecordOnce(permit RecordingPermit, commandID CommandID) RecordResult {
	if service == nil || !service.recordAttempt.CompareAndSwap(false, true) {
		return RecordDropped
	}
	if !permit.Valid() || permit.releaseVersion != service.deps.release.releaseVersion ||
		permit.metricsEpoch != service.deps.release.metricsEpoch ||
		permit.operatingSystem == "" || permit.operatingSystem != operatingSystemForRuntime() {
		return RecordDropped
	}
	if _, err := commandIDWire(commandID, productionCommandIDCatalog); err != nil {
		return RecordDropped
	}
	occurred, err := parseCanonicalHourUTC(permit.occurredHourUTC)
	if err != nil {
		return RecordDropped
	}

	started := service.deps.now()
	if !eventWithinRetention(occurred, started) {
		return RecordDropped
	}
	window := recordDecisionWindow{started: started, now: service.deps.now, limit: defaultRecordDecisionBudget}
	eventID, err := service.deps.newUUID()
	if err != nil || !validCanonicalUUIDv4(eventID) {
		return RecordDropped
	}
	event := Event{
		EventID:         eventID,
		InstallationID:  permit.installationID,
		App:             AppGasCity,
		ReleaseVersion:  permit.releaseVersion,
		OS:              permit.operatingSystem,
		OccurredHourUTC: permit.occurredHourUTC,
		CommandID:       commandID,
	}
	encoded, err := EncodeEvent(event)
	if err != nil || len(encoded) == 0 || uint64(len(encoded)) > maximumEventBytes {
		return RecordDropped
	}
	if _, ok := window.remaining(); !ok {
		return RecordDropped
	}

	storageHooks := service.deps.storageHooks
	existingStorageGate := storageHooks.decisionGate
	storageHooks.decisionGate = func() bool {
		if existingStorageGate != nil && !existingStorageGate() {
			return false
		}
		_, ok := window.remaining()
		return ok
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, storageHooks)
	if err != nil {
		return RecordDropped
	}
	defer func() { _ = root.Close() }()
	remaining, ok := window.remaining()
	if !ok {
		return RecordDropped
	}
	lockContext, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	lock, err := root.acquireLock(lockContext, stateLockName)
	if err != nil {
		return RecordDropped
	}
	defer func() { _ = lock.Release() }()
	if _, ok := window.remaining(); !ok {
		return RecordDropped
	}

	loaded := loadStateFromDirectory(root)
	defer func() { _ = loaded.Close() }()
	if loaded.err != nil || !loaded.present || loaded.lease == nil ||
		!permit.recordLease.Matches(loaded.lease) || !stateMatchesPermit(loaded.state, permit) ||
		service.project(InvocationContext{}, loaded).state != StateEnabled {
		return RecordDropped
	}
	canStart := func(operation recordOperation) bool {
		if service.deps.beforeRecordOperation != nil {
			service.deps.beforeRecordOperation(operation)
		}
		_, ok := window.remaining()
		return ok
	}
	diagnosticStorageSafe := false
	authorizedDrop := func(class DiagnosticErrorClass) RecordResult {
		if diagnosticStorageSafe {
			service.bestEffortUpdateDiagnosticStatusLocked(root, diagnosticStatusUpdate{
				incrementDroppedEvents: true,
				lastErrorClass:         class,
			}, canStart)
		}
		return RecordDropped
	}
	if _, ok := window.remaining(); !ok {
		return authorizedDrop(DiagnosticErrorLockTimeout)
	}
	quota, present, err := loadForegroundSpoolQuota(root, canStart)
	if err != nil {
		return authorizedDrop(diagnosticClassForStorageError(err))
	}
	// Conservative quota markers and control/cursor residue are cleanup
	// barriers, not ordinary capacity failures. Never create status.toml while
	// that evidence is active: doing so can make bounded cleanup alternate
	// forever between removing the status record and repairing the barrier.
	diagnosticStorageSafe = quota.Events <= maximumSpoolEvents && quota.Bytes <= maximumSpoolBytes
	reserved, err := quota.reserve(uint64(len(encoded)))
	if err != nil {
		return authorizedDrop(diagnosticClassForStorageError(err))
	}
	if _, ok := window.remaining(); !ok {
		return authorizedDrop(DiagnosticErrorLockTimeout)
	}
	if err := persistForegroundSpoolQuota(root, reserved, !present, canStart); err != nil {
		// Failed quota persistence may leave fail-closed control evidence. Do not
		// add a second root mutation until reconciliation has made it safe.
		diagnosticStorageSafe = false
		return authorizedDrop(diagnosticClassForStorageError(err))
	}
	if !canStart(recordOperationQueueOpen) {
		return authorizedDrop(DiagnosticErrorLockTimeout)
	}
	queueRoot, err := root.openDir([]string{queueDirectoryName}, true)
	if err != nil {
		return authorizedDrop(diagnosticClassForStorageError(err))
	}
	defer func() { _ = queueRoot.Close() }()
	if !canStart(recordOperationGenerationOpen) {
		return authorizedDrop(DiagnosticErrorLockTimeout)
	}
	queue, err := queueRoot.openDir([]string{permit.spoolGeneration}, true)
	if err != nil {
		return authorizedDrop(diagnosticClassForStorageError(err))
	}
	defer func() { _ = queue.Close() }()
	if !canStart(recordOperationEventWrite) {
		return authorizedDrop(DiagnosticErrorLockTimeout)
	}
	if err := queue.writeFileAtomicNoReplace(eventFileName(eventID), encoded); err != nil {
		// The durable reservation deliberately remains. This crash/failure
		// window can overcount but can never admit an event past the cap.
		return authorizedDrop(diagnosticClassForStorageError(err))
	}
	spawnDependencies := service.deps.spawn
	if spawnDependencies.executable == nil || spawnDependencies.environ == nil || spawnDependencies.start == nil {
		return RecordStored
	}
	if _, ok := window.remaining(); !ok {
		return RecordStored
	}
	attemptedAt := service.deps.now().UTC()
	reservation, reservedSpawn, reservationErr := service.reserveSpawnAttemptAtRoot(root, attemptedAt, canStart)
	if reservationErr != nil || !reservedSpawn {
		return RecordStored
	}
	// Process creation must not inherit any transaction authority. Close every
	// directory, exact-config lease, and state lock opened by this operation
	// before even resolving the executable/environment or calling Start.
	if err := errors.Join(queue.Close(), queueRoot.Close(), loaded.Close(), lock.Release(), root.Close(), permit.Close()); err != nil {
		return RecordStored
	}
	_ = service.startReservedUploader(reservation, spawnDependencies, canStart)
	return RecordStored
}

func loadForegroundSpoolQuota(root *storageRoot, canStart func(recordOperation) bool) (spoolQuota, bool, error) {
	quota, present, err := loadSpoolQuotaWithGate(root, canStart)
	if err != nil {
		return spoolQuota{}, false, err
	}
	names := []string{spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName}
	if !present {
		names = []string{queueDirectoryName, inflightDirectoryName, spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName}
	}
	for _, name := range names {
		if !recordOperationCanStart(canStart, recordLookupOperation(name)) {
			return spoolQuota{}, false, errRecordDecisionWindowExpired
		}
		_, err := root.lookupEntry(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return spoolQuota{}, false, err
		}
		return spoolQuota{}, false, fmt.Errorf("productmetrics: conservative spool evidence %q is present", name)
	}
	return quota, present, nil
}

func eventFileName(eventID string) string {
	return eventID + eventFileSuffix
}

func eventIDFromFileName(name string) (string, bool) {
	if len(name) != 36+len(eventFileSuffix) || !strings.HasSuffix(name, eventFileSuffix) {
		return "", false
	}
	id := strings.TrimSuffix(name, eventFileSuffix)
	return id, validCanonicalUUIDv4(id)
}

type spoolWorkBudget struct {
	maxEntries     uint64
	maxDirectories uint64
	maxReadBytes   uint64
	maxNameBytes   uint64
}

func defaultSpoolWorkBudget() spoolWorkBudget {
	return spoolWorkBudget{
		maxEntries:     maximumCleanupEntries,
		maxDirectories: maximumCleanupDirectories,
		maxReadBytes:   maximumCleanupReadBytes,
		maxNameBytes:   maximumCleanupNameBytes,
	}
}

type spoolWorkUsage struct {
	entries     uint64
	directories uint64
	readBytes   uint64
	nameBytes   uint64
}

type spoolWorkMeter struct {
	budget                  spoolWorkBudget
	usage                   spoolWorkUsage
	eventEntries            uint64
	exhausted               bool
	traversalError          error
	physicalDirectories     bool
	fixedDirectoryPermits   uint64
	cleanupDirectoryPermits uint64
	fixedEnvelopeClaimed    bool
	rootTempJournalMarkers  uint64
	rootTempJournalSentinel bool
}

func newSpoolWorkMeter(budget spoolWorkBudget) *spoolWorkMeter {
	if budget.maxEntries == 0 || budget.maxDirectories == 0 || budget.maxNameBytes == 0 {
		return &spoolWorkMeter{budget: budget, exhausted: true}
	}
	return &spoolWorkMeter{budget: budget}
}

func (meter *spoolWorkMeter) chargeDirectory() bool {
	if meter.physicalDirectories {
		ordinaryLimit := meter.ordinaryDirectoryLimit()
		if meter.exhausted || ordinaryLimit < meter.usage.directories ||
			ordinaryLimit-meter.usage.directories < spoolTraversalDirectoryEnvelope {
			meter.exhausted = true
			return false
		}
		return true
	}
	if meter.exhausted || meter.budget.maxDirectories == 0 ||
		meter.usage.directories >= meter.budget.maxDirectories-1 {
		meter.exhausted = true
		return false
	}
	meter.usage.directories++
	return true
}

// chargeFixedDirectory spends the one directory slot reserved for bounded
// control recovery after ordinary traversal has stopped at the directory cap.
func (meter *spoolWorkMeter) chargeFixedDirectory() bool {
	if meter.physicalDirectories {
		if meter.fixedDirectoryPermits > 0 {
			return true
		}
		if meter.usage.directories >= meter.budget.maxDirectories {
			meter.exhausted = true
			return false
		}
		meter.fixedDirectoryPermits++
		return true
	}
	if meter.usage.directories >= meter.budget.maxDirectories {
		// Legacy logical-only meters do not observe the actual fixed open.
		// Physical sweeps above remain strictly capped; direct state-machine
		// tests may reuse the already-reserved fixed slot at the logical cap.
		return true
	}
	meter.usage.directories++
	return true
}

// chargeFixedTraversalDirectory reserves the two physical opens used to
// retain a directory and create its independent iterator. Both consume the
// same root-global directory cap.
func (meter *spoolWorkMeter) chargeFixedTraversalDirectory() bool {
	if meter == nil {
		return false
	}
	if !meter.physicalDirectories {
		return meter.chargeDirectory()
	}
	if meter.fixedDirectoryPermits > 0 {
		return true
	}
	if meter.usage.directories > meter.budget.maxDirectories ||
		spoolTraversalDirectoryEnvelope > meter.budget.maxDirectories-meter.usage.directories {
		meter.exhausted = true
		return false
	}
	meter.fixedDirectoryPermits += spoolTraversalDirectoryEnvelope
	return true
}

func (meter *spoolWorkMeter) chargeCleanupDirectory() bool {
	if meter != nil && meter.physicalDirectories {
		ordinaryLimit := meter.ordinaryDirectoryLimit()
		if ordinaryLimit < meter.usage.directories ||
			ordinaryLimit-meter.usage.directories < spoolTraversalDirectoryEnvelope {
			meter.exhausted = true
			return false
		}
		meter.cleanupDirectoryPermits += spoolTraversalDirectoryEnvelope
		return true
	}
	return meter != nil && meter.chargeDirectory()
}

func (meter *spoolWorkMeter) ordinaryDirectoryLimit() uint64 {
	if meter == nil {
		return 0
	}
	reserve := spoolFixedDirectoryReserve
	// Explicit tiny budgets remain progress-capable: leave one traversal
	// envelope for priority cleanup and reserve only the remainder. Such a
	// pass cannot certify final success, but it can make bounded progress.
	if meter.budget.maxDirectories < reserve+spoolTraversalDirectoryEnvelope {
		if meter.budget.maxDirectories <= spoolTraversalDirectoryEnvelope {
			reserve = 0
		} else {
			reserve = meter.budget.maxDirectories - spoolTraversalDirectoryEnvelope
		}
	}
	return meter.budget.maxDirectories - reserve
}

func (meter *spoolWorkMeter) beforePhysicalDirectoryOpen(string) error {
	if meter == nil {
		return errStorageClosed
	}
	limit := meter.ordinaryDirectoryLimit()
	if meter.fixedDirectoryPermits > 0 {
		limit = meter.budget.maxDirectories
	}
	if meter.usage.directories >= limit {
		meter.exhausted = true
		return errors.New("productmetrics: physical directory-open budget is exhausted")
	}
	return nil
}

func (meter *spoolWorkMeter) afterPhysicalDirectoryOpen(string) {
	if meter == nil {
		return
	}
	if meter.usage.directories < math.MaxUint64 {
		meter.usage.directories++
	}
	if meter.fixedDirectoryPermits > 0 {
		meter.fixedDirectoryPermits--
	} else if meter.cleanupDirectoryPermits > 0 {
		meter.cleanupDirectoryPermits--
	}
}

func (meter *spoolWorkMeter) refundLogicalDirectoryCharge() {
	if meter != nil && !meter.physicalDirectories && meter.usage.directories > 0 {
		meter.usage.directories--
	}
}

func (meter *spoolWorkMeter) next(iterator *storageIterator) (storageEntry, bool) {
	entryLimit := meter.ordinaryEntryLimit()
	nameLimit := meter.ordinaryNameLimit()
	if meter.exhausted || meter.usage.entries >= entryLimit ||
		nameLimit < maximumFilesystemName || meter.usage.nameBytes > nameLimit-maximumFilesystemName {
		meter.exhausted = true
		return storageEntry{}, false
	}
	entry, err := iterator.Next()
	if errors.Is(err, io.EOF) {
		return storageEntry{}, false
	}
	if err != nil {
		meter.traversalError = errors.Join(meter.traversalError, err)
		return storageEntry{}, false
	}
	nameBytes := uint64(entry.nameBytes)
	if nameBytes > nameLimit-meter.usage.nameBytes {
		meter.exhausted = true
		return storageEntry{}, false
	}
	meter.usage.entries++
	meter.usage.nameBytes += nameBytes
	return entry, true
}

func (meter *spoolWorkMeter) chargeEventEntry() bool {
	if meter.eventEntries >= maximumEnumerationEvents {
		meter.exhausted = true
		return false
	}
	meter.eventEntries++
	return true
}

func (meter *spoolWorkMeter) chargeRead(bytes uint64) bool {
	limit := meter.ordinaryReadLimit()
	if meter.exhausted || meter.usage.readBytes > limit || bytes > limit-meter.usage.readBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.readBytes += bytes
	return true
}

func (meter *spoolWorkMeter) refundRead(reserved, used uint64) {
	if meter == nil || used > reserved || meter.usage.readBytes < reserved-used {
		return
	}
	meter.usage.readBytes -= reserved - used
}

func (meter *spoolWorkMeter) chargeNamedEntry(name string) bool {
	nameBytes := uint64(len(name))
	entryLimit := meter.ordinaryEntryLimit()
	nameLimit := meter.ordinaryNameLimit()
	if meter.exhausted || meter.usage.entries >= entryLimit || meter.usage.nameBytes > nameLimit ||
		nameBytes > nameLimit-meter.usage.nameBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.entries++
	meter.usage.nameBytes += nameBytes
	return true
}

// chargeFixedEntry charges one exact fd-relative lookup after traversal has
// stopped on the directory cap. It never relaxes the entry or name budgets.
func (meter *spoolWorkMeter) chargeFixedEntry(name string) bool {
	if meter.physicalDirectories {
		return meter.claimFixedWorkEnvelope()
	}
	nameBytes := uint64(len(name))
	if meter.usage.entries >= meter.budget.maxEntries || nameBytes > meter.budget.maxNameBytes ||
		meter.usage.nameBytes > meter.budget.maxNameBytes-nameBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.entries++
	meter.usage.nameBytes += nameBytes
	return true
}

func (meter *spoolWorkMeter) availableFixedSlots(nameBytes uint64, maximum int) int {
	if meter != nil && meter.physicalDirectories {
		if meter.claimFixedWorkEnvelope() {
			return maximum
		}
		return 0
	}
	if meter == nil || maximum <= 0 || nameBytes == 0 || meter.usage.entries >= meter.budget.maxEntries ||
		meter.usage.nameBytes >= meter.budget.maxNameBytes {
		return 0
	}
	byEntries := meter.budget.maxEntries - meter.usage.entries
	byNames := (meter.budget.maxNameBytes - meter.usage.nameBytes) / nameBytes
	available := min(byEntries, byNames, uint64(maximum))
	return int(available)
}

func (meter *spoolWorkMeter) chargeFixedRead(bytes uint64) bool {
	if meter != nil && meter.physicalDirectories {
		return meter.claimFixedWorkEnvelope()
	}
	if meter == nil || meter.usage.readBytes > meter.budget.maxReadBytes ||
		bytes > meter.budget.maxReadBytes-meter.usage.readBytes {
		if meter != nil {
			meter.exhausted = true
		}
		return false
	}
	meter.usage.readBytes += bytes
	return true
}

func (meter *spoolWorkMeter) acceptRootTempJournalMarker() bool {
	if meter == nil || meter.rootTempJournalMarkers >= maximumStorageTempAttempts {
		if meter != nil {
			meter.exhausted = true
		}
		return false
	}
	meter.rootTempJournalMarkers++
	return true
}

func (meter *spoolWorkMeter) reserveRootTempJournalSentinel(fixed bool) bool {
	if meter == nil {
		return false
	}
	if meter.rootTempJournalSentinel {
		return true
	}
	if fixed && meter.physicalDirectories {
		if !meter.claimFixedWorkEnvelope() {
			return false
		}
		meter.rootTempJournalSentinel = true
		return true
	}
	entryLimit := meter.ordinaryEntryLimit()
	nameLimit := meter.ordinaryNameLimit()
	if meter.exhausted || meter.usage.entries >= entryLimit || meter.usage.nameBytes > nameLimit ||
		maximumStorageNameBytes > nameLimit-meter.usage.nameBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.entries++
	meter.usage.nameBytes += maximumStorageNameBytes
	meter.rootTempJournalSentinel = true
	return true
}

func (meter *spoolWorkMeter) ordinaryEntryLimit() uint64 {
	if meter == nil || !meter.physicalDirectories {
		if meter == nil {
			return 0
		}
		return meter.budget.maxEntries
	}
	if meter.budget.maxEntries <= spoolFixedEntryEnvelope {
		return 0
	}
	if meter.fixedEnvelopeClaimed {
		return meter.budget.maxEntries
	}
	return meter.budget.maxEntries - spoolFixedEntryEnvelope
}

func (meter *spoolWorkMeter) ordinaryNameLimit() uint64 {
	if meter == nil || !meter.physicalDirectories {
		if meter == nil {
			return 0
		}
		return meter.budget.maxNameBytes
	}
	if meter.budget.maxNameBytes <= spoolFixedNameEnvelope {
		return 0
	}
	if meter.fixedEnvelopeClaimed {
		return meter.budget.maxNameBytes
	}
	return meter.budget.maxNameBytes - spoolFixedNameEnvelope
}

func (meter *spoolWorkMeter) ordinaryReadLimit() uint64 {
	if meter == nil || !meter.physicalDirectories {
		if meter == nil {
			return 0
		}
		return meter.budget.maxReadBytes
	}
	if meter.budget.maxReadBytes <= spoolFixedReadEnvelope {
		return 0
	}
	if meter.fixedEnvelopeClaimed {
		return meter.budget.maxReadBytes
	}
	return meter.budget.maxReadBytes - spoolFixedReadEnvelope
}

func (meter *spoolWorkMeter) claimFixedWorkEnvelope() bool {
	if meter == nil {
		return false
	}
	if meter.fixedEnvelopeClaimed {
		return true
	}
	if meter.usage.entries > meter.budget.maxEntries ||
		spoolFixedEntryEnvelope > meter.budget.maxEntries-meter.usage.entries ||
		meter.usage.nameBytes > meter.budget.maxNameBytes ||
		spoolFixedNameEnvelope > meter.budget.maxNameBytes-meter.usage.nameBytes ||
		meter.usage.readBytes > meter.budget.maxReadBytes ||
		spoolFixedReadEnvelope > meter.budget.maxReadBytes-meter.usage.readBytes {
		meter.exhausted = true
		return false
	}
	meter.usage.entries += spoolFixedEntryEnvelope
	meter.usage.nameBytes += spoolFixedNameEnvelope
	meter.usage.readBytes += spoolFixedReadEnvelope
	meter.fixedEnvelopeClaimed = true
	return true
}

type spoolPolicy struct {
	generation     string
	installationID string
}

func policyFromPermit(permit RecordingPermit) spoolPolicy {
	return spoolPolicy{generation: permit.spoolGeneration, installationID: permit.installationID}
}

type spoolRecord struct {
	tree             string
	generation       string
	name             string
	event            Event
	bytes            uint64
	incarnation      recordIncarnation
	mtimeSeconds     int64
	mtimeNanoseconds int64
}

type spoolClaim struct {
	generation string
	records    []spoolRecord
	authority  *spoolClaimAuthority
}

type spoolClaimAuthority struct {
	mu      sync.Mutex
	settled bool
}

func (claim spoolClaim) beginSettlement() (func(), error) {
	if len(claim.records) == 0 {
		return func() {}, nil
	}
	if claim.authority == nil {
		return nil, errors.New("productmetrics: spool claim has no settlement authority")
	}
	claim.authority.mu.Lock()
	if claim.authority.settled {
		claim.authority.mu.Unlock()
		return nil, errors.New("productmetrics: spool claim is already settled")
	}
	claim.authority.settled = true
	return claim.authority.mu.Unlock, nil
}

func (claim spoolClaim) events() []Event {
	events := make([]Event, len(claim.records))
	for index := range claim.records {
		events[index] = claim.records[index].event
	}
	return events
}

type spoolSweepResult struct {
	complete      bool
	usage         spoolWorkUsage
	eventEntries  uint64
	meter         *spoolWorkMeter
	quota         spoolQuota
	removedEvents uint64
	removedBytes  uint64
}

type spoolSweepState struct {
	root                       *storageRoot
	policy                     spoolPolicy
	now                        time.Time
	purgeAll                   bool
	meter                      *spoolWorkMeter
	quota                      spoolQuota
	records                    []spoolRecord
	seen                       map[string]struct{}
	pruneDirs                  map[string]*storageDir
	removedEvents              uint64
	removedBytes               uint64
	operation                  error
	traversed                  bool
	mutated                    bool
	afterRelocationReservation func() error
	relocationQuotaMarked      bool
	restoreDirectoryOpenHooks  func()
	retainedControl            *storageDir
	retainedRetiredControl     *storageDir
	failClosedArmed            bool
	durableQuotaMarker         bool
	journalSettled             bool
	journalFixedDirectory      bool
}

// reconcileSpool is a caller-held-state.lock primitive. It may lower durable
// quota only after one bounded traversal has accounted for the complete tree;
// otherwise it installs overflow markers so foreground recording stays closed.
func reconcileSpool(root *storageRoot, policy spoolPolicy, now time.Time, budget spoolWorkBudget) (spoolSweepResult, error) {
	state := runSpoolSweep(root, policy, now, budget, false)
	return state.finish()
}

// purgeSpool is a caller-held-state.lock primitive. Disable/pause callers also
// hold uploader.lock first. A complete result is a durable proof that every
// queue/inflight generation is empty and quota.toml is durably zero.
func purgeSpool(root *storageRoot, budget spoolWorkBudget) (spoolSweepResult, error) {
	state := runSpoolSweep(root, spoolPolicy{}, time.Time{}, budget, true)
	return state.finish()
}

// purgeSpoolWithinBudget retries mutation-only purge passes with one shared
// meter until a mutation-free pass proves the exact root clean. The aggregate
// invocation never replenishes any cleanup-work dimension between passes.
func purgeSpoolWithinBudget(root *storageRoot, budget spoolWorkBudget) (spoolSweepResult, error) {
	budget = constrainSpoolDirectoryBudget(root, budget)
	meter := newSpoolWorkMeter(budget)
	aggregate := spoolSweepResult{}
	for {
		beforeUsage := meter.usage
		beforeEventEntries := meter.eventEntries
		state := runSpoolSweepWithMeter(root, spoolPolicy{}, time.Time{}, meter, true)
		result, err := state.finish()
		aggregate.complete = result.complete
		aggregate.usage = result.usage
		aggregate.eventEntries = result.eventEntries
		aggregate.meter = result.meter
		aggregate.quota = result.quota
		aggregate.removedEvents = saturatingAddUint64(aggregate.removedEvents, result.removedEvents)
		aggregate.removedBytes = saturatingAddUint64(aggregate.removedBytes, result.removedBytes)
		if err != nil || result.complete || meter.exhausted || !state.mutated {
			return aggregate, err
		}
		if meter.usage == beforeUsage && meter.eventEntries == beforeEventEntries {
			return aggregate, nil
		}
		if meter.usage.entries >= meter.budget.maxEntries ||
			meter.usage.directories >= meter.budget.maxDirectories ||
			meter.usage.readBytes >= meter.budget.maxReadBytes ||
			meter.usage.nameBytes >= meter.budget.maxNameBytes ||
			meter.eventEntries >= maximumEnumerationEvents {
			return aggregate, nil
		}
		if meter.fixedDirectoryPermits != 0 || meter.cleanupDirectoryPermits != 0 {
			return aggregate, errors.New("productmetrics: cleanup pass left directory permits outstanding")
		}
		meter.fixedEnvelopeClaimed = false
	}
}

func saturatingAddUint64(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func runSpoolSweep(root *storageRoot, policy spoolPolicy, now time.Time, budget spoolWorkBudget, purgeAll bool) *spoolSweepState {
	budget = constrainSpoolDirectoryBudget(root, budget)
	return runSpoolSweepWithMeter(root, policy, now, newSpoolWorkMeter(budget), purgeAll)
}

func runSpoolSweepWithMeter(root *storageRoot, policy spoolPolicy, now time.Time, meter *spoolWorkMeter, purgeAll bool) *spoolSweepState {
	state := &spoolSweepState{
		root: root, policy: policy, now: now.UTC().Truncate(time.Hour), purgeAll: purgeAll,
		meter: meter, seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir),
	}
	if root == nil || root.storageDir == nil || root.backend == nil || meter == nil {
		state.operation = errStorageClosed
		return state
	}
	state.meter.physicalDirectories = true
	state.restoreDirectoryOpenHooks = root.installDirectoryOpenHooks(
		state.meter.beforePhysicalDirectoryOpen, state.meter.afterPhysicalDirectoryOpen,
	)
	state.cleanupUnsafeQuota()
	if state.mutated || state.operation != nil || state.meter.exhausted {
		return state
	}
	state.cleanupUnsafeFallbackCursor()
	if state.mutated || state.operation != nil || state.meter.exhausted {
		return state
	}
	state.cleanupDualControlPriority()
	if state.mutated || state.operation != nil || state.meter.exhausted {
		return state
	}
	if state.purgeAll {
		// Journal authority is name-addressed and must not be starved behind a
		// deep or over-budget event tree. Drain/prove it before ordinary descent.
		state.journalFixedDirectory = true
		state.cleanupRootTempJournal()
		state.journalFixedDirectory = false
		if state.mutated || state.operation != nil || state.meter.exhausted {
			return state
		}
		state.cleanupSpawnThrottle()
		if state.mutated || state.operation != nil || state.meter.exhausted {
			return state
		}
		state.cleanupDiagnosticStatus()
		if state.mutated || state.operation != nil || state.meter.exhausted {
			return state
		}
	}
	for _, tree := range []string{queueDirectoryName, inflightDirectoryName} {
		if state.meter.exhausted {
			break
		}
		state.walkTree(tree)
	}
	if !state.purgeAll && state.operation == nil && state.meter.traversalError == nil {
		// Every expired or malformed event reached within this invocation's
		// global budget has already been removed. Only then prune oldest valid
		// records, so expiry always wins within the bounded working set.
		state.pruneOldestToQuota()
	}
	if !state.mutated && state.operation == nil && state.meter.traversalError == nil && !state.meter.exhausted {
		state.cleanupRetiredControlDirectory()
	}
	if !state.mutated && state.operation == nil && state.meter.traversalError == nil && !state.meter.exhausted {
		state.cleanupSpoolControlDirectory()
	}
	if !state.mutated && state.operation == nil && state.meter.traversalError == nil && !state.meter.exhausted {
		state.cleanupFallbackCursor()
	}
	if state.purgeAll && state.journalSettled && !state.mutated && state.operation == nil && state.meter.traversalError == nil && !state.meter.exhausted {
		state.cleanupUnexpectedRootEntries()
	}
	state.traversed = !state.meter.exhausted && state.meter.traversalError == nil && state.operation == nil
	return state
}

// cleanupSpawnThrottle removes only the exact identity-leased, owner-private
// regular file at the implemented control name. Invalid and oversized bytes
// are still safe to remove during opt-out because the fd-relative type,
// ownership, link-count, device, and incarnation checks establish that this
// is the subsystem-owned ephemeral attempt record; no content grants deletion
// authority. Unsafe shapes remain preserved and keep cleanup pending.
func (state *spoolSweepState) cleanupSpawnThrottle() {
	if state == nil || state.root == nil || state.meter == nil {
		return
	}
	if !state.meter.chargeFixedEntry(spawnThrottleFileName) ||
		!state.meter.chargeFixedRead(maximumSpawnThrottleBytes+1) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect spawn throttle"))
		return
	}
	_, _, lease, err := state.root.readFileMeasured(spawnThrottleFileName, maximumSpawnThrottleBytes)
	if lease == nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if errors.Is(err, errStorageUnsafeRecordShape) {
			err = errors.Join(err, errUnrecognizedMetricsRootEntry)
		}
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, lease.Close()) }()
	// A retained lease proves the safe filesystem shape even when bounded
	// decoding failed. Bytes never grant deletion authority for this ephemeral
	// record; the exact retained incarnation does.
	if err != nil && !errors.Is(err, errStorageReadLimit) {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if !state.meter.chargeFixedEntry(spawnThrottleFileName) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot remove spawn throttle"))
		return
	}
	if err := state.root.removeFileMatchingLease(spawnThrottleFileName, lease); err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	state.mutated = true
}

// cleanupDiagnosticStatus removes only the exact identity-leased,
// owner-private regular status record. Its bounded contents never grant
// deletion authority, so corrupt and oversized records are removable while
// unsafe filesystem shapes remain preserved and keep cleanup pending.
func (state *spoolSweepState) cleanupDiagnosticStatus() {
	if state == nil || state.root == nil || state.meter == nil {
		return
	}
	if !state.meter.chargeFixedEntry(statusFileName) ||
		!state.meter.chargeFixedRead(maximumDiagnosticStatusBytes+1) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect diagnostic status"))
		return
	}
	_, _, lease, err := state.root.readFileMeasured(statusFileName, maximumDiagnosticStatusBytes)
	if lease == nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if errors.Is(err, errStorageUnsafeRecordShape) {
			err = errors.Join(err, errUnrecognizedMetricsRootEntry)
		}
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, lease.Close()) }()
	if err != nil && !errors.Is(err, errStorageReadLimit) {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if !state.meter.chargeFixedEntry(statusFileName) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot remove diagnostic status"))
		return
	}
	if err := state.root.removeFileMatchingLease(statusFileName, lease); err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	state.mutated = true
}

// cleanupUnexpectedRootEntries preserves every unrecognized root child and
// makes exact cleanup incomplete. Unjournaled canonical staging names are
// unrecognized too; only the root-temp journal can authorize their removal.
// The scan never opens or recurses into an unrecognized directory.
func (state *spoolSweepState) cleanupUnexpectedRootEntries() {
	if !state.meter.chargeDirectory() {
		return
	}
	// Recover any prior unlink whose root-directory sync acknowledgement was
	// lost before treating a fresh enumeration as an absence proof.
	if err := state.root.syncDirectory(); err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	iterator, err := state.root.iterateEntries()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	unrecognized := false
	defer func() {
		state.operation = errors.Join(state.operation, iterator.Close())
		if unrecognized {
			state.operation = errors.Join(state.operation, errUnrecognizedMetricsRootEntry)
		}
	}()
	for {
		entry, ok := state.meter.next(iterator)
		if !ok {
			return
		}
		if knownProductMetricsRootEntry(entry.name) {
			continue
		}
		if entry.name == rootTempJournalDirectoryName && state.journalSettled {
			continue
		}
		unrecognized = true
	}
}

func (state *spoolSweepState) cleanupRootTempJournal() {
	if state == nil || state.root == nil || state.meter == nil ||
		!state.chargeRootTempJournalName(rootTempJournalDirectoryName) {
		return
	}
	entry, err := state.root.lookupEntry(rootTempJournalDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		// A missing directory can be the visible side of an unlink whose root
		// sync acknowledgement was lost. Recover the root and recheck the name
		// before treating absence as durable.
		if syncErr := state.root.syncDirectory(); syncErr != nil {
			state.operation = errors.Join(state.operation, syncErr)
			return
		}
		if !state.chargeRootTempJournalName(rootTempJournalDirectoryName) {
			return
		}
		if _, recheckErr := state.root.lookupEntry(rootTempJournalDirectoryName); errors.Is(recheckErr, fs.ErrNotExist) {
			state.journalSettled = true
		} else {
			state.operation = errors.Join(state.operation, recheckErr, errUnsettledRootTempJournal)
		}
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	var directoryCharged bool
	if state.journalFixedDirectory {
		directoryCharged = state.meter.chargeFixedTraversalDirectory()
	} else {
		directoryCharged = state.meter.chargeDirectory()
	}
	if entry.metadata.kind != storageEntryDirectory {
		state.operation = errors.Join(state.operation, errUnsettledRootTempJournal)
		return
	}
	if !directoryCharged {
		return
	}
	journal, err := state.root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, journal.Close()) }()
	if journal.cleanupOnly() {
		state.operation = errors.Join(state.operation, errUnsettledRootTempJournal)
		return
	}
	// Recover both sides of either uncertainty window before trusting marker or
	// root-temp absence: marker unlink -> journal sync, temp unlink -> root sync.
	if err := journal.syncDirectory(); err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if err := state.root.syncDirectory(); err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	iterator, err := journal.iterateEntries()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() {
		if iterator != nil {
			state.operation = errors.Join(state.operation, iterator.Close())
		}
	}()
	if !state.meter.reserveRootTempJournalSentinel(state.journalFixedDirectory) {
		return
	}
	marker, ok := state.nextRootTempJournalMarker(iterator)
	if !ok {
		closeErr := iterator.Close()
		iterator = nil
		state.operation = errors.Join(state.operation, closeErr)
		if state.mutated || state.meter.exhausted || state.meter.traversalError != nil || state.operation != nil {
			return
		}
		if !state.chargeRootTempJournalName(rootTempJournalDirectoryName) {
			return
		}
		named, lookupErr := state.root.lookupEntry(rootTempJournalDirectoryName)
		if lookupErr != nil || !samePrivateJournalDirectoryEntry(entry, named) {
			state.operation = errors.Join(state.operation, lookupErr, errUnsettledRootTempJournal)
			return
		}
		state.journalSettled = true
		return
	}
	loadedMarker, markerErr := loadRootTempJournalMarker(state.meter, journal, entry, marker, state.journalFixedDirectory)
	if markerErr != nil {
		state.operation = errors.Join(state.operation, markerErr, errUnsettledRootTempJournal)
		return
	}
	markerLease := loadedMarker.lease
	loadedMarker.lease = nil
	expectedMarker := markerLease.incarnation()
	expectedEvidence := loadedMarker.evidence
	closeMarkerLease := func() {
		if markerLease != nil {
			state.operation = errors.Join(state.operation, markerLease.Close())
			markerLease = nil
		}
	}
	if !state.chargeRootTempJournalName(marker.name) {
		closeMarkerLease()
		return
	}
	temp, lookupErr := state.root.lookupEntry(marker.name)
	switch {
	case errors.Is(lookupErr, fs.ErrNotExist):
		if !state.chargeRootTempJournalName(marker.name) {
			closeMarkerLease()
			return
		}
		if absenceErr := state.root.confirmEntryAbsent(marker.name); absenceErr != nil {
			state.operation = errors.Join(state.operation, absenceErr, errUnsettledRootTempJournal)
			closeMarkerLease()
			return
		}
	case lookupErr != nil:
		state.operation = errors.Join(state.operation, lookupErr)
		closeMarkerLease()
		return
	case loadedMarker.evidence.state != rootTempJournalMarkerBound ||
		!boundRootTempJournalMarkerMatches(loadedMarker, temp):
		state.operation = errors.Join(state.operation, errUnsettledRootTempJournal)
		closeMarkerLease()
		return
	default:
		if authorityErr := state.revalidateRootTempJournalMarkerAuthority(entry, journal, marker, markerLease); authorityErr != nil {
			state.operation = errors.Join(state.operation, authorityErr, errUnsettledRootTempJournal)
			closeMarkerLease()
			return
		}
		state.deleteJournaledRootTemp(temp, loadedMarker.evidence.temp, func() error {
			return state.revalidateRootTempJournalMarkerEvidence(
				entry, journal, marker, expectedMarker, expectedEvidence,
			)
		})
		if state.operation != nil || state.meter.exhausted {
			closeMarkerLease()
			return
		}
	}
	if authorityErr := state.revalidateRootTempJournalMarkerAuthority(entry, journal, marker, markerLease); authorityErr != nil {
		state.operation = errors.Join(state.operation, authorityErr, errUnsettledRootTempJournal)
		closeMarkerLease()
		return
	}
	if err := journal.removeFileMatchingLeaseGuarded(marker.name, markerLease, func() error {
		return state.revalidateRootTempJournalMarkerRetirement(
			entry, journal, marker, expectedMarker, expectedEvidence,
		)
	}); err != nil {
		state.operation = errors.Join(state.operation, err)
		closeMarkerLease()
		return
	}
	closeMarkerLease()
	state.mutated = true
}

type loadedRootTempJournalMarker struct {
	entry    storageEntry
	evidence rootTempJournalMarkerEvidence
	lease    *storageRecordLease
}

func loadRootTempJournalMarker(
	meter *spoolWorkMeter,
	journal *storageDir,
	journalEntry storageEntry,
	marker storageEntry,
	fixed bool,
) (loadedRootTempJournalMarker, error) {
	loaded := loadedRootTempJournalMarker{entry: marker}
	if meter == nil || journal == nil || !canonicalStorageTempName(marker.name) ||
		marker.metadata.kind != storageEntryRegular || marker.metadata.uid != uint32(os.Geteuid()) ||
		!marker.metadata.ownerOnly || marker.metadata.nlink != 1 || marker.metadata.dev != journalEntry.metadata.dev {
		return loaded, errUnsettledRootTempJournal
	}
	reservation := uint64(rootTempJournalMarkerReadLimit)
	if fixed {
		if !meter.chargeFixedRead(reservation) {
			return loaded, errUnsettledRootTempJournal
		}
	} else if !meter.chargeRead(reservation) {
		return loaded, errUnsettledRootTempJournal
	}
	data, physicalReadBytes, lease, err := journal.readFileMeasured(marker.name, maximumRootTempJournalMarkerBytes)
	if !fixed {
		meter.refundRead(reservation, physicalReadBytes)
	}
	if err != nil || lease == nil {
		if lease != nil {
			err = errors.Join(err, lease.Close())
		}
		return loaded, errors.Join(err, errUnsettledRootTempJournal)
	}
	incarnation := lease.incarnation()
	if incarnation.dev != marker.metadata.dev || incarnation.ino != marker.metadata.ino {
		return loaded, errors.Join(lease.Close(), errUnsettledRootTempJournal)
	}
	evidence, err := decodeRootTempJournalMarker(marker.name, data)
	if err != nil {
		return loaded, errors.Join(err, lease.Close(), errUnsettledRootTempJournal)
	}
	if evidence.state == rootTempJournalMarkerBound && evidence.temp.dev != marker.metadata.dev {
		return loaded, errors.Join(lease.Close(), errUnsettledRootTempJournal)
	}
	loaded.evidence = evidence
	loaded.lease = lease
	return loaded, nil
}

func boundRootTempJournalMarkerMatches(marker loadedRootTempJournalMarker, temp storageEntry) bool {
	return marker.evidence.state == rootTempJournalMarkerBound && removableStorageTempEntry(temp) &&
		marker.entry.metadata.dev == temp.metadata.dev &&
		marker.evidence.temp == (recordIncarnation{dev: temp.metadata.dev, ino: temp.metadata.ino})
}

func (state *spoolSweepState) revalidateRootTempJournalMarkerAuthority(
	journalEntry storageEntry,
	journal *storageDir,
	marker storageEntry,
	lease *storageRecordLease,
) error {
	if state == nil || state.root == nil || journal == nil || lease == nil {
		return errUnsettledRootTempJournal
	}
	if !state.chargeRootTempJournalName(rootTempJournalDirectoryName) {
		return errUnsettledRootTempJournal
	}
	namedJournal, err := state.root.lookupEntry(rootTempJournalDirectoryName)
	if err != nil || !samePrivateJournalDirectoryEntry(journalEntry, namedJournal) {
		return errors.Join(err, errUnsettledRootTempJournal)
	}
	if !state.chargeRootTempJournalName(marker.name) {
		return errUnsettledRootTempJournal
	}
	namedMarker, err := journal.lookupEntry(marker.name)
	if err != nil || !sameRootTempJournalMarkerIdentity(marker, namedMarker, lease, journalEntry.metadata.dev) {
		return errors.Join(err, errUnsettledRootTempJournal)
	}
	return nil
}

func sameRootTempJournalMarkerIdentity(enumerated, named storageEntry, lease *storageRecordLease, journalDevice uint64) bool {
	if lease == nil {
		return false
	}
	incarnation := lease.incarnation()
	return named.name == enumerated.name && named.metadata.dev == enumerated.metadata.dev &&
		named.metadata.ino == enumerated.metadata.ino && named.metadata.dev == journalDevice &&
		named.metadata.kind == storageEntryRegular && named.metadata.uid == uint32(os.Geteuid()) &&
		named.metadata.ownerOnly && named.metadata.nlink == 1 &&
		incarnation == (recordIncarnation{dev: named.metadata.dev, ino: named.metadata.ino})
}

func (state *spoolSweepState) revalidateRootTempJournalMarkerEvidence(
	journalEntry storageEntry,
	journal *storageDir,
	marker storageEntry,
	expectedMarker recordIncarnation,
	expectedEvidence rootTempJournalMarkerEvidence,
) error {
	if state == nil || state.root == nil || journal == nil || expectedMarker == (recordIncarnation{}) {
		return errUnsettledRootTempJournal
	}
	if !state.chargeRootTempJournalName(rootTempJournalDirectoryName) {
		return errUnsettledRootTempJournal
	}
	namedJournal, err := state.root.lookupEntry(rootTempJournalDirectoryName)
	if err != nil || !samePrivateJournalDirectoryEntry(journalEntry, namedJournal) {
		return errors.Join(err, errUnsettledRootTempJournal)
	}
	loaded, err := loadRootTempJournalMarker(state.meter, journal, journalEntry, marker, state.journalFixedDirectory)
	if err != nil || loaded.lease == nil {
		return errors.Join(err, errUnsettledRootTempJournal)
	}
	lease := loaded.lease
	if loaded.evidence != expectedEvidence || lease.incarnation() != expectedMarker {
		return errors.Join(lease.Close(), errUnsettledRootTempJournal)
	}
	if !state.chargeRootTempJournalName(marker.name) {
		return errors.Join(lease.Close(), errUnsettledRootTempJournal)
	}
	namedMarker, lookupErr := journal.lookupEntry(marker.name)
	if lookupErr != nil || !sameRootTempJournalMarkerIdentity(marker, namedMarker, lease, journalEntry.metadata.dev) {
		return errors.Join(lookupErr, lease.Close(), errUnsettledRootTempJournal)
	}
	return lease.Close()
}

func (state *spoolSweepState) revalidateRootTempJournalMarkerRetirement(
	journalEntry storageEntry,
	journal *storageDir,
	marker storageEntry,
	expectedMarker recordIncarnation,
	expectedEvidence rootTempJournalMarkerEvidence,
) error {
	if !state.chargeRootTempJournalName(marker.name) {
		return errUnsettledRootTempJournal
	}
	if err := state.root.confirmEntryAbsent(marker.name); err != nil {
		return errors.Join(err, errUnsettledRootTempJournal)
	}
	if err := state.revalidateRootTempJournalMarkerEvidence(
		journalEntry, journal, marker, expectedMarker, expectedEvidence,
	); err != nil {
		return err
	}
	if !state.chargeRootTempJournalName(marker.name) {
		return errUnsettledRootTempJournal
	}
	return state.root.confirmEntryAbsent(marker.name)
}

func (state *spoolSweepState) chargeRootTempJournalName(name string) bool {
	if state == nil || state.meter == nil {
		return false
	}
	if state.journalFixedDirectory {
		return state.meter.chargeFixedEntry(name)
	}
	return state.meter.chargeNamedEntry(name)
}

func (state *spoolSweepState) nextRootTempJournalMarker(iterator *storageIterator) (storageEntry, bool) {
	if state == nil || state.meter == nil || iterator == nil {
		return storageEntry{}, false
	}
	if !state.journalFixedDirectory {
		entry, ok := state.meter.next(iterator)
		if !ok {
			return storageEntry{}, false
		}
		if !state.meter.acceptRootTempJournalMarker() {
			return storageEntry{}, false
		}
		return entry, true
	}
	entry, err := iterator.Next()
	if errors.Is(err, io.EOF) {
		return storageEntry{}, false
	}
	if err != nil {
		state.meter.traversalError = errors.Join(state.meter.traversalError, err)
		return storageEntry{}, false
	}
	if !state.meter.chargeFixedEntry(entry.name) || !state.meter.acceptRootTempJournalMarker() {
		return storageEntry{}, false
	}
	return entry, true
}

func samePrivateJournalDirectoryEntry(opened, named storageEntry) bool {
	return opened.name == rootTempJournalDirectoryName && named.name == opened.name &&
		named.metadata.dev == opened.metadata.dev && named.metadata.ino == opened.metadata.ino &&
		named.metadata.kind == storageEntryDirectory && named.metadata.nlink > 0 &&
		named.metadata.uid == uint32(os.Geteuid()) && named.metadata.ownerOnly
}

func removableStorageTempEntry(entry storageEntry) bool {
	return canonicalStorageTempName(entry.name) && entry.metadata.kind == storageEntryRegular &&
		entry.metadata.uid == uint32(os.Geteuid()) && entry.metadata.ownerOnly && entry.metadata.nlink == 1
}

func canonicalStorageTempName(name string) bool {
	remainder, ok := strings.CutPrefix(name, ".pm-tmp-")
	if !ok {
		return false
	}
	pid, sequence, ok := strings.Cut(remainder, "-")
	return ok && !strings.Contains(sequence, "-") &&
		canonicalNonzeroLowerHex(pid) && canonicalNonzeroLowerHex(sequence)
}

func canonicalNonzeroLowerHex(value string) bool {
	if value == "" || len(value) > 16 || value[0] == '0' {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	parsed, err := strconv.ParseUint(value, 16, 64)
	return err == nil && parsed != 0 && strconv.FormatUint(parsed, 16) == value
}

func knownProductMetricsRootEntry(name string) bool {
	if isStorageLockName(name) {
		return true
	}
	switch name {
	case configFileName, quotaFileName, queueDirectoryName, inflightDirectoryName,
		spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName:
		return true
	default:
		return false
	}
}

func constrainSpoolDirectoryBudget(root *storageRoot, budget spoolWorkBudget) spoolWorkBudget {
	if root == nil || root.storageDir == nil || root.backend == nil || budget.maxDirectories == 0 {
		return budget
	}
	limiter, ok := root.backend.(storageFileDescriptorLimitBackend)
	if !ok {
		return budget
	}
	softLimit, err := limiter.fileDescriptorSoftLimit()
	effective := spoolFallbackDirectoryLimit
	if err == nil {
		effective = spoolDirectoryBudgetForSoftLimit(budget.maxDirectories, softLimit)
	}
	if effective < budget.maxDirectories {
		budget.maxDirectories = effective
	}
	return budget
}

func spoolDirectoryBudgetForSoftLimit(requested, softLimit uint64) uint64 {
	effective := softLimit / spoolFileDescriptorHeadroom
	if effective < spoolMinimumDirectoryProgress {
		effective = spoolMinimumDirectoryProgress
	}
	if requested < effective {
		return requested
	}
	return effective
}

func (state *spoolSweepState) cleanupUnsafeQuota() {
	if state == nil || state.root == nil || state.meter == nil || !state.meter.chargeNamedEntry(quotaFileName) {
		return
	}
	entry, err := state.root.lookupEntry(quotaFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	safeRegular := entry.metadata.kind == storageEntryRegular && entry.metadata.nlink == 1 &&
		entry.metadata.uid == uint32(os.Geteuid()) && entry.metadata.ownerOnly
	if safeRegular {
		return
	}
	if !state.ensureFailClosedControl() {
		return
	}
	if entry.metadata.kind != storageEntryDirectory {
		if err := state.root.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, err)
		} else if err == nil {
			state.mutated = true
		}
		return
	}
	if !state.meter.chargeCleanupDirectory() {
		return
	}
	directory, err := state.root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	state.purgeDirectory(directory, directory, false)
	state.operation = errors.Join(state.operation, directory.Close())
	if state.meter.exhausted || state.operation != nil {
		return
	}
	if err := state.root.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
	} else if err == nil {
		state.mutated = true
	}
}

func (state *spoolSweepState) cleanupUnsafeFallbackCursor() {
	if state == nil || state.root == nil || state.meter == nil || !state.meter.chargeNamedEntry(fallbackRelocationCursorName) {
		return
	}
	entry, err := state.root.lookupEntry(fallbackRelocationCursorName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if safeFallbackRelocationCursor(entry) {
		return
	}
	if !state.ensureFailClosedControl() {
		return
	}
	if entry.metadata.kind != storageEntryDirectory {
		if err := state.root.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, err)
		} else if err == nil {
			state.mutated = true
		}
		return
	}
	if !state.meter.chargeCleanupDirectory() {
		return
	}
	directory, err := state.root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	state.purgeDirectory(directory, directory, false)
	state.operation = errors.Join(state.operation, directory.Close())
	if state.meter.exhausted || state.operation != nil {
		return
	}
	if err := state.root.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
	} else if err == nil {
		state.mutated = true
	}
}

func (state *spoolSweepState) cleanupFallbackCursor() {
	if state == nil || state.root == nil || state.meter == nil || !state.meter.chargeNamedEntry(fallbackRelocationCursorName) {
		return
	}
	entry, err := state.root.lookupEntry(fallbackRelocationCursorName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if !safeFallbackRelocationCursor(entry) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: fallback relocation cursor changed before cleanup"))
		return
	}
	if !state.ensureAlternateControlBarrier(spoolControlDirectoryName) {
		return
	}
	if err := state.root.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
	} else if err == nil {
		state.mutated = true
	}
}

func safeFallbackRelocationCursor(entry storageEntry) bool {
	return entry.metadata.kind == storageEntryRegular && entry.metadata.nlink == 1 &&
		entry.metadata.uid == uint32(os.Geteuid()) && entry.metadata.ownerOnly &&
		entry.metadata.size >= 0 && entry.metadata.size <= maximumRelocationBytes
}

func (state *spoolSweepState) cleanupDualControlPriority() {
	if state == nil || state.root == nil || state.meter == nil {
		return
	}
	if !state.meter.chargeNamedEntry(spoolControlDirectoryName) {
		return
	}
	_, activeErr := state.root.lookupEntry(spoolControlDirectoryName)
	if activeErr != nil && !errors.Is(activeErr, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, activeErr)
		return
	}
	if !state.meter.chargeNamedEntry(retiredControlDirectoryName) {
		return
	}
	_, retiredErr := state.root.lookupEntry(retiredControlDirectoryName)
	if retiredErr != nil && !errors.Is(retiredErr, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, retiredErr)
		return
	}
	if retiredErr == nil {
		if activeErr == nil {
			state.failClosedArmed = true
		}
		state.cleanupRetiredControlDirectory()
	}
}

func (state *spoolSweepState) ensureFailClosedControl() bool {
	if state == nil || state.root == nil || state.meter == nil {
		return false
	}
	if state.failClosedArmed {
		return true
	}
	if !state.meter.chargeFixedEntry(spoolControlDirectoryName) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect fail-closed control"))
		return false
	}
	_, activeErr := state.root.lookupEntry(spoolControlDirectoryName)
	if activeErr == nil {
		state.failClosedArmed = true
		if !state.meter.chargeFixedEntry(retiredControlDirectoryName) {
			return true
		}
		if _, retiredErr := state.root.lookupEntry(retiredControlDirectoryName); retiredErr == nil {
			// Active evidence remains named while the one fixed directory slot
			// is spent making retired cleanup progress first.
			return true
		} else if !errors.Is(retiredErr, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, retiredErr)
			return false
		}
		// Presence alone is durable fail-closed evidence. Defer opening an
		// existing namespace until the caller knows whether it needs cursor or
		// quota contents, preserving the one fixed physical slot.
		return true
	}
	if !errors.Is(activeErr, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, activeErr)
		return false
	}
	if !state.meter.chargeFixedEntry(retiredControlDirectoryName) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect retired fail-closed control"))
		return false
	}
	_, retiredErr := state.root.lookupEntry(retiredControlDirectoryName)
	if retiredErr == nil {
		state.failClosedArmed = true
		return true
	}
	if !errors.Is(retiredErr, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, retiredErr)
		return false
	}
	if !state.meter.chargeFixedDirectory() {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot open fail-closed control"))
		return false
	}
	control, err := state.root.openDir([]string{spoolControlDirectoryName}, true)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return false
	}
	state.retainedControl = control
	state.failClosedArmed = true
	state.mutated = true
	return true
}

func (state *spoolSweepState) ensureDurableQuotaMarker() bool {
	if state.durableQuotaMarker {
		return true
	}
	if !state.meter.chargeFixedEntry(quotaFileName) || !state.meter.chargeFixedRead(maximumQuotaBytes+1) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect fail-closed quota marker"))
		return false
	}
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	quota, present, err := loadSpoolQuota(state.root)
	if err == nil && present && quota == markers {
		state.durableQuotaMarker = true
		return true
	}
	data, encodeErr := encodeSpoolQuota(markers)
	if encodeErr != nil || !state.meter.chargeFixedRead(uint64(len(data))) {
		state.operation = errors.Join(state.operation, err, encodeErr, errors.New("productmetrics: cleanup budget cannot write fail-closed quota marker"))
		return false
	}
	if !state.meter.chargeFixedDirectory() {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot open root quota journal"))
		return false
	}
	if persistErr := persistSpoolQuotaDirect(state.root, markers); persistErr != nil {
		state.operation = errors.Join(state.operation, err, persistErr)
		return false
	}
	state.durableQuotaMarker = true
	state.mutated = true
	return true
}

func (state *spoolSweepState) ensureAlternateControlBarrier(alternateName string) bool {
	if state == nil || state.root == nil || state.meter == nil {
		return false
	}
	if !state.meter.chargeFixedEntry(alternateName) {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: cleanup budget cannot inspect alternate fail-closed control"))
		return false
	}
	_, err := state.root.lookupEntry(alternateName)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
		return false
	}
	return state.ensureDurableQuotaMarker()
}

func (state *spoolSweepState) cleanupSpoolControlDirectory() {
	if !state.meter.chargeNamedEntry(spoolControlDirectoryName) {
		return
	}
	entry, err := state.root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if !state.meter.chargeFixedEntry(retiredControlDirectoryName) {
		return
	}
	_, retiredErr := state.root.lookupEntry(retiredControlDirectoryName)
	if errors.Is(retiredErr, fs.ErrNotExist) {
		if !state.ensureDurableQuotaMarker() {
			return
		}
	} else if retiredErr != nil {
		state.operation = errors.Join(state.operation, retiredErr)
		return
	}
	unlinkErr := state.root.unlinkEnumeratedEntry(entry)
	if unlinkErr == nil {
		state.mutated = true
		return
	}
	if errors.Is(unlinkErr, fs.ErrNotExist) {
		return
	}
	if !errors.Is(unlinkErr, errStorageEntryIsDirectory) {
		state.operation = errors.Join(state.operation, unlinkErr)
		return
	}
	removeErr := state.root.removeEnumeratedCleanupDirectory(entry)
	if removeErr == nil {
		state.mutated = true
		return
	}
	if !errors.Is(removeErr, errStorageDirectoryNotEmpty) {
		if !errors.Is(removeErr, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, removeErr)
		}
		return
	}
	result, renameErr := state.root.renameEnumeratedDirectory(entry, state.root.storageDir, retiredControlDirectoryName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if renameErr != nil {
		state.operation = errors.Join(state.operation, renameErr)
	} else if result.state != storageRenameAppliedDurable {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: control retirement was not durable"))
	}
}

func (state *spoolSweepState) cleanupRetiredControlDirectory() {
	if !state.meter.chargeNamedEntry(retiredControlDirectoryName) {
		return
	}
	entry, err := state.root.lookupEntry(retiredControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if !state.meter.chargeFixedEntry(spoolControlDirectoryName) {
		return
	}
	_, activeErr := state.root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(activeErr, fs.ErrNotExist) {
		if !state.ensureDurableQuotaMarker() {
			return
		}
	} else if activeErr != nil {
		state.operation = errors.Join(state.operation, activeErr)
		return
	}
	unlinkErr := state.root.unlinkEnumeratedEntry(entry)
	if unlinkErr == nil {
		state.mutated = true
		return
	}
	if errors.Is(unlinkErr, fs.ErrNotExist) {
		return
	}
	if !errors.Is(unlinkErr, errStorageEntryIsDirectory) {
		state.operation = errors.Join(state.operation, unlinkErr)
		return
	}
	if !state.meter.chargeCleanupDirectory() {
		return
	}
	retired, openErr := state.root.openEnumeratedCleanupDirectory(entry)
	if openErr != nil {
		state.operation = errors.Join(state.operation, openErr)
		return
	}
	state.retainedRetiredControl = retired
	state.purgeDirectory(retired, retired, false)
	state.retainedRetiredControl = nil
	state.operation = errors.Join(state.operation, retired.Close())
	if state.meter.exhausted {
		return
	}
	if err := state.root.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
	} else if err == nil {
		state.mutated = true
	}
}

func (state *spoolSweepState) walkTree(treeName string) {
	if !state.meter.chargeNamedEntry(treeName) {
		return
	}
	entry, lookupErr := state.root.lookupEntry(treeName)
	if errors.Is(lookupErr, fs.ErrNotExist) {
		return
	}
	if lookupErr != nil {
		state.operation = errors.Join(state.operation, lookupErr)
		return
	}
	if !state.meter.chargeDirectory() {
		return
	}
	tree, err := state.root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		if errors.Is(err, syscall.EXDEV) {
			state.operation = errors.Join(state.operation, err)
			return
		}
		state.operation = errors.Join(state.operation, directoryDescriptorExhaustion(err))
		if !state.meter.chargeEventEntry() {
			return
		}
		state.deleteLeaf(state.root.storageDir, entry, true)
		return
	}
	cleanupMalformedTree := tree.cleanupOnly()
	defer func() {
		if !cleanupMalformedTree || state.meter.exhausted {
			return
		}
		if !state.ensureFailClosedControl() {
			return
		}
		removeErr := state.root.removeEnumeratedCleanupDirectory(entry)
		if removeErr == nil {
			state.mutated = true
			return
		}
		if !errors.Is(removeErr, fs.ErrNotExist) && !errors.Is(removeErr, errStorageDirectoryNotEmpty) {
			state.operation = errors.Join(state.operation, removeErr)
		}
	}()
	defer func() { state.operation = errors.Join(state.operation, tree.Close()) }()
	iterator, err := tree.iterateEntries()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, iterator.Close()) }()
	for {
		entry, ok := state.meter.next(iterator)
		if !ok {
			return
		}
		validGeneration := validCanonicalUUIDv4(entry.name)
		deleteGeneration := state.purgeAll || tree.cleanupOnly() || !validGeneration || entry.name != state.policy.generation
		if entry.metadata.kind != storageEntryDirectory {
			if !state.meter.chargeEventEntry() {
				return
			}
			state.deleteLeaf(tree, entry, true)
			continue
		}
		var canOpen bool
		if deleteGeneration {
			canOpen = state.meter.chargeCleanupDirectory()
		} else {
			canOpen = state.meter.chargeDirectory()
		}
		if !canOpen {
			state.quarantineDirectory(tree, tree, entry, true, true)
			return
		}
		generation, openErr := openEnumeratedStorageDirectory(tree, entry)
		if openErr != nil {
			if errors.Is(openErr, syscall.EXDEV) {
				state.operation = errors.Join(state.operation, openErr)
				return
			}
			state.operation = errors.Join(state.operation, directoryDescriptorExhaustion(openErr))
			// The declared layout permits only generation directories here.
			state.meter.refundLogicalDirectoryCharge()
			if !state.meter.chargeEventEntry() {
				return
			}
			state.deleteLeaf(tree, entry, true)
			continue
		}
		cleanupGeneration := deleteGeneration || generation.cleanupOnly()
		if cleanupGeneration {
			state.purgeDirectory(generation, tree, true)
		} else {
			state.scanCurrentGeneration(treeName, entry.name, generation, tree)
		}
		state.operation = errors.Join(state.operation, generation.Close())
		if cleanupGeneration && !state.meter.exhausted {
			if !state.ensureFailClosedControl() {
				return
			}
			if err := tree.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
				state.operation = errors.Join(state.operation, err)
			} else if err == nil {
				state.mutated = true
			}
		}
		if state.mutated {
			return
		}
	}
}

func openEnumeratedStorageDirectory(parent *storageDir, entry storageEntry) (*storageDir, error) {
	if parent == nil || parent.backend == nil {
		return nil, errStorageClosed
	}
	if err := validateEnumeratedEntry(entry); err != nil {
		return nil, err
	}
	return parent.openEnumeratedCleanupDirectory(entry)
}

func (state *spoolSweepState) purgeDirectory(directory, quarantineRoot *storageDir, eventTree bool) {
	iterator, err := directory.iterateEntries()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, iterator.Close()) }()
	for {
		entry, ok := state.meter.next(iterator)
		if !ok {
			return
		}
		if entry.metadata.kind != storageEntryDirectory {
			if eventTree && !state.meter.chargeEventEntry() {
				return
			}
			state.deleteLeaf(directory, entry, eventTree)
			continue
		}
		if !state.meter.chargeCleanupDirectory() {
			state.quarantineDirectory(directory, quarantineRoot, entry, true, eventTree)
			return
		}
		child, openErr := openEnumeratedStorageDirectory(directory, entry)
		if openErr == nil {
			state.purgeDirectory(child, quarantineRoot, eventTree)
			state.operation = errors.Join(state.operation, child.Close())
			if errors.Is(state.operation, syscall.EXDEV) {
				return
			}
			if !state.meter.exhausted {
				if !state.ensureFailClosedControl() {
					return
				}
				if err := directory.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
					state.operation = errors.Join(state.operation, err)
				} else if err == nil {
					state.mutated = true
				}
			}
			continue
		}
		if errors.Is(openErr, syscall.EXDEV) {
			state.operation = errors.Join(state.operation, openErr)
			return
		}
		state.operation = errors.Join(state.operation, directoryDescriptorExhaustion(openErr))
		state.meter.refundLogicalDirectoryCharge()
		if eventTree && !state.meter.chargeEventEntry() {
			return
		}
		state.deleteLeaf(directory, entry, eventTree)
	}
}

func (state *spoolSweepState) quarantineDirectory(parent, quarantineRoot *storageDir, entry storageEntry, deleteLeaf, eventTree bool) {
	if !state.ensureFailClosedControl() {
		return
	}
	name := fmt.Sprintf(".orphan-%x-%x", entry.metadata.dev, entry.metadata.ino)
	if !state.meter.chargeFixedEntry(name) {
		return
	}
	result, err := parent.renameEnumeratedDirectory(entry, quarantineRoot, name)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if errors.Is(err, errStorageDestinationExists) {
		state.resolveQuarantineCollision(parent, quarantineRoot, entry, name, eventTree)
		return
	}
	if err != nil {
		if deleteLeaf {
			unlinkErr := parent.unlinkEnumeratedEntry(entry)
			if unlinkErr == nil || errors.Is(unlinkErr, fs.ErrNotExist) {
				if unlinkErr == nil {
					state.mutated = true
				}
				if eventTree {
					state.noteRemoved(entry.metadata.size)
				}
				return
			}
			err = errors.Join(err, unlinkErr)
		}
		state.operation = errors.Join(state.operation, err)
		return
	}
	if result.state != storageRenameAppliedDurable {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: malformed subtree quarantine was not durable"))
	}
}

func (state *spoolSweepState) resolveQuarantineCollision(parent, quarantineRoot *storageDir, source storageEntry, targetName string, eventTree bool) {
	target, err := quarantineRoot.lookupEntry(targetName)
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if err := quarantineRoot.unlinkEnumeratedEntry(target); err == nil {
		state.mutated = true
		if eventTree {
			state.noteRemoved(target.metadata.size)
		}
		return
	} else if !errors.Is(err, errStorageEntryIsDirectory) {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if err := quarantineRoot.removeEnumeratedCleanupDirectory(target); err == nil {
		state.mutated = true
		return
	} else if !errors.Is(err, errStorageDirectoryNotEmpty) {
		state.operation = errors.Join(state.operation, err)
		return
	}
	result, err := parent.exchangeEnumeratedEntries(source, quarantineRoot, target)
	if errors.Is(err, errStorageExchangeAncestor) {
		state.relocateQuarantineBlocker(quarantineRoot, target, eventTree)
		return
	}
	if errors.Is(err, errStorageExchangeUnsupported) || errors.Is(err, errStorageExchangeSameEntry) {
		state.relocateUnsupportedExchangeBlocker(quarantineRoot, target)
		return
	}
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if result.state != storageRenameAppliedDurable {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: malformed subtree exchange was not durable"))
	}
}

func (state *spoolSweepState) relocateUnsupportedExchangeBlocker(quarantineRoot *storageDir, blocker storageEntry) {
	start, slots, err := state.reserveRelocationSlots()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	for offset := 0; offset < slots; offset++ {
		sequence := start + uint64(offset)
		name := relocationCandidateName(sequence)
		if !state.meter.chargeFixedEntry(name) {
			state.operation = errors.Join(state.operation, errors.New("productmetrics: reserved relocation slot exceeded cleanup budget"))
			return
		}
		_, lookupErr := quarantineRoot.lookupEntry(name)
		if lookupErr == nil {
			continue
		}
		if !errors.Is(lookupErr, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, lookupErr)
			return
		}
		result, renameErr := quarantineRoot.renameEnumeratedDirectory(blocker, quarantineRoot, name)
		if result.state != storageRenameNotApplied {
			state.mutated = true
		}
		if errors.Is(renameErr, errStorageDestinationExists) {
			continue
		}
		if renameErr != nil {
			state.operation = errors.Join(state.operation, renameErr)
			return
		}
		if result.state != storageRenameAppliedDurable {
			state.operation = errors.Join(state.operation, errors.New("productmetrics: fallback blocker relocation was not durable"))
		}
		return
	}
}

func relocationCandidateName(sequence uint64) string {
	return fmt.Sprintf(".pm-relocated-%016x", sequence)
}

func (state *spoolSweepState) reserveRelocationSlots() (uint64, int, error) {
	if state == nil || state.root == nil || state.meter == nil {
		return 0, 0, errStorageClosed
	}
	_, retiredPresent, progressErr := state.cleanupOneRetiredControlEntryFixed()
	if progressErr != nil || retiredPresent {
		return 0, 0, progressErr
	}
	created := false
	control := state.retainedControl
	if control == nil {
		if !state.meter.chargeFixedDirectory() {
			return 0, 0, errors.New("productmetrics: cleanup budget cannot open relocation control directory")
		}
		if !state.meter.chargeFixedEntry(spoolControlDirectoryName) {
			return 0, 0, errors.New("productmetrics: cleanup budget cannot inspect relocation control directory")
		}
		_, lookupErr := state.root.lookupEntry(spoolControlDirectoryName)
		var err error
		switch {
		case errors.Is(lookupErr, fs.ErrNotExist):
			control, err = state.root.openDir([]string{spoolControlDirectoryName}, true)
			created = err == nil
		case lookupErr != nil:
			return 0, 0, lookupErr
		default:
			control, err = state.root.openDir([]string{spoolControlDirectoryName}, false)
		}
		if err != nil {
			if lookupErr == nil {
				retireErr := state.retireActiveControlNamespace(nil, err)
				if retireErr == nil {
					return 0, 0, nil
				}
				return 0, 0, errors.Join(err, retireErr)
			}
			return 0, 0, err
		}
		state.retainedControl = control
		state.failClosedArmed = true
		if created {
			state.mutated = true
		}
	}
	if err := state.ensureConservativeRelocationQuota(control); err != nil {
		retireErr := state.retireActiveControlNamespace(control, err)
		if retireErr == nil {
			return 0, 0, nil
		}
		return 0, 0, errors.Join(err, retireErr)
	}

	if !state.meter.chargeFixedEntry(relocationCursorFileName) {
		return 0, 0, errors.New("productmetrics: cleanup budget cannot inspect relocation cursor")
	}
	cursor := relocationCursor{}
	cursorEntry, cursorLookupErr := control.lookupEntry(relocationCursorFileName)
	if cursorLookupErr == nil {
		if !state.meter.chargeFixedRead(maximumRelocationBytes + 1) {
			return 0, 0, errors.New("productmetrics: cleanup budget cannot read relocation cursor")
		}
		data, readErr := control.readFile(relocationCursorFileName, maximumRelocationBytes)
		if readErr != nil {
			if err := state.retireRelocationCursor(control, cursorEntry, readErr); err != nil {
				return 0, 0, err
			}
			return 0, 0, nil
		}
		decoded, decodeErr := decodeRelocationCursor(data)
		if decodeErr != nil {
			if err := state.retireRelocationCursor(control, cursorEntry, decodeErr); err != nil {
				return 0, 0, err
			}
			return 0, 0, nil
		}
		cursor = decoded
	} else if !errors.Is(cursorLookupErr, fs.ErrNotExist) {
		return 0, 0, cursorLookupErr
	}

	candidateBytes := uint64(len(relocationCandidateName(maximumRelocationSequence)))
	slots := state.meter.availableFixedSlots(candidateBytes, maximumRelocationSlots)
	if slots != maximumRelocationSlots {
		return 0, 0, errors.New("productmetrics: cleanup budget cannot reserve a complete relocation block")
	}
	if cursor.Next > maximumRelocationSequence-uint64(slots) {
		if err := state.retireRelocationCursor(control, cursorEntry, errors.New("productmetrics: relocation cursor is exhausted")); err != nil {
			return 0, 0, err
		}
		return 0, 0, nil
	}
	reserved := relocationCursor{Next: cursor.Next + uint64(slots)}
	data, err := encodeRelocationCursor(reserved)
	if err != nil {
		return 0, 0, err
	}
	if !state.meter.chargeFixedRead(uint64(len(data))) {
		return 0, 0, errors.New("productmetrics: cleanup budget cannot write relocation cursor")
	}
	result, writeErr := control.writeFileAtomicOutcome(relocationCursorFileName, data)
	if result.state != storageWriteNotApplied {
		state.mutated = true
	}
	if writeErr != nil {
		return 0, 0, writeErr
	}
	if result.state != storageWriteAppliedDurable {
		return 0, 0, errors.New("productmetrics: relocation cursor reservation was not durable")
	}
	state.mutated = true
	if state.afterRelocationReservation != nil {
		if err := state.afterRelocationReservation(); err != nil {
			return 0, 0, fmt.Errorf("productmetrics: injected post-reservation failure: %w", err)
		}
	}
	return cursor.Next, slots, nil
}

func (state *spoolSweepState) cleanupOneRetiredControlEntryFixed() (bool, bool, error) {
	if !state.meter.chargeFixedEntry(retiredControlDirectoryName) {
		return false, false, errors.New("productmetrics: cleanup budget cannot inspect retired control")
	}
	entry, err := state.root.lookupEntry(retiredControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !state.ensureAlternateControlBarrier(spoolControlDirectoryName) {
		return false, true, errors.New("productmetrics: cannot remove retired control without replacement fail-closed evidence")
	}
	if err := state.root.unlinkEnumeratedEntry(entry); err == nil {
		state.mutated = true
		return true, true, nil
	} else if !errors.Is(err, errStorageEntryIsDirectory) {
		return false, true, err
	}
	if err := state.root.removeEnumeratedCleanupDirectory(entry); err == nil {
		state.mutated = true
		return true, true, nil
	} else if !errors.Is(err, errStorageDirectoryNotEmpty) {
		return false, true, err
	}
	retired := state.retainedRetiredControl
	closeRetired := false
	if retired == nil {
		if !state.meter.chargeFixedDirectory() {
			return false, true, nil
		}
		retired, err = state.root.openEnumeratedCleanupDirectory(entry)
		if err != nil {
			return false, true, err
		}
		closeRetired = true
	}
	if closeRetired {
		defer func() { state.operation = errors.Join(state.operation, retired.Close()) }()
	}
	child, err := retired.firstEntryFromRetainedHandle()
	if errors.Is(err, io.EOF) {
		return false, true, nil
	}
	if err != nil {
		return false, true, err
	}
	if !state.meter.chargeFixedEntry(child.name) {
		return false, true, errors.New("productmetrics: cleanup budget cannot inspect retired-control child")
	}
	if child.metadata.kind != storageEntryDirectory {
		if err := retired.unlinkEnumeratedEntry(child); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, true, err
		}
		state.mutated = true
		return true, true, nil
	}
	if err := retired.removeEnumeratedCleanupDirectory(child); err == nil {
		state.mutated = true
		return true, true, nil
	} else if !errors.Is(err, errStorageDirectoryNotEmpty) {
		return false, true, err
	}
	if !state.meter.chargeFixedDirectory() {
		return false, true, nil
	}
	childDirectory, err := retired.openEnumeratedCleanupDirectory(child)
	if err != nil {
		return false, true, err
	}
	defer func() { state.operation = errors.Join(state.operation, childDirectory.Close()) }()
	grandchild, err := childDirectory.firstEntryFromRetainedHandle()
	if errors.Is(err, io.EOF) {
		return false, true, nil
	}
	if err != nil {
		return false, true, err
	}
	if !state.meter.chargeFixedEntry(grandchild.name) {
		return false, true, errors.New("productmetrics: cleanup budget cannot inspect nested retired-control child")
	}
	if grandchild.metadata.kind != storageEntryDirectory {
		if err := childDirectory.unlinkEnumeratedEntry(grandchild); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, true, err
		}
		state.mutated = true
		return true, true, nil
	}
	if err := childDirectory.removeEnumeratedCleanupDirectory(grandchild); err == nil {
		state.mutated = true
		return true, true, nil
	} else if !errors.Is(err, errStorageDirectoryNotEmpty) {
		return false, true, err
	}
	targetName := fmt.Sprintf(".orphan-%x-%x", grandchild.metadata.dev, grandchild.metadata.ino)
	progressed, err := state.liftNestedRetiredControlDirectory(childDirectory, retired, grandchild, targetName)
	return progressed, true, err
}

func (state *spoolSweepState) liftNestedRetiredControlDirectory(parent, retired *storageDir, source storageEntry, targetName string) (bool, error) {
	if !state.meter.chargeFixedEntry(targetName) {
		return false, errors.New("productmetrics: cleanup budget cannot relocate nested retired-control child")
	}
	result, renameErr := parent.renameEnumeratedDirectory(source, retired, targetName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if !errors.Is(renameErr, errStorageDestinationExists) {
		return validateRetiredControlRename(result, renameErr, "nested retired-control relocation")
	}
	if result.state != storageRenameNotApplied {
		return true, renameErr
	}

	blocker, lookupErr := retired.lookupEntry(targetName)
	if lookupErr != nil {
		return false, lookupErr
	}
	if unlinkErr := retired.unlinkEnumeratedEntry(blocker); unlinkErr == nil {
		state.mutated = true
		return true, nil
	} else if !errors.Is(unlinkErr, errStorageEntryIsDirectory) {
		return false, unlinkErr
	}
	if removeErr := retired.removeEnumeratedCleanupDirectory(blocker); removeErr == nil {
		state.mutated = true
		return true, nil
	} else if !errors.Is(removeErr, errStorageDirectoryNotEmpty) {
		return false, removeErr
	}

	exchangeResult, exchangeErr := parent.exchangeEnumeratedEntries(source, retired, blocker)
	if exchangeResult.state != storageRenameNotApplied {
		state.mutated = true
	}
	unsupported := errors.Is(exchangeErr, errStorageExchangeUnsupported)
	ancestor := errors.Is(exchangeErr, errStorageExchangeAncestor)
	if !unsupported && !ancestor {
		return validateRetiredControlRename(exchangeResult, exchangeErr, "nested retired-control exchange")
	}
	if exchangeResult.state != storageRenameNotApplied {
		return true, exchangeErr
	}
	return state.rotateRetiredControlCollision(retired, blocker)
}

func (state *spoolSweepState) rotateRetiredControlCollision(retired *storageDir, blocker storageEntry) (bool, error) {
	current := blocker
	seen := make(map[[2]uint64]struct{}, maximumRelocationSlots)
	for attempts := 0; attempts < maximumRelocationSlots; attempts++ {
		identity := [2]uint64{current.metadata.dev, current.metadata.ino}
		if _, duplicate := seen[identity]; duplicate {
			return state.breakRetiredControlCanonicalGraph(retired, current)
		}
		seen[identity] = struct{}{}
		candidateName := fmt.Sprintf(".orphan-%x-%x", current.metadata.dev, current.metadata.ino)
		if candidateName == current.name {
			return state.breakRetiredControlCanonicalGraph(retired, current)
		}
		progressed, occupant, collision, err := state.rotateRetiredControlEntry(retired, current, candidateName)
		if progressed || err != nil {
			return progressed, err
		}
		if !collision {
			return false, errors.New("productmetrics: retired-control canonical rotation made no progress")
		}
		current = occupant
	}
	return state.breakRetiredControlCanonicalGraph(retired, current)
}

func (state *spoolSweepState) breakRetiredControlCanonicalGraph(retired *storageDir, current storageEntry) (bool, error) {
	span := maximumRelocationSequence - uint64(maximumRelocationSlots)
	start := (current.metadata.dev ^ current.metadata.ino*0x9e3779b97f4a7c15) % (span + 1)
	for offset := 0; offset < maximumRelocationSlots; offset++ {
		candidateName := relocationCandidateName(start + uint64(offset))
		if candidateName == current.name {
			continue
		}
		progressed, _, collision, err := state.rotateRetiredControlEntry(retired, current, candidateName)
		if progressed || err != nil {
			return progressed, err
		}
		if !collision {
			return false, errors.New("productmetrics: retired-control graph breaker made no progress")
		}
	}
	return state.promoteRetiredControlBlocker(retired, current)
}

func (state *spoolSweepState) promoteRetiredControlBlocker(retired *storageDir, blocker storageEntry) (bool, error) {
	if state == nil || state.root == nil || state.root.storageDir == nil || state.meter == nil {
		return false, errStorageClosed
	}
	if !state.meter.chargeFixedEntry(spoolControlDirectoryName) {
		return false, errors.New("productmetrics: cleanup budget cannot inspect graph-breaker promotion target")
	}
	active, activeErr := state.root.lookupEntry(spoolControlDirectoryName)
	if activeErr == nil {
		state.failClosedArmed = true
		exchangeResult, exchangeErr := retired.exchangeEnumeratedEntries(blocker, state.root.storageDir, active)
		if exchangeResult.state != storageRenameNotApplied {
			state.mutated = true
		}
		if !errors.Is(exchangeErr, errStorageExchangeUnsupported) || exchangeResult.state != storageRenameNotApplied {
			return validateRetiredControlRename(exchangeResult, exchangeErr, "active/retired graph-breaker exchange")
		}
		return state.parkActiveControlInRetired(retired, active)
	}
	if !errors.Is(activeErr, fs.ErrNotExist) {
		return false, activeErr
	}
	result, renameErr := retired.renameEnumeratedDirectory(blocker, state.root.storageDir, spoolControlDirectoryName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
		state.failClosedArmed = true
	}
	return validateRetiredControlRename(result, renameErr, "retired-control graph-breaker promotion")
}

func (state *spoolSweepState) parkActiveControlInRetired(retired *storageDir, active storageEntry) (bool, error) {
	reservation, err := state.reserveFallbackRelocationBlock()
	if err != nil {
		return state.mutated, err
	}
	for offset := 0; offset < reservation.slots; offset++ {
		candidateName := fallbackRelocationCandidateName(reservation.cursor, reservation.start+uint64(offset))
		if !state.meter.chargeFixedEntry(candidateName) {
			return state.mutated, errors.New("productmetrics: cleanup budget cannot park active-control collision blocker")
		}
		result, renameErr := state.root.renameEnumeratedEntry(active, retired, candidateName)
		if result.state != storageRenameNotApplied {
			state.mutated = true
			state.failClosedArmed = true
		}
		if errors.Is(renameErr, errStorageDestinationExists) && result.state == storageRenameNotApplied {
			continue
		}
		return validateRetiredControlRename(result, renameErr, "active-control collision parking")
	}
	// Reserving and persisting a fresh cursor incarnation is itself durable
	// progress. A pass whose entire block was occupied therefore stops cleanly;
	// the next pass reserves a disjoint inode-qualified namespace.
	return true, nil
}

type fallbackRelocationReservation struct {
	start  uint64
	slots  int
	cursor recordIncarnation
}

func (state *spoolSweepState) reserveFallbackRelocationBlock() (fallbackRelocationReservation, error) {
	if state == nil || state.root == nil || state.root.storageDir == nil || state.meter == nil {
		return fallbackRelocationReservation{}, errStorageClosed
	}
	if !state.meter.chargeFixedEntry(fallbackRelocationCursorName) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot inspect fallback relocation cursor")
	}

	entry, lookupErr := state.root.lookupEntry(fallbackRelocationCursorName)
	present := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, fs.ErrNotExist) {
		return fallbackRelocationReservation{}, lookupErr
	}
	start := uint64(0)
	if present {
		if !safeFallbackRelocationCursor(entry) {
			return fallbackRelocationReservation{}, errors.New("productmetrics: unsafe fallback relocation cursor cannot reserve names")
		}
		if !state.meter.chargeFixedRead(maximumRelocationBytes + 1) {
			return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot read fallback relocation cursor")
		}
		data, readErr := state.root.readFile(fallbackRelocationCursorName, maximumRelocationBytes)
		cursor, decodeErr := decodeRelocationCursor(data)
		if readErr == nil && decodeErr == nil && cursor.Next <= maximumRelocationSequence-uint64(maximumRelocationSlots) {
			start = cursor.Next
		} else {
			// Corrupt and exhausted cursors recover without reusing their old
			// sequence block. The atomic replacement below also creates a fresh
			// inode, making the final candidate namespace disjoint on reopen.
			start = fallbackRelocationRecoveryStart(entry)
		}
	}
	worstCaseName := fallbackRelocationCandidateName(recordIncarnation{dev: math.MaxUint64, ino: math.MaxUint64}, maximumRelocationSequence)
	// One additional fixed name operation revalidates the cursor after its
	// atomic replacement. Reserve it together with all candidate attempts so a
	// logical meter can never persist a block that it cannot fully consume.
	available := state.meter.availableFixedSlots(uint64(len(worstCaseName)), maximumRelocationSlots+1)
	if available != maximumRelocationSlots+1 {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot reserve a complete fallback relocation block")
	}
	slots := maximumRelocationSlots

	reserved := relocationCursor{Next: start + uint64(slots)}
	data, err := encodeRelocationCursor(reserved)
	if err != nil {
		return fallbackRelocationReservation{}, err
	}
	if !state.meter.chargeFixedRead(uint64(len(data))) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot write fallback relocation cursor")
	}
	if !state.meter.chargeFixedDirectory() {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot open fallback cursor journal")
	}
	result, writeErr := state.root.writeFileAtomicOutcome(fallbackRelocationCursorName, data)
	if result.state != storageWriteNotApplied {
		state.mutated = true
		state.failClosedArmed = true
	}
	if writeErr != nil {
		return fallbackRelocationReservation{}, writeErr
	}
	if result.state != storageWriteAppliedDurable {
		return fallbackRelocationReservation{}, errors.New("productmetrics: fallback relocation cursor reservation was not durable")
	}
	if !state.meter.chargeFixedEntry(fallbackRelocationCursorName) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: cleanup budget cannot revalidate fallback relocation cursor")
	}
	current, err := state.root.lookupEntry(fallbackRelocationCursorName)
	if err != nil {
		return fallbackRelocationReservation{}, err
	}
	if !safeFallbackRelocationCursor(current) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: fallback relocation cursor changed after reservation")
	}
	incarnation := recordIncarnation{dev: current.metadata.dev, ino: current.metadata.ino}
	if present && incarnation == (recordIncarnation{dev: entry.metadata.dev, ino: entry.metadata.ino}) {
		return fallbackRelocationReservation{}, errors.New("productmetrics: fallback relocation cursor replacement did not create a new incarnation")
	}
	if state.afterRelocationReservation != nil {
		if err := state.afterRelocationReservation(); err != nil {
			return fallbackRelocationReservation{}, fmt.Errorf("productmetrics: injected post-reservation failure: %w", err)
		}
	}
	return fallbackRelocationReservation{start: start, slots: slots, cursor: incarnation}, nil
}

func fallbackRelocationRecoveryStart(entry storageEntry) uint64 {
	span := maximumRelocationSequence - uint64(maximumRelocationSlots)
	return (entry.metadata.dev ^ entry.metadata.ino*0x9e3779b97f4a7c15) % (span + 1)
}

func fallbackRelocationCandidateName(cursor recordIncarnation, sequence uint64) string {
	return fmt.Sprintf(".pm-fallback-%x-%x-%016x", cursor.dev, cursor.ino, sequence)
}

func (state *spoolSweepState) rotateRetiredControlEntry(retired *storageDir, current storageEntry, candidateName string) (bool, storageEntry, bool, error) {
	if !state.meter.chargeFixedEntry(candidateName) {
		return false, storageEntry{}, false, errors.New("productmetrics: cleanup budget cannot rotate retired-control collision blocker")
	}
	result, renameErr := retired.renameEnumeratedDirectory(current, retired, candidateName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if !errors.Is(renameErr, errStorageDestinationExists) {
		progressed, err := validateRetiredControlRename(result, renameErr, "retired-control collision rotation")
		return progressed, storageEntry{}, false, err
	}
	if result.state != storageRenameNotApplied {
		return true, storageEntry{}, false, renameErr
	}

	occupant, lookupErr := retired.lookupEntry(candidateName)
	if lookupErr != nil {
		return false, storageEntry{}, false, lookupErr
	}
	if unlinkErr := retired.unlinkEnumeratedEntry(occupant); unlinkErr == nil {
		state.mutated = true
		return true, storageEntry{}, false, nil
	} else if !errors.Is(unlinkErr, errStorageEntryIsDirectory) {
		return false, storageEntry{}, false, unlinkErr
	}
	if removeErr := retired.removeEnumeratedCleanupDirectory(occupant); removeErr == nil {
		state.mutated = true
		return true, storageEntry{}, false, nil
	} else if !errors.Is(removeErr, errStorageDirectoryNotEmpty) {
		return false, storageEntry{}, false, removeErr
	}
	return false, occupant, true, nil
}

func validateRetiredControlRename(result storageRenameResult, err error, operation string) (bool, error) {
	progressed := result.state != storageRenameNotApplied
	if err != nil {
		return progressed, err
	}
	if result.state != storageRenameAppliedDurable {
		return progressed, fmt.Errorf("productmetrics: %s was not durable", operation)
	}
	return true, nil
}

func (state *spoolSweepState) retireRelocationCursor(control *storageDir, entry storageEntry, cause error) error {
	if entry.name == "" {
		return cause
	}
	return state.retireActiveControlNamespace(control, cause)
}

func (state *spoolSweepState) retireActiveControlNamespace(control *storageDir, cause error) error {
	if state == nil || state.root == nil {
		return errors.Join(cause, errStorageClosed)
	}
	if control != nil {
		if err := control.Close(); err != nil {
			return errors.Join(cause, err)
		}
		if state.retainedControl == control {
			state.retainedControl = nil
		}
	}
	entry, err := state.root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return cause
	}
	if err != nil {
		return errors.Join(cause, err)
	}
	if entry.metadata.kind != storageEntryDirectory {
		if !state.ensureAlternateControlBarrier(retiredControlDirectoryName) {
			return errors.Join(cause, errors.New("productmetrics: cannot remove active control without replacement fail-closed evidence"))
		}
		unlinkErr := state.root.unlinkEnumeratedEntry(entry)
		if unlinkErr == nil {
			state.mutated = true
			return nil
		}
		if errors.Is(unlinkErr, fs.ErrNotExist) {
			return nil
		}
		return errors.Join(cause, unlinkErr)
	}
	result, retireErr := state.root.renameEnumeratedDirectory(entry, state.root.storageDir, retiredControlDirectoryName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if errors.Is(retireErr, errStorageDestinationExists) {
		return nil
	}
	if retireErr != nil {
		return errors.Join(cause, retireErr)
	}
	if result.state != storageRenameAppliedDurable {
		return errors.Join(cause, errors.New("productmetrics: active control retirement was not durable"))
	}
	return nil
}

func (state *spoolSweepState) ensureConservativeRelocationQuota(control *storageDir) error {
	if !state.meter.chargeFixedEntry(quotaFileName) || !state.meter.chargeFixedRead(maximumQuotaBytes+1) {
		return errors.New("productmetrics: cleanup budget cannot inspect quota before relocation")
	}
	quota, present, loadErr := loadSpoolQuota(state.root)
	markers := spoolQuota{Events: maximumQuotaEventMarker, Bytes: maximumQuotaByteMarker}
	if loadErr == nil && present {
		if state.purgeAll {
			return nil
		}
		if quota == markers {
			state.relocationQuotaMarked = true
			return nil
		}
	}
	data, encodeErr := encodeSpoolQuota(markers)
	if encodeErr != nil {
		return errors.Join(loadErr, encodeErr)
	}
	if !state.meter.chargeFixedRead(uint64(len(data))) {
		return errors.Join(loadErr, errors.New("productmetrics: cleanup budget cannot write conservative relocation quota"))
	}
	if err := persistSpoolQuotaFromControl(state.root, control, markers, loadErr == nil && !present); err != nil {
		return errors.Join(loadErr, err)
	}
	state.mutated = true
	state.relocationQuotaMarked = true
	return nil
}

func (state *spoolSweepState) relocateQuarantineBlocker(quarantineRoot *storageDir, blocker storageEntry, eventTree bool) {
	targetName := fmt.Sprintf(".orphan-%x-%x", blocker.metadata.dev, blocker.metadata.ino)
	if !state.meter.chargeFixedEntry(targetName) {
		return
	}
	result, err := quarantineRoot.renameEnumeratedDirectory(blocker, quarantineRoot, targetName)
	if result.state != storageRenameNotApplied {
		state.mutated = true
	}
	if errors.Is(err, errStorageDestinationExists) {
		state.resolveQuarantineCollision(quarantineRoot, quarantineRoot, blocker, targetName, eventTree)
		return
	}
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	if result.state != storageRenameAppliedDurable {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: ancestor blocker relocation was not durable"))
	}
}

func (state *spoolSweepState) scanCurrentGeneration(treeName, generationName string, directory, quarantineRoot *storageDir) {
	if directory.cleanupOnly() {
		state.purgeDirectory(directory, quarantineRoot, true)
		return
	}
	if state.pruneDirs[treeName] == nil {
		retained, err := directory.openDir(nil, false)
		if err != nil {
			state.operation = errors.Join(state.operation, err)
			return
		}
		state.pruneDirs[treeName] = retained
	}
	iterator, err := directory.iterateEntries()
	if err != nil {
		state.operation = errors.Join(state.operation, err)
		return
	}
	defer func() { state.operation = errors.Join(state.operation, iterator.Close()) }()
	for {
		entry, ok := state.meter.next(iterator)
		if !ok {
			return
		}
		if entry.metadata.kind != storageEntryDirectory {
			if !state.meter.chargeEventEntry() {
				return
			}
			state.scanCurrentLeaf(treeName, generationName, directory, entry)
			continue
		}
		if !state.meter.chargeCleanupDirectory() {
			state.quarantineDirectory(directory, quarantineRoot, entry, false, true)
			return
		}
		child, openErr := openEnumeratedStorageDirectory(directory, entry)
		if openErr == nil {
			state.purgeDirectory(child, quarantineRoot, true)
			state.operation = errors.Join(state.operation, child.Close())
			if errors.Is(state.operation, syscall.EXDEV) {
				return
			}
			if !state.meter.exhausted {
				if !state.ensureFailClosedControl() {
					return
				}
				if err := directory.removeEnumeratedCleanupDirectory(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
					state.operation = errors.Join(state.operation, err)
				} else if err == nil {
					state.mutated = true
				}
			}
			continue
		}
		if errors.Is(openErr, syscall.EXDEV) {
			state.operation = errors.Join(state.operation, openErr)
			return
		}
		state.operation = errors.Join(state.operation, directoryDescriptorExhaustion(openErr))
		state.meter.refundLogicalDirectoryCharge()
		if !state.meter.chargeEventEntry() {
			return
		}
		state.scanCurrentLeaf(treeName, generationName, directory, entry)
	}
}

func directoryDescriptorExhaustion(err error) error {
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.EXDEV) {
		return err
	}
	return nil
}

func (state *spoolSweepState) scanCurrentLeaf(treeName, generationName string, directory *storageDir, entry storageEntry) {
	eventID, validName := eventIDFromFileName(entry.name)
	if !validName || entry.metadata.size < 0 || uint64(entry.metadata.size) > maximumEventBytes {
		state.deleteLeaf(directory, entry, true)
		return
	}
	const readReservation = maximumEventBytes + 1
	if !state.meter.chargeRead(readReservation) {
		state.makeOverflowProgress(directory, entry)
		return
	}
	data, physicalReadBytes, lease, err := directory.readFileMeasured(entry.name, int64(maximumEventBytes))
	if lease != nil {
		err = errors.Join(err, lease.Close())
	}
	state.meter.refundRead(readReservation, physicalReadBytes)
	if err != nil {
		if errors.Is(err, syscall.EXDEV) {
			state.operation = errors.Join(state.operation, err)
			state.addConservativeEntry(entry)
			return
		}
		state.deleteLeaf(directory, entry, true)
		return
	}
	event, err := DecodeEvent(data)
	if err != nil || event.EventID != eventID || event.InstallationID != state.policy.installationID {
		state.deleteLeaf(directory, entry, true)
		return
	}
	canonical, err := EncodeEvent(event)
	if err != nil || !bytes.Equal(canonical, data) {
		state.deleteLeaf(directory, entry, true)
		return
	}
	occurred, err := parseCanonicalHourUTC(event.OccurredHourUTC)
	if err != nil || !eventWithinRetention(occurred, state.now) {
		state.deleteLeaf(directory, entry, true)
		return
	}
	record := spoolRecord{
		tree: treeName, generation: generationName, name: entry.name, event: event,
		bytes: uint64(len(data)), incarnation: recordIncarnation{dev: entry.metadata.dev, ino: entry.metadata.ino},
		mtimeSeconds: entry.metadata.mtimeSeconds, mtimeNanoseconds: entry.metadata.mtimeNanoseconds,
	}
	if _, duplicate := state.seen[event.EventID]; duplicate {
		existing := -1
		for index := range state.records {
			if state.records[index].event.EventID == event.EventID {
				existing = index
				break
			}
		}
		if existing < 0 || !spoolRecordLess(record, state.records[existing]) {
			state.deleteLeaf(directory, entry, true)
			return
		}
		if !state.deleteValidRecord(existing) {
			state.deleteLeaf(directory, entry, true)
			return
		}
	} else {
		state.seen[event.EventID] = struct{}{}
	}
	state.records = append(state.records, record)
	events, eventsOK := checkedAddUint64(state.quota.Events, 1)
	bytes, bytesOK := checkedAddUint64(state.quota.Bytes, record.bytes)
	if !eventsOK || !bytesOK {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: reconciled quota overflow"))
		return
	}
	state.quota = spoolQuota{Events: events, Bytes: bytes}
}

func (state *spoolSweepState) makeOverflowProgress(current *storageDir, entry storageEntry) {
	if !state.ensureFailClosedControl() {
		state.addConservativeEntry(entry)
		return
	}
	currentRecord := spoolRecord{
		name:             entry.name,
		mtimeSeconds:     entry.metadata.mtimeSeconds,
		mtimeNanoseconds: entry.metadata.mtimeNanoseconds,
	}
	oldest := -1
	for index := range state.records {
		if oldest == -1 || spoolRecordLess(state.records[index], state.records[oldest]) {
			oldest = index
		}
	}
	if oldest == -1 || spoolRecordLess(currentRecord, state.records[oldest]) {
		if err := current.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
			state.operation = errors.Join(state.operation, err)
		} else {
			if err == nil {
				state.mutated = true
			}
			state.noteRemoved(entry.metadata.size)
		}
		return
	}
	state.deleteValidRecord(oldest)
}

func eventWithinRetention(occurred, now time.Time) bool {
	occurred = occurred.UTC().Truncate(time.Hour)
	now = now.UTC().Truncate(time.Hour)
	if occurred.After(now) {
		return false
	}
	cutoff := now.Add(-maximumEventAgeHours * time.Hour)
	return !occurred.Before(cutoff)
}

func (state *spoolSweepState) deleteLeaf(directory *storageDir, entry storageEntry, eventTree bool) {
	if !state.ensureFailClosedControl() {
		if eventTree {
			state.addConservativeEntry(entry)
		}
		return
	}
	if err := directory.unlinkEnumeratedEntry(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
		if eventTree {
			state.addConservativeEntry(entry)
		}
	} else {
		if err == nil {
			state.mutated = true
		}
		if eventTree {
			state.noteRemoved(entry.metadata.size)
		}
	}
}

func (state *spoolSweepState) deleteJournaledRootTemp(
	entry storageEntry,
	expected recordIncarnation,
	guard func() error,
) {
	if state == nil || state.root == nil || state.root.backend == nil {
		return
	}
	// A strict durable binding to this exact incarnation is the deletion
	// authority for the root temp.
	// Root staging files carry no event quota, so replay must not create an
	// event fail-closed control namespace merely to retire crash residue.
	if expected == (recordIncarnation{}) || expected != (recordIncarnation{dev: entry.metadata.dev, ino: entry.metadata.ino}) {
		state.operation = errors.Join(state.operation, errUnsettledRootTempJournal)
		return
	}
	if err := state.root.backend.removeFileMatchingGuarded(entry.name, expected, guard); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.operation = errors.Join(state.operation, err)
		return
	}
	state.mutated = true
}

func (state *spoolSweepState) noteRemoved(size int64) {
	if state.removedEvents < math.MaxUint64 {
		state.removedEvents++
	}
	if size <= 0 || state.removedBytes == math.MaxUint64 {
		return
	}
	bytes, ok := checkedAddUint64(state.removedBytes, uint64(size))
	if !ok {
		state.removedBytes = math.MaxUint64
		return
	}
	state.removedBytes = bytes
}

func (state *spoolSweepState) addConservativeEntry(entry storageEntry) {
	bytes := uint64(0)
	if entry.metadata.size < 0 || uint64(entry.metadata.size) > maximumQuotaByteMarker {
		bytes = maximumQuotaByteMarker
	} else {
		bytes = uint64(entry.metadata.size)
	}
	state.addQuota(spoolQuota{Events: 1, Bytes: bytes})
}

func (state *spoolSweepState) addQuota(add spoolQuota) {
	if add.Events >= maximumQuotaEventMarker || state.quota.Events >= maximumQuotaEventMarker-add.Events {
		state.quota.Events = maximumQuotaEventMarker
	} else {
		state.quota.Events += add.Events
	}
	if add.Bytes >= maximumQuotaByteMarker || state.quota.Bytes >= maximumQuotaByteMarker-add.Bytes {
		state.quota.Bytes = maximumQuotaByteMarker
	} else {
		state.quota.Bytes += add.Bytes
	}
}

func (state *spoolSweepState) pruneOldestToQuota() {
	for (state.quota.Events > maximumSpoolEvents || state.quota.Bytes > maximumSpoolBytes) && len(state.records) > 0 {
		oldest := 0
		for index := 1; index < len(state.records); index++ {
			if spoolRecordLess(state.records[index], state.records[oldest]) {
				oldest = index
			}
		}
		if !state.deleteValidRecord(oldest) {
			return
		}
	}
}

func (state *spoolSweepState) deleteValidRecord(index int) bool {
	if index < 0 || index >= len(state.records) {
		return false
	}
	record := state.records[index]
	directory := state.pruneDirs[record.tree]
	if directory == nil {
		state.operation = errors.Join(state.operation, errors.New("productmetrics: missing retained generation handle for pruning"))
		return false
	}
	if !state.ensureFailClosedControl() {
		return false
	}
	if err := directory.removeFile(record.name); err != nil {
		state.operation = errors.Join(state.operation, err)
		return false
	}
	state.mutated = true
	state.quota.Events--
	state.quota.Bytes -= record.bytes
	state.noteRemoved(int64(record.bytes))
	copy(state.records[index:], state.records[index+1:])
	state.records = state.records[:len(state.records)-1]
	return true
}

func (state *spoolSweepState) finish() (spoolSweepResult, error) {
	if state.restoreDirectoryOpenHooks != nil {
		defer state.restoreDirectoryOpenHooks()
	}
	for tree, directory := range state.pruneDirs {
		state.operation = errors.Join(state.operation, directory.Close())
		delete(state.pruneDirs, tree)
	}
	// Mutating a directory while a live getdents/readdir iterator is open can
	// make that iterator skip a later entry on some filesystems. A successful
	// tree mutation therefore makes this pass progress-only; only a subsequent
	// bounded mutation-free pass may certify exact quota or an empty spool.
	complete := state.traversed && !state.mutated && !state.meter.exhausted && state.meter.traversalError == nil && state.operation == nil
	target := state.quota
	if state.purgeAll && complete {
		target = spoolQuota{}
	}
	persistQuota := true
	if !complete {
		if state.purgeAll {
			// Consent cleanup leaves the existing conservative reservation
			// untouched until the event tree is proven empty. In particular, do
			// not spend bytes outside the one global cleanup budget rereading it.
			persistQuota = false
		} else {
			target.Events = maximumQuotaEventMarker
			target.Bytes = maximumQuotaByteMarker
			if state.relocationQuotaMarked || state.failClosedArmed {
				persistQuota = false
			}
		}
	}
	var persistErr error
	if persistQuota {
		switch {
		case !state.meter.claimFixedWorkEnvelope():
			persistErr = errors.New("productmetrics: cleanup budget cannot reserve fixed quota persistence work")
		case state.retainedControl != nil:
			persistErr = state.persistQuotaFromRetainedControl(target)
		case !state.meter.chargeFixedDirectory():
			persistErr = errors.New("productmetrics: cleanup budget cannot open quota staging directory")
		default:
			persistErr = persistSpoolQuota(state.root, target)
		}
	}
	if state.retainedControl != nil {
		persistErr = errors.Join(persistErr, state.retainedControl.Close())
		state.retainedControl = nil
	}
	if state.purgeAll && complete && persistErr == nil {
		// Quota persistence mutates the control/root namespace after the clean
		// traversal. Re-prove the persistent root journal with the same meter
		// before certifying the combined result.
		state.journalSettled = false
		state.journalFixedDirectory = true
		state.cleanupRootTempJournal()
		complete = state.journalSettled && !state.mutated && !state.meter.exhausted &&
			state.meter.traversalError == nil && state.operation == nil
	}
	result := spoolSweepResult{
		complete: complete && persistErr == nil, usage: state.meter.usage, eventEntries: state.meter.eventEntries,
		meter: state.meter, quota: target,
		removedEvents: state.removedEvents, removedBytes: state.removedBytes,
	}
	err := errors.Join(state.meter.traversalError, state.operation, persistErr)
	return result, err
}

func proveRootTempJournalReadOnlyWithMeter(root *storageRoot, meter *spoolWorkMeter, fixedDirectory bool) (returnErr error) {
	if root == nil || root.storageDir == nil || root.backend == nil || meter == nil {
		return errStorageClosed
	}
	restore := root.installDirectoryOpenHooks(meter.beforePhysicalDirectoryOpen, meter.afterPhysicalDirectoryOpen)
	defer restore()
	chargeName := meter.chargeNamedEntry
	if fixedDirectory {
		chargeName = meter.chargeFixedEntry
	}
	if !chargeName(rootTempJournalDirectoryName) {
		return errUnsettledRootTempJournal
	}
	entry, err := root.lookupEntry(rootTempJournalDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		if syncErr := root.syncDirectory(); syncErr != nil {
			return syncErr
		}
		if !chargeName(rootTempJournalDirectoryName) {
			return errUnsettledRootTempJournal
		}
		if _, recheckErr := root.lookupEntry(rootTempJournalDirectoryName); errors.Is(recheckErr, fs.ErrNotExist) {
			return nil
		} else if recheckErr != nil {
			return recheckErr
		}
		return errUnsettledRootTempJournal
	}
	if err != nil {
		return err
	}
	var directoryCharged bool
	if fixedDirectory {
		directoryCharged = meter.chargeFixedTraversalDirectory()
	} else {
		directoryCharged = meter.chargeDirectory()
	}
	if entry.metadata.kind != storageEntryDirectory || !directoryCharged {
		return errUnsettledRootTempJournal
	}
	journal, err := root.openEnumeratedCleanupDirectory(entry)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, journal.Close()) }()
	if journal.cleanupOnly() {
		return errUnsettledRootTempJournal
	}
	if err := journal.syncDirectory(); err != nil {
		return err
	}
	if err := root.syncDirectory(); err != nil {
		return err
	}
	iterator, err := journal.iterateEntries()
	if err != nil {
		return err
	}
	defer func() {
		if iterator != nil {
			returnErr = errors.Join(returnErr, iterator.Close())
		}
	}()
	if !meter.reserveRootTempJournalSentinel(fixedDirectory) {
		return errUnsettledRootTempJournal
	}
	for {
		var marker storageEntry
		var ok bool
		if fixedDirectory {
			marker, err = iterator.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if !chargeName(marker.name) {
				return errUnsettledRootTempJournal
			}
			if !meter.acceptRootTempJournalMarker() {
				return errUnsettledRootTempJournal
			}
			ok = true
		} else {
			marker, ok = meter.next(iterator)
			if !ok {
				if meter.traversalError != nil {
					return meter.traversalError
				}
				if meter.exhausted {
					return errUnsettledRootTempJournal
				}
				break
			}
			if !meter.acceptRootTempJournalMarker() {
				return errUnsettledRootTempJournal
			}
		}
		if !ok {
			return errUnsettledRootTempJournal
		}
		loadedMarker, markerErr := loadRootTempJournalMarker(meter, journal, entry, marker, fixedDirectory)
		if markerErr != nil {
			return errors.Join(markerErr, errUnsettledRootTempJournal)
		}
		markerLease := loadedMarker.lease
		if !chargeName(marker.name) {
			_ = markerLease.Close()
			return errUnsettledRootTempJournal
		}
		_, lookupErr := root.lookupEntry(marker.name)
		if !errors.Is(lookupErr, fs.ErrNotExist) {
			_ = markerLease.Close()
			if lookupErr != nil {
				return lookupErr
			}
			return errUnsettledRootTempJournal
		}
		if !chargeName(marker.name) {
			_ = markerLease.Close()
			return errUnsettledRootTempJournal
		}
		if absenceErr := root.confirmEntryAbsent(marker.name); absenceErr != nil {
			_ = markerLease.Close()
			return errors.Join(absenceErr, errUnsettledRootTempJournal)
		}
		if !chargeName(rootTempJournalDirectoryName) {
			_ = markerLease.Close()
			return errUnsettledRootTempJournal
		}
		namedJournal, journalErr := root.lookupEntry(rootTempJournalDirectoryName)
		if journalErr != nil || !samePrivateJournalDirectoryEntry(entry, namedJournal) {
			_ = markerLease.Close()
			return errors.Join(journalErr, errUnsettledRootTempJournal)
		}
		if !chargeName(marker.name) {
			_ = markerLease.Close()
			return errUnsettledRootTempJournal
		}
		namedMarker, markerLookupErr := journal.lookupEntry(marker.name)
		if markerLookupErr != nil || !sameRootTempJournalMarkerIdentity(marker, namedMarker, markerLease, entry.metadata.dev) {
			_ = markerLease.Close()
			return errors.Join(markerLookupErr, errUnsettledRootTempJournal)
		}
		if closeErr := markerLease.Close(); closeErr != nil {
			return closeErr
		}
	}
	if closeErr := iterator.Close(); closeErr != nil {
		iterator = nil
		return closeErr
	}
	iterator = nil
	if !chargeName(rootTempJournalDirectoryName) {
		return errUnsettledRootTempJournal
	}
	named, lookupErr := root.lookupEntry(rootTempJournalDirectoryName)
	if lookupErr != nil {
		return lookupErr
	}
	if !samePrivateJournalDirectoryEntry(entry, named) {
		return errUnsettledRootTempJournal
	}
	return nil
}

func (state *spoolSweepState) persistQuotaFromRetainedControl(quota spoolQuota) error {
	control := state.retainedControl
	if control == nil {
		return errStorageClosed
	}
	persistErr := persistSpoolQuotaFromControl(state.root, control, quota, false)
	closeErr := control.Close()
	state.retainedControl = nil
	if persistErr != nil || closeErr != nil {
		return errors.Join(persistErr, closeErr)
	}
	entry, err := state.root.lookupEntry(spoolControlDirectoryName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	removeErr := state.root.removeEnumeratedCleanupDirectory(entry)
	if errors.Is(removeErr, errStorageDirectoryNotEmpty) || errors.Is(removeErr, fs.ErrNotExist) {
		return nil
	}
	return removeErr
}

func sortSpoolRecords(records []spoolRecord) {
	sort.Slice(records, func(left, right int) bool {
		return spoolRecordLess(records[left], records[right])
	})
}

func spoolRecordLess(left, right spoolRecord) bool {
	if left.mtimeSeconds != right.mtimeSeconds {
		return left.mtimeSeconds < right.mtimeSeconds
	}
	if left.mtimeNanoseconds != right.mtimeNanoseconds {
		return left.mtimeNanoseconds < right.mtimeNanoseconds
	}
	return left.name < right.name
}

// claimSpoolBatch is a caller-held uploader.lock-then-state.lock primitive.
// The caller revalidates permit against the current config record before entry;
// the returned claim contains one oldest-first, same-release generation batch.
func claimSpoolBatch(root *storageRoot, permit RecordingPermit, now time.Time, budget spoolWorkBudget) (spoolClaim, error) {
	if !permit.Valid() {
		return spoolClaim{}, errors.New("productmetrics: invalid permit cannot claim a spool batch")
	}
	state := runSpoolSweep(root, policyFromPermit(permit), now, budget, false)
	result, err := state.finish()
	if err != nil {
		return spoolClaim{}, err
	}
	if !result.complete {
		return spoolClaim{}, nil
	}

	queue, err := root.openDir([]string{queueDirectoryName, permit.spoolGeneration}, true)
	if err != nil {
		return spoolClaim{}, err
	}
	defer func() { _ = queue.Close() }()
	inflight, err := root.openDir([]string{inflightDirectoryName, permit.spoolGeneration}, true)
	if err != nil {
		return spoolClaim{}, err
	}
	defer func() { _ = inflight.Close() }()
	for index := range state.records {
		record := &state.records[index]
		if record.tree != inflightDirectoryName {
			continue
		}
		result, renameErr := inflight.renameFile(record.name, queue, record.name)
		if renameErr != nil {
			if errors.Is(renameErr, errStorageDestinationExists) {
				if duplicateErr := retireExactDuplicateSpoolRecord(inflight, queue, *record); duplicateErr != nil {
					return spoolClaim{}, errors.Join(renameErr, duplicateErr)
				}
				continue
			}
			return spoolClaim{}, renameErr
		}
		if result.state != storageRenameAppliedDurable {
			return spoolClaim{}, errors.New("productmetrics: inflight restore was not durable")
		}
		record.tree = queueDirectoryName
	}

	candidates := state.records[:0]
	for _, record := range state.records {
		if record.tree == queueDirectoryName {
			candidates = append(candidates, record)
		}
	}
	sortSpoolRecords(candidates)
	claim := spoolClaim{generation: permit.spoolGeneration}
	batchRelease := ""
	for _, record := range candidates {
		if len(claim.records) >= maximumBatchEvents {
			break
		}
		if batchRelease == "" {
			batchRelease = record.event.ReleaseVersion
		} else if record.event.ReleaseVersion != batchRelease {
			break
		}
		candidateRecords := append(append([]spoolRecord(nil), claim.records...), record)
		candidateEvents := make([]Event, len(candidateRecords))
		for index := range candidateRecords {
			candidateEvents[index] = candidateRecords[index].event
		}
		encoded, encodeErr := EncodeBatch(Batch{SchemaVersion: SchemaVersionV1, Events: candidateEvents})
		if encodeErr != nil {
			return spoolClaim{}, encodeErr
		}
		if len(encoded) > maximumRequestBytes {
			break
		}
		claim.records = candidateRecords
	}
	if len(claim.records) == 0 {
		return claim, nil
	}
	claimed := spoolClaim{generation: claim.generation, authority: &spoolClaimAuthority{}}
	for _, record := range claim.records {
		result, renameErr := queue.renameFile(record.name, inflight, record.name)
		if result.state != storageRenameNotApplied {
			record.tree = inflightDirectoryName
			claimed.records = append(claimed.records, record)
		}
		if renameErr != nil || result.state != storageRenameAppliedDurable {
			restoreErr := restoreSpoolClaim(root, claimed)
			if renameErr == nil {
				renameErr = errors.New("productmetrics: queue claim was not durable")
			}
			return spoolClaim{}, errors.Join(renameErr, restoreErr)
		}
	}
	return claimed, nil
}

// restoreSpoolClaim is a caller-held uploader.lock-then-state.lock primitive.
// Settlement authority is shared by copied claims, so restore and delete can
// never both consume the same claim in one process.
func restoreSpoolClaim(root *storageRoot, claim spoolClaim) error {
	if root == nil || !validCanonicalUUIDv4(claim.generation) {
		return errors.New("productmetrics: invalid spool claim")
	}
	settle, err := claim.beginSettlement()
	if err != nil {
		return err
	}
	defer settle()
	queue, err := root.openDir([]string{queueDirectoryName, claim.generation}, true)
	if err != nil {
		return err
	}
	defer func() { _ = queue.Close() }()
	inflight, err := root.openDir([]string{inflightDirectoryName, claim.generation}, true)
	if err != nil {
		return err
	}
	defer func() { _ = inflight.Close() }()
	var restoreErr error
	for _, record := range claim.records {
		if record.generation != claim.generation || record.name != eventFileName(record.event.EventID) ||
			record.bytes == 0 || record.bytes > maximumEventBytes || record.incarnation == (recordIncarnation{}) {
			restoreErr = errors.Join(restoreErr, errors.New("productmetrics: malformed claimed record"))
			continue
		}
		result, err := inflight.renameFile(record.name, queue, record.name)
		if err == nil && result.state == storageRenameAppliedDurable {
			continue
		}
		if errors.Is(err, errStorageDestinationExists) {
			err = retireExactDuplicateSpoolRecord(inflight, queue, record)
			if err == nil {
				continue
			}
		}
		if errors.Is(err, fs.ErrNotExist) && queuedRecordMatches(queue, record) {
			continue
		}
		if err == nil {
			err = errors.New("productmetrics: claim restore was not durable")
		}
		restoreErr = errors.Join(restoreErr, err)
	}
	return restoreErr
}

func retireExactDuplicateSpoolRecord(source, destination *storageDir, record spoolRecord) error {
	if source == nil || destination == nil || record.name != eventFileName(record.event.EventID) ||
		record.bytes == 0 || record.bytes > maximumEventBytes || record.incarnation == (recordIncarnation{}) {
		return errors.New("productmetrics: invalid duplicate spool record")
	}
	want, err := EncodeEvent(record.event)
	if err != nil || uint64(len(want)) != record.bytes {
		return errors.Join(err, errors.New("productmetrics: invalid duplicate spool record bytes"))
	}
	destinationData, destinationLease, err := destination.readFileLease(record.name, int64(maximumEventBytes))
	if err != nil || destinationLease == nil {
		if destinationLease != nil {
			err = errors.Join(err, destinationLease.Close())
		}
		return errors.Join(err, errors.New("productmetrics: cannot prove duplicate queue destination"))
	}
	if !bytes.Equal(destinationData, want) {
		return errors.Join(destinationLease.Close(), errors.New("productmetrics: queue destination does not match inflight record"))
	}
	if err := destination.validateFileMatchingLease(record.name, destinationLease); err != nil {
		return errors.Join(err, destinationLease.Close(), errors.New("productmetrics: queue destination identity is unsafe"))
	}
	sourceData, sourceLease, err := source.readFileLease(record.name, int64(maximumEventBytes))
	if err != nil || sourceLease == nil {
		if sourceLease != nil {
			err = errors.Join(err, sourceLease.Close())
		}
		return errors.Join(err, destinationLease.Close(), errors.New("productmetrics: cannot prove duplicate inflight source"))
	}
	if !bytes.Equal(sourceData, want) || sourceLease.incarnation() != record.incarnation {
		return errors.Join(sourceLease.Close(), destinationLease.Close(),
			errors.New("productmetrics: duplicate spool identities changed before retirement"))
	}
	if err := source.validateFileMatchingLease(record.name, sourceLease); err != nil {
		return errors.Join(err, sourceLease.Close(), destinationLease.Close(),
			errors.New("productmetrics: duplicate inflight source identity is unsafe"))
	}
	if err := destination.validateFileMatchingLease(record.name, destinationLease); err != nil {
		return errors.Join(err, sourceLease.Close(), destinationLease.Close(),
			errors.New("productmetrics: duplicate queue destination identity changed before retirement"))
	}
	if err := revalidateDuplicateSpoolDestination(destination, record.name, want, destinationLease); err != nil {
		return errors.Join(err, sourceLease.Close(), destinationLease.Close(),
			errors.New("productmetrics: duplicate queue destination changed before source-authoritative exchange"))
	}
	exchangeResult, exchangeErr := source.exchangeFilesMatchingLeases(
		record.name, sourceLease, destination, record.name, destinationLease,
	)
	if exchangeErr != nil {
		return errors.Join(exchangeErr, sourceLease.Close(), destinationLease.Close())
	}
	if exchangeResult.state != storageRenameAppliedDurable {
		return errors.Join(sourceLease.Close(), destinationLease.Close(),
			errors.New("productmetrics: duplicate spool exchange was not durable"))
	}
	// The exact claimed source is now authoritative at queue. The old queue
	// destination is displaced to inflight and may be deleted only after the
	// two-parent exchange is durable and queue still names the claimed source.
	removeErr := source.removeFileMatchingLeaseGuarded(record.name, destinationLease, func() error {
		return revalidateDuplicateSpoolDestination(destination, record.name, want, sourceLease)
	})
	return errors.Join(removeErr, sourceLease.Close(), destinationLease.Close())
}

func revalidateDuplicateSpoolDestination(destination *storageDir, name string, want []byte, expected *storageRecordLease) error {
	if destination == nil || expected == nil {
		return errors.New("productmetrics: missing duplicate queue destination authority")
	}
	data, current, err := destination.readFileLease(name, int64(maximumEventBytes))
	if err != nil || current == nil {
		if current != nil {
			err = errors.Join(err, current.Close())
		}
		return errors.Join(err, errors.New("productmetrics: duplicate queue destination cannot be revalidated"))
	}
	defer func() { _ = current.Close() }()
	if !bytes.Equal(data, want) || !expected.Matches(current) {
		return errors.New("productmetrics: duplicate queue destination changed before source retirement")
	}
	return destination.validateFileMatchingLease(name, current)
}

func queuedRecordMatches(queue *storageDir, record spoolRecord) bool {
	data, err := queue.readFile(record.name, int64(maximumEventBytes))
	if err != nil {
		return false
	}
	want, err := EncodeEvent(record.event)
	return err == nil && bytes.Equal(data, want)
}

// deleteSpoolClaim is a caller-held uploader.lock-then-state.lock primitive.
// Files are durably removed before quota is lowered; every uncertain window
// therefore leaves a conservative overcount for reconciliation.
func deleteSpoolClaim(root *storageRoot, claim spoolClaim) error {
	if root == nil || !validCanonicalUUIDv4(claim.generation) {
		return errors.New("productmetrics: invalid spool claim")
	}
	settle, err := claim.beginSettlement()
	if err != nil {
		return err
	}
	defer settle()
	inflight, err := root.openDir([]string{inflightDirectoryName, claim.generation}, false)
	if errors.Is(err, fs.ErrNotExist) && len(claim.records) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = inflight.Close() }()
	seen := make(map[string]struct{}, len(claim.records))
	released := spoolQuota{}
	var deleteErr error
	for _, record := range claim.records {
		if record.generation != claim.generation || record.name != eventFileName(record.event.EventID) ||
			record.bytes == 0 || record.bytes > maximumEventBytes || record.incarnation == (recordIncarnation{}) {
			deleteErr = errors.Join(deleteErr, errors.New("productmetrics: malformed claimed record"))
			continue
		}
		if _, duplicate := seen[record.name]; duplicate {
			deleteErr = errors.Join(deleteErr, errors.New("productmetrics: duplicate claimed record"))
			continue
		}
		seen[record.name] = struct{}{}
		removed, err := deleteOneClaimedRecord(root, inflight, claim.generation, record)
		if err != nil {
			deleteErr = errors.Join(deleteErr, err)
			continue
		}
		if !removed {
			continue
		}
		bytes, ok := checkedAddUint64(released.Bytes, record.bytes)
		if !ok {
			deleteErr = errors.Join(deleteErr, errors.New("productmetrics: claimed-byte release overflow"))
			continue
		}
		released.Events++
		released.Bytes = bytes
	}
	if released.Events == 0 && released.Bytes == 0 {
		return deleteErr
	}
	quota, present, err := loadSpoolQuota(root)
	if err != nil {
		return errors.Join(deleteErr, err)
	}
	if !present {
		return errors.Join(deleteErr, errors.New("productmetrics: quota is absent while settling a spool claim"))
	}
	quota, err = quota.release(released.Events, released.Bytes)
	if err != nil {
		return errors.Join(deleteErr, err)
	}
	return errors.Join(deleteErr, persistSpoolQuota(root, quota))
}

func deleteOneClaimedRecord(root *storageRoot, inflight *storageDir, generation string, record spoolRecord) (bool, error) {
	data, lease, readErr := inflight.readFileLease(record.name, int64(maximumEventBytes))
	closeLease := func() error {
		if lease == nil {
			return nil
		}
		return lease.Close()
	}
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
		if closeErr := closeLease(); closeErr != nil {
			return false, closeErr
		}
		if err := inflight.confirmEntryAbsent(record.name); err != nil {
			return false, err
		}
		restored, err := missingClaimQueueDisposition(root, generation, record)
		if err != nil {
			return false, err
		}
		return !restored, nil
	case readErr != nil:
		return false, errors.Join(readErr, closeLease())
	}
	want, encodeErr := EncodeEvent(record.event)
	if encodeErr != nil || uint64(len(data)) != record.bytes || !bytes.Equal(data, want) {
		return false, errors.Join(encodeErr, errors.New("productmetrics: claimed event changed before deletion"), closeLease())
	}
	if lease == nil {
		return false, errors.New("productmetrics: claimed event read returned no record lease")
	}
	if lease.incarnation() != record.incarnation {
		return false, errors.Join(errors.New("productmetrics: claimed event incarnation changed before deletion"), closeLease())
	}
	removeErr := inflight.removeFileMatchingLease(record.name, lease)
	closeErr := lease.Close()
	if removeErr != nil || closeErr != nil {
		return false, errors.Join(removeErr, closeErr)
	}
	return true, nil
}

func missingClaimQueueDisposition(root *storageRoot, generation string, record spoolRecord) (restored bool, err error) {
	queueRoot, err := root.openDir([]string{queueDirectoryName}, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, root.confirmEntryAbsent(queueDirectoryName)
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, queueRoot.Close()) }()

	queue, err := queueRoot.openDir([]string{generation}, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, queueRoot.confirmEntryAbsent(generation)
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, queue.Close()) }()

	data, readErr := queue.readFile(record.name, int64(maximumEventBytes))
	if errors.Is(readErr, fs.ErrNotExist) {
		return false, queue.confirmEntryAbsent(record.name)
	}
	if readErr != nil {
		return false, readErr
	}
	want, encodeErr := EncodeEvent(record.event)
	if encodeErr != nil {
		return false, encodeErr
	}
	if !bytes.Equal(data, want) {
		return false, errors.New("productmetrics: restored queue event changed before settlement")
	}
	return true, nil
}

// String returns the bounded recording outcome name.
func (result RecordResult) String() string {
	switch result {
	case RecordDropped:
		return "dropped"
	case RecordStored:
		return "stored"
	default:
		return fmt.Sprintf("RecordResult(%d)", result)
	}
}
