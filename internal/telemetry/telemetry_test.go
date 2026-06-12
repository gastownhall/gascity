package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// resetInitState resets the package-level telemetry init guard so tests run
// independently of each other.
func resetInitState(t *testing.T) {
	t.Helper()
	initMu.Lock()
	initDone = false
	globalProvider = nil
	initMu.Unlock()
	t.Cleanup(func() {
		initMu.Lock()
		initDone = false
		globalProvider = nil
		initMu.Unlock()
	})
}

// resourceAttr returns the value of key in res, or "" when absent.
func resourceAttr(t *testing.T, res *resource.Resource, key string) string {
	t.Helper()
	for _, kv := range res.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

func TestNewResource_SetsUniqueServiceInstanceID(t *testing.T) {
	ctx := context.Background()

	res1, err := newResource(ctx, "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("newResource error: %v", err)
	}
	res2, err := newResource(ctx, "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("newResource error: %v", err)
	}

	id1 := resourceAttr(t, res1, string(semconv.ServiceInstanceIDKey))
	id2 := resourceAttr(t, res2, string(semconv.ServiceInstanceIDKey))

	if id1 == "" {
		t.Fatal("resource is missing service.instance.id")
	}
	if _, err := uuid.Parse(id1); err != nil {
		t.Errorf("service.instance.id %q is not a valid UUID: %v", id1, err)
	}
	if id1 == id2 {
		t.Errorf("service.instance.id must be unique per resource, got %q twice", id1)
	}
}

func TestNewResource_KeepsServiceIdentity(t *testing.T) {
	res, err := newResource(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("newResource error: %v", err)
	}
	if got := resourceAttr(t, res, string(semconv.ServiceNameKey)); got != "test-svc" {
		t.Errorf("service.name = %q, want %q", got, "test-svc")
	}
	if got := resourceAttr(t, res, string(semconv.ServiceVersionKey)); got != "0.0.1" {
		t.Errorf("service.version = %q, want %q", got, "0.0.1")
	}
}

func TestInit_WiresResourceWithInstanceID(t *testing.T) {
	resetInitState(t)
	// Unreachable endpoint: the exporter does not dial at construction and
	// export is best-effort, so Init succeeds without a live backend.
	t.Setenv(EnvMetricsURL, "http://127.0.0.1:1/v1/metrics")
	t.Setenv(EnvLogsURL, "")

	// Init mutates the process-global meter and logger providers; restore
	// them so later tests never observe these shut-down providers.
	prevMeter := otel.GetMeterProvider()
	prevLogger := global.GetLoggerProvider()
	t.Cleanup(func() {
		otel.SetMeterProvider(prevMeter)
		global.SetLoggerProvider(prevLogger)
	})

	p, err := Init(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider when metrics URL is set")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.Shutdown(ctx) // flush to the unreachable endpoint fails; irrelevant here
	})

	id := resourceAttr(t, p.resource, string(semconv.ServiceInstanceIDKey))
	if id == "" {
		t.Fatal("Init did not wire a resource carrying service.instance.id")
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Errorf("service.instance.id %q is not a valid UUID: %v", id, err)
	}
}

func TestInit_BothURLsUnset_ReturnsNil(t *testing.T) {
	resetInitState(t)
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "")

	p, err := Init(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if p != nil {
		t.Error("expected nil provider when both URLs are unset")
	}
}

func TestInit_Idempotent(t *testing.T) {
	resetInitState(t)
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "")

	p1, _ := Init(context.Background(), "test-svc", "0.0.1")
	p2, _ := Init(context.Background(), "test-svc", "0.0.1")

	if p1 != p2 {
		t.Error("second Init call should return the same provider as the first")
	}
}

func TestProvider_Shutdown_Idempotent(t *testing.T) {
	p := &Provider{}
	called := 0
	p.shutdowns = []func(context.Context) error{
		func(_ context.Context) error { called++; return nil },
	}

	ctx := context.Background()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown error: %v", err)
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown error: %v", err)
	}
	if called != 1 {
		t.Errorf("expected shutdown fn called once, called %d times", called)
	}
}

func TestProvider_Shutdown_CollectsErrors(t *testing.T) {
	p := &Provider{}
	p.shutdowns = []func(context.Context) error{
		func(_ context.Context) error { return errors.New("err1") },
		func(_ context.Context) error { return errors.New("err2") },
	}

	err := p.Shutdown(context.Background())
	if err == nil {
		t.Fatal("expected error from Shutdown when shutdown fns fail")
	}
}

func TestProvider_Shutdown_Empty(t *testing.T) {
	p := &Provider{}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown with no fns should not error: %v", err)
	}
}

func TestInit_OTLPEndpointFallbackEnablesTelemetry(t *testing.T) {
	resetInitState(t)
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	// Unroutable port; OTLP HTTP exporters connect lazily so Init succeeds.
	t.Setenv(EnvOTLPEndpoint, "http://127.0.0.1:1")

	p, err := Init(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Best-effort flush to a dead endpoint; the export error is expected.
	_ = p.Shutdown(ctx)
}

func TestResolveEndpoints_GCVarsWinOverOTLPEndpoint(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://vm:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "http://vl:9428/insert/opentelemetry/v1/logs")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")

	metricsURL, logsURL, enabled := resolveEndpoints()
	if !enabled {
		t.Fatal("expected telemetry enabled")
	}
	if metricsURL != "http://vm:8428/opentelemetry/api/v1/push" {
		t.Errorf("metricsURL = %q, want GC_OTEL_METRICS_URL value", metricsURL)
	}
	if logsURL != "http://vl:9428/insert/opentelemetry/v1/logs" {
		t.Errorf("logsURL = %q, want GC_OTEL_LOGS_URL value", logsURL)
	}
}

func TestResolveEndpoints_OTLPEndpointFallback(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")

	metricsURL, logsURL, enabled := resolveEndpoints()
	if !enabled {
		t.Fatal("expected telemetry enabled via OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if want := "http://collector:4318/v1/metrics"; metricsURL != want {
		t.Errorf("metricsURL = %q, want %q", metricsURL, want)
	}
	if want := "http://collector:4318/v1/logs"; logsURL != want {
		t.Errorf("logsURL = %q, want %q", logsURL, want)
	}
}

func TestResolveEndpoints_OTLPEndpointTrailingSlash(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318/")

	metricsURL, logsURL, enabled := resolveEndpoints()
	if !enabled {
		t.Fatal("expected telemetry enabled via OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if want := "http://collector:4318/v1/metrics"; metricsURL != want {
		t.Errorf("metricsURL = %q, want %q", metricsURL, want)
	}
	if want := "http://collector:4318/v1/logs"; logsURL != want {
		t.Errorf("logsURL = %q, want %q", logsURL, want)
	}
}

func TestResolveEndpoints_PartialGCVarsKeepDefaults(t *testing.T) {
	// When at least one GC_OTEL_*_URL is set, the unset one falls back to the
	// package default, not to OTEL_EXPORTER_OTLP_ENDPOINT — existing behavior
	// is unchanged.
	t.Setenv(EnvMetricsURL, "http://vm:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")

	metricsURL, logsURL, enabled := resolveEndpoints()
	if !enabled {
		t.Fatal("expected telemetry enabled")
	}
	if metricsURL != "http://vm:8428/opentelemetry/api/v1/push" {
		t.Errorf("metricsURL = %q, want GC_OTEL_METRICS_URL value", metricsURL)
	}
	if logsURL != DefaultLogsURL {
		t.Errorf("logsURL = %q, want DefaultLogsURL %q", logsURL, DefaultLogsURL)
	}
}

func TestResolveEndpoints_AllUnset_Disabled(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "")

	_, _, enabled := resolveEndpoints()
	if enabled {
		t.Error("expected telemetry disabled when no endpoint env var is set")
	}
}

func TestResolveEndpoints_SDKDisabledWinsOverEndpoints(t *testing.T) {
	// Case-insensitive per the OTel spec for boolean env vars; beats both
	// the GC vars and the standard endpoint fallback.
	t.Setenv(EnvSDKDisabled, "TRUE")
	t.Setenv(EnvMetricsURL, "http://vm:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")

	_, _, enabled := resolveEndpoints()
	if enabled {
		t.Error("expected telemetry disabled when OTEL_SDK_DISABLED=true")
	}
}

func TestResolveEndpoints_SDKDisabledFalseKeepsTelemetryOn(t *testing.T) {
	t.Setenv(EnvSDKDisabled, "false")
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")

	_, _, enabled := resolveEndpoints()
	if !enabled {
		t.Error("expected telemetry enabled when OTEL_SDK_DISABLED is not true")
	}
}

func TestNewResource_HonorsOTELResourceAttributes(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=prod,gc.city=testcity")
	// No GC context vars: inherited gc.* labels flow through unchanged, as
	// in a process without an identity of its own (controller topology).
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	res, err := newResource(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("newResource error: %v", err)
	}

	attrs := make(map[string]string)
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if got := attrs["deployment.environment"]; got != "prod" {
		t.Errorf("deployment.environment = %q, want %q", got, "prod")
	}
	if got := attrs["gc.city"]; got != "testcity" {
		t.Errorf("gc.city = %q, want %q", got, "testcity")
	}
	if got := attrs["service.name"]; got != "test-svc" {
		t.Errorf("service.name = %q, want %q", got, "test-svc")
	}
}

func TestNewResource_OwnGCIdentityWinsOverInheritedEnv(t *testing.T) {
	// A session env carries the spawning agent's identity labels via
	// OTEL_RESOURCE_ATTRIBUTES; a gc process with its own GC context must
	// export its own identity, not the spawner's.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "gc.agent=a,team=platform")
	t.Setenv("GC_ALIAS", "b")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	res, err := newResource(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("newResource error: %v", err)
	}

	attrs := make(map[string]string)
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if got := attrs["gc.agent"]; got != "b" {
		t.Errorf("gc.agent = %q, want own identity %q to win over inherited env", got, "b")
	}
	if got := attrs["team"]; got != "platform" {
		t.Errorf("team = %q, want %q preserved", got, "platform")
	}
}

func TestNewResource_ExplicitServiceIdentityWinsOverEnv(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=env-svc,service.version=9.9.9")

	res, err := newResource(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("newResource error: %v", err)
	}

	attrs := make(map[string]string)
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if got := attrs["service.name"]; got != "test-svc" {
		t.Errorf("service.name = %q, want explicit %q to win over env", got, "test-svc")
	}
	if got := attrs["service.version"]; got != "0.0.1" {
		t.Errorf("service.version = %q, want explicit %q to win over env", got, "0.0.1")
	}
}

func TestNewResource_ExplicitServiceNameWinsOverOTELServiceName(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv("OTEL_SERVICE_NAME", "env-svc")

	res, err := newResource(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("newResource error: %v", err)
	}

	for _, kv := range res.Attributes() {
		if string(kv.Key) == "service.name" && kv.Value.Emit() != "test-svc" {
			t.Errorf("service.name = %q, want explicit %q to win over OTEL_SERVICE_NAME", kv.Value.Emit(), "test-svc")
		}
	}
}

func TestNewResource_ToleratesMalformedResourceAttributes(t *testing.T) {
	// A trailing comma makes the env detector report ErrPartialResource
	// alongside a usable resource; the malformed segment is dropped and the
	// valid attributes survive.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=prod,")

	res, err := newResource(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("newResource error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil resource for partial detection")
	}

	attrs := make(map[string]string)
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if got := attrs["deployment.environment"]; got != "prod" {
		t.Errorf("deployment.environment = %q, want %q", got, "prod")
	}
	if got := attrs["service.name"]; got != "test-svc" {
		t.Errorf("service.name = %q, want %q", got, "test-svc")
	}
}

func TestInit_MalformedResourceAttributesStillReturnsProvider(t *testing.T) {
	resetInitState(t)
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://127.0.0.1:1")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "a=b,")

	p, err := Init(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider despite malformed OTEL_RESOURCE_ATTRIBUTES")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Best-effort flush to a dead endpoint; the export error is expected.
	_ = p.Shutdown(ctx)
}

func TestProvider_Shutdown_ConcurrentSafe(t *testing.T) {
	p := &Provider{}
	called := 0
	var mu sync.Mutex
	p.shutdowns = []func(context.Context) error{
		func(_ context.Context) error {
			mu.Lock()
			called++
			mu.Unlock()
			return nil
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Shutdown(context.Background())
		}()
	}
	wg.Wait()

	if called != 1 {
		t.Errorf("expected shutdown fn called exactly once, called %d times", called)
	}
}
