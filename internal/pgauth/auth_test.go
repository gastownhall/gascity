package pgauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// clearProcessEnv zeroes the process-env tiers so each test starts from a
// known empty state. Tests opt into a non-empty value by re-setting the
// specific env they exercise.
func clearProcessEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_POSTGRES_PASSWORD", "")
	t.Setenv("BEADS_POSTGRES_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")
	// Redirect HOME to a temp dir so tier 7 cannot leak the operator's
	// real ~/.config/beads/credentials into a test that intends to fall
	// through to SourceNone.
	t.Setenv("HOME", t.TempDir())
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", t.TempDir())
	}
}

func writeStorePassword(t *testing.T, scopeRoot, password string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	path := filepath.Join(scopeRoot, ".beads", ".env")
	if err := os.WriteFile(path, []byte("BEADS_POSTGRES_PASSWORD="+password+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(.env): %v", err)
	}
}

func writeStorePasswordWithMode(t *testing.T, scopeRoot, password string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	path := filepath.Join(scopeRoot, ".beads", ".env")
	if err := os.WriteFile(path, []byte("BEADS_POSTGRES_PASSWORD="+password+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(.env): %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(.env, %o): %v", mode, err)
	}
	return path
}

func writeCredentialsFile(t *testing.T, host, port, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials")
	contents := fmt.Sprintf("[%s:%s]\npassword = %s\n", host, port, password)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile(credentials): %v", err)
	}
	return path
}

func writeCredentialsFileRaw(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile(credentials): %v", err)
	}
	return path
}

func writeCredentialsFileWithMode(t *testing.T, host, port, password string, mode os.FileMode) string {
	t.Helper()
	path := writeCredentialsFile(t, host, port, password)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(credentials, %o): %v", mode, err)
	}
	return path
}

// pinHome sets HOME to a tmp dir and (optionally) seeds
// ~/.config/beads/credentials with the given content. When content is
// empty no file is written.
func pinHome(t *testing.T, content string) {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	if content == "" {
		return
	}
	credPath := filepath.Join(homeDir, ".config", "beads", "credentials")
	if err := os.MkdirAll(filepath.Dir(credPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(.config/beads): %v", err)
	}
	if err := os.WriteFile(credPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(credentials): %v", err)
	}
}

func endpoint() Endpoint {
	return Endpoint{Host: "127.0.0.1", Port: "5433", User: "bd"}
}

func TestResolveFromEnv_Tier1_ProjectedGC(t *testing.T) {
	clearProcessEnv(t)
	envMap := map[string]string{
		"GC_POSTGRES_PASSWORD":    "tier1",
		"BEADS_POSTGRES_PASSWORD": "tier2-loses",
	}
	t.Setenv("GC_POSTGRES_PASSWORD", "tier3-loses")

	got, err := ResolveFromEnv(envMap, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "tier1" {
		t.Fatalf("Password = %q, want tier1", got.Password)
	}
	if got.Source != SourceProjectedGC {
		t.Fatalf("Source = %v, want SourceProjectedGC", got.Source)
	}
	if got.User != "bd" {
		t.Fatalf("User = %q, want bd", got.User)
	}
}

func TestResolveFromEnv_Tier2_ProjectedBeads(t *testing.T) {
	clearProcessEnv(t)
	envMap := map[string]string{
		"BEADS_POSTGRES_PASSWORD": "tier2",
	}
	t.Setenv("GC_POSTGRES_PASSWORD", "tier3-loses")

	got, err := ResolveFromEnv(envMap, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "tier2" {
		t.Fatalf("Password = %q, want tier2", got.Password)
	}
	if got.Source != SourceProjectedBeads {
		t.Fatalf("Source = %v, want SourceProjectedBeads", got.Source)
	}
}

func TestResolveFromEnv_Tier3_ProcessEnvGC(t *testing.T) {
	clearProcessEnv(t)
	t.Setenv("GC_POSTGRES_PASSWORD", "tier3")

	scopeRoot := t.TempDir()
	writeStorePassword(t, scopeRoot, "tier4-loses")

	got, err := ResolveFromEnv(nil, scopeRoot, endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "tier3" {
		t.Fatalf("Password = %q, want tier3", got.Password)
	}
	if got.Source != SourceProcessEnvGC {
		t.Fatalf("Source = %v, want SourceProcessEnvGC", got.Source)
	}
}

func TestResolveFromEnv_Tier4_ScopeFile(t *testing.T) {
	clearProcessEnv(t)
	scopeRoot := t.TempDir()
	writeStorePassword(t, scopeRoot, "tier4")
	t.Setenv("BEADS_POSTGRES_PASSWORD", "tier5-loses")

	got, err := ResolveFromEnv(nil, scopeRoot, endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "tier4" {
		t.Fatalf("Password = %q, want tier4", got.Password)
	}
	if got.Source != SourceScopeFile {
		t.Fatalf("Source = %v, want SourceScopeFile", got.Source)
	}
}

func TestResolveFromEnv_Tier5_ProcessEnvBeads(t *testing.T) {
	clearProcessEnv(t)
	t.Setenv("BEADS_POSTGRES_PASSWORD", "tier5")
	credPath := writeCredentialsFile(t, "127.0.0.1", "5433", "tier6-loses")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "tier5" {
		t.Fatalf("Password = %q, want tier5", got.Password)
	}
	if got.Source != SourceProcessEnvBeads {
		t.Fatalf("Source = %v, want SourceProcessEnvBeads", got.Source)
	}
}

func TestResolveFromEnv_ScopeFileScanErrorStopsFallback(t *testing.T) {
	clearProcessEnv(t)
	scopeRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	envPath := filepath.Join(scopeRoot, ".beads", ".env")
	if err := os.WriteFile(envPath, []byte(strings.Repeat("x", 70*1024)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(.env): %v", err)
	}
	t.Setenv("BEADS_POSTGRES_PASSWORD", "tier5-must-not-win")

	_, err := ResolveFromEnv(nil, scopeRoot, endpoint())
	if err == nil {
		t.Fatal("ResolveFromEnv error = nil, want scope file scan error")
	}
	if !strings.Contains(err.Error(), envPath) {
		t.Fatalf("error = %q, want scope env path %q", err.Error(), envPath)
	}
}

func TestResolveFromEnv_Tier6_CredentialsFileEnv(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFile(t, "127.0.0.1", "5433", "tier6")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "tier6" {
		t.Fatalf("Password = %q, want tier6", got.Password)
	}
	if got.Source != SourceCredentialsFileEnv {
		t.Fatalf("Source = %v, want SourceCredentialsFileEnv", got.Source)
	}
}

func TestResolveFromEnv_CredentialsFileScanErrorStopsFallback(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[127.0.0.1:5433]\n"+strings.Repeat("x", 70*1024)+"\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)
	pinHome(t, "[127.0.0.1:5433]\npassword = home-must-not-win\n")

	_, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err == nil {
		t.Fatal("ResolveFromEnv error = nil, want credentials file scan error")
	}
	if !strings.Contains(err.Error(), credPath) {
		t.Fatalf("error = %q, want credentials path %q", err.Error(), credPath)
	}
}

func TestResolveFromEnv_Tier7_CredentialsFileHome(t *testing.T) {
	clearProcessEnv(t)
	pinHome(t, "[127.0.0.1:5433]\npassword = tier7\n")

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "tier7" {
		t.Fatalf("Password = %q, want tier7", got.Password)
	}
	if got.Source != SourceCredentialsFileHome {
		t.Fatalf("Source = %v, want SourceCredentialsFileHome", got.Source)
	}
}

func TestResolveFromEnv_AllEmptyReturnsErrNoPassword(t *testing.T) {
	clearProcessEnv(t)

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err == nil {
		t.Fatalf("ResolveFromEnv = %+v, want error", got)
	}
	if !errors.Is(err, ErrNoPasswordResolvable) {
		t.Fatalf("err = %v, want errors.Is ErrNoPasswordResolvable", err)
	}
	if got.Source != SourceNone {
		t.Fatalf("Source = %v, want SourceNone", got.Source)
	}
	if got.Password != "" {
		t.Fatalf("Password = %q, want empty", got.Password)
	}
	want := "no postgres password resolvable for bd@127.0.0.1:5433: no postgres password resolvable"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func TestResolveFromEnv_NilEnvSkipsTiers1And2(t *testing.T) {
	clearProcessEnv(t)
	t.Setenv("GC_POSTGRES_PASSWORD", "tier3-wins-when-envmap-nil")

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Source != SourceProcessEnvGC {
		t.Fatalf("Source = %v, want SourceProcessEnvGC", got.Source)
	}
	if got.Password != "tier3-wins-when-envmap-nil" {
		t.Fatalf("Password = %q, want tier3-wins-when-envmap-nil", got.Password)
	}
}

func TestResolveFromEnv_PermissiveScopeFileStopsChain(t *testing.T) {
	clearProcessEnv(t)
	scopeRoot := t.TempDir()
	envPath := writeStorePasswordWithMode(t, scopeRoot, "tier4-blocked", 0o644)
	// Tier 5 has a value that would win if the chain fell through.
	t.Setenv("BEADS_POSTGRES_PASSWORD", "tier5-must-not-win")

	_, err := ResolveFromEnv(nil, scopeRoot, endpoint())
	if err == nil {
		t.Fatalf("expected *PermissivePermissionError, got nil")
	}
	var perm *PermissivePermissionError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want *PermissivePermissionError", err)
	}
	if perm.Path != envPath {
		t.Fatalf("Path = %q, want %q", perm.Path, envPath)
	}
	if perm.Mode.Perm() != 0o644 {
		t.Fatalf("Mode = %o, want 0644", perm.Mode.Perm())
	}
	want := fmt.Sprintf("credentials file %s has mode 0644; refuse to read (require 0600 or 0400; owner-executable modes such as 0700 are rejected)", envPath)
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func TestResolveFromEnv_OwnerExecutableScopeFileExplainsRejection(t *testing.T) {
	clearProcessEnv(t)
	scopeRoot := t.TempDir()
	writeStorePasswordWithMode(t, scopeRoot, "tier4-blocked", 0o700)

	_, err := ResolveFromEnv(nil, scopeRoot, endpoint())
	if err == nil {
		t.Fatalf("expected *PermissivePermissionError, got nil")
	}
	var perm *PermissivePermissionError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want *PermissivePermissionError", err)
	}
	if perm.Mode.Perm() != 0o700 {
		t.Fatalf("Mode = %o, want 0700", perm.Mode.Perm())
	}
	if !strings.Contains(err.Error(), "owner-executable modes such as 0700 are rejected") {
		t.Fatalf("err = %q, want owner-executable rejection detail", err.Error())
	}
}

func TestResolveFromEnv_PermissiveCredentialsFileStopsChain(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileWithMode(t, "127.0.0.1", "5433", "tier6-blocked", 0o604)
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)
	// Tier 7 would otherwise be reachable.
	pinHome(t, "[127.0.0.1:5433]\npassword = tier7-must-not-win\n")

	_, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err == nil {
		t.Fatalf("expected *PermissivePermissionError, got nil")
	}
	var perm *PermissivePermissionError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want *PermissivePermissionError", err)
	}
	if perm.Path != credPath {
		t.Fatalf("Path = %q, want %q", perm.Path, credPath)
	}
	if perm.Mode.Perm() != 0o604 {
		t.Fatalf("Mode = %o, want 0604", perm.Mode.Perm())
	}
}

func TestResolveFromEnv_MalformedCredentialsFileStopsChain(t *testing.T) {
	clearProcessEnv(t)
	// Line 3 is malformed: section header missing closing bracket.
	contents := "# header\n\n[127.0.0.1:5433\npassword = oops\n"
	credPath := writeCredentialsFileRaw(t, contents)
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)
	pinHome(t, "[127.0.0.1:5433]\npassword = tier7-must-not-win\n")

	_, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err == nil {
		t.Fatalf("expected *CredentialsParseError, got nil")
	}
	var pe *CredentialsParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *CredentialsParseError", err)
	}
	if pe.Path != credPath {
		t.Fatalf("Path = %q, want %q", pe.Path, credPath)
	}
	if pe.Line != 3 {
		t.Fatalf("Line = %d, want 3", pe.Line)
	}
	if pe.Reason != "unterminated section header (expected ']')" {
		t.Fatalf("Reason = %q, want unterminated section header (expected ']')", pe.Reason)
	}
	want := fmt.Sprintf("parse credentials file %s at line 3: unterminated section header (expected ']')", credPath)
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func TestResolveFromEnv_MalformedCredentialsFile_EmptyHeader(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[]\npassword = oops\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	_, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	var pe *CredentialsParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *CredentialsParseError", err)
	}
	if pe.Line != 1 {
		t.Fatalf("Line = %d, want 1", pe.Line)
	}
	if pe.Reason != "empty section header" {
		t.Fatalf("Reason = %q, want empty section header", pe.Reason)
	}
}

func TestResolveFromEnv_MalformedCredentialsFile_MissingEquals(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[127.0.0.1:5433]\npassword without equals\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	_, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	var pe *CredentialsParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *CredentialsParseError", err)
	}
	if pe.Line != 2 {
		t.Fatalf("Line = %d, want 2", pe.Line)
	}
	if pe.Reason != "missing '=' in key/value line" {
		t.Fatalf("Reason = %q, want missing '=' in key/value line", pe.Reason)
	}
}

func TestResolveFromEnv_CredentialsFileRejectsDuplicateMatchingSection(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[127.0.0.1:5433]\npassword = first\n[other.example.com:5432]\npassword = ignored\n[127.0.0.1:5433]\npassword = second\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	_, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err == nil {
		t.Fatal("ResolveFromEnv error = nil, want duplicate section parse error")
	}
	var pe *CredentialsParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *CredentialsParseError", err)
	}
	if pe.Line != 5 {
		t.Fatalf("Line = %d, want 5", pe.Line)
	}
	if pe.Reason != "duplicate credentials section for 127.0.0.1:5433" {
		t.Fatalf("Reason = %q, want duplicate section reason", pe.Reason)
	}
}

func TestResolveFromEnv_CredentialsFileRejectsDuplicatePasswordKey(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[127.0.0.1:5433]\npassword = first\npassword = second\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	_, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err == nil {
		t.Fatal("ResolveFromEnv error = nil, want duplicate password key parse error")
	}
	var pe *CredentialsParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *CredentialsParseError", err)
	}
	if pe.Line != 3 {
		t.Fatalf("Line = %d, want 3", pe.Line)
	}
	if pe.Reason != "duplicate password key in credentials section for 127.0.0.1:5433" {
		t.Fatalf("Reason = %q, want duplicate password reason", pe.Reason)
	}
}

func TestResolveFromEnv_AbsentScopeFileFallsThrough(t *testing.T) {
	clearProcessEnv(t)
	t.Setenv("BEADS_POSTGRES_PASSWORD", "tier5-wins")

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Source != SourceProcessEnvBeads {
		t.Fatalf("Source = %v, want SourceProcessEnvBeads", got.Source)
	}
	if got.Password != "tier5-wins" {
		t.Fatalf("Password = %q, want tier5-wins", got.Password)
	}
}

func TestResolveFromEnv_NonMatchingSectionFallsThroughTier6(t *testing.T) {
	clearProcessEnv(t)
	// Tier 6 has only a section for a different host:port.
	credPath := writeCredentialsFile(t, "other.example.com", "5432", "tier6-not-matching")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)
	// Tier 7 has the matching section.
	pinHome(t, "[127.0.0.1:5433]\npassword = tier7-matches\n")

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Source != SourceCredentialsFileHome {
		t.Fatalf("Source = %v, want SourceCredentialsFileHome", got.Source)
	}
	if got.Password != "tier7-matches" {
		t.Fatalf("Password = %q, want tier7-matches", got.Password)
	}
}

func TestResolveFromEnv_WhitespaceValueIsEmpty(t *testing.T) {
	clearProcessEnv(t)
	envMap := map[string]string{
		"GC_POSTGRES_PASSWORD": "   ",
	}
	t.Setenv("BEADS_POSTGRES_PASSWORD", "tier5-wins")

	got, err := ResolveFromEnv(envMap, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Source != SourceProcessEnvBeads {
		t.Fatalf("Source = %v, want SourceProcessEnvBeads (whitespace tier1 should be skipped)", got.Source)
	}
}

func TestResolveFromEnv_QuotedPasswordInCredentialsFile(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[127.0.0.1:5433]\npassword = \"p&ssword!\"\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "p&ssword!" {
		t.Fatalf("Password = %q, want p&ssword!", got.Password)
	}
}

func TestResolveFromEnv_CredentialsFilePreservesHashInPassword(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[127.0.0.1:5433]\npassword = s3cr3t#1\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "s3cr3t#1" {
		t.Fatalf("Password = %q, want s3cr3t#1", got.Password)
	}
}

func TestResolveFromEnv_CredentialsFilePreservesHashInQuotedPassword(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[127.0.0.1:5433]\npassword = \"p#ss\"\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "p#ss" {
		t.Fatalf("Password = %q, want p#ss", got.Password)
	}
}

func TestResolveFromEnv_UnknownKeyInSectionIsIgnored(t *testing.T) {
	clearProcessEnv(t)
	credPath := writeCredentialsFileRaw(t, "[127.0.0.1:5433]\nfoo = bar\npassword = ok\n")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "ok" {
		t.Fatalf("Password = %q, want ok", got.Password)
	}
	if got.Source != SourceCredentialsFileEnv {
		t.Fatalf("Source = %v, want SourceCredentialsFileEnv", got.Source)
	}
}

func TestResolveFromEnv_EndpointFieldsEchoedInError(t *testing.T) {
	clearProcessEnv(t)
	ep := Endpoint{Host: "", Port: "", User: ""}

	_, err := ResolveFromEnv(nil, t.TempDir(), ep)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	want := "no postgres password resolvable for @:: no postgres password resolvable"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q (empty endpoint fields must remain visible)", err.Error(), want)
	}
}

func TestSourceString_AllConstantsHaveStableOutput(t *testing.T) {
	cases := []struct {
		s    Source
		want string
	}{
		{SourceNone, "none"},
		{SourceProjectedGC, "projected_gc"},
		{SourceProjectedBeads, "projected_beads"},
		{SourceProcessEnvGC, "process_env_gc"},
		{SourceScopeFile, "scope_file"},
		{SourceProcessEnvBeads, "process_env_beads"},
		{SourceCredentialsFileEnv, "credentials_file_env"},
		{SourceCredentialsFileHome, "credentials_file_home"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("Source(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

func TestSourceString_OutOfRangeFallsBackToNone(t *testing.T) {
	// Defends the documented invariant that String() never panics on an
	// unknown int value. Any future widening that introduces an extra
	// constant without updating String() will surface here.
	if got := Source(99).String(); got != "none" {
		t.Fatalf("Source(99).String() = %q, want none", got)
	}
}

func TestSource_StableEnumOrdering(t *testing.T) {
	// Asserts the iota positions match the resolution-chain order. If a
	// future change reorders constants, slice-4 event payloads (which
	// store int-valued Source) silently break.
	want := map[Source]int{
		SourceNone:                0,
		SourceProjectedGC:         1,
		SourceProjectedBeads:      2,
		SourceProcessEnvGC:        3,
		SourceScopeFile:           4,
		SourceProcessEnvBeads:     5,
		SourceCredentialsFileEnv:  6,
		SourceCredentialsFileHome: 7,
	}
	for s, n := range want {
		if int(s) != n {
			t.Errorf("Source %s = %d, want %d", s, int(s), n)
		}
	}
}

func TestReadStoreLocalPassword_ReturnsValueOnHappyPath(t *testing.T) {
	clearProcessEnv(t)
	scopeRoot := t.TempDir()
	writeStorePassword(t, scopeRoot, "store-secret")

	got, err := ReadStoreLocalPassword(scopeRoot)
	if err != nil {
		t.Fatalf("ReadStoreLocalPassword error: %v", err)
	}
	if got != "store-secret" {
		t.Fatalf("got = %q, want store-secret", got)
	}
}

func TestReadStoreLocalPassword_PermissiveModeReturnsTypedError(t *testing.T) {
	clearProcessEnv(t)
	scopeRoot := t.TempDir()
	envPath := writeStorePasswordWithMode(t, scopeRoot, "store-secret", 0o644)

	got, err := ReadStoreLocalPassword(scopeRoot)
	if got != "" {
		t.Fatalf("got = %q, want empty on permissive mode", got)
	}
	var perm *PermissivePermissionError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want *PermissivePermissionError", err)
	}
	if perm.Path != envPath {
		t.Fatalf("Path = %q, want %q", perm.Path, envPath)
	}
	if perm.Mode.Perm() != 0o644 {
		t.Fatalf("Mode = %o, want 0644", perm.Mode.Perm())
	}
}

func TestReadStoreLocalPassword_AbsentFileReturnsEmptyNoError(t *testing.T) {
	clearProcessEnv(t)
	scopeRoot := t.TempDir() // no .beads/.env created

	got, err := ReadStoreLocalPassword(scopeRoot)
	if err != nil {
		t.Fatalf("ReadStoreLocalPassword error: %v", err)
	}
	if got != "" {
		t.Fatalf("got = %q, want empty", got)
	}
}

func TestReadStoreLocalPassword_EmptyScopeRootReturnsEmptyNoError(t *testing.T) {
	clearProcessEnv(t)

	got, err := ReadStoreLocalPassword("")
	if err != nil {
		t.Fatalf("ReadStoreLocalPassword error: %v", err)
	}
	if got != "" {
		t.Fatalf("got = %q, want empty", got)
	}
}

func TestDefaultCredentialsPath_PointsUnderHomeOrAppData(t *testing.T) {
	clearProcessEnv(t)
	got := DefaultCredentialsPath()
	if got == "" {
		t.Skip("no home/appdata available on this platform; skipping")
	}
	wantSuffix := filepath.Join("beads", "credentials")
	if !pathHasSuffix(got, wantSuffix) {
		t.Fatalf("DefaultCredentialsPath = %q, want suffix %q", got, wantSuffix)
	}
}

// pathHasSuffix returns true when p ends with suffix (uses filepath
// semantics so test passes on Windows and POSIX).
func pathHasSuffix(p, suffix string) bool {
	cleanP := filepath.Clean(p)
	cleanS := filepath.Clean(suffix)
	if len(cleanP) < len(cleanS) {
		return false
	}
	return cleanP[len(cleanP)-len(cleanS):] == cleanS
}
