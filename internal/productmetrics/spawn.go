package productmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	privateUploaderSentinel          = "__gc-product-metrics-uploader-v1"
	privateUploaderMarkerEnvironment = "GC_PRODUCT_METRICS_PRIVATE_UPLOADER"
	privateUploaderMarkerValue       = "1"

	spawnThrottleFileName      = "spawn-throttle"
	currentSpawnThrottleSchema = uint64(1)
	maximumSpawnThrottleBytes  = 4 * 1024
	spawnThrottleInterval      = 60 * time.Second
	privateUploaderWorkBudget  = 10 * time.Second
	privateUploaderLockWait    = 100 * time.Millisecond
)

var (
	errDetachedUploaderUnsupported = errors.New("productmetrics: detached uploader is unsupported")
	errStaleSpawnAttempt           = staleSpawnAttemptError{}
)

type staleSpawnAttemptError struct{}

func (staleSpawnAttemptError) Error() string {
	return "productmetrics: stale private uploader attempt"
}

// PrivateUploaderInvocation is a validated private-entry capability. Its
// private token prevents another package from constructing a child invocation
// without passing the exact package-owned argv parser.
type PrivateUploaderInvocation struct {
	attemptToken string
}

// ParsePrivateUploaderInvocation recognizes the package-owned private argv
// prefix without consulting ambient os.Args. detected is true for every argv
// beginning with the sentinel, including malformed shapes, so callers can
// fail closed before constructing the normal CLI.
func ParsePrivateUploaderInvocation(args []string) (invocation PrivateUploaderInvocation, detected bool, err error) {
	if len(args) == 0 || args[0] != privateUploaderSentinel {
		return PrivateUploaderInvocation{}, false, nil
	}
	if len(args) != 2 {
		return PrivateUploaderInvocation{}, true, errors.New("productmetrics: malformed private uploader arguments")
	}
	if err := validateCanonicalUUIDv4(args[1]); err != nil {
		return PrivateUploaderInvocation{}, true, fmt.Errorf("productmetrics: invalid private uploader token: %w", err)
	}
	return PrivateUploaderInvocation{attemptToken: args[1]}, true, nil
}

type spawnThrottleRecord struct {
	attemptToken string
	attemptedAt  time.Time
}

type spawnThrottleWire struct {
	ThrottleSchema uint64 `toml:"throttle_schema"`
	AttemptToken   string `toml:"attempt_token"`
	AttemptedAt    string `toml:"attempted_at"`
}

func encodeSpawnThrottle(record spawnThrottleRecord) ([]byte, error) {
	if err := validateSpawnThrottleRecord(record); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := toml.NewEncoder(&output).Encode(spawnThrottleWire{
		ThrottleSchema: currentSpawnThrottleSchema,
		AttemptToken:   record.attemptToken,
		AttemptedAt:    record.attemptedAt.Format(time.RFC3339Nano),
	}); err != nil {
		return nil, fmt.Errorf("productmetrics: encode spawn throttle: %w", err)
	}
	if output.Len() > maximumSpawnThrottleBytes {
		return nil, fmt.Errorf("productmetrics: encoded spawn throttle exceeds %d bytes", maximumSpawnThrottleBytes)
	}
	return output.Bytes(), nil
}

func decodeSpawnThrottle(data []byte) (spawnThrottleRecord, error) {
	if len(data) == 0 || len(data) > maximumSpawnThrottleBytes {
		return spawnThrottleRecord{}, errors.New("productmetrics: invalid spawn throttle size")
	}
	var wire spawnThrottleWire
	metadata, err := toml.Decode(string(data), &wire)
	if err != nil {
		return spawnThrottleRecord{}, fmt.Errorf("productmetrics: decode spawn throttle TOML: %w", err)
	}
	required := map[string]bool{
		"throttle_schema": false,
		"attempt_token":   false,
		"attempted_at":    false,
	}
	for _, key := range metadata.Keys() {
		parts := []string(key)
		if len(parts) != 1 {
			return spawnThrottleRecord{}, fmt.Errorf("productmetrics: nested spawn throttle key %q is not allowed", key.String())
		}
		if _, ok := required[parts[0]]; !ok {
			return spawnThrottleRecord{}, fmt.Errorf("productmetrics: unknown spawn throttle field %q", parts[0])
		}
		required[parts[0]] = true
	}
	for key, present := range required {
		if !present {
			return spawnThrottleRecord{}, fmt.Errorf("productmetrics: required spawn throttle field %q is absent", key)
		}
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		return spawnThrottleRecord{}, fmt.Errorf("productmetrics: unrecognized spawn throttle field %q", undecoded[0].String())
	}
	if wire.ThrottleSchema != currentSpawnThrottleSchema {
		return spawnThrottleRecord{}, fmt.Errorf(
			"productmetrics: spawn throttle schema is %d, want %d", wire.ThrottleSchema, currentSpawnThrottleSchema,
		)
	}
	attemptedAt, err := time.Parse(time.RFC3339Nano, wire.AttemptedAt)
	if err != nil || attemptedAt.Location() != time.UTC || attemptedAt.Format(time.RFC3339Nano) != wire.AttemptedAt {
		return spawnThrottleRecord{}, errors.New("productmetrics: spawn throttle instant is not canonical UTC")
	}
	record := spawnThrottleRecord{attemptToken: wire.AttemptToken, attemptedAt: attemptedAt}
	if err := validateSpawnThrottleRecord(record); err != nil {
		return spawnThrottleRecord{}, err
	}
	return record, nil
}

func validateSpawnThrottleRecord(record spawnThrottleRecord) error {
	if err := validateCanonicalUUIDv4(record.attemptToken); err != nil {
		return fmt.Errorf("productmetrics: spawn throttle token: %w", err)
	}
	if record.attemptedAt.IsZero() || record.attemptedAt.Location() != time.UTC ||
		record.attemptedAt.Format(time.RFC3339Nano) == "" {
		return errors.New("productmetrics: spawn throttle instant is not canonical UTC")
	}
	return nil
}

type spawnReservation struct {
	attemptToken string
}

func (service *Service) reserveSpawnAttempt(ctx context.Context) (reservation spawnReservation, reserved bool, returnErr error) {
	if service == nil {
		return spawnReservation{}, false, errors.New("productmetrics: service is nil")
	}
	if ctx == nil {
		return spawnReservation{}, false, errors.New("productmetrics: spawn reservation context is nil")
	}
	eligible, err := service.uploadNeedsMutableWork()
	if err != nil {
		return spawnReservation{}, false, err
	}
	if !eligible {
		return spawnReservation{}, false, nil
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, service.deps.storageHooks)
	if err != nil {
		return spawnReservation{}, false, err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	locked, err := service.lockState(ctx, root)
	if err != nil {
		return spawnReservation{}, false, err
	}
	defer func() { returnErr = errors.Join(returnErr, locked.Close()) }()
	eligible, err = service.spawnWorkEligibleLocked(locked)
	if err != nil {
		return spawnReservation{}, false, err
	}
	if !eligible {
		return spawnReservation{}, false, nil
	}

	return service.reserveSpawnAttemptAtRoot(root, service.deps.now().UTC(), nil)
}

func (service *Service) reserveSpawnAttemptAtRoot(
	root *storageRoot,
	now time.Time,
	canStart func(recordOperation) bool,
) (reservation spawnReservation, reserved bool, returnErr error) {
	if service == nil || root == nil {
		return spawnReservation{}, false, errStorageClosed
	}
	if now.IsZero() {
		return spawnReservation{}, false, errors.New("productmetrics: spawn reservation instant is zero")
	}
	now = now.UTC()
	if !recordOperationCanStart(canStart, recordOperationSpawnThrottleRead) {
		return spawnReservation{}, false, errRecordDecisionWindowExpired
	}
	data, existingLease, readErr := root.readFileLease(spawnThrottleFileName, maximumSpawnThrottleBytes)
	if existingLease != nil {
		defer func() { returnErr = errors.Join(returnErr, existingLease.Close()) }()
	}
	priorTokens := make(map[string]struct{})
	switch {
	case readErr == nil:
		priorTokens = recoverSpawnThrottleTokens(data)
		record, decodeErr := decodeSpawnThrottle(data)
		if decodeErr == nil && !record.attemptedAt.After(now) && now.Sub(record.attemptedAt) < spawnThrottleInterval {
			return spawnReservation{}, false, nil
		}
	case existingLease != nil && !errors.Is(readErr, errStorageReadLimit):
		return spawnReservation{}, false, readErr
	case existingLease == nil && !errors.Is(readErr, fs.ErrNotExist):
		// An unsafe file shape must not be replaced through a widened path-based
		// authority. The atomic writer below is reserved for missing, bounded
		// corrupt, future, or expired private records.
		return spawnReservation{}, false, readErr
	}

	if !recordOperationCanStart(canStart, recordOperationSpawnToken) {
		return spawnReservation{}, false, errRecordDecisionWindowExpired
	}
	token, err := service.deps.newUUID()
	if err != nil {
		return spawnReservation{}, false, err
	}
	if err := validateCanonicalUUIDv4(token); err != nil {
		return spawnReservation{}, false, fmt.Errorf("productmetrics: generated spawn token: %w", err)
	}
	if _, recovered := priorTokens[token]; recovered {
		return spawnReservation{}, false, errors.New("productmetrics: regenerated spawn token equals its prior authority")
	}
	record := spawnThrottleRecord{attemptToken: token, attemptedAt: now}
	encoded, err := encodeSpawnThrottle(record)
	if err != nil {
		return spawnReservation{}, false, err
	}
	if !recordOperationCanStart(canStart, recordOperationSpawnThrottleWrite) {
		return spawnReservation{}, false, errRecordDecisionWindowExpired
	}
	result, err := root.writeFileAtomicOutcome(spawnThrottleFileName, encoded)
	if err != nil {
		return spawnReservation{}, false, err
	}
	if result.state != storageWriteAppliedDurable {
		return spawnReservation{}, false, errors.New("productmetrics: spawn reservation was not durable")
	}
	return spawnReservation{attemptToken: token}, true, nil
}

func recoverSpawnThrottleTokens(data []byte) map[string]struct{} {
	tokens := make(map[string]struct{})
	if len(data) == 0 || len(data) > maximumSpawnThrottleBytes {
		return tokens
	}
	const canonicalUUIDTextBytes = 36
	for offset := 0; offset+canonicalUUIDTextBytes <= len(data); offset++ {
		candidate := data[offset : offset+canonicalUUIDTextBytes]
		if !canonicalUUIDv4Bytes(candidate) {
			continue
		}
		tokens[string(candidate)] = struct{}{}
	}
	return tokens
}

func canonicalUUIDv4Bytes(value []byte) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' ||
		value[14] != '4' || (value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b') {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (service *Service) spawnWorkEligibleLocked(locked *lockedState) (bool, error) {
	if service == nil || !locked.valid() {
		return false, nil
	}
	loaded := loadStateFromDirectory(locked.root)
	eligible := false
	if loaded.err != nil || !loaded.present {
		return false, loaded.Close()
	}
	projection := service.project(InvocationContext{
		DoNotTrack:          service.deps.getenv(envDoNotTrack),
		DisableUsageMetrics: service.deps.getenv(envDisableUsageMetrics),
	}, loaded)
	eligible = projection.state == StateEnabled ||
		(projection.state == StateServerPaused && projection.reason == ReasonPauseCleanupPending)
	if err := loaded.Close(); err != nil {
		return false, err
	}
	return eligible, nil
}

type privateUploaderProcessSpec struct {
	executable  string
	args        []string
	environment []string
	directory   string
}

type spawnDependencies struct {
	executable func() (string, error)
	environ    func() []string
	start      func(privateUploaderProcessSpec) (func() error, error)
}

// SpawnUploader durably reserves and asynchronously starts at most one
// detached uploader attempt. Callers deliberately ignore its error so metrics
// cannot affect command behavior.
func (service *Service) SpawnUploader(ctx context.Context) error {
	if !platformPrivateUploaderSupported() {
		return errDetachedUploaderUnsupported
	}
	return service.spawnUploader(ctx, service.deps.spawn)
}

func (service *Service) spawnUploader(ctx context.Context, dependencies spawnDependencies) error {
	if service == nil {
		return errors.New("productmetrics: service is nil")
	}
	if ctx == nil {
		return errors.New("productmetrics: spawn context is nil")
	}
	if service.deps.getenv(privateUploaderMarkerEnvironment) != "" {
		return errors.New("productmetrics: private uploader recursion rejected")
	}
	if dependencies.executable == nil || dependencies.environ == nil || dependencies.start == nil {
		return errors.New("productmetrics: spawn dependencies are incomplete")
	}
	reservation, reserved, err := service.reserveSpawnAttempt(ctx)
	if err != nil || !reserved {
		return err
	}
	return service.startReservedUploader(reservation, dependencies, nil)
}

func (service *Service) startReservedUploader(
	reservation spawnReservation,
	dependencies spawnDependencies,
	canStart func(recordOperation) bool,
) error {
	if service == nil {
		return errors.New("productmetrics: service is nil")
	}
	if err := validateCanonicalUUIDv4(reservation.attemptToken); err != nil {
		return fmt.Errorf("productmetrics: reserved spawn token: %w", err)
	}
	if dependencies.executable == nil || dependencies.environ == nil || dependencies.start == nil {
		return errors.New("productmetrics: spawn dependencies are incomplete")
	}
	if !recordOperationCanStart(canStart, recordOperationSpawnPrepare) {
		return errRecordDecisionWindowExpired
	}
	executable, err := dependencies.executable()
	if err != nil {
		return fmt.Errorf("productmetrics: resolve current executable: %w", err)
	}
	spec, err := buildPrivateUploaderProcessSpec(
		executable, reservation.attemptToken, service.deps.home.Home().Path(), dependencies.environ(),
	)
	if err != nil {
		return err
	}
	if !recordOperationCanStart(canStart, recordOperationSpawnStart) {
		return errRecordDecisionWindowExpired
	}
	wait, err := dependencies.start(spec)
	if err != nil {
		return fmt.Errorf("productmetrics: start private uploader: %w", err)
	}
	if wait == nil {
		return errors.New("productmetrics: private uploader returned no Wait function")
	}
	go func() { _ = wait() }()
	return nil
}

func buildPrivateUploaderProcessSpec(executable, token, home string, parentEnvironment []string) (privateUploaderProcessSpec, error) {
	if err := validateCanonicalUUIDv4(token); err != nil {
		return privateUploaderProcessSpec{}, fmt.Errorf("productmetrics: private uploader token: %w", err)
	}
	if executable == "" {
		return privateUploaderProcessSpec{}, errors.New("productmetrics: current executable is empty")
	}
	absolute, err := filepath.Abs(executable)
	if err != nil || !filepath.IsAbs(absolute) || filepath.Clean(absolute) != absolute {
		return privateUploaderProcessSpec{}, errors.New("productmetrics: current executable is not an absolute clean path")
	}
	environment, err := buildPrivateUploaderEnvironment(parentEnvironment, home)
	if err != nil {
		return privateUploaderProcessSpec{}, err
	}
	return privateUploaderProcessSpec{
		executable:  absolute,
		args:        []string{privateUploaderSentinel, token},
		environment: environment,
		directory:   "/",
	}, nil
}

func buildPrivateUploaderEnvironment(parent []string, home string) ([]string, error) {
	normalizedHome, ok := normalizePrivateUploaderPath(home)
	if !ok {
		return nil, errors.New("productmetrics: private uploader home is not an absolute clean path")
	}
	pathNames := map[string]bool{
		"HOME": true, "TMPDIR": true,
		"XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"XDG_RUNTIME_DIR": true, "XDG_STATE_HOME": true,
	}
	localeNames := map[string]bool{
		"LANG": true, "LC_ALL": true, "LC_ADDRESS": true, "LC_COLLATE": true,
		"LC_CTYPE": true, "LC_IDENTIFICATION": true, "LC_MEASUREMENT": true,
		"LC_MESSAGES": true, "LC_MONETARY": true, "LC_NAME": true,
		"LC_NUMERIC": true, "LC_PAPER": true, "LC_TELEPHONE": true, "LC_TIME": true,
	}
	values := make(map[string]string)
	for _, entry := range parent {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.IndexByte(name, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			continue
		}
		switch {
		case pathNames[name]:
			if normalized, ok := normalizePrivateUploaderPath(value); ok {
				values[name] = normalized
			}
		case localeNames[name]:
			if normalized, ok := normalizePrivateUploaderLocale(value); ok {
				values[name] = normalized
			}
		}
	}
	values["GC_HOME"] = normalizedHome
	values[privateUploaderMarkerEnvironment] = privateUploaderMarkerValue
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment, nil
}

func normalizePrivateUploaderPath(value string) (string, bool) {
	if value == "" || len(value) > 4096 || strings.IndexByte(value, 0) >= 0 || !filepath.IsAbs(value) {
		return "", false
	}
	normalized := filepath.Clean(value)
	if normalized == "." || len(normalized) > 4096 {
		return "", false
	}
	return normalized, true
}

func normalizePrivateUploaderLocale(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return "", false
	}
	for index := range len(value) {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' &&
			character != '-' && character != '@' {
			return "", false
		}
	}
	return value, true
}

type privateUploaderRunDependencies struct {
	now              func() time.Time
	start            uploadStartFunc
	budget           spoolWorkBudget
	uploaderLockWait time.Duration
	beforeOperation  func(uploaderOperation)
}

// RunPrivateUploader runs one attempt-bound batch in a cooperative ten-second
// child budget. It validates the recursion marker before touching storage.
func (service *Service) RunPrivateUploader(ctx context.Context, invocation PrivateUploaderInvocation) error {
	if ctx == nil {
		return errors.New("productmetrics: private uploader context is nil")
	}
	if service == nil {
		return errors.New("productmetrics: service is nil")
	}
	if service.deps.getenv(privateUploaderMarkerEnvironment) != privateUploaderMarkerValue {
		return errors.New("productmetrics: private uploader recursion marker is absent")
	}
	// Inert/stale children must exit silently before constructing production
	// transport policy. The retained-root path below repeats this eligibility
	// check and then performs token authorization under uploader->state locks,
	// so this read-only fast path grants no upload authority.
	eligible, err := service.uploadNeedsMutableWork()
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	start := service.deps.privateUploaderStart
	if start == nil {
		factory := service.deps.privateUploaderStartFactory
		if factory == nil {
			factory = productionUploaderStartFactory
		}
		start = lazyUploadStart(factory)
	}
	workContext, cancel := context.WithTimeout(ctx, privateUploaderWorkBudget)
	defer cancel()
	return service.runPrivateUploader(workContext, invocation, privateUploaderRunDependencies{
		now:   service.deps.now,
		start: start,
	})
}

func (service *Service) runPrivateUploader(
	ctx context.Context,
	invocation PrivateUploaderInvocation,
	dependencies privateUploaderRunDependencies,
) (returnErr error) {
	if service == nil {
		return errors.New("productmetrics: service is nil")
	}
	if ctx == nil {
		return errors.New("productmetrics: private uploader context is nil")
	}
	if err := validateCanonicalUUIDv4(invocation.attemptToken); err != nil {
		return fmt.Errorf("productmetrics: invalid private uploader capability: %w", err)
	}
	if service.deps.getenv(privateUploaderMarkerEnvironment) != privateUploaderMarkerValue {
		return errors.New("productmetrics: private uploader recursion marker is absent")
	}
	if dependencies.start == nil {
		return errors.New("productmetrics: private uploader starter is nil")
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
	if dependencies.uploaderLockWait <= 0 || dependencies.uploaderLockWait > privateUploaderWorkBudget {
		dependencies.uploaderLockWait = privateUploaderLockWait
	}
	eligible, err := service.uploadNeedsMutableWork()
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	root, err := openStorageRootMutableWithHooks(service.deps.home, service.deps.storageHooks)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()
	lockContext, cancelLock := context.WithTimeout(ctx, dependencies.uploaderLockWait)
	uploader, err := service.lockUploader(lockContext, root)
	cancelLock()
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, uploader.Close()) }()
	authorize := func(locked *lockedState) error {
		return validateSpawnAttemptLocked(locked, invocation.attemptToken, dependencies.now().UTC())
	}
	_, err = service.uploadOneBatchLocked(ctx, root, uploader, uploaderDependencies{
		now:             dependencies.now,
		start:           dependencies.start,
		budget:          dependencies.budget,
		beforeOperation: dependencies.beforeOperation,
		authorizeLocked: authorize,
	})
	if onlyStaleSpawnAttempt(err) {
		return nil
	}
	return err
}

func validateSpawnAttemptLocked(locked *lockedState, token string, now time.Time) error {
	if !locked.valid() || locked.root == nil {
		return errors.New("productmetrics: state lock is not held")
	}
	if err := validateCanonicalUUIDv4(token); err != nil {
		return fmt.Errorf("productmetrics: invalid private uploader token: %w", err)
	}
	data, err := locked.root.readFile(spawnThrottleFileName, maximumSpawnThrottleBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errStaleSpawnAttempt
		}
		return err
	}
	record, err := decodeSpawnThrottle(data)
	if err != nil {
		return err
	}
	if record.attemptToken != token || record.attemptedAt.After(now.UTC()) {
		return errStaleSpawnAttempt
	}
	return nil
}

func onlyStaleSpawnAttempt(err error) bool {
	if err == nil {
		return false
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		var stale staleSpawnAttemptError
		return errors.As(err, &stale)
	}
	found := false
	for _, child := range joined.Unwrap() {
		if child == nil {
			continue
		}
		found = true
		if !onlyStaleSpawnAttempt(child) {
			return false
		}
	}
	return found
}

func productionUploaderStartFactory() (uploadStartFunc, error) {
	transport, err := newProductionUploadTransport(CurrentReleaseIdentity())
	if err != nil {
		return nil, err
	}
	return asynchronousUploadStart(transport), nil
}

func lazyUploadStart(factory func() (uploadStartFunc, error)) uploadStartFunc {
	return func(ctx context.Context, prepared preparedUploadBatch, metricsEpoch uint64) (uploadWaitFunc, error) {
		if factory == nil {
			return nil, errors.New("productmetrics: private uploader start factory is nil")
		}
		start, err := factory()
		if err != nil {
			return nil, err
		}
		if start == nil {
			return nil, errors.New("productmetrics: private uploader start factory returned nil")
		}
		return start(ctx, prepared, metricsEpoch)
	}
}

type asynchronousUploadResult struct {
	response uploadResponse
	err      error
}

func asynchronousUploadStart(transport *uploadTransport) uploadStartFunc {
	return func(ctx context.Context, prepared preparedUploadBatch, metricsEpoch uint64) (uploadWaitFunc, error) {
		if transport == nil {
			return nil, errors.New("productmetrics: upload transport is nil")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result := make(chan asynchronousUploadResult, 1)
		gate := newRoundTripStartGate()
		attempt := *transport
		attempt.roundTripGate = gate
		go func() {
			response, err := attempt.upload(ctx, prepared, metricsEpoch)
			result <- asynchronousUploadResult{response: response, err: err}
		}()
		var completed *asynchronousUploadResult
		select {
		case <-gate.entered:
		case early := <-result:
			if gate.didEnter() {
				completed = &early
			} else {
				_ = gate.abort()
				if early.err == nil {
					early.err = errors.New("productmetrics: upload completed before RoundTrip entry")
				}
				return nil, early.err
			}
		case <-ctx.Done():
			if gate.abort() {
				return nil, ctx.Err()
			}
			// RoundTrip won the gate race, so request initiation already
			// linearized and Wait must settle it even when the context expired.
		}
		return func() (uploadResponse, error) {
			if completed != nil {
				return completed.response, completed.err
			}
			finished := <-result
			return finished.response, finished.err
		}, nil
	}
}
