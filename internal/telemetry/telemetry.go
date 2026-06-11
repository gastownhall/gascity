// Package telemetry initializes OpenTelemetry providers for metric and log export.
//
// Metrics → VictoriaMetrics via OTLP HTTP
// Logs    → VictoriaLogs via OTLP HTTP
//
// Enabled by setting at least one of:
//
//	GC_OTEL_METRICS_URL  (default: http://localhost:8428/opentelemetry/api/v1/push)
//	GC_OTEL_LOGS_URL     (default: http://localhost:9428/insert/opentelemetry/v1/logs)
//
// When both GC_OTEL_*_URL vars are unset, the standard OpenTelemetry
// OTEL_EXPORTER_OTLP_ENDPOINT is honored as a fallback: signal URLs are
// derived by appending the OTLP/HTTP paths /v1/metrics and /v1/logs.
// Standard resource env vars (OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME)
// are merged into the exported resource; explicitly passed service
// name/version take precedence on conflict.
//
// Telemetry is best-effort: initialization errors are returned but do not
// affect normal gc operation — callers should log and continue.
//
// Init is idempotent: multiple calls return the same provider.
package telemetry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	// EnvMetricsURL is the env var for the VictoriaMetrics OTLP endpoint.
	EnvMetricsURL = "GC_OTEL_METRICS_URL"

	// EnvLogsURL is the env var for the VictoriaLogs OTLP endpoint.
	EnvLogsURL = "GC_OTEL_LOGS_URL"

	// EnvOTLPEndpoint is the standard OpenTelemetry base endpoint env var.
	// It is honored as a fallback when neither GC_OTEL_METRICS_URL nor
	// GC_OTEL_LOGS_URL is set: signal URLs are derived from it by appending
	// the standard OTLP/HTTP paths /v1/metrics and /v1/logs.
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

	// DefaultMetricsURL is VictoriaMetrics' OTLP push endpoint.
	DefaultMetricsURL = "http://localhost:8428/opentelemetry/api/v1/push"

	// DefaultLogsURL is VictoriaLogs' OTLP insert endpoint.
	DefaultLogsURL = "http://localhost:9428/insert/opentelemetry/v1/logs"

	// ExportInterval is how often metrics are pushed to VictoriaMetrics.
	ExportInterval = 30 * time.Second
)

// package-level state for idempotent Init.
var (
	initMu         sync.Mutex
	initDone       bool
	globalProvider *Provider
)

// Provider wraps OTel SDK providers and their shutdown functions.
type Provider struct {
	shutdowns    []func(context.Context) error
	shutdownMu   sync.Mutex
	shutdownDone bool

	// resource is the OTel resource shared by all providers; retained so
	// tests can assert Init wires newResource into the export pipeline.
	resource *resource.Resource
}

// Shutdown flushes all pending data and stops the OTel providers.
// Idempotent: safe to call more than once.
// Should be called with a deadline context (e.g. 5s timeout) on process exit.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.shutdownMu.Lock()
	defer p.shutdownMu.Unlock()
	if p.shutdownDone {
		return nil
	}
	p.shutdownDone = true

	var errs []error
	for _, fn := range p.shutdowns {
		if err := fn(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("telemetry shutdown errors: %v", errs)
	}
	return nil
}

// ResetForTest resets the global init state so tests can re-initialize.
// Must be called only from tests, after Shutdown.
func ResetForTest() {
	initMu.Lock()
	defer initMu.Unlock()
	initDone = false
	globalProvider = nil
}

// resolveEndpoints determines the metric and log OTLP endpoints from the
// environment. enabled is false when no telemetry env var is set.
//
// GC_OTEL_METRICS_URL / GC_OTEL_LOGS_URL take precedence: when at least one
// is set, the unset one falls back to its package default. When both are
// unset, the standard OTEL_EXPORTER_OTLP_ENDPOINT is used as the base URL
// for both signals with /v1/metrics and /v1/logs appended.
func resolveEndpoints() (metricsURL, logsURL string, enabled bool) {
	metricsURL = os.Getenv(EnvMetricsURL)
	logsURL = os.Getenv(EnvLogsURL)

	if metricsURL == "" && logsURL == "" {
		base := strings.TrimRight(os.Getenv(EnvOTLPEndpoint), "/")
		if base == "" {
			return "", "", false
		}
		return base + "/v1/metrics", base + "/v1/logs", true
	}
	if metricsURL == "" {
		metricsURL = DefaultMetricsURL
	}
	if logsURL == "" {
		logsURL = DefaultLogsURL
	}
	return metricsURL, logsURL, true
}

// newResource builds the OTel resource attached to exported metrics and logs.
//
// Standard resource env vars (OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME)
// are honored via resource.WithFromEnv and override detected host/OS
// attributes; the explicitly passed service name/version win on conflict.
// service.instance.id (a fresh UUID, per the OTel semantic conventions) is
// required for correctness, not just attribution: gc counters are cumulative
// per process, and many gc processes run concurrently on one host — without
// a unique instance id their snapshots interleave on a single series, so
// rate queries over the merged series are meaningless. It is applied with
// the service identity, after WithFromEnv, so an inherited
// OTEL_RESOURCE_ATTRIBUTES value can never override per-process uniqueness.
func newResource(ctx context.Context, serviceName, serviceVersion string) (*resource.Resource, error) {
	instanceID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("generating service.instance.id: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithHost(),
		resource.WithOS(),
		resource.WithFromEnv(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
			semconv.ServiceInstanceID(instanceID.String()),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTel resource: %w", err)
	}
	return res, nil
}

// Init initializes OTel metric and log providers.
//
// Idempotent: subsequent calls return the provider created on the first call.
//
// Returns (nil, nil) if none of GC_OTEL_METRICS_URL, GC_OTEL_LOGS_URL, and
// OTEL_EXPORTER_OTLP_ENDPOINT is set, so that telemetry is strictly opt-in.
// Set any of them to activate.
//
// When a GC_OTEL_*_URL var activates telemetry, defaults are used for the
// unset endpoint:
//
//	metrics → http://localhost:8428/opentelemetry/api/v1/push
//	logs    → http://localhost:9428/insert/opentelemetry/v1/logs
//
// When only OTEL_EXPORTER_OTLP_ENDPOINT is set, both signal URLs are derived
// from it per the OTLP/HTTP convention (base + /v1/metrics, base + /v1/logs).
func Init(ctx context.Context, serviceName, serviceVersion string) (*Provider, error) {
	initMu.Lock()
	defer initMu.Unlock()
	if initDone {
		return globalProvider, nil
	}

	metricsURL, logsURL, enabled := resolveEndpoints()

	// No endpoint env var set → telemetry disabled, not an error.
	if !enabled {
		initDone = true
		globalProvider = nil
		return nil, nil
	}

	res, err := newResource(ctx, serviceName, serviceVersion)
	if err != nil {
		return nil, err
	}

	p := &Provider{resource: res}

	// Metrics → VictoriaMetrics
	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(metricsURL),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(metricExp,
				sdkmetric.WithInterval(ExportInterval),
			),
		),
	)
	otel.SetMeterProvider(mp)
	p.shutdowns = append(p.shutdowns, mp.Shutdown)
	initInstruments()

	// Logs → VictoriaLogs
	logExp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(logsURL),
	)
	if err != nil {
		// Shut down the already-registered metric provider to avoid leaking
		// its periodic reader goroutine.
		_ = mp.Shutdown(ctx)
		return nil, fmt.Errorf("creating OTLP log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)
	global.SetLoggerProvider(lp)
	p.shutdowns = append(p.shutdowns, lp.Shutdown)

	initDone = true
	globalProvider = p
	return p, nil
}
