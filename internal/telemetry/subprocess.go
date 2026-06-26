package telemetry

import (
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// gcIdentityAttrs returns the GC identity resource attributes (gc.agent,
// gc.rig, gc.city) parsed from the GC context vars in the current process
// environment. Empty when no GC context vars are set.
func gcIdentityAttrs() []attribute.KeyValue {
	var attrs []attribute.KeyValue
	agent := os.Getenv("GC_ALIAS")
	if agent == "" {
		agent = os.Getenv("GC_AGENT")
	}
	if agent != "" {
		attrs = append(attrs, attribute.String("gc.agent", agent))
	}
	if v := os.Getenv("GC_RIG"); v != "" {
		attrs = append(attrs, attribute.String("gc.rig", v))
	}
	if v := os.Getenv("GC_CITY"); v != "" {
		attrs = append(attrs, attribute.String("gc.city", v))
	}
	return attrs
}

// buildGCResourceAttrs builds the OTEL_RESOURCE_ATTRIBUTES value from GC context
// vars present in the current process environment.
// Returns "" when no GC vars are found.
func buildGCResourceAttrs() string {
	identity := gcIdentityAttrs()
	pairs := make([]string, 0, len(identity))
	for _, kv := range identity {
		pairs = append(pairs, string(kv.Key)+"="+kv.Value.AsString())
	}
	return strings.Join(pairs, ",")
}

// gcResourceAttrKeys are the identity keys gc injects into
// OTEL_RESOURCE_ATTRIBUTES for the processes it spawns.
var gcResourceAttrKeys = map[string]bool{
	"gc.agent": true,
	"gc.rig":   true,
	"gc.city":  true,
}

// stripGCResourceAttrs removes gc identity pairs (gc.agent, gc.rig, gc.city)
// from a comma-separated OTEL_RESOURCE_ATTRIBUTES value. Other pairs pass
// through untouched, including malformed segments, which the env detector
// already tolerates downstream.
func stripGCResourceAttrs(attrs string) string {
	if attrs == "" {
		return ""
	}
	var kept []string
	for _, segment := range strings.Split(attrs, ",") {
		key, _, hasValue := strings.Cut(segment, "=")
		if hasValue && gcResourceAttrKeys[strings.TrimSpace(key)] {
			continue
		}
		kept = append(kept, segment)
	}
	return strings.Join(kept, ",")
}

// resourceAttrsForChildren returns the OTEL_RESOURCE_ATTRIBUTES value gc hands
// to spawned processes: any pre-existing value with previously injected GC
// identity pairs stripped and the current process's GC context labels
// appended. Stripping first keeps the merge idempotent when gc has already
// rewritten its own environment (SetProcessOTELAttrs) and stops a spawning
// agent's identity from surviving into the labels handed to other agents'
// sessions. Returns "" when neither source has attributes.
func resourceAttrsForChildren() string {
	existing := stripGCResourceAttrs(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"))
	gcAttrs := buildGCResourceAttrs()
	switch {
	case existing == "":
		return gcAttrs
	case gcAttrs == "":
		return existing
	default:
		return existing + "," + gcAttrs
	}
}

// setenvWarn sets an environment variable and reports failures to stderr.
// Telemetry env propagation is best-effort: a failed write must not abort
// gc, but staying silent would make a missing-telemetry diagnosis impossible.
func setenvWarn(key, value string) {
	if err := os.Setenv(key, value); err != nil {
		fmt.Fprintf(os.Stderr, "gc: telemetry: setting %s: %v\n", key, err) //nolint:errcheck // best-effort stderr
	}
}

// SetProcessOTELAttrs sets OTEL-related variables in the current process
// environment so that all bd subprocesses spawned via exec.Command inherit
// them automatically — no per-call injection needed.
//
// Sets:
//   - OTEL_RESOURCE_ATTRIBUTES — GC context labels (gc.agent, gc.rig, gc.city)
//     merged into any pre-existing value; previously injected GC labels are
//     replaced, not duplicated
//   - BD_OTEL_METRICS_URL      — bd's own metrics var (resolved metrics endpoint)
//   - BD_OTEL_LOGS_URL         — bd's own logs var   (resolved logs endpoint)
//   - CLAUDE_CODE_ENABLE_TELEMETRY=1 — enables Claude Code's built-in telemetry
//
// Called once at gc startup when telemetry is active.
// No-op when telemetry is not active — the same activation rule as Init,
// including the OTEL_EXPORTER_OTLP_ENDPOINT fallback.
func SetProcessOTELAttrs() {
	metricsURL, logsURL, enabled := resolveEndpoints()
	if !enabled {
		return
	}
	if buildGCResourceAttrs() != "" {
		setenvWarn("OTEL_RESOURCE_ATTRIBUTES", resourceAttrsForChildren())
	}
	// Mirror the resolved endpoints into bd's own var names so bd
	// subprocesses emit to the same collectors as gc itself.
	setenvWarn("BD_OTEL_METRICS_URL", metricsURL)
	setenvWarn("BD_OTEL_LOGS_URL", logsURL)
	// Enable Claude Code's built-in telemetry for agent sessions.
	setenvWarn("CLAUDE_CODE_ENABLE_TELEMETRY", "1")
}

// OTELEnvForSubprocess returns OTEL environment variables to inject into bd
// subprocesses when cmd.Env is built explicitly (overriding os.Environ).
//
// Complements SetProcessOTELAttrs for callers that construct cmd.Env manually
// so the vars aren't lost when the explicit env slice is built from scratch.
// No gc code path builds cmd.Env from scratch today, so this helper has no
// production caller; it is kept as SDK surface for embedders that do. The
// active propagation paths are SetProcessOTELAttrs (inherited process env)
// and OTELEnvMap (session env merging).
//
// Returns nil when telemetry is not active — the same activation rule as
// Init, including the OTEL_EXPORTER_OTLP_ENDPOINT fallback.
func OTELEnvForSubprocess() []string {
	metricsURL, logsURL, enabled := resolveEndpoints()
	if !enabled {
		return nil
	}
	var env []string
	if attrs := resourceAttrsForChildren(); attrs != "" {
		env = append(env, "OTEL_RESOURCE_ATTRIBUTES="+attrs)
	}
	env = append(env,
		"BD_OTEL_METRICS_URL="+metricsURL,
		"BD_OTEL_LOGS_URL="+logsURL,
		"CLAUDE_CODE_ENABLE_TELEMETRY=1",
	)
	return env
}

// OTELEnvMap returns OTEL environment variables as a map for Gas City's
// mergeEnv() pattern. Returns nil when telemetry is not active — the same
// activation rule as Init, including the OTEL_EXPORTER_OTLP_ENDPOINT fallback.
func OTELEnvMap() map[string]string {
	metricsURL, logsURL, enabled := resolveEndpoints()
	if !enabled {
		return nil
	}
	m := map[string]string{
		"BD_OTEL_METRICS_URL":          metricsURL,
		"BD_OTEL_LOGS_URL":             logsURL,
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
	}
	if attrs := resourceAttrsForChildren(); attrs != "" {
		m["OTEL_RESOURCE_ATTRIBUTES"] = attrs
	}
	return m
}
