package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/gitcred"
)

// installFakeGit puts a fake `git` executable at the front of PATH for the test.
// The script records its full argv to a file under a temp dir and then behaves
// per mode: "authfail" prints a canned auth-failure stderr and exits 128; any
// other mode records argv and exits 0. Returns the temp dir holding the recorded
// argv.
func installFakeGit(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + argvFile + "'\n"
	switch mode {
	case "authfail":
		script += "echo \"fatal: could not read Username for 'https://github.com': terminal prompts disabled\" 1>&2\n" +
			"exit 128\n"
	default:
		script += "exit 0\n"
	}
	gitPath := filepath.Join(dir, "git")
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func writeCityCredRule(t *testing.T, city, body string) {
	t.Helper()
	gcDir := filepath.Join(city, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(gcDir, "credentials.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write cred rule: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

func TestImportHeadCommitClassifiesAuthFailure(t *testing.T) {
	installFakeGit(t, "authfail")
	city := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(gitcred.EnvCredentialsFile, "")
	t.Setenv(gitcred.EnvCredentialCommand, "")

	// Unmatched: no rule → still classified (auth required), Matched=false.
	_, err := defaultImportHeadCommit(city, "https://github.com/gascity/gas-city-inc")
	var authErr *gitcred.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *gitcred.AuthError, got %v", err)
	}
	if authErr.Matched {
		t.Fatalf("expected unmatched auth error")
	}
	if authErr.OrgPrefix != "github.com/gascity" {
		t.Fatalf("OrgPrefix = %q", authErr.OrgPrefix)
	}
}

func TestImportHeadCommitClassifiesMatchedAuthFailure(t *testing.T) {
	installFakeGit(t, "authfail")
	city := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(gitcred.EnvCredentialsFile, "")
	t.Setenv(gitcred.EnvCredentialCommand, "")
	writeCityCredRule(t, city, "[[credential]]\nmatch=\"github.com/gascity\"\nhelper=\"echo tok\"\n")

	_, err := defaultImportHeadCommit(city, "https://github.com/gascity/gas-city-inc")
	var authErr *gitcred.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *gitcred.AuthError, got %v", err)
	}
	if !authErr.Matched {
		t.Fatalf("expected matched auth error")
	}
	if !strings.Contains(authErr.RuleOrigin, "credentials.toml") {
		t.Fatalf("RuleOrigin = %q, want the city rules file", authErr.RuleOrigin)
	}
}

func TestImportHeadCommitInjectsCredentialHelperArgv(t *testing.T) {
	fakeDir := installFakeGit(t, "ok")
	city := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(gitcred.EnvCredentialsFile, "")
	t.Setenv(gitcred.EnvCredentialCommand, "")
	writeCityCredRule(t, city, "[[credential]]\nmatch=\"github.com/gascity\"\nhelper=\"printf 'ghp_roundtrip\\\\n'\"\n")

	// The "ok" fake git records its argv and exits 0. defaultImportHeadCommit
	// returns an empty-response error (the fake prints no refs), which is fine —
	// we only assert the injected credential-helper wiring reached git.
	_, _ = defaultImportHeadCommit(city, "https://github.com/gascity/gas-city-inc")

	argv, err := os.ReadFile(filepath.Join(fakeDir, "argv"))
	if err != nil {
		t.Fatalf("fake git argv not recorded: %v", err)
	}
	if !strings.Contains(string(argv), "credential.helper=") {
		t.Fatalf("injected git argv missing credential.helper: %q", string(argv))
	}
	if !strings.Contains(string(argv), "credential.useHttpPath=true") {
		t.Fatalf("injected git argv missing useHttpPath: %q", string(argv))
	}
}
