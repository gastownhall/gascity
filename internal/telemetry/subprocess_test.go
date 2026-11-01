package telemetry

import (
	"os"
	"strings"
	"testing"
)

func TestBuildGCResourceAttrs_Empty(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	result := buildGCResourceAttrs()
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestBuildGCResourceAttrs_AllVars(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_RIG", "tower")
	t.Setenv("GC_CITY", "/tmp/bright-lights")

	result := buildGCResourceAttrs()
	for _, want := range []string{"gc.agent=mayor", "gc.rig=tower", "gc.city=/tmp/bright-lights"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %q in result, got %q", want, result)
		}
	}
}

func TestBuildGCResourceAttrs_Comma(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "a")
	t.Setenv("GC_RIG", "b")
	t.Setenv("GC_CITY", "")

	result := buildGCResourceAttrs()
	if !strings.Contains(result, ",") {
		t.Errorf("expected comma-separated result, got %q", result)
	}
}

func TestBuildGCResourceAttrs_PrefersAlias(t *testing.T) {
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_AGENT", "bl-9jl")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	result := buildGCResourceAttrs()
	if !strings.Contains(result, "gc.agent=mayor") {
		t.Errorf("expected gc.agent=mayor (from GC_ALIAS), got %q", result)
	}
	if strings.Contains(result, "bl-9jl") {
		t.Errorf("gc.agent should not contain bead ID, got %q", result)
	}
}

func TestStripGCResourceAttrs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no gc pairs", "team=platform,deployment.environment=prod", "team=platform,deployment.environment=prod"},
		{"only gc pairs", "gc.agent=a,gc.rig=r,gc.city=c", ""},
		{"mixed", "team=platform,gc.agent=a,env=prod,gc.rig=r", "team=platform,env=prod"},
		{"malformed trailing comma preserved", "team=platform,", "team=platform,"},
		{"gc key without value preserved", "gc.agent,team=platform", "gc.agent,team=platform"},
		{"spaced gc key stripped", "team=platform, gc.agent=a", "team=platform"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripGCResourceAttrs(tc.in); got != tc.want {
				t.Errorf("stripGCResourceAttrs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestResourceAttrsForChildren_ReplacesInheritedSpawnerIdentity(t *testing.T) {
	// A session env created by another agent carries that agent's identity
	// labels; the merge must replace them with this process's own labels
	// instead of appending duplicates or forwarding the spawner's identity.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform,gc.agent=mayor,gc.rig=tower")
	t.Setenv("GC_ALIAS", "polecat-1")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "refinery")
	t.Setenv("GC_CITY", "")

	want := "team=platform,gc.agent=polecat-1,gc.rig=refinery"
	if got := resourceAttrsForChildren(); got != want {
		t.Errorf("resourceAttrsForChildren() = %q, want %q", got, want)
	}
}

func TestOTELEnvMap_Disabled(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "")
	m := OTELEnvMap()
	if m != nil {
		t.Errorf("expected nil when telemetry disabled, got %v", m)
	}
}

func TestOTELEnvMap_UsesStandardNamesNotLegacy(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	m := OTELEnvMap()
	for _, legacy := range []string{"BD_OTEL_METRICS_URL", "BD_OTEL_LOGS_URL"} {
		if _, ok := m[legacy]; ok {
			t.Errorf("OTELEnvMap must not emit legacy %q (bd warns on its presence)", legacy)
		}
	}
	if m["BD_OTEL_ENABLED"] != "true" {
		t.Errorf("BD_OTEL_ENABLED = %q, want %q", m["BD_OTEL_ENABLED"], "true")
	}
	if m["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] != "http://collector:4318/v1/metrics" {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT = %q", m["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"])
	}
	if m["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != "http://collector:4318/v1/logs" {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT = %q", m["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"])
	}
}

func TestOTELEnvMap_StandardEndpointOnly(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	m := OTELEnvMap()
	if m == nil {
		t.Fatal("expected non-nil map when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	}
	if want := "http://collector:4318/v1/metrics"; m["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] != want {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT = %q, want %q", m["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"], want)
	}
	if want := "http://collector:4318/v1/logs"; m["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != want {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT = %q, want %q", m["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"], want)
	}
	if m["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Errorf("CLAUDE_CODE_ENABLE_TELEMETRY = %q", m["CLAUDE_CODE_ENABLE_TELEMETRY"])
	}
}

func TestOTELEnvMap_Enabled(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "http://localhost:9428/insert/opentelemetry/v1/logs")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	m := OTELEnvMap()
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if m["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] != "http://localhost:8428/opentelemetry/api/v1/push" {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT = %q", m["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"])
	}
	if m["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != "http://localhost:9428/insert/opentelemetry/v1/logs" {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT = %q", m["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"])
	}
	if m["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Errorf("CLAUDE_CODE_ENABLE_TELEMETRY = %q", m["CLAUDE_CODE_ENABLE_TELEMETRY"])
	}
}

func TestOTELEnvMap_NoLogsURL_FallsBackToDefault(t *testing.T) {
	// The resolved logs endpoint mirrors what the main provider uses, so bd
	// emits to the same collector as gc when GC_OTEL_LOGS_URL is unset.
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	m := OTELEnvMap()
	if got := m["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"]; got != DefaultLogsURL {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT = %q, want DefaultLogsURL %q", got, DefaultLogsURL)
	}
}

func TestOTELEnvMap_WithResourceAttrs(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_RIG", "tower")
	t.Setenv("GC_CITY", "")

	m := OTELEnvMap()
	attrs := m["OTEL_RESOURCE_ATTRIBUTES"]
	if !strings.Contains(attrs, "gc.agent=mayor") {
		t.Errorf("expected gc.agent in OTEL_RESOURCE_ATTRIBUTES, got %q", attrs)
	}
}

func TestOTELEnvMap_MergesExistingResourceAttrs(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform,deployment.environment=prod")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	m := OTELEnvMap()
	want := "team=platform,deployment.environment=prod,gc.agent=mayor"
	if got := m["OTEL_RESOURCE_ATTRIBUTES"]; got != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want %q", got, want)
	}
}

func TestOTELEnvMap_PreservesUserAttrsWithoutGCContext(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	m := OTELEnvMap()
	if got := m["OTEL_RESOURCE_ATTRIBUTES"]; got != "team=platform" {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want user value preserved", got)
	}
}

func TestOTELEnvForSubprocess_Disabled(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "")
	env := OTELEnvForSubprocess()
	if env != nil {
		t.Errorf("expected nil when telemetry disabled, got %v", env)
	}
}

func TestOTELEnvForSubprocess_StandardEndpointOnly(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	env := OTELEnvForSubprocess()
	if env == nil {
		t.Fatal("expected non-nil env when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	}
	want := map[string]bool{
		"BD_OTEL_ENABLED=true": false,
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://collector:4318/v1/metrics": false,
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://collector:4318/v1/logs":       false,
		"OTEL_RESOURCE_ATTRIBUTES=team=platform,gc.agent=mayor":                false,
		"CLAUDE_CODE_ENABLE_TELEMETRY=1":                                       false,
	}
	for _, e := range env {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for entry, seen := range want {
		if !seen {
			t.Errorf("expected %q in subprocess env, got %v", entry, env)
		}
	}
}

func TestOTELEnvForSubprocess_BothURLs(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "http://localhost:9428/insert/opentelemetry/v1/logs")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	env := OTELEnvForSubprocess()
	if len(env) == 0 {
		t.Fatal("expected non-empty env")
	}

	hasMetrics, hasLogs, hasEnabled := false, false, false
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="):
			hasMetrics = true
		case strings.HasPrefix(e, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT="):
			hasLogs = true
		case e == "BD_OTEL_ENABLED=true":
			hasEnabled = true
		case strings.HasPrefix(e, "BD_OTEL_METRICS_URL=") || strings.HasPrefix(e, "BD_OTEL_LOGS_URL="):
			t.Errorf("legacy name must not appear in subprocess env: %q", e)
		}
	}
	if !hasMetrics {
		t.Error("expected OTEL_EXPORTER_OTLP_METRICS_ENDPOINT in subprocess env")
	}
	if !hasLogs {
		t.Error("expected OTEL_EXPORTER_OTLP_LOGS_ENDPOINT in subprocess env")
	}
	if !hasEnabled {
		t.Error("expected BD_OTEL_ENABLED=true in subprocess env")
	}
}

func TestSetProcessOTELAttrs_Disabled(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")

	SetProcessOTELAttrs()

	if v := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); v != "" {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT should not be set when telemetry disabled, got %q", v)
	}
}

func TestSetProcessOTELAttrs_NoLegacyNames(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")
	t.Setenv("BD_OTEL_ENABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	SetProcessOTELAttrs()

	for _, legacy := range []string{"BD_OTEL_METRICS_URL", "BD_OTEL_LOGS_URL"} {
		if v := os.Getenv(legacy); v != "" {
			t.Errorf("%s must not be set by gc (bd warns on its presence), got %q", legacy, v)
		}
	}
	if got := os.Getenv("BD_OTEL_ENABLED"); got != "true" {
		t.Errorf("BD_OTEL_ENABLED = %q, want %q", got, "true")
	}
}

func TestSetProcessOTELAttrs_StandardEndpointOnly(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvLogsURL, "")
	t.Setenv(EnvOTLPEndpoint, "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	SetProcessOTELAttrs()

	if got, want := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"), "http://collector:4318/v1/metrics"; got != want {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT = %q, want %q", got, want)
	}
	if got, want := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"), "http://collector:4318/v1/logs"; got != want {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT = %q, want %q", got, want)
	}
	if got := os.Getenv("CLAUDE_CODE_ENABLE_TELEMETRY"); got != "1" {
		t.Errorf("CLAUDE_CODE_ENABLE_TELEMETRY = %q, want %q", got, "1")
	}
}

func TestSetProcessOTELAttrs_Enabled(t *testing.T) {
	metricsURL := "http://localhost:8428/opentelemetry/api/v1/push"
	logsURL := "http://localhost:9428/insert/opentelemetry/v1/logs"
	t.Setenv(EnvMetricsURL, metricsURL)
	t.Setenv(EnvLogsURL, logsURL)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")

	SetProcessOTELAttrs()

	if got := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); got != metricsURL {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT = %q, want %q", got, metricsURL)
	}
	if got := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); got != logsURL {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT = %q, want %q", got, logsURL)
	}
	if got := os.Getenv("CLAUDE_CODE_ENABLE_TELEMETRY"); got != "1" {
		t.Errorf("CLAUDE_CODE_ENABLE_TELEMETRY = %q, want %q", got, "1")
	}
}

func TestSetProcessOTELAttrs_SetsResourceAttrs(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_RIG", "tower")
	t.Setenv("GC_CITY", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")

	SetProcessOTELAttrs()

	got := os.Getenv("OTEL_RESOURCE_ATTRIBUTES")
	if got == "" {
		t.Error("expected OTEL_RESOURCE_ATTRIBUTES to be set")
	}
	if !strings.Contains(got, "gc.agent=mayor") {
		t.Errorf("expected gc.agent in OTEL_RESOURCE_ATTRIBUTES, got %q", got)
	}
}

func TestSetProcessOTELAttrs_MergesExistingResourceAttrs(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform")

	SetProcessOTELAttrs()

	want := "team=platform,gc.agent=mayor"
	if got := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); got != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want merged %q", got, want)
	}
}

func TestSetProcessOTELAttrs_RepeatedMergeIsIdempotent(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_RIG", "tower")
	t.Setenv("GC_CITY", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform")

	SetProcessOTELAttrs()
	SetProcessOTELAttrs()

	want := "team=platform,gc.agent=mayor,gc.rig=tower"
	if got := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); got != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES after repeated merge = %q, want %q", got, want)
	}
}

func TestSetProcessOTELAttrs_ThenOTELEnvMap_NoDuplicateLabels(t *testing.T) {
	// gc startup calls SetProcessOTELAttrs, then the session-env path calls
	// OTELEnvMap in the same process; the map value must not accumulate a
	// second copy of the GC labels from the already-mutated process env.
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_RIG", "tower")
	t.Setenv("GC_CITY", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform")

	SetProcessOTELAttrs()
	m := OTELEnvMap()

	want := "team=platform,gc.agent=mayor,gc.rig=tower"
	if got := m["OTEL_RESOURCE_ATTRIBUTES"]; got != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want %q", got, want)
	}
}

func TestSetProcessOTELAttrs_PreservesUserAttrsWithoutGCContext(t *testing.T) {
	t.Setenv(EnvMetricsURL, "http://localhost:8428/opentelemetry/api/v1/push")
	t.Setenv(EnvLogsURL, "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_CITY", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform")

	SetProcessOTELAttrs()

	if got := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); got != "team=platform" {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want user value untouched", got)
	}
}
