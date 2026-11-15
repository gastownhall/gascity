package gitcred

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// exitCodeEnv makes this test binary re-execute as a process that does nothing
// but exit with the requested code.
//
// The helpers below need a genuine *exec.ExitError, which cannot be fabricated
// -- only a real finished process produces one. Shelling out to `sh -c "exit N"`
// is the usual trick, but it makes an entire file of portable assertions about
// git's stderr depend on a POSIX shell. Re-executing this binary needs no shell
// on any platform.
const exitCodeEnv = "GC_TEST_GITCRED_EXIT_CODE"

func TestMain(m *testing.M) {
	if raw := os.Getenv(exitCodeEnv); raw != "" {
		code, err := strconv.Atoi(raw)
		if err != nil {
			os.Exit(125)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// exitError runs a child that exits with code and returns the resulting error.
func exitError(t *testing.T, code int) error {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("setup: locating test binary: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), exitCodeEnv+"="+strconv.Itoa(code))
	return cmd.Run()
}

// exit128 returns a real *exec.ExitError with code 128.
func exit128(t *testing.T) error {
	t.Helper()
	err := exitError(t, 128)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		t.Fatalf("setup: could not produce exit-128 error, got %v", err)
	}
	return err
}

func exit1(t *testing.T) error {
	t.Helper()
	err := exitError(t, 1)
	if err == nil {
		t.Fatalf("setup: expected exit-1 error")
	}
	return err
}

func TestClassifyAuthErrorTriggers(t *testing.T) {
	err128 := exit128(t)
	triggers := []string{
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled",
		"remote: Invalid username or password.",
		"fatal: Authentication failed for 'https://github.com/org/repo.git/'",
		"The requested URL returned error: 401",
		"The requested URL returned error: 403",
		"git@github.com: Permission denied (publickey).",
		"Host key verification failed.",
	}
	for _, out := range triggers {
		got := ClassifyAuthError("https://github.com/org/repo", Injection{}, out, err128)
		var authErr *AuthError
		if !errors.As(got, &authErr) {
			t.Errorf("expected AuthError for %q, got %v", out, got)
		}
	}
}

func TestClassifyAuthErrorExitGate(t *testing.T) {
	got := ClassifyAuthError("https://github.com/org/repo", Injection{},
		"fatal: Authentication failed for 'x'", exit1(t))
	if got != nil {
		t.Fatalf("exit != 128 must not be classified, got %v", got)
	}
}

func TestClassifyAuthErrorNonAuthNotClassified(t *testing.T) {
	got := ClassifyAuthError("https://github.com/org/repo", Injection{},
		"fatal: unable to access: Could not resolve host: github.com", exit128(t))
	if got != nil {
		t.Fatalf("non-auth 128 failure must not be classified, got %v", got)
	}
}

func TestClassifyRepositoryNotFoundMatchedOnly(t *testing.T) {
	out := "remote: Repository not found.\nfatal: repository 'https://github.com/org/repo/' not found"
	// Unmatched → not classified.
	if got := ClassifyAuthError("https://github.com/org/repo", Injection{Matched: false}, out, exit128(t)); got != nil {
		t.Fatalf("Repository not found unmatched must not be classified, got %v", got)
	}
	// Matched → classified.
	got := ClassifyAuthError("https://github.com/org/repo", Injection{Matched: true}, out, exit128(t))
	var authErr *AuthError
	if !errors.As(got, &authErr) {
		t.Fatalf("Repository not found matched must be classified, got %v", got)
	}
}

func TestAuthErrorOrgPrefixAndStrings(t *testing.T) {
	err128 := exit128(t)
	out := "fatal: could not read Username for 'https://github.com': terminal prompts disabled"

	unmatched := ClassifyAuthError("https://github.com/gascity/gas-city-inc", Injection{}, out, err128)
	var u *AuthError
	if !errors.As(unmatched, &u) {
		t.Fatalf("expected AuthError")
	}
	if u.OrgPrefix != "github.com/gascity" {
		t.Fatalf("OrgPrefix = %q, want github.com/gascity", u.OrgPrefix)
	}
	if !strings.Contains(u.Error(), "no credential rule matches") {
		t.Fatalf("unmatched Error() missing phrasing: %q", u.Error())
	}

	matched := ClassifyAuthError("https://github.com/gascity/repo",
		Injection{Matched: true, RuleOrigin: "/home/u/city/.gc/credentials.toml"}, out, err128)
	var m *AuthError
	if !errors.As(matched, &m) {
		t.Fatalf("expected AuthError")
	}
	if !strings.Contains(m.Error(), "/home/u/city/.gc/credentials.toml was rejected") {
		t.Fatalf("matched Error() missing origin: %q", m.Error())
	}
}

func TestAuthErrorUnwrap(t *testing.T) {
	err128 := exit128(t)
	got := ClassifyAuthError("https://github.com/org/repo", Injection{},
		"fatal: Authentication failed for 'x'", err128)
	if !errors.Is(got, err128) {
		t.Fatalf("Unwrap chain broken: errors.Is could not find the underlying error")
	}
}

func TestAuthErrorRedactsUserinfo(t *testing.T) {
	got := ClassifyAuthError("https://user:ghp_secret@github.com/org/repo", Injection{},
		"fatal: Authentication failed for 'x'", exit128(t))
	var authErr *AuthError
	if !errors.As(got, &authErr) {
		t.Fatalf("expected AuthError")
	}
	if strings.Contains(authErr.Error(), "ghp_secret") {
		t.Fatalf("Error() leaked userinfo secret: %q", authErr.Error())
	}
}
