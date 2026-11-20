package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
)

// isolateSupervisorRoutingEnv gives a test an empty HOME and GC_HOME, a fixed
// PATH, no GC_SUPERVISOR_ENV or omit flag, stubbed service managers, and no
// provider-prefixed name from the test process's own environment (the shell
// scan skips empty values).
func isolateSupervisorRoutingEnv(t *testing.T) {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("GC_SUPERVISOR_ENV", "")
	t.Setenv(supervisorOmitProviderCredsEnv, "")
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && processenv.IsProviderCredentialEnv(key) {
			t.Setenv(key, "")
		}
	}
	stubServiceManagers(t, "", nil, "")
}

// incidentRoutingNames are the shell-only ANTHROPIC_ routing names from the
// #9257 incident, sorted.
var incidentRoutingNames = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
}

// setIncidentEnv reproduces the #9257 host: secrets.env holds three
// ANTHROPIC_ tokens and the calling shell exports four ANTHROPIC_ routing
// names and no ANTHROPIC_ credential.
func setIncidentEnv(t *testing.T) {
	t.Helper()
	writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN=tok-file-1\nANTHROPIC_AUTH_TOKEN2=tok-file-2\nANTHROPIC_AUTH_TOKEN_ZAI=tok-file-zai\n")
	for _, name := range incidentRoutingNames {
		t.Setenv(name, "shell-"+strings.ToLower(name))
	}
}

// TestSupervisorServiceEnvRejectsShellOnlyRoutingBesideFileCredential
// reproduces issue #9257: the provider credential comes only from
// ${GC_HOME}/secrets.env while ANTHROPIC_BASE_URL is exported only in the
// calling shell. The routing name must not be persisted without an explicit
// GC_SUPERVISOR_ENV opt-in, so the service env never pairs a file-tier
// credential with a shell-tier endpoint.
func TestSupervisorServiceEnvRejectsShellOnlyRoutingBesideFileCredential(t *testing.T) {
	isolateSupervisorRoutingEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://other-provider.example.test")
	writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN=sk-from-file\n")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	if got["ANTHROPIC_AUTH_TOKEN"] != "sk-from-file" {
		t.Fatalf("ExtraEnv[ANTHROPIC_AUTH_TOKEN] = %q, want file credential", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if v, ok := got["ANTHROPIC_BASE_URL"]; ok {
		t.Fatalf("ExtraEnv persisted shell-only routing ANTHROPIC_BASE_URL=%q beside a secrets.env-sourced credential (mixed-tier credential/routing chimera)", v)
	}
}

// TestSupervisorServiceEnvWithholdsIncidentShellRouting pins AC-1: with every
// ANTHROPIC_ credential from secrets.env and the incident's four routing names
// only in the shell, the tokens persist from the file, none of the routing
// names persists, and one ANTHROPIC_ group records them withheld.
func TestSupervisorServiceEnvWithholdsIncidentShellRouting(t *testing.T) {
	isolateSupervisorRoutingEnv(t)
	setIncidentEnv(t)

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	for name, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN":     "tok-file-1",
		"ANTHROPIC_AUTH_TOKEN2":    "tok-file-2",
		"ANTHROPIC_AUTH_TOKEN_ZAI": "tok-file-zai",
	} {
		if got[name] != want {
			t.Errorf("ExtraEnv[%s] = %q, want %q", name, got[name], want)
		}
	}
	for _, name := range incidentRoutingNames {
		if v, ok := got[name]; ok {
			t.Errorf("ExtraEnv persisted shell-only routing %s=%q beside secrets.env credentials", name, v)
		}
	}
	want := []supervisorWithheldEnv{{
		Prefix:            "ANTHROPIC_",
		Names:             incidentRoutingNames,
		CredentialSources: []supervisorEnvSource{supervisorEnvSourceSecretsFile},
	}}
	if !reflect.DeepEqual(data.WithheldEnv, want) {
		t.Fatalf("WithheldEnv = %+v, want %+v", data.WithheldEnv, want)
	}
}

// TestSupervisorServiceEnvWithholdsShellRoutingPerPrefix pins that the gate
// is per provider prefix, not Anthropic-specific.
func TestSupervisorServiceEnvWithholdsShellRoutingPerPrefix(t *testing.T) {
	isolateSupervisorRoutingEnv(t)
	t.Setenv("OPENAI_BASE_URL", "https://shell.example.test/v1")
	writeSupervisorSecretsEnvFile(t, "OPENAI_API_KEY=sk-openai-file\n")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	got := supervisorServiceEnvMap(data.ExtraEnv)
	if got["OPENAI_API_KEY"] != "sk-openai-file" {
		t.Errorf("ExtraEnv[OPENAI_API_KEY] = %q, want file credential", got["OPENAI_API_KEY"])
	}
	if v, ok := got["OPENAI_BASE_URL"]; ok {
		t.Errorf("ExtraEnv persisted shell-only routing OPENAI_BASE_URL=%q beside a secrets.env credential", v)
	}
	want := []supervisorWithheldEnv{{
		Prefix:            "OPENAI_",
		Names:             []string{"OPENAI_BASE_URL"},
		CredentialSources: []supervisorEnvSource{supervisorEnvSourceSecretsFile},
	}}
	if !reflect.DeepEqual(data.WithheldEnv, want) {
		t.Fatalf("WithheldEnv = %+v, want %+v", data.WithheldEnv, want)
	}
}

// TestSupervisorServiceEnvKeepsShellRoutingWithoutDurableOnlyCredentials pins
// AC-2: shell routing persists as it always has when no credential under the
// prefix comes from a durable tier, or when the shell itself supplies one.
func TestSupervisorServiceEnvKeepsShellRoutingWithoutDurableOnlyCredentials(t *testing.T) {
	t.Run("no secrets.env", func(t *testing.T) {
		isolateSupervisorRoutingEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example.test")
		t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

		data, err := buildSupervisorServiceData()
		if err != nil {
			t.Fatalf("buildSupervisorServiceData: %v", err)
		}
		got := supervisorServiceEnvMap(data.ExtraEnv)
		if got["ANTHROPIC_BASE_URL"] != "https://shell.example.test" {
			t.Errorf("ExtraEnv[ANTHROPIC_BASE_URL] = %q, want shell value", got["ANTHROPIC_BASE_URL"])
		}
		if got["OLLAMA_HOST"] != "http://127.0.0.1:11434" {
			t.Errorf("ExtraEnv[OLLAMA_HOST] = %q, want shell value", got["OLLAMA_HOST"])
		}
		if len(data.WithheldEnv) != 0 {
			t.Errorf("WithheldEnv = %+v, want none", data.WithheldEnv)
		}
	})
	t.Run("shell supplies a credential", func(t *testing.T) {
		isolateSupervisorRoutingEnv(t)
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-shell")
		t.Setenv("ANTHROPIC_BASE_URL", "https://shell.example.test")
		writeSupervisorSecretsEnvFile(t, "ANTHROPIC_AUTH_TOKEN_ZAI=tok-file-zai\n")

		data, err := buildSupervisorServiceData()
		if err != nil {
			t.Fatalf("buildSupervisorServiceData: %v", err)
		}
		got := supervisorServiceEnvMap(data.ExtraEnv)
		for name, want := range map[string]string{
			"ANTHROPIC_AUTH_TOKEN":     "tok-shell",
			"ANTHROPIC_BASE_URL":       "https://shell.example.test",
			"ANTHROPIC_AUTH_TOKEN_ZAI": "tok-file-zai",
		} {
			if got[name] != want {
				t.Errorf("ExtraEnv[%s] = %q, want %q", name, got[name], want)
			}
		}
		if len(data.WithheldEnv) != 0 {
			t.Errorf("WithheldEnv = %+v, want none", data.WithheldEnv)
		}
	})
}
