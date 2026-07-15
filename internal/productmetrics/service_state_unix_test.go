//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/gchome"
)

const testNotice = "TEST-ONLY product metrics notice\n"

func TestEnvironmentDisableTruthSets(t *testing.T) {
	for _, value := range []string{"1", "true", "yes", "on", "TRUE", " Yes ", "\ton\n"} {
		t.Run("gc-true-"+fmt.Sprintf("%q", value), func(t *testing.T) {
			if !gcDisableTruthy(value) {
				t.Fatalf("gcDisableTruthy(%q) = false, want true", value)
			}
		})
	}
	for _, value := range []string{"", "0", "false", "no", "off", "enabled", "2", "truth"} {
		t.Run("gc-false-"+fmt.Sprintf("%q", value), func(t *testing.T) {
			if gcDisableTruthy(value) {
				t.Fatalf("gcDisableTruthy(%q) = true, want false", value)
			}
		})
	}

	for _, value := range []string{"1", "true", "yes", "on", "anything", " ", "FALSELY"} {
		t.Run("dnt-true-"+fmt.Sprintf("%q", value), func(t *testing.T) {
			if !doNotTrackTruthy(value) {
				t.Fatalf("doNotTrackTruthy(%q) = false, want true", value)
			}
		})
	}
	for _, value := range []string{"", "0", "false", "no", "off", " FALSE ", "OFF"} {
		t.Run("dnt-false-"+fmt.Sprintf("%q", value), func(t *testing.T) {
			if doNotTrackTruthy(value) {
				t.Fatalf("doNotTrackTruthy(%q) = true, want false", value)
			}
		})
	}
}

func TestEffectiveStateCompletePrecedenceMatrix(t *testing.T) {
	type stateFixture struct {
		state  *persistedState
		raw    []byte
		mutate func(*serviceDependencies)
		env    map[string]string
		want   EffectiveState
		reason StateReason
	}
	enabled := enabledState(4, 2, testInstallationID, testSpoolGeneration)
	disabled := disabledState(5, 0, cleanupNone)
	disabling := disabledState(6, 3, cleanupDisable)
	stale := enabledState(7, 1, testInstallationID, "")
	stale.RequiredNoticeVersion = 2
	paused := enabledState(8, 2, testInstallationID, "")
	paused.PausedThroughMetricsEpoch = 2
	paused.CleanupEpoch = 1
	cases := map[string]stateFixture{
		"absent":            {want: StatePendingNotice, reason: ReasonPreferenceUnset},
		"persisted pending": {state: statePointer(pendingState(2)), want: StatePendingNotice, reason: ReasonPreferenceUnset},
		"enabled":           {state: &enabled, want: StateEnabled, reason: ReasonEnabled},
		"disabled":          {state: &disabled, want: StateDisabled, reason: ReasonPersistedDisabled},
		"cleanup pending":   {state: &disabling, want: StateDisabledCleanupPending, reason: ReasonDisableCleanupPending},
		"stale notice":      {state: &stale, want: StateNoticeUpdateRequired, reason: ReasonNoticeVersionStale},
		"server paused":     {state: &paused, want: StateServerPaused, reason: ReasonServerPauseCoversEpoch},
		"corrupt":           {raw: []byte("not = [toml"), want: StateFailClosed, reason: ReasonConfigInvalid},
		"newer schema":      {raw: replaceStateField(t, enabled, "state_schema = 1", "state_schema = 2"), want: StateFailClosed, reason: ReasonStateSchemaNewer},
		"newer notice floor": {state: statePointer(withState(enabled, func(s *persistedState) {
			s.RequiredNoticeVersion = 3
			s.AcceptedNoticeVersion = 2
			s.SpoolGeneration = ""
		})), want: StateFailClosed, reason: ReasonNoticeFloorNewer},
		"DNT":                     {state: &enabled, env: map[string]string{envDoNotTrack: "1"}, want: StateEnvironmentDisabled, reason: ReasonDoNotTrack},
		"GC disable":              {state: &enabled, env: map[string]string{envDisableUsageMetrics: "yes"}, want: StateEnvironmentDisabled, reason: ReasonGCDisable},
		"DNT precedes GC disable": {state: &enabled, env: map[string]string{envDoNotTrack: "yes", envDisableUsageMetrics: "yes"}, want: StateEnvironmentDisabled, reason: ReasonDoNotTrack},
		"development build":       {state: &enabled, mutate: func(d *serviceDependencies) { d.release.official = false }, env: map[string]string{envDoNotTrack: "1"}, want: StateFailClosed, reason: ReasonDevelopmentBuild},
		"unsupported platform":    {state: &enabled, mutate: func(d *serviceDependencies) { d.release.platformSupported = false }, want: StateFailClosed, reason: ReasonUnsupportedPlatform},
		"empty endpoint":          {state: &enabled, mutate: func(d *serviceDependencies) { d.release.endpointConfigured = false }, want: StateFailClosed, reason: ReasonEndpointMissing},
		"default-off rollout":     {state: &enabled, mutate: func(d *serviceDependencies) { d.release.rollout = RolloutDefaultOff }, want: StateFailClosed, reason: ReasonRolloutDisabled},
		"notice unavailable":      {state: &enabled, mutate: func(d *serviceDependencies) { d.notice = noticeDefinition{} }, want: StateFailClosed, reason: ReasonNoticeUnavailable},
		"unstable home":           {mutate: func(d *serviceDependencies) { d.homeErr = errors.New("unstable test home") }, want: StateFailClosed, reason: ReasonHomeUnstable},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			deps := defaultTestServiceDependencies(home, 2)
			if test.mutate != nil {
				test.mutate(&deps)
			}
			deps.getenv = mapGetenv(test.env)
			if test.state != nil {
				writeStateFixture(t, home, *test.state)
			} else if test.raw != nil {
				writeRawConfigFixture(t, home, test.raw)
			}
			service := mustOpenTestService(t, deps)
			status := service.Status(context.Background())
			if status.State != test.want || status.Reason != test.reason {
				t.Fatalf("Status() = (%q, %q), want (%q, %q)", status.State, status.Reason, test.want, test.reason)
			}
		})
	}
}

func TestConfigUnreadablePrecedesEnvironmentDisable(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	if err := os.Chmod(filepath.Join(home.Root(), configFileName), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := defaultTestServiceDependencies(home, 2)
	deps.getenv = mapGetenv(map[string]string{
		envDoNotTrack:          "1",
		envDisableUsageMetrics: "1",
	})
	status := mustOpenTestService(t, deps).Status(context.Background())
	if status.State != StateFailClosed || status.Reason != ReasonConfigUnreadable {
		t.Fatalf("Status() = (%q, %q), want (%q, %q)", status.State, status.Reason, StateFailClosed, ReasonConfigUnreadable)
	}
}

func TestFalseEnvironmentValuesNeverForceCollection(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, disabledState(2, 0, cleanupNone))
	deps := defaultTestServiceDependencies(home, 1)
	deps.getenv = mapGetenv(map[string]string{envDoNotTrack: "0", envDisableUsageMetrics: "false"})
	status := mustOpenTestService(t, deps).Status(context.Background())
	if status.State != StateDisabled {
		t.Fatalf("false environment values changed saved opt-out: %q", status.State)
	}
}

func TestStatusIsByteForByteReadOnlyAcrossAbsentCorruptAndUnsafeStates(t *testing.T) {
	tests := map[string]func(*testing.T, gchome.ProductUsageHome){
		"absent config": func(_ *testing.T, _ gchome.ProductUsageHome) {},
		"corrupt config": func(t *testing.T, home gchome.ProductUsageHome) {
			writeRawConfigFixture(t, home, []byte("state_schema = [\n"))
		},
		"newer config": func(t *testing.T, home gchome.ProductUsageHome) {
			state := enabledState(1, 1, testInstallationID, testSpoolGeneration)
			writeRawConfigFixture(t, home, replaceStateField(t, state, "state_schema = 1", "state_schema = 9"))
		},
		"wrong config mode": func(t *testing.T, home gchome.ProductUsageHome) {
			writeStateFixture(t, home, enabledState(1, 1, testInstallationID, testSpoolGeneration))
			if err := os.Chmod(filepath.Join(home.Root(), configFileName), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			ensureMetricsRoot(t, home)
			setup(t, home)
			before := snapshotTree(t, home.Root())
			service := mustOpenTestService(t, defaultTestServiceDependencies(home, 1))
			_ = service.Status(context.Background())
			after := snapshotTree(t, home.Root())
			if before != after {
				t.Fatalf("Status mutated root\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if _, err := os.Lstat(filepath.Join(home.Root(), "state.lock")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Status created state.lock: %v", err)
			}
		})
	}
}

func TestOpenProductionAndPreparationAreLazyAndNonCreating(t *testing.T) {
	parent := t.TempDir()
	homePath := filepath.Join(parent, "not-created")
	t.Setenv("GC_HOME", homePath)
	service, err := OpenProduction(ProductionOptions{Home: gchome.ResolveReadOnly(), Release: CurrentReleaseIdentity()})
	if err != nil {
		t.Fatalf("OpenProduction() error = %v", err)
	}
	if _, err := os.Lstat(homePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenProduction created home: %v", err)
	}
	_ = service.Status(context.Background())
	_ = service.RecordingPermit(recordableInvocation())
	if _, err := os.Lstat(homePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only preparation created home: %v", err)
	}
	status := service.Status(context.Background())
	if status.State != StateFailClosed || status.Reason != ReasonDevelopmentBuild {
		t.Fatalf("development Status = (%q, %q), want fail-closed development", status.State, status.Reason)
	}
}

func TestStatusReportsBoundedProjectionWithoutExposingIdentity(t *testing.T) {
	home := newMetricsTestHome(t)
	state := enabledState(9, 1, testInstallationID, testSpoolGeneration)
	state.CleanupEpoch = 4
	writeStateFixture(t, home, state)
	deps := defaultTestServiceDependencies(home, 1)
	deps.notice.version = 1
	status := mustOpenTestService(t, deps).Status(context.Background())
	if !status.ConfigPresent || !status.InstallationIDPresent || !status.SpoolGenerationPresent {
		t.Fatalf("status presence projection = %#v", status)
	}
	if status.State != StateEnabled || status.Reason != ReasonEnabled || status.StateSchema != currentStateSchema ||
		status.RequiredNoticeVersion != 1 || status.AcceptedNoticeVersion != 1 {
		t.Fatalf("status bounded state/version projection = %#v", status)
	}
	if status.ConfigPath != filepath.Join(home.Root(), configFileName) {
		t.Fatalf("ConfigPath = %q", status.ConfigPath)
	}
	if strings.Contains(fmt.Sprintf("%#v", status), testInstallationID) || strings.Contains(fmt.Sprintf("%#v", status), testSpoolGeneration) {
		t.Fatalf("default status representation exposed raw identity: %#v", status)
	}

	cleanup := disabledState(10, 7, cleanupDisable)
	writeStateFixture(t, home, cleanup)
	cleanupStatus := mustOpenTestService(t, deps).Status(context.Background())
	if cleanupStatus.State != StateDisabledCleanupPending || cleanupStatus.Reason != ReasonDisableCleanupPending ||
		!cleanupStatus.CleanupPending || cleanupStatus.InstallationIDPresent || cleanupStatus.SpoolGenerationPresent {
		t.Fatalf("status cleanup presence projection = %#v", cleanupStatus)
	}
}

func TestRecordingPermitIsImmutableAndCapturesExactState(t *testing.T) {
	home := newMetricsTestHome(t)
	state := enabledState(3, 1, testInstallationID, testSpoolGeneration)
	writeStateFixture(t, home, state)
	deps := defaultTestServiceDependencies(home, 1)
	deps.notice.version = 1
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocation())
	if !permit.Valid() {
		t.Fatal("RecordingPermit is invalid, want valid")
	}
	if permit.stateGeneration != 3 || permit.installationID != testInstallationID || permit.spoolGeneration != testSpoolGeneration || permit.releaseVersion != "1.0.0" || permit.metricsEpoch != 1 {
		t.Fatalf("permit snapshot = %#v", permit)
	}
	if _, err := service.beginDisable(context.Background(), testStateVersion(3)); err != nil {
		t.Fatalf("beginDisable: %v", err)
	}
	if !permit.Valid() || permit.stateGeneration != 3 {
		t.Fatalf("persisted mutation changed immutable value permit: %#v", permit)
	}
	if got := service.RecordingPermit(recordableInvocation()); got.Valid() {
		t.Fatalf("disabled service returned permit: %#v", got)
	}
}

func TestRecordingPermitConjunctiveEnvironmentAndAutomationGates(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(1, 1, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 1)
	deps.notice.version = 1
	service := mustOpenTestService(t, deps)
	cases := map[string]InvocationContext{
		"not recordable":     {Recordable: false},
		"managed automation": {Recordable: true, ManagedAutomation: true},
		"DNT":                {Recordable: true, DoNotTrack: "1"},
		"GC disable":         {Recordable: true, DisableUsageMetrics: "on"},
	}
	for name, invocation := range cases {
		t.Run(name, func(t *testing.T) {
			if permit := service.RecordingPermit(invocation); permit.Valid() {
				t.Fatalf("permit = %#v, want invalid", permit)
			}
		})
	}
}

func TestNoticeInvalidationIsAtomicAndFirstReacceptingInvocationHasNoPermit(t *testing.T) {
	home := newMetricsTestHome(t)
	oldID := testInstallationID
	writeStateFixture(t, home, enabledState(10, 1, oldID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 1)
	newSpool := "33333333-3333-4333-8333-333333333333"
	deps.newUUID = uuidSequence(t, newSpool)
	service := mustOpenTestService(t, deps)

	beforeStatusBytes := readConfigFixture(t, home)
	status := service.Status(context.Background())
	if status.State != StateNoticeUpdateRequired {
		t.Fatalf("pre-invalidation status = %q", status.State)
	}
	if got := readConfigFixture(t, home); string(got) != string(beforeStatusBytes) {
		t.Fatal("Status persisted notice invalidation")
	}

	if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("invalidation invocation got permit: %#v", permit)
	}
	invalidated := readStateFixture(t, home)
	if invalidated.StateGeneration != 11 || invalidated.RequiredNoticeVersion != 2 || invalidated.AcceptedNoticeVersion != 1 || invalidated.InstallationID != oldID || invalidated.SpoolGeneration != "" {
		t.Fatalf("invalidated state = %#v", invalidated)
	}

	firstPermit := service.RecordingPermit(recordableInvocation())
	if firstPermit.Valid() {
		t.Fatal("stale-notice snapshot unexpectedly recordable")
	}
	var output strings.Builder
	result := service.MaybeActivateNotice(noticeInvocation(), &output)
	if result.Outcome != NoticeActivated || output.String() != testNotice {
		t.Fatalf("notice result/output = (%q, %q)", result.Outcome, output.String())
	}
	reaccepted := readStateFixture(t, home)
	if reaccepted.InstallationID != oldID || reaccepted.SpoolGeneration != newSpool || reaccepted.AcceptedNoticeVersion != 2 || reaccepted.StateGeneration != 12 {
		t.Fatalf("reaccepted state = %#v", reaccepted)
	}
	if firstPermit.Valid() {
		t.Fatal("reaccept transition retroactively changed sticky permit")
	}
	if permit := service.RecordingPermit(recordableInvocation()); !permit.Valid() {
		t.Fatal("invocation after reacceptance has no permit")
	}
}

func TestPauseCleanupAndGreaterEpochResumeAreMonotonic(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(5, 1, testInstallationID, testSpoolGeneration))
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	baseDeps := defaultTestServiceDependencies(home, 1)
	baseDeps.notice.version = 1
	service := mustOpenTestService(t, baseDeps)
	permit := service.RecordingPermit(recordableInvocation())
	token, err := service.applyPause(context.Background(), permit, 1)
	if err != nil {
		t.Fatalf("applyPause: %v", err)
	}
	paused := readStateFixture(t, home)
	if paused.StateGeneration != 6 || paused.CleanupEpoch != 1 || paused.CleanupKind != cleanupPause || paused.PausedThroughMetricsEpoch != 1 || paused.InstallationID != testInstallationID || paused.SpoolGeneration != "" {
		t.Fatalf("paused state = %#v", paused)
	}
	if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatal("paused cleanup returned permit")
	}
	if err := service.completeCleanup(context.Background(), token); err != nil {
		t.Fatalf("completeCleanup: %v", err)
	}
	cleanPaused := readStateFixture(t, home)
	if cleanPaused.StateGeneration != 7 || cleanPaused.CleanupEpoch != 1 || cleanPaused.CleanupKind != cleanupNone {
		t.Fatalf("clean paused state = %#v", cleanPaused)
	}

	if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatal("same epoch resumed pause")
	}
	resumeSpool := "44444444-4444-4444-8444-444444444444"
	deps := defaultTestServiceDependencies(home, 2)
	deps.notice.version = 1
	deps.newUUID = uuidSequence(t, resumeSpool)
	upgraded := mustOpenTestService(t, deps)
	if permit := upgraded.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("resuming invocation got permit: %#v", permit)
	}
	resumed := readStateFixture(t, home)
	if resumed.StateGeneration != 8 || resumed.CleanupEpoch != 1 || resumed.PausedThroughMetricsEpoch != 1 || resumed.InstallationID != testInstallationID || resumed.SpoolGeneration != resumeSpool {
		t.Fatalf("resumed state = %#v", resumed)
	}
	if permit := upgraded.RecordingPermit(recordableInvocation()); !permit.Valid() {
		t.Fatal("invocation after greater-epoch resume has no permit")
	}

	downgraded := mustOpenTestService(t, defaultTestServiceDependencies(home, 1))
	if permit := downgraded.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatal("downgraded release obtained permit for newer generation")
	}
}

func TestGreaterEpochResumeEntropyFailureLeavesPauseStateUnchanged(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(7, 1, testInstallationID, "")
	paused.PausedThroughMetricsEpoch = 1
	paused.CleanupEpoch = 2
	writeStateFixture(t, home, paused)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	before := readConfigFixture(t, home)
	deps := defaultTestServiceDependencies(home, 2)
	deps.notice.version = 1
	deps.newUUID = func() (string, error) { return "", errors.New("entropy unavailable") }
	service := mustOpenTestService(t, deps)
	if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
		t.Fatalf("entropy-failed resume returned permit: %#v", permit)
	}
	if after := readConfigFixture(t, home); string(after) != string(before) {
		t.Fatalf("entropy-failed resume mutated state\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestPauseAndDisableDoNotDependOnEntropyAndRejectStaleCAS(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(3, 1, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 1)
	deps.notice.version = 1
	deps.newUUID = func() (string, error) { return "", errors.New("entropy unavailable") }
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocation())
	if _, err := service.applyPause(context.Background(), permit, 1); err != nil {
		t.Fatalf("pause depended on entropy: %v", err)
	}
	if _, err := service.beginDisable(context.Background(), testStateVersion(3)); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("stale disable CAS error = %v, want ErrStateChangedConcurrently", err)
	}
	paused := readStateFixture(t, home)
	if _, err := service.beginDisable(context.Background(), stateVersionFrom(paused)); err != nil {
		t.Fatalf("disable depended on entropy: %v", err)
	}
	if _, err := service.applyPause(context.Background(), permit, 2); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("stale pause CAS error = %v, want ErrStateChangedConcurrently", err)
	}
}

func TestPauseRejectsPermitFromDifferentReleaseOrMetricsEpoch(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(3, 1, testInstallationID, testSpoolGeneration))
	originalDeps := defaultTestServiceDependencies(home, 1)
	originalDeps.notice.version = 1
	permit := mustOpenTestService(t, originalDeps).RecordingPermit(recordableInvocation())
	if !permit.Valid() {
		t.Fatal("original permit is invalid")
	}
	before := readConfigFixture(t, home)
	newerDeps := defaultTestServiceDependencies(home, 2)
	newerDeps.notice.version = 1
	newerDeps.release.releaseVersion = "2.0.0"
	newer := mustOpenTestService(t, newerDeps)
	if _, err := newer.applyPause(context.Background(), permit, 2); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("cross-release pause error = %v, want ErrStateChangedConcurrently", err)
	}
	if after := readConfigFixture(t, home); string(after) != string(before) {
		t.Fatalf("cross-release pause mutated state\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDisableRecoversSafelyWritableCorruptAndNewerStateWithoutReleaseOrEntropy(t *testing.T) {
	tests := map[string][]byte{
		"corrupt": []byte("state_schema = [\n"),
		"newer": replaceStateField(t,
			enabledState(9, 1, testInstallationID, testSpoolGeneration),
			"state_schema = 1", "state_schema = 99"),
		"oversize": []byte(strings.Repeat("#", maximumConfigBytes+1)),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeRawConfigFixture(t, home, raw)
			deps := defaultTestServiceDependencies(home, 1)
			deps.release = serviceRelease{}
			deps.notice = noticeDefinition{}
			deps.newUUID = func() (string, error) {
				t.Fatal("disable requested entropy")
				return "", errors.New("unreachable")
			}
			service := mustOpenTestService(t, deps)
			token, err := service.beginDisable(context.Background(), stateVersion{})
			if err != nil {
				t.Fatalf("beginDisable recovery: %v", err)
			}
			state := readStateFixture(t, home)
			if state.Preference != preferenceDisabled || state.StateGeneration != 1 || state.CleanupKind != cleanupDisable || state.CleanupEpoch != 1 || state.InstallationID != "" || state.SpoolGeneration != "" {
				t.Fatalf("recovered disabled state = %#v", state)
			}
			if token.stateGeneration != 1 || token.cleanupEpoch != 1 || token.kind != cleanupDisable {
				t.Fatalf("cleanup token = %#v", token)
			}
		})
	}
}

func TestMutationTerminalCountersFailClosedAndRemainDurablyOptOutRecoverable(t *testing.T) {
	tests := map[string]struct {
		neighbor persistedState
		terminal persistedState
	}{
		"state generation": {
			neighbor: enabledState(maximumStateCounter-1, 1, testInstallationID, testSpoolGeneration),
			terminal: enabledState(maximumStateCounter, 1, testInstallationID, testSpoolGeneration),
		},
		"cleanup epoch": {
			neighbor: withState(enabledState(7, 1, testInstallationID, testSpoolGeneration), func(state *persistedState) {
				state.CleanupEpoch = maximumStateCounter - 1
			}),
			terminal: withState(enabledState(7, 1, testInstallationID, testSpoolGeneration), func(state *persistedState) {
				state.CleanupEpoch = maximumStateCounter
			}),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, test.neighbor)
			deps := defaultTestServiceDependencies(home, 1)
			deps.notice.version = 1
			service := mustOpenTestService(t, deps)
			neighborStatus := service.Status(context.Background())
			neighborPermit := service.RecordingPermit(recordableInvocation())
			if neighborStatus.State != StateEnabled || !neighborPermit.Valid() {
				t.Fatalf("lower neighbor = status %#v permit %#v, want enabled permit", neighborStatus, neighborPermit)
			}
			neighborToken, err := service.beginDisable(context.Background(), stateVersion{counterNamespace: neighborPermit.counterNamespace, stateGeneration: neighborPermit.stateGeneration})
			if err != nil {
				t.Fatalf("lower neighbor could not install durable disable: %v", err)
			}
			neighborDisabled := readStateFixture(t, home)
			if neighborDisabled.Preference != preferenceDisabled || neighborDisabled.CleanupKind != cleanupDisable ||
				neighborDisabled.CounterNamespace <= test.neighbor.CounterNamespace ||
				neighborDisabled.InstallationID != "" || neighborDisabled.SpoolGeneration != "" {
				t.Fatalf("lower-neighbor disable = %#v", neighborDisabled)
			}
			if neighborToken.counterNamespace != neighborDisabled.CounterNamespace ||
				neighborToken.stateGeneration != neighborDisabled.StateGeneration ||
				neighborToken.cleanupEpoch != neighborDisabled.CleanupEpoch || neighborToken.kind != cleanupDisable {
				t.Fatalf("lower-neighbor token/state mismatch: token=%#v state=%#v", neighborToken, neighborDisabled)
			}
			if stateMatchesPermit(neighborDisabled, neighborPermit) {
				t.Fatalf("lower-neighbor permit retained authority after disable: permit=%#v state=%#v", neighborPermit, neighborDisabled)
			}

			terminalBytes := encodeUncheckedState(t, test.terminal)
			writeRawConfigFixture(t, home, terminalBytes)
			terminalStatus := service.Status(context.Background())
			if terminalStatus.State != StateFailClosed || terminalStatus.Reason != ReasonConfigInvalid {
				t.Fatalf("terminal status = %#v, want fail-closed config-invalid", terminalStatus)
			}
			if permit := service.RecordingPermit(recordableInvocation()); permit.Valid() {
				t.Fatalf("terminal state issued permit: %#v", permit)
			}

			token, err := service.beginDisable(context.Background(), stateVersion{})
			if err != nil {
				t.Fatalf("recover terminal state with durable disable: %v", err)
			}
			recovered := readStateFixture(t, home)
			if recovered.Preference != preferenceDisabled || recovered.StateGeneration != 1 ||
				recovered.CleanupKind != cleanupDisable || recovered.CleanupEpoch != 1 ||
				recovered.InstallationID != "" || recovered.SpoolGeneration != "" {
				t.Fatalf("terminal recovery = %#v", recovered)
			}
			if token.stateGeneration != 1 || token.cleanupEpoch != 1 || token.kind != cleanupDisable {
				t.Fatalf("terminal cleanup token = %#v", token)
			}
			if stateMatchesPermit(recovered, neighborPermit) {
				t.Fatalf("pre-recovery permit regained authority: permit=%#v state=%#v", neighborPermit, recovered)
			}

			peer := mustOpenTestService(t, deps)
			peerStatus := peer.Status(context.Background())
			if peerStatus.State != StateDisabledCleanupPending {
				t.Fatalf("peer status after durable disable = %#v", peerStatus)
			}
			if permit := peer.RecordingPermit(recordableInvocation()); permit.Valid() {
				t.Fatalf("peer issued permit after terminal recovery: %#v", permit)
			}
		})
	}
}

func TestMutationCounterLowerNeighborsCanDurablyApplyPause(t *testing.T) {
	tests := map[string]persistedState{
		"state generation": enabledState(maximumStateCounter-1, 1, testInstallationID, testSpoolGeneration),
		"cleanup epoch": withState(enabledState(7, 1, testInstallationID, testSpoolGeneration), func(state *persistedState) {
			state.CleanupEpoch = maximumStateCounter - 1
		}),
	}
	for name, state := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, state)
			deps := defaultTestServiceDependencies(home, 1)
			deps.notice.version = 1
			service := mustOpenTestService(t, deps)
			permit := service.RecordingPermit(recordableInvocation())
			if !permit.Valid() {
				t.Fatal("lower-neighbor state did not issue the expected initial permit")
			}
			token, err := service.applyPause(context.Background(), permit, 1)
			if err != nil {
				t.Fatalf("lower neighbor could not durably apply signed pause: %v", err)
			}
			paused := readStateFixture(t, home)
			if paused.Preference != preferenceEnabled || paused.StateGeneration != 1 ||
				paused.CounterNamespace <= state.CounterNamespace ||
				paused.CleanupKind != cleanupPause || paused.CleanupEpoch != 1 ||
				paused.PausedThroughMetricsEpoch != 1 || paused.InstallationID != testInstallationID ||
				paused.SpoolGeneration != "" {
				t.Fatalf("lower-neighbor pause state = %#v", paused)
			}
			if token.counterNamespace != paused.CounterNamespace || token.stateGeneration != 1 || token.cleanupEpoch != 1 || token.kind != cleanupPause {
				t.Fatalf("lower-neighbor pause token = %#v", token)
			}
			if stateMatchesPermit(paused, permit) {
				t.Fatalf("pre-pause permit retained authority: permit=%#v state=%#v", permit, paused)
			}
			peer := mustOpenTestService(t, deps)
			if status := peer.Status(context.Background()); status.State != StateServerPaused || status.Reason != ReasonPauseCleanupPending {
				t.Fatalf("peer pause status = %#v", status)
			}
			if got := peer.RecordingPermit(recordableInvocation()); got.Valid() {
				t.Fatalf("peer issued permit after pause recovery: %#v", got)
			}
		})
	}
}

func TestTerminalAdjacentDisableCleanupReusesExactOwnerAndCompletionRollsNamespace(t *testing.T) {
	tests := map[string]persistedState{
		"state generation": disabledState(maximumStateCounter-1, 1, cleanupDisable),
		"cleanup epoch":    disabledState(7, maximumStateCounter-1, cleanupDisable),
	}
	for name, state := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, state)
			deps := defaultTestServiceDependencies(home, 1)
			deps.notice.version = 1
			service := mustOpenTestService(t, deps)
			token, err := service.beginDisable(context.Background(), stateVersionFrom(state))
			if err != nil {
				t.Fatalf("reuse cleanup owner: %v", err)
			}
			if token.counterNamespace != state.CounterNamespace || token.stateGeneration != state.StateGeneration ||
				token.cleanupEpoch != state.CleanupEpoch || token.kind != cleanupDisable {
				t.Fatalf("reused cleanup token = %#v, want %#v", token, state)
			}
			if visible := readStateFixture(t, home); visible != state {
				t.Fatalf("reusing cleanup owner mutated state:\nbefore=%#v\nafter=%#v", state, visible)
			}
			if err := service.completeCleanup(context.Background(), token); err != nil {
				t.Fatalf("complete reused cleanup: %v", err)
			}
			clean := readStateFixture(t, home)
			if clean.Preference != preferenceDisabled || clean.StateGeneration != 1 ||
				clean.CounterNamespace != state.CounterNamespace+1 ||
				clean.CleanupKind != cleanupNone || clean.CleanupEpoch != 1 ||
				clean.InstallationID != "" || clean.SpoolGeneration != "" {
				t.Fatalf("clean disabled state = %#v", clean)
			}
		})
	}
}

func TestTerminalAdjacentPauseCleanupMovesToFreshCompletableNamespace(t *testing.T) {
	base := enabledState(7, 1, testInstallationID, "")
	base.CleanupKind = cleanupPause
	base.CleanupEpoch = 1
	base.PausedThroughMetricsEpoch = 1
	tests := map[string]persistedState{
		"state generation": withState(base, func(state *persistedState) { state.StateGeneration = maximumStateCounter - 1 }),
		"cleanup epoch":    withState(base, func(state *persistedState) { state.CleanupEpoch = maximumStateCounter - 1 }),
	}
	for name, state := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, state)
			deps := defaultTestServiceDependencies(home, 1)
			deps.notice.version = 1
			service := mustOpenTestService(t, deps)
			token := leasedCleanupTokenFixture(t, home)
			if err := service.completeCleanup(context.Background(), token); err != nil {
				t.Fatalf("complete terminal-adjacent pause cleanup: %v", err)
			}
			clean := readStateFixture(t, home)
			if clean.Preference != preferenceEnabled || clean.StateGeneration != 1 ||
				clean.CounterNamespace <= state.CounterNamespace ||
				clean.CleanupKind != cleanupNone || clean.CleanupEpoch != 1 ||
				clean.PausedThroughMetricsEpoch != 1 || clean.InstallationID != testInstallationID ||
				clean.SpoolGeneration != "" {
				t.Fatalf("clean paused state = %#v", clean)
			}
			peer := mustOpenTestService(t, deps)
			if status := peer.Status(context.Background()); status.State != StateServerPaused || status.Reason != ReasonServerPauseCoversEpoch {
				t.Fatalf("peer clean-pause status = %#v", status)
			}
		})
	}
}

func TestCurrentEndpointEmptyProductionServiceCanPersistAbsentAndCorruptOptOutWithoutEntropy(t *testing.T) {
	tests := map[string][]byte{
		"absent":  nil,
		"corrupt": []byte("invalid = [\n"),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			if raw != nil {
				writeRawConfigFixture(t, home, raw)
			}
			service, err := OpenProduction(ProductionOptions{Home: home.Home(), Release: CurrentReleaseIdentity()})
			if err != nil {
				t.Fatalf("OpenProduction: %v", err)
			}
			service.deps.newUUID = func() (string, error) {
				t.Fatal("endpoint-empty production opt-out requested entropy")
				return "", errors.New("unreachable")
			}
			token, err := service.beginDisable(context.Background(), stateVersion{})
			if err != nil {
				t.Fatalf("beginDisable: %v", err)
			}
			state := readStateFixture(t, home)
			if state.Preference != preferenceDisabled || state.RequiredNoticeVersion != 0 || state.AcceptedNoticeVersion != 0 || state.InstallationID != "" || state.SpoolGeneration != "" || state.CleanupKind != cleanupDisable {
				t.Fatalf("endpoint-empty disabled state = %#v", state)
			}
			if token.stateGeneration != state.StateGeneration || token.cleanupEpoch != state.CleanupEpoch {
				t.Fatalf("token/state mismatch token=%#v state=%#v", token, state)
			}
			if err := service.Enable(context.Background(), noticeInvocation(), io.Discard); err == nil {
				t.Fatal("endpoint-empty production Enable succeeded")
			}
			if result := service.MaybeActivateNotice(noticeInvocation(), io.Discard); result.Outcome == NoticeActivated {
				t.Fatalf("endpoint-empty production notice activated: %#v", result)
			}
		})
	}
}

func TestAppliedSyncPendingDisableAndPauseReturnNonSuccessWhilePeersFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		transition func(*testing.T, *Service) error
		wantState  EffectiveState
	}{
		{
			name: "disable",
			transition: func(_ *testing.T, service *Service) error {
				_, err := service.beginDisable(context.Background(), testStateVersion(4))
				return err
			},
			wantState: StateDisabledCleanupPending,
		},
		{
			name: "pause",
			transition: func(t *testing.T, service *Service) error {
				permitDeps := service.deps
				permitDeps.storageHooks = storageTestHooks{}
				permit := mustOpenTestService(t, permitDeps).RecordingPermit(recordableInvocation())
				_, err := service.applyPause(context.Background(), permit, 1)
				return err
			},
			wantState: StateServerPaused,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeStateFixture(t, home, enabledState(4, 1, testInstallationID, testSpoolGeneration))
			precreateStateLock(t, home)
			var renamed bool
			deps := defaultTestServiceDependencies(home, 1)
			deps.notice.version = 1
			deps.storageHooks = storageTestHooks{beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					renamed = true
				}
				if renamed && step == storageStepDirectorySync {
					return errors.New("persistent sync failure")
				}
				return nil
			}}
			service := mustOpenTestService(t, deps)
			err := test.transition(t, service)
			if !errors.Is(err, errStateAppliedSyncPending) {
				t.Fatalf("transition error = %v, want applied-sync-pending non-success", err)
			}
			peerDeps := defaultTestServiceDependencies(home, 1)
			peerDeps.notice.version = 1
			peer := mustOpenTestService(t, peerDeps)
			status := peer.Status(context.Background())
			if status.State != test.wantState {
				t.Fatalf("peer state = (%q, %q), want %q", status.State, status.Reason, test.wantState)
			}
			if permit := peer.RecordingPermit(recordableInvocation()); permit.Valid() {
				t.Fatalf("peer issued permit after sync-pending privacy barrier: %#v", permit)
			}
		})
	}
}

func TestCallerHeldStateMutationDoesNotReacquireStateLock(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, disabledState(8, 3, cleanupDisable))

	lockAttempts := 0
	deps := defaultTestServiceDependencies(home, 2)
	deps.storageHooks.beforeStep = func(step storageStep) error {
		if step != storageStepLock {
			return nil
		}
		lockAttempts++
		if lockAttempts > 1 {
			return errors.New("caller-held mutation attempted a nested state lock")
		}
		return nil
	}
	service := mustOpenTestService(t, deps)
	root, err := openStorageRootMutableWithHooks(home, deps.storageHooks)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := service.lockState(context.Background(), root)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	loaded := loadStateFromDirectory(root)
	if loaded.err != nil || !loaded.present {
		_ = loaded.Close()
		_ = locked.Close()
		_ = root.Close()
		t.Fatalf("load cleanup state: present=%v err=%v", loaded.present, loaded.err)
	}
	token := cleanupTokenFromLoaded(&loaded)
	_ = loaded.Close()
	if err := service.completeCleanupLocked(locked, token); err != nil {
		_ = token.Close()
		_ = locked.Close()
		_ = root.Close()
		t.Fatalf("complete cleanup under caller-held lock: %v", err)
	}
	if err := token.Close(); err != nil {
		t.Errorf("close cleanup token: %v", err)
	}
	if err := locked.Close(); err != nil {
		t.Errorf("release state lock: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Errorf("close root: %v", err)
	}
	if lockAttempts != 1 {
		t.Fatalf("state-lock attempts = %d, want exactly one", lockAttempts)
	}
	clean := readStateFixture(t, home)
	if clean.Preference != preferenceDisabled || clean.CleanupKind != cleanupNone || clean.StateGeneration != 9 {
		t.Fatalf("clean state = %#v", clean)
	}
}

func TestCallerHeldPermitRevalidationIncludesRecordReleaseAndEpoch(t *testing.T) {
	home := newMetricsTestHome(t)
	state := enabledState(7, 2, testInstallationID, testSpoolGeneration)
	writeStateFixture(t, home, state)
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))
	permit := service.RecordingPermit(recordableInvocation())
	defer func() { _ = permit.Close() }()
	if !permit.Valid() {
		t.Fatal("enabled state did not issue a permit")
	}

	root, err := openStorageRootMutable(home)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := service.lockState(context.Background(), root)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = locked.Close()
		_ = root.Close()
	}()
	if err := service.revalidatePermitLocked(locked, permit); err != nil {
		t.Fatalf("revalidate exact permit: %v", err)
	}

	wrongRelease := permit
	wrongRelease.releaseVersion = "1.0.1"
	if err := service.revalidatePermitLocked(locked, wrongRelease); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("wrong-release revalidation error = %v, want state conflict", err)
	}
	wrongEpoch := permit
	wrongEpoch.metricsEpoch++
	if err := service.revalidatePermitLocked(locked, wrongEpoch); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("wrong-epoch revalidation error = %v, want state conflict", err)
	}
	wrongOS := permit
	if wrongOS.operatingSystem == OSLinux {
		wrongOS.operatingSystem = OSDarwin
	} else {
		wrongOS.operatingSystem = OSLinux
	}
	if err := service.revalidatePermitLocked(locked, wrongOS); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("wrong-OS revalidation error = %v, want state conflict", err)
	}

	data, err := encodePersistedState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.writeFileAtomic(configFileName, data); err != nil {
		t.Fatalf("replace config with byte-identical state: %v", err)
	}
	if err := service.revalidatePermitLocked(locked, permit); !errors.Is(err, ErrStateChangedConcurrently) {
		t.Fatalf("old-incarnation revalidation error = %v, want state conflict", err)
	}
}

func TestConcurrentGreaterEpochResumeAndDisableHaveOneCASWinner(t *testing.T) {
	home := newMetricsTestHome(t)
	paused := enabledState(7, 1, testInstallationID, "")
	paused.PausedThroughMetricsEpoch = 1
	paused.CleanupEpoch = 2
	writeStateFixture(t, home, paused)
	root := mustOpenMutableRoot(t, home)
	if err := persistSpoolQuota(root, spoolQuota{}); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	deps := defaultTestServiceDependencies(home, 2)
	deps.notice.version = 1
	deps.newUUID = uuidSequence(t, "56565656-5656-4656-8656-565656565656")
	service := mustOpenTestService(t, deps)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		transitioned, err := service.finishPauseCleanupAndResume(context.Background())
		if err == nil && !transitioned {
			err = ErrStateChangedConcurrently
		}
		results <- err
	}()
	go func() {
		<-start
		_, err := service.beginDisable(context.Background(), testStateVersion(7))
		results <- err
	}()
	close(start)
	err1, err2 := <-results, <-results
	winners, conflicts := 0, 0
	for _, err := range []error{err1, err2} {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrStateChangedConcurrently):
			conflicts++
		default:
			t.Fatalf("concurrent resume/disable error = %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("resume/disable winners=%d conflicts=%d errors=(%v, %v)", winners, conflicts, err1, err2)
	}
}

func TestConcurrentDisablePauseAndResumeHaveOneCASWinner(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(20, 1, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 1)
	deps.notice.version = 1
	deps.newUUID = uuidSequence(t, "55555555-5555-4555-8555-555555555555")
	service := mustOpenTestService(t, deps)
	permit := service.RecordingPermit(recordableInvocation())
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := service.beginDisable(context.Background(), testStateVersion(20))
		results <- err
	}()
	go func() {
		<-start
		_, err := service.applyPause(context.Background(), permit, 1)
		results <- err
	}()
	close(start)
	err1, err2 := <-results, <-results
	winners := 0
	conflicts := 0
	for _, err := range []error{err1, err2} {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrStateChangedConcurrently):
			conflicts++
		default:
			t.Fatalf("concurrent mutation error = %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("CAS results winners=%d conflicts=%d errors=(%v, %v)", winners, conflicts, err1, err2)
	}
	state := readStateFixture(t, home)
	if state.StateGeneration != 21 || state.CleanupEpoch != 1 {
		t.Fatalf("concurrent state = %#v", state)
	}
}

func TestConfigModesAndBoundedReadsFailClosedWithoutRepair(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(1, 1, testInstallationID, testSpoolGeneration))
	path := filepath.Join(home.Root(), configFileName)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status := mustOpenTestService(t, defaultTestServiceDependencies(home, 1)).Status(context.Background())
	if status.State != StateFailClosed || status.Reason != ReasonConfigUnreadable {
		t.Fatalf("unsafe mode status = (%q, %q)", status.State, status.Reason)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("status repaired unsafe config")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	tooLarge := []byte(strings.Repeat("#", maximumConfigBytes+1))
	if err := os.WriteFile(path, tooLarge, 0o600); err != nil {
		t.Fatal(err)
	}
	status = mustOpenTestService(t, defaultTestServiceDependencies(home, 1)).Status(context.Background())
	if status.State != StateFailClosed || status.Reason != ReasonConfigUnreadable {
		t.Fatalf("oversize status = (%q, %q)", status.State, status.Reason)
	}
}

func defaultTestServiceDependencies(home gchome.ProductUsageHome, epoch uint64) serviceDependencies {
	return serviceDependencies{
		home: home,
		release: serviceRelease{
			platformSupported:  true,
			official:           true,
			endpointConfigured: true,
			rollout:            RolloutDefaultOn,
			releaseVersion:     "1.0.0",
			metricsEpoch:       epoch,
		},
		notice: noticeDefinition{testOnly: true, version: 2, text: []byte(testNotice)},
		getenv: func(string) string { return "" },
		newUUID: func() (string, error) {
			return randomUUIDv4(rand.Reader)
		},
		verifyTTY: func(io.Writer) bool { return true },
	}
}

func mustOpenTestService(t *testing.T, deps serviceDependencies) *Service {
	t.Helper()
	service, err := openWithDependencies(deps)
	if err != nil {
		t.Fatalf("openWithDependencies: %v", err)
	}
	return service
}

func newMetricsTestHome(t *testing.T) gchome.ProductUsageHome {
	t.Helper()
	return inspectStorageTestHome(t, false)
}

func ensureMetricsRoot(t *testing.T, home gchome.ProductUsageHome) {
	t.Helper()
	root, err := openStorageRootMutable(home)
	if err != nil {
		t.Fatalf("openStorageRootMutable: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close metrics root: %v", err)
	}
}

func writeStateFixture(t *testing.T, home gchome.ProductUsageHome, state persistedState) {
	t.Helper()
	data, err := encodePersistedState(state)
	if err != nil {
		t.Fatalf("encode state fixture: %v", err)
	}
	writeRawConfigFixture(t, home, data)
}

func writeRawConfigFixture(t *testing.T, home gchome.ProductUsageHome, data []byte) {
	t.Helper()
	root, err := openStorageRootMutable(home)
	if err != nil {
		t.Fatalf("open mutable root: %v", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Fatalf("close mutable root: %v", err)
		}
	}()
	if err := root.writeFileAtomic(configFileName, data); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
}

func readConfigFixture(t *testing.T, home gchome.ProductUsageHome) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home.Root(), configFileName))
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	return data
}

func readStateFixture(t *testing.T, home gchome.ProductUsageHome) persistedState {
	t.Helper()
	state, err := decodePersistedState(readConfigFixture(t, home))
	if err != nil {
		t.Fatalf("decode config fixture: %v", err)
	}
	return state
}

func leasedCleanupTokenFixture(t *testing.T, home gchome.ProductUsageHome) cleanupToken {
	t.Helper()
	root, err := openStorageRootReadOnly(home)
	if err != nil {
		t.Fatalf("open cleanup-token fixture: %v", err)
	}
	loaded := loadStateFromDirectory(root)
	if err := root.Close(); err != nil {
		_ = loaded.Close()
		t.Fatalf("close cleanup-token root: %v", err)
	}
	if loaded.err != nil || !loaded.present || loaded.lease == nil {
		_ = loaded.Close()
		t.Fatalf("load cleanup-token fixture: present=%v err=%v", loaded.present, loaded.err)
	}
	token := cleanupTokenFromLoaded(&loaded)
	_ = loaded.Close()
	t.Cleanup(func() { _ = token.Close() })
	return token
}

func cleanupTokenMatchesState(token cleanupToken, state persistedState) bool {
	return token.recordLease != nil && token.counterNamespace == state.CounterNamespace &&
		token.stateGeneration == state.StateGeneration && token.cleanupEpoch == state.CleanupEpoch && token.kind == state.CleanupKind
}

func statePointer(state persistedState) *persistedState { return &state }

func replaceStateField(t *testing.T, state persistedState, old, replacement string) []byte {
	t.Helper()
	data, err := encodePersistedState(state)
	if err != nil {
		t.Fatal(err)
	}
	result := strings.Replace(string(data), old, replacement, 1)
	if result == string(data) {
		t.Fatalf("fixture field %q not found in:\n%s", old, data)
	}
	return []byte(result)
}

func mapGetenv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func recordableInvocation() InvocationContext { return InvocationContext{Recordable: true} }

func noticeInvocation() InvocationContext { return InvocationContext{NoticeEligible: true} }

func uuidSequence(t *testing.T, values ...string) func() (string, error) {
	t.Helper()
	var mu sync.Mutex
	index := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if index >= len(values) {
			return "", errors.New("test UUID sequence exhausted")
		}
		value := values[index]
		index++
		return value, nil
	}
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %s %d", relative, info.Mode(), info.Size())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			line += " " + fmt.Sprintf("%x", data)
		}
		entries = append(entries, line)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %q: %v", root, err)
	}
	sort.Strings(entries)
	return strings.Join(entries, "\n")
}

func TestUUIDFactoryUsedOnlyForEnablement(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(1, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	var calls atomic.Int64
	deps.newUUID = func() (string, error) {
		calls.Add(1)
		return "", errors.New("unexpected entropy request")
	}
	service := mustOpenTestService(t, deps)
	_ = service.Status(context.Background())
	_ = service.RecordingPermit(recordableInvocation())
	if calls.Load() != 0 {
		t.Fatalf("read-only operations requested entropy %d times", calls.Load())
	}
}
