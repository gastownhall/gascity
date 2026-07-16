package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestExplainAgentRendersLifecycleTimeoutKeys guards #3965: gc config explain
// agent blocks omitted the idle/lifecycle-timeout keys (idle_timeout,
// sleep_after_idle, max_session_age, max_session_age_jitter) entirely, so their
// resolved values and provenance had to be read from the pack agent.toml
// directly. Each set key must now render a row.
func TestExplainAgentRendersLifecycleTimeoutKeys(t *testing.T) {
	const source = "/city/packs/refinery/agent.toml"
	agent := config.Agent{
		Name:                "refinery",
		IdleTimeout:         "15m",
		SleepAfterIdle:      "30m",
		MaxSessionAge:       "6h",
		MaxSessionAgeJitter: "10m",
	}
	prov := config.Provenance{
		Root:   source,
		Agents: map[string]string{agent.QualifiedName(): source},
	}

	var buf bytes.Buffer
	explainAgent(&buf, &agent, &prov)
	out := buf.String()

	for _, want := range []struct{ key, value string }{
		{"idle_timeout", "15m"},
		{"sleep_after_idle", "30m"},
		{"max_session_age", "6h"},
		{"max_session_age_jitter", "10m"},
	} {
		if !strings.Contains(out, want.key) {
			t.Errorf("explain output missing key %q; got:\n%s", want.key, out)
			continue
		}
		if !strings.Contains(out, want.value) {
			t.Errorf("explain output missing value %q for key %q; got:\n%s", want.value, want.key, out)
		}
	}
}

// TestExplainAgentOmitsUnsetLifecycleTimeoutKeys confirms the new rows follow
// the existing conditional pattern: keys left empty produce no row (no spurious
// "= " lines for unconfigured timeouts).
func TestExplainAgentOmitsUnsetLifecycleTimeoutKeys(t *testing.T) {
	agent := config.Agent{Name: "plain"}
	prov := config.Provenance{Root: "/city/city.toml"}

	var buf bytes.Buffer
	explainAgent(&buf, &agent, &prov)
	out := buf.String()

	for _, key := range []string{"idle_timeout", "sleep_after_idle", "max_session_age", "max_session_age_jitter"} {
		if strings.Contains(out, key) {
			t.Errorf("explain output should omit unset key %q; got:\n%s", key, out)
		}
	}
}
