//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/unix"
)

func immediateUploadStart(send func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error)) uploadStartFunc {
	return func(ctx context.Context, prepared preparedUploadBatch, epoch uint64) (uploadWaitFunc, error) {
		return func() (uploadResponse, error) {
			return send(ctx, prepared, epoch)
		}, nil
	}
}

type deterministicDeadlineContext struct {
	context.Context
	deadline time.Time
	done     chan struct{}
	expired  atomic.Bool
	once     sync.Once
}

func newDeterministicDeadlineContext() *deterministicDeadlineContext {
	return &deterministicDeadlineContext{
		Context:  context.Background(),
		deadline: time.Now().Add(time.Hour),
		done:     make(chan struct{}),
	}
}

func (ctx *deterministicDeadlineContext) Deadline() (time.Time, bool) {
	return ctx.deadline, true
}

func (ctx *deterministicDeadlineContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *deterministicDeadlineContext) Err() error {
	if ctx.expired.Load() {
		return context.DeadlineExceeded
	}
	return nil
}

func (ctx *deterministicDeadlineContext) expire() {
	ctx.expired.Store(true)
	ctx.once.Do(func() { close(ctx.done) })
}

func TestUploaderAcceptedClaimsOldestReleaseAndDeletesDurably(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	oldRelease := testSpoolEvent(testEventIDOne, "0.9.0", testRecordHour, CommandHelp)
	currentRelease := testSpoolEvent(testEventIDTwo, permit.releaseVersion, testRecordHour, CommandVersion)
	oldBytes := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, oldRelease)
	currentBytes := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, currentRelease)
	oldPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(oldRelease.EventID))
	currentPath := filepath.Join(home.Root(), queueDirectoryName, testSpoolGeneration, eventFileName(currentRelease.EventID))
	oldTime := testRecordHour.Add(-2 * time.Hour)
	currentTime := testRecordHour.Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(currentPath, currentTime, currentTime); err != nil {
		t.Fatal(err)
	}
	if err := persistSpoolQuota(root, spoolQuota{Events: 2, Bytes: uint64(len(oldBytes) + len(currentBytes))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	sends := 0
	var captured preparedUploadBatch
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(_ context.Context, prepared preparedUploadBatch, epoch uint64) (uploadResponse, error) {
			sends++
			captured = clonePreparedUploadBatch(prepared)
			if epoch != permit.metricsEpoch {
				t.Fatalf("upload epoch = %d, want %d", epoch, permit.metricsEpoch)
			}
			return uploadResponse{kind: uploadResponseAccepted, statusCode: 200}, nil
		}),
	})
	if err != nil {
		t.Fatalf("uploadOneBatch: %v", err)
	}
	if sends != 1 || result.outcome != uploadRunDeleted || result.events != 1 {
		t.Fatalf("upload result = %+v sends=%d", result, sends)
	}
	if len(captured.eventIDs) != 1 || captured.eventIDs[0] != oldRelease.EventID ||
		captured.releaseVersion != oldRelease.ReleaseVersion || captured.installationID != permit.installationID {
		t.Fatalf("captured oldest-release batch = %+v", captured)
	}

	root = mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{Events: 1, Bytes: uint64(len(currentBytes))}) {
		t.Fatalf("quota after exact acknowledgement = %+v", got)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, currentRelease.EventID)
	if _, err := os.Lstat(oldPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("acknowledged old-release event remains queued: %v", err)
	}
}

func TestUploaderRetryRestoreCrashReplaysInflightOnNextAttempt(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	var failRestore atomic.Bool
	service.deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepRename && failRestore.Load() {
			return errors.New("injected restore crash")
		}
		return nil
	}
	firstSends := 0
	first, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			firstSends++
			return uploadResponse{kind: uploadResponseRetry, statusCode: 503}, nil
		}),
		beforeOperation: func(operation uploaderOperation) {
			if operation == uploaderOperationAfterSend {
				failRestore.Store(true)
			}
		},
	})
	if err == nil || firstSends != 1 || first.outcome != uploadRunRestored || first.events != 1 {
		t.Fatalf("restore-crash result = %+v sends=%d err=%v", first, firstSends, err)
	}
	assertSpoolFileLocation(t, home, inflightDirectoryName, event.EventID)

	failRestore.Store(false)
	secondSends := 0
	second, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			secondSends++
			return uploadResponse{kind: uploadResponseDuplicate, statusCode: 409}, nil
		}),
	})
	if err != nil || secondSends != 1 || second.outcome != uploadRunDeleted || second.events != 1 {
		t.Fatalf("replayed upload result = %+v sends=%d err=%v", second, secondSends, err)
	}
	root = mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{}) {
		t.Fatalf("quota after replayed duplicate acknowledgement = %+v", quota)
	}
}

func TestUploaderStaleBeforeSendDoesNotNetworkOrRestore(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	var token cleanupToken
	sends := 0
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			sends++
			return uploadResponse{kind: uploadResponseAccepted}, nil
		}),
		beforeOperation: func(operation uploaderOperation) {
			if operation != uploaderOperationBeforePreSendRevalidation {
				return
			}
			var disableErr error
			token, disableErr = service.beginDisable(context.Background(), testStateVersion(7))
			if disableErr != nil {
				t.Fatalf("disable before pre-send revalidation: %v", disableErr)
			}
		},
	})
	defer func() { _ = token.Close() }()
	if !errors.Is(err, ErrStateChangedConcurrently) || sends != 0 || result.outcome != uploadRunStale || result.events != 1 {
		t.Fatalf("stale pre-send result = %+v sends=%d err=%v", result, sends, err)
	}
	assertSpoolFileLocation(t, home, inflightDirectoryName, event.EventID)
}

func TestUploaderDisableInPostRevalidationGapSuppressesSendStart(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	var token cleanupToken
	starts := 0
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
			starts++
			return func() (uploadResponse, error) {
				return uploadResponse{kind: uploadResponseAccepted, statusCode: 200}, nil
			}, nil
		},
		beforeOperation: func(operation uploaderOperation) {
			if operation != uploaderOperation("before-send-start") {
				return
			}
			var disableErr error
			token, disableErr = service.beginDisable(context.Background(), testStateVersion(7))
			if disableErr != nil {
				t.Fatalf("disable in post-revalidation gap: %v", disableErr)
			}
		},
	})
	defer func() { _ = token.Close() }()
	if !errors.Is(err, ErrStateChangedConcurrently) || starts != 0 ||
		result.outcome != uploadRunStale || result.events != 1 {
		t.Fatalf("post-revalidation disable result = %+v starts=%d err=%v", result, starts, err)
	}
	state := readStateFixture(t, home)
	if state.Preference != preferenceDisabled || state.CleanupKind != cleanupDisable {
		t.Fatalf("post-revalidation disable state = %#v", state)
	}
	assertSpoolFileLocation(t, home, inflightDirectoryName, event.EventID)
}

func TestUploaderSendStartRunsUnderFinalStateLockAndWaitRunsOutside(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	probeRoot := mustOpenMutableRoot(t, home)
	defer func() { _ = probeRoot.Close() }()
	var startStateErr, waitStateErr, waitUploaderErr error
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
			startContext, cancelStart := context.WithTimeout(context.Background(), 250*time.Millisecond)
			startState, lockErr := service.lockState(startContext, probeRoot)
			cancelStart()
			startStateErr = lockErr
			if startState != nil {
				_ = startState.Close()
			}

			return func() (uploadResponse, error) {
				waitContext, cancelWait := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
				waitState, lockErr := service.lockState(waitContext, probeRoot)
				cancelWait()
				waitStateErr = lockErr
				if waitState != nil {
					_ = waitState.Close()
				}

				uploaderContext, cancelUploader := context.WithTimeout(context.Background(), 250*time.Millisecond)
				probeUploader, lockErr := service.lockUploader(uploaderContext, probeRoot)
				cancelUploader()
				waitUploaderErr = lockErr
				if probeUploader != nil {
					_ = probeUploader.Close()
				}
				return uploadResponse{kind: uploadResponseRetry, statusCode: 503}, nil
			}, nil
		},
	})
	if err != nil || result.outcome != uploadRunRestored || result.events != 1 {
		t.Fatalf("upload settlement = %+v err=%v", result, err)
	}
	if !errors.Is(startStateErr, context.DeadlineExceeded) {
		t.Fatalf("send start observed state lock released: %v", startStateErr)
	}
	if waitStateErr != nil {
		t.Fatalf("send wait observed state lock held: %v", waitStateErr)
	}
	if !errors.Is(waitUploaderErr, context.DeadlineExceeded) {
		t.Fatalf("send wait observed uploader lock released: %v", waitUploaderErr)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
}

func TestUploaderTerminalCallerContextDoesNotSkipSettlement(t *testing.T) {
	waitFailure := errors.New("injected upload wait failure")
	contextCases := []struct {
		name        string
		newContext  func() (context.Context, func())
		terminalErr error
	}{
		{
			name: "canceled",
			newContext: func() (context.Context, func()) {
				ctx, cancel := context.WithCancel(context.Background())
				return ctx, cancel
			},
			terminalErr: context.Canceled,
		},
		{
			name: "deadline-exceeded",
			newContext: func() (context.Context, func()) {
				ctx := newDeterministicDeadlineContext()
				return ctx, ctx.expire
			},
			terminalErr: context.DeadlineExceeded,
		},
	}
	responseCases := []struct {
		name        string
		response    func(preparedUploadBatch, uint64) uploadResponse
		waitErr     error
		terminalErr bool
		wantOutcome uploadRunOutcome
		wantQueue   bool
		wantPause   bool
	}{
		{
			name: "accepted",
			response: func(preparedUploadBatch, uint64) uploadResponse {
				return uploadResponse{kind: uploadResponseAccepted, statusCode: 200}
			},
			wantOutcome: uploadRunDeleted,
		},
		{
			name: "retry",
			response: func(preparedUploadBatch, uint64) uploadResponse {
				return uploadResponse{kind: uploadResponseRetry, statusCode: 503}
			},
			terminalErr: true,
			wantOutcome: uploadRunRestored,
			wantQueue:   true,
		},
		{
			name: "signed-pause",
			response: func(prepared preparedUploadBatch, epoch uint64) uploadResponse {
				return uploadResponse{kind: uploadResponsePause, statusCode: 410, pause: verifiedPause{
					releaseVersion: prepared.releaseVersion,
					metricsEpoch:   epoch,
					keyID:          "test-key",
				}}
			},
			wantOutcome: uploadRunPaused,
			wantPause:   true,
		},
		{
			name: "wait-error",
			response: func(preparedUploadBatch, uint64) uploadResponse {
				return uploadResponse{kind: uploadResponseAccepted, statusCode: 200}
			},
			waitErr:     waitFailure,
			wantOutcome: uploadRunRestored,
			wantQueue:   true,
		},
	}

	for _, contextCase := range contextCases {
		for _, responseCase := range responseCases {
			t.Run(contextCase.name+"/"+responseCase.name, func(t *testing.T) {
				home, service, permit := newRecordServiceFixture(t, testEventIDThree)
				root := mustOpenMutableRoot(t, home)
				event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
				data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
				if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
					t.Fatal(err)
				}
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}

				ctx, terminate := contextCase.newContext()
				wantWaitErr := responseCase.waitErr
				if responseCase.terminalErr {
					wantWaitErr = contextCase.terminalErr
				}
				result, err := service.uploadOneBatch(ctx, uploaderDependencies{
					now: func() time.Time { return testRecordHour },
					start: func(_ context.Context, prepared preparedUploadBatch, epoch uint64) (uploadWaitFunc, error) {
						return func() (uploadResponse, error) {
							terminate()
							if !errors.Is(ctx.Err(), contextCase.terminalErr) {
								t.Fatalf("terminal caller context error = %v, want %v", ctx.Err(), contextCase.terminalErr)
							}
							return responseCase.response(prepared, epoch), wantWaitErr
						}, nil
					},
				})
				if wantWaitErr == nil && err != nil {
					t.Fatalf("terminal caller settlement = %+v err=%v", result, err)
				}
				if wantWaitErr != nil && !errors.Is(err, wantWaitErr) {
					t.Fatalf("terminal caller wait error = %v, want %v", err, wantWaitErr)
				}
				if result.outcome != responseCase.wantOutcome || result.events != 1 {
					t.Fatalf("terminal caller settlement = %+v, want outcome %v with one event", result, responseCase.wantOutcome)
				}

				root = mustOpenMutableRoot(t, home)
				defer func() { _ = root.Close() }()
				wantQuota := spoolQuota{}
				if responseCase.wantQueue {
					wantQuota = spoolQuota{Events: 1, Bytes: uint64(len(data))}
					assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
				} else {
					for _, tree := range []string{queueDirectoryName, inflightDirectoryName} {
						path := filepath.Join(home.Root(), tree, testSpoolGeneration, eventFileName(event.EventID))
						if _, statErr := os.Lstat(path); !errors.Is(statErr, fs.ErrNotExist) {
							t.Fatalf("terminal caller settlement retained %s event: %v", tree, statErr)
						}
					}
				}
				if quota := readQuotaFromRoot(t, root); quota != wantQuota {
					t.Fatalf("terminal caller settlement quota = %+v, want %+v", quota, wantQuota)
				}
				if responseCase.wantPause {
					state := readStateFixture(t, home)
					if state.Preference != preferenceEnabled || state.CleanupKind != cleanupNone ||
						state.SpoolGeneration != "" || state.PausedThroughMetricsEpoch != permit.metricsEpoch {
						t.Fatalf("terminal caller signed-pause state = %#v", state)
					}
				}
			})
		}
	}
}

func TestUploaderStartFailureRestoresWithoutWaiting(t *testing.T) {
	startFailure := errors.New("injected upload start failure")
	for _, testCase := range []struct {
		name      string
		nilWait   bool
		wantError error
	}{
		{
			name:      "start-error",
			wantError: startFailure,
		},
		{
			name:    "nil-wait",
			nilWait: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home, service, permit := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			quota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
			if err := persistSpoolQuota(root, quota); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}

			waits := 0
			result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
				now: func() time.Time { return testRecordHour },
				start: func(context.Context, preparedUploadBatch, uint64) (uploadWaitFunc, error) {
					if testCase.nilWait {
						return nil, nil
					}
					return func() (uploadResponse, error) {
						waits++
						return uploadResponse{kind: uploadResponseAccepted}, nil
					}, startFailure
				},
			})
			if err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) ||
				result.outcome != uploadRunRestored || result.events != 1 || waits != 0 {
				t.Fatalf("failed upload start result = %+v waits=%d err=%v", result, waits, err)
			}
			assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
			root = mustOpenMutableRoot(t, home)
			defer func() { _ = root.Close() }()
			if got := readQuotaFromRoot(t, root); got != quota {
				t.Fatalf("failed upload start quota = %+v, want %+v", got, quota)
			}
		})
	}
}

func TestUploaderStaleAcceptedResponseDoesNotDeleteOrRestore(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	var token cleanupToken
	sends := 0
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			sends++
			var disableErr error
			token, disableErr = service.beginDisable(context.Background(), testStateVersion(7))
			if disableErr != nil {
				t.Fatalf("disable during network request: %v", disableErr)
			}
			return uploadResponse{kind: uploadResponseAccepted, statusCode: 200}, nil
		}),
	})
	defer func() { _ = token.Close() }()
	if !errors.Is(err, ErrStateChangedConcurrently) || sends != 1 || result.outcome != uploadRunStale || result.events != 1 {
		t.Fatalf("stale response result = %+v sends=%d err=%v", result, sends, err)
	}
	assertSpoolFileLocation(t, home, inflightDirectoryName, event.EventID)
	root = mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{Events: 1, Bytes: uint64(len(data))}) {
		t.Fatalf("stale accepted response changed quota: %+v", quota)
	}
}

func TestUploaderEnvironmentDisableBeforeSendSuppressesNetwork(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	var disabled atomic.Bool
	service.deps.getenv = func(name string) string {
		if name == envDisableUsageMetrics && disabled.Load() {
			return "1"
		}
		return ""
	}
	sends := 0
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			sends++
			return uploadResponse{kind: uploadResponseAccepted}, nil
		}),
		beforeOperation: func(operation uploaderOperation) {
			if operation == uploaderOperationBeforePreSendRevalidation {
				disabled.Store(true)
			}
		},
	})
	if !errors.Is(err, ErrStateChangedConcurrently) || sends != 0 || result.outcome != uploadRunStale {
		t.Fatalf("environment-disabled upload result = %+v sends=%d err=%v", result, sends, err)
	}
	assertSpoolFileLocation(t, home, inflightDirectoryName, event.EventID)
}

func TestUploaderSenderErrorAlwaysRestoresContradictoryDestructiveResponse(t *testing.T) {
	for _, response := range []uploadResponse{
		{kind: uploadResponseAccepted, statusCode: 200},
		{kind: uploadResponseDuplicate, statusCode: 409},
		{kind: uploadResponsePause, statusCode: 410, pause: verifiedPause{releaseVersion: "1.0.0", metricsEpoch: 2}},
	} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{name: "ambiguous", err: errors.New("ambiguous sender failure")},
			{name: "context-canceled", err: context.Canceled},
			{name: "deadline-exceeded", err: context.DeadlineExceeded},
		} {
			t.Run(fmt.Sprintf("kind-%d/%s", response.kind, failure.name), func(t *testing.T) {
				home, service, permit := newRecordServiceFixture(t, testEventIDThree)
				root := mustOpenMutableRoot(t, home)
				event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
				data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
				if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
					t.Fatal(err)
				}
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}

				sends := 0
				result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
					now: func() time.Time { return testRecordHour },
					start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
						sends++
						return response, failure.err
					}),
				})
				if !errors.Is(err, failure.err) || sends != 1 || result.outcome != uploadRunRestored || result.events != 1 {
					t.Fatalf("contradictory sender result = %+v sends=%d err=%v", result, sends, err)
				}
				assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
				root = mustOpenMutableRoot(t, home)
				defer func() { _ = root.Close() }()
				if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{Events: 1, Bytes: uint64(len(data))}) {
					t.Fatalf("contradictory sender response changed quota: %+v", quota)
				}
			})
		}
	}
}

func TestUploaderIneligibleAbsentStateDoesNotCreateMetricsRoot(t *testing.T) {
	home := newMetricsTestHome(t)
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	if _, err := os.Lstat(home.Root()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("test root exists before upload preflight: %v", err)
	}
	sends := 0
	_, _ = service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			sends++
			return uploadResponse{kind: uploadResponseAccepted}, nil
		}),
	})
	if sends != 0 {
		t.Fatalf("absent ineligible state made %d network attempts", sends)
	}
	if _, err := os.Lstat(home.Root()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ineligible upload created the metrics root: %v", err)
	}
}

func TestUploaderSignedPausePersistsBarrierAndCompletesBoundedLocalCleanup(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "0.9.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	rootTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa12)))
	writeJournaledRootTempCrashFixture(t, root, filepath.Base(rootTemp),
		[]byte("installation_id = \""+testInstallationID+"\"\n"), 0)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	barrierObservedBeforeDelete := false
	pauseReceived := false
	journalPath := filepath.Join(home.Root(), rootTempJournalDirectoryName)
	service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
		if !pauseReceived || (step != storageStepDelete && step != storageStepUnlink && step != storageStepRmdir) {
			return
		}
		if path == journalPath || strings.HasPrefix(path, journalPath+string(os.PathSeparator)) {
			return
		}
		state := readStateFixture(t, home)
		if state.CleanupKind != cleanupPause || state.SpoolGeneration != "" ||
			state.PausedThroughMetricsEpoch != permit.metricsEpoch {
			t.Fatalf("destructive cleanup ran before pause barrier: %#v", state)
		}
		barrierObservedBeforeDelete = true
	}
	sends := 0
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(_ context.Context, prepared preparedUploadBatch, epoch uint64) (uploadResponse, error) {
			sends++
			pauseReceived = true
			return uploadResponse{kind: uploadResponsePause, statusCode: 410, pause: verifiedPause{
				releaseVersion: prepared.releaseVersion,
				metricsEpoch:   epoch,
				keyID:          "test-key",
			}}, nil
		}),
	})
	if err != nil || sends != 1 || result.outcome != uploadRunPaused || !barrierObservedBeforeDelete {
		t.Fatalf("initial signed-pause result = %+v sends=%d barrier=%v err=%v", result, sends, barrierObservedBeforeDelete, err)
	}
	clean := readStateFixture(t, home)
	if clean.CleanupKind != cleanupNone || clean.Preference != preferenceEnabled || clean.SpoolGeneration != "" ||
		clean.InstallationID != testInstallationID || clean.PausedThroughMetricsEpoch != permit.metricsEpoch {
		t.Fatalf("clean paused state = %#v", clean)
	}
	if _, err := os.Lstat(rootTemp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("clean pause left root temp containing identity: %v", err)
	}
	root = mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{}) {
		t.Fatalf("clean pause quota = %+v", quota)
	}
	for _, name := range []string{spoolControlDirectoryName, retiredControlDirectoryName, fallbackRelocationCursorName} {
		if _, err := root.lookupEntry(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("clean pause control %q remains: %v", name, err)
		}
	}
}

func TestSignedPauseDiagnosticWritePrecedesCleanupSuccessor(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	defer func() { _ = permit.Close() }()
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	pauseReceived := false
	var pauseWrites []string
	service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
		if !pauseReceived || step != storageStepRename {
			return
		}
		switch name := filepath.Base(path); name {
		case configFileName, statusFileName:
			pauseWrites = append(pauseWrites, name)
		}
	}
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(_ context.Context, prepared preparedUploadBatch, epoch uint64) (uploadResponse, error) {
			pauseReceived = true
			return uploadResponse{kind: uploadResponsePause, statusCode: 410, pause: verifiedPause{
				releaseVersion: prepared.releaseVersion,
				metricsEpoch:   epoch,
				keyID:          "test-key",
			}}, nil
		}),
	})
	if err != nil || result.outcome != uploadRunPaused {
		t.Fatalf("signed pause = %+v err=%v", result, err)
	}
	paused := readStateFixture(t, home)
	if paused.CleanupKind != cleanupNone || paused.PausedThroughMetricsEpoch != permit.metricsEpoch || paused.SpoolGeneration != "" {
		t.Fatalf("signed-pause successor = %#v", paused)
	}
	statusIndex, successorIndex := -1, -1
	for index, name := range pauseWrites {
		switch name {
		case statusFileName:
			if statusIndex < 0 {
				statusIndex = index
			}
		case configFileName:
			successorIndex = index
		}
	}
	if statusIndex < 0 || successorIndex < 0 || statusIndex >= successorIndex {
		t.Fatalf("post-barrier writes = %v; diagnostic status must precede the clean pause successor", pauseWrites)
	}
}

func TestUploaderSignedPauseBudgetExhaustionReplaysLocallyWithoutNetwork(t *testing.T) {
	home, service, permit := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	quota := spoolQuota{Events: 1, Bytes: uint64(len(data))}
	if err := persistSpoolQuota(root, quota); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	limited := defaultSpoolWorkBudget()
	limited.maxEntries = 2*spoolFixedEntryEnvelope + 64
	limited.maxNameBytes = 2*spoolFixedNameEnvelope + 4096
	limited.maxReadBytes = 2*spoolFixedReadEnvelope + maximumEventBytes

	sends := 0
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now:    func() time.Time { return testRecordHour },
		budget: limited,
		start: immediateUploadStart(func(_ context.Context, prepared preparedUploadBatch, epoch uint64) (uploadResponse, error) {
			sends++
			return uploadResponse{kind: uploadResponsePause, statusCode: 410, pause: verifiedPause{
				releaseVersion: prepared.releaseVersion, metricsEpoch: epoch, keyID: "test-key",
			}}, nil
		}),
	})
	if err != nil || sends != 1 || result.outcome != uploadRunPausePending || result.events != 1 {
		t.Fatalf("budget-limited signed pause = %+v sends=%d err=%v", result, sends, err)
	}
	pending := readStateFixture(t, home)
	if pending.Preference != preferenceEnabled || pending.CleanupKind != cleanupPause ||
		pending.InstallationID != testInstallationID || pending.SpoolGeneration != "" ||
		pending.PausedThroughMetricsEpoch != permit.metricsEpoch {
		t.Fatalf("budget-limited pause state = %#v", pending)
	}

	tiny := spoolWorkBudget{maxEntries: 1, maxDirectories: 1, maxReadBytes: 1, maxNameBytes: maximumStorageNameBytes}
	local, localErr := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now:    func() time.Time { return testRecordHour },
		budget: tiny,
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			t.Fatal("pending pause cleanup attempted network")
			return uploadResponse{}, nil
		}),
	})
	if localErr != nil || local.outcome != uploadRunPausePending || sends != 1 {
		t.Fatalf("tiny local replay = %+v sends=%d err=%v", local, sends, localErr)
	}
	if after := readStateFixture(t, home); after != pending {
		t.Fatalf("incomplete local replay changed pause owner:\nbefore=%#v\nafter=%#v", pending, after)
	}

	complete, completeErr := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
			t.Fatal("adequate pause cleanup attempted network")
			return uploadResponse{}, nil
		}),
	})
	if completeErr != nil || complete.outcome != uploadRunPaused || sends != 1 {
		t.Fatalf("adequate local replay = %+v sends=%d err=%v", complete, sends, completeErr)
	}
	clean := readStateFixture(t, home)
	if clean.CleanupKind != cleanupNone || clean.Preference != preferenceEnabled || clean.SpoolGeneration != "" ||
		clean.InstallationID != testInstallationID || clean.PausedThroughMetricsEpoch != permit.metricsEpoch {
		t.Fatalf("clean paused state = %#v", clean)
	}
	root = mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if got := readQuotaFromRoot(t, root); got != (spoolQuota{}) {
		t.Fatalf("clean paused quota = %+v", got)
	}
}

func TestGreaterEpochResumeFinishesPauseCleanupBeforeTransitionPermit(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(9, 2, testInstallationID, "")
	paused.CleanupKind = cleanupPause
	paused.CleanupEpoch = 3
	paused.PausedThroughMetricsEpoch = 1
	writeStateFixture(t, home, paused)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	resumeGeneration := "45454545-4545-4545-8545-454545454545"
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, resumeGeneration)
	deps.now = func() time.Time { return testRecordHour }
	service := mustOpenTestService(t, deps)
	transitioned := false
	for attempts := 0; attempts < 16; attempts++ {
		permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
		state := readStateFixture(t, home)
		if state.SpoolGeneration == resumeGeneration {
			if permit.Valid() {
				t.Fatal("greater-epoch cleanup/resume transition invocation received a permit")
			}
			transitioned = true
			break
		}
		if permit.Valid() {
			t.Fatalf("pause-cleanup invocation received a permit before resume: %#v", permit)
		}
	}
	if !transitioned {
		t.Fatal("greater-epoch cleanup/resume did not converge")
	}
	resumed := readStateFixture(t, home)
	if resumed.CleanupKind != cleanupNone || resumed.PausedThroughMetricsEpoch != 1 ||
		resumed.SpoolGeneration != resumeGeneration || resumed.InstallationID != testInstallationID {
		t.Fatalf("resumed state = %#v", resumed)
	}
	next := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	defer func() { _ = next.Close() }()
	if !next.Valid() {
		t.Fatal("invocation after greater-epoch cleanup/resume has no permit")
	}
}

func TestPauseBarrierShortCircuitReturnsCleanupTokenCloseError(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(9, 2, testInstallationID, "")
	paused.CleanupKind = cleanupPause
	paused.CleanupEpoch = 3
	paused.PausedThroughMetricsEpoch = 2
	writeStateFixture(t, home, paused)

	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	closedRetainedConfig := false
	var injectionErr error
	service.deps.storageHooks.afterRead = func(path string, _, read int, readErr error) {
		if closedRetainedConfig || filepath.Base(path) != configFileName || read != 0 || readErr != nil {
			return
		}
		closedRetainedConfig = true
		injectionErr = closeOpenFileMatchingPath(path)
	}

	transitioned, err := service.finishPauseCleanupAndResume(context.Background())
	if !closedRetainedConfig || injectionErr != nil {
		t.Fatalf("close retained cleanup-token record: attempted=%v err=%v", closedRetainedConfig, injectionErr)
	}
	if transitioned {
		t.Fatal("release at the pause barrier transitioned state")
	}
	if !errors.Is(err, unix.EBADF) {
		t.Fatalf("pause-barrier cleanup-token close error = %v, want EBADF", err)
	}
	if after := readStateFixture(t, home); after != paused {
		t.Fatalf("pause-barrier close failure changed state:\nbefore=%#v\nafter=%#v", paused, after)
	}
}

func closeOpenFileMatchingPath(path string) error {
	var target unix.Stat_t
	if err := unix.Stat(path, &target); err != nil {
		return fmt.Errorf("stat target: %w", err)
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("read descriptor limit: %w", err)
	}
	maximum := limit.Cur
	if maximum > 1<<16 {
		maximum = 1 << 16
	}
	for descriptor := 0; uint64(descriptor) < maximum; descriptor++ {
		var opened unix.Stat_t
		if err := unix.Fstat(descriptor, &opened); err != nil {
			continue
		}
		if opened.Dev == target.Dev && opened.Ino == target.Ino {
			return unix.Close(descriptor)
		}
	}
	return errors.New("matching open descriptor was not found")
}

func TestGreaterEpochResumeReprovesJournalAfterPauseSuccessorProofFailure(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(9, 2, testInstallationID, "")
	paused.CleanupKind = cleanupPause
	paused.CleanupEpoch = 3
	paused.PausedThroughMetricsEpoch = 1
	writeStateFixture(t, home, paused)

	injected := errors.New("injected persistent pause-successor journal proof failure")
	var armed atomic.Bool
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, "45454545-4545-4545-8545-454545454545")
	deps.storageHooks.beforeStep = func(step storageStep) error {
		if armed.Load() && step == storageStepEnumerate {
			return injected
		}
		return nil
	}
	service := mustOpenTestService(t, deps)
	root, err := openStorageRootMutableWithHooks(home, deps.storageHooks)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	uploader, err := service.lockUploader(context.Background(), root)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	state, err := uploader.lockState(context.Background(), service)
	if err != nil {
		_ = uploader.Close()
		_ = root.Close()
		t.Fatal(err)
	}
	token, _, pending, err := service.pauseCleanupLocked(state)
	if err != nil || !pending {
		_ = state.Close()
		_ = uploader.Close()
		_ = root.Close()
		t.Fatalf("load pause-cleanup authority: pending=%v err=%v", pending, err)
	}
	sweep, err := purgeSpoolWithinBudget(root, defaultSpoolWorkBudget())
	if err != nil || !sweep.complete {
		_ = token.Close()
		_ = state.Close()
		_ = uploader.Close()
		_ = root.Close()
		t.Fatalf("prepare empty pause cleanup = %+v err=%v", sweep, err)
	}
	armed.Store(true)
	proofErr := service.completeCleanupLockedWithJournalProof(state, token, sweep.meter)
	if !errors.Is(proofErr, injected) {
		t.Fatalf("pause successor proof error = %v, want injected failure", proofErr)
	}
	if closeErr := errors.Join(token.Close(), state.Close(), uploader.Close(), root.Close()); closeErr != nil {
		t.Fatal(closeErr)
	}
	visible := readStateFixture(t, home)
	if visible.CleanupKind != cleanupNone || visible.SpoolGeneration != "" || visible.PausedThroughMetricsEpoch != 1 {
		t.Fatalf("failed post-successor proof state = %#v", visible)
	}

	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	defer func() { _ = permit.Close() }()
	if permit.Valid() {
		t.Fatal("failed pause-successor proof produced a recording permit")
	}
	after := readStateFixture(t, home)
	if after.SpoolGeneration != "" || after != visible {
		t.Fatalf("future epoch bypassed failed pause-successor proof:\nvisible=%#v\nafter=%#v", visible, after)
	}
}

func TestGreaterEpochResumeRejectsUnprovenPauseSuccessorTree(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(10, 2, testInstallationID, "")
	paused.PausedThroughMetricsEpoch = 1
	writeStateFixture(t, home, paused)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, "56565656-5656-4656-8656-565656565656")
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocationAt(testRecordHour))
	defer func() { _ = permit.Close() }()
	if permit.Valid() {
		t.Fatal("residual pause-successor tree produced a recording permit")
	}
	if after := readStateFixture(t, home); after != paused {
		t.Fatalf("unproven pause-successor tree resumed:\nwant=%#v\nafter=%#v", paused, after)
	}
	assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
}

func TestUploaderRejectsMismatchedSignedPauseWithoutStateMutation(t *testing.T) {
	for _, mismatch := range []string{"release", "epoch"} {
		t.Run(mismatch, func(t *testing.T) {
			home, service, permit := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, "0.9.0", testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			before := readStateFixture(t, home)

			result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
				now: func() time.Time { return testRecordHour },
				start: immediateUploadStart(func(_ context.Context, prepared preparedUploadBatch, epoch uint64) (uploadResponse, error) {
					pause := verifiedPause{releaseVersion: prepared.releaseVersion, metricsEpoch: epoch, keyID: "test-key"}
					if mismatch == "release" {
						pause.releaseVersion = permit.releaseVersion
					}
					if mismatch == "epoch" {
						pause.metricsEpoch++
					}
					return uploadResponse{kind: uploadResponsePause, statusCode: 410, pause: pause}, nil
				}),
			})
			if err == nil || result.outcome != uploadRunRestored || result.events != 1 {
				t.Fatalf("mismatched pause result = %+v err=%v", result, err)
			}
			if after := readStateFixture(t, home); after != before {
				t.Fatalf("mismatched pause mutated state:\nbefore=%#v\nafter=%#v", before, after)
			}
			assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
		})
	}
}

func TestUploaderPauseCommitFailureDoesNotStartSpoolPurge(t *testing.T) {
	for _, failure := range []string{"not-applied", "applied-sync-pending"} {
		t.Run(failure, func(t *testing.T) {
			home, service, permit := newRecordServiceFixture(t, testEventIDThree)
			root := mustOpenMutableRoot(t, home)
			event := testSpoolEvent(testEventIDOne, permit.releaseVersion, testRecordHour, CommandHelp)
			data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
			if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}

			pauseReturned := false
			renameSeen := false
			failureInjected := false
			spoolDestruction := 0
			service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
				if pauseReturned && (step == storageStepDelete || step == storageStepUnlink || step == storageStepRmdir) &&
					(strings.Contains(path, queueDirectoryName) || strings.Contains(path, inflightDirectoryName)) {
					spoolDestruction++
				}
			}
			service.deps.storageHooks.beforeStep = func(step storageStep) error {
				if !pauseReturned || failureInjected {
					return nil
				}
				switch failure {
				case "not-applied":
					if step == storageStepRename {
						failureInjected = true
						return errors.New("injected pause rename failure")
					}
				case "applied-sync-pending":
					if step == storageStepRename {
						renameSeen = true
					}
					if renameSeen && step == storageStepDirectorySync {
						failureInjected = true
						return errors.New("injected pause parent-sync failure")
					}
				}
				return nil
			}
			result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
				now: func() time.Time { return testRecordHour },
				start: immediateUploadStart(func(_ context.Context, prepared preparedUploadBatch, epoch uint64) (uploadResponse, error) {
					pauseReturned = true
					return uploadResponse{kind: uploadResponsePause, statusCode: 410, pause: verifiedPause{
						releaseVersion: prepared.releaseVersion, metricsEpoch: epoch, keyID: "test-key",
					}}, nil
				}),
			})
			if err == nil || !failureInjected || spoolDestruction != 0 {
				t.Fatalf("pause commit failure = result:%+v injected:%v destruction:%d err:%v", result, failureInjected, spoolDestruction, err)
			}
			wantOutcome := uploadRunStale
			if failure == "not-applied" {
				wantOutcome = uploadRunRestored
			}
			if result.outcome != wantOutcome || result.events != 1 {
				t.Fatalf("pause commit failure outcome = %+v, want outcome %v with one event", result, wantOutcome)
			}
			state := readStateFixture(t, home)
			switch failure {
			case "not-applied":
				if state.CleanupKind != cleanupNone || state.SpoolGeneration != testSpoolGeneration {
					t.Fatalf("not-applied pause state = %#v", state)
				}
				assertSpoolFileLocation(t, home, queueDirectoryName, event.EventID)
			case "applied-sync-pending":
				if !errors.Is(err, errStateAppliedSyncPending) || state.CleanupKind != cleanupPause || state.SpoolGeneration != "" {
					t.Fatalf("sync-pending pause state = %#v err=%v", state, err)
				}
				assertSpoolFileLocation(t, home, inflightDirectoryName, event.EventID)
				spoolDestruction = 0
				service.deps.storageHooks.beforeStep = func(step storageStep) error {
					if step == storageStepDirectorySync {
						return errors.New("pause barrier remains unsynced")
					}
					return nil
				}
				service.deps.storageHooks.beforeMutation = func(step storageStep, path string) {
					if (step == storageStepDelete || step == storageStepUnlink || step == storageStepRmdir) &&
						(strings.Contains(path, queueDirectoryName) || strings.Contains(path, inflightDirectoryName)) {
						spoolDestruction++
					}
				}
				replay, replayErr := service.uploadOneBatch(context.Background(), uploaderDependencies{
					now: func() time.Time { return testRecordHour },
					start: immediateUploadStart(func(context.Context, preparedUploadBatch, uint64) (uploadResponse, error) {
						t.Fatal("unsynced pause replay attempted network")
						return uploadResponse{}, nil
					}),
				})
				if replayErr == nil || spoolDestruction != 0 {
					t.Fatalf("unsynced pause replay = result:%+v destruction:%d err:%v", replay, spoolDestruction, replayErr)
				}
				if replayState := readStateFixture(t, home); replayState.CleanupKind != cleanupPause {
					t.Fatalf("unsynced pause replay cleared barrier: %#v", replayState)
				}
			}
		})
	}
}

func TestUploaderStaleSignedPauseResponseDoesNotMutateOrSettle(t *testing.T) {
	home, service, _ := newRecordServiceFixture(t, testEventIDThree)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "0.9.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	var disableToken cleanupToken
	result, err := service.uploadOneBatch(context.Background(), uploaderDependencies{
		now: func() time.Time { return testRecordHour },
		start: immediateUploadStart(func(_ context.Context, prepared preparedUploadBatch, epoch uint64) (uploadResponse, error) {
			var disableErr error
			disableToken, disableErr = service.beginDisable(context.Background(), testStateVersion(7))
			if disableErr != nil {
				t.Fatalf("disable during signed-pause request: %v", disableErr)
			}
			return uploadResponse{kind: uploadResponsePause, statusCode: 410, pause: verifiedPause{
				releaseVersion: prepared.releaseVersion, metricsEpoch: epoch, keyID: "test-key",
			}}, nil
		}),
	})
	defer func() { _ = disableToken.Close() }()
	if !errors.Is(err, ErrStateChangedConcurrently) || result.outcome != uploadRunStale || result.events != 1 {
		t.Fatalf("stale signed-pause result = %+v err=%v", result, err)
	}
	state := readStateFixture(t, home)
	if state.Preference != preferenceDisabled || state.CleanupKind != cleanupDisable || state.PausedThroughMetricsEpoch != 0 {
		t.Fatalf("stale signed pause changed disable barrier: %#v", state)
	}
	assertSpoolFileLocation(t, home, inflightDirectoryName, event.EventID)
	root = mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if quota := readQuotaFromRoot(t, root); quota != (spoolQuota{Events: 1, Bytes: uint64(len(data))}) {
		t.Fatalf("stale signed pause changed quota: %+v", quota)
	}
}

func TestPauseCleanupCallerHeldBarrierSyncFailurePreventsDeletion(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(9, 2, testInstallationID, "")
	paused.CleanupKind = cleanupPause
	paused.CleanupEpoch = 3
	paused.PausedThroughMetricsEpoch = 2
	writeStateFixture(t, home, paused)
	plainRoot := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(plainRoot, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	rootTemp := filepath.Join(home.Root(), fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xa13)))
	writeJournaledRootTempCrashFixture(t, plainRoot, filepath.Base(rootTemp),
		[]byte("installation_id = \""+testInstallationID+"\"\n"), 0)
	if err := plainRoot.Close(); err != nil {
		t.Fatal(err)
	}

	failSync := false
	spoolDestruction := 0
	enumeratedBeforeBarrierSync := false
	hooks := storageTestHooks{
		beforeStep: func(step storageStep) error {
			if failSync && step == storageStepEnumerate {
				enumeratedBeforeBarrierSync = true
			}
			if failSync && step == storageStepDirectorySync {
				return errors.New("post-barrier sync remains uncertain")
			}
			return nil
		},
		beforeMutation: func(step storageStep, path string) {
			if failSync && (step == storageStepDelete || step == storageStepUnlink || step == storageStepRmdir) &&
				strings.Contains(path, ".pm-tmp-") {
				spoolDestruction++
			}
		},
	}
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	root, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	uploader, err := service.lockUploader(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = uploader.Close() }()
	state, err := uploader.lockState(context.Background(), service)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	token, _, pending, err := service.pauseCleanupLocked(state)
	if err != nil || !pending {
		t.Fatalf("load pause cleanup token: pending=%v err=%v", pending, err)
	}
	defer func() { _ = token.Close() }()
	failSync = true
	complete, err := service.finishPauseCleanupLocked(state, token, defaultSpoolWorkBudget())
	if err == nil || complete || spoolDestruction != 0 || enumeratedBeforeBarrierSync {
		t.Fatalf("unsynced caller-held cleanup = complete:%v destruction:%d enumerated:%v err:%v",
			complete, spoolDestruction, enumeratedBeforeBarrierSync, err)
	}
	if _, err := os.Lstat(rootTemp); err != nil {
		t.Fatalf("unsynced pause removed root temp: %v", err)
	}
	if current := readStateFixture(t, home); current.CleanupKind != cleanupPause {
		t.Fatalf("unsynced caller-held cleanup cleared pause: %#v", current)
	}
}

func TestPauseCleanupPurgesSpawnThrottleBeforeCompleting(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(9, 2, testInstallationID, "")
	paused.CleanupKind = cleanupPause
	paused.CleanupEpoch = 3
	paused.PausedThroughMetricsEpoch = 1
	writeStateFixture(t, home, paused)

	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		t.Fatal(err)
	}
	throttle, err := encodeSpawnThrottle(spawnThrottleRecord{
		attemptToken: testSpawnTokenOne,
		attemptedAt:  testRecordHour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := root.writeFileAtomic(spawnThrottleFileName, throttle); err != nil {
		t.Fatal(err)
	}

	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	uploader, err := service.lockUploader(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = uploader.Close() }()
	state, err := uploader.lockState(context.Background(), service)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	token, _, pending, err := service.pauseCleanupLocked(state)
	if err != nil || !pending {
		t.Fatalf("load pause-cleanup authority: pending=%v err=%v", pending, err)
	}
	defer func() { _ = token.Close() }()
	complete, cleanupErr := service.finishPauseCleanupLocked(state, token, defaultSpoolWorkBudget())
	if cleanupErr != nil || !complete {
		t.Fatalf("pause cleanup with spawn throttle = complete:%v err:%v", complete, cleanupErr)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), spawnThrottleFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("completed pause cleanup retained spawn throttle: %v", err)
	}
	if err := proveCleanMetricsTree(root, defaultSpoolWorkBudget()); err != nil {
		t.Fatalf("completed pause cleanup did not prove a clean root: %v", err)
	}
	clean := readStateFixture(t, home)
	if clean.CleanupKind != cleanupNone || clean.Preference != preferenceEnabled || clean.SpoolGeneration != "" ||
		clean.InstallationID != testInstallationID || clean.PausedThroughMetricsEpoch != 1 {
		t.Fatalf("pause cleanup successor state = %#v", clean)
	}
}

func TestGreaterEpochResumeWaitsForUploaderLockBeforeCleanup(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(9, 2, testInstallationID, "")
	paused.CleanupKind = cleanupPause
	paused.CleanupEpoch = 3
	paused.PausedThroughMetricsEpoch = 1
	writeStateFixture(t, home, paused)
	root := mustOpenMutableRoot(t, home)
	event := testSpoolEvent(testEventIDOne, "1.0.0", testRecordHour, CommandHelp)
	data := writeSpoolEventFixture(t, root, queueDirectoryName, testSpoolGeneration, event)
	if err := persistSpoolQuota(root, spoolQuota{Events: 1, Bytes: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}

	resumeGeneration := "56565656-5656-4656-8656-565656565656"
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, resumeGeneration)
	reachedLock := make(chan struct{})
	var once sync.Once
	deps.storageHooks.beforeStep = func(step storageStep) error {
		if step == storageStepLock {
			once.Do(func() { close(reachedLock) })
		}
		return nil
	}
	service := mustOpenTestService(t, deps)
	held, err := service.lockUploader(context.Background(), root)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	permitResult := make(chan RecordingPermit, 1)
	go func() { permitResult <- service.RecordingPermit(recordableInvocationAt(testRecordHour)) }()
	select {
	case <-reachedLock:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("greater-epoch cleanup did not attempt uploader lock")
	}
	if state := readStateFixture(t, home); state != paused {
		t.Fatalf("greater-epoch path mutated state before uploader barrier: %#v", state)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case permit := <-permitResult:
		if permit.Valid() {
			t.Fatal("uploader-barrier transition invocation received a permit")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("greater-epoch cleanup did not finish after uploader release")
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}
