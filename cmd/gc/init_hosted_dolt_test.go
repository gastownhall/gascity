package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func initHostedCity(t *testing.T) (cityPath, prefix string) {
	t.Helper()
	t.Setenv("GC_DOLT", "") // exercise the external defer branch, not gcDoltSkip
	cityPath = filepath.Join(t.TempDir(), "hosted-city")
	wiz := wizardConfig{
		configName:      "gascity",
		defaultProvider: "claude",
		provider:        "claude",
		providers:       []string{"claude"},
		hostedDolt: hostedDoltInitOptions{
			Host:      "gateway.example.com",
			Port:      "4406",
			User:      "eia",
			Database:  "bd_prj_abc",
			ProjectID: "prj_abc",
		},
	}
	var stdout, stderr bytes.Buffer
	if code := doInit(fsys.OSFS{}, cityPath, wiz, "hosted-city", &stdout, &stderr, false); code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr=%s", code, stderr.String())
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	return cityPath, config.EffectiveHQPrefix(cfg)
}

func envGetterFromMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestResolveHostedDoltInitOptionsFlagsWinOverEnv(t *testing.T) {
	flags := hostedDoltInitFlagValues{
		Host:      "gateway.example.com",
		Port:      "4406",
		User:      "flaguser",
		Database:  "bd_prj_flag",
		ProjectID: "prj_flag",
	}
	env := map[string]string{
		envDoltHost:       "env.example.com",
		envDoltPort:       "9999",
		envDoltUser:       "envuser",
		envDoltDatabase:   "bd_prj_env",
		envBeadsProjectID: "prj_env",
	}
	got := resolveHostedDoltInitOptions(flags, envGetterFromMap(env))
	want := hostedDoltInitOptions{
		Host:      "gateway.example.com",
		Port:      "4406",
		User:      "flaguser",
		Database:  "bd_prj_flag",
		ProjectID: "prj_flag",
	}
	if got != want {
		t.Fatalf("resolveHostedDoltInitOptions flags-win = %+v, want %+v", got, want)
	}
}

func TestResolveHostedDoltInitOptionsEnvFallback(t *testing.T) {
	env := map[string]string{
		envDoltHost:       "env.example.com",
		envDoltPort:       "4406",
		envDoltDatabase:   "bd_prj_env",
		envBeadsProjectID: "prj_env",
	}
	got := resolveHostedDoltInitOptions(hostedDoltInitFlagValues{}, envGetterFromMap(env))
	if got.Host != "env.example.com" || got.Port != "4406" || got.Database != "bd_prj_env" || got.ProjectID != "prj_env" {
		t.Fatalf("resolveHostedDoltInitOptions env-fallback = %+v", got)
	}
}

func TestResolveHostedDoltInitOptionsDerivesProjectIDFromDatabase(t *testing.T) {
	flags := hostedDoltInitFlagValues{Host: "h", Port: "4406", Database: "bd_prj_abc123"}
	got := resolveHostedDoltInitOptions(flags, envGetterFromMap(nil))
	if got.ProjectID != "prj_abc123" {
		t.Fatalf("derived ProjectID = %q, want prj_abc123", got.ProjectID)
	}
}

func TestResolveHostedDoltInitOptionsDoesNotDeriveWhenExplicit(t *testing.T) {
	flags := hostedDoltInitFlagValues{Host: "h", Port: "4406", Database: "bd_prj_abc", ProjectID: "prj_explicit"}
	got := resolveHostedDoltInitOptions(flags, envGetterFromMap(nil))
	if got.ProjectID != "prj_explicit" {
		t.Fatalf("explicit ProjectID overwritten = %q", got.ProjectID)
	}
}

func TestResolveHostedDoltInitOptionsDoesNotDeriveWithoutBdPrefix(t *testing.T) {
	flags := hostedDoltInitFlagValues{Host: "h", Port: "4406", Database: "weird_name"}
	got := resolveHostedDoltInitOptions(flags, envGetterFromMap(nil))
	if got.ProjectID != "" {
		t.Fatalf("ProjectID should not be derived from non-bd_ database, got %q", got.ProjectID)
	}
}

func TestHostedDoltInitOptionsEnabled(t *testing.T) {
	if (hostedDoltInitOptions{}).enabled() {
		t.Fatal("empty options should not be enabled")
	}
	if !(hostedDoltInitOptions{Host: "h"}).enabled() {
		t.Fatal("options with host should be enabled")
	}
	if (hostedDoltInitOptions{Host: "   "}).enabled() {
		t.Fatal("whitespace-only host should not be enabled")
	}
}

func TestHostedDoltInitOptionsValidate(t *testing.T) {
	base := hostedDoltInitOptions{Host: "gateway.example.com", Port: "4406", Database: "bd_prj_x", ProjectID: "prj_x"}
	tests := []struct {
		name    string
		mutate  func(o hostedDoltInitOptions) hostedDoltInitOptions
		wantErr string // substring; "" means no error
	}{
		{"valid", func(o hostedDoltInitOptions) hostedDoltInitOptions { return o }, ""},
		{"not-enabled-empty", func(_ hostedDoltInitOptions) hostedDoltInitOptions { return hostedDoltInitOptions{} }, ""},
		{"port-without-host", func(_ hostedDoltInitOptions) hostedDoltInitOptions {
			return hostedDoltInitOptions{Port: "4406"}
		}, "--dolt-host"},
		{"missing-port", func(o hostedDoltInitOptions) hostedDoltInitOptions { o.Port = ""; return o }, "--dolt-port"},
		{"bad-port", func(o hostedDoltInitOptions) hostedDoltInitOptions { o.Port = "abc"; return o }, "invalid --dolt-port"},
		{"zero-port", func(o hostedDoltInitOptions) hostedDoltInitOptions { o.Port = "0"; return o }, "invalid --dolt-port"},
		{"missing-database", func(o hostedDoltInitOptions) hostedDoltInitOptions { o.Database = ""; return o }, "--dolt-database"},
		{"reserved-database", func(o hostedDoltInitOptions) hostedDoltInitOptions { o.Database = "mysql"; return o }, "reserved"},
		{"missing-project-id", func(o hostedDoltInitOptions) hostedDoltInitOptions {
			o.ProjectID = ""
			o.Database = "weird" // non-bd_ so no derivation; but reserved check... "weird" is fine
			return o
		}, "--dolt-project-id"},
		{"wildcard-host", func(o hostedDoltInitOptions) hostedDoltInitOptions { o.Host = "0.0.0.0"; return o }, "concrete host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mutate(base).validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestHostedDoltInitAppliesAPIPortDefault pins the control-plane reachability
// contract for hosted cities. A hosted city's controller runs out-of-session,
// so the control dispatcher and gc CLI reach it only through the HTTP API, and
// every API consumer treats cfg.API.Port == 0 as "API disabled". Neither plain
// init nor the hosted endpoint flags write an [api] section (only the k8s-cell
// bootstrap profile does), so without this default a hosted init yields a city
// whose control plane is unreachable until an [api] section is hand-added.
func TestHostedDoltInitAppliesAPIPortDefault(t *testing.T) {
	t.Run("defaults the API port when no [api] section is set", func(t *testing.T) {
		o := hostedDoltInitOptions{Host: "gateway.example.com", Port: "4406", Database: "bd_prj_x", ProjectID: "prj_x"}
		var cfg config.City
		if err := o.applyToCityConfig(&cfg); err != nil {
			t.Fatalf("applyToCityConfig() error = %v", err)
		}
		if cfg.API.Port != config.DefaultAPIPort {
			t.Fatalf("cfg.API.Port = %d, want %d (hosted controller is reachable only via the HTTP API)", cfg.API.Port, config.DefaultAPIPort)
		}
	})
	t.Run("preserves an API config already pinned by a bootstrap profile", func(t *testing.T) {
		o := hostedDoltInitOptions{Host: "gateway.example.com", Port: "4406", Database: "bd_prj_x", ProjectID: "prj_x"}
		var cfg config.City
		cfg.API.Port = 12345
		cfg.API.Bind = "0.0.0.0"
		cfg.API.AllowMutations = true
		if err := o.applyToCityConfig(&cfg); err != nil {
			t.Fatalf("applyToCityConfig() error = %v", err)
		}
		if cfg.API.Port != 12345 {
			t.Fatalf("cfg.API.Port = %d, want 12345 preserved (bootstrap profile wins)", cfg.API.Port)
		}
		if cfg.API.Bind != "0.0.0.0" || !cfg.API.AllowMutations {
			t.Fatalf("bootstrap-profile API config clobbered: bind=%q allowMutations=%v", cfg.API.Bind, cfg.API.AllowMutations)
		}
	})
}

func TestInitWizardConfigFromFlagsCapturesHostedDolt(t *testing.T) {
	cmd := newInitCmd(io.Discard, io.Discard)
	if err := cmd.Flags().Set("template", "custom"); err != nil {
		t.Fatalf("set template: %v", err)
	}
	hosted := hostedDoltInitOptions{Host: "gateway.example.com", Port: "4406", Database: "bd_prj_x", ProjectID: "prj_x"}
	wiz, _, err := initWizardConfigFromFlags(cmd, "", "", nil, "custom", "", hosted)
	if err != nil {
		t.Fatalf("initWizardConfigFromFlags: %v", err)
	}
	if !wiz.hostedDolt.enabled() {
		t.Fatal("expected wiz.hostedDolt to be enabled")
	}
	if wiz.hostedDolt.ProjectID != "prj_x" || wiz.hostedDolt.Database != "bd_prj_x" {
		t.Fatalf("wiz.hostedDolt = %+v", wiz.hostedDolt)
	}
}

func TestInitWizardConfigFromFlagsRejectsInvalidHostedDolt(t *testing.T) {
	cmd := newInitCmd(io.Discard, io.Discard)
	if err := cmd.Flags().Set("template", "custom"); err != nil {
		t.Fatalf("set template: %v", err)
	}
	hosted := hostedDoltInitOptions{Host: "gateway.example.com"} // missing port/database/project-id
	_, _, err := initWizardConfigFromFlags(cmd, "", "", nil, "custom", "", hosted)
	if err == nil || !strings.Contains(err.Error(), "--dolt-port") {
		t.Fatalf("initWizardConfigFromFlags = %v, want --dolt-port error", err)
	}
}

// Hosted-dolt alone must defeat the early "no flags changed" return so the
// non-interactive path runs; with the default (gascity) template that then
// surfaces the existing provider requirement rather than silently dropping
// the hosted endpoint into an interactive wizard.
func TestInitWizardConfigFromFlagsHostedDoltDefaultTemplateRequiresProvider(t *testing.T) {
	cmd := newInitCmd(io.Discard, io.Discard)
	hosted := hostedDoltInitOptions{Host: "gateway.example.com", Port: "4406", Database: "bd_prj_x", ProjectID: "prj_x"}
	_, _, err := initWizardConfigFromFlags(cmd, "", "", nil, "", "", hosted)
	if err == nil || !strings.Contains(err.Error(), "default-provider") {
		t.Fatalf("initWizardConfigFromFlags = %v, want default-provider requirement", err)
	}
}

// doInit with a hosted endpoint writes the full canonical external config
// (R2/R3/R4/R5): city.toml [dolt], .beads/config.yaml (city_canonical +
// unverified), .beads/metadata.json (backend=dolt, dolt_mode=server,
// dolt_database, project_id), and .beads/identity.toml — and the lifecycle
// machinery then resolves the city as external (no managed-local bootstrap).
func TestDoInitWritesCanonicalHostedDoltConfig(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "hosted-city")
	wiz := wizardConfig{
		configName:      "gascity",
		defaultProvider: "claude",
		provider:        "claude",
		providers:       []string{"claude"},
		hostedDolt: hostedDoltInitOptions{
			Host:      "gateway.example.com",
			Port:      "4406",
			User:      "eia",
			Database:  "bd_prj_abc",
			ProjectID: "prj_abc",
		},
	}
	var stdout, stderr bytes.Buffer
	if code := doInit(fsys.OSFS{}, cityPath, wiz, "hosted-city", &stdout, &stderr, false); code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr=%s", code, stderr.String())
	}

	cityData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if !strings.Contains(string(cityData), "gateway.example.com") {
		t.Fatalf("city.toml missing [dolt] host:\n%s", cityData)
	}

	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil || !ok {
		t.Fatalf("ReadConfigState ok=%v err=%v", ok, err)
	}
	if state.EndpointOrigin != contract.EndpointOriginCityCanonical {
		t.Fatalf("EndpointOrigin = %q, want city_canonical", state.EndpointOrigin)
	}
	if state.EndpointStatus != contract.EndpointStatusUnverified {
		t.Fatalf("EndpointStatus = %q, want unverified", state.EndpointStatus)
	}
	if state.DoltHost != "gateway.example.com" || state.DoltPort != "4406" {
		t.Fatalf("config dolt host/port = %q/%q", state.DoltHost, state.DoltPort)
	}

	metaRaw, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	for k, want := range map[string]string{"backend": "dolt", "dolt_mode": "server", "dolt_database": "bd_prj_abc", "project_id": "prj_abc"} {
		if got, _ := meta[k].(string); got != want {
			t.Fatalf("metadata.json[%q] = %q, want %q (full: %s)", k, got, want, metaRaw)
		}
	}

	id, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, cityPath)
	if err != nil || !ok || id != "prj_abc" {
		t.Fatalf("ReadProjectIdentity id=%q ok=%v err=%v", id, ok, err)
	}

	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		t.Fatalf("managedDoltLifecycleOwned: %v", err)
	}
	if owned {
		t.Fatal("managedDoltLifecycleOwned = true, want false (external endpoint)")
	}
	if !isExternalDolt(cityPath) {
		t.Fatal("isExternalDolt = false, want true")
	}
}

// An unverified external endpoint must not require a live connection at init
// time (R5): initDirIfReady writes the canonical files and defers the bd init
// to gc start (which has credentials) instead of running it now.
func TestInitDirIfReadyDefersUnverifiedExternalDolt(t *testing.T) {
	cityPath, prefix := initHostedCity(t)

	orig := initDirIfReadyInitAndHookDir
	t.Cleanup(func() { initDirIfReadyInitAndHookDir = orig })
	called := false
	initDirIfReadyInitAndHookDir = func(_, _, _ string) error { called = true; return nil }

	deferred, err := initDirIfReady(cityPath, cityPath, prefix)
	if err != nil {
		t.Fatalf("initDirIfReady: %v", err)
	}
	if !deferred {
		t.Fatal("initDirIfReady deferred = false, want true for unverified external")
	}
	if called {
		t.Fatal("initDirIfReadyInitAndHookDir ran; the live bd init must be deferred for an unverified external endpoint")
	}
}

// A verified external endpoint keeps the existing behavior: init-and-hook runs
// now (credentials are presumed available), so this change does not regress
// `gc rig add` against an already-validated external city.
func TestInitDirIfReadyInitsVerifiedExternalDolt(t *testing.T) {
	cityPath, prefix := initHostedCity(t)

	cfgPath := filepath.Join(cityPath, ".beads", "config.yaml")
	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, cfgPath)
	if err != nil || !ok {
		t.Fatalf("ReadConfigState ok=%v err=%v", ok, err)
	}
	state.EndpointStatus = contract.EndpointStatusVerified
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, cfgPath, state); err != nil {
		t.Fatalf("EnsureCanonicalConfig verified: %v", err)
	}

	orig := initDirIfReadyInitAndHookDir
	t.Cleanup(func() { initDirIfReadyInitAndHookDir = orig })
	called := false
	initDirIfReadyInitAndHookDir = func(_, _, _ string) error { called = true; return nil }

	deferred, err := initDirIfReady(cityPath, cityPath, prefix)
	if err != nil {
		t.Fatalf("initDirIfReady: %v", err)
	}
	if deferred {
		t.Fatal("initDirIfReady deferred = true for a verified external endpoint; want init-and-hook")
	}
	if !called {
		t.Fatal("initDirIfReadyInitAndHookDir did not run for a verified external endpoint")
	}
}

// Command-level regression: the hosted endpoint can be supplied entirely
// through GC_DOLT_*/GC_BEADS_PROJECT_ID, mirroring the --dolt-* flags so the
// create-city controller need not pass them explicitly. The controller still
// selects the city template/provider, so this drives the real
// newInitCmd(...).Execute() path with the hosted env vars set (no --dolt-*
// flags) and --template/--default-provider supplied, and confirms init exits 0
// recording the env-derived endpoint without a managed-local bootstrap or live
// connection (R5): verification is deferred to gc start.
func TestGcInitCommandHostedDoltEnvOnlyEndpoint(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "") // not "skip": exercise the external defer branch
	t.Setenv("GC_BOOTSTRAP", "skip")
	t.Setenv(envDoltHost, "gateway.example.com")
	t.Setenv(envDoltPort, "4406")
	t.Setenv(envDoltDatabase, "bd_prj_envonly")
	t.Setenv(envBeadsProjectID, "prj_envonly")

	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{"user.name": "ci", "user.email": "ci@example.com"})

	cityPath := filepath.Join(t.TempDir(), "env-city")
	var stdout, stderr bytes.Buffer
	cmd := newInitCmd(&stdout, &stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--template", "gascity",
		"--default-provider", "claude",
		"--skip-provider-readiness",
		"--no-start",
		cityPath,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc init env-only hosted dolt = %v, want success; stderr=%s", err, stderr.String())
	}

	if !isExternalDolt(cityPath) {
		t.Fatal("isExternalDolt = false after env-only hosted init")
	}
	metaRaw, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	for _, want := range []string{"bd_prj_envonly", "prj_envonly"} {
		if !strings.Contains(string(metaRaw), want) {
			t.Fatalf("metadata.json missing env-derived %q:\n%s", want, metaRaw)
		}
	}
}

// Command-level regression for the controller contract boundary: env-only
// hosted-Dolt input does NOT make `gc init` flagless — the controller must
// still pass --template/--default-provider. With the hosted endpoint supplied
// through the environment and no provider flags, the real
// newInitCmd(...).Execute() path rejects the invocation at the existing
// provider requirement (the default gascity template needs a provider) rather
// than silently dropping the hosted endpoint into an interactive wizard, and
// writes no ledger artifacts.
func TestGcInitCommandHostedDoltEnvOnlyRequiresProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	t.Setenv(envDoltHost, "gateway.example.com")
	t.Setenv(envDoltPort, "4406")
	t.Setenv(envDoltDatabase, "bd_prj_x")
	t.Setenv(envBeadsProjectID, "prj_x")

	cityPath := filepath.Join(t.TempDir(), "env-noprov-city")
	var stdout, stderr bytes.Buffer
	cmd := newInitCmd(&stdout, &stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--skip-provider-readiness", "--no-start", cityPath})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("gc init env-only hosted dolt without --default-provider = nil error, want failure; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "default-provider") {
		t.Fatalf("stderr = %q, want a --default-provider requirement", stderr.String())
	}
	assertNoHostedDoltStoreArtifacts(t, cityPath)
}

// A hosted endpoint only makes sense for a bd-backed ledger; supplying
// --dolt-host for a non-bd (file) city must fail fast with a clear message.
func TestDoInitHostedDoltRequiresBdBackedProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "file") // force a non-bd backend
	t.Setenv("GC_DOLT", "")
	cityPath := filepath.Join(t.TempDir(), "file-city")
	wiz := wizardConfig{
		configName:      "gascity",
		defaultProvider: "claude",
		provider:        "claude",
		providers:       []string{"claude"},
		hostedDolt:      hostedDoltInitOptions{Host: "gateway.example.com", Port: "4406", Database: "bd_prj_x", ProjectID: "prj_x"},
	}
	var stdout, stderr bytes.Buffer
	if code := doInit(fsys.OSFS{}, cityPath, wiz, "file-city", &stdout, &stderr, false); code == 0 {
		t.Fatalf("doInit = 0, want failure for hosted dolt on a non-bd city")
	}
	if !strings.Contains(stderr.String(), "bd-backed") {
		t.Fatalf("stderr = %q, want a bd-backed-provider error", stderr.String())
	}
	assertNoHostedDoltStoreArtifacts(t, cityPath)
}

// The doltlite backend is bd-backed (so the provider guard passes) but is a
// local embedded store, not an external Dolt server. Pinning --dolt-host for a
// doltlite city would write backend=dolt server metadata that permanently
// disagrees with the configured doltlite backend (split-brain) and skip the
// external-endpoint defer, so init must reject it before writing any canonical
// hosted-Dolt files. The backend is supplied through GC_BEADS_BACKEND, the
// documented "set env -> gc init -> gc start" controller path.
func TestDoInitHostedDoltRejectsDoltliteBackend(t *testing.T) {
	t.Setenv("GC_BEADS_BACKEND", "doltlite")
	t.Setenv("GC_DOLT", "")
	cityPath := filepath.Join(t.TempDir(), "doltlite-city")
	wiz := wizardConfig{
		configName:      "gascity",
		defaultProvider: "claude",
		provider:        "claude",
		providers:       []string{"claude"},
		hostedDolt:      hostedDoltInitOptions{Host: "gateway.example.com", Port: "4406", Database: "bd_prj_x", ProjectID: "prj_x"},
	}
	var stdout, stderr bytes.Buffer
	if code := doInit(fsys.OSFS{}, cityPath, wiz, "doltlite-city", &stdout, &stderr, false); code == 0 {
		t.Fatalf("doInit = 0, want failure for hosted dolt on a doltlite city; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "doltlite") {
		t.Fatalf("stderr = %q, want a doltlite-incompatibility error", stderr.String())
	}
	assertNoHostedDoltStoreArtifacts(t, cityPath)
}

// Command-level regression: the real `gc init` RunE resolves --dolt-* flags,
// reads GC_BEADS_BACKEND, builds the wizard config, and runs doInit. A doltlite
// effective backend must fail the command and leave no canonical/mixed ledger
// artifacts behind.
func TestGcInitCommandHostedDoltRejectsDoltliteBackend(t *testing.T) {
	t.Setenv("GC_BEADS_BACKEND", "doltlite")
	t.Setenv("GC_DOLT", "")
	cityPath := filepath.Join(t.TempDir(), "doltlite-cmd-city")
	var stdout, stderr bytes.Buffer
	cmd := newInitCmd(&stdout, &stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--template", "gascity",
		"--default-provider", "claude",
		"--skip-provider-readiness",
		"--no-start",
		"--dolt-host", "gateway.example.com",
		"--dolt-port", "4406",
		"--dolt-database", "bd_prj_x",
		"--dolt-project-id", "prj_x",
		cityPath,
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("gc init --dolt-host with doltlite backend = nil error, want failure; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "doltlite") {
		t.Fatalf("stderr = %q, want a doltlite-incompatibility error", stderr.String())
	}
	assertNoHostedDoltStoreArtifacts(t, cityPath)
}

// Command-level regression: a non-bd (file) effective backend must fail the
// real `gc init` command with a clear bd-backed-provider error and leave no
// canonical/mixed ledger artifacts behind. This is the full-RunE sibling of
// TestGcInitCommandHostedDoltRejectsDoltliteBackend, closing the RunE
// flag/env-wiring coverage gap for the file case.
func TestGcInitCommandHostedDoltRejectsFileBackend(t *testing.T) {
	t.Setenv("GC_BEADS", "file") // force a non-bd backend
	t.Setenv("GC_DOLT", "")
	cityPath := filepath.Join(t.TempDir(), "file-cmd-city")
	var stdout, stderr bytes.Buffer
	cmd := newInitCmd(&stdout, &stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--template", "gascity",
		"--default-provider", "claude",
		"--skip-provider-readiness",
		"--no-start",
		"--dolt-host", "gateway.example.com",
		"--dolt-port", "4406",
		"--dolt-database", "bd_prj_x",
		"--dolt-project-id", "prj_x",
		cityPath,
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("gc init --dolt-host with file backend = nil error, want failure; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "bd-backed") {
		t.Fatalf("stderr = %q, want a bd-backed-provider error", stderr.String())
	}
	assertNoHostedDoltStoreArtifacts(t, cityPath)
}

// assertNoHostedDoltStoreArtifacts fails when a rejected hosted-Dolt init left
// any canonical Dolt ledger files or file-store markers on disk — the contract
// is that an incompatible-backend rejection writes no ledger state, so reruns
// after fixing the backend are not poisoned by a split-brain scaffold.
func assertNoHostedDoltStoreArtifacts(t *testing.T, cityPath string) {
	t.Helper()
	for _, rel := range []string{
		filepath.Join(".beads", "config.yaml"),
		filepath.Join(".beads", "metadata.json"),
		filepath.Join(".beads", "identity.toml"),
		filepath.Join(".gc", "beads.json"),
		filepath.Join(".gc", "file-beads-layout"),
	} {
		switch _, err := os.Stat(filepath.Join(cityPath, rel)); {
		case err == nil:
			t.Fatalf("rejected hosted-dolt init left a store artifact: %s", rel)
		case !os.IsNotExist(err):
			t.Fatalf("stat %s: %v", rel, err)
		}
	}
}
