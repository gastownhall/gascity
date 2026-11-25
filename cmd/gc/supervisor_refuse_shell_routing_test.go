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
