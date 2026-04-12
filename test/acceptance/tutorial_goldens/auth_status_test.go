//go:build acceptance_c

package tutorialgoldens

import (
	"os"
	"path/filepath"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestClaudeStatusOutputLoggedIn(t *testing.T) {
	t.Parallel()

	if !claudeStatusOutputLoggedIn([]byte(`{"loggedIn":true}`)) {
		t.Fatal("expected loggedIn=true to be accepted")
	}
	if claudeStatusOutputLoggedIn([]byte(`{"loggedIn":false}`)) {
		t.Fatal("expected loggedIn=false to be rejected")
	}
	if claudeStatusOutputLoggedIn([]byte(`not json`)) {
		t.Fatal("expected invalid JSON to be rejected")
	}
}

func TestCodexStatusOutputLoggedIn(t *testing.T) {
	t.Parallel()

	if !codexStatusOutputLoggedIn([]byte("Logged in using ChatGPT\n")) {
		t.Fatal("expected successful login text to be accepted")
	}
	if codexStatusOutputLoggedIn([]byte("Not logged in\n")) {
		t.Fatal("expected unauthenticated output to be rejected")
	}
}

func TestLoadEnvFileAppliesSupportedAssignments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := writeEnvFixture(path, "# comment\nexport CLAUDE_CODE_OAUTH_TOKEN=token-value\nOPENAI_API_KEY='openai-value'\nEMPTY=\n"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("EMPTY", "")
	if err := loadEnvFile(path); err != nil {
		t.Fatalf("loadEnvFile() error = %v", err)
	}

	if got := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); got != "token-value" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want %q", got, "token-value")
	}
	if got := os.Getenv("OPENAI_API_KEY"); got != "openai-value" {
		t.Fatalf("OPENAI_API_KEY = %q, want %q", got, "openai-value")
	}
	if got := os.Getenv("EMPTY"); got != "" {
		t.Fatalf("EMPTY = %q, want empty string", got)
	}
}

func TestLoadEnvFilePreservesExistingValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := writeEnvFixture(path, "CLAUDE_CODE_OAUTH_TOKEN=token-value\n"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "existing-token")
	if err := loadEnvFile(path); err != nil {
		t.Fatalf("loadEnvFile() error = %v", err)
	}
	if got := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); got != "existing-token" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want existing value", got)
	}
}

func TestLoadEnvFileRejectsMalformedAssignments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := writeEnvFixture(path, "NOT_AN_ASSIGNMENT\n"); err != nil {
		t.Fatal(err)
	}

	if err := loadEnvFile(path); err == nil {
		t.Fatal("loadEnvFile() error = nil, want malformed assignment error")
	}
}

func TestEnsureTutorialUserEnvBackfillsLogname(t *testing.T) {
	env := helpers.NewEnv("", t.TempDir(), t.TempDir())
	env.With("USER", "csells")
	env.With("LOGNAME", "")
	ensureTutorialUserEnv(env)

	if got := env.Get("USER"); got != "csells" {
		t.Fatalf("USER = %q, want %q", got, "csells")
	}
	if got := env.Get("LOGNAME"); got != "csells" {
		t.Fatalf("LOGNAME = %q, want %q", got, "csells")
	}
}

func TestResolveTutorialUserIdentityPrefersProvidedValues(t *testing.T) {
	t.Parallel()

	userName, login := resolveTutorialUserIdentity("user-a", "login-b")
	if userName != "user-a" || login != "login-b" {
		t.Fatalf("resolveTutorialUserIdentity() = (%q, %q), want (%q, %q)", userName, login, "user-a", "login-b")
	}
}

func writeEnvFixture(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
