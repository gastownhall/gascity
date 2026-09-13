package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/productmetrics"
)

const disclosedMetricsInstallationID = "3cf9fd4e-3337-4c29-a0ab-2858cd8a1f21"

type fakeProductMetricsControlService struct {
	status             productmetrics.Status
	policy             productmetrics.PolicyMetadata
	disclosedID        string
	disclosedIDPresent bool
	disclosureCalls    int
	enableErr          error
	enableInvocation   productmetrics.InvocationContext
	enableHasDeadline  bool
	enableNotice       string
	disableResult      productmetrics.PurgeResult
	disableErr         error
	disableCalls       int
}

func (service *fakeProductMetricsControlService) Status(context.Context) productmetrics.Status {
	return service.status
}

func (service *fakeProductMetricsControlService) PolicyMetadata() productmetrics.PolicyMetadata {
	return service.policy
}

func (service *fakeProductMetricsControlService) InstallationIDForDisclosure(context.Context) (string, bool) {
	service.disclosureCalls++
	return service.disclosedID, service.disclosedIDPresent
}

func (service *fakeProductMetricsControlService) Enable(
	ctx context.Context,
	invocation productmetrics.InvocationContext,
	writer io.Writer,
) error {
	service.enableInvocation = invocation
	_, service.enableHasDeadline = ctx.Deadline()
	if service.enableNotice != "" {
		_, _ = io.WriteString(writer, service.enableNotice)
	}
	return service.enableErr
}

func (service *fakeProductMetricsControlService) DisableAndPurge(context.Context) (productmetrics.PurgeResult, error) {
	service.disableCalls++
	return service.disableResult, service.disableErr
}

func (service *fakeProductMetricsControlService) RecordingPermit(productmetrics.InvocationContext) productmetrics.RecordingPermit {
	return productmetrics.RecordingPermit{}
}

func (service *fakeProductMetricsControlService) RecordOnce(productmetrics.RecordingPermit, productmetrics.CommandID) productmetrics.RecordResult {
	return productmetrics.RecordDropped
}

func TestMetricsStatusDefaultAndJSONStayRedacted(t *testing.T) {
	service := &fakeProductMetricsControlService{
		status: productmetrics.Status{
			State:                      productmetrics.StateEnabled,
			Reason:                     productmetrics.ReasonEnabled,
			HomeStable:                 true,
			ConfigPath:                 "/home/alice/.gc/product-usage/config.toml",
			ConfigPresent:              true,
			StateSchema:                1,
			RequiredNoticeVersion:      2,
			AcceptedNoticeVersion:      2,
			InstallationIDPresent:      true,
			SpoolGenerationPresent:     true,
			QueueEvents:                3,
			QueueBytes:                 1536,
			QueueDiagnosticsAvailable:  true,
			OldestQueuedEventAge:       2*time.Hour + 3*time.Minute + 4*time.Second,
			OldestQueuedEventPresent:   true,
			DroppedEvents:              7,
			LastUploadAttemptHourUTC:   "2026-07-12T20:00:00Z",
			LastUploadSuccessHourUTC:   "2026-07-12T19:00:00Z",
			LastErrorClass:             productmetrics.DiagnosticErrorServer5xx,
			StatusDiagnosticsAvailable: true,
			SpawnThrottleAge:           45 * time.Second,
			SpawnThrottlePresent:       true,
		},
		policy: productmetrics.PolicyMetadata{
			EndpointHostname:         "metrics.gascity.example",
			PrivacyURL:               "https://gascity.example/privacy/command-usage",
			EdgeLogRetentionDays:     7,
			RawEventRetentionDays:    90,
			AggregateRetentionMonths: 13,
		},
		disclosedID:        disclosedMetricsInstallationID,
		disclosedIDPresent: true,
	}
	withProductMetricsControlService(t, service)

	stdout, stderr, err := executeMetricsCommand(t, "status")
	if err != nil || stderr != "" {
		t.Fatalf("metrics status = err:%v stderr:%q", err, stderr)
	}
	for _, want := range []string{
		"State: enabled (enabled)",
		"Home: stable",
		"State schema: 1; notice required/accepted: 2/2",
		"Installation ID: present (redacted)",
		"Queue: 3 events, 1.5 KiB, oldest 2h3m4s",
		"Last upload attempt: 2026-07-12T20:00:00Z",
		"Last error: server-5xx",
		"Fields sent: schema_version, event_id, installation_id, app, release_version, os, occurred_hour_utc, command_id.",
		"Fields never sent: arguments, flag values, paths, names, prompts, output, error text, exact timestamps, durations, outcomes, models, tokens, costs, or credentials.",
		"Gas City OTel, local costs, event export, and Beads telemetry are separate and unchanged.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text status missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, disclosedMetricsInstallationID) || service.disclosureCalls != 0 {
		t.Fatalf("default status disclosed ID: calls=%d output=%q", service.disclosureCalls, stdout)
	}

	stdout, stderr, err = executeMetricsCommand(t, "status", "--json")
	if err != nil || stderr != "" {
		t.Fatalf("metrics status --json = err:%v stderr:%q", err, stderr)
	}
	wantJSON := `{"ok":true,"state":"enabled","reason":"enabled","home_stable":true,"home_reason":null,"config_path":"/home/alice/.gc/product-usage/config.toml","config_present":true,"state_schema":1,"required_notice_version":2,"accepted_notice_version":2,"endpoint_hostname":"metrics.gascity.example","installation_id_present":true,"spool_generation_present":true,"cleanup_pending":false,"queue":{"available":true,"events":3,"bytes":1536,"oldest_age_seconds":7384},"diagnostics":{"available":true,"dropped_events":7,"last_upload_attempt_hour_utc":"2026-07-12T20:00:00Z","last_upload_success_hour_utc":"2026-07-12T19:00:00Z","last_error_class":"server-5xx","spawn_throttle_age_seconds":45},"retention":{"edge_log_days":7,"raw_event_days":90,"aggregate_months":13,"privacy_url":"https://gascity.example/privacy/command-usage"},"independence":"Gas City OTel, local costs, event export, and Beads telemetry are separate and unchanged."}` + "\n"
	if stdout != wantJSON || strings.Contains(stdout, disclosedMetricsInstallationID) || service.disclosureCalls != 0 {
		t.Fatalf("JSON status = %q, disclosure calls=%d; want %q", stdout, service.disclosureCalls, wantJSON)
	}
}

func TestMetricsStatusInstallationIDDisclosureIsExplicitTextOnly(t *testing.T) {
	service := &fakeProductMetricsControlService{
		status: productmetrics.Status{
			State:                 productmetrics.StateEnabled,
			Reason:                productmetrics.ReasonEnabled,
			InstallationIDPresent: true,
		},
		disclosedID:        disclosedMetricsInstallationID,
		disclosedIDPresent: true,
	}
	withProductMetricsControlService(t, service)

	stdout, stderr, err := executeMetricsCommand(t, "status", "--show-installation-id")
	if err != nil || stderr != "" {
		t.Fatalf("disclosure status = err:%v stderr:%q", err, stderr)
	}
	warning := "Warning: this stable installation ID is a linkable pseudonym. Do not put it in public logs."
	if !strings.Contains(stdout, warning) || !strings.Contains(stdout, "Installation ID: "+disclosedMetricsInstallationID) ||
		strings.Index(stdout, warning) > strings.Index(stdout, disclosedMetricsInstallationID) || service.disclosureCalls != 1 {
		t.Fatalf("disclosure output = %q, calls=%d", stdout, service.disclosureCalls)
	}
	for _, args := range [][]string{
		{"status", "--json", "--show-installation-id"},
		{"status", "--show-installation-id", "--json"},
	} {
		stdout, stderr, err = executeMetricsCommand(t, args...)
		if err == nil || strings.Contains(stdout+stderr, disclosedMetricsInstallationID) {
			t.Fatalf("mixed JSON disclosure args %v = err:%v stdout:%q stderr:%q", args, err, stdout, stderr)
		}
	}
}

func TestMetricsExampleJSONIsExactAndNeverConstructsService(t *testing.T) {
	original := productMetricsControlServiceFactory
	productMetricsControlServiceFactory = func() (productMetricsControlService, error) {
		panic("example constructed product-metrics service")
	}
	t.Cleanup(func() { productMetricsControlServiceFactory = original })

	want, err := productmetrics.EncodeBatch(productmetrics.ExampleBatch())
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, runErr := executeMetricsCommand(t, "example", "--json")
	if runErr != nil || stderr != "" || stdout != string(want) {
		t.Fatalf("example --json = err:%v stdout:%q stderr:%q, want %q", runErr, stdout, stderr, string(want))
	}
	stdout, stderr, runErr = executeMetricsCommand(t, "example")
	if runErr != nil || stderr != "" || !strings.HasSuffix(stdout, string(want)+"\n") ||
		!strings.Contains(stdout, "Fixed placeholders") {
		t.Fatalf("example text = err:%v stdout:%q stderr:%q", runErr, stdout, stderr)
	}
}

type nilShortWriter struct{}

func (nilShortWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	return len(data) - 1, nil
}

func TestMetricsExampleJSONRejectsShortWrite(t *testing.T) {
	command := newMetricsExampleCmd(nilShortWriter{})
	command.SetArgs([]string{"--json"})
	if err := command.Execute(); !errors.Is(err, errExit) {
		t.Fatalf("short example write error = %v, want errExit", err)
	}
}

func TestMetricsOnUsesVerifiedNoticeWriterAndPrintsBoundedResults(t *testing.T) {
	service := &fakeProductMetricsControlService{enableNotice: "TEST NOTICE\n"}
	withProductMetricsControlService(t, service)
	stdout, stderr, err := executeMetricsCommand(t, "on")
	if err != nil || stdout != "Gas City command usage metrics are enabled. This command was not recorded.\n" || stderr != "TEST NOTICE\n" ||
		!service.enableInvocation.NoticeEligible || service.enableInvocation.Recordable || !service.enableHasDeadline {
		t.Fatalf("metrics on = err:%v stdout:%q stderr:%q invocation:%+v", err, stdout, stderr, service.enableInvocation)
	}

	secret := errors.New("/private/city/path: injected secret")
	service.enableErr = secret
	service.status = productmetrics.Status{State: productmetrics.StateFailClosed, Reason: productmetrics.ReasonEndpointMissing}
	stdout, stderr, err = executeMetricsCommand(t, "on")
	if !errors.Is(err, errExit) || stdout != "" || strings.Contains(stderr, secret.Error()) ||
		!strings.Contains(stderr, "gc metrics on: cannot enable while state is fail-closed (endpoint-missing)") {
		t.Fatalf("failed metrics on = err:%v stdout:%q stderr:%q", err, stdout, stderr)
	}
}

func TestMetricsOnCapturesDisableAndManagedEnvironment(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		check func(productmetrics.InvocationContext) bool
	}{
		{name: "do not track", key: "DO_NOT_TRACK", value: "1", check: func(invocation productmetrics.InvocationContext) bool {
			return invocation.DoNotTrack == "1"
		}},
		{name: "gc disable", key: "GC_DISABLE_USAGE_METRICS", value: "yes", check: func(invocation productmetrics.InvocationContext) bool {
			return invocation.DisableUsageMetrics == "yes"
		}},
	}
	for _, key := range []string{"GC_SESSION_ID", "GC_SESSION_NAME", "GC_AGENT", "GC_TEMPLATE", "GC_MANAGED_SESSION_HOOK", "GC_HOOK_EVENT_NAME", "BEADS_ACTOR"} {
		key := key
		tests = append(tests, struct {
			name  string
			key   string
			value string
			check func(productmetrics.InvocationContext) bool
		}{name: "managed " + key, key: key, value: "set", check: func(invocation productmetrics.InvocationContext) bool {
			return invocation.ManagedAutomation
		}})
	}
	for _, key := range []string{"GC_HOOK_SOURCE", "GC_PROVIDER_SESSION_ID", "GC_PROVIDER_SESSION_ID_REQUIRED"} {
		key := key
		tests = append(tests, struct {
			name  string
			key   string
			value string
			check func(productmetrics.InvocationContext) bool
		}{name: "provider hook " + key, key: key, value: "set", check: func(invocation productmetrics.InvocationContext) bool {
			return invocation.ManagedAutomation
		}})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			service := &fakeProductMetricsControlService{}
			withProductMetricsControlService(t, service)
			_, _, _ = executeMetricsCommand(t, "on")
			if !test.check(service.enableInvocation) {
				t.Fatalf("metrics on invocation for %s=%q = %+v", test.key, test.value, service.enableInvocation)
			}
		})
	}
}

func TestMetricsStatusMarksUnavailableDiagnostics(t *testing.T) {
	service := &fakeProductMetricsControlService{status: productmetrics.Status{
		State: productmetrics.StateFailClosed, Reason: productmetrics.ReasonConfigInvalid,
		DroppedEvents: 99, LastUploadAttemptHourUTC: "2026-07-12T20:00:00Z",
		LastErrorClass: productmetrics.DiagnosticErrorStorageFailure,
	}}
	withProductMetricsControlService(t, service)
	stdout, stderr, err := executeMetricsCommand(t, "status")
	if err != nil || stderr != "" || !strings.Contains(stdout, "Diagnostics: unavailable") {
		t.Fatalf("unavailable status = err:%v stdout:%q stderr:%q", err, stdout, stderr)
	}
	for _, misleading := range []string{"Dropped events: 99", "Last upload attempt: 2026-07-12T20:00:00Z", "Last error: storage-failure"} {
		if strings.Contains(stdout, misleading) {
			t.Fatalf("unavailable status printed %q as authoritative: %s", misleading, stdout)
		}
	}
}

func TestMetricsOffMapsEveryClosedResultWithoutLeakingCauses(t *testing.T) {
	secret := errors.New("/private/city/path: event body secret")
	tests := []struct {
		name       string
		result     productmetrics.PurgeResult
		err        error
		wantErr    bool
		wantStdout []string
		wantStderr []string
	}{
		{
			name: "completed",
			result: productmetrics.PurgeResult{
				Outcome: productmetrics.PurgeCompleted, RemovedEvents: 12, RemovedBytes: 8602, DisabledDurable: true,
			},
			wantStdout: []string{"disabled", "Removed 12 queued events (8.4 KiB)", "made no server request", "Beads telemetry were not changed"},
		},
		{
			name:       "already disabled",
			result:     productmetrics.PurgeResult{Outcome: productmetrics.PurgeAlreadyDisabled, DisabledDurable: true},
			wantStdout: []string{"already disabled and locally clean", "uploader quiescence was rechecked"},
		},
		{
			name:       "recovered corrupt state",
			result:     productmetrics.PurgeResult{Outcome: productmetrics.PurgeCompleted, RecoveredState: true, DisabledDurable: true},
			wantStdout: []string{"disabled", "Recovered a corrupt local consent record"},
		},
		{
			name:       "disable write failed",
			result:     productmetrics.PurgeResult{Outcome: productmetrics.PurgeFailed},
			err:        &productmetrics.PurgeError{Class: productmetrics.PurgeErrorDisableWrite},
			wantErr:    true,
			wantStderr: []string{"disable-write-failed", "Previous state may remain", "Retry `gc metrics off`"},
		},
		{
			name: "durably disabled cleanup incomplete",
			result: productmetrics.PurgeResult{
				Outcome: productmetrics.PurgeCleanupPending, DisabledDurable: true, RemovedEvents: 1, RemovedBytes: 10,
				IncompletePhase: productmetrics.PurgeIncompleteLocalCleanup,
			},
			err:        errors.Join(&productmetrics.PurgeError{Class: productmetrics.PurgeErrorCleanupIncomplete}, secret),
			wantErr:    true,
			wantStderr: []string{"cleanup-incomplete", "phase local-cleanup", "Future collection and new uploads are already disabled", "Retry `gc metrics off`"},
		},
		{
			name: "manual cleanup residue",
			result: productmetrics.PurgeResult{
				Outcome: productmetrics.PurgeCleanupPending, DisabledDurable: true,
				IncompletePhase:       productmetrics.PurgeIncompleteLocalCleanup,
				ManualCleanupRequired: true, ManualCleanupReason: productmetrics.PurgeManualCleanupUnsettledRootTempJournal,
			},
			err:     &productmetrics.PurgeError{Class: productmetrics.PurgeErrorCleanupIncomplete},
			wantErr: true,
			wantStderr: []string{
				"cleanup-incomplete", "phase local-cleanup", "Manual cleanup is required", "unsettled-root-temp-journal",
				"same-UID", "product-usage root shown by `gc metrics status`", "Retry `gc metrics off`",
			},
		},
		{
			name:       "uploader timeout after disable",
			result:     productmetrics.PurgeResult{Outcome: productmetrics.PurgeCleanupPending, DisabledDurable: true},
			err:        &productmetrics.PurgeError{Class: productmetrics.PurgeErrorUploaderQuiescence},
			wantErr:    true,
			wantStderr: []string{"uploader-quiescence-timeout", "Future collection and new uploads are already disabled"},
		},
		{
			name:       "concurrent conflict",
			result:     productmetrics.PurgeResult{Outcome: productmetrics.PurgeCleanupPending},
			err:        &productmetrics.PurgeError{Class: productmetrics.PurgeErrorStateChanged},
			wantErr:    true,
			wantStderr: []string{"state-changed-concurrently", "could not prove durable opt-out"},
		},
		{
			name:       "unsafe storage",
			result:     productmetrics.PurgeResult{Outcome: productmetrics.PurgeFailed},
			err:        errors.Join(&productmetrics.PurgeError{Class: productmetrics.PurgeErrorStorage}, secret),
			wantErr:    true,
			wantStderr: []string{"storage-failure", "could not prove durable opt-out"},
		},
		{
			name:       "unknown class is closed",
			result:     productmetrics.PurgeResult{Outcome: productmetrics.PurgeFailed},
			err:        &productmetrics.PurgeError{Class: productmetrics.PurgeErrorClass("private-secret")},
			wantErr:    true,
			wantStderr: []string{"storage-failure", "could not prove durable opt-out"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeProductMetricsControlService{disableResult: test.result, disableErr: test.err}
			withProductMetricsControlService(t, service)
			stdout, stderr, err := executeMetricsCommand(t, "off")
			if (err != nil) != test.wantErr || service.disableCalls != 1 || strings.Contains(stdout+stderr, secret.Error()) {
				t.Fatalf("metrics off = err:%v stdout:%q stderr:%q calls:%d", err, stdout, stderr, service.disableCalls)
			}
			for _, want := range test.wantStdout {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout missing %q: %s", want, stdout)
				}
			}
			for _, want := range test.wantStderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing %q: %s", want, stderr)
				}
			}
		})
	}
}

func withProductMetricsControlService(t *testing.T, service productMetricsControlService) {
	t.Helper()
	original := productMetricsControlServiceFactory
	productMetricsControlServiceFactory = func() (productMetricsControlService, error) { return service, nil }
	t.Cleanup(func() { productMetricsControlServiceFactory = original })
}

func executeMetricsCommand(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var output, errors bytes.Buffer
	command := newMetricsCmd(&output, &errors)
	command.SetArgs(args)
	command.SetOut(&output)
	command.SetErr(&errors)
	err = command.Execute()
	return output.String(), errors.String(), err
}
