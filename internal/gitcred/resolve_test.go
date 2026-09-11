package gitcred

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveHelper(t *testing.T) {
	cred, err := Resolve(Rule{Match: "github.com", Helper: "printf 'ghp_tok\\n'"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Password != "ghp_tok" {
		t.Fatalf("want trimmed helper stdout, got %q", cred.Password)
	}
	if cred.Username != DefaultUsername {
		t.Fatalf("want default username, got %q", cred.Username)
	}
}

func TestResolveHelperCustomUsername(t *testing.T) {
	cred, err := Resolve(Rule{Match: "github.com", Username: "bot", Helper: "echo tok"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Username != "bot" {
		t.Fatalf("want custom username, got %q", cred.Username)
	}
}

func TestResolveTokenFileTildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tokenPath := filepath.Join(home, "tok")
	if err := os.WriteFile(tokenPath, []byte("  secret-token\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cred, err := Resolve(Rule{Match: "github.com", TokenFile: "~/tok"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Password != "secret-token" {
		t.Fatalf("want trimmed token file contents, got %q", cred.Password)
	}
}

func TestResolveTokenEnvUnsetErrorNamesTheName(t *testing.T) {
	t.Setenv("GC_TEST_MISSING_TOK", "")
	_, err := Resolve(Rule{Match: "github.com", TokenEnv: "GC_TEST_MISSING_TOK"})
	if err == nil {
		t.Fatalf("expected error for unset env var")
	}
	if !strings.Contains(err.Error(), "GC_TEST_MISSING_TOK") {
		t.Fatalf("error must name the env var, got %v", err)
	}
}

func TestResolveErrorsNeverLeakSecret(t *testing.T) {
	// token_file points at a missing path; the error names the pointer, not a
	// secret (there is none), and never the resolved absolute path token value.
	_, err := Resolve(Rule{Match: "github.com", TokenFile: "/nonexistent/token"})
	if err == nil {
		t.Fatalf("expected error for missing token file")
	}
	if !strings.Contains(err.Error(), "/nonexistent/token") {
		t.Fatalf("error should name the file pointer, got %v", err)
	}
}

func TestResolveTokenEnvValue(t *testing.T) {
	t.Setenv("GC_TEST_TOK", "envtok")
	cred, err := Resolve(Rule{Match: "github.com", TokenEnv: "GC_TEST_TOK"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Password != "envtok" {
		t.Fatalf("want env token value, got %q", cred.Password)
	}
}

func TestRunCredentialCommandDeclineOnEmptyOutput(t *testing.T) {
	_, ok, err := RunCredentialCommand("true", Request{Host: "github.com"})
	if err != nil {
		t.Fatalf("RunCredentialCommand: %v", err)
	}
	if ok {
		t.Fatalf("empty stdout must decline")
	}
}

func TestRunCredentialCommandEmit(t *testing.T) {
	cred, ok, err := RunCredentialCommand("printf 'username=bot\\npassword=tok\\n'", Request{Host: "github.com"})
	if err != nil {
		t.Fatalf("RunCredentialCommand: %v", err)
	}
	if !ok || cred.Username != "bot" || cred.Password != "tok" {
		t.Fatalf("unexpected credential: %+v ok=%v", cred, ok)
	}
}
