//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/gchome"
)

func TestCompleteVerifiedTTYNoticeCommitsIdentityAtomicallyAndExcludesFirstInvocation(t *testing.T) {
	home := newMetricsTestHome(t)
	installationID := "66666666-6666-4666-8666-666666666666"
	spool := "77777777-7777-4777-8777-777777777777"
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t, installationID, spool)
	service := mustOpenTestService(t, deps)

	firstPermit := service.RecordingPermit(recordableInvocation())
	if firstPermit.Valid() {
		t.Fatal("pending invocation received a permit")
	}
	var output oneWriteBuffer
	result := service.MaybeActivateNotice(noticeInvocation(), &output)
	if result.Outcome != NoticeActivated || result.Err != nil {
		t.Fatalf("MaybeActivateNotice() = %#v", result)
	}
	if output.String() != testNotice || output.calls != 1 {
		t.Fatalf("notice output = %q in %d writes", output.String(), output.calls)
	}
	state := readStateFixture(t, home)
	if state.StateSchema != currentStateSchema || state.StateGeneration != 1 || state.Preference != preferenceEnabled || state.RequiredNoticeVersion != 2 || state.AcceptedNoticeVersion != 2 || state.InstallationID != installationID || state.SpoolGeneration != spool || state.CleanupKind != cleanupNone {
		t.Fatalf("activated state = %#v", state)
	}
	info, err := os.Stat(filepath.Join(home.Root(), configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 0600", info.Mode().Perm())
	}
	if firstPermit.Valid() {
		t.Fatal("activation retroactively made first permit valid")
	}
	if permit := service.RecordingPermit(recordableInvocation()); !permit.Valid() {
		t.Fatal("following invocation did not receive permit")
	}
	if entries, err := os.ReadDir(home.Root()); err != nil {
		t.Fatal(err)
	} else {
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "id") || strings.Contains(entry.Name(), installationID) {
				t.Fatalf("identity escaped atomic config record into %q", entry.Name())
			}
		}
	}
}

func TestNoticeRequiresEligibilityCompleteWriteAndAvailableApprovedTestDependency(t *testing.T) {
	tests := map[string]struct {
		invocation InvocationContext
		writer     io.Writer
		mutate     func(*serviceDependencies)
		mayCreate  bool
	}{
		"non TTY":              {writer: &oneWriteBuffer{}},
		"unverified writer":    {invocation: noticeInvocation(), writer: &oneWriteBuffer{}, mutate: func(d *serviceDependencies) { d.verifyTTY = func(io.Writer) bool { return false } }},
		"managed context":      {invocation: InvocationContext{NoticeEligible: true, ManagedAutomation: true}, writer: &oneWriteBuffer{}},
		"DNT":                  {invocation: InvocationContext{NoticeEligible: true, DoNotTrack: "1"}, writer: &oneWriteBuffer{}},
		"GC disable":           {invocation: InvocationContext{NoticeEligible: true, DisableUsageMetrics: "1"}, writer: &oneWriteBuffer{}},
		"nil writer":           {invocation: noticeInvocation()},
		"short writer":         {invocation: noticeInvocation(), writer: shortNoticeWriter{}, mayCreate: true},
		"failed writer":        {invocation: noticeInvocation(), writer: failingNoticeWriter{}, mayCreate: true},
		"notice unavailable":   {invocation: noticeInvocation(), writer: &oneWriteBuffer{}, mutate: func(d *serviceDependencies) { d.notice = noticeDefinition{} }},
		"development build":    {invocation: noticeInvocation(), writer: &oneWriteBuffer{}, mutate: func(d *serviceDependencies) { d.release.official = false }},
		"missing endpoint":     {invocation: noticeInvocation(), writer: &oneWriteBuffer{}, mutate: func(d *serviceDependencies) { d.release.endpointConfigured = false }},
		"unsupported platform": {invocation: noticeInvocation(), writer: &oneWriteBuffer{}, mutate: func(d *serviceDependencies) { d.release.platformSupported = false }},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			deps := defaultTestServiceDependencies(home, 2)
			if test.mutate != nil {
				test.mutate(&deps)
			}
			service := mustOpenTestService(t, deps)
			result := service.MaybeActivateNotice(test.invocation, test.writer)
			if result.Outcome == NoticeActivated {
				t.Fatalf("MaybeActivateNotice() = %#v, want no activation", result)
			}
			if _, err := os.Lstat(home.Root()); !test.mayCreate && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("pre-mutation notice gate created product root: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(home.Root(), configFileName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("rejected notice left config or identity: %v", err)
			}
		})
	}
}

func TestNotAppliedNoticePersistenceRetainsPriorRecordAtEveryPreInstallStep(t *testing.T) {
	steps := []storageStep{storageStepWrite, storageStepFileSync, storageStepRename}
	for _, target := range steps {
		t.Run(string(target), func(t *testing.T) {
			home := newMetricsTestHome(t)
			prior := pendingState(4)
			prior.RequiredNoticeVersion = 2
			writeStateFixture(t, home, prior)
			precreateStateLock(t, home)
			before := readConfigFixture(t, home)
			var failed bool
			hooks := storageTestHooks{beforeStep: func(step storageStep) error {
				matches := step == target
				if matches && !failed {
					failed = true
					return errors.New("injected persistence crash point")
				}
				return nil
			}}
			deps := defaultTestServiceDependencies(home, 2)
			deps.storageHooks = hooks
			deps.newUUID = uuidSequence(t,
				"88888888-8888-4888-8888-888888888888",
				"99999999-9999-4999-8999-999999999999",
			)
			result := mustOpenTestService(t, deps).MaybeActivateNotice(noticeInvocation(), io.Discard)
			if result.Outcome != NoticeFailed || result.Err == nil {
				t.Fatalf("MaybeActivateNotice() = %#v, want failed", result)
			}
			if !failed {
				t.Fatal("target persistence step was not exercised")
			}
			after := readConfigFixture(t, home)
			if string(after) != string(before) {
				t.Fatalf("failed persistence changed prior record\nbefore:\n%s\nafter:\n%s", before, after)
			}
			assertNoTemporaryStateArtifacts(t, home)
		})
	}
}

func TestAppliedSyncPendingNoticeIsLogicalActivationForSeparatelyOpenedPeer(t *testing.T) {
	home := newMetricsTestHome(t)
	prior := pendingState(4)
	prior.RequiredNoticeVersion = 2
	writeStateFixture(t, home, prior)
	precreateStateLock(t, home)
	var renameSeen bool
	deps := defaultTestServiceDependencies(home, 2)
	deps.storageHooks = storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepRename {
			renameSeen = true
		}
		if renameSeen && step == storageStepDirectorySync {
			return errors.New("injected persistent directory sync failure")
		}
		return nil
	}}
	deps.newUUID = uuidSequence(t,
		"12121212-1212-4212-8212-121212121212",
		"34343434-3434-4434-8434-343434343434",
	)
	service := mustOpenTestService(t, deps)
	firstPermit := service.RecordingPermit(recordableInvocation())
	result := service.MaybeActivateNotice(noticeInvocation(), io.Discard)
	if result.Outcome != NoticeActivated || result.Err != nil {
		t.Fatalf("sync-pending activation = %#v, want logical activation", result)
	}
	if !renameSeen {
		t.Fatal("test did not reach the applied rename")
	}
	if firstPermit.Valid() {
		t.Fatal("sync-pending activation made first invocation recordable")
	}
	visible := readStateFixture(t, home)
	if visible.StateGeneration != 5 || visible.InstallationID == "" || visible.SpoolGeneration == "" {
		t.Fatalf("visible sync-pending state = %#v", visible)
	}

	peerDeps := defaultTestServiceDependencies(home, 2)
	peer := mustOpenTestService(t, peerDeps)
	if permit := peer.RecordingPermit(recordableInvocation()); !permit.Valid() {
		t.Fatal("separately opened peer treated visible logical activation as failed")
	}
}

func TestFailedFirstPersistenceLeavesNoConfigOrIdentityArtifact(t *testing.T) {
	home := newMetricsTestHome(t)
	ensureMetricsRoot(t, home)
	precreateStateLock(t, home)
	var failed bool
	deps := defaultTestServiceDependencies(home, 2)
	deps.storageHooks = storageTestHooks{beforeStep: func(step storageStep) error {
		if step == storageStepRename && !failed {
			failed = true
			return errors.New("rename crash")
		}
		return nil
	}}
	deps.newUUID = uuidSequence(t,
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	)
	result := mustOpenTestService(t, deps).MaybeActivateNotice(noticeInvocation(), io.Discard)
	if result.Outcome != NoticeFailed {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Lstat(filepath.Join(home.Root(), configFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed first commit left config: %v", err)
	}
	assertNoTemporaryStateArtifacts(t, home)
}

func TestConcurrentFirstActivationPrintsAndCommitsAtMostOnce(t *testing.T) {
	home := newMetricsTestHome(t)
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t,
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"dddddddd-dddd-4ddd-8ddd-dddddddddddd",
	)
	service := mustOpenTestService(t, deps)
	permits := []RecordingPermit{
		service.RecordingPermit(recordableInvocation()),
		service.RecordingPermit(recordableInvocation()),
	}
	start := make(chan struct{})
	type activation struct {
		result NoticeResult
		text   string
	}
	results := make(chan activation, 2)
	for range 2 {
		go func() {
			var writer oneWriteBuffer
			<-start
			result := service.MaybeActivateNotice(noticeInvocation(), &writer)
			results <- activation{result: result, text: writer.String()}
		}()
	}
	close(start)
	first, second := <-results, <-results
	activated := 0
	printed := 0
	for _, got := range []activation{first, second} {
		if got.result.Outcome == NoticeActivated {
			activated++
		}
		if got.text != "" {
			if got.text != testNotice {
				t.Fatalf("partial/unexpected notice = %q", got.text)
			}
			printed++
		}
	}
	if activated != 1 || printed != 1 {
		t.Fatalf("concurrent activation: activated=%d printed=%d results=(%#v, %#v)", activated, printed, first, second)
	}
	for index, permit := range permits {
		if permit.Valid() {
			t.Fatalf("first invocation permit %d became valid", index)
		}
	}
	state := readStateFixture(t, home)
	if state.StateGeneration != 1 || state.InstallationID == "" || state.SpoolGeneration == "" {
		t.Fatalf("concurrent activation state = %#v", state)
	}
}

func TestEnableIsTTYOnlyIdempotentAndRotatesOnlyAfterDisable(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	var entropyCalls atomic.Int64
	deps.newUUID = func() (string, error) {
		entropyCalls.Add(1)
		return "", errors.New("idempotent enable must not request entropy")
	}
	service := mustOpenTestService(t, deps)
	if err := service.Enable(context.Background(), noticeInvocation(), failingNoticeWriter{}); err != nil {
		t.Fatalf("idempotent Enable() error = %v", err)
	}
	if entropyCalls.Load() != 0 {
		t.Fatalf("idempotent Enable requested entropy %d times", entropyCalls.Load())
	}
	if got := readStateFixture(t, home); got.InstallationID != testInstallationID || got.StateGeneration != 4 {
		t.Fatalf("idempotent Enable mutated state: %#v", got)
	}

	token, err := service.beginDisable(context.Background(), testStateVersion(4))
	if err != nil {
		t.Fatalf("beginDisable: %v", err)
	}
	if err := service.completeCleanup(context.Background(), token); err != nil {
		t.Fatalf("completeCleanup: %v", err)
	}
	oldDisabled := readStateFixture(t, home)
	if oldDisabled.InstallationID != "" || oldDisabled.SpoolGeneration != "" {
		t.Fatalf("disable retained identity: %#v", oldDisabled)
	}
	newID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	newSpool := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	deps.newUUID = uuidSequence(t, newID, newSpool)
	service = mustOpenTestService(t, deps)
	var output oneWriteBuffer
	if err := service.Enable(context.Background(), noticeInvocation(), &output); err != nil {
		t.Fatalf("Enable after off: %v", err)
	}
	rotated := readStateFixture(t, home)
	if rotated.InstallationID != newID || rotated.SpoolGeneration != newSpool || rotated.InstallationID == testInstallationID || output.String() != testNotice {
		t.Fatalf("rotated state/output = (%#v, %q)", rotated, output.String())
	}
}

func TestEnableBlockedByTTYEnvironmentBuildCleanupAndPauseGates(t *testing.T) {
	paused := enabledState(4, 2, testInstallationID, "")
	paused.PausedThroughMetricsEpoch = 2
	cleanup := disabledState(4, 1, cleanupDisable)
	tests := map[string]struct {
		state      persistedState
		invocation InvocationContext
		mutate     func(*serviceDependencies)
	}{
		"non TTY":            {state: pendingState(4)},
		"DNT":                {state: pendingState(4), invocation: InvocationContext{NoticeEligible: true, DoNotTrack: "1"}},
		"GC disable":         {state: pendingState(4), invocation: InvocationContext{NoticeEligible: true, DisableUsageMetrics: "1"}},
		"managed":            {state: pendingState(4), invocation: InvocationContext{NoticeEligible: true, ManagedAutomation: true}},
		"cleanup pending":    {state: cleanup, invocation: noticeInvocation()},
		"server pause":       {state: paused, invocation: noticeInvocation()},
		"development":        {state: pendingState(4), invocation: noticeInvocation(), mutate: func(d *serviceDependencies) { d.release.official = false }},
		"unsupported":        {state: pendingState(4), invocation: noticeInvocation(), mutate: func(d *serviceDependencies) { d.release.platformSupported = false }},
		"endpoint missing":   {state: pendingState(4), invocation: noticeInvocation(), mutate: func(d *serviceDependencies) { d.release.endpointConfigured = false }},
		"rollout defaultoff": {state: pendingState(4), invocation: noticeInvocation(), mutate: func(d *serviceDependencies) { d.release.rollout = RolloutDefaultOff }},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			if test.state.RequiredNoticeVersion == 1 {
				test.state.RequiredNoticeVersion = 2
			}
			writeStateFixture(t, home, test.state)
			before := readConfigFixture(t, home)
			deps := defaultTestServiceDependencies(home, 2)
			if test.mutate != nil {
				test.mutate(&deps)
			}
			if err := mustOpenTestService(t, deps).Enable(context.Background(), test.invocation, io.Discard); err == nil {
				t.Fatal("Enable() error = nil, want gate rejection")
			}
			if after := readConfigFixture(t, home); string(after) != string(before) {
				t.Fatalf("blocked Enable mutated state\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestStaleNoticeCannotCrossPauseCleanupOrCoveredPauseBarrier(t *testing.T) {
	cleanupPending := enabledState(4, 1, testInstallationID, "")
	cleanupPending.RequiredNoticeVersion = 2
	cleanupPending.CleanupKind = cleanupPause
	cleanupPending.CleanupEpoch = 1
	cleanupPending.PausedThroughMetricsEpoch = 1
	coveredPause := enabledState(5, 1, testInstallationID, "")
	coveredPause.PausedThroughMetricsEpoch = 1
	coveredPause.CleanupEpoch = 1
	tests := map[string]struct {
		state persistedState
		epoch uint64
	}{
		"cleanup pending": {state: cleanupPending, epoch: 2},
		"covered pause":   {state: coveredPause, epoch: 1},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, test.state)
			deps := defaultTestServiceDependencies(home, test.epoch)
			service := mustOpenTestService(t, deps)
			var automatic oneWriteBuffer
			if result := service.MaybeActivateNotice(noticeInvocation(), &automatic); result.Outcome == NoticeActivated {
				t.Fatalf("automatic notice crossed barrier: %#v", result)
			}
			if automatic.String() != "" {
				t.Fatalf("automatic notice printed across barrier: %q", automatic.String())
			}
			var explicit oneWriteBuffer
			if err := service.Enable(context.Background(), noticeInvocation(), &explicit); err == nil {
				t.Fatal("Enable crossed pause barrier")
			}
			if explicit.String() != "" {
				t.Fatalf("Enable printed across barrier: %q", explicit.String())
			}
			want := test.state
			if want.RequiredNoticeVersion < deps.notice.version {
				want.RequiredNoticeVersion = deps.notice.version
				want.StateGeneration++
			}
			if after := readStateFixture(t, home); after != want {
				t.Fatalf("pause barrier did not preserve the monotonic invalidated state\nwant=%#v\nafter=%#v", want, after)
			}
		})
	}
}

func TestEnableEntropyFailureLeavesPriorStateAndNoIdentity(t *testing.T) {
	home := newMetricsTestHome(t)
	state := pendingState(2)
	state.RequiredNoticeVersion = 2
	writeStateFixture(t, home, state)
	before := readConfigFixture(t, home)
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = func() (string, error) { return "", errors.New("entropy failed") }
	service := mustOpenTestService(t, deps)
	var output oneWriteBuffer
	if err := service.Enable(context.Background(), noticeInvocation(), &output); err == nil {
		t.Fatal("Enable() error = nil, want entropy error")
	}
	if output.String() != testNotice {
		t.Fatalf("notice was not completely written before entropy failure: %q", output.String())
	}
	if after := readConfigFixture(t, home); string(after) != string(before) {
		t.Fatalf("entropy failure changed state\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestEveryActivationUUIDFailureLeavesPriorRecordAndNoIdentityArtifact(t *testing.T) {
	validID := "89898989-8989-4989-8989-898989898989"
	tests := map[string]func() (string, error){
		"installation entropy": func() (string, error) { return "", errors.New("installation entropy failed") },
		"invalid installation": func() (string, error) { return "not-a-uuid", nil },
		"spool entropy": func() func() (string, error) {
			var call int
			return func() (string, error) {
				call++
				if call == 1 {
					return validID, nil
				}
				return "", errors.New("spool entropy failed")
			}
		}(),
		"invalid spool": func() func() (string, error) {
			var call int
			return func() (string, error) {
				call++
				if call == 1 {
					return validID, nil
				}
				return "invalid-spool", nil
			}
		}(),
	}
	for name, factory := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			state := pendingState(2)
			state.RequiredNoticeVersion = 2
			writeStateFixture(t, home, state)
			before := readConfigFixture(t, home)
			deps := defaultTestServiceDependencies(home, 2)
			deps.newUUID = factory
			service := mustOpenTestService(t, deps)
			var output oneWriteBuffer
			if err := service.Enable(context.Background(), noticeInvocation(), &output); err == nil {
				t.Fatal("Enable() error = nil, want UUID failure")
			}
			if output.String() != testNotice {
				t.Fatalf("notice output = %q", output.String())
			}
			if after := readConfigFixture(t, home); string(after) != string(before) {
				t.Fatalf("UUID failure changed state\nbefore:\n%s\nafter:\n%s", before, after)
			}
			assertNoTemporaryStateArtifacts(t, home)
		})
	}
}

func TestConcurrentEnableAndDisableHaveOneStateGenerationWinner(t *testing.T) {
	home := newMetricsTestHome(t)
	state := pendingState(4)
	state.RequiredNoticeVersion = 2
	writeStateFixture(t, home, state)
	deps := defaultTestServiceDependencies(home, 2)
	deps.newUUID = uuidSequence(t,
		"67676767-6767-4767-8767-676767676767",
		"78787878-7878-4878-8878-787878787878",
	)
	service := mustOpenTestService(t, deps)
	start := make(chan struct{})
	type operationResult struct {
		name string
		err  error
		text string
	}
	results := make(chan operationResult, 2)
	go func() {
		var writer oneWriteBuffer
		<-start
		err := service.Enable(context.Background(), noticeInvocation(), &writer)
		results <- operationResult{name: "enable", err: err, text: writer.String()}
	}()
	go func() {
		<-start
		_, err := service.beginDisable(context.Background(), testStateVersion(4))
		results <- operationResult{name: "disable", err: err}
	}()
	close(start)
	first, second := <-results, <-results
	winners := 0
	for _, result := range []operationResult{first, second} {
		if result.err == nil {
			winners++
		}
		if result.text != "" && result.text != testNotice {
			t.Fatalf("partial concurrent notice = %q", result.text)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent enable/disable winners=%d results=(%#v, %#v)", winners, first, second)
	}
	final := readStateFixture(t, home)
	if final.StateGeneration != 5 {
		t.Fatalf("final generation = %d, want 5", final.StateGeneration)
	}
	if final.Preference != preferenceEnabled && (final.Preference != preferenceDisabled || final.CleanupKind != cleanupDisable) {
		t.Fatalf("unexpected final state = %#v", final)
	}
}

func precreateStateLock(t *testing.T, home gchome.ProductUsageHome) {
	t.Helper()
	root, err := openStorageRootMutable(home)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := root.acquireLock(context.Background(), "state.lock")
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertNoTemporaryStateArtifacts(t *testing.T, home gchome.ProductUsageHome) {
	t.Helper()
	entries, err := os.ReadDir(home.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pm-tmp-") || entry.Name() == "installation-id" {
			t.Fatalf("failed transaction left artifact %q", entry.Name())
		}
	}
}

type oneWriteBuffer struct {
	strings.Builder
	calls int
}

func (writer *oneWriteBuffer) Write(data []byte) (int, error) {
	writer.calls++
	return writer.Builder.Write(data)
}

type shortNoticeWriter struct{}

func (shortNoticeWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	return len(data) - 1, nil
}

type failingNoticeWriter struct{}

func (failingNoticeWriter) Write([]byte) (int, error) {
	return 0, errors.New("notice output failed")
}
