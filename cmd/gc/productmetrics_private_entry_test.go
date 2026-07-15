package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/productmetrics"
)

const productMetricsPrivateUploaderSentinelFixture = "__gc-product-metrics-uploader-v1"

func TestMainExitCodeConsumesEveryPrivateUploaderSentinelShapeBeforeRun(t *testing.T) {
	t.Setenv("GC_OTEL_METRICS_URL", "://invalid-product-metrics-private-test-url")

	tests := map[string][]string{
		"missing token": {productMetricsPrivateUploaderSentinelFixture},
		"invalid token": {productMetricsPrivateUploaderSentinelFixture, "not-a-uuid"},
		"extra argument": {
			productMetricsPrivateUploaderSentinelFixture,
			"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
			"version",
		},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := mainExitCode(args, &stdout, &stderr); code == 0 {
				t.Fatalf("mainExitCode(%q) = 0, want private-entry failure", args)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("private entry wrote normal streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestMainExitCodePrivateUploaderRequiresRecursionMarker(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_OTEL_METRICS_URL", "://invalid-product-metrics-private-test-url")

	args := []string{
		productMetricsPrivateUploaderSentinelFixture,
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}
	var stdout, stderr bytes.Buffer
	if code := mainExitCode(args, &stdout, &stderr); code == 0 {
		t.Fatalf("mainExitCode(%q) = 0 without the private recursion marker", args)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("private entry wrote normal streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestPrivateProductMetricsEntrypointDoesNotConsumeOrdinaryArgs(t *testing.T) {
	handled, code := privateProductMetricsEntrypoint([]string{"version"})
	if handled || code != 0 {
		t.Fatalf("privateProductMetricsEntrypoint(version) = (%t, %d), want (false, 0)", handled, code)
	}
}

func TestPrivateProductMetricsEntrypointChecksExactMarkerBeforeRunner(t *testing.T) {
	previous := privateProductMetricsRunnerFactory
	t.Cleanup(func() { privateProductMetricsRunnerFactory = previous })

	args := []string{
		productMetricsPrivateUploaderSentinelFixture,
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}
	for _, test := range []struct {
		name      string
		marker    string
		wantCode  int
		wantCalls int
	}{
		{name: "missing", marker: "", wantCode: privateProductMetricsFailureExitCode, wantCalls: 0},
		{name: "wrong value", marker: "true", wantCode: privateProductMetricsFailureExitCode, wantCalls: 0},
		{name: "exact", marker: "1", wantCode: 0, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GC_PRODUCT_METRICS_PRIVATE_UPLOADER", test.marker)
			factorySelections := 0
			runnerCalls := 0
			privateProductMetricsRunnerFactory = func() privateProductMetricsRunFunc {
				factorySelections++
				return func(context.Context, productmetrics.PrivateUploaderInvocation) error {
					runnerCalls++
					return nil
				}
			}
			handled, code := privateProductMetricsEntrypoint(args)
			if !handled || code != test.wantCode || factorySelections != test.wantCalls || runnerCalls != test.wantCalls {
				t.Fatalf("private entry = handled:%t code:%d factory selections:%d runner calls:%d, want true/%d/%d/%d",
					handled, code, factorySelections, runnerCalls, test.wantCode, test.wantCalls, test.wantCalls)
			}
		})
	}
}

func TestPrivateProductMetricsEntrypointRejectsMalformedArgsBeforeRunnerSelection(t *testing.T) {
	t.Setenv(privateProductMetricsMarkerEnvironment, privateProductMetricsMarkerValue)
	previous := privateProductMetricsRunnerFactory
	t.Cleanup(func() { privateProductMetricsRunnerFactory = previous })
	factorySelections := 0
	runnerCalls := 0
	privateProductMetricsRunnerFactory = func() privateProductMetricsRunFunc {
		factorySelections++
		return func(context.Context, productmetrics.PrivateUploaderInvocation) error {
			runnerCalls++
			return nil
		}
	}

	handled, code := privateProductMetricsEntrypointForPlatform([]string{
		productMetricsPrivateUploaderSentinelFixture,
		"not-a-uuid",
	}, "linux")
	if !handled || code != privateProductMetricsFailureExitCode || factorySelections != 0 || runnerCalls != 0 {
		t.Fatalf("malformed private entry = handled:%t code:%d factory selections:%d runner calls:%d, want true/%d/0/0",
			handled, code, factorySelections, runnerCalls, privateProductMetricsFailureExitCode)
	}
}

func TestPrivateProductMetricsPlatformSupportIsClosed(t *testing.T) {
	for _, test := range []struct {
		goos string
		want bool
	}{
		{goos: "linux", want: true},
		{goos: "darwin", want: true},
		{goos: "android", want: false},
		{goos: "ios", want: false},
		{goos: "windows", want: false},
		{goos: "plan9", want: false},
		{goos: "", want: false},
	} {
		t.Run(test.goos, func(t *testing.T) {
			if got := privateProductMetricsPlatformSupported(test.goos); got != test.want {
				t.Fatalf("privateProductMetricsPlatformSupported(%q) = %t, want %t", test.goos, got, test.want)
			}
		})
	}
}

func TestPrivateProductMetricsEntrypointRejectsUnsupportedPlatformBeforeRunner(t *testing.T) {
	t.Setenv(privateProductMetricsMarkerEnvironment, privateProductMetricsMarkerValue)
	previous := privateProductMetricsRunnerFactory
	t.Cleanup(func() { privateProductMetricsRunnerFactory = previous })
	factorySelections := 0
	runnerCalls := 0
	privateProductMetricsRunnerFactory = func() privateProductMetricsRunFunc {
		factorySelections++
		return func(context.Context, productmetrics.PrivateUploaderInvocation) error {
			runnerCalls++
			return nil
		}
	}

	handled, code := privateProductMetricsEntrypointForPlatform([]string{
		productMetricsPrivateUploaderSentinelFixture,
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}, "windows")
	if !handled || code != privateProductMetricsFailureExitCode || factorySelections != 0 || runnerCalls != 0 {
		t.Fatalf("unsupported private entry = handled:%t code:%d factory selections:%d runner calls:%d, want true/%d/0/0",
			handled, code, factorySelections, runnerCalls, privateProductMetricsFailureExitCode)
	}
}
