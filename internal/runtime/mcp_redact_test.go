package runtime

import (
	"strings"
	"testing"
)

func TestRedactMCPServerConfigsReplacesCredentials(t *testing.T) {
	in := []MCPServerConfig{{
		Name:      "remote",
		Transport: MCPTransportHTTP,
		Command:   "/bin/mcp",
		Args: []string{
			"--serve",
			"--api-key",
			"super-secret",
			"--token=abc123",
			"Authorization: Bearer secret",
			"https://user:pass@example.invalid/mcp?token=abc123",
		},
		Env:     map[string]string{"API_TOKEN": "super-secret", "PLAIN": "visible-env"},
		URL:     "https://user:pass@example.invalid/mcp?token=abc123",
		Headers: map[string]string{"Authorization": "Bearer secret"},
	}}
	got := RedactMCPServerConfigs(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	s := got[0]
	wantArgs := []string{
		"--serve",
		"--api-key",
		RedactedMCPValue,
		"--token=" + RedactedMCPValue,
		RedactedMCPValue,
		"https://__redacted__:__redacted__@example.invalid/mcp?token=" + RedactedMCPValue,
	}
	if strings.Join(s.Args, "|") != strings.Join(wantArgs, "|") {
		t.Fatalf("Args = %q, want %q", s.Args, wantArgs)
	}
	// Every env and header value is replaced, secret-looking or not: a
	// template can render any value from the agent environment.
	for k, v := range s.Env {
		if v != RedactedMCPValue {
			t.Fatalf("Env[%s] = %q, want %q", k, v, RedactedMCPValue)
		}
	}
	if s.Headers["Authorization"] != RedactedMCPValue {
		t.Fatalf("Headers[Authorization] = %q, want redacted", s.Headers["Authorization"])
	}
	if want := "https://__redacted__:__redacted__@example.invalid/mcp?token=" + RedactedMCPValue; s.URL != want {
		t.Fatalf("URL = %q, want %q", s.URL, want)
	}
	if s.Name != "remote" || s.Command != "/bin/mcp" || s.Transport != MCPTransportHTTP {
		t.Fatalf("non-secret fields changed: %+v", s)
	}
	// The input is not mutated.
	if in[0].Env["API_TOKEN"] != "super-secret" || in[0].Args[2] != "super-secret" {
		t.Fatalf("RedactMCPServerConfigs mutated its input: %+v", in[0])
	}
}

func TestRedactMCPServerConfigsEmpty(t *testing.T) {
	if got := RedactMCPServerConfigs(nil); got != nil {
		t.Fatalf("RedactMCPServerConfigs(nil) = %v, want nil", got)
	}
}

func TestRedactMCPURLLeavesPlainURL(t *testing.T) {
	const plain = "https://example.invalid/mcp"
	if got := redactMCPURL(plain); got != plain {
		t.Fatalf("redactMCPURL(%q) = %q, want unchanged", plain, got)
	}
}

func TestIsSensitiveMCPName(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "--api-key", "Authorization", "db_password", "COOKIE"} {
		if !IsSensitiveMCPName(name) {
			t.Errorf("IsSensitiveMCPName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"--serve", "PATH", "--port"} {
		if IsSensitiveMCPName(name) {
			t.Errorf("IsSensitiveMCPName(%q) = true, want false", name)
		}
	}
}
