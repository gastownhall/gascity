// Package telemetry — recorder.go
// Recording helper functions for all GC telemetry events (Phases 1 & 2).
// Each function emits both an OTel log event (→ VictoriaLogs) and increments
// a metric counter (→ VictoriaMetrics).
package telemetry

import (
	"context"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
)

const (
	meterRecorderName = "github.com/gastownhall/gascity"
	loggerName        = "gascity"
)

// recorderInstruments holds all lazy-initialized OTel metric instruments.
type recorderInstruments struct {
	// Counters — Phase 1 (11)
	agentStartTotal      metric.Int64Counter
	agentStopTotal       metric.Int64Counter
	agentCrashTotal      metric.Int64Counter
	agentQuarantineTotal metric.Int64Counter
	agentIdleKillTotal   metric.Int64Counter
	reconcileCycleTotal  metric.Int64Counter
	nudgeTotal           metric.Int64Counter
	configReloadTotal    metric.Int64Counter
	controllerTotal      metric.Int64Counter
	bdTotal              metric.Int64Counter
	slingTotal           metric.Int64Counter

	// Counters — Phase 2 (4)
	poolSpawnTotal  metric.Int64Counter
	poolRemoveTotal metric.Int64Counter
	mailOpsTotal    metric.Int64Counter
	drainTotal      metric.Int64Counter

	// Gauges (1)
	beadStoreHealthy metric.Int64Gauge

	// Histograms — Phase 1 (1)
	bdDurationHist metric.Float64Histogram

	// Histograms — Phase 2 (1)
	poolCheckDurationHist metric.Float64Histogram

	// HTTP API request instrumentation
	httpRequestTotal    metric.Int64Counter
	httpRequestDuration metric.Float64Histogram
}

var (
	instOnce sync.Once
	inst     recorderInstruments
)

// initInstruments registers all recorder metric instruments against the current
// global MeterProvider. Must be called after telemetry.Init so the real
// provider is set. Also called lazily on first use as a safety net.
func initInstruments() {
	instOnce.Do(func() {
		m := otel.GetMeterProvider().Meter(meterRecorderName)

		// Counters
		inst.agentStartTotal, _ = m.Int64Counter("gc.agent.starts.total",
			metric.WithDescription("Total agent session starts"),
		)
		inst.agentStopTotal, _ = m.Int64Counter("gc.agent.stops.total",
			metric.WithDescription("Total agent session stops"),
		)
		inst.agentCrashTotal, _ = m.Int64Counter("gc.agent.crashes.total",
			metric.WithDescription("Total agent crash detections"),
		)
		inst.agentQuarantineTotal, _ = m.Int64Counter("gc.agent.quarantines.total",
			metric.WithDescription("Total agent crash loop quarantines"),
		)
		inst.agentIdleKillTotal, _ = m.Int64Counter("gc.agent.idle_kills.total",
			metric.WithDescription("Total agent idle timeout restarts"),
		)
		inst.reconcileCycleTotal, _ = m.Int64Counter("gc.reconcile.cycles.total",
			metric.WithDescription("Total reconciliation cycles"),
		)
		inst.nudgeTotal, _ = m.Int64Counter("gc.session.nudges.total",
			metric.WithDescription("Total session nudge sends"),
		)
		inst.configReloadTotal, _ = m.Int64Counter("gc.config.reloads.total",
			metric.WithDescription("Total config reload attempts"),
		)
		inst.controllerTotal, _ = m.Int64Counter("gc.controller.lifecycle.total",
			metric.WithDescription("Total controller lifecycle events"),
		)
		inst.bdTotal, _ = m.Int64Counter("gc.bd.calls.total",
			metric.WithDescription("Total bd CLI command invocations"),
		)
		inst.slingTotal, _ = m.Int64Counter("gc.sling.dispatches.total",
			metric.WithDescription("Total sling work dispatches"),
		)

		// Counters — Phase 2
		inst.poolSpawnTotal, _ = m.Int64Counter("gc.pool.spawns.total",
			metric.WithDescription("Total pool member instance spawns"),
		)
		inst.poolRemoveTotal, _ = m.Int64Counter("gc.pool.removes.total",
			metric.WithDescription("Total pool member instance removals"),
		)
		inst.mailOpsTotal, _ = m.Int64Counter("gc.mail.operations.total",
			metric.WithDescription("Total mail operations"),
		)
		inst.drainTotal, _ = m.Int64Counter("gc.drain.transitions.total",
			metric.WithDescription("Total agent drain lifecycle transitions"),
		)

		// Gauges
		inst.beadStoreHealthy, _ = m.Int64Gauge("gc.bead_store.healthy",
			metric.WithDescription("Whether the bead store is healthy (1) or unavailable (0)"),
		)

		// Histograms
		inst.bdDurationHist, _ = m.Float64Histogram("gc.bd.duration_ms",
			metric.WithDescription("bd CLI call round-trip latency in milliseconds"),
			metric.WithUnit("ms"),
		)

		// Histograms — Phase 2
		inst.poolCheckDurationHist, _ = m.Float64Histogram("gc.pool.check.duration_ms",
			metric.WithDescription("Pool scale_check command latency in milliseconds"),
			metric.WithUnit("ms"),
		)

		// HTTP API request instrumentation
		inst.httpRequestTotal, _ = m.Int64Counter("gc.http.requests.total",
			metric.WithDescription("Total HTTP API requests"),
		)
		inst.httpRequestDuration, _ = m.Float64Histogram("gc.http.duration_ms",
			metric.WithDescription("HTTP API request latency in milliseconds"),
			metric.WithUnit("ms"),
		)
	})
}

// statusStr returns "ok" or "error" depending on whether err is nil.
func statusStr(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// emit sends an OTel log event with the given body and key-value attributes.
func emit(ctx context.Context, body string, sev otellog.Severity, attrs ...otellog.KeyValue) {
	logger := global.GetLoggerProvider().Logger(loggerName)
	var r otellog.Record
	r.SetBody(otellog.StringValue(body))
	r.SetSeverity(sev)
	r.AddAttributes(attrs...)
	logger.Emit(ctx, r)
}

// errKV returns a log KeyValue with the error message, or empty string if nil.
func errKV(err error) otellog.KeyValue {
	if err != nil {
		return otellog.String("error", err.Error())
	}
	return otellog.String("error", "")
}

// severity returns SeverityInfo on success, SeverityError on failure.
func severity(err error) otellog.Severity {
	if err != nil {
		return otellog.SeverityError
	}
	return otellog.SeverityInfo
}

const (
	// maxStdoutLog is the maximum number of bytes of stdout captured in logs.
	maxStdoutLog = 2048
	// maxStderrLog is the maximum number of bytes of stderr captured in logs.
	maxStderrLog = 1024
)

// truncateOutput trims s to max bytes and appends "…" when truncated.
// Avoids splitting multi-byte UTF-8 characters at the boundary.
func truncateOutput(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// Walk back from the cut point to avoid splitting a multi-byte rune.
	truncated := s[:limit]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "…"
}

// RecordAgentStart records an agent session start (metrics + log event).
func RecordAgentStart(ctx context.Context, sessionName, agentName string, err error) {
	initInstruments()
	status := statusStr(err)
	inst.agentStartTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("agent", agentName),
			attribute.String("status", status),
		),
	)
	emit(ctx, "agent.start", severity(err),
		otellog.String("session", sessionName),
		otellog.String("agent", agentName),
		otellog.String("status", status),
		errKV(err),
	)
}

// RecordAgentStop records an agent session stop (metrics + log event).
func RecordAgentStop(ctx context.Context, sessionName, reason string, err error) {
	initInstruments()
	status := statusStr(err)
	inst.agentStopTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("agent", sessionName),
			attribute.String("reason", reason),
			attribute.String("status", status),
		),
	)
	emit(ctx, "agent.stop", severity(err),
		otellog.String("session", sessionName),
		otellog.String("reason", reason),
		otellog.String("status", status),
		errKV(err),
	)
}

// RecordAgentCrash records a detected agent crash (metrics + log event).
func RecordAgentCrash(ctx context.Context, agentName, lastOutput string) {
	initInstruments()
	inst.agentCrashTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("agent", agentName)),
	)
	emit(ctx, "agent.crash", otellog.SeverityWarn,
		otellog.String("agent", agentName),
		otellog.String("last_output", truncateOutput(lastOutput, maxStdoutLog)),
	)
}

// RecordAgentQuarantine records a crash loop quarantine (metrics + log event).
func RecordAgentQuarantine(ctx context.Context, agentName string) {
	initInstruments()
	inst.agentQuarantineTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("agent", agentName)),
	)
	emit(ctx, "agent.quarantine", otellog.SeverityWarn,
		otellog.String("agent", agentName),
	)
}

// RecordAgentIdleKill records an idle timeout restart (metrics + log event).
func RecordAgentIdleKill(ctx context.Context, agentName string) {
	initInstruments()
	inst.agentIdleKillTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("agent", agentName)),
	)
	emit(ctx, "agent.idle_kill", otellog.SeverityInfo,
		otellog.String("agent", agentName),
	)
}

// RecordReconcileCycle records a reconciliation cycle with counts (metrics + log event).
func RecordReconcileCycle(ctx context.Context, started, stopped, skipped int) {
	initInstruments()
	inst.reconcileCycleTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.Int("started", started),
			attribute.Int("stopped", stopped),
			attribute.Int("skipped", skipped),
		),
	)
	emit(ctx, "reconcile.cycle", otellog.SeverityInfo,
		otellog.Int("started", started),
		otellog.Int("stopped", stopped),
		otellog.Int("skipped", skipped),
	)
}

// RecordNudge records a session nudge send (metrics + log event).
func RecordNudge(ctx context.Context, target string, err error) {
	initInstruments()
	status := statusStr(err)
	inst.nudgeTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("target", target),
			attribute.String("status", status),
		),
	)
	emit(ctx, "session.nudge", severity(err),
		otellog.String("target", target),
		otellog.String("status", status),
		errKV(err),
	)
}

// RecordConfigReload records a config reload attempt (metrics + log event).
func RecordConfigReload(ctx context.Context, revision string, err error) {
	initInstruments()
	status := statusStr(err)
	inst.configReloadTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("status", status)),
	)
	emit(ctx, "config.reload", severity(err),
		otellog.String("revision", revision),
		otellog.String("status", status),
		errKV(err),
	)
}

// RecordControllerLifecycle records a controller lifecycle event (metrics + log event).
// event is "started" or "stopped".
func RecordControllerLifecycle(ctx context.Context, event string) {
	initInstruments()
	inst.controllerTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("event", event)),
	)
	emit(ctx, "controller.lifecycle", otellog.SeverityInfo,
		otellog.String("event", event),
	)
}

// RecordSling records a sling dispatch (metrics + log event).
// target is the agent/pool qualified name, targetType is "agent" or "pool",
// method is "bead" or "formula".
func RecordSling(ctx context.Context, target, targetType, method string, err error) {
	initInstruments()
	status := statusStr(err)
	inst.slingTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("target", target),
			attribute.String("target_type", targetType),
			attribute.String("method", method),
			attribute.String("status", status),
		),
	)
	emit(ctx, "sling.dispatch", severity(err),
		otellog.String("target", target),
		otellog.String("target_type", targetType),
		otellog.String("method", method),
		otellog.String("status", status),
		errKV(err),
	)
}

// RecordBeadStoreHealth records the bead store health status as a gauge.
// healthy=true sets the gauge to 1, healthy=false sets it to 0.
func RecordBeadStoreHealth(ctx context.Context, cityName string, healthy bool) {
	initInstruments()
	var val int64
	if healthy {
		val = 1
	}
	inst.beadStoreHealthy.Record(ctx, val,
		metric.WithAttributes(attribute.String("city", cityName)),
	)
}

// RecordBDCall records a bd CLI invocation with duration (metrics + log event).
// args is the full argument list; args[0] is used as the subcommand label.
// durationMs is the wall-clock time of the subprocess in milliseconds.
// stdout and stderr are the raw process outputs; both are truncated before logging.
//
// stdout and stderr are only included in the log event when GC_LOG_BD_OUTPUT=true.
func RecordBDCall(ctx context.Context, args []string, durationMs float64, err error, stdout []byte, stderr string) {
	initInstruments()
	subcommand := ""
	if len(args) > 0 {
		subcommand = args[0]
	}
	status := statusStr(err)
	attrs := metric.WithAttributes(
		attribute.String("status", status),
		attribute.String("subcommand", subcommand),
	)
	inst.bdTotal.Add(ctx, 1, attrs)
	inst.bdDurationHist.Record(ctx, durationMs, attrs)
	kvs := []otellog.KeyValue{
		otellog.String("subcommand", subcommand),
		otellog.String("args", strings.Join(args, " ")),
		otellog.Float64("duration_ms", durationMs),
		otellog.String("status", status),
		errKV(err),
	}
	// stdout/stderr are opt-in: they may contain tokens or PII returned by bd.
	if os.Getenv("GC_LOG_BD_OUTPUT") == "true" {
		kvs = append(kvs,
			otellog.String("stdout", truncateOutput(string(stdout), maxStdoutLog)),
			otellog.String("stderr", truncateOutput(stderr, maxStderrLog)),
		)
	}
	emit(ctx, "bd.call", severity(err), kvs...)
}

// ── Phase 2 recording functions ──────────────────────────────────────────

// RecordPoolSpawn records a pool member instance being spawned (metrics + log event).
func RecordPoolSpawn(ctx context.Context, agent string, instance int) {
	initInstruments()
	inst.poolSpawnTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("agent", agent),
			attribute.Int("instance", instance),
		),
	)
	emit(ctx, "pool.spawn", otellog.SeverityInfo,
		otellog.String("agent", agent),
		otellog.Int("instance", instance),
	)
}

// RecordPoolRemove records a pool member instance being removed (metrics + log event).
// reason is "scale-down", "drain-timeout", "drain-ack", "orphan", etc.
func RecordPoolRemove(ctx context.Context, agent, reason string) {
	initInstruments()
	inst.poolRemoveTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("agent", agent),
			attribute.String("reason", reason),
		),
	)
	emit(ctx, "pool.remove", otellog.SeverityInfo,
		otellog.String("agent", agent),
		otellog.String("reason", reason),
	)
}

// RecordPoolCheck records a pool scale_check command execution (metrics + log event).
func RecordPoolCheck(ctx context.Context, agent string, durationMs float64, desired int, err error) {
	initInstruments()
	status := statusStr(err)
	inst.poolCheckDurationHist.Record(ctx, durationMs,
		metric.WithAttributes(
			attribute.String("agent", agent),
			attribute.String("status", status),
		),
	)
	emit(ctx, "pool.check", severity(err),
		otellog.String("agent", agent),
		otellog.Float64("duration_ms", durationMs),
		otellog.Int("desired", desired),
		otellog.String("status", status),
		errKV(err),
	)
}

// RecordMailOp records a mail operation (metrics + log event).
// operation is "send", "read", "reply", "delete", "archive", "mark_read", "mark_unread".
func RecordMailOp(ctx context.Context, operation string, err error) {
	initInstruments()
	status := statusStr(err)
	inst.mailOpsTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("operation", operation),
			attribute.String("status", status),
		),
	)
	emit(ctx, "mail.operation", severity(err),
		otellog.String("operation", operation),
		otellog.String("status", status),
		errKV(err),
	)
}

// RecordHTTPRequest records an API request with method, route, status, duration,
// and the data source used to fulfill it (memory, cache, bd_subprocess, sql).
func RecordHTTPRequest(ctx context.Context, method, route string, status int, durationMs float64, dataSource string) {
	initInstruments()
	statusStr := "ok"
	if status >= 500 {
		statusStr = "error"
	}
	attrs := metric.WithAttributes(
		attribute.String("method", method),
		attribute.String("route", route),
		attribute.Int("status", status),
		attribute.String("data_source", dataSource),
	)
	inst.httpRequestTotal.Add(ctx, 1, attrs)
	inst.httpRequestDuration.Record(ctx, durationMs, attrs)

	sev := otellog.SeverityInfo
	if durationMs > 1000 {
		sev = otellog.SeverityWarn
	}
	emit(ctx, "http.request", sev,
		otellog.String("method", method),
		otellog.String("route", route),
		otellog.Int("status", status),
		otellog.Float64("duration_ms", durationMs),
		otellog.String("data_source", dataSource),
		otellog.String("status_class", statusStr),
	)
}

// RecordDrainTransition records a drain lifecycle transition (metrics + log event).
// transition is "begin", "complete", "cancel", "timeout".
func RecordDrainTransition(ctx context.Context, sessionName, reason, transition string) {
	initInstruments()
	inst.drainTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("session", sessionName),
			attribute.String("reason", reason),
			attribute.String("transition", transition),
		),
	)
	emit(ctx, "drain.transition", otellog.SeverityInfo,
		otellog.String("session", sessionName),
		otellog.String("reason", reason),
		otellog.String("transition", transition),
	)
}
