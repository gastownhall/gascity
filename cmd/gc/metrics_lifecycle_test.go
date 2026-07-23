package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/productmetrics"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/spf13/cobra"
)

type productMetricsInvocationSpy struct {
	fakeProductMetricsControlService

	mu           sync.Mutex
	events       []string
	recordedIDs  []productmetrics.CommandID
	permitInputs []productmetrics.InvocationContext
	noticeInputs []productmetrics.InvocationContext
	recordResult productmetrics.RecordResult
	factoryCalls int
	permitCalls  int
}

func (spy *productMetricsInvocationSpy) RecordingPermit(invocation productmetrics.InvocationContext) productmetrics.RecordingPermit {
	spy.mu.Lock()
	spy.permitCalls++
	spy.events = append(spy.events, "permit")
	spy.permitInputs = append(spy.permitInputs, invocation)
	spy.mu.Unlock()
	if !invocation.Recordable {
		spy.appendEvent("permit-not-recordable")
	}
	return productmetrics.RecordingPermit{}
}

func (spy *productMetricsInvocationSpy) MaybeActivateNotice(invocation productmetrics.InvocationContext, _ io.Writer) productmetrics.NoticeResult {
	spy.mu.Lock()
	spy.events = append(spy.events, "notice")
	spy.noticeInputs = append(spy.noticeInputs, invocation)
	spy.mu.Unlock()
	return productmetrics.NoticeResult{Outcome: productmetrics.NoticeNotNeeded}
}

func (spy *productMetricsInvocationSpy) RecordOnce(_ productmetrics.RecordingPermit, commandID productmetrics.CommandID) productmetrics.RecordResult {
	spy.mu.Lock()
	spy.events = append(spy.events, "record")
	spy.recordedIDs = append(spy.recordedIDs, commandID)
	result := spy.recordResult
	spy.mu.Unlock()
	return result
}

func (spy *productMetricsInvocationSpy) appendEvent(event string) {
	spy.mu.Lock()
	spy.events = append(spy.events, event)
	spy.mu.Unlock()
}

func (spy *productMetricsInvocationSpy) snapshot() ([]string, []productmetrics.CommandID) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return append([]string(nil), spy.events...), append([]productmetrics.CommandID(nil), spy.recordedIDs...)
}

func (spy *productMetricsInvocationSpy) counts() (factory, permit int) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return spy.factoryCalls, spy.permitCalls
}

func (spy *productMetricsInvocationSpy) permitInvocations() []productmetrics.InvocationContext {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return append([]productmetrics.InvocationContext(nil), spy.permitInputs...)
}

func (spy *productMetricsInvocationSpy) noticeInvocations() []productmetrics.InvocationContext {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return append([]productmetrics.InvocationContext(nil), spy.noticeInputs...)
}

type productMetricsOrderingWriter struct {
	spy  *productMetricsInvocationSpy
	name string
	mu   sync.Mutex
	data []byte
}

type productMetricsFailOrderingWriter struct {
	spy  *productMetricsInvocationSpy
	name string
}

type productMetricsPanicWriter struct {
	value any
}

func (writer productMetricsPanicWriter) Write([]byte) (int, error) {
	panic(writer.value)
}

type productMetricsTelemetrySpy struct {
	mu             sync.Mutex
	initCalls      int
	attributeCalls int
	shutdownCalls  int
	shutdownTimed  bool
}

func (spy *productMetricsTelemetrySpy) Shutdown(ctx context.Context) error {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	spy.shutdownCalls++
	_, spy.shutdownTimed = ctx.Deadline()
	return nil
}

func (spy *productMetricsTelemetrySpy) snapshot() (initCalls, attributeCalls, shutdownCalls int, shutdownTimed bool) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return spy.initCalls, spy.attributeCalls, spy.shutdownCalls, spy.shutdownTimed
}

type productMetricsNoticeFailureService struct {
	productMetricsInvocationSpy
}

func (service *productMetricsNoticeFailureService) MaybeActivateNotice(productmetrics.InvocationContext, io.Writer) productmetrics.NoticeResult {
	return productmetrics.NoticeResult{Outcome: productmetrics.NoticeFailed, Err: errors.New("private notice failure")}
}

type productMetricsNoticeWritingService struct {
	productMetricsInvocationSpy
	notice string
}

func (service *productMetricsNoticeWritingService) MaybeActivateNotice(_ productmetrics.InvocationContext, writer io.Writer) productmetrics.NoticeResult {
	_, _ = io.WriteString(writer, service.notice)
	return productmetrics.NoticeResult{Outcome: productmetrics.NoticeActivated}
}

func (writer productMetricsFailOrderingWriter) Write([]byte) (int, error) {
	writer.spy.appendEvent(writer.name)
	return 0, errors.New("injected output failure")
}

type productMetricsTransitionState uint8

const (
	productMetricsTransitionPending productMetricsTransitionState = iota
	productMetricsTransitionStale
	productMetricsTransitionGreaterEpoch
	productMetricsTransitionEnabled
)

type productMetricsTransitionSpy struct {
	fakeProductMetricsControlService

	mu               sync.Mutex
	state            productMetricsTransitionState
	permitRecordable bool
	permitInputs     []productmetrics.InvocationContext
	permitCalls      int
	transitions      int
	storedIDs        []productmetrics.CommandID
}

func (spy *productMetricsTransitionSpy) RecordingPermit(invocation productmetrics.InvocationContext) productmetrics.RecordingPermit {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	spy.permitCalls++
	spy.permitInputs = append(spy.permitInputs, invocation)
	spy.permitRecordable = false
	if invocation.ManagedAutomation || !invocation.Recordable {
		return productmetrics.RecordingPermit{}
	}
	if spy.state == productMetricsTransitionGreaterEpoch {
		spy.state = productMetricsTransitionEnabled
		spy.transitions++
		return productmetrics.RecordingPermit{}
	}
	spy.permitRecordable = spy.state == productMetricsTransitionEnabled
	return productmetrics.RecordingPermit{}
}

func (spy *productMetricsTransitionSpy) MaybeActivateNotice(invocation productmetrics.InvocationContext, _ io.Writer) productmetrics.NoticeResult {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if !invocation.ManagedAutomation && invocation.NoticeEligible &&
		(spy.state == productMetricsTransitionPending || spy.state == productMetricsTransitionStale) {
		spy.state = productMetricsTransitionEnabled
		spy.transitions++
		return productmetrics.NoticeResult{Outcome: productmetrics.NoticeActivated}
	}
	return productmetrics.NoticeResult{Outcome: productmetrics.NoticeNotNeeded}
}

func (spy *productMetricsTransitionSpy) RecordOnce(_ productmetrics.RecordingPermit, commandID productmetrics.CommandID) productmetrics.RecordResult {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if !spy.permitRecordable {
		return productmetrics.RecordDropped
	}
	spy.storedIDs = append(spy.storedIDs, commandID)
	return productmetrics.RecordStored
}

func (spy *productMetricsTransitionSpy) snapshot() (productMetricsTransitionState, int, int, []productmetrics.CommandID, []productmetrics.InvocationContext) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return spy.state, spy.permitCalls, spy.transitions, append([]productmetrics.CommandID(nil), spy.storedIDs...), append([]productmetrics.InvocationContext(nil), spy.permitInputs...)
}

func (writer *productMetricsOrderingWriter) Write(data []byte) (int, error) {
	writer.spy.appendEvent(writer.name)
	writer.mu.Lock()
	writer.data = append(writer.data, data...)
	writer.mu.Unlock()
	return len(data), nil
}

func (writer *productMetricsOrderingWriter) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return string(writer.data)
}

func TestProductMetricsLifecycleCapturesOneStickyPermitBeforeNotice(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordStored}
	withProductMetricsInvocationSpy(t, spy)

	if code := run([]string{"version"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("gc version exit = %d, want 0", code)
	}
	events, _ := spy.snapshot()
	factoryCalls, permitCalls := spy.counts()
	if factoryCalls != 1 || permitCalls != 1 || eventIndex(events, "permit") < 0 || eventIndex(events, "notice") < eventIndex(events, "permit") {
		t.Fatalf("factory/permit/notice = %d/%d/%v, want one factory, one permit, then notice", factoryCalls, permitCalls, events)
	}
}

func TestProductMetricsLifecycleCapturesHourBeforeSlowSetup(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	initial := time.Date(2026, 7, 13, 10, 59, 59, 0, time.UTC)
	later := initial.Add(2 * time.Hour)
	current := initial
	originalNow := productMetricsInvocationNow
	productMetricsInvocationNow = func() time.Time { return current }
	t.Cleanup(func() { productMetricsInvocationNow = originalNow })
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	withProductMetricsInvocationSpy(t, spy)
	telemetrySpy := &productMetricsTelemetrySpy{}
	originalInit := initializeCLITelemetry
	originalAttrs := setCLIProcessOTELAttrs
	initializeCLITelemetry = func(context.Context, string, string) (cliTelemetryShutdowner, error) {
		current = later
		telemetrySpy.mu.Lock()
		telemetrySpy.initCalls++
		telemetrySpy.mu.Unlock()
		return telemetrySpy, nil
	}
	setCLIProcessOTELAttrs = func() {
		telemetrySpy.mu.Lock()
		telemetrySpy.attributeCalls++
		telemetrySpy.mu.Unlock()
	}
	t.Cleanup(func() {
		initializeCLITelemetry = originalInit
		setCLIProcessOTELAttrs = originalAttrs
	})

	if code := run([]string{"version"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("gc version exit = %d, want 0", code)
	}
	invocations := spy.permitInvocations()
	wantHour := initial.Truncate(time.Hour).Format(productMetricsInvocationHourLayout)
	if len(invocations) != 1 || invocations[0].OccurredHourUTC != wantHour {
		t.Fatalf("permit invocations = %+v, want captured entry hour %q", invocations, wantHour)
	}
	assertProductMetricsTelemetryRun(t, telemetrySpy)
}

func TestProductMetricsLifecycleProviderHookGatesEntryPermit(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	providerKeys := []string{"GC_HOOK_SOURCE", "GC_PROVIDER_SESSION_ID", "GC_PROVIDER_SESSION_ID_REQUIRED"}
	allAutomationKeys := append([]string{
		"GC_SESSION_ID", "GC_SESSION_NAME", "GC_AGENT", "GC_TEMPLATE", "GC_MANAGED_SESSION_HOOK", "GC_HOOK_EVENT_NAME", "BEADS_ACTOR",
	}, providerKeys...)
	for _, providerKey := range providerKeys {
		t.Run(providerKey, func(t *testing.T) {
			for _, key := range allAutomationKeys {
				t.Setenv(key, "")
			}
			t.Setenv(providerKey, "provider-secret")
			spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordStored}
			withProductMetricsInvocationSpy(t, spy)

			if code := run([]string{"version"}, io.Discard, io.Discard); code != 0 {
				t.Fatalf("gc version exit = %d, want 0", code)
			}
			invocations := spy.permitInvocations()
			if len(invocations) != 1 || !invocations[0].ManagedAutomation {
				t.Fatalf("permit invocations = %+v, want one provider-gated automation context", invocations)
			}
			if _, recordedIDs := spy.snapshot(); len(recordedIDs) != 0 {
				t.Fatalf("provider hook recorded IDs = %v, want none", recordedIDs)
			}
		})
	}
}

func TestProductMetricsLifecycleMetricsControlBypassesCentralService(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	tests := []struct {
		name        string
		args        []string
		wantFactory int
	}{
		{name: "status", args: []string{"metrics", "status"}, wantFactory: 1},
		{name: "bare metrics", args: []string{"metrics"}, wantFactory: 1},
		{name: "example", args: []string{"metrics", "example"}},
		{name: "help", args: []string{"metrics", "--help"}},
		{name: "scoped city status", args: []string{"--city", "/private/city", "metrics", "status"}, wantFactory: 1},
		{name: "scoped rig example", args: []string{"--rig=private-rig", "metrics", "example"}},
		{name: "remote context status", args: []string{"--context=private-context", "metrics", "status"}, wantFactory: 1},
		{name: "remote URL example", args: []string{"--city-url", "https://city.example", "--city-name=private-city", "metrics", "example"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &productMetricsInvocationSpy{}
			withProductMetricsInvocationSpy(t, spy)
			if code := run(test.args, io.Discard, io.Discard); code != 0 {
				t.Fatalf("gc %v exit = %d, want 0", test.args, code)
			}
			factoryCalls, permitCalls := spy.counts()
			events, recordedIDs := spy.snapshot()
			if factoryCalls != test.wantFactory || permitCalls != 0 || len(recordedIDs) != 0 || eventIndex(events, "notice") >= 0 {
				t.Fatalf("metrics control lifecycle = factory:%d permit:%d ids:%v events:%v, want %d/0/none/no-notice", factoryCalls, permitCalls, recordedIDs, events, test.wantFactory)
			}
		})
	}
}

func TestProductMetricsLifecycleTransitionInvocationKeepsStickyZeroPermit(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	for _, key := range []string{
		"DO_NOT_TRACK", "GC_DISABLE_USAGE_METRICS", "GC_SESSION_ID", "GC_SESSION_NAME", "GC_AGENT", "GC_TEMPLATE",
		"GC_MANAGED_SESSION_HOOK", "GC_HOOK_EVENT_NAME", "BEADS_ACTOR", "GC_HOOK_SOURCE", "GC_PROVIDER_SESSION_ID", "GC_PROVIDER_SESSION_ID_REQUIRED",
	} {
		t.Setenv(key, "")
	}
	tests := []struct {
		name  string
		state productMetricsTransitionState
	}{
		{name: "pending notice activation", state: productMetricsTransitionPending},
		{name: "stale notice reacceptance", state: productMetricsTransitionStale},
		{name: "greater epoch resume", state: productMetricsTransitionGreaterEpoch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &productMetricsTransitionSpy{state: test.state}
			original := productMetricsControlServiceFactory
			productMetricsControlServiceFactory = func() (productMetricsControlService, error) { return spy, nil }
			t.Cleanup(func() { productMetricsControlServiceFactory = original })

			if code := run([]string{"help"}, io.Discard, io.Discard); code != 0 {
				t.Fatalf("transition gc help exit = %d, want 0", code)
			}
			state, permitCalls, transitions, storedIDs, _ := spy.snapshot()
			if state != productMetricsTransitionEnabled || permitCalls != 1 || transitions != 1 || len(storedIDs) != 0 {
				t.Fatalf("transition invocation = state:%d permits:%d transitions:%d stored:%v, want enabled/1/1/none", state, permitCalls, transitions, storedIDs)
			}

			if code := run([]string{"help"}, io.Discard, io.Discard); code != 0 {
				t.Fatalf("following gc help exit = %d, want 0", code)
			}
			_, permitCalls, transitions, storedIDs, _ = spy.snapshot()
			if permitCalls != 2 || transitions != 1 || len(storedIDs) != 1 || storedIDs[0] != productmetrics.CommandHelp {
				t.Fatalf("following invocation = permits:%d transitions:%d stored:%v, want 2/1/[help]", permitCalls, transitions, storedIDs)
			}
		})
	}
}

func TestProductMetricsLifecycleProviderHookBlocksPermitTransition(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	for _, key := range []string{
		"DO_NOT_TRACK", "GC_DISABLE_USAGE_METRICS", "GC_SESSION_ID", "GC_SESSION_NAME", "GC_AGENT", "GC_TEMPLATE",
		"GC_MANAGED_SESSION_HOOK", "GC_HOOK_EVENT_NAME", "BEADS_ACTOR", "GC_HOOK_SOURCE", "GC_PROVIDER_SESSION_ID", "GC_PROVIDER_SESSION_ID_REQUIRED",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("GC_HOOK_SOURCE", "provider-secret")
	spy := &productMetricsTransitionSpy{state: productMetricsTransitionGreaterEpoch}
	original := productMetricsControlServiceFactory
	productMetricsControlServiceFactory = func() (productMetricsControlService, error) { return spy, nil }
	t.Cleanup(func() { productMetricsControlServiceFactory = original })

	if code := run([]string{"version"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("provider gc version exit = %d, want 0", code)
	}
	state, permitCalls, transitions, storedIDs, inputs := spy.snapshot()
	if state != productMetricsTransitionGreaterEpoch || permitCalls != 1 || transitions != 0 || len(storedIDs) != 0 || len(inputs) != 1 || !inputs[0].ManagedAutomation {
		t.Fatalf("provider transition = state:%d permits:%d transitions:%d stored:%v inputs:%+v, want untouched/gated", state, permitCalls, transitions, storedIDs, inputs)
	}

	t.Setenv("GC_HOOK_SOURCE", "")
	if code := run([]string{"version"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("resume gc version exit = %d, want 0", code)
	}
	if code := run([]string{"version"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("post-resume gc version exit = %d, want 0", code)
	}
	state, permitCalls, transitions, storedIDs, _ = spy.snapshot()
	if state != productMetricsTransitionEnabled || permitCalls != 3 || transitions != 1 || len(storedIDs) != 1 || storedIDs[0] != productmetrics.CommandVersion {
		t.Fatalf("post-provider transition = state:%d permits:%d transitions:%d stored:%v, want enabled/3/1/[version]", state, permitCalls, transitions, storedIDs)
	}
}

func TestProductMetricsLifecycleImmediateAttemptsBeforeOutput(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordStored}
	withProductMetricsInvocationSpy(t, spy)
	stdout := &productMetricsOrderingWriter{spy: spy, name: "stdout"}
	stderr := &productMetricsOrderingWriter{spy: spy, name: "stderr"}

	if code := run([]string{"version"}, stdout, stderr); code != 0 {
		t.Fatalf("gc version exit = %d, want 0", code)
	}
	assertProductMetricsInvocationOrder(t, spy, productmetrics.CommandVersion, "stdout")
}

func TestProductMetricsLifecycleHelpFirstDropAttemptsOnceBeforeOutput(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	withProductMetricsInvocationSpy(t, spy)
	stdout := &productMetricsOrderingWriter{spy: spy, name: "stdout"}
	stderr := &productMetricsOrderingWriter{spy: spy, name: "stderr"}

	if code := run([]string{"help"}, stdout, stderr); code != 0 {
		t.Fatalf("gc help exit = %d, want 0", code)
	}
	assertProductMetricsInvocationOrder(t, spy, productmetrics.CommandHelp, "stdout")
}

func TestProductMetricsLifecycleValidationErrorsAttemptBeforeOutput(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	tests := []struct {
		name               string
		args               []string
		wantID             productmetrics.CommandID
		wantNoticeEligible bool
	}{
		{name: "flag", args: []string{"version", "--not-a-flag"}, wantID: productmetrics.CommandVersion},
		{name: "arg", args: []string{"version", "unexpected"}, wantID: productmetrics.CommandVersion},
		{name: "flag group", args: []string{"graph", "--mermaid", "--tree"}, wantID: productMetricsGeneratedCommandID60},
		{name: "deferred root empty scope", args: []string{"--city=", "definitely-not-a-command"}, wantID: productmetrics.CommandUnknown, wantNoticeEligible: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
			withProductMetricsInvocationSpy(t, spy)
			stdout := &productMetricsOrderingWriter{spy: spy, name: "stdout"}
			stderr := &productMetricsOrderingWriter{spy: spy, name: "stderr"}
			if code := run(test.args, stdout, stderr); code == 0 {
				t.Fatalf("gc %v exit = 0, want failure", test.args)
			}
			assertProductMetricsInvocationOrder(t, spy, test.wantID, "stderr")
			if test.wantNoticeEligible {
				notices := spy.noticeInvocations()
				if len(notices) != 1 || !notices[0].NoticeEligible {
					t.Fatalf("notice inputs = %+v, want one eligible notice", notices)
				}
			}
		})
	}
}

func TestProductMetricsLifecycleEarlyJSONAttemptsBeforeOutput(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	tests := []struct {
		name     string
		args     []string
		wantID   productmetrics.CommandID
		wantExit int
	}{
		{name: "schema", args: []string{"version", "--json-schema"}, wantID: productmetrics.CommandVersion},
		{name: "unsupported contract", args: []string{"completion", "bash", "--json"}, wantID: productMetricsGeneratedCommandID20, wantExit: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
			withProductMetricsInvocationSpy(t, spy)
			stdout := &productMetricsOrderingWriter{spy: spy, name: "stdout"}
			stderr := &productMetricsOrderingWriter{spy: spy, name: "stderr"}
			if code := run(test.args, stdout, stderr); code != test.wantExit {
				t.Fatalf("gc %v exit = %d, want %d", test.args, code, test.wantExit)
			}
			assertProductMetricsInvocationOrder(t, spy, test.wantID, "stdout")
		})
	}
}

func TestProductMetricsLifecycleEarlyJSONOutcomesAreClosedAndTyped(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	tests := []struct {
		name       string
		args       []string
		wantKind   productMetricsEarlyOutcomeKind
		wantID     productmetrics.CommandID
		wantHandle bool
		wantExit   int
	}{
		{name: "schema", args: []string{"version", "--json-schema"}, wantKind: productMetricsEarlyOutcomeJSONSchema, wantID: productmetrics.CommandVersion, wantHandle: true},
		{name: "missing schema command", args: []string{"does-not-exist", "--json-schema"}, wantKind: productMetricsEarlyOutcomeJSONSchema, wantID: productmetrics.CommandUnknown, wantHandle: true, wantExit: 1},
		{name: "unsupported contract", args: []string{"completion", "bash", "--json"}, wantKind: productMetricsEarlyOutcomeJSONContractFailure, wantID: productMetricsGeneratedCommandID20, wantHandle: true, wantExit: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classification := classifyProductMetricsCommand(root, test.args, productMetricsPolicyContext{})
			action, ok := prepareJSONEarlyAction(root, test.args)
			if !ok {
				t.Fatalf("prepare early action for %v = not handled", test.args)
			}
			outcome := resolveProductMetricsEarlyOutcome(action, classification)
			if outcome.kind != test.wantKind || outcome.handled != test.wantHandle || outcome.exitCode != test.wantExit || outcome.classification.ID != test.wantID {
				t.Fatalf("early outcome = %+v, want kind=%d handled=%t exit=%d command=%d", outcome, test.wantKind, test.wantHandle, test.wantExit, test.wantID)
			}
		})
	}
}

func TestProductMetricsLifecyclePreparedEarlyJSONDoesNotReresolve(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "0")
	schemaDir := filepath.Join(t.TempDir(), "schemas")
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{
		Use:         "packcmd",
		Annotations: map[string]string{jsonSchemaDirAnnotation: schemaDir},
	})
	action, ok := prepareJSONEarlyAction(root, []string{"packcmd", "--json"})
	if !ok || action.kind != jsonPreparedEarlyContractWarning || action.handled {
		t.Fatalf("prepared action = %+v ok=%t, want passthrough warning", action, ok)
	}
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "result.schema.json"), []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	handled, code := action.execute(&stdout, &stderr)
	if handled || code != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "does not declare JSON support") {
		t.Fatalf("prepared warning after schema mutation = handled:%t code:%d stdout:%q stderr:%q", handled, code, stdout.String(), stderr.String())
	}
}

func TestProductMetricsLifecycleEarlyJSONWriterFailurePreservesExitAndOrdering(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	withProductMetricsInvocationSpy(t, spy)
	stdout := productMetricsFailOrderingWriter{spy: spy, name: "stdout"}

	if code := run([]string{"version", "--json-schema"}, stdout, io.Discard); code != 1 {
		t.Fatalf("gc version --json-schema writer failure exit = %d, want 1", code)
	}
	events, recordedIDs := spy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != productmetrics.CommandVersion || eventIndex(events, "record") >= eventIndex(events, "stdout") {
		t.Fatalf("writer-failure lifecycle = ids:%v events:%v, want version attempt before failed write", recordedIDs, events)
	}
}

func TestProductMetricsLifecycleFailuresPreserveOutputAndOTel(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	healthyFactory := func() (productMetricsControlService, error) {
		return &productMetricsInvocationSpy{recordResult: productmetrics.RecordStored}, nil
	}
	factoryFailure := func() (productMetricsControlService, error) {
		return nil, errors.New("private factory failure")
	}
	rootExecutions := 0
	capture := func(
		t *testing.T,
		args []string,
		runner func([]string, io.Writer, io.Writer) int,
		factory func() (productMetricsControlService, error),
		closePermit func(productmetrics.RecordingPermit) error,
	) productMetricsCapturedRun {
		t.Helper()
		rootExecutions++
		captured, telemetry := captureProductMetricsLifecycleRunWithRunner(t, args, runner, factory, closePermit)
		assertProductMetricsTelemetryRun(t, telemetry)
		return captured
	}
	assertSameResult := func(t *testing.T, got, baseline productMetricsCapturedRun) {
		t.Helper()
		if got != baseline {
			t.Fatalf("product-metrics failure changed command result:\n got = %#v\n baseline = %#v", got, baseline)
		}
	}

	scenarios := []struct {
		name    string
		factory func() (productMetricsControlService, error)
		close   func(productmetrics.RecordingPermit) error
	}{
		{name: "nil service", factory: func() (productMetricsControlService, error) { return nil, nil }},
		{name: "factory error", factory: factoryFailure},
		{name: "wrong lifecycle interface", factory: func() (productMetricsControlService, error) {
			return &fakeProductMetricsControlService{}, nil
		}},
		{name: "notice failure", factory: func() (productMetricsControlService, error) {
			return &productMetricsNoticeFailureService{productMetricsInvocationSpy: productMetricsInvocationSpy{recordResult: productmetrics.RecordStored}}, nil
		}},
		{name: "record dropped", factory: func() (productMetricsControlService, error) {
			return &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}, nil
		}},
		{name: "permit close failure", factory: func() (productMetricsControlService, error) {
			return &productMetricsInvocationSpy{recordResult: productmetrics.RecordStored}, nil
		}, close: func(productmetrics.RecordingPermit) error { return errors.New("private close failure") }},
	}

	t.Run("failure modes", func(t *testing.T) {
		args := []string{"help"}
		baseline := capture(t, args, run, healthyFactory, nil)
		for _, scenario := range scenarios {
			t.Run(scenario.name, func(t *testing.T) {
				assertSameResult(t, capture(t, args, run, scenario.factory, scenario.close), baseline)
			})
		}
	})

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "direct success", args: []string{"version"}},
		{name: "direct Cobra failure", args: []string{"version", "--not-a-flag"}},
		{name: "buffered JSON success", args: []string{"version", "--json"}},
		{name: "buffered JSON failure", args: []string{"config", "explain", "--json"}},
		{name: "reported JSON failure", args: []string{"events", "--json", "--after", "1", "--after-cursor", "x"}},
		{name: "early JSON schema", args: []string{"version", "--json-schema"}},
		{name: "early JSON contract failure", args: []string{"completion", "bash", "--json"}},
	} {
		t.Run("output control/"+test.name, func(t *testing.T) {
			baseline := capture(t, test.args, run, healthyFactory, nil)
			assertSameResult(t, capture(t, test.args, run, factoryFailure, nil), baseline)
		})
	}

	city := setupPackExitCity(t)
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(city); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })
	packArgs := []string{"backstage", "hello"}
	for _, discovery := range []string{"eager", "lazy"} {
		t.Run("pack action/"+discovery, func(t *testing.T) {
			runner := func(args []string, stdout, stderr io.Writer) int {
				return runWithRootCommandOptions(args, stdout, stderr, packCommandScenarioRootOptions(t, discovery, args))
			}
			baseline := capture(t, packArgs, runner, healthyFactory, nil)
			assertSameResult(t, capture(t, packArgs, runner, factoryFailure, nil), baseline)
		})
	}

	if rootExecutions != 25 {
		t.Fatalf("command-root executions = %d, want 25", rootExecutions)
	}
}

func TestProductMetricsLifecyclePendingNoticeIsOnlyOutputDelta(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	args := []string{"help"}
	baseline, baselineTelemetry := captureProductMetricsLifecycleRun(t, args, func() (productMetricsControlService, error) { return nil, nil }, nil)
	assertProductMetricsTelemetryRun(t, baselineTelemetry)
	const completeNotice = "COMPLETE PRODUCT METRICS NOTICE\n"
	withNotice, noticeTelemetry := captureProductMetricsLifecycleRun(t, args, func() (productMetricsControlService, error) {
		return &productMetricsNoticeWritingService{notice: completeNotice}, nil
	}, nil)
	assertProductMetricsTelemetryRun(t, noticeTelemetry)
	if withNotice.code != baseline.code || withNotice.stdout != baseline.stdout || withNotice.stderr != completeNotice+baseline.stderr {
		t.Fatalf("notice delta = %#v, baseline %#v; want exact complete stderr prefix only", withNotice, baseline)
	}
}

func TestProductMetricsLifecyclePanicRunsPermitCloseAndOTelShutdown(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	withProductMetricsInvocationSpy(t, spy)
	telemetrySpy := withProductMetricsTelemetrySpy(t)
	originalClose := closeProductMetricsRecordingPermit
	closeCalls := 0
	closeProductMetricsRecordingPermit = func(productmetrics.RecordingPermit) error {
		closeCalls++
		return errors.New("injected close failure")
	}
	t.Cleanup(func() { closeProductMetricsRecordingPermit = originalClose })
	panicValue := &struct{ marker string }{marker: "writer panic"}

	defer func() {
		if recovered := recover(); recovered != panicValue {
			t.Fatalf("recovered panic = %#v, want original %#v", recovered, panicValue)
		}
		if closeCalls != 1 {
			t.Fatalf("permit close calls = %d, want 1", closeCalls)
		}
		assertProductMetricsTelemetryRun(t, telemetrySpy)
		_, recordedIDs := spy.snapshot()
		if len(recordedIDs) != 1 || recordedIDs[0] != productmetrics.CommandVersion {
			t.Fatalf("panic recorded IDs = %v, want [version]", recordedIDs)
		}
	}()
	_ = run([]string{"version"}, productMetricsPanicWriter{value: panicValue}, io.Discard)
}

type productMetricsCapturedRun struct {
	code   int
	stdout string
	stderr string
}

func captureProductMetricsLifecycleRun(
	t *testing.T,
	args []string,
	factory func() (productMetricsControlService, error),
	closePermit func(productmetrics.RecordingPermit) error,
) (productMetricsCapturedRun, *productMetricsTelemetrySpy) {
	t.Helper()
	return captureProductMetricsLifecycleRunWithRunner(t, args, run, factory, closePermit)
}

func captureProductMetricsLifecycleRunWithRunner(
	t *testing.T,
	args []string,
	runner func([]string, io.Writer, io.Writer) int,
	factory func() (productMetricsControlService, error),
	closePermit func(productmetrics.RecordingPermit) error,
) (productMetricsCapturedRun, *productMetricsTelemetrySpy) {
	t.Helper()
	originalFactory := productMetricsControlServiceFactory
	originalClose := closeProductMetricsRecordingPermit
	originalInit := initializeCLITelemetry
	originalAttrs := setCLIProcessOTELAttrs
	defer func() {
		productMetricsControlServiceFactory = originalFactory
		closeProductMetricsRecordingPermit = originalClose
		initializeCLITelemetry = originalInit
		setCLIProcessOTELAttrs = originalAttrs
	}()
	productMetricsControlServiceFactory = factory
	if closePermit != nil {
		closeProductMetricsRecordingPermit = closePermit
	}
	telemetrySpy := &productMetricsTelemetrySpy{}
	initializeCLITelemetry = func(context.Context, string, string) (cliTelemetryShutdowner, error) {
		telemetrySpy.mu.Lock()
		telemetrySpy.initCalls++
		telemetrySpy.mu.Unlock()
		return telemetrySpy, nil
	}
	setCLIProcessOTELAttrs = func() {
		telemetrySpy.mu.Lock()
		telemetrySpy.attributeCalls++
		telemetrySpy.mu.Unlock()
	}
	var stdout, stderr strings.Builder
	code := runner(args, &stdout, &stderr)
	return productMetricsCapturedRun{code: code, stdout: stdout.String(), stderr: stderr.String()}, telemetrySpy
}

func withProductMetricsTelemetrySpy(t *testing.T) *productMetricsTelemetrySpy {
	t.Helper()
	originalInit := initializeCLITelemetry
	originalAttrs := setCLIProcessOTELAttrs
	spy := &productMetricsTelemetrySpy{}
	initializeCLITelemetry = func(context.Context, string, string) (cliTelemetryShutdowner, error) {
		spy.mu.Lock()
		spy.initCalls++
		spy.mu.Unlock()
		return spy, nil
	}
	setCLIProcessOTELAttrs = func() {
		spy.mu.Lock()
		spy.attributeCalls++
		spy.mu.Unlock()
	}
	t.Cleanup(func() {
		initializeCLITelemetry = originalInit
		setCLIProcessOTELAttrs = originalAttrs
	})
	return spy
}

func assertProductMetricsTelemetryRun(t *testing.T, spy *productMetricsTelemetrySpy) {
	t.Helper()
	initCalls, attributeCalls, shutdownCalls, shutdownTimed := spy.snapshot()
	if initCalls != 1 || attributeCalls != 1 || shutdownCalls != 1 || !shutdownTimed {
		t.Fatalf("OTel lifecycle = init:%d attrs:%d shutdown:%d timed:%t, want 1/1/1/true", initCalls, attributeCalls, shutdownCalls, shutdownTimed)
	}
}

func TestProductMetricsLifecycleBindingRetainsOnlyClosedState(t *testing.T) {
	for _, retainedType := range []reflect.Type{
		reflect.TypeOf(productMetricsLifecycleBinding{}),
		reflect.TypeOf(productMetricsInvocationLifecycle{}),
		reflect.TypeOf(productMetricsEarlyOutcome{}),
		reflect.TypeOf(productMetricsFinalOutcome{}),
		reflect.TypeOf(productMetricsDeferredOutcome{}),
	} {
		for index := 0; index < retainedType.NumField(); index++ {
			field := retainedType.Field(index)
			lowerName := strings.ToLower(field.Name)
			for _, forbidden := range []string{"arg", "path", "command", "error", "writer", "output"} {
				if strings.Contains(lowerName, forbidden) {
					t.Fatalf("%s field %q retains forbidden %s state", retainedType, field.Name, forbidden)
				}
			}
			if field.Type.Kind() == reflect.Func || field.Type.Kind() == reflect.Slice || field.Type == reflect.TypeOf((*error)(nil)).Elem() {
				t.Fatalf("%s field %q has forbidden type %v", retainedType, field.Name, field.Type)
			}
			if field.Type == reflect.TypeOf(productMetricsDeferredAction{}) || field.Type == reflect.TypeOf(&productMetricsDeferredAction{}) {
				t.Fatalf("%s field %q retains stack-local deferred action", retainedType, field.Name)
			}
		}
	}
}

func TestProductMetricsLifecyclePackActionReportsMinimizedOutcomeBeforeInvoke(t *testing.T) {
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	lifecycle := &productMetricsInvocationLifecycle{service: spy}
	action := resolvedPackCommandAction(func() int {
		spy.appendEvent("invoke")
		return 0
	})
	outcome := action.executeReporting(lifecycle.attemptPackOutcome)
	if !outcome.handled || outcome.classification != packCommandClassification || outcome.exitCode != 0 {
		t.Fatalf("pack outcome = %+v, want handled minimized pack success", outcome)
	}
	// A final-funnel retry after RecordOnce dropped must not reclassify.
	lifecycle.attemptClassification(classificationFromSynthetic(""))
	events, recordedIDs := spy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != productmetrics.CommandPackCommand {
		t.Fatalf("recorded IDs = %v, want one pack-command attempt; events=%v", recordedIDs, events)
	}
	record, invoke := eventIndex(events, "record"), eventIndex(events, "invoke")
	if record < 0 || invoke < 0 || record >= invoke {
		t.Fatalf("pack action order = %v, want minimized outcome attempt before invoke", events)
	}
}

func TestProductMetricsLifecycleRealPackDispatchMatrix(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	city := setupPackExitCity(t)
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(city); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })
	tests := []struct {
		name       string
		args       []string
		wantID     productmetrics.CommandID
		wantExit   int
		wantOutput string
	}{
		{name: "success", args: []string{"backstage", "repo", "sync"}, wantID: productmetrics.CommandPackCommand, wantOutput: "stdout"},
		{name: "nonzero", args: []string{"backstage", "hello"}, wantID: productmetrics.CommandPackCommand, wantExit: 42, wantOutput: "stdout"},
		{name: "leaf help", args: []string{"backstage", "hello", "--help"}, wantID: productmetrics.CommandPackCommand, wantOutput: "stdout"},
		{name: "group help", args: []string{"backstage"}, wantID: productmetrics.CommandPackCommand, wantOutput: "stdout"},
		{name: "materialized group unknown", args: []string{"backstage", "missing"}, wantID: productmetrics.CommandPackCommand, wantExit: 1, wantOutput: "stderr"},
	}
	for _, scenario := range []string{"eager", "lazy"} {
		for _, test := range tests {
			t.Run(scenario+"/"+test.name, func(t *testing.T) {
				spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
				withProductMetricsInvocationSpy(t, spy)
				stdout := &productMetricsOrderingWriter{spy: spy, name: "stdout"}
				stderr := &productMetricsOrderingWriter{spy: spy, name: "stderr"}
				options := packCommandScenarioRootOptions(t, scenario, test.args)
				if code := runWithRootCommandOptions(test.args, stdout, stderr, options); code != test.wantExit {
					t.Fatalf("%s gc %v exit = %d, want %d; stdout=%q stderr=%q", scenario, test.args, code, test.wantExit, stdout.String(), stderr.String())
				}
				assertProductMetricsInvocationOrder(t, spy, test.wantID, test.wantOutput)
				_, recordedIDs := spy.snapshot()
				metricsBoundary := fmt.Sprintf("permit=%+v ids=%v", spy.permitInvocations(), recordedIDs)
				for _, secret := range []string{"backstage", "hello", "repo", "sync", "missing", city} {
					if strings.Contains(metricsBoundary, secret) {
						t.Fatalf("metrics boundary leaked pack secret %q in %q", secret, metricsBoundary)
					}
				}
			})
		}
	}
}

func TestProductMetricsLifecycleConfigChangeFallbackReportsBeforeInvoke(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	tests := []struct {
		name       string
		args       []string
		wantID     productmetrics.CommandID
		wantExit   int
		wantOutput string
		wantNotice bool
	}{
		{name: "late pack success", args: []string{"latepack", "hello"}, wantID: productmetrics.CommandPackCommand, wantOutput: "stdout"},
		{name: "late pack selected unknown", args: []string{"latepack", "missing"}, wantID: productmetrics.CommandUnknown, wantExit: 1, wantOutput: "stderr"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workingDirectory := t.TempDir()
			oldWorkingDirectory, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(workingDirectory); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })
			spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
			withProductMetricsInvocationSpy(t, spy)
			stdout := &productMetricsOrderingWriter{spy: spy, name: "stdout"}
			stderr := &productMetricsOrderingWriter{spy: spy, name: "stderr"}
			lifecycle := openProductMetricsInvocationLifecycle(test.args)
			defer lifecycle.Close()
			root := newRootCmdWithOptions(stdout, stderr, rootCommandOptions{})
			root.SetArgs(test.args)
			root.SetOut(stdout)
			root.SetErr(stderr)
			// The initial lazy materialization observes no city or pack.
			materializePackCommandTreeForArgs(root, test.args, stdout, stderr)
			if findSubcommand(root, "latepack") != nil {
				t.Fatal("initial materialization unexpectedly found latepack")
			}
			binding := bindProductMetricsInvocationLifecycle(root, test.args, lifecycle)
			if got := binding.classification; got.ID != productmetrics.CommandUnknown || got.Owner != productMetricsOwnerDeferred ||
				got.Resolver != productMetricsResolverRootDispatch || got.Notice != productMetricsNoticeEligible {
				t.Fatalf("initial late fallback classification = %+v, want eligible deferred root unknown", got)
			}
			lifecycle.prepareNotice(binding.classification, stderr)
			if notices := spy.noticeInvocations(); len(notices) != 0 {
				t.Fatalf("initial unresolved root emitted notice before typed fallback: %+v", notices)
			}
			writeLateProductMetricsPackFixture(t, workingDirectory)
			executed, executeErr := root.ExecuteC()
			if executed != root {
				t.Fatalf("late fallback ExecuteC command = %v, want root dispatcher", executed)
			}
			lifecycle.attemptFinalOutcome(resolveProductMetricsFinalOutcome(executed, binding.classification))
			if code := commandExitCode(executeErr); code != test.wantExit {
				t.Fatalf("late fallback gc %v exit = %d, want %d; stdout=%q stderr=%q", test.args, code, test.wantExit, stdout.String(), stderr.String())
			}
			assertProductMetricsInvocationOrder(t, spy, test.wantID, test.wantOutput)
			notices := spy.noticeInvocations()
			if len(notices) != 1 || notices[0].NoticeEligible != test.wantNotice {
				t.Fatalf("late fallback notice inputs = %+v, want one eligible=%t typed outcome", notices, test.wantNotice)
			}
		})
	}
}

func TestProductMetricsLifecycleGenuineUnknownRetainsNoticeEligibility(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	withProductMetricsInvocationSpy(t, spy)
	if code := run([]string{"genuine-unknown-command"}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("genuine unknown exit = %d, want 1", code)
	}
	notices := spy.noticeInvocations()
	if len(notices) != 1 || !notices[0].NoticeEligible {
		t.Fatalf("genuine unknown notice inputs = %+v, want one eligible notice", notices)
	}
	_, recordedIDs := spy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != productmetrics.CommandUnknown {
		t.Fatalf("genuine unknown recorded IDs = %v, want [unknown]", recordedIDs)
	}
}

func TestProductMetricsLifecycleUnknownBadFlagKeepsResolvedHelpNotice(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	withProductMetricsInvocationSpy(t, spy)
	stdout := &productMetricsOrderingWriter{spy: spy, name: "stdout"}
	stderr := &productMetricsOrderingWriter{spy: spy, name: "stderr"}
	args := []string{"genuine-unknown-command", "--bad-flag"}
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	classification := classifyProductMetricsCommand(root, args, productMetricsPolicyContext{})
	if classification.ID != productmetrics.CommandHelp || classification.Owner != productMetricsOwnerDeferred || classification.Notice != productMetricsNoticeEligible {
		t.Fatalf("unknown bad-flag classification = %+v, want eligible deferred help", classification)
	}
	if code := run(args, stdout, stderr); code != 1 {
		t.Fatalf("unknown bad-flag exit = %d, want 1", code)
	}
	assertProductMetricsInvocationOrder(t, spy, productmetrics.CommandHelp, "stderr")
	notices := spy.noticeInvocations()
	if len(notices) != 1 || !notices[0].NoticeEligible {
		t.Fatalf("unknown bad-flag notice inputs = %+v, want one eligible notice", notices)
	}
}

func writeLateProductMetricsPackFixture(t *testing.T, city string) {
	t.Helper()
	commandDir := filepath.Join(city, "commands", "hello")
	if err := os.MkdirAll(commandDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(city, "city.toml"):          "[workspace]\nname = \"late-city\"\n",
		filepath.Join(city, "pack.toml"):          "[pack]\nname = \"latepack\"\nschema = 2\n",
		filepath.Join(commandDir, "run.sh"):       "#!/bin/sh\nprintf 'late-pack-invoked\\n'\n",
		filepath.Join(commandDir, "command.toml"): "description = \"Late pack command\"\n",
		filepath.Join(commandDir, "help.md"):      "Late pack help.\n",
	} {
		mode := os.FileMode(0o600)
		if strings.HasSuffix(path, "run.sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProductMetricsLifecycleConcurrentFirstAttemptWinsEvenWhenDropped(t *testing.T) {
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	lifecycle := &productMetricsInvocationLifecycle{service: spy}
	help := productMetricsClassification{ID: productmetrics.CommandHelp, Recording: productMetricsRecordingRecordable}
	version := productMetricsClassification{ID: productmetrics.CommandVersion, Recording: productMetricsRecordingRecordable}
	unknown := productMetricsClassification{ID: productmetrics.CommandUnknown, Recording: productMetricsRecordingRecordable}
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < 128; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			switch index % 5 {
			case 0:
				lifecycle.attemptClassification(help)
			case 1:
				lifecycle.attemptFinalOutcome(productMetricsFinalOutcome{classification: version})
			case 2:
				lifecycle.attemptDeferredOutcome(productMetricsDeferredOutcome{classification: unknown})
			case 3:
				lifecycle.attemptEarlyOutcome(productMetricsEarlyOutcome{kind: productMetricsEarlyOutcomeJSONSchema, classification: help})
			case 4:
				lifecycle.attemptPackOutcome(packCommandOutcome{handled: true, classification: packCommandClassification})
			}
		}()
	}
	close(start)
	waitDone := make(chan struct{})
	go func() {
		wait.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("concurrent first-attempt workers did not finish")
	}
	_, recordedIDs := spy.snapshot()
	if len(recordedIDs) != 1 {
		t.Fatalf("concurrent recorded IDs = %v, want exactly one first attempt", recordedIDs)
	}
	firstID := recordedIDs[0]
	if firstID != productmetrics.CommandHelp && firstID != productmetrics.CommandVersion && firstID != productmetrics.CommandUnknown && firstID != productmetrics.CommandPackCommand {
		t.Fatalf("concurrent first ID = %d, want one closed candidate", firstID)
	}

	// A dropped winner remains authoritative; later callbacks cannot retry or
	// replace it with a different classification.
	lifecycle.attemptClassification(version)
	lifecycle.attemptPackOutcome(packCommandOutcome{handled: true, classification: packCommandClassification})
	_, recordedIDs = spy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != firstID {
		t.Fatalf("post-drop recorded IDs = %v, want unchanged first ID %d", recordedIDs, firstID)
	}
}

func TestProductMetricsLifecycleExecutionPhasesAttemptBeforeOutput(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*cobra.Command, *productMetricsInvocationSpy)
	}{
		{
			name: "persistent pre-run error",
			configure: func(command *cobra.Command, spy *productMetricsInvocationSpy) {
				command.PersistentPreRunE = func(*cobra.Command, []string) error {
					spy.appendEvent("phase-output")
					return errors.New("persistent pre-run failure")
				}
			},
		},
		{
			name:      "pre-run error",
			configure: func(*cobra.Command, *productMetricsInvocationSpy) {},
		},
		{
			name:      "handler error",
			configure: func(*cobra.Command, *productMetricsInvocationSpy) {},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
			root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
			leaf, _, findErr := root.Find([]string{"version"})
			if findErr != nil || leaf == nil || leaf == root {
				t.Fatalf("resolve version = command:%v err:%v", leaf, findErr)
			}
			switch test.name {
			case "pre-run error":
				leaf.PreRunE = func(*cobra.Command, []string) error {
					spy.appendEvent("phase-output")
					return errors.New("pre-run failure")
				}
				leaf.RunE = func(*cobra.Command, []string) error {
					t.Fatal("handler ran after pre-run failure")
					return nil
				}
				leaf.Run = nil
			case "handler error":
				leaf.RunE = func(*cobra.Command, []string) error {
					spy.appendEvent("phase-output")
					return errors.New("handler failure")
				}
				leaf.Run = nil
			default:
				leaf.RunE = func(*cobra.Command, []string) error {
					t.Fatal("handler ran after persistent pre-run failure")
					return nil
				}
				leaf.Run = nil
			}
			test.configure(leaf, spy)
			root.SetArgs([]string{"version"})
			installProductMetricsInvocationWrappers(root, productMetricsLifecycleBinding{
				lifecycle: &productMetricsInvocationLifecycle{service: spy},
				classification: productMetricsClassification{
					ID:        productmetrics.CommandVersion,
					Recording: productMetricsRecordingRecordable,
					Owner:     productMetricsOwnerImmediate,
				},
			})

			if _, err := root.ExecuteC(); err == nil {
				t.Fatal("ExecuteC() error = nil, want injected failure")
			}
			events, recordedIDs := spy.snapshot()
			if len(recordedIDs) != 1 || recordedIDs[0] != productmetrics.CommandVersion {
				t.Fatalf("recorded IDs = %v, want [version]; events=%v", recordedIDs, events)
			}
			if record, output := eventIndex(events, "record"), eventIndex(events, "phase-output"); record < 0 || output < 0 || record >= output {
				t.Fatalf("execution order = %v, want record before phase output", events)
			}
		})
	}
}

func TestProductMetricsLifecycleResolvedCommandWinsFinalOutcome(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	resolved, _, err := root.Find([]string{"status"})
	if err != nil || resolved == nil || resolved == root {
		t.Fatalf("resolve status = command:%v err:%v", resolved, err)
	}
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	binding := productMetricsLifecycleBinding{
		lifecycle: &productMetricsInvocationLifecycle{service: spy},
		classification: productMetricsClassification{
			ID:        productmetrics.CommandVersion,
			Notice:    productMetricsNoticeIneligible,
			Recording: productMetricsRecordingRecordable,
			Owner:     productMetricsOwnerImmediate,
		},
	}
	bindingContext := context.WithValue(context.Background(), productMetricsLifecycleContextKey{}, binding)
	root.SetContext(bindingContext)
	resolved.SetContext(bindingContext)

	attemptProductMetricsForCommand(resolved)
	_, recordedIDs := spy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != productMetricsGeneratedCommandID163 {
		t.Fatalf("resolved-command attempt IDs = %v, want [status] rather than stale pre-scan version", recordedIDs)
	}
}

func TestProductMetricsLifecycleBindingFallsBackToRootContext(t *testing.T) {
	type childContextKey struct{}
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	lifecycle := &productMetricsInvocationLifecycle{service: spy}
	binding := productMetricsLifecycleBinding{
		lifecycle:      lifecycle,
		classification: classificationFromSyntheticPack(),
	}
	root := &cobra.Command{Use: "gc"}
	child := &cobra.Command{Use: "pack-child"}
	child.SetContext(context.WithValue(context.Background(), childContextKey{}, "preserved"))
	root.AddCommand(child)
	root.SetContext(context.WithValue(context.Background(), productMetricsLifecycleContextKey{}, binding))
	action := resolvedPackCommandAction(func() int {
		spy.appendEvent("invoke")
		return 0
	})

	executeProductMetricsPackAction(child, action)
	if got := child.Context().Value(childContextKey{}); got != "preserved" {
		t.Fatalf("child context value = %v, want preserved", got)
	}
	events, recordedIDs := spy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != productmetrics.CommandPackCommand || eventIndex(events, "record") >= eventIndex(events, "invoke") {
		t.Fatalf("root fallback lifecycle = ids:%v events:%v, want pack record before invoke", recordedIDs, events)
	}
}

func TestProductMetricsLifecycleInstallerRespectsAnnotatedOwner(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		wantID productmetrics.CommandID
	}{
		{name: "immediate", args: []string{"version"}, wantID: productmetrics.CommandVersion},
		{name: "deferred", args: []string{"analyze"}, wantID: productmetrics.CommandHelp},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
			root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
			command, _, findErr := root.Find(test.args)
			if findErr != nil || command == nil || command == root {
				t.Fatalf("resolve %v = command:%v err:%v", test.args, command, findErr)
			}
			command.Run = nil
			command.RunE = func(*cobra.Command, []string) error {
				spy.appendEvent("invoke")
				return nil
			}
			bindProductMetricsInvocationLifecycle(root, test.args, &productMetricsInvocationLifecycle{service: spy})
			if err := command.RunE(command, nil); err != nil {
				t.Fatalf("wrapped RunE() error = %v", err)
			}
			events, recordedIDs := spy.snapshot()
			if len(recordedIDs) != 1 || recordedIDs[0] != test.wantID || eventIndex(events, "record") >= eventIndex(events, "invoke") {
				t.Fatalf("%s owner lifecycle = ids:%v events:%v, want one typed attempt before invoke", test.name, recordedIDs, events)
			}
		})
	}
}

func TestProductMetricsLifecycleHandlerPanicPropagatesAfterAttempt(t *testing.T) {
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	leaf, _, findErr := root.Find([]string{"version"})
	if findErr != nil || leaf == nil || leaf == root {
		t.Fatalf("resolve version = command:%v err:%v", leaf, findErr)
	}
	panicValue := &struct{ marker string }{marker: "original-panic"}
	leaf.RunE = nil
	leaf.Run = func(*cobra.Command, []string) {
		spy.appendEvent("handler")
		panic(panicValue)
	}
	root.SetArgs([]string{"version"})
	installProductMetricsInvocationWrappers(root, productMetricsLifecycleBinding{
		lifecycle: &productMetricsInvocationLifecycle{service: spy},
		classification: productMetricsClassification{
			ID:        productmetrics.CommandVersion,
			Recording: productMetricsRecordingRecordable,
		},
	})

	defer func() {
		if recovered := recover(); recovered != panicValue {
			t.Fatalf("recovered panic = %#v, want original %#v", recovered, panicValue)
		}
		events, recordedIDs := spy.snapshot()
		if len(recordedIDs) != 1 || eventIndex(events, "record") >= eventIndex(events, "handler") {
			t.Fatalf("panic lifecycle = ids %v events %v, want one attempt before handler", recordedIDs, events)
		}
	}()
	_, _ = root.ExecuteC()
}

func TestProductMetricsLifecycleLongRunningHandlerAttemptsBeforeWaiting(t *testing.T) {
	spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
	entered := make(chan struct{})
	release := make(chan struct{})
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	leaf, _, findErr := root.Find([]string{"version"})
	if findErr != nil || leaf == nil || leaf == root {
		t.Fatalf("resolve version = command:%v err:%v", leaf, findErr)
	}
	leaf.Run = nil
	leaf.RunE = func(*cobra.Command, []string) error {
		close(entered)
		<-release
		return nil
	}
	root.SetArgs([]string{"version"})
	installProductMetricsInvocationWrappers(root, productMetricsLifecycleBinding{
		lifecycle: &productMetricsInvocationLifecycle{service: spy},
		classification: productMetricsClassification{
			ID:        productmetrics.CommandVersion,
			Recording: productMetricsRecordingRecordable,
		},
	})

	done := make(chan error, 1)
	go func() {
		_, err := root.ExecuteC()
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("handler did not start")
	}
	_, recordedIDs := spy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != productmetrics.CommandVersion {
		t.Fatalf("recorded IDs while handler blocked = %v, want [version]", recordedIDs)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ExecuteC() error = %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("handler did not finish after release")
	}
}

func TestProductMetricsLifecycleCommandPathMatrixAttemptsOnce(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	tests := []struct {
		name       string
		args       []string
		wantID     productmetrics.CommandID
		wantExit   int
		wantOutput string
		wantRecord bool
	}{
		{name: "bare root", wantID: productmetrics.CommandHelp, wantOutput: "stdout", wantRecord: true},
		{name: "explicit help", args: []string{"help"}, wantID: productmetrics.CommandHelp, wantOutput: "stdout", wantRecord: true},
		{name: "target help flag", args: []string{"status", "--help"}, wantID: productmetrics.CommandHelp, wantOutput: "stdout", wantRecord: true},
		{name: "target help command", args: []string{"help", "status"}, wantID: productmetrics.CommandHelp, wantOutput: "stdout", wantRecord: true},
		{name: "unknown root", args: []string{"definitely-not-a-command"}, wantID: productmetrics.CommandUnknown, wantExit: 1, wantOutput: "stderr", wantRecord: true},
		{name: "unknown nested", args: []string{"completion", "bogus"}, wantID: productmetrics.CommandUnknown, wantOutput: "stdout", wantRecord: true},
		{name: "version", args: []string{"version"}, wantID: productmetrics.CommandVersion, wantOutput: "stdout", wantRecord: true},
		{name: "user completion", args: []string{"completion", "bash"}, wantID: productMetricsGeneratedCommandID20, wantOutput: "stdout", wantRecord: true},
		{name: "private completion", args: []string{"__complete", "status"}},
		{name: "events failure", args: []string{"events", "--after", "1", "--after-cursor", "x"}, wantID: productMetricsGeneratedCommandID50, wantExit: 1, wantOutput: "stderr", wantRecord: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &productMetricsInvocationSpy{recordResult: productmetrics.RecordDropped}
			withProductMetricsInvocationSpy(t, spy)
			stdout := &productMetricsOrderingWriter{spy: spy, name: "stdout"}
			stderr := &productMetricsOrderingWriter{spy: spy, name: "stderr"}

			if code := run(test.args, stdout, stderr); code != test.wantExit {
				t.Fatalf("gc %v exit = %d, want %d; stdout=%q stderr=%q", test.args, code, test.wantExit, stdout.String(), stderr.String())
			}
			if test.wantRecord {
				if test.wantOutput == "" {
					_, recordedIDs := spy.snapshot()
					if len(recordedIDs) != 1 || recordedIDs[0] != test.wantID {
						t.Fatalf("recorded IDs = %v, want [%d]", recordedIDs, test.wantID)
					}
				} else {
					assertProductMetricsInvocationOrder(t, spy, test.wantID, test.wantOutput)
				}
				return
			}
			if _, recordedIDs := spy.snapshot(); len(recordedIDs) != 0 {
				t.Fatalf("excluded invocation recorded IDs = %v, want none", recordedIDs)
			}
		})
	}
}

func withProductMetricsInvocationSpy(t *testing.T, spy *productMetricsInvocationSpy) {
	t.Helper()
	original := productMetricsControlServiceFactory
	productMetricsControlServiceFactory = func() (productMetricsControlService, error) {
		spy.mu.Lock()
		spy.factoryCalls++
		spy.events = append(spy.events, "factory")
		spy.mu.Unlock()
		return spy, nil
	}
	t.Cleanup(func() { productMetricsControlServiceFactory = original })
}

func assertProductMetricsInvocationOrder(t *testing.T, spy *productMetricsInvocationSpy, wantID productmetrics.CommandID, outputEvent string) {
	t.Helper()
	events, recordedIDs := spy.snapshot()
	if len(recordedIDs) != 1 || recordedIDs[0] != wantID {
		t.Fatalf("recorded command IDs = %v, want exactly [%v]; events=%v", recordedIDs, wantID, events)
	}
	factory, permit, notice, record, output := eventIndex(events, "factory"), eventIndex(events, "permit"), eventIndex(events, "notice"), eventIndex(events, "record"), eventIndex(events, outputEvent)
	if factory < 0 || permit < 0 || notice < 0 || record < 0 || output < 0 || factory >= permit || permit >= notice || notice >= record || record >= output {
		t.Fatalf("invocation order = %v, want factory < permit < notice < record < %s", events, outputEvent)
	}
	factoryCalls, permitCalls := spy.counts()
	if factoryCalls != 1 || permitCalls != 1 {
		t.Fatalf("factory/permit calls = %d/%d, want 1/1", factoryCalls, permitCalls)
	}
}

func eventIndex(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}

var _ productMetricsControlService = (*productMetricsInvocationSpy)(nil)
