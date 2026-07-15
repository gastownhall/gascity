package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/gitcred"
)

func writeGitCredRules(t *testing.T, city, body string, mode os.FileMode) {
	t.Helper()
	dir := filepath.Join(city, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

func clearGitCredEnv(t *testing.T, city string) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(gitcred.EnvCredentialsFile, "")
	t.Setenv(gitcred.EnvCredentialCommand, "")
	t.Setenv(gitcred.EnvCredentialCity, city)
	// Clear the ambient GitHub token env so the built-in github.com default rule
	// stays inert and these tests observe only their own configured rules.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
}

func TestRunGitCredentialGetHappyPath(t *testing.T) {
	city := t.TempDir()
	clearGitCredEnv(t, city)
	writeGitCredRules(t, city, "[[credential]]\nmatch=\"github.com/org\"\nhelper=\"printf 'ghp_tok\\\\n'\"\n", 0o600)

	var stdout, stderr strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo.git\n\n")
	if err := runGitCredential("get", in, &stdout, &stderr); err != nil {
		t.Fatalf("runGitCredential: %v (stderr=%q)", err, stderr.String())
	}
	want := "username=x-access-token\npassword=ghp_tok\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunGitCredentialDeclineOnNoMatch(t *testing.T) {
	city := t.TempDir()
	clearGitCredEnv(t, city)
	writeGitCredRules(t, city, "[[credential]]\nmatch=\"other.com\"\nhelper=\"echo x\"\n", 0o600)

	var stdout, stderr strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo\n\n")
	if err := runGitCredential("get", in, &stdout, &stderr); err != nil {
		t.Fatalf("runGitCredential: %v", err)
	}
	if stdout.String() != "" {
		t.Fatalf("decline must produce zero stdout, got %q", stdout.String())
	}
}

func TestRunGitCredentialGitHubDefaultHTTPSOnly(t *testing.T) {
	// The built-in ambient github.com token is an HTTPS convenience. A plaintext
	// http credential request must be declined so the bearer token is never
	// served over cleartext; an https request still resolves it.
	city := t.TempDir()
	clearGitCredEnv(t, city)
	t.Setenv("GITHUB_TOKEN", "ghp_ambient")

	// protocol=http → declined, zero stdout.
	var httpOut, httpErr strings.Builder
	httpIn := strings.NewReader("protocol=http\nhost=github.com\npath=org/repo\n\n")
	if err := runGitCredential("get", httpIn, &httpOut, &httpErr); err != nil {
		t.Fatalf("runGitCredential(http): %v (stderr=%q)", err, httpErr.String())
	}
	if httpOut.String() != "" {
		t.Fatalf("plaintext http must not receive the ambient token, got %q", httpOut.String())
	}

	// protocol=https → the default rule resolves the ambient token.
	var httpsOut, httpsErr strings.Builder
	httpsIn := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo\n\n")
	if err := runGitCredential("get", httpsIn, &httpsOut, &httpsErr); err != nil {
		t.Fatalf("runGitCredential(https): %v (stderr=%q)", err, httpsErr.String())
	}
	want := "username=x-access-token\npassword=ghp_ambient\n"
	if httpsOut.String() != want {
		t.Fatalf("https stdout = %q, want %q", httpsOut.String(), want)
	}
}

func TestRunGitCredentialStoreEraseUnknownDrainAndExit0(t *testing.T) {
	city := t.TempDir()
	clearGitCredEnv(t, city)
	for _, op := range []string{"store", "erase", "future-op"} {
		var stdout, stderr strings.Builder
		in := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo\n\n")
		if err := runGitCredential(op, in, &stdout, &stderr); err != nil {
			t.Fatalf("op %q: %v", op, err)
		}
		if stdout.String() != "" {
			t.Fatalf("op %q must produce zero stdout, got %q", op, stdout.String())
		}
	}
}

func TestRunGitCredentialInsecurePermsFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are POSIX-only")
	}
	city := t.TempDir()
	clearGitCredEnv(t, city)
	writeGitCredRules(t, city, "[[credential]]\nmatch=\"github.com\"\nhelper=\"echo x\"\n", 0o644)

	var stdout, stderr strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo\n\n")
	if err := runGitCredential("get", in, &stdout, &stderr); err == nil {
		t.Fatalf("expected error on insecure perms")
	}
	if stdout.String() != "" {
		t.Fatalf("stdout must be empty on failure, got %q", stdout.String())
	}
	if stderr.String() == "" {
		t.Fatalf("expected a stderr message")
	}
}

func TestRunGitCredentialTokenEnvUnsetFailsWithoutSecret(t *testing.T) {
	city := t.TempDir()
	clearGitCredEnv(t, city)
	t.Setenv("GC_TEST_MISSING", "")
	writeGitCredRules(t, city, "[[credential]]\nmatch=\"github.com\"\ntoken_env=\"GC_TEST_MISSING\"\n", 0o600)

	var stdout, stderr strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo\n\n")
	if err := runGitCredential("get", in, &stdout, &stderr); err == nil {
		t.Fatalf("expected error for unset token_env")
	}
	if !strings.Contains(stderr.String(), "GC_TEST_MISSING") {
		t.Fatalf("stderr should name the env var, got %q", stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout must be empty on failure")
	}
}

func TestRunGitCredentialErrorNeverLeaksSecret(t *testing.T) {
	city := t.TempDir()
	clearGitCredEnv(t, city)
	t.Setenv("GC_TEST_TOK", "ghp_supersecret")
	// A helper that fails: stderr must not carry the resolved secret. Use a
	// token_file pointer at a path that does not exist so Resolve errors.
	writeGitCredRules(t, city, "[[credential]]\nmatch=\"github.com\"\ntoken_file=\"/nonexistent/secret-token\"\n", 0o600)

	var stdout, stderr strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo\n\n")
	_ = runGitCredential("get", in, &stdout, &stderr)
	if strings.Contains(stderr.String(), "ghp_supersecret") {
		t.Fatalf("stderr leaked a secret: %q", stderr.String())
	}
}

func TestRunGitCredentialSSHRuleDeclines(t *testing.T) {
	city := t.TempDir()
	clearGitCredEnv(t, city)
	writeGitCredRules(t, city, "[[credential]]\nmatch=\"github.com\"\nssh_key_file=\"~/.ssh/id\"\n", 0o600)

	var stdout, stderr strings.Builder
	// The helper serves http(s) transport; an ssh_key_file rule is transport-
	// incompatible (served via GIT_SSH_COMMAND on the injection side), so the
	// helper declines silently.
	in := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo\n\n")
	if err := runGitCredential("get", in, &stdout, &stderr); err != nil {
		t.Fatalf("runGitCredential: %v", err)
	}
	if stdout.String() != "" {
		t.Fatalf("ssh rule must decline with zero stdout, got %q", stdout.String())
	}
}

func TestRunGitCredentialMalformedRequest(t *testing.T) {
	city := t.TempDir()
	clearGitCredEnv(t, city)
	var stdout, stderr strings.Builder
	in := strings.NewReader("this-is-not-a-key-value-line\n")
	if err := runGitCredential("get", in, &stdout, &stderr); err == nil {
		t.Fatalf("expected error for malformed request")
	}
}

func TestRunGitCredentialCommandLayer(t *testing.T) {
	city := t.TempDir()
	clearGitCredEnv(t, city)
	t.Setenv(gitcred.EnvCredentialCommand, "printf 'username=bot\\npassword=cmdtok\\n'")

	var stdout, stderr strings.Builder
	in := strings.NewReader("protocol=https\nhost=github.com\npath=org/repo\n\n")
	if err := runGitCredential("get", in, &stdout, &stderr); err != nil {
		t.Fatalf("runGitCredential: %v", err)
	}
	if !strings.Contains(stdout.String(), "password=cmdtok") {
		t.Fatalf("command layer should emit credential, got %q", stdout.String())
	}
}
