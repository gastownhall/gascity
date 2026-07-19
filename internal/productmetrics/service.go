package productmetrics

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/google/uuid"
	"golang.org/x/term"
)

const (
	envDoNotTrack          = "DO_NOT_TRACK"
	envDisableUsageMetrics = "GC_DISABLE_USAGE_METRICS"
	stateLockName          = "state.lock"
	stateLockTimeout       = 12 * time.Second
)

// EffectiveState is the bounded user-visible product-metrics state.
type EffectiveState string

const (
	// StatePendingNotice has no accepted notice or identity.
	StatePendingNotice EffectiveState = "pending-notice"
	// StateNoticeUpdateRequired retains an identity but cannot record until a
	// revised notice is accepted.
	StateNoticeUpdateRequired EffectiveState = "notice-update-required"
	// StateEnabled may issue recording permits.
	StateEnabled EffectiveState = "enabled"
	// StateDisabled is the persisted clean opt-out state.
	StateDisabled EffectiveState = "disabled"
	// StateDisabledCleanupPending is durably opted out but still owns cleanup.
	StateDisabledCleanupPending EffectiveState = "disabled-cleanup-pending"
	// StateEnvironmentDisabled is disabled for this process by environment.
	StateEnvironmentDisabled EffectiveState = "environment-disabled"
	// StateFailClosed cannot collect because a prerequisite is untrusted,
	// unsupported, unavailable, or invalid.
	StateFailClosed EffectiveState = "fail-closed"
	// StateServerPaused is covered by a signed pause epoch or awaits a
	// greater-epoch resume transaction.
	StateServerPaused EffectiveState = "server-paused"
)

// StateReason is a bounded explanation for an EffectiveState.
type StateReason string

// Closed StateReason values keep status and errors free of arbitrary content.
const (
	ReasonPreferenceUnset           StateReason = "preference-unset"
	ReasonEnabled                   StateReason = "enabled"
	ReasonPersistedDisabled         StateReason = "persisted-disabled"
	ReasonDisableCleanupPending     StateReason = "disable-cleanup-pending"
	ReasonPauseCleanupPending       StateReason = "pause-cleanup-pending"
	ReasonNoticeVersionStale        StateReason = "notice-version-stale"
	ReasonServerPauseCoversEpoch    StateReason = "server-pause-covers-epoch"
	ReasonGreaterEpochResumeNeeded  StateReason = "greater-epoch-resume-required"
	ReasonDoNotTrack                StateReason = "do-not-track"
	ReasonGCDisable                 StateReason = "gc-disable-usage-metrics"
	ReasonDevelopmentBuild          StateReason = "development-build"
	ReasonUnsupportedPlatform       StateReason = "unsupported-platform"
	ReasonEndpointMissing           StateReason = "endpoint-missing"
	ReasonRolloutDisabled           StateReason = "rollout-default-off"
	ReasonNoticeUnavailable         StateReason = "notice-unavailable"
	ReasonHomeUnstable              StateReason = "home-unstable"
	ReasonConfigUnreadable          StateReason = "config-unreadable"
	ReasonConfigInvalid             StateReason = "config-invalid"
	ReasonStateSchemaNewer          StateReason = "state-schema-newer"
	ReasonNoticeFloorNewer          StateReason = "notice-floor-newer"
	ReasonCounterNamespaceExhausted StateReason = "counter-namespace-exhausted"
)

// ProductionOptions contains only runtime-unoverrideable release identity and
// a provenance-bearing Gas City home. It deliberately has no endpoint,
// notice, transport, clock, or entropy injection surface.
type ProductionOptions struct {
	Home    gchome.ResolvedHome
	Release ReleaseIdentity
}

// InvocationContext is the immutable product-metrics classification captured
// for one gc invocation. False is the conservative default for eligibility.
type InvocationContext struct {
	DoNotTrack          string
	DisableUsageMetrics string
	ManagedAutomation   bool
	NoticeEligible      bool
	Recordable          bool
	OccurredHourUTC     string
}

// RecordingPermit is an immutable snapshot of the state that authorized one
// invocation. Its private fields prevent construction outside this package.
type RecordingPermit struct {
	valid            bool
	recordLease      *storageRecordLease
	counterNamespace uint64
	stateGeneration  uint64
	installationID   string
	spoolGeneration  string
	releaseVersion   string
	metricsEpoch     uint64
	requiredNotice   uint64
	acceptedNotice   uint64
	operatingSystem  OperatingSystem
	occurredHourUTC  string
}

// Valid reports whether the immutable snapshot was recording-eligible when
// captured. RecordOnce must still compare every private field under the lock.
func (permit RecordingPermit) Valid() bool {
	return permit.valid && permit.recordLease != nil && permit.recordLease.Valid()
}

// Close releases the retained exact-config-record lease. Callers must defer
// Close after capturing a permit; it is idempotent across copied values.
func (permit RecordingPermit) Close() error {
	if permit.recordLease == nil {
		return nil
	}
	return permit.recordLease.Close()
}

// Status is a bounded, redacted, read-only projection of local consent state.
type Status struct {
	State                      EffectiveState
	Reason                     StateReason
	HomeStable                 bool
	HomeReason                 StateReason
	ConfigPath                 string
	ConfigPresent              bool
	StateSchema                uint64
	RequiredNoticeVersion      uint64
	AcceptedNoticeVersion      uint64
	InstallationIDPresent      bool
	SpoolGenerationPresent     bool
	CleanupPending             bool
	QueueEvents                uint64
	QueueBytes                 uint64
	QueueDiagnosticsAvailable  bool
	OldestQueuedEventAge       time.Duration
	OldestQueuedEventPresent   bool
	DroppedEvents              uint64
	LastUploadAttemptHourUTC   string
	LastUploadSuccessHourUTC   string
	LastErrorClass             DiagnosticErrorClass
	StatusDiagnosticsAvailable bool
	SpawnThrottleAge           time.Duration
	SpawnThrottlePresent       bool
}

// ErrStateChangedConcurrently identifies a lost state-generation or cleanup
// ownership comparison.
var ErrStateChangedConcurrently = errors.New("productmetrics: state changed concurrently")

var errStateAppliedSyncPending = errors.New("productmetrics: state transition applied but directory sync is pending")

type serviceRelease struct {
	platformSupported  bool
	official           bool
	endpointConfigured bool
	endpointHostname   string
	privacyURL         string
	rollout            RolloutMode
	releaseVersion     string
	metricsEpoch       uint64
}

// serviceDependencies is package-private by design. Unit tests can exercise a
// marked synthetic release; normal binaries can only call OpenProduction.
type serviceDependencies struct {
	home                        gchome.ProductUsageHome
	homeErr                     error
	homeReason                  StateReason
	release                     serviceRelease
	notice                      noticeDefinition
	getenv                      func(string) string
	newUUID                     func() (string, error)
	now                         func() time.Time
	beforeRecordOperation       func(recordOperation)
	verifyTTY                   func(io.Writer) bool
	storageHooks                storageTestHooks
	disableUploaderWait         time.Duration
	disableStateWait            time.Duration
	disableCleanupBudget        spoolWorkBudget
	beforeDisableUploaderLock   func()
	controlCloseError           func(controlCloseTarget) error
	spawn                       spawnDependencies
	privateUploaderStart        uploadStartFunc
	privateUploaderStartFactory func() (uploadStartFunc, error)
}

// Service owns the lazy consent and identity state machine.
type Service struct {
	deps          serviceDependencies
	recordAttempt atomic.Bool
}

type loadedState struct {
	state   persistedState
	raw     []byte
	lease   *storageRecordLease
	present bool
	err     error
	reason  StateReason
}

func (loaded *loadedState) Close() error {
	if loaded == nil || loaded.lease == nil {
		return nil
	}
	lease := loaded.lease
	loaded.lease = nil
	return lease.Close()
}

func (loaded *loadedState) takeLease() *storageRecordLease {
	if loaded == nil {
		return nil
	}
	lease := loaded.lease
	loaded.lease = nil
	return lease
}

type stateProjection struct {
	state  EffectiveState
	reason StateReason
}

type stateVersion struct {
	counterNamespace uint64
	stateGeneration  uint64
	recordLease      *storageRecordLease
}

// lockedState is a capability proving that state.lock is held for root. Code
// that already owns uploader.lock acquires this capability second and passes
// it to caller-held state/spool helpers; those helpers must never reacquire
// state.lock themselves.
type lockedState struct {
	root   *storageRoot
	lock   *advisoryLock
	closed atomic.Bool
}

func (locked *lockedState) Close() error {
	if locked == nil || !locked.closed.CompareAndSwap(false, true) {
		return nil
	}
	if locked.lock == nil {
		return nil
	}
	return locked.lock.Release()
}

func (locked *lockedState) valid() bool {
	return locked != nil && locked.root != nil && locked.lock != nil && !locked.closed.Load()
}

func stateVersionFrom(state persistedState) stateVersion {
	return stateVersion{counterNamespace: state.CounterNamespace, stateGeneration: state.StateGeneration}
}

func stateVersionFromLoaded(loaded loadedState) stateVersion {
	return stateVersion{
		counterNamespace: loaded.state.CounterNamespace,
		stateGeneration:  loaded.state.StateGeneration,
		recordLease:      loaded.lease,
	}
}

func (version stateVersion) Close() error {
	if version.recordLease == nil {
		return nil
	}
	return version.recordLease.Close()
}

type stateMutationOptions struct {
	allowAppliedActivation bool
	recoverInvalid         bool
	noticeFloor            uint64
}

type cleanupToken struct {
	recordLease      *storageRecordLease
	counterNamespace uint64
	stateGeneration  uint64
	cleanupEpoch     uint64
	kind             cleanupKind
	barrier          persistedState
}

func (token cleanupToken) Close() error {
	if token.recordLease == nil {
		return nil
	}
	return token.recordLease.Close()
}

func cleanupTokenFromLoaded(loaded *loadedState) cleanupToken {
	if loaded == nil {
		return cleanupToken{}
	}
	return cleanupToken{
		recordLease:      loaded.takeLease(),
		counterNamespace: loaded.state.CounterNamespace,
		stateGeneration:  loaded.state.StateGeneration,
		cleanupEpoch:     loaded.state.CleanupEpoch,
		kind:             loaded.state.CleanupKind,
		barrier:          loaded.state,
	}
}

// prepareCleanupLocked establishes the durability barrier required before a
// cleanup owner may delete local data. A waiter can open the root before a
// peer installs an applied-but-unsynced state record, so mutable-root open
// recovery alone is insufficient: sync and exact-token revalidation must
// happen after uploader.lock then state.lock are held.
func (service *Service) prepareCleanupLocked(locked *lockedState, token cleanupToken) error {
	if service == nil || !locked.valid() || token.recordLease == nil {
		return errors.New("productmetrics: invalid cleanup authority")
	}
	if err := service.revalidateCleanupTokenLocked(locked, token); err != nil {
		return err
	}
	if err := locked.root.syncDirectory(); err != nil {
		return fmt.Errorf("productmetrics: sync cleanup barrier: %w", err)
	}
	return service.revalidateCleanupTokenLocked(locked, token)
}

func (service *Service) revalidateCleanupTokenLocked(locked *lockedState, token cleanupToken) error {
	if service == nil || !locked.valid() || token.recordLease == nil {
		return ErrStateChangedConcurrently
	}
	loaded := loadStateFromDirectory(locked.root)
	defer func() { _ = loaded.Close() }()
	if loaded.err != nil || !loaded.present || loaded.lease == nil ||
		!token.recordLease.Matches(loaded.lease) ||
		loaded.state.CounterNamespace != token.counterNamespace ||
		loaded.state.StateGeneration != token.stateGeneration ||
		loaded.state.CleanupEpoch != token.cleanupEpoch || loaded.state.CleanupKind != token.kind ||
		loaded.state != token.barrier {
		return ErrStateChangedConcurrently
	}
	return nil
}

// OpenProduction validates and snapshots side-effect-free dependencies. It
// never creates the metrics root, opens a mutable file, repairs state, or
// starts a process.
func OpenProduction(options ProductionOptions) (*Service, error) {
	resolved := options.Home
	if resolved.Path() == "" {
		resolved = gchome.ResolveReadOnly()
	}
	home, homeErr := gchome.InspectProductUsageHome(resolved)
	deps := serviceDependencies{
		home:       home,
		homeErr:    homeErr,
		homeReason: ReasonHomeUnstable,
		release:    productionServiceRelease(options.Release),
		getenv:     os.Getenv,
		newUUID: func() (string, error) {
			return randomUUIDv4(rand.Reader)
		},
		now:       time.Now,
		verifyTTY: productionNoticeWriterIsTTY,
		spawn: spawnDependencies{
			executable: os.Executable,
			environ:    os.Environ,
			start:      platformStartPrivateUploader,
		},
		privateUploaderStartFactory: productionUploaderStartFactory,
	}
	return openWithDependencies(deps)
}

func openWithDependencies(deps serviceDependencies) (*Service, error) {
	if deps.getenv == nil {
		return nil, errors.New("productmetrics: getenv dependency is nil")
	}
	if deps.newUUID == nil {
		return nil, errors.New("productmetrics: UUID dependency is nil")
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.verifyTTY == nil {
		return nil, errors.New("productmetrics: TTY verifier dependency is nil")
	}
	if len(deps.notice.text) != 0 && !deps.notice.testOnly {
		return nil, errors.New("productmetrics: unapproved production notice material is forbidden")
	}
	if deps.notice.testOnly {
		if deps.notice.version == 0 || len(deps.notice.text) == 0 {
			return nil, errors.New("productmetrics: incomplete test-only notice dependency")
		}
	}
	if deps.homeErr != nil && deps.homeReason == "" {
		deps.homeReason = ReasonHomeUnstable
	}
	return &Service{deps: deps}, nil
}

func productionNoticeWriterIsTTY(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func productionServiceRelease(identity ReleaseIdentity) serviceRelease {
	return serviceRelease{
		platformSupported:  runtime.GOOS == "linux" || runtime.GOOS == "darwin",
		official:           identity.BuildKind() != BuildDevelopment && identity.BuildKind().String() != "unknown",
		endpointConfigured: identity.Endpoint() != "",
		endpointHostname:   endpointHostnameForPolicy(identity.Endpoint()),
		privacyURL:         identity.PrivacyURL(),
		rollout:            identity.Rollout(),
		releaseVersion:     identity.ReleaseVersion(),
		metricsEpoch:       identity.MetricsEpoch(),
	}
}

func randomUUIDv4(reader io.Reader) (string, error) {
	value, err := uuid.NewRandomFromReader(reader)
	if err != nil {
		return "", fmt.Errorf("productmetrics: generate random UUID: %w", err)
	}
	return value.String(), nil
}

// Status returns a pure projection over a no-create read-only view. The
// context is accepted for API consistency; no lock, retry, or repair occurs.
func (service *Service) Status(_ context.Context) Status {
	loaded := service.readStateReadOnly()
	defer func() { _ = loaded.Close() }()
	diagnostics := service.readDiagnosticsReadOnly()
	invocation := InvocationContext{
		DoNotTrack:          service.deps.getenv(envDoNotTrack),
		DisableUsageMetrics: service.deps.getenv(envDisableUsageMetrics),
	}
	projection := service.project(invocation, loaded)
	status := Status{
		State:                      projection.state,
		Reason:                     projection.reason,
		HomeStable:                 service.deps.homeErr == nil,
		ConfigPath:                 service.configPath(),
		ConfigPresent:              loaded.present,
		QueueEvents:                diagnostics.queueEvents,
		QueueBytes:                 diagnostics.queueBytes,
		QueueDiagnosticsAvailable:  diagnostics.queueAvailable,
		OldestQueuedEventAge:       nonnegativeAge(service.deps.now(), diagnostics.oldestQueuedAt),
		OldestQueuedEventPresent:   diagnostics.oldestQueuedPresent,
		DroppedEvents:              diagnostics.status.droppedEvents,
		LastUploadAttemptHourUTC:   diagnostics.status.lastUploadAttemptHourUTC,
		LastUploadSuccessHourUTC:   diagnostics.status.lastUploadSuccessHourUTC,
		LastErrorClass:             diagnostics.status.lastErrorClass,
		StatusDiagnosticsAvailable: diagnostics.statusAvailable,
		SpawnThrottleAge:           nonnegativeAge(service.deps.now(), diagnostics.spawnThrottleAttemptedAt),
		SpawnThrottlePresent:       diagnostics.spawnThrottlePresent,
	}
	if service.deps.homeErr != nil {
		status.HomeReason = service.deps.homeReason
	}
	if loaded.err == nil && loaded.present {
		state := loaded.state
		status.StateSchema = state.StateSchema
		status.RequiredNoticeVersion = state.RequiredNoticeVersion
		status.AcceptedNoticeVersion = state.AcceptedNoticeVersion
		status.InstallationIDPresent = state.InstallationID != ""
		status.SpoolGenerationPresent = state.SpoolGeneration != ""
		status.CleanupPending = state.CleanupKind != cleanupNone
	}
	return status
}

// RecordingPermit captures a sticky eligibility snapshot. Pending and
// transition-performing invocations always receive the zero permit.
func (service *Service) RecordingPermit(invocation InvocationContext) RecordingPermit {
	if !invocation.Recordable || invocation.ManagedAutomation ||
		doNotTrackTruthy(invocation.DoNotTrack) || gcDisableTruthy(invocation.DisableUsageMetrics) {
		return RecordingPermit{}
	}
	loaded := service.readStateReadOnly()
	defer func() { _ = loaded.Close() }()
	projection := service.project(invocation, loaded)
	if loaded.err != nil || !loaded.present {
		return RecordingPermit{}
	}
	state := loaded.state
	occurredHour := invocation.OccurredHourUTC
	if occurredHour == "" {
		occurredHour = depsHourUTC(service.deps.now())
	}
	if _, err := parseCanonicalHourUTC(occurredHour); err != nil {
		return RecordingPermit{}
	}
	if projection.state == StateNoticeUpdateRequired {
		ctx, cancel := context.WithTimeout(context.Background(), stateLockTimeout)
		defer cancel()
		_ = service.invalidateNotice(ctx, stateVersionFromLoaded(loaded))
		return RecordingPermit{}
	}
	if projection.state == StateServerPaused &&
		(projection.reason == ReasonPauseCleanupPending || projection.reason == ReasonGreaterEpochResumeNeeded) &&
		service.deps.release.metricsEpoch > state.PausedThroughMetricsEpoch {
		ctx, cancel := context.WithTimeout(context.Background(), stateLockTimeout)
		defer cancel()
		_, _ = service.finishPauseCleanupAndResume(ctx)
		return RecordingPermit{}
	}
	if projection.state != StateEnabled {
		return RecordingPermit{}
	}
	return RecordingPermit{
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
		occurredHourUTC:  occurredHour,
	}
}

func (service *Service) project(invocation InvocationContext, loaded loadedState) stateProjection {
	if !service.deps.release.platformSupported {
		return stateProjection{StateFailClosed, ReasonUnsupportedPlatform}
	}
	if !service.deps.release.official {
		return stateProjection{StateFailClosed, ReasonDevelopmentBuild}
	}
	if !service.deps.release.endpointConfigured {
		return stateProjection{StateFailClosed, ReasonEndpointMissing}
	}
	if service.deps.release.rollout == RolloutDefaultOff {
		return stateProjection{StateFailClosed, ReasonRolloutDisabled}
	}
	if service.deps.release.rollout != RolloutCanary && service.deps.release.rollout != RolloutDefaultOn {
		return stateProjection{StateFailClosed, ReasonRolloutDisabled}
	}
	if !service.deps.notice.testOnly || service.deps.notice.version == 0 || len(service.deps.notice.text) == 0 {
		return stateProjection{StateFailClosed, ReasonNoticeUnavailable}
	}
	if service.deps.homeErr != nil {
		return stateProjection{StateFailClosed, service.deps.homeReason}
	}
	if loaded.err != nil {
		return stateProjection{StateFailClosed, loaded.reason}
	}
	if doNotTrackTruthy(invocation.DoNotTrack) {
		return stateProjection{StateEnvironmentDisabled, ReasonDoNotTrack}
	}
	if gcDisableTruthy(invocation.DisableUsageMetrics) {
		return stateProjection{StateEnvironmentDisabled, ReasonGCDisable}
	}
	if !loaded.present {
		return stateProjection{StatePendingNotice, ReasonPreferenceUnset}
	}
	state := loaded.state
	if state.CounterNamespace == terminalCounterNamespace && state.Preference != preferenceDisabled {
		return stateProjection{StateFailClosed, ReasonCounterNamespaceExhausted}
	}
	if state.RequiredNoticeVersion > service.deps.notice.version {
		return stateProjection{StateFailClosed, ReasonNoticeFloorNewer}
	}
	switch state.Preference {
	case preferenceDisabled:
		if state.CleanupKind == cleanupDisable {
			return stateProjection{StateDisabledCleanupPending, ReasonDisableCleanupPending}
		}
		return stateProjection{StateDisabled, ReasonPersistedDisabled}
	case preferenceUnset:
		return stateProjection{StatePendingNotice, ReasonPreferenceUnset}
	case preferenceEnabled:
		if state.AcceptedNoticeVersion < state.RequiredNoticeVersion || state.AcceptedNoticeVersion < service.deps.notice.version {
			return stateProjection{StateNoticeUpdateRequired, ReasonNoticeVersionStale}
		}
		if state.CleanupKind == cleanupPause {
			return stateProjection{StateServerPaused, ReasonPauseCleanupPending}
		}
		if state.PausedThroughMetricsEpoch >= service.deps.release.metricsEpoch {
			return stateProjection{StateServerPaused, ReasonServerPauseCoversEpoch}
		}
		if state.PausedThroughMetricsEpoch > 0 && state.SpoolGeneration == "" {
			return stateProjection{StateServerPaused, ReasonGreaterEpochResumeNeeded}
		}
		if state.SpoolGeneration == "" {
			return stateProjection{StateFailClosed, ReasonConfigInvalid}
		}
		return stateProjection{StateEnabled, ReasonEnabled}
	default:
		return stateProjection{StateFailClosed, ReasonConfigInvalid}
	}
}

func (service *Service) readStateReadOnly() loadedState {
	return service.readStateReadOnlyWithHooks(storageTestHooks{})
}

func (service *Service) readStateReadOnlyWithHooks(hooks storageTestHooks) loadedState {
	if service.deps.homeErr != nil {
		return loadedState{err: service.deps.homeErr, reason: service.deps.homeReason}
	}
	root, err := openStorageRootReadOnlyWithHooks(service.deps.home, hooks)
	if errors.Is(err, fs.ErrNotExist) {
		return loadedState{}
	}
	if err != nil {
		return loadedState{err: err, reason: ReasonConfigUnreadable}
	}
	loaded := loadStateFromDirectory(root)
	if closeErr := root.Close(); closeErr != nil && loaded.err == nil {
		loaded.err = closeErr
		loaded.reason = ReasonConfigUnreadable
	}
	return loaded
}

func loadStateFromDirectory(root *storageRoot) loadedState {
	data, lease, err := root.readFileLease(configFileName, maximumConfigBytes)
	if errors.Is(err, fs.ErrNotExist) {
		_ = lease.Close()
		return loadedState{}
	}
	if err != nil {
		return loadedState{lease: lease, present: true, err: err, reason: ReasonConfigUnreadable}
	}
	loaded := loadedState{raw: append([]byte(nil), data...), lease: lease, present: true}
	state, err := decodePersistedState(data)
	if err != nil {
		loaded.err = err
		if errors.Is(err, errStateSchemaNewer) {
			loaded.reason = ReasonStateSchemaNewer
		} else {
			loaded.reason = ReasonConfigInvalid
		}
		return loaded
	}
	loaded.state = state
	return loaded
}

func (service *Service) configPath() string {
	root := service.deps.home.Root()
	if root == "" {
		root = filepath.Join(service.deps.home.Home().Path(), "product-usage")
	}
	return filepath.Join(root, configFileName)
}

func gcDisableTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func doNotTrackTruthy(value string) bool {
	if value == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func (service *Service) invalidateNotice(ctx context.Context, expected stateVersion) error {
	result, err := service.mutateState(ctx, expected, stateMutationOptions{noticeFloor: service.deps.notice.version}, func(state *persistedState) error {
		if state.Preference != preferenceEnabled {
			return ErrStateChangedConcurrently
		}
		if state.RequiredNoticeVersion > service.deps.notice.version {
			return errors.New("productmetrics: persisted notice floor is newer than this binary")
		}
		if state.RequiredNoticeVersion == service.deps.notice.version {
			// A peer may have already installed this floor and then reaccepted it.
			// The durable floor, not this stale observer, owns that newer record.
			return nil
		}
		state.RequiredNoticeVersion = service.deps.notice.version
		state.SpoolGeneration = ""
		if mutationCounterRecoveryRequired(*state) {
			if err := advanceCounterNamespace(state); err != nil {
				return err
			}
			state.StateGeneration = 1
			if state.CleanupKind == cleanupNone {
				state.CleanupEpoch = 0
			} else {
				state.CleanupEpoch = 1
			}
			return nil
		}
		return incrementStateGeneration(state)
	})
	return errors.Join(err, result.Close())
}

func (service *Service) resumeGreaterEpochLocked(locked *lockedState, expected stateVersion) error {
	result, err := service.mutateStateLocked(locked, expected, stateMutationOptions{allowAppliedActivation: true}, service.resumeGreaterEpochMutation())
	return errors.Join(err, result.Close())
}

func (service *Service) resumeGreaterEpochMutation() func(*persistedState) error {
	return func(state *persistedState) error {
		if state.Preference != preferenceEnabled || state.CleanupKind != cleanupNone ||
			state.PausedThroughMetricsEpoch == 0 || service.deps.release.metricsEpoch <= state.PausedThroughMetricsEpoch ||
			state.RequiredNoticeVersion != service.deps.notice.version ||
			state.AcceptedNoticeVersion != service.deps.notice.version || state.InstallationID == "" || state.SpoolGeneration != "" {
			return ErrStateChangedConcurrently
		}
		spool, err := service.deps.newUUID()
		if err != nil {
			return err
		}
		if err := validateCanonicalUUIDv4(spool); err != nil {
			return fmt.Errorf("productmetrics: generated spool UUID: %w", err)
		}
		state.SpoolGeneration = spool
		return incrementStateGeneration(state)
	}
}

func (service *Service) beginDisable(ctx context.Context, expected stateVersion) (cleanupToken, error) {
	if service == nil {
		return cleanupToken{}, errors.New("productmetrics: service is nil")
	}
	if service.deps.homeErr != nil {
		return cleanupToken{}, service.deps.homeErr
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, service.deps.storageHooks)
	if err != nil {
		return cleanupToken{}, err
	}
	token, disableErr := service.beginDisableAtRoot(ctx, expected, root)
	if closeErr := root.Close(); closeErr != nil {
		disableErr = errors.Join(disableErr, closeErr, token.Close())
		token = cleanupToken{}
	}
	return token, disableErr
}

// beginDisableAtRoot keeps the exact metrics-root descriptor alive from the
// durable disable transition through the caller's uploader barrier. A lexical
// root replacement can therefore never redirect quiescence or cleanup to a
// different lock domain.
func (service *Service) beginDisableAtRoot(ctx context.Context, expected stateVersion, root *storageRoot) (cleanupToken, error) {
	bound, closeBound, err := service.bindDisableExpectation(expected)
	if err != nil {
		return cleanupToken{}, err
	}
	defer closeBound()
	locked, err := service.lockState(ctx, root)
	if err != nil {
		return cleanupToken{}, err
	}
	result, mutationErr := service.mutateStateLocked(locked, bound, stateMutationOptions{recoverInvalid: true}, service.beginDisableMutation())
	closeErr := locked.Close()
	if mutationErr != nil || closeErr != nil {
		return cleanupToken{}, errors.Join(mutationErr, closeErr, result.Close())
	}
	defer func() { _ = result.Close() }()
	return cleanupTokenFromLoaded(&result), nil
}

func (service *Service) beginDisableMutation() func(*persistedState) error {
	return func(state *persistedState) error {
		if state.Preference == preferenceDisabled && state.CleanupKind == cleanupDisable {
			return nil
		}
		if mutationCounterRecoveryRequired(*state) {
			// The next ordinary increment would enter the reserved terminal
			// value. Opt-out must remain available, so use the same fresh,
			// identity-free cleanup namespace as corrupt-state recovery. The
			// cleared ID and spool keep every pre-recovery permit invalid even
			// when its numeric generation happens to equal the fresh counter.
			requiredNotice := state.RequiredNoticeVersion
			if requiredNotice < service.deps.notice.version {
				requiredNotice = service.deps.notice.version
			}
			acceptedNotice := state.AcceptedNoticeVersion
			if acceptedNotice > requiredNotice {
				acceptedNotice = requiredNotice
			}
			if state.CounterNamespace < terminalCounterNamespace {
				state.CounterNamespace++
			}
			counterNamespace := state.CounterNamespace
			*state = persistedState{
				StateSchema:           currentStateSchema,
				CounterNamespace:      counterNamespace,
				StateGeneration:       1,
				Preference:            preferenceDisabled,
				RequiredNoticeVersion: requiredNotice,
				AcceptedNoticeVersion: acceptedNotice,
				CleanupKind:           cleanupDisable,
				CleanupEpoch:          1,
			}
			return nil
		}
		state.Preference = preferenceDisabled
		state.InstallationID = ""
		state.SpoolGeneration = ""
		state.PausedThroughMetricsEpoch = 0
		state.CleanupKind = cleanupDisable
		if state.RequiredNoticeVersion < service.deps.notice.version {
			state.RequiredNoticeVersion = service.deps.notice.version
		}
		if state.AcceptedNoticeVersion > state.RequiredNoticeVersion {
			state.AcceptedNoticeVersion = state.RequiredNoticeVersion
		}
		if err := incrementCleanupEpoch(state); err != nil {
			return err
		}
		if err := incrementStateGeneration(state); err != nil {
			return err
		}
		return nil
	}
}

// bindDisableExpectation turns the disable call's numeric observation into an
// exact-record lease before it waits for state.lock. A replacement in that
// interval then loses the incarnation comparison under the lock. Invalid but
// safely readable records are bound the same way, so recovery cannot recreate
// authority from untrusted numeric fields.
func (service *Service) bindDisableExpectation(expected stateVersion) (stateVersion, func(), error) {
	if expected.recordLease != nil {
		return expected, func() {}, nil
	}
	loaded := service.readStateReadOnly()
	if !loaded.present {
		_ = loaded.Close()
		if loaded.err != nil {
			return stateVersion{}, func() {}, loaded.err
		}
		if expected.counterNamespace != 0 || expected.stateGeneration != 0 {
			return stateVersion{}, func() {}, ErrStateChangedConcurrently
		}
		return expected, func() {}, nil
	}
	if loaded.lease == nil {
		err := loaded.err
		_ = loaded.Close()
		if err == nil {
			err = errors.New("productmetrics: present config has no exact-record lease")
		}
		return stateVersion{}, func() {}, err
	}
	if loaded.err == nil {
		if loaded.state.CounterNamespace != expected.counterNamespace || loaded.state.StateGeneration != expected.stateGeneration {
			_ = loaded.Close()
			return stateVersion{}, func() {}, ErrStateChangedConcurrently
		}
	} else if expected.counterNamespace != 0 || expected.stateGeneration != 0 {
		_ = loaded.Close()
		return stateVersion{}, func() {}, ErrStateChangedConcurrently
	}
	expected.recordLease = loaded.takeLease()
	_ = loaded.Close()
	return expected, func() { _ = expected.Close() }, nil
}

func (service *Service) applyPause(ctx context.Context, permit RecordingPermit, pausedThrough uint64) (cleanupToken, error) {
	if err := service.validatePauseAuthority(permit, pausedThrough); err != nil {
		return cleanupToken{}, ErrStateChangedConcurrently
	}
	result, err := service.mutateState(ctx, stateVersion{
		counterNamespace: permit.counterNamespace,
		stateGeneration:  permit.stateGeneration,
		recordLease:      permit.recordLease,
	}, stateMutationOptions{}, service.pauseMutation(permit, pausedThrough))
	if err != nil {
		_ = result.Close()
		return cleanupToken{}, err
	}
	defer func() { _ = result.Close() }()
	return cleanupTokenFromLoaded(&result), nil
}

func (service *Service) applyPauseLocked(locked *lockedState, permit RecordingPermit, pausedThrough uint64) (cleanupToken, error) {
	if err := service.validatePauseAuthority(permit, pausedThrough); err != nil {
		return cleanupToken{}, err
	}
	result, err := service.mutateStateLocked(locked, stateVersion{
		counterNamespace: permit.counterNamespace,
		stateGeneration:  permit.stateGeneration,
		recordLease:      permit.recordLease,
	}, stateMutationOptions{}, service.pauseMutation(permit, pausedThrough))
	if err != nil {
		_ = result.Close()
		return cleanupToken{}, err
	}
	defer func() { _ = result.Close() }()
	return cleanupTokenFromLoaded(&result), nil
}

func (service *Service) validatePauseAuthority(permit RecordingPermit, pausedThrough uint64) error {
	if service == nil || !permit.Valid() || pausedThrough < permit.metricsEpoch ||
		permit.releaseVersion != service.deps.release.releaseVersion || permit.metricsEpoch != service.deps.release.metricsEpoch {
		return ErrStateChangedConcurrently
	}
	return nil
}

func (service *Service) pauseMutation(permit RecordingPermit, pausedThrough uint64) func(*persistedState) error {
	return func(state *persistedState) error {
		if !stateMatchesPermit(*state, permit) {
			return ErrStateChangedConcurrently
		}
		if mutationCounterRecoveryRequired(*state) {
			if pausedThrough < state.PausedThroughMetricsEpoch {
				pausedThrough = state.PausedThroughMetricsEpoch
			}
			if state.CounterNamespace < terminalCounterNamespace {
				state.CounterNamespace++
			}
			counterNamespace := state.CounterNamespace
			*state = persistedState{
				StateSchema:               currentStateSchema,
				CounterNamespace:          counterNamespace,
				StateGeneration:           1,
				Preference:                preferenceEnabled,
				RequiredNoticeVersion:     state.RequiredNoticeVersion,
				AcceptedNoticeVersion:     state.AcceptedNoticeVersion,
				InstallationID:            state.InstallationID,
				CleanupKind:               cleanupPause,
				CleanupEpoch:              1,
				PausedThroughMetricsEpoch: pausedThrough,
			}
			return nil
		}
		if pausedThrough > state.PausedThroughMetricsEpoch {
			state.PausedThroughMetricsEpoch = pausedThrough
		}
		state.SpoolGeneration = ""
		state.CleanupKind = cleanupPause
		if err := incrementCleanupEpoch(state); err != nil {
			return err
		}
		if err := incrementStateGeneration(state); err != nil {
			return err
		}
		return nil
	}
}

func (service *Service) completeCleanup(ctx context.Context, token cleanupToken) error {
	result, err := service.mutateState(ctx, stateVersion{
		counterNamespace: token.counterNamespace,
		stateGeneration:  token.stateGeneration,
		recordLease:      token.recordLease,
	}, stateMutationOptions{}, completeCleanupMutation(token))
	return errors.Join(err, result.Close())
}

func (service *Service) completeCleanupLocked(locked *lockedState, token cleanupToken) error {
	result, err := service.mutateStateLocked(locked, stateVersion{
		counterNamespace: token.counterNamespace,
		stateGeneration:  token.stateGeneration,
		recordLease:      token.recordLease,
	}, stateMutationOptions{}, completeCleanupMutation(token))
	return errors.Join(err, result.Close())
}

func (service *Service) completeCleanupLockedWithJournalProof(locked *lockedState, token cleanupToken, meter *spoolWorkMeter) error {
	if service == nil || !locked.valid() || meter == nil {
		return errStorageClosed
	}
	if !meter.chargeFixedDirectory() {
		return errors.New("productmetrics: cleanup budget cannot persist final state")
	}
	restore := locked.root.installDirectoryOpenHooks(meter.beforePhysicalDirectoryOpen, meter.afterPhysicalDirectoryOpen)
	completeErr := service.completeCleanupLocked(locked, token)
	restore()
	if completeErr != nil {
		return completeErr
	}
	return proveRootTempJournalReadOnlyWithMeter(locked.root, meter, true)
}

func completeCleanupMutation(token cleanupToken) func(*persistedState) error {
	return func(state *persistedState) error {
		if *state != token.barrier || state.CleanupKind != token.kind || state.CleanupEpoch != token.cleanupEpoch {
			return ErrStateChangedConcurrently
		}
		*state = cleanupSuccessorState(*state)
		return nil
	}
}

func cleanupSuccessorState(state persistedState) persistedState {
	if mutationCounterRecoveryRequired(state) {
		if state.CounterNamespace < terminalCounterNamespace {
			state.CounterNamespace++
		}
		state.StateGeneration = 1
		state.CleanupEpoch = 1
		state.CleanupKind = cleanupNone
		return state
	}
	state.CleanupKind = cleanupNone
	state.StateGeneration++
	return state
}

func stateMatchesPermit(state persistedState, permit RecordingPermit) bool {
	return permit.valid && state.CounterNamespace == permit.counterNamespace && state.StateGeneration == permit.stateGeneration &&
		state.Preference == preferenceEnabled && state.CleanupKind == cleanupNone &&
		state.InstallationID == permit.installationID && state.SpoolGeneration == permit.spoolGeneration &&
		state.RequiredNoticeVersion == permit.requiredNotice && state.AcceptedNoticeVersion == permit.acceptedNotice
}

func mutationCounterRecoveryRequired(state persistedState) bool {
	return state.StateGeneration >= maximumStateCounter-1 || state.CleanupEpoch >= maximumStateCounter-1
}

func (service *Service) lockState(ctx context.Context, root *storageRoot) (*lockedState, error) {
	if service == nil {
		return nil, errors.New("productmetrics: service is nil")
	}
	if ctx == nil {
		return nil, errors.New("productmetrics: state-lock context is nil")
	}
	if root == nil {
		return nil, errStorageClosed
	}
	lock, err := root.acquireLock(ctx, stateLockName)
	if err != nil {
		return nil, err
	}
	return &lockedState{root: root, lock: lock}, nil
}

func (service *Service) revalidatePermitLocked(locked *lockedState, permit RecordingPermit) error {
	if service == nil || !locked.valid() || !permit.Valid() ||
		permit.releaseVersion != service.deps.release.releaseVersion ||
		permit.metricsEpoch != service.deps.release.metricsEpoch ||
		permit.operatingSystem != operatingSystemForRuntime() {
		return ErrStateChangedConcurrently
	}
	loaded := loadStateFromDirectory(locked.root)
	defer func() { _ = loaded.Close() }()
	if loaded.err != nil || !loaded.present || loaded.lease == nil ||
		!permit.recordLease.Matches(loaded.lease) || !stateMatchesPermit(loaded.state, permit) ||
		service.project(InvocationContext{
			DoNotTrack:          service.deps.getenv(envDoNotTrack),
			DisableUsageMetrics: service.deps.getenv(envDisableUsageMetrics),
		}, loaded).state != StateEnabled {
		return ErrStateChangedConcurrently
	}
	return nil
}

func (service *Service) mutateState(ctx context.Context, expected stateVersion, options stateMutationOptions, mutate func(*persistedState) error) (result loadedState, returnErr error) {
	if ctx == nil {
		return loadedState{}, errors.New("productmetrics: mutation context is nil")
	}
	if service.deps.homeErr != nil {
		return loadedState{}, service.deps.homeErr
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, service.deps.storageHooks)
	if err != nil {
		return loadedState{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	locked, err := service.lockState(ctx, root)
	if err != nil {
		return loadedState{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, locked.Close()) }()
	return service.mutateStateLocked(locked, expected, options, mutate)
}

func (service *Service) mutateStateLocked(locked *lockedState, expected stateVersion, options stateMutationOptions, mutate func(*persistedState) error) (loadedState, error) {
	if service == nil || !locked.valid() {
		return loadedState{}, errors.New("productmetrics: state lock is not held")
	}
	if service.deps.homeErr != nil {
		return loadedState{}, service.deps.homeErr
	}
	if mutate == nil {
		return loadedState{}, errors.New("productmetrics: state mutation is nil")
	}
	loaded := loadStateFromDirectory(locked.root)
	defer func() { _ = loaded.Close() }()
	if loaded.err != nil && !options.recoverInvalid {
		return loadedState{}, loaded.err
	}
	state := persistedState{
		StateSchema:           currentStateSchema,
		CounterNamespace:      initialCounterNamespace,
		Preference:            preferenceUnset,
		RequiredNoticeVersion: service.deps.notice.version,
		CleanupKind:           cleanupNone,
	}
	if loaded.present && loaded.err == nil {
		state = loaded.state
	} else if loaded.present && options.recoverInvalid {
		state.CounterNamespace = recoveryCounterNamespace(loaded.raw)
	}
	if !expectedStateMatchesLoaded(expected, loaded, options.recoverInvalid) &&
		!noticeFloorCanRebase(loaded, options.noticeFloor) {
		return loadedState{}, ErrStateChangedConcurrently
	}
	before := state
	if err := mutate(&state); err != nil {
		return loadedState{}, err
	}
	if state == before {
		loaded.state = state
		loaded.err = nil
		loaded.reason = ""
		return loadedState{state: state, raw: loaded.raw, lease: loaded.takeLease(), present: loaded.present}, nil
	}
	return persistStateMutation(locked.root, state, options.allowAppliedActivation)
}

func expectedStateMatchesLoaded(expected stateVersion, loaded loadedState, recoverInvalid bool) bool {
	if !loaded.present {
		return loaded.err == nil && expected.recordLease == nil && expected.counterNamespace == 0 && expected.stateGeneration == 0
	}
	if expected.recordLease == nil || loaded.lease == nil || !expected.recordLease.Matches(loaded.lease) {
		return false
	}
	if loaded.err != nil {
		return recoverInvalid && expected.counterNamespace == 0 && expected.stateGeneration == 0
	}
	return loaded.state.CounterNamespace == expected.counterNamespace && loaded.state.StateGeneration == expected.stateGeneration
}

func noticeFloorCanRebase(loaded loadedState, noticeFloor uint64) bool {
	return noticeFloor > 0 && loaded.err == nil && loaded.present && loaded.state.Preference == preferenceEnabled &&
		loaded.state.RequiredNoticeVersion <= noticeFloor
}

func persistStateMutation(root *storageRoot, state persistedState, allowAppliedActivation bool) (loadedState, error) {
	data, err := encodePersistedState(state)
	if err != nil {
		return loadedState{}, err
	}
	result, writeErr := root.writeFileAtomicOutcome(configFileName, data)
	switch result.state {
	case storageWriteAppliedDurable:
		if writeErr != nil {
			return loadedState{}, writeErr
		}
	case storageWriteNotApplied:
		if writeErr == nil {
			return loadedState{}, errors.New("productmetrics: storage reported a not-applied write without an error")
		}
		return loadedState{}, writeErr
	case storageWriteAppliedSyncPending:
		// The exact installed record is loaded below before deciding whether an
		// applied activation is a logical success.
	default:
		return loadedState{}, errors.New("productmetrics: storage returned an unknown atomic-write outcome")
	}
	installed := loadStateFromDirectory(root)
	if installed.err != nil || !installed.present || installed.state != state || !bytes.Equal(installed.raw, data) {
		err := errors.Join(installed.err, errors.New("productmetrics: applied state did not read back exactly"))
		_ = installed.Close()
		if result.state == storageWriteAppliedSyncPending {
			err = errors.Join(errStateAppliedSyncPending, writeErr, err)
		}
		return loadedState{}, err
	}
	if result.state == storageWriteAppliedSyncPending {
		if !allowAppliedActivation {
			_ = installed.Close()
			return loadedState{}, errors.Join(errStateAppliedSyncPending, writeErr)
		}
		// The whole new record is the logical activation point. A retry may
		// establish rename durability; failure can only conservatively lose this
		// opt-in after a crash.
		_ = root.syncDirectory()
	}
	return installed, nil
}
