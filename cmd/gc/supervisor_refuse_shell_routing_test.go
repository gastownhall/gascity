package main

import "testing"

// A routing-class provider name (base URL or model selector) that exists only
// in the calling shell must not be persisted into the supervisor service env.
// Credential-class names from the same shell keep their existing behavior.
// Refs gastownhall/gascity#9257.
func TestSupervisorServiceExtraEnvRefusesShellOnlyRoutingNames(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_SUPERVISOR_ENV", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "credential-test-value")
	t.Setenv("ANTHROPIC_BASE_URL", "https://routing.example.test")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "routing-test-model")

	got := make(map[string]string)
	for _, entry := range supervisorServiceExtraEnv() {
		got[entry.Name] = entry.Value
	}

	if _, ok := got["ANTHROPIC_AUTH_TOKEN"]; !ok {
		t.Errorf("ANTHROPIC_AUTH_TOKEN (credential, shell-only) was not persisted; want it persisted")
	}
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_OPUS_MODEL"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s (routing, shell-only) was persisted; want it refused", name)
		}
	}
}

// The explicit opt-in: a routing-class name from the calling shell is persisted
// when it is listed in GC_SUPERVISOR_ENV. Refs gastownhall/gascity#9257.
func TestSupervisorServiceExtraEnvPersistsOptedInRoutingName(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_SUPERVISOR_ENV", "ANTHROPIC_BASE_URL")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "credential-test-value")
	t.Setenv("ANTHROPIC_BASE_URL", "https://routing.example.test")

	got := make(map[string]string)
	for _, entry := range supervisorServiceExtraEnv() {
		got[entry.Name] = entry.Value
	}

	if got["ANTHROPIC_BASE_URL"] != "https://routing.example.test" {
		t.Errorf("ANTHROPIC_BASE_URL listed in GC_SUPERVISOR_ENV was not persisted; got %q", got["ANTHROPIC_BASE_URL"])
	}
}
