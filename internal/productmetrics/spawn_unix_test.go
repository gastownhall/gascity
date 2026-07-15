//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestSpawnUploaderUsesAbsoluteExactSpecAndWaitsAsynchronously(t *testing.T) {
	home := newMetricsTestHome(t)
	service := mustOpenTestService(t, spawnTestDependencies(
		home,
		func() time.Time { return time.Date(2026, time.July, 12, 3, 0, 0, 0, time.UTC) },
		func() (string, error) { return testSpawnTokenOne, nil },
	))
	writeStateFixture(t, service.deps.home, activeEnabledStateForSpawnTest())

	waitRelease := make(chan struct{})
	waitStarted := make(chan struct{})
	waitFinished := make(chan struct{})
	var starts atomic.Int32
	var captured privateUploaderProcessSpec
	err := service.spawnUploader(context.Background(), spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return []string{"HOME=/home/alice", "SECRET=no"} },
		start: func(spec privateUploaderProcessSpec) (func() error, error) {
			starts.Add(1)
			captured = spec
			return func() error {
				close(waitStarted)
				<-waitRelease
				close(waitFinished)
				return nil
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || captured.executable != "/opt/gascity/bin/gc" || captured.directory != "/" ||
		!reflect.DeepEqual(captured.args, []string{privateUploaderSentinel, testSpawnTokenOne}) {
		t.Fatalf("spawn spec = %#v, starts=%d", captured, starts.Load())
	}
	select {
	case <-waitStarted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("asynchronous Wait was not started")
	}
	select {
	case <-waitFinished:
		t.Fatal("spawnUploader blocked on or prematurely completed Wait")
	default:
	}
	close(waitRelease)
	select {
	case <-waitFinished:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("asynchronous Wait did not reap completion")
	}
}

func TestSpawnUploaderEntropyFailureSkipsProcessOnly(t *testing.T) {
	sentinel := errors.New("entropy failed")
	home := newMetricsTestHome(t)
	service := mustOpenTestService(t, spawnTestDependencies(
		home,
		func() time.Time { return time.Date(2026, time.July, 12, 3, 0, 0, 0, time.UTC) },
		func() (string, error) { return "", sentinel },
	))
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	started := false
	err := service.spawnUploader(context.Background(), spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return nil },
		start: func(privateUploaderProcessSpec) (func() error, error) {
			started = true
			return func() error { return nil }, nil
		},
	})
	if !errors.Is(err, sentinel) || started {
		t.Fatalf("entropy failure = %v, started=%v", err, started)
	}
	state := readStateFixture(t, home)
	if state != activeEnabledStateForSpawnTest() {
		t.Fatalf("entropy failure changed durable consent state: %#v", state)
	}
}

func activeEnabledStateForSpawnTest() persistedState {
	return persistedState{
		StateSchema: 1, CounterNamespace: 1, StateGeneration: 1,
		Preference: preferenceEnabled, RequiredNoticeVersion: 2, AcceptedNoticeVersion: 2,
		InstallationID: testInstallationID, SpoolGeneration: testSpoolGeneration,
		CleanupKind: cleanupNone,
	}
}

func TestStartedPrivateUploaderIsReapedWhenParentDescriptorCloseFails(t *testing.T) {
	closeFailure := errors.New("close failed")
	waitCalled := make(chan struct{})
	var waits atomic.Int32
	wait, err := startPrivateUploaderCommand(
		func() error { return nil },
		func() error {
			if waits.Add(1) == 1 {
				close(waitCalled)
			}
			return nil
		},
		func() error { return closeFailure },
	)
	if !errors.Is(err, closeFailure) || wait != nil {
		t.Fatalf("post-Start close failure = (%T, %v)", wait, err)
	}
	select {
	case <-waitCalled:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("started child was not reaped after parent descriptor close failure")
	}
	if waits.Load() != 1 {
		t.Fatalf("started child Wait calls = %d, want exactly one", waits.Load())
	}
}

func TestPlatformPrivateUploaderCommandSnapshot(t *testing.T) {
	spec := privateUploaderProcessSpec{
		executable: "/opt/gascity/bin/gc",
		args:       []string{privateUploaderSentinel, testSpawnTokenOne},
		environment: []string{
			"GC_HOME=/home/alice/.gc",
			privateUploaderMarkerEnvironment + "=" + privateUploaderMarkerValue,
			"LANG=C",
		},
		directory: "/",
	}
	command, null, err := newPlatformPrivateUploaderCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	wantArgs := []string{"/opt/gascity/bin/gc", privateUploaderSentinel, testSpawnTokenOne}
	if command.Path != spec.executable || !reflect.DeepEqual(command.Args, wantArgs) ||
		!reflect.DeepEqual(command.Env, spec.environment) || command.Dir != "/" {
		t.Fatalf("private uploader command = Path:%q Args:%q Env:%q Dir:%q", command.Path, command.Args, command.Env, command.Dir)
	}
	if command.Stdin != null || command.Stdout != null || command.Stderr != null {
		t.Fatalf("private uploader stdio does not share one null file: stdin=%T stdout=%T stderr=%T null=%p",
			command.Stdin, command.Stdout, command.Stderr, null)
	}
	if command.SysProcAttr == nil || !command.SysProcAttr.Setsid || command.SysProcAttr.Setpgid {
		t.Fatalf("private uploader SysProcAttr = %#v, want Setsid only", command.SysProcAttr)
	}
	if command.Process != nil || len(command.ExtraFiles) != 0 {
		t.Fatalf("unstarted private command leaked process/extra files: process=%v extra=%d", command.Process, len(command.ExtraFiles))
	}
}

func TestRecordOnceReservesAndStartsAfterReleasingStateTransaction(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne, testSpawnTokenOne)
	deps.now = func() time.Time { return testRecordHour }
	startCalled := false
	waitCalled := make(chan struct{})
	deps.spawn = spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return []string{"HOME=/home/alice"} },
		start: func(spec privateUploaderProcessSpec) (func() error, error) {
			startCalled = true
			if spec.args[1] != testSpawnTokenOne || spec.directory != "/" {
				t.Fatalf("integrated spawn spec = %#v", spec)
			}
			probe := mustOpenMutableRoot(t, home)
			defer func() { _ = probe.Close() }()
			probeContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			state, err := probe.acquireLock(probeContext, stateLockName)
			if err != nil {
				t.Fatalf("Start ran before state transaction released: %v", err)
			}
			if err := state.Release(); err != nil {
				t.Fatal(err)
			}
			return func() error { close(waitCalled); return errors.New("ignored test Wait error") }, nil
		},
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	defer func() { _ = permit.Close() }()
	if got := service.RecordOnce(permit, CommandHelp); got != RecordStored || !startCalled {
		t.Fatalf("RecordOnce = %v, startCalled=%v", got, startCalled)
	}
	select {
	case <-waitCalled:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("integrated spawn was not asynchronously reaped")
	}
	assertSpawnThrottleRecord(t, home, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour})
}

func TestRecordOnceKeepsStoredEventWhenSpawnWindowExpires(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	current := testRecordHour
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne, testSpawnTokenOne)
	deps.now = func() time.Time { return current }
	deps.beforeRecordOperation = func(operation recordOperation) {
		if operation == recordOperationSpawnThrottleRead {
			current = testRecordHour.Add(defaultRecordDecisionBudget)
		}
	}
	starts := 0
	deps.spawn = spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return nil },
		start: func(privateUploaderProcessSpec) (func() error, error) {
			starts++
			return func() error { return nil }, nil
		},
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	defer func() { _ = permit.Close() }()
	if got := service.RecordOnce(permit, CommandHelp); got != RecordStored || starts != 0 {
		t.Fatalf("window-expired RecordOnce = %v, starts=%d", got, starts)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), spawnThrottleFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("window-expired record wrote throttle: %v", err)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, testEventIDOne)
}

func TestRecordOnceRejectsRegeneratedPriorSpawnToken(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	prior := spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour.Add(-spawnThrottleInterval)}
	writeSpawnThrottleToRoot(t, root, prior)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne, testSpawnTokenOne)
	deps.now = func() time.Time { return testRecordHour }
	starts := 0
	deps.spawn = spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return nil },
		start: func(privateUploaderProcessSpec) (func() error, error) {
			starts++
			return func() error { return nil }, nil
		},
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	defer func() { _ = permit.Close() }()
	if got := service.RecordOnce(permit, CommandHelp); got != RecordStored || starts != 0 {
		t.Fatalf("repeated-token RecordOnce = %v, starts=%d", got, starts)
	}
	assertSpawnThrottleRecord(t, home, prior)
}

func TestRecordOnceDoesNotStartWhenRetainedThrottleLeaseCloseFails(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(7, 2, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{
		attemptToken: testSpawnTokenOne,
		attemptedAt:  testRecordHour.Add(-spawnThrottleInterval),
	})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, testEventIDOne, testSpawnTokenTwo)
	deps.now = func() time.Time { return testRecordHour }
	closedLease := false
	var injectionErr error
	deps.storageHooks.afterRead = func(path string, _, read int, readErr error) {
		if closedLease || path != filepath.Join(home.Root(), spawnThrottleFileName) || read != 0 || readErr != nil {
			return
		}
		closedLease = true
		injectionErr = closeOpenFileMatchingPath(path)
	}
	starts := 0
	deps.spawn = spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return []string{"HOME=/home/alice"} },
		start: func(privateUploaderProcessSpec) (func() error, error) {
			starts++
			return func() error { return nil }, nil
		},
	}
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	defer func() { _ = permit.Close() }()
	if got := service.RecordOnce(permit, CommandHelp); got != RecordStored {
		t.Fatalf("RecordOnce with reservation close uncertainty = %v, want stored event", got)
	}
	if !closedLease || injectionErr != nil {
		t.Fatalf("close retained throttle lease: attempted=%v err=%v", closedLease, injectionErr)
	}
	if starts != 0 {
		t.Fatalf("reservation close uncertainty started %d uploader processes", starts)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, testEventIDOne)
}

func TestUploadStartWaitsForActualRoundTripEntry(t *testing.T) {
	allowEntry := make(chan struct{})
	var releaseEntry sync.Once
	releaseValidation := func() { releaseEntry.Do(func() { close(allowEntry) }) }
	t.Cleanup(releaseValidation)
	validationStarted := make(chan struct{})
	entered := make(chan struct{})
	roundTripper := roundTripFunc(func(*http.Request) (*http.Response, error) {
		close(entered)
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	client := newStrictUploadHTTPClient(roundTripper)
	endpoint, err := url.Parse("https://127.0.0.1/upload")
	if err != nil {
		t.Fatal(err)
	}
	transport := &uploadTransport{
		endpoint: endpoint,
		client:   client,
		pauseKeys: func(func(pausePublicKeyEntry)) {
			close(validationStarted)
			<-allowEntry
		},
	}
	event := fixedEvent()
	body, err := EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := buildUploadBatch([]claimedEventFile{{name: event.EventID, body: body}}, uploadBatchIdentity{
		installationID: event.InstallationID,
		releaseVersion: event.ReleaseVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	type startResult struct {
		wait uploadWaitFunc
		err  error
	}
	returned := make(chan startResult, 1)
	go func() {
		wait, err := asynchronousUploadStart(transport)(context.Background(), prepared, 1)
		returned <- startResult{wait: wait, err: err}
	}()
	select {
	case <-validationStarted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("upload worker did not reach pre-RoundTrip validation")
	}
	select {
	case result := <-returned:
		t.Fatalf("Start returned before RoundTrip entry: %#v", result)
	default:
	}
	releaseValidation()
	select {
	case <-entered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("RoundTrip was never entered")
	}
	var result startResult
	select {
	case result = <-returned:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Start did not return after RoundTrip entry")
	}
	if result.err != nil || result.wait == nil {
		t.Fatalf("Start after RoundTrip entry = (%T, %v)", result.wait, result.err)
	}
	if _, err := result.wait(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadStartReturnsPreEntryValidationErrorWithoutDeadlock(t *testing.T) {
	returned := make(chan error, 1)
	go func() {
		wait, err := asynchronousUploadStart(&uploadTransport{})(context.Background(), preparedUploadBatch{}, 1)
		if wait != nil && err == nil {
			err = errors.New("unexpected Wait from invalid transport")
		}
		returned <- err
	}()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("invalid transport Start succeeded")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Start deadlocked before RoundTrip on transport validation error")
	}
}

func TestUploadStartCancellationAbortsBeforeRoundTripWithoutNetwork(t *testing.T) {
	releaseValidation := make(chan struct{})
	var releaseValidationOnce sync.Once
	releaseBlockedValidation := func() { releaseValidationOnce.Do(func() { close(releaseValidation) }) }
	t.Cleanup(releaseBlockedValidation)
	validationStarted := make(chan struct{})
	validationReleased := make(chan struct{})
	roundTripper := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network must not start")
	})
	client := newStrictUploadHTTPClient(roundTripper)
	endpoint, err := url.Parse("https://127.0.0.1/upload")
	if err != nil {
		t.Fatal(err)
	}
	transport := &uploadTransport{
		endpoint: endpoint,
		client:   client,
		pauseKeys: func(func(pausePublicKeyEntry)) {
			close(validationStarted)
			<-releaseValidation
			close(validationReleased)
		},
	}
	event := fixedEvent()
	body, err := EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := buildUploadBatch([]claimedEventFile{{name: event.EventID, body: body}}, uploadBatchIdentity{
		installationID: event.InstallationID,
		releaseVersion: event.ReleaseVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type startResult struct {
		wait uploadWaitFunc
		err  error
	}
	returned := make(chan startResult, 1)
	go func() {
		wait, err := asynchronousUploadStart(transport)(ctx, prepared, 1)
		returned <- startResult{wait: wait, err: err}
	}()
	select {
	case <-validationStarted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("upload worker did not reach cancellable pre-RoundTrip validation")
	}
	cancel()
	select {
	case result := <-returned:
		if !errors.Is(result.err, context.Canceled) || result.wait != nil {
			t.Fatalf("canceled pre-entry Start = (%T, %v)", result.wait, result.err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("pre-entry Start did not return after cancellation")
	}
	releaseBlockedValidation()
	select {
	case <-validationReleased:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("canceled upload worker did not leave pre-RoundTrip validation")
	}
}

func TestAbortedRoundTripStartGatePermanentlyBlocksNetwork(t *testing.T) {
	var roundTrips atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		roundTrips.Add(1)
		return nil, errors.New("network must not start")
	})
	gate := newRoundTripStartGate()
	if !gate.abort() {
		t.Fatal("pending RoundTrip start gate could not be aborted")
	}
	transport := &roundTripEntryTransport{base: base, gate: gate}
	request, err := http.NewRequest(http.MethodPost, "https://127.0.0.1/upload", nil)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if response, err := transport.RoundTrip(request); err == nil || response != nil {
			t.Fatalf("aborted RoundTrip attempt %d = (%v, %v), want blocked error", attempt, response, err)
		}
	}
	if got := roundTrips.Load(); got != 0 {
		t.Fatalf("aborted RoundTrip gate made %d network attempts", got)
	}
}

func TestReserveSpawnAttemptDurablyThrottlesForSixtySeconds(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	now := time.Date(2026, time.July, 12, 4, 0, 0, 0, time.UTC)
	tokens := []string{testSpawnTokenOne, testSpawnTokenTwo}
	issued := 0
	service := mustOpenTestService(t, spawnTestDependencies(home, func() time.Time { return now }, func() (string, error) {
		token := tokens[issued]
		issued++
		return token, nil
	}))

	first, reserved, err := service.reserveSpawnAttempt(context.Background())
	if err != nil || !reserved || first.attemptToken != testSpawnTokenOne {
		t.Fatalf("first reservation = (%#v, %v, %v)", first, reserved, err)
	}
	assertSpawnThrottleRecord(t, home, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: now})

	now = now.Add(spawnThrottleInterval - time.Nanosecond)
	second, reserved, err := service.reserveSpawnAttempt(context.Background())
	if err != nil || reserved || second != (spawnReservation{}) || issued != 1 {
		t.Fatalf("suppressed reservation = (%#v, %v, %v), UUID calls=%d", second, reserved, err, issued)
	}

	now = now.Add(time.Nanosecond)
	third, reserved, err := service.reserveSpawnAttempt(context.Background())
	if err != nil || !reserved || third.attemptToken != testSpawnTokenTwo || issued != 2 {
		t.Fatalf("boundary reservation = (%#v, %v, %v), UUID calls=%d", third, reserved, err, issued)
	}
	assertSpawnThrottleRecord(t, home, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: now})
}

func TestReserveSpawnAttemptReplacesCorruptAndFutureRecordsOnceWithoutABA(t *testing.T) {
	for _, fixture := range []struct {
		name string
		body func(time.Time) []byte
	}{
		{name: "corrupt", body: func(time.Time) []byte { return []byte("not = [toml") }},
		{name: "future-by-one-nanosecond-after-clock-rollback", body: func(now time.Time) []byte {
			data, err := encodeSpawnThrottle(spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: now.Add(time.Nanosecond)})
			if err != nil {
				t.Fatal(err)
			}
			return data
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, activeEnabledStateForSpawnTest())
			now := time.Date(2026, time.July, 12, 5, 0, 0, 0, time.UTC)
			if err := os.WriteFile(filepath.Join(home.Root(), spawnThrottleFileName), fixture.body(now), 0o600); err != nil {
				t.Fatal(err)
			}
			issued := 0
			service := mustOpenTestService(t, spawnTestDependencies(home, func() time.Time { return now }, func() (string, error) {
				issued++
				return testSpawnTokenTwo, nil
			}))
			reservation, reserved, err := service.reserveSpawnAttempt(context.Background())
			if err != nil || !reserved || reservation.attemptToken != testSpawnTokenTwo || issued != 1 {
				t.Fatalf("replacement reservation = (%#v, %v, %v), UUID calls=%d", reservation, reserved, err, issued)
			}
			if again, reserved, err := service.reserveSpawnAttempt(context.Background()); err != nil || reserved || again != (spawnReservation{}) || issued != 1 {
				t.Fatalf("replacement retry = (%#v, %v, %v), UUID calls=%d", again, reserved, err, issued)
			}
			assertSpawnThrottleRecord(t, home, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: now})
		})
	}
}

func TestReserveSpawnAttemptRejectsRecoveredCorruptTokenABA(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	now := time.Date(2026, time.July, 12, 5, 30, 0, 0, time.UTC)
	valid, err := encodeSpawnThrottle(spawnThrottleRecord{
		attemptToken: testSpawnTokenOne,
		attemptedAt:  now.Add(-spawnThrottleInterval),
	})
	if err != nil {
		t.Fatal(err)
	}
	corrupt := slices.Clone(valid)
	corrupt = append(corrupt, []byte("unknown = true\n")...)
	if err := os.WriteFile(filepath.Join(home.Root(), spawnThrottleFileName), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	service := mustOpenTestService(t, spawnTestDependencies(home, func() time.Time { return now }, func() (string, error) {
		return testSpawnTokenOne, nil
	}))
	reservation, reserved, err := service.reserveSpawnAttempt(context.Background())
	if err == nil || reserved || reservation != (spawnReservation{}) {
		t.Fatalf("corrupt-token ABA reservation = (%#v, %v, %v)", reservation, reserved, err)
	}
	got, readErr := os.ReadFile(filepath.Join(home.Root(), spawnThrottleFileName))
	if readErr != nil || string(got) != string(corrupt) {
		t.Fatalf("corrupt-token ABA changed prior authority: data=%q err=%v", got, readErr)
	}
}

func TestReserveSpawnAttemptRecoversEveryTokenFromMalformedDuplicateKeys(t *testing.T) {
	now := time.Date(2026, time.July, 12, 5, 35, 0, 0, time.UTC)
	duplicateBody := func(first, second string) []byte {
		return []byte(fmt.Sprintf(
			"throttle_schema = 1\nattempt_token = %q\nattempted_at = %q\nattempt_token = %q\n",
			first, now.Add(-spawnThrottleInterval).Format(time.RFC3339Nano), second,
		))
	}
	for _, test := range []struct {
		name      string
		body      []byte
		generated string
	}{
		{name: "old-then-other/generated-old", body: duplicateBody(testSpawnTokenOne, testSpawnTokenTwo), generated: testSpawnTokenOne},
		{name: "old-then-other/generated-other", body: duplicateBody(testSpawnTokenOne, testSpawnTokenTwo), generated: testSpawnTokenTwo},
		{name: "other-then-old/generated-old", body: duplicateBody(testSpawnTokenTwo, testSpawnTokenOne), generated: testSpawnTokenOne},
		{name: "other-then-old/generated-other", body: duplicateBody(testSpawnTokenTwo, testSpawnTokenOne), generated: testSpawnTokenTwo},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, activeEnabledStateForSpawnTest())
			path := filepath.Join(home.Root(), spawnThrottleFileName)
			if err := os.WriteFile(path, test.body, 0o600); err != nil {
				t.Fatal(err)
			}
			service := mustOpenTestService(t, spawnTestDependencies(home, func() time.Time { return now }, func() (string, error) {
				return test.generated, nil
			}))
			reservation, reserved, err := service.reserveSpawnAttempt(context.Background())
			if err == nil || reserved || reservation != (spawnReservation{}) {
				t.Fatalf("duplicate-key recovered-token reservation = (%#v, %v, %v)", reservation, reserved, err)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != string(test.body) {
				t.Fatalf("recovered-token rejection changed malformed record: data=%q err=%v", got, readErr)
			}
		})
	}

	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "old-then-other", body: duplicateBody(testSpawnTokenOne, testSpawnTokenTwo)},
		{name: "other-then-old", body: duplicateBody(testSpawnTokenTwo, testSpawnTokenOne)},
	} {
		t.Run("fresh-replacement/"+test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, activeEnabledStateForSpawnTest())
			if err := os.WriteFile(filepath.Join(home.Root(), spawnThrottleFileName), test.body, 0o600); err != nil {
				t.Fatal(err)
			}
			uuidCalls := 0
			service := mustOpenTestService(t, spawnTestDependencies(home, func() time.Time { return now }, func() (string, error) {
				uuidCalls++
				return testSpawnTokenThree, nil
			}))
			reservation, reserved, err := service.reserveSpawnAttempt(context.Background())
			if err != nil || !reserved || reservation.attemptToken != testSpawnTokenThree || uuidCalls != 1 {
				t.Fatalf("fresh duplicate-key recovery = (%#v, %v, %v), UUID calls=%d", reservation, reserved, err, uuidCalls)
			}
			if again, reserved, err := service.reserveSpawnAttempt(context.Background()); err != nil || reserved || again != (spawnReservation{}) || uuidCalls != 1 {
				t.Fatalf("fresh duplicate-key retry = (%#v, %v, %v), UUID calls=%d", again, reserved, err, uuidCalls)
			}
			assertSpawnThrottleRecord(t, home, spawnThrottleRecord{attemptToken: testSpawnTokenThree, attemptedAt: now})
		})
	}
}

func TestReserveSpawnAttemptDoesNotReplaceArbitraryRetainedLeaseReadError(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	now := time.Date(2026, time.July, 12, 5, 45, 0, 0, time.UTC)
	prior := spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: now.Add(-spawnThrottleInterval)}
	root := mustOpenMutableRoot(t, home)
	writeSpawnThrottleToRoot(t, root, prior)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	blockRead := false
	uuidCalls := 0
	deps := spawnTestDependencies(home, func() time.Time { return now }, func() (string, error) {
		uuidCalls++
		return testSpawnTokenTwo, nil
	})
	deps.storageHooks.beforeRead = func(path string) {
		if path == filepath.Join(home.Root(), spawnThrottleFileName) {
			blockRead = true
		}
	}
	deps.storageHooks.decisionGate = func() bool { return !blockRead }
	service := mustOpenTestService(t, deps)
	reservation, reserved, err := service.reserveSpawnAttempt(context.Background())
	if !errors.Is(err, errRecordDecisionWindowExpired) || reserved || reservation != (spawnReservation{}) || uuidCalls != 0 {
		t.Fatalf("injected read-error reservation = (%#v, %v, %v), UUID calls=%d", reservation, reserved, err, uuidCalls)
	}
	assertSpawnThrottleRecord(t, home, prior)
}

func TestReserveSpawnAttemptReplacesOversizedSafeRecord(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	path := filepath.Join(home.Root(), spawnThrottleFileName)
	if err := os.WriteFile(path, make([]byte, maximumSpawnThrottleBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 12, 5, 50, 0, 0, time.UTC)
	service := mustOpenTestService(t, spawnTestDependencies(home, func() time.Time { return now }, func() (string, error) {
		return testSpawnTokenTwo, nil
	}))
	reservation, reserved, err := service.reserveSpawnAttempt(context.Background())
	if err != nil || !reserved || reservation.attemptToken != testSpawnTokenTwo {
		t.Fatalf("oversized reservation = (%#v, %v, %v)", reservation, reserved, err)
	}
	assertSpawnThrottleRecord(t, home, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: now})
}

func TestSpawnUploaderDoesNotStartAfterReservationDirectorySyncFailure(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	syncFailure := errors.New("spawn throttle directory sync failed")
	renameApplied := false
	deps := spawnTestDependencies(home, func() time.Time {
		return time.Date(2026, time.July, 12, 5, 55, 0, 0, time.UTC)
	}, func() (string, error) { return testSpawnTokenOne, nil })
	deps.storageHooks.beforeMutation = func(step storageStep, path string) {
		if step == storageStepRename && path == spawnThrottleFileName {
			renameApplied = true
		}
	}
	deps.storageHooks.beforeStep = func(step storageStep) error {
		if renameApplied && step == storageStepDirectorySync {
			return syncFailure
		}
		return nil
	}
	service := mustOpenTestService(t, deps)
	started := false
	err := service.spawnUploader(context.Background(), spawnDependencies{
		executable: func() (string, error) { return "/opt/gascity/bin/gc", nil },
		environ:    func() []string { return nil },
		start: func(privateUploaderProcessSpec) (func() error, error) {
			started = true
			return func() error { return nil }, nil
		},
	})
	if !errors.Is(err, syncFailure) || !renameApplied || started {
		t.Fatalf("sync-pending spawn = err:%v rename:%v started:%v", err, renameApplied, started)
	}
}

func TestReserveSpawnAttemptIsSingleWinnerAcrossConcurrentParents(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	now := time.Date(2026, time.July, 12, 6, 0, 0, 0, time.UTC)
	const contenders = 24
	services := make([]*Service, contenders)
	for index := range contenders {
		token := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1)
		services[index] = mustOpenTestService(t, spawnTestDependencies(home, func() time.Time { return now }, func() (string, error) {
			return token, nil
		}))
	}
	start := make(chan struct{})
	var winners atomic.Int32
	var failures atomic.Int32
	var group sync.WaitGroup
	for _, service := range services {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, reserved, err := service.reserveSpawnAttempt(context.Background())
			if err != nil {
				failures.Add(1)
			} else if reserved {
				winners.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	if failures.Load() != 0 || winners.Load() != 1 {
		t.Fatalf("concurrent reservation failures=%d winners=%d, want 0/1", failures.Load(), winners.Load())
	}
}

func TestPrivateUploaderFinalTokenRecheckPreventsSupersededChildNetwork(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	service.deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	sends := 0
	superseded := false
	err := service.runPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne}, privateUploaderRunDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			sends++
			return uploadResponse{kind: uploadResponseAccepted, statusCode: 200}, nil
		}),
		beforeOperation: func(operation uploaderOperation) {
			if operation != uploaderOperationBeforePreSendRevalidation || superseded {
				return
			}
			superseded = true
			peer := mustOpenMutableRoot(t, home)
			defer func() { _ = peer.Close() }()
			locked, lockErr := service.lockState(context.Background(), peer)
			if lockErr != nil {
				t.Fatal(lockErr)
			}
			writeSpawnThrottleToRoot(t, peer, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: testRecordHour.Add(spawnThrottleInterval)})
			if closeErr := locked.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		},
	})
	if err != nil || !superseded || sends != 0 {
		t.Fatalf("superseded child: err=%v superseded=%v sends=%d", err, superseded, sends)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
}

func TestPrivateUploaderDoesNotHideRestoreFailureBehindStaleAttempt(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	service.deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	restoreFailure := errors.New("restore failed after stale attempt")
	restorePhase := false
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if restorePhase && step == storageStepRename {
			return restoreFailure
		}
		return nil
	}
	sends := 0
	err := service.runPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne}, privateUploaderRunDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			sends++
			return uploadResponse{kind: uploadResponseAccepted}, nil
		}),
		beforeOperation: func(operation uploaderOperation) {
			if operation != uploaderOperationBeforePreSendRevalidation || restorePhase {
				return
			}
			peer := mustOpenMutableRoot(t, home)
			locked, lockErr := service.lockState(context.Background(), peer)
			if lockErr != nil {
				t.Fatal(lockErr)
			}
			writeSpawnThrottleToRoot(t, peer, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: testRecordHour})
			if closeErr := errors.Join(locked.Close(), peer.Close()); closeErr != nil {
				t.Fatal(closeErr)
			}
			restorePhase = true
		},
	})
	if !errors.Is(err, restoreFailure) || sends != 0 {
		t.Fatalf("stale attempt restore failure = %v, sends=%d", err, sends)
	}
}

func TestPrivateUploaderRetainsOneRootAcrossLexicalReplacement(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	service.deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	root := mustOpenMutableRoot(t, home)
	eventA := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	dataA := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, eventA)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(dataA))}); err != nil {
		t.Fatal(err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	replaced := false
	movedRoot := home.Root() + "-retained"
	sends := 0
	err := service.runPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne}, privateUploaderRunDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(_ context.Context, prepared preparedUploadBatch, _ uint64) (uploadResponse, error) {
			sends++
			if !reflect.DeepEqual(prepared.eventIDs, []string{testEventIDOne}) {
				t.Fatalf("retained-root upload IDs = %v", prepared.eventIDs)
			}
			return uploadResponse{kind: uploadResponseAccepted, statusCode: 200}, nil
		}),
		beforeOperation: func(operation uploaderOperation) {
			if operation != uploaderOperationBeforePreSendRevalidation || replaced {
				return
			}
			replaced = true
			if err := os.Rename(home.Root(), movedRoot); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(home.Root(), 0o700); err != nil {
				t.Fatal(err)
			}
			writeStateFixture(t, home, activeEnabledStateForSpawnTest())
			rootB := mustOpenMutableRoot(t, home)
			eventB := testSpoolEvent(testEventIDTwo, permit.releaseVersion, testRecordHour, CommandVersion)
			dataB := writeSpoolEventFixture(t, rootB, queueDirectoryName, testSpoolGeneration, eventB)
			if err := persistSpoolQuota(rootB, spoolQuota{Events: 1, Bytes: uint64(len(dataB))}); err != nil {
				t.Fatal(err)
			}
			writeSpawnThrottleToRoot(t, rootB, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: testRecordHour})
			if err := rootB.Close(); err != nil {
				t.Fatal(err)
			}
		},
	})
	if err != nil || !replaced || sends != 1 {
		t.Fatalf("retained-root run = err:%v replaced:%v sends:%d", err, replaced, sends)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, testEventIDTwo)
	if _, err := os.Lstat(filepath.Join(movedRoot, queueDirectoryName, testSpoolGeneration, eventFileName(testEventIDOne))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("retained root still contains acknowledged event: %v", err)
	}
	assertSpawnThrottleRecord(t, home, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: testRecordHour})
}

func TestPrivateUploaderRequiresMarkerBeforeFilesystemOrNetwork(t *testing.T) {
	home := newMetricsTestHome(t)
	service := mustOpenTestService(t, spawnTestDependencies(home, time.Now, func() (string, error) { return testSpawnTokenOne, nil }))
	service.deps.getenv = func(string) string { return "" }
	sends := 0
	err := service.runPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne}, privateUploaderRunDependencies{
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			sends++
			return uploadResponse{}, nil
		}),
	})
	if err == nil || sends != 0 {
		t.Fatalf("missing-marker run = %v, sends=%d", err, sends)
	}
	if _, err := os.Lstat(home.Root()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing-marker run touched root: %v", err)
	}
}

func TestPrivateUploaderProductionNoWorkReturnsBeforeTransportConstruction(t *testing.T) {
	t.Setenv(privateUploaderMarkerEnvironment, privateUploaderMarkerValue)
	home := newMetricsTestHome(t)
	service, err := OpenProduction(ProductionOptions{
		Home:    home.Home(),
		Release: CurrentReleaseIdentity(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = service.RunPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne})
	if err != nil {
		t.Fatalf("marker-valid no-work production child = %v, want silent success", err)
	}
	if _, err := os.Lstat(home.Root()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no-work production child touched metrics root: %v", err)
	}
}

func TestPrivateUploaderEnabledEmptyQueueNeverConstructsProductionTransport(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	factoryCalls := 0
	startCalls := 0
	deps := spawnTestDependencies(home, func() time.Time { return testRecordHour }, func() (string, error) {
		return testSpawnTokenTwo, nil
	})
	deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	deps.privateUploaderStart = nil
	deps.privateUploaderStartFactory = func() (uploadStartFunc, error) {
		factoryCalls++
		return func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
			startCalls++
			return func() (uploadResponse, error) { return uploadResponse{}, nil }, nil
		}, nil
	}
	service := mustOpenTestService(t, deps)
	err := service.RunPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne})
	if err != nil || factoryCalls != 0 || startCalls != 0 {
		t.Fatalf("empty-queue private child = err:%v factory:%d starts:%d", err, factoryCalls, startCalls)
	}
}

func TestPrivateUploaderSupersededTokenIsSilentWithoutFactoryOrNetwork(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: testRecordHour})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	factoryCalls := 0
	startCalls := 0
	deps := spawnTestDependencies(home, func() time.Time { return testRecordHour }, func() (string, error) {
		return testSpawnTokenTwo, nil
	})
	deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	deps.privateUploaderStart = nil
	deps.privateUploaderStartFactory = func() (uploadStartFunc, error) {
		factoryCalls++
		return func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
			startCalls++
			return func() (uploadResponse, error) { return uploadResponse{}, nil }, nil
		}, nil
	}
	service := mustOpenTestService(t, deps)
	err := service.RunPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne})
	if err != nil || factoryCalls != 0 || startCalls != 0 {
		t.Fatalf("superseded private child = err:%v factory:%d starts:%d", err, factoryCalls, startCalls)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
}

func TestPrivateUploaderCorruptThrottleIsNotClassifiedAsOrdinaryStale(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := root.writeFileAtomic(spawnThrottleFileName, []byte("not = [toml")); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	deps := spawnTestDependencies(home, func() time.Time { return testRecordHour }, func() (string, error) {
		return testSpawnTokenTwo, nil
	})
	deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	deps.privateUploaderStart = func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
		t.Fatal("corrupt throttle reached network Start")
		return nil, nil
	}
	service := mustOpenTestService(t, deps)
	if err := service.RunPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne}); err == nil {
		t.Fatal("corrupt throttle was silently classified as an ordinary stale attempt")
	}
}

func TestPrivateUploaderThrottleReadFailureIsNotClassifiedAsOrdinaryStale(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	blockRead := false
	deps := spawnTestDependencies(home, func() time.Time { return testRecordHour }, func() (string, error) {
		return testSpawnTokenTwo, nil
	})
	deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	deps.storageHooks.beforeRead = func(path string) {
		if path == filepath.Join(home.Root(), spawnThrottleFileName) {
			blockRead = true
		}
	}
	deps.storageHooks.decisionGate = func() bool { return !blockRead }
	deps.privateUploaderStart = func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
		t.Fatal("throttle read failure reached network Start")
		return nil, nil
	}
	service := mustOpenTestService(t, deps)
	err := service.RunPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne})
	if !errors.Is(err, errRecordDecisionWindowExpired) || onlyStaleSpawnAttempt(err) {
		t.Fatalf("throttle read failure classification = %v, want non-stale I/O uncertainty", err)
	}
}

func TestSpawnAttemptClassificationSeparatesSafeStaleFromCorruptUncertainty(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, activeEnabledStateForSpawnTest())
	service := mustOpenTestService(t, spawnTestDependencies(home, func() time.Time { return testRecordHour }, func() (string, error) {
		return testSpawnTokenTwo, nil
	}))
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	locked, err := service.lockState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Close() }()

	if err := validateSpawnAttemptLocked(locked, testSpawnTokenOne, testRecordHour); !errors.Is(err, errStaleSpawnAttempt) {
		t.Fatalf("missing throttle classification = %v, want ordinary stale", err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: testRecordHour})
	if err := validateSpawnAttemptLocked(locked, testSpawnTokenOne, testRecordHour); !errors.Is(err, errStaleSpawnAttempt) {
		t.Fatalf("replaced throttle classification = %v, want ordinary stale", err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour.Add(time.Nanosecond)})
	if err := validateSpawnAttemptLocked(locked, testSpawnTokenOne, testRecordHour); !errors.Is(err, errStaleSpawnAttempt) {
		t.Fatalf("future throttle classification = %v, want ordinary stale", err)
	}
	if err := root.writeFileAtomic(spawnThrottleFileName, []byte("not = [toml")); err != nil {
		t.Fatal(err)
	}
	if err := validateSpawnAttemptLocked(locked, testSpawnTokenOne, testRecordHour); err == nil || onlyStaleSpawnAttempt(err) {
		t.Fatalf("corrupt throttle classification = %v, want non-stale uncertainty", err)
	}
}

func TestPrivateUploaderLosingUploaderLockPerformsZeroNetworkWork(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	service.deps.getenv = func(name string) string {
		if name == privateUploaderMarkerEnvironment {
			return privateUploaderMarkerValue
		}
		return ""
	}
	root := mustOpenMutableRoot(t, home)
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour})
	barrier, err := root.acquireLock(context.Background(), uploaderLockName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = barrier.Release(); _ = root.Close() }()
	sends := 0
	err = service.runPrivateUploader(context.Background(), PrivateUploaderInvocation{attemptToken: testSpawnTokenOne}, privateUploaderRunDependencies{
		uploaderLockWait: 20 * time.Millisecond,
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			sends++
			return uploadResponse{}, nil
		}),
	})
	if !errors.Is(err, context.DeadlineExceeded) || sends != 0 {
		t.Fatalf("losing child = %v, sends=%d", err, sends)
	}
}

func TestPurgeAndCleanProofRequireSpawnThrottleAbsent(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	writeSpawnThrottleToRoot(t, root, spawnThrottleRecord{
		attemptToken: testSpawnTokenOne,
		attemptedAt:  time.Date(2026, time.July, 12, 7, 0, 0, 0, time.UTC),
	})
	if err := proveCleanMetricsTree(root, defaultSpoolWorkBudget()); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("clean proof with throttle = %v, want state changed", err)
	}
	result, err := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
	if err != nil || !result.complete {
		t.Fatalf("purge with throttle = %+v, %v", result, err)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), spawnThrottleFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("purge left throttle: %v", err)
	}
	if err := proveCleanMetricsTree(root, defaultSpoolWorkBudget()); err != nil {
		t.Fatalf("clean proof after throttle purge: %v", err)
	}
}

func TestPurgeSpawnThrottlePreservesReplacementAtDeleteBoundary(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, disabledState(7, 2, cleanupDisable))
	plain := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(plain, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	first := spawnThrottleRecord{attemptToken: testSpawnTokenOne, attemptedAt: testRecordHour}
	second := spawnThrottleRecord{attemptToken: testSpawnTokenTwo, attemptedAt: testRecordHour.Add(time.Second)}
	writeSpawnThrottleToRoot(t, plain, first)
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home.Root(), spawnThrottleFileName)
	replaced := false
	root, err := openStorageRootMutableWithHooks(home, storageTestHooks{
		beforeMutation: func(step storageStep, observed string) {
			if replaced || step != storageStepDelete || observed != path {
				return
			}
			replaced = true
			data, encodeErr := encodeSpawnThrottle(second)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if removeErr := os.Remove(path); removeErr != nil {
				t.Fatal(removeErr)
			}
			if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, purgeErr := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
	if closeErr := root.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !replaced || purgeErr == nil || result.complete {
		t.Fatalf("replacement-boundary purge = %+v err=%v replaced=%v", result, purgeErr, replaced)
	}
	assertSpawnThrottleRecord(t, home, second)
}

func spawnTestDependencies(home gchome.ProductUsageHome, now func() time.Time, newUUID func() (string, error)) serviceDependencies {
	deps := defaultTestServiceDependencies(home, 2)
	deps.now = now
	deps.newUUID = newUUID
	return deps
}

func assertSpawnThrottleRecord(t *testing.T, home gchome.ProductUsageHome, want spawnThrottleRecord) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home.Root(), spawnThrottleFileName))
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSpawnThrottle(data)
	if err != nil || got != want {
		t.Fatalf("spawn throttle = %#v, %v; want %#v", got, err, want)
	}
}

func writeSpawnThrottleToRoot(t *testing.T, root *storageRoot, record spawnThrottleRecord) {
	t.Helper()
	data, err := encodeSpawnThrottle(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.writeFileAtomic(spawnThrottleFileName, data); err != nil {
		t.Fatal(err)
	}
}
