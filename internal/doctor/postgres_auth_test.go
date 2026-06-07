package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pgauth"
)

// scrubAmbientPostgresEnv ensures resolver tier 3/5 (process env) cannot
// leak into a test that means to exercise tier 4 (scope file).
func scrubAmbientPostgresEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GC_POSTGRES_PASSWORD", "BEADS_POSTGRES_PASSWORD", "BEADS_CREDENTIALS_FILE"} {
		t.Setenv(key, "")
	}
	// Sandbox HOME so tier 7 (~/.config/beads/credentials) cannot match.
	t.Setenv("HOME", t.TempDir())
}

func writePGMetadata(t *testing.T, scopeRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads"}`
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePGScopeEnv(t *testing.T, scopeRoot string) {
	t.Helper()
	envFile := filepath.Join(scopeRoot, ".beads", ".env")
	if err := os.WriteFile(envFile, []byte("BEADS_POSTGRES_PASSWORD=devpw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresAuthCheck_StatusOK_ScopeFile exercises §3.3.1 — resolver
// returns scope_file as the source.
func TestPostgresAuthCheck_StatusOK_ScopeFile(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pwu")
	writePGMetadata(t, rigPath)
	writePGScopeEnv(t, rigPath)

	cfg := &config.City{Rigs: []config.Rig{{Name: "pwu", Path: "rigs/pwu"}}}
	check := NewPostgresAuthCheck(cityPath, cfg)
	r := check.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %v; want StatusOK; message=%q", r.Status, r.Message)
	}
	if want := "rigs/pwu (db.example.test:5432): password from scope file"; r.Message != want {
		t.Fatalf("message = %q; want %q", r.Message, want)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q; want empty for OK", r.FixHint)
	}
}

// TestPostgresAuthCheck_StatusWarning_ParentShellEnv exercises §3.3.2 —
// resolver returns process_env_beads (tier 5) as the source.
func TestPostgresAuthCheck_StatusWarning_ParentShellEnv(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	t.Setenv("BEADS_POSTGRES_PASSWORD", "shellpw")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pwu")
	writePGMetadata(t, rigPath)

	cfg := &config.City{Rigs: []config.Rig{{Name: "pwu", Path: "rigs/pwu"}}}
	check := NewPostgresAuthCheck(cityPath, cfg)
	r := check.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %v; want StatusWarning; message=%q", r.Status, r.Message)
	}
	if want := "rigs/pwu (db.example.test:5432): password from parent shell env"; r.Message != want {
		t.Fatalf("message = %q; want %q", r.Message, want)
	}
	wantHint := "parent-shell env works for the current shell only. Persist via rigs/pwu/.beads/.env (chmod 600) for non-interactive use."
	if r.FixHint != wantHint {
		t.Fatalf("FixHint = %q\n want %q", r.FixHint, wantHint)
	}
}

// TestPostgresAuthCheck_StatusError_NoCredentials exercises §3.3.3 —
// resolver exhausted with no value at any tier.
func TestPostgresAuthCheck_StatusError_NoCredentials(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pwu")
	writePGMetadata(t, rigPath)

	cfg := &config.City{Rigs: []config.Rig{{Name: "pwu", Path: "rigs/pwu"}}}
	check := NewPostgresAuthCheck(cityPath, cfg)
	r := check.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusError {
		t.Fatalf("status = %v; want StatusError; message=%q", r.Status, r.Message)
	}
	if want := "rigs/pwu (db.example.test:5432): no password resolvable"; r.Message != want {
		t.Fatalf("message = %q; want %q", r.Message, want)
	}
	wantHintPrefix := "set BEADS_POSTGRES_PASSWORD in rigs/pwu/.beads/.env (chmod 600)"
	if !strings.HasPrefix(r.FixHint, wantHintPrefix) {
		t.Fatalf("FixHint = %q; want prefix %q", r.FixHint, wantHintPrefix)
	}
	if !strings.Contains(r.FixHint, "[db.example.test:5432]") {
		t.Fatalf("FixHint = %q; want host:port section reference", r.FixHint)
	}
}

// TestPostgresAuthCheck_StatusError_PermissiveMode exercises §3.3.4 —
// scope-file mode is group/other readable.
func TestPostgresAuthCheck_StatusError_PermissiveMode(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pwu")
	writePGMetadata(t, rigPath)
	envFile := filepath.Join(rigPath, ".beads", ".env")
	if err := os.WriteFile(envFile, []byte("BEADS_POSTGRES_PASSWORD=devpw\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Rigs: []config.Rig{{Name: "pwu", Path: "rigs/pwu"}}}
	check := NewPostgresAuthCheck(cityPath, cfg)
	r := check.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusError {
		t.Fatalf("status = %v; want StatusError; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "credentials file mode") {
		t.Fatalf("message = %q; want credentials-file-mode wording", r.Message)
	}
	if !strings.Contains(r.Message, "(group/other readable)") {
		t.Fatalf("message = %q; want (group/other readable) suffix", r.Message)
	}
	if !strings.HasPrefix(r.FixHint, "chmod 600 ") {
		t.Fatalf("FixHint = %q; want chmod 600 prefix", r.FixHint)
	}
}

// TestPostgresAuthCheck_NoScopes_ReturnsOK confirms the §3.5 branch
// where collectPostgresAuthScopes returns nothing (registration site
// gates this; the check's own no-scope branch is defensive).
func TestPostgresAuthCheck_NoScopes_ReturnsOK(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()

	check := NewPostgresAuthCheck(cityPath, &config.City{})
	r := check.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %v; want StatusOK", r.Status)
	}
	if r.Message != "no postgres-backed scopes" {
		t.Fatalf("message = %q; want \"no postgres-backed scopes\"", r.Message)
	}
}

// TestPostgresAuthCheck_MultipleScopes verifies §3.5 aggregation: max
// severity wins, summary line prepends the count.
func TestPostgresAuthCheck_MultipleScopes(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()

	// Rig A — OK (scope file).
	rigAPath := filepath.Join(cityPath, "rigs", "alpha")
	writePGMetadata(t, rigAPath)
	writePGScopeEnv(t, rigAPath)

	// Rig B — Error (no creds).
	rigBPath := filepath.Join(cityPath, "rigs", "beta")
	writePGMetadata(t, rigBPath)

	cfg := &config.City{Rigs: []config.Rig{
		{Name: "alpha", Path: "rigs/alpha"},
		{Name: "beta", Path: "rigs/beta"},
	}}
	check := NewPostgresAuthCheck(cityPath, cfg)
	r := check.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusError {
		t.Fatalf("status = %v; want StatusError (rig B failed)", r.Status)
	}
	if !strings.HasPrefix(r.Message, "2 postgres-backed scope(s); first issue: ") {
		t.Fatalf("message = %q; want count + first-issue prefix", r.Message)
	}
	if !strings.Contains(r.Message, "rigs/beta") {
		t.Fatalf("message = %q; want first-issue scope to be rig B", r.Message)
	}
}

// TestPostgresAuthCheck_CanFix_ReturnsFalse locks design §3.6.
func TestPostgresAuthCheck_CanFix_ReturnsFalse(t *testing.T) {
	check := NewPostgresAuthCheck(t.TempDir(), &config.City{})
	if check.CanFix() {
		t.Fatal("CanFix() = true; want false")
	}
}

// TestPostgresAuthCheck_RenderExtras_NoScopesPrintsHint locks §4.6.
func TestPostgresAuthCheck_RenderExtras_NoScopesPrintsHint(t *testing.T) {
	check := NewPostgresAuthCheck(t.TempDir(), &config.City{})
	var buf bytes.Buffer
	check.RenderExtras(&CheckContext{ExplainPostgresAuth: true}, &buf)
	got := buf.String()
	if !strings.Contains(got, "no postgres-backed scopes (this flag has no effect)") {
		t.Fatalf("RenderExtras (no scopes) output = %q; want §4.6 hint", got)
	}
}

// TestPostgresAuthCheck_RenderExtras_FlagOff exits early — no output.
func TestPostgresAuthCheck_RenderExtras_FlagOff(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pwu")
	writePGMetadata(t, rigPath)
	writePGScopeEnv(t, rigPath)

	cfg := &config.City{Rigs: []config.Rig{{Name: "pwu", Path: "rigs/pwu"}}}
	check := NewPostgresAuthCheck(cityPath, cfg)
	check.Run(&CheckContext{CityPath: cityPath})

	var buf bytes.Buffer
	check.RenderExtras(&CheckContext{ExplainPostgresAuth: false}, &buf)
	if buf.Len() != 0 {
		t.Fatalf("RenderExtras (flag off) wrote %d bytes; want zero", buf.Len())
	}
}

// TestPostgresAuthCheck_RenderExtras_ExplainTable_OK_TierFour confirms
// the table shape for a §3.3.1 success: header + seven tier rows +
// footer. The winning tier is 4 (scope file); tiers 5-7 are [skip].
func TestPostgresAuthCheck_RenderExtras_ExplainTable_OK_TierFour(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pwu")
	writePGMetadata(t, rigPath)
	writePGScopeEnv(t, rigPath)

	cfg := &config.City{Rigs: []config.Rig{{Name: "pwu", Path: "rigs/pwu"}}}
	check := NewPostgresAuthCheck(cityPath, cfg)
	check.Run(&CheckContext{CityPath: cityPath})

	var buf bytes.Buffer
	check.RenderExtras(&CheckContext{ExplainPostgresAuth: true}, &buf)
	out := buf.String()

	// Header.
	if !strings.Contains(out, "PG-backed scope: rigs/pwu (db.example.test:5432)") {
		t.Errorf("missing scope header in:\n%s", out)
	}

	// Tier 4 winner.
	if !strings.Contains(out, "[YES]  ← winner") {
		t.Errorf("missing [YES]  ← winner mark in:\n%s", out)
	}
	if !strings.Contains(out, "Tier 4 ") {
		t.Errorf("missing Tier 4 row in:\n%s", out)
	}

	// Tiers 5-7 skip.
	for _, want := range []string{"Tier 5 ", "Tier 6 ", "Tier 7 "} {
		if !strings.Contains(out, want+"") {
			t.Errorf("missing %s row in:\n%s", want, out)
		}
	}
	skipCount := strings.Count(out, "[skip]")
	if skipCount < 3 {
		t.Errorf("[skip] tokens = %d; want >=3 for tiers 5-7", skipCount)
	}

	// Footer.
	if !strings.Contains(out, "Source identifier: scope_file") {
		t.Errorf("missing footer Source identifier in:\n%s", out)
	}
	if !strings.Contains(out, "Source position: tier 4 of 7") {
		t.Errorf("missing footer Source position in:\n%s", out)
	}

	// Sanity: password value never rendered.
	if strings.Contains(out, "devpw") {
		t.Errorf("explain table leaked password value:\n%s", out)
	}
}

// TestPostgresAuthCheck_RenderExtras_NoCredsPrintsAllNoTokens covers
// the §4.6 / §4 footer for an exhausted resolver.
func TestPostgresAuthCheck_RenderExtras_NoCredsPrintsAllNoTokens(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "pwu")
	writePGMetadata(t, rigPath)

	cfg := &config.City{Rigs: []config.Rig{{Name: "pwu", Path: "rigs/pwu"}}}
	check := NewPostgresAuthCheck(cityPath, cfg)
	check.Run(&CheckContext{CityPath: cityPath})

	var buf bytes.Buffer
	check.RenderExtras(&CheckContext{ExplainPostgresAuth: true}, &buf)
	out := buf.String()
	if strings.Contains(out, "[YES]") {
		t.Errorf("no-creds case rendered [YES]:\n%s", out)
	}
	if strings.Contains(out, "[skip]") {
		t.Errorf("no-creds case rendered [skip] (should be all [no]):\n%s", out)
	}
	noCount := strings.Count(out, "[no]")
	if noCount < 7 {
		t.Errorf("[no] count = %d; want 7 for no-creds case:\n%s", noCount, out)
	}
	if !strings.Contains(out, "No password resolvable. See: gc doctor") {
		t.Errorf("missing no-creds footer in:\n%s", out)
	}
}

// TestHumanSourceLabel locks design §3.4 mappings by driving the
// production humanSourceLabel directly.
func TestHumanSourceLabel(t *testing.T) {
	cases := []struct {
		name string
		in   pgauth.Source
		want string
	}{
		{"projected_gc", pgauth.SourceProjectedGC, "projected env (GC_POSTGRES_PASSWORD)"},
		{"projected_beads", pgauth.SourceProjectedBeads, "projected env (BEADS_POSTGRES_PASSWORD)"},
		{"process_env_gc", pgauth.SourceProcessEnvGC, "parent shell env (GC_POSTGRES_PASSWORD)"},
		{"scope_file", pgauth.SourceScopeFile, "scope file"},
		{"process_env_beads", pgauth.SourceProcessEnvBeads, "parent shell env (BEADS_POSTGRES_PASSWORD)"},
		{"credentials_file_env", pgauth.SourceCredentialsFileEnv, "$BEADS_CREDENTIALS_FILE"},
		{"credentials_file_home", pgauth.SourceCredentialsFileHome, "~/.config/beads/credentials"},
		{"none_returns_empty", pgauth.SourceNone, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := humanSourceLabel(tc.in)
			if got != tc.want {
				t.Fatalf("humanSourceLabel(%v) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
