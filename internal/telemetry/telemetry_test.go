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
