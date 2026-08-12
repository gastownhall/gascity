//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticStatusCodecIsCanonicalBoundedAndSchemaClosed(t *testing.T) {
	t.Parallel()

	record := diagnosticStatus{
		droppedEvents:            7,
		lastUploadAttemptHourUTC: "2026-07-12T01:00:00Z",
		lastUploadSuccessHourUTC: "2026-07-12T00:00:00Z",
		lastErrorClass:           DiagnosticErrorNetworkTimeout,
	}
	encoded, err := encodeDiagnosticStatus(record)
	if err != nil {
		t.Fatal(err)
	}
	const want = "status_schema = 1\n" +
		"dropped_events = 7\n" +
		"last_upload_attempt_hour_utc = \"2026-07-12T01:00:00Z\"\n" +
		"last_upload_success_hour_utc = \"2026-07-12T00:00:00Z\"\n" +
		"last_error_class = \"network-timeout\"\n"
	if string(encoded) != want {
		t.Fatalf("encoded status = %q, want %q", encoded, want)
	}
	if len(encoded) > maximumDiagnosticStatusBytes {
		t.Fatalf("encoded status is %d bytes, maximum %d", len(encoded), maximumDiagnosticStatusBytes)
	}
	decoded, err := decodeDiagnosticStatus(encoded)
	if err != nil || decoded != record {
		t.Fatalf("decoded status = %#v, %v; want %#v", decoded, err, record)
	}

	for name, body := range map[string]string{
		"empty":            "",
		"unknown field":    want + "detail = \"must not survive\"\n",
		"unknown table":    want + "[future]\nvalue = 1\n",
		"missing field":    strings.Replace(want, "dropped_events = 7\n", "", 1),
		"future schema":    strings.Replace(want, "status_schema = 1", "status_schema = 2", 1),
		"duplicate":        want + "dropped_events = 8\n",
		"fractional hour":  strings.Replace(want, "2026-07-12T01:00:00Z", "2026-07-12T01:00:01Z", 1),
		"offset hour":      strings.Replace(want, "2026-07-12T01:00:00Z", "2026-07-12T02:00:00+01:00", 1),
		"unknown class":    strings.Replace(want, "network-timeout", "request-body-was-secret", 1),
		"negative counter": strings.Replace(want, "dropped_events = 7", "dropped_events = -1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeDiagnosticStatus([]byte(body)); err == nil {
				t.Fatalf("decodeDiagnosticStatus(%q) succeeded", body)
			}
		})
	}
	if _, err := decodeDiagnosticStatus(make([]byte, maximumDiagnosticStatusBytes+1)); err == nil {
		t.Fatal("oversized status record decoded")
	}
}

func TestDiagnosticStatusCodecAcceptsEmptyOptionalDiagnostics(t *testing.T) {
	t.Parallel()

	encoded, err := encodeDiagnosticStatus(diagnosticStatus{})
	if err != nil {
		t.Fatal(err)
	}
	const want = "status_schema = 1\n" +
		"dropped_events = 0\n" +
		"last_upload_attempt_hour_utc = \"\"\n" +
		"last_upload_success_hour_utc = \"\"\n" +
		"last_error_class = \"\"\n"
	if string(encoded) != want {
		t.Fatalf("encoded empty status = %q, want %q", encoded, want)
	}
	if got, err := decodeDiagnosticStatus(encoded); err != nil || got != (diagnosticStatus{}) {
		t.Fatalf("decoded empty status = %#v, %v", got, err)
	}
}

func TestStatusAndDisclosureKeepInstallationIDSeparated(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	service := mustOpenTestService(t, defaultTestServiceDependencies(home, 2))

	status := service.Status(context.Background())
	if !status.InstallationIDPresent {
		t.Fatal("redacted status did not report installation-ID presence")
	}
	statusValue := reflect.ValueOf(status)
	for index := 0; index < statusValue.NumField(); index++ {
		field := statusValue.Type().Field(index)
		if field.Type.Kind() == reflect.String && statusValue.Field(index).String() == testInstallationID {
			t.Fatalf("Status.%s leaked the installation ID", field.Name)
		}
	}

	id, present := service.InstallationIDForDisclosure(context.Background())
	if !present || id != testInstallationID {
		t.Fatalf("InstallationIDForDisclosure() = (%q, %v)", id, present)
	}

	writeStateFixture(t, home, disabledState(5, 3, cleanupNone))
	if id, present := service.InstallationIDForDisclosure(context.Background()); present || id != "" {
		t.Fatalf("disabled disclosure = (%q, %v), want absent", id, present)
	}
}

func TestPolicyMetadataExposesOnlyHostnameAndFixedRetention(t *testing.T) {
	home := newMetricsTestHome(t)
	deps := defaultTestServiceDependencies(home, 2)
	deps.release.endpointHostname = "metrics.gascity.example"
	deps.release.privacyURL = "https://gascity.example/privacy"
	policy := mustOpenTestService(t, deps).PolicyMetadata()
	if policy != (PolicyMetadata{
		EndpointHostname:         "metrics.gascity.example",
		PrivacyURL:               "https://gascity.example/privacy",
		EdgeLogRetentionDays:     7,
		RawEventRetentionDays:    90,
		AggregateRetentionMonths: 13,
	}) {
		t.Fatalf("PolicyMetadata() = %#v", policy)
	}
	for _, forbidden := range []string{"/v1/command-usage", "?", "#", "@"} {
		if strings.Contains(policy.EndpointHostname, forbidden) {
			t.Fatalf("endpoint hostname leaked forbidden URL material %q", forbidden)
		}
	}
}

func TestStatusDiagnosticDurationsAreNonnegative(t *testing.T) {
	home := newMetricsTestHome(t)
	writeStateFixture(t, home, enabledState(4, 2, testInstallationID, testSpoolGeneration))
	deps := defaultTestServiceDependencies(home, 2)
	deps.now = func() time.Time { return time.Date(2026, time.July, 12, 2, 0, 0, 0, time.UTC) }
	status := mustOpenTestService(t, deps).Status(context.Background())
	if status.OldestQueuedEventAge < 0 || status.SpawnThrottleAge < 0 {
		t.Fatalf("status contains a negative age: %#v", status)
	}
}

func TestStatusReportsHomeStabilityIndependentlyOfEffectiveReason(t *testing.T) {
	home := newMetricsTestHome(t)
	deps := defaultTestServiceDependencies(home, 2)
	deps.release.platformSupported = false
	deps.homeErr = errors.New("unsafe home detail must not escape")
	deps.homeReason = ReasonHomeUnstable
	status := mustOpenTestService(t, deps).Status(context.Background())
	if status.Reason != ReasonUnsupportedPlatform || status.HomeStable || status.HomeReason != ReasonHomeUnstable {
		t.Fatalf("status home projection = %#v", status)
	}
}
