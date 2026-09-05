package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCredentialsTestEnv points GC_HOME at a temp dir and clears the variables
// the report inspects, so an operator's real shell cannot change the result.
func newCredentialsTestEnv(t *testing.T) (gcHome string) {
	t.Helper()
	gcHome = t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_SUPERVISOR_SYSTEMD_UNIT", "")
	t.Setenv("GC_SUPERVISOR_ENV", "")
	// shouldPersistSupervisorEnv reads this, so leaving it exported in the
	// ambient environment flips the forwarding warning and fails the
	// forwarded-name test for a reason that has nothing to do with the code.
	t.Setenv("GC_SUPERVISOR_OMIT_PROVIDER_CREDS", "")
	t.Setenv("ACME_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN_ZAI", "")
	t.Setenv("ZAI_BASE_URL", "")
	return gcHome
}

// TestProviderCredentialsReportsCredentialSourceOnly goes through run(), so it
// covers the path the operator actually takes.
//
// The source variable is ACME_KEY on purpose: a name matching no credential
// prefix the supervisor recognizes. A name like ANTHROPIC_AUTH_TOKEN_ZAI
// clears the supervisor's persist gate, so a test using one cannot tell a
// report that checks forwarding from one that does not.
func TestProviderCredentialsReportsCredentialSourceOnly(t *testing.T) {
	newCredentialsTestEnv(t)
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
base = "builtin:claude"
env = {ANTHROPIC_BASE_URL = "${ZAI_BASE_URL}", ANTHROPIC_AUTH_TOKEN = "${ACME_KEY}", ANTHROPIC_API_KEY = "${ACME_KEY}"}
`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "zai"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()

	if !strings.Contains(out, "ACME_KEY") {
		t.Errorf("output does not name the credential's source variable:\n%s", out)
	}
	if strings.Contains(out, "ZAI_BASE_URL") {
		t.Errorf("output offers ZAI_BASE_URL as a credential source; it backs upstream_env.base_url:\n%s", out)
	}
	if !strings.Contains(out, "does not apply itself") {
		t.Errorf("output does not state that changing a credential is not self-applying:\n%s", out)
	}
}

// TestProviderCredentialsWarnsUnforwardedSourceVar is the regression guard for
// the sharpest failure on this path: the supervisor's service file carries
// only names that clear its persist gate, so a value placed in the secrets
// file under an unrecognized name is DROPPED, session expansion of "${VAR}"
// yields "", and the fleet comes up with a blank credential rather than the
// old one.
func TestProviderCredentialsWarnsUnforwardedSourceVar(t *testing.T) {
	newCredentialsTestEnv(t)
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
base = "builtin:claude"
env = {ANTHROPIC_API_KEY = "${ACME_KEY}"}
`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "zai"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0\nstderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"does NOT forward", "ACME_KEY", "BLANK", "GC_SUPERVISOR_ENV"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q — the operator is not told this variable never reaches the fleet:\n%s", want, out)
		}
	}
}

// TestProviderCredentialsQuietForForwardedSourceVar is the other half of the
// guard above: a recognized name must NOT carry the warning, or the warning
// becomes noise that is skipped exactly when it matters.
func TestProviderCredentialsQuietForForwardedSourceVar(t *testing.T) {
	newCredentialsTestEnv(t)
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
base = "builtin:claude"
env = {ANTHROPIC_API_KEY = "${ANTHROPIC_AUTH_TOKEN_ZAI}"}
`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "zai"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0\nstderr: %s", code, stderr.String())
	}
	if out := stdout.String(); strings.Contains(out, "does NOT forward") {
		t.Errorf("ANTHROPIC_AUTH_TOKEN_ZAI clears the supervisor's persist gate, so no forwarding warning belongs here:\n%s", out)
	}
}

// TestProviderCredentialsResolvesBaselessBuiltinProvider is the regression
// guard for the provider-resolution divergence: a city entry naming a built-in
// with no `base` line is the legacy shape, and session start layers it over
// the built-in (ResolveProvider -> lookupProvider) while the eager
// ResolvedProviders cache returns it as-is with no binding. Reading the cache
// makes this command refuse a credential the fleet demonstrably uses.
func TestProviderCredentialsResolvesBaselessBuiltinProvider(t *testing.T) {
	newCredentialsTestEnv(t)
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.claude]
env = {ANTHROPIC_API_KEY = "${ACME_KEY}"}
`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"provider", "credentials", "--city", cityPath, "claude"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d; want 0 — this provider inherits its upstream_env binding from the built-in\nstdout: %s\nstderr: %s",
			code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ANTHROPIC_API_KEY") || !strings.Contains(out, "ACME_KEY") {
		t.Errorf("output does not resolve the inherited binding to its source variable:\n%s", out)
	}
}

// TestProviderCredentialsReportsAgentScopedOverrides: when an agent selects an
// upstream, the credential the agent reads comes from the upstream axis, which
// session start injects after the provider layer. Reporting the provider's
// variable alone would send the operator to change the wrong one.
func TestProviderCredentialsReportsAgentScopedOverrides(t *testing.T) {
	newCredentialsTestEnv(t)
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
base = "builtin:claude"
env = {ANTHROPIC_API_KEY = "${ACME_KEY}"}

[upstreams.alt]
api_key = "${ALT_KEY}"

[[agent]]
name = "worker"
provider = "zai"
upstream = "alt"
`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "zai"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0\nstderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Agent-scoped overrides", "worker", "upstreams"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q — the agent's real credential source is not the provider's:\n%s", want, out)
		}
	}
}

// TestProviderCredentialsSurfacesUnparseableSecretsFile: the supervisor's own
// reader abandons the whole file when it does not parse, dropping every entry
// rather than the bad line. Reporting "not set" for such a file would send the
// operator to add a value that is already there.
func TestProviderCredentialsSurfacesUnparseableSecretsFile(t *testing.T) {
	gcHome := newCredentialsTestEnv(t)
	if err := os.WriteFile(filepath.Join(gcHome, "secrets.env"), []byte("ACME_KEY=good\nthis-line-has-no-equals\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
base = "builtin:claude"
env = {ANTHROPIC_API_KEY = "${ACME_KEY}"}
`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "zai"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0\nstderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "does not parse") || !strings.Contains(out, "drops every entry") {
		t.Errorf("output does not surface the unparseable secrets file:\n%s", out)
	}
}

func TestProviderCredentialsUnknownProvider(t *testing.T) {
	newCredentialsTestEnv(t)
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "nope"}, &stdout, &stderr); code == 0 {
		t.Fatalf("run() = 0; want non-zero for an unknown provider\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "nope") {
		t.Errorf("stderr does not name the provider: %q", stderr.String())
	}
}

// TestProviderCredentialsOffersNoWritePath pins the deliberate absence of a
// --set flag. Writing the credential from here can report success while the
// fleet gets the old key or a blank one, for reasons that live outside this
// command; the report names them instead.
func TestProviderCredentialsOffersNoWritePath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--help) = %d; want 0\nstderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, forbidden := range []string{"--set-stdin", "--set-from-file"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("help advertises %s; the write path is deliberately withheld:\n%s", forbidden, out)
		}
	}
}

// TestProviderCredentialsHonorsSupervisorEnvOptIn guards against warning about
// a variable the supervisor DOES forward. supervisorServiceExtraEnv gates on
// the persist allow-list OR an explicit GC_SUPERVISOR_ENV opt-in; a report
// that checks only the allow-list tells the operator their rotation will come
// up blank when it will not. A warning that fires when it should not is worn
// down to noise, which is how the real one gets skipped.
func TestProviderCredentialsHonorsSupervisorEnvOptIn(t *testing.T) {
	newCredentialsTestEnv(t)
	t.Setenv("GC_SUPERVISOR_ENV", "ACME_KEY")
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
base = "builtin:claude"
env = {ANTHROPIC_API_KEY = "${ACME_KEY}"}
`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "zai"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0\nstderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ACME_KEY") {
		t.Fatalf("output does not name the source variable at all:\n%s", out)
	}
	if strings.Contains(out, "does NOT forward") {
		t.Errorf("warned that ACME_KEY is not forwarded, but GC_SUPERVISOR_ENV opts it in and supervisorServiceExtraEnv honors that:\n%s", out)
	}
}
