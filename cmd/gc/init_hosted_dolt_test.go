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

func TestHostedDoltInitOptionsApplySelector(t *testing.T) {
	tests := []struct {
		name       string
		opts       hostedDoltInitOptions
		wantHost   string
		wantPort   int
		wantErrSub string
	}{
		{name: "direct local", opts: hostedDoltInitOptions{Transport: "direct", Target: "local"}},
		{name: "proxied local", opts: hostedDoltInitOptions{Transport: "proxied", Target: "local"}},
		{name: "direct external", opts: hostedDoltInitOptions{Transport: "direct", Target: "external", Host: "db.example", Port: "4406", Database: "bd_x", ProjectID: "x"}},
		{name: "proxied external", opts: hostedDoltInitOptions{Transport: "proxied", Target: "external", Host: "db.example", Port: "4406", Database: "bd_x", ProjectID: "x"}},
		{name: "external requires host", opts: hostedDoltInitOptions{Transport: "proxied", Target: "external"}, wantErrSub: "--dolt-host"},
		{name: "local rejects host", opts: hostedDoltInitOptions{Transport: "direct", Target: "local", Host: "db.example", Port: "4406"}, wantErrSub: "local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.City{}
			err := tt.opts.applySelectorToCityConfig(cfg)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantErrSub)) {
					t.Fatalf("applySelectorToCityConfig() = %v, want error containing %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("applySelectorToCityConfig() error = %v", err)
			}
			if cfg.Dolt.Host != tt.wantHost || cfg.Dolt.Port != tt.wantPort {
				t.Fatalf("Dolt config = %+v, want generic selector to leave city config unchanged", cfg.Dolt)
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
	wiz, _, err := initWizardConfigFromFlags(cmd, "", "", nil, "custom", "", hosted, false)
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
	_, _, err := initWizardConfigFromFlags(cmd, "", "", nil, "custom", "", hosted, false)
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
	_, _, err := initWizardConfigFromFlags(cmd, "", "", nil, "", "", hosted, false)
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

// TestGcInitCommandBeadsSelectorMatrix exercises the real init RunE selector
// wiring. Each supported transport/target pair is rejected cleanly when the
// effective provider is incompatible, before any city ledger files are
// written; this also pins the typed refusal path for malformed selectors.
func TestGcInitCommandBeadsSelectorMatrix(t *testing.T) {
	tests := []struct {
		name, transport, target string
	}{
		{name: "direct local", transport: "direct", target: "local"},
		{name: "direct external", transport: "direct", target: "external"},
		{name: "proxied local", transport: "proxied", target: "local"},
		{name: "proxied external", transport: "proxied", target: "external"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_DOLT", "")
			cityPath := filepath.Join(t.TempDir(), "selector-city")
			var stdout, stderr bytes.Buffer
			cmd := newInitCmd(&stdout, &stderr)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			args := []string{"--template", "gascity", "--default-provider", "claude", "--skip-provider-readiness", "--no-start", "--beads-transport", tc.transport, "--beads-target", tc.target}
			if tc.target == "external" {
				args = append(args, "--dolt-host", "gateway.example.com", "--dolt-port", "4406", "--dolt-database", "bd_prj_x", "--dolt-project-id", "prj_x")
			}
			args = append(args, cityPath)
			cmd.SetArgs(args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("gc init unexpectedly succeeded with incompatible file backend")
			}
			if !strings.Contains(stderr.String(), "bd-backed") {
				t.Fatalf("gc init selector error = %q, want bd-backed provider refusal", stderr.String())
			}
			assertNoHostedDoltStoreArtifacts(t, cityPath)
		})
	}
}

// TestGcInitFileBeadsSelectorMatrixRejectsFileBackendWithoutLedgerMutation
// covers the --file front door. Its source config selects the file provider,
// so every explicit bd transport/target selector must fail before the file
// bootstrap can create .gc/beads.json.
func TestGcInitFileBeadsSelectorMatrixRejectsFileBackendWithoutLedgerMutation(t *testing.T) {
	source := filepath.Join(t.TempDir(), "file-provider.toml")
	if err := os.WriteFile(source, []byte("[workspace]\nname = \"file-source\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, transport, target string
	}{
		{name: "direct local", transport: "direct", target: "local"},
		{name: "direct external", transport: "direct", target: "external"},
		{name: "proxied local", transport: "proxied", target: "local"},
		{name: "proxied external", transport: "proxied", target: "external"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			// --file's source config is authoritative even when the invoking
			// shell selects bd for another city.
			t.Setenv("GC_BEADS", "bd")
			cityPath := filepath.Join(t.TempDir(), "file-selector-city")
			var stdout, stderr bytes.Buffer
			cmd := newInitCmd(&stdout, &stderr)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			args := []string{"--file", source, "--skip-provider-readiness", "--no-start", "--beads-transport", tc.transport, "--beads-target", tc.target}
			if tc.target == "external" {
				// --file owns the complete city TOML, so --dolt-* flags are
				// intentionally exclusive with it. The selector's ephemeral
				// external endpoint therefore arrives through the documented
				// environment fallback.
				t.Setenv(envDoltHost, "gateway.example.com")
				t.Setenv(envDoltPort, "4406")
				t.Setenv(envDoltDatabase, "bd_file")
				t.Setenv(envBeadsProjectID, "file")
			}
			args = append(args, cityPath)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("gc init --file unexpectedly succeeded with incompatible file backend")
			}
			if !strings.Contains(stderr.String(), "bd-backed") {
				t.Fatalf("gc init --file selector error = %q, want bd-backed provider refusal", stderr.String())
			}
			assertNoHostedDoltStoreArtifacts(t, cityPath)
		})
	}
}

// TestGcInitFileBeadsSelectorMatrixRejectsEffectiveBackendOverrides covers
// the inverse --file mismatch: bootstrap honors the effective GC_BEADS and
// GC_BEADS_BACKEND settings, so selector admission must reject either before
// it creates a file store.
func TestGcInitFileBeadsSelectorMatrixRejectsEffectiveBackendOverrides(t *testing.T) {
	for _, backend := range []struct {
		name, provider, environment, backend, wantError string
	}{
		{name: "file provider override", provider: "bd", environment: "file", wantError: "bd-backed"},
		{name: "doltlite backend override", provider: "bd", environment: "bd", backend: "doltlite", wantError: "incompatible with the doltlite"},
	} {
		t.Run(backend.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "bd-provider.toml")
			if err := os.WriteFile(source, []byte("[workspace]\nname = \"bd-source\"\n\n[beads]\nprovider = \""+backend.provider+"\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, transport, target string
			}{
				{name: "direct local", transport: "direct", target: "local"},
				{name: "direct external", transport: "direct", target: "external"},
				{name: "proxied local", transport: "proxied", target: "local"},
				{name: "proxied external", transport: "proxied", target: "external"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					clearGCEnv(t)
					t.Setenv("GC_BEADS", backend.environment)
					t.Setenv("GC_BEADS_BACKEND", backend.backend)
					if tc.target == "external" {
						t.Setenv(envDoltHost, "gateway.example.com")
						t.Setenv(envDoltPort, "4406")
						t.Setenv(envDoltDatabase, "bd_file")
						t.Setenv(envBeadsProjectID, "file")
					}
					cityPath := filepath.Join(t.TempDir(), "file-selector-city")
					var stdout, stderr bytes.Buffer
					cmd := newInitCmd(&stdout, &stderr)
					cmd.SilenceUsage = true
					cmd.SilenceErrors = true
					cmd.SetArgs([]string{"--file", source, "--skip-provider-readiness", "--no-start", "--beads-transport", tc.transport, "--beads-target", tc.target, cityPath})
					if err := cmd.Execute(); err == nil {
						t.Fatalf("gc init --file unexpectedly succeeded with %s", backend.name)
					}
					if !strings.Contains(stderr.String(), backend.wantError) {
						t.Fatalf("gc init --file error = %q, want %q", stderr.String(), backend.wantError)
					}
					assertNoHostedDoltStoreArtifacts(t, cityPath)
				})
			}
		})
	}
}

// TestGcInitCommandGenericSelectorRecordsOwnershipBeforeProviderInit drives
// the real command path through a bd-contract wrapper. The wrapper observes
// the durable pending journal before it creates Beads files, which keeps a
// generic selector from being reclassified as a legacy store on retry.
func TestGcInitCommandGenericSelectorRecordsOwnershipBeforeProviderInit(t *testing.T) {
	for _, tc := range []struct {
		name, transport, target, wantMode string
	}{
		{name: "direct local", transport: "direct", target: "local", wantMode: "server"},
		{name: "proxied local", transport: "proxied", target: "local", wantMode: "proxied-server"},
		{name: "direct external", transport: "direct", target: "external", wantMode: "server"},
		{name: "proxied external", transport: "proxied", target: "external", wantMode: "proxied-server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			stubInitDependencyChecks(t)
			stubInitDoltAuthorIdentity(t, map[string]string{"user.name": "test", "user.email": "test@example.com"})
			stubInitRemoteImports(t)
			disableBootstrapForTests(t)

			cityPath := filepath.Join(t.TempDir(), "selector-city")
			providerLog := filepath.Join(t.TempDir(), "provider.log")
			script := filepath.Join(t.TempDir(), "gc-beads-bd.sh")
			const scriptBody = `#!/bin/sh
set -eu
op="$1"
if [ "$op" = init ]; then
  scope="$2"
  test -f "$GC_CITY_PATH/.gc/scope-ownership.json"
  test ! -e "$scope/.beads/metadata.json"
  printf 'init|%s|%s|%s|%s\n' "${GC_BEADS_TRANSPORT:-}" "${GC_BEADS_TARGET:-}" "${BEADS_DOLT_SERVER_HOST:-}" "${GC_BEADS_PROXY_EXTERNAL_HOST:-}" >> "$GC_TEST_PROVIDER_LOG"
  mkdir -p "$scope/.beads"
  if [ "${GC_BEADS_TRANSPORT:-}" = proxied ]; then mode=proxied-server; else mode=server; fi
  printf '{"backend":"dolt","dolt_mode":"%s","dolt_database":"%s"}\n' "$mode" "${GC_DOLT_DATABASE:-hq}" > "$scope/.beads/metadata.json"
  if [ "${GC_BEADS_TARGET:-}" = external ] && [ "${GC_BEADS_TRANSPORT:-}" = direct ]; then
    printf 'issue_prefix: gc\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.host: %s\ndolt.port: %s\ndolt.auto-start: false\ndolt.mode: server\n' "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" > "$scope/.beads/config.yaml"
  else
    printf 'issue_prefix: gc\ndolt.mode: %s\n' "$mode" > "$scope/.beads/config.yaml"
  fi
  if [ "${GC_BEADS_TARGET:-}" = external ] && [ "${GC_BEADS_TRANSPORT:-}" = proxied ]; then
    printf '{"external":{"host":"gateway.example.com","port":4406}}\n' > "$scope/.beads/proxied_server_client_info.json"
  fi
  exit 0
fi
printf '%s\n' "$op" >> "$GC_TEST_PROVIDER_LOG"
`
			if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_BEADS", "exec:"+script)
			t.Setenv("GC_TEST_PROVIDER_LOG", providerLog)
			t.Setenv("GC_DOLT", "")

			args := []string{"--template", "gascity", "--default-provider", "claude", "--skip-provider-readiness", "--no-start", "--beads-transport", tc.transport, "--beads-target", tc.target}
			if tc.target == "external" {
				args = append(args, "--dolt-host", "gateway.example.com", "--dolt-port", "4406", "--dolt-database", "bd_selector", "--dolt-project-id", "selector")
			}
			args = append(args, cityPath)
			run := func() (string, string, error) {
				var stdout, stderr bytes.Buffer
				cmd := newInitCmd(&stdout, &stderr)
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				cmd.SetArgs(args)
				return stdout.String(), stderr.String(), cmd.Execute()
			}
			if stdout, stderr, err := run(); err != nil {
				data, _ := os.ReadFile(providerLog)
				t.Fatalf("gc init %s: %v; stdout=%s; stderr=%s; provider=%s", tc.name, err, stdout, stderr, data)
			}

			entry, owned, err := providerScopeOwnership(cityPath, cityPath)
			if err != nil || !owned || entry.State != providerScopeReady {
				t.Fatalf("ownership after init = (%+v, %t, %v), want ready provider scope", entry, owned, err)
			}
			data, err := os.ReadFile(providerLog)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) == 0 || lines[0] != "init|"+tc.transport+"|"+tc.target+"|"+map[bool]string{true: "gateway.example.com", false: ""}[tc.target == "external" && tc.transport == "direct"]+"|"+map[bool]string{true: "gateway.example.com", false: ""}[tc.target == "external" && tc.transport == "proxied"] {
				t.Fatalf("provider init observation = %q, want pending selector intent and only its applicable endpoint", string(data))
			}
			mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "metadata.json"))
			if err != nil || !ok || mode != tc.wantMode {
				t.Fatalf("provider metadata mode = (%q, %v, %v), want %q", mode, ok, err, tc.wantMode)
			}
			cityTOML, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(cityTOML), "gateway.example.com") || strings.Contains(string(cityTOML), "bd_selector") {
				t.Fatalf("generic selector persisted endpoint identity in city.toml:\n%s", cityTOML)
			}

			if _, stderr, err := run(); err != nil {
				t.Fatalf("gc init resume %s: %v; stderr=%s", tc.name, err, stderr)
			}
			data, err = os.ReadFile(providerLog)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "init|") != 1 {
				t.Fatalf("resume reinitialized provider scope:\n%s", data)
			}
		})
	}
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

func TestPersistFreshProviderOwnershipPrecedesUninstalledRemoteImport(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(`[workspace]
name = "remote-pending"
[beads]
provider = "bd"
[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistFreshProviderOwnership(city, hostedDoltInitOptions{}); err != nil {
		t.Fatalf("persistFreshProviderOwnership: %v", err)
	}
	entry, owned, err := providerScopeOwnership(city, city)
	if err != nil || !owned || entry.State != providerScopeInitializing || entry.Intent != (providerScopeIntent{Transport: "proxied", Target: "local"}) {
		t.Fatalf("ownership = (%+v, %t, %v), want pending proxied/local", entry, owned, err)
	}
	for _, name := range []string{"metadata.json", "config.yaml"} {
		if _, err := os.Stat(filepath.Join(city, ".beads", name)); !os.IsNotExist(err) {
			t.Fatalf("ownership persistence seeded legacy %s: %v", name, err)
		}
	}
}

func TestGenericExternalSelectorRejectsFileBackendWithoutBeadsMutation(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	city := filepath.Join(t.TempDir(), "file-city")
	var stdout, stderr bytes.Buffer
	code := doInit(fsys.OSFS{}, city, wizardConfig{
		configName: "minimal",
		provider:   "claude",
		hostedDolt: hostedDoltInitOptions{Transport: "direct", Target: "external", Host: "db.example", Port: "3306", Database: "bd_file", ProjectID: "file"},
	}, "", &stdout, &stderr, false)
	if code != 1 || !strings.Contains(stderr.String(), "bd-backed beads provider") {
		t.Fatalf("generic external file init = %d, stderr=%q", code, stderr.String())
	}
	for _, name := range []string{"metadata.json", "config.yaml"} {
		if _, err := os.Stat(filepath.Join(city, ".beads", name)); !os.IsNotExist(err) {
			t.Fatalf("generic external selector wrote .beads/%s for file backend: %v", name, err)
		}
	}
}

func TestPersistedSelectorAuthorityHonorsInitializedBeadsBinding(t *testing.T) {
	for _, tc := range []struct {
		name         string
		metadata     string
		beadsConfig  string
		sidecar      string
		opts         hostedDoltInitOptions
		wantIntent   providerScopeIntent
		wantErr      string
		checkNoLease bool
	}{
		{
			name:        "legacy direct local identical",
			metadata:    `{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`,
			beadsConfig: "issue_prefix: gc\ndolt.mode: server\n",
			opts:        hostedDoltInitOptions{Transport: "direct", Target: "local"},
			wantIntent:  providerScopeIntent{Transport: "direct", Target: "local"},
		},
		{
			name:        "proxied local identical",
			metadata:    `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`,
			beadsConfig: "issue_prefix: gc\ndolt.mode: proxied-server\n",
			opts:        hostedDoltInitOptions{Transport: "proxied", Target: "local"},
			wantIntent:  providerScopeIntent{Transport: "proxied", Target: "local"},
		},
		{
			name:         "direct external identical has no endpoint lease",
			metadata:     `{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`,
			beadsConfig:  "issue_prefix: gc\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.host: db.example.test\ndolt.port: 4406\ndolt.auto-start: false\ndolt.mode: server\n",
			opts:         hostedDoltInitOptions{Transport: "direct", Target: "external", Host: "other.example.test", Port: "4407", Database: "bd_other", ProjectID: "other"},
			wantIntent:   providerScopeIntent{Transport: "direct", Target: "external"},
			checkNoLease: true,
		},
		{
			name:         "proxied external identical has no endpoint lease",
			metadata:     `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`,
			beadsConfig:  "issue_prefix: gc\ndolt.mode: proxied-server\n",
			sidecar:      `{"external":{"host":"db.example.test","port":4406}}`,
			opts:         hostedDoltInitOptions{Transport: "proxied", Target: "external", Host: "other.example.test", Port: "4407", Database: "bd_other", ProjectID: "other"},
			wantIntent:   providerScopeIntent{Transport: "proxied", Target: "external"},
			checkNoLease: true,
		},
		{
			name:        "direct conflict refuses",
			metadata:    `{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`,
			beadsConfig: "issue_prefix: gc\ndolt.mode: server\n",
			opts:        hostedDoltInitOptions{Transport: "proxied", Target: "local"},
			wantErr:     "conflicting provider initialization intent",
		},
		{
			name:     "embedded refuses",
			metadata: `{"backend":"dolt","dolt_mode":"embedded","dolt_database":"hq"}`,
			opts:     hostedDoltInitOptions{Transport: "proxied", Target: "local"},
			wantErr:  "cannot apply beads selector",
		},
		{
			name:     "non dolt refuses",
			metadata: `{"backend":"postgres","dolt_mode":"server"}`,
			opts:     hostedDoltInitOptions{Transport: "direct", Target: "local"},
			wantErr:  "unsupported backend",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"authority\"\n[beads]\nprovider = \"bd\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(scopeMetadataJSONPath(city), []byte(tc.metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.beadsConfig != "" {
				if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(tc.beadsConfig), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sidecar != "" {
				if err := os.WriteFile(filepath.Join(city, ".beads", "proxied_server_client_info.json"), []byte(tc.sidecar), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := loadCityConfig(city, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			got, initialized, err := tc.opts.persistedSelectorAuthority(city, *cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("persistedSelectorAuthority error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !initialized || got != tc.wantIntent {
				t.Fatalf("persistedSelectorAuthority = (%+v, %t, %v), want (%+v, true, nil)", got, initialized, err, tc.wantIntent)
			}
			if tc.checkNoLease {
				if err := tc.opts.registerSelectorEndpointForInit(city); err != nil {
					t.Fatal(err)
				}
				if hasSelectorExternalInitOptions(city) {
					t.Fatal("ready selector retained a one-shot external endpoint lease")
				}
			}
		})
	}
}

func TestPersistedSelectorAuthorityUsesPendingIntentOverPartialProviderFiles(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"pending\"\n[beads]\nprovider = \"bd\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// bd may write config.yaml before metadata.json. A durable pending journal
	// must still make the matching selector retryable.
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte("issue_prefix: gc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending := providerScopeIntent{Transport: "direct", Target: "external"}
	if err := persistProviderScopeOwnership(city, city, pending); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	matching := hostedDoltInitOptions{Transport: "direct", Target: "external", Host: "db.retry.example", Port: "4406", Database: "bd_retry", ProjectID: "retry"}
	got, initialized, err := matching.persistedSelectorAuthority(city, *cfg)
	if err != nil || initialized || got != pending {
		t.Fatalf("matching partial retry authority = (%+v, %t, %v), want (%+v, false, nil)", got, initialized, err, pending)
	}
	if err := matching.registerSelectorEndpointForInit(city); err != nil {
		t.Fatalf("register matching partial retry endpoint: %v", err)
	}
	if !hasSelectorExternalInitOptions(city) {
		t.Fatal("matching partial retry did not retain its one-shot endpoint")
	}
	t.Cleanup(func() {
		clearSelectorExternalInitOptions(city, city)
		clearCityDoltConfig(city)
	})
	conflicting := matching
	conflicting.Transport = "proxied"
	if _, _, err := conflicting.persistedSelectorAuthority(city, *cfg); err == nil || !strings.Contains(err.Error(), "conflicting provider initialization intent") {
		t.Fatalf("conflicting partial retry = %v, want intent conflict", err)
	}
}
