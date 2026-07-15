package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/gitcred"
)

func setupCredCity(t *testing.T) string {
	t.Helper()
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir .gc: %v", err)
	}
	// Point resolveImportRoot at the city via GC_CITY_PATH.
	t.Setenv("GC_CITY_PATH", city)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(gitcred.EnvCredentialsFile, "")
	t.Setenv(gitcred.EnvCredentialCommand, "")
	// Clear the ambient GitHub token env so the built-in github.com default rule
	// stays inert and these tests observe only their own configured rules.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	return city
}

func TestImportCredentialAddWrites0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are POSIX-only")
	}
	city := setupCredCity(t)
	var stdout, stderr strings.Builder
	rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com/gascity", Helper: "gh auth token"}, false, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("add rc=%d stderr=%q", rc, stderr.String())
	}
	path := filepath.Join(city, ".gc", "credentials.toml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(stdout.String(), "Added pack credential for \"github.com/gascity\" (helper)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestImportCredentialAddRoundTripLoads(t *testing.T) {
	city := setupCredCity(t)
	var stdout, stderr strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com/gascity", TokenEnv: "GC_TOK"}, false, &stdout, &stderr); rc != 0 {
		t.Fatalf("add rc=%d stderr=%q", rc, stderr.String())
	}
	rules, err := gitcred.Load(city)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := rules.MatchSource("https://github.com/gascity/repo")
	if !ok || got.TokenEnv != "GC_TOK" {
		t.Fatalf("round trip failed: %+v ok=%v", got, ok)
	}
}

func TestImportCredentialAddDuplicateRejected(t *testing.T) {
	setupCredCity(t)
	var out strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com/gascity", Helper: "gh auth token"}, false, &out, &out); rc != 0 {
		t.Fatalf("first add failed: %q", out.String())
	}
	var stderr strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com/gascity/", Helper: "gh auth token"}, false, &out, &stderr); rc == 0 {
		t.Fatalf("duplicate add should fail")
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestImportCredentialAddNoPointer(t *testing.T) {
	setupCredCity(t)
	var stderr strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com"}, false, discardBuf(), &stderr); rc == 0 {
		t.Fatalf("add with no pointer should fail")
	}
	if !strings.Contains(stderr.String(), "exactly one of") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestImportCredentialAddRejectsURLMatch(t *testing.T) {
	setupCredCity(t)
	var stderr strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "https://github.com/org", Helper: "x"}, false, discardBuf(), &stderr); rc == 0 {
		t.Fatalf("URL match should fail")
	}
	if !strings.Contains(stderr.String(), "not a URL") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestImportCredentialAddRejectsUserinfoMatch(t *testing.T) {
	setupCredCity(t)
	var stderr strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "user@github.com", Helper: "x"}, false, discardBuf(), &stderr); rc == 0 {
		t.Fatalf("userinfo match should fail")
	}
	if !strings.Contains(stderr.String(), "user info") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestImportCredentialAddRefusesLiteralToken(t *testing.T) {
	setupCredCity(t)
	var stderr strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com", Helper: "ghp_literaltoken"}, false, discardBuf(), &stderr); rc == 0 {
		t.Fatalf("literal token should be refused")
	}
	if !strings.Contains(stderr.String(), "refusing a literal token") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestImportCredentialAddGlobal(t *testing.T) {
	setupCredCity(t)
	home := os.Getenv("GC_HOME")
	var out strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com", Helper: "gh auth token"}, true, &out, &out); rc != 0 {
		t.Fatalf("global add failed: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "credentials.toml")); err != nil {
		t.Fatalf("global file not written: %v", err)
	}
}

func TestImportCredentialAddEnvFileRedirect(t *testing.T) {
	setupCredCity(t)
	explicit := filepath.Join(t.TempDir(), "explicit.toml")
	t.Setenv(gitcred.EnvCredentialsFile, explicit)
	var stdout, stderr strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com", Helper: "gh auth token"}, false, &stdout, &stderr); rc != 0 {
		t.Fatalf("add failed: %q", stderr.String())
	}
	if _, err := os.Stat(explicit); err != nil {
		t.Fatalf("explicit file not written: %v", err)
	}
	if !strings.Contains(stderr.String(), "writing $GC_GIT_CREDENTIALS_FILE") {
		t.Fatalf("expected redirect notice, got %q", stderr.String())
	}
}

func TestImportCredentialListExactOutput(t *testing.T) {
	city := setupCredCity(t)
	if err := os.WriteFile(filepath.Join(city, ".gc", "credentials.toml"),
		[]byte("[[credential]]\nmatch=\"github.com/gascity\"\ntoken_env=\"GC_TOK\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = os.Chmod(filepath.Join(city, ".gc", "credentials.toml"), 0o600)
	var stdout, stderr strings.Builder
	if rc := doImportCredentialList(&stdout, &stderr); rc != 0 {
		t.Fatalf("list rc stderr=%q", stderr.String())
	}
	line := strings.TrimSpace(stdout.String())
	fields := strings.Split(line, "\t")
	if len(fields) != 4 {
		t.Fatalf("want 4 tab-separated fields, got %q", line)
	}
	if fields[0] != "github.com/gascity" || fields[1] != "x-access-token" || fields[2] != "token_env=GC_TOK" {
		t.Fatalf("unexpected list line: %q", line)
	}
}

func TestImportCredentialListEmpty(t *testing.T) {
	setupCredCity(t)
	var stdout strings.Builder
	if rc := doImportCredentialList(&stdout, discardBuf()); rc != 0 {
		t.Fatalf("list rc")
	}
	if !strings.Contains(stdout.String(), "No pack credentials configured") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestImportCredentialListCommandLayer(t *testing.T) {
	setupCredCity(t)
	t.Setenv(gitcred.EnvCredentialCommand, "my-helper get")
	var stdout strings.Builder
	if rc := doImportCredentialList(&stdout, discardBuf()); rc != 0 {
		t.Fatalf("list rc")
	}
	if !strings.Contains(stdout.String(), "command=$GC_GIT_CREDENTIAL_COMMAND") {
		t.Fatalf("command layer not listed: %q", stdout.String())
	}
}

func TestImportCredentialRemove(t *testing.T) {
	city := setupCredCity(t)
	var out strings.Builder
	if rc := doImportCredentialAdd(gitcred.Rule{Match: "github.com/gascity", Helper: "gh auth token"}, false, &out, &out); rc != 0 {
		t.Fatalf("add failed: %q", out.String())
	}
	var stdout, stderr strings.Builder
	if rc := doImportCredentialRemove("github.com/gascity", false, &stdout, &stderr); rc != 0 {
		t.Fatalf("remove rc stderr=%q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Removed pack credential for \"github.com/gascity\"") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	rules, _ := gitcred.Load(city)
	if len(rules.All()) != 0 {
		t.Fatalf("rule not removed: %+v", rules.All())
	}
}

func TestImportCredentialRemoveNotFound(t *testing.T) {
	setupCredCity(t)
	var stderr strings.Builder
	if rc := doImportCredentialRemove("github.com/none", false, discardBuf(), &stderr); rc == 0 {
		t.Fatalf("remove of absent credential should fail")
	}
}

func TestImportCredentialRemoveIgnoresAmbientTokenOrigin(t *testing.T) {
	// The built-in ambient github.com default has a synthetic $GH_TOKEN origin,
	// not a file. Removing an absent github.com credential must fail with a plain
	// not-found message, never "found in $GH_TOKEN; ... edit that file".
	setupCredCity(t)
	t.Setenv("GH_TOKEN", "gh_ambient")
	var stderr strings.Builder
	if rc := doImportCredentialRemove("github.com", false, discardBuf(), &stderr); rc == 0 {
		t.Fatalf("remove of an ambient-only credential should fail")
	}
	msg := stderr.String()
	if strings.Contains(msg, "$GH_TOKEN") || strings.Contains(msg, "edit that file") {
		t.Fatalf("remove guidance must not point at the synthetic env origin, got %q", msg)
	}
	if !strings.Contains(msg, `no credential for "github.com"`) {
		t.Fatalf("expected a plain not-found message, got %q", msg)
	}
}

func discardBuf() *strings.Builder { return &strings.Builder{} }
