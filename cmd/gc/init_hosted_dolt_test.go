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

// initHostedCity runs doInit against a hosted external endpoint. transport is
// the --beads-transport selector: "" keeps the compat meaning of a bare
// --dolt-host (direct), and "proxied" fronts the endpoint with bd's local
// proxy (--beads-target external).
func initHostedCity(t *testing.T, transport string) (cityPath, prefix string) {
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
	if transport != "" {
		wiz.hostedDolt.Transport = transport
		wiz.hostedDolt.Target = "external"
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

// gc projects GC_DOLT_HOST/PORT/USER into every session and subprocess it
// spawns, so those three alone are the caller's own city connection, not an
// init request. They fill the endpoint only alongside an input gc never
// projects: an explicit --dolt-* flag, a database or project id, or an
// external beads target.
func TestResolveHostedDoltInitOptionsProjectedEndpointNeedsRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags hostedDoltInitFlagValues
		env   map[string]string
		want  hostedDoltInitOptions
	}{
		{name: "projected endpoint alone"},
		{
			name: "local target selector",
			env:  map[string]string{envBeadsTransport: "proxied", envBeadsTarget: "local"},
			want: hostedDoltInitOptions{Transport: "proxied", Target: "local"},
		},
		{
			name: "database env",
			env:  map[string]string{envDoltDatabase: "bd_prj_x"},
			want: hostedDoltInitOptions{Host: "127.0.0.1", Port: "3307", User: "root", Database: "bd_prj_x", ProjectID: "prj_x"},
		},
		{
			name: "project id env",
			env:  map[string]string{envBeadsProjectID: "prj_x"},
			want: hostedDoltInitOptions{Host: "127.0.0.1", Port: "3307", User: "root", ProjectID: "prj_x"},
		},
		{
			name: "external target env",
			env:  map[string]string{envBeadsTransport: "direct", envBeadsTarget: "External"},
			want: hostedDoltInitOptions{Host: "127.0.0.1", Port: "3307", User: "root", Transport: "direct", Target: "External"},
		},
		{
			name:  "external target flag",
			flags: hostedDoltInitFlagValues{Transport: "proxied", Target: "external"},
			want:  hostedDoltInitOptions{Host: "127.0.0.1", Port: "3307", User: "root", Transport: "proxied", Target: "external"},
		},
		{
			name:  "host flag",
			flags: hostedDoltInitFlagValues{Host: "gateway.example.com"},
			want:  hostedDoltInitOptions{Host: "gateway.example.com", Port: "3307", User: "root"},
		},
		{
			name:  "port flag",
			flags: hostedDoltInitFlagValues{Port: "4406"},
			want:  hostedDoltInitOptions{Host: "127.0.0.1", Port: "4406", User: "root"},
		},
		{
			name:  "user flag",
			flags: hostedDoltInitFlagValues{User: "eia"},
			want:  hostedDoltInitOptions{Host: "127.0.0.1", Port: "3307", User: "eia"},
		},
		{
			name:  "database flag",
			flags: hostedDoltInitFlagValues{Database: "bd_prj_x"},
			want:  hostedDoltInitOptions{Host: "127.0.0.1", Port: "3307", User: "root", Database: "bd_prj_x", ProjectID: "prj_x"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{envDoltHost: "127.0.0.1", envDoltPort: "3307", envDoltUser: "root"}
			for k, v := range tc.env {
				env[k] = v
			}
			if got := resolveHostedDoltInitOptions(tc.flags, envGetterFromMap(env)); got != tc.want {
				t.Fatalf("resolveHostedDoltInitOptions = %+v, want %+v", got, tc.want)
			}
		})
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
		wantMode   string
		wantHost   string
		wantPort   int
		wantErrSub string
	}{
		{name: "direct local", opts: hostedDoltInitOptions{Transport: "direct", Target: "local"}, wantMode: "server"},
		{name: "proxied local", opts: hostedDoltInitOptions{Transport: "proxied", Target: "local"}, wantMode: "proxied-server"},
		{name: "direct external", opts: hostedDoltInitOptions{Transport: "direct", Target: "external", Host: "db.example", Port: "4406", Database: "bd_x", ProjectID: "x"}, wantMode: "server", wantHost: "db.example", wantPort: 4406},
		{name: "proxied external", opts: hostedDoltInitOptions{Transport: "proxied", Target: "external", Host: "db.example", Port: "4406", Database: "bd_x", ProjectID: "x"}, wantMode: "proxied-server", wantHost: "db.example", wantPort: 4406},
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
			if cfg.Dolt.Mode != tt.wantMode || cfg.Dolt.Host != tt.wantHost || cfg.Dolt.Port != tt.wantPort {
				t.Fatalf("Dolt config = %+v, want mode=%q host=%q port=%d", cfg.Dolt, tt.wantMode, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// TestConfigDoltInitIntentNeverHalfSpecified pins the city-config layer of the
// init precedence: a [dolt] endpoint resolves the same way
// desiredCityDoltConfigState does (host or port selects an external server,
// direct unless mode is proxied-server), a bare mode selects a local target,
// and no row yields a half intent that ResolveInitIntent would reject.
func TestConfigDoltInitIntentNeverHalfSpecified(t *testing.T) {
	tests := []struct {
		name string
		dolt config.DoltConfig
		want contract.InitIntent
	}{
		{name: "empty", dolt: config.DoltConfig{}, want: contract.InitIntent{}},
		{name: "port only", dolt: config.DoltConfig{Port: 3307}, want: contract.InitIntent{Transport: "direct", Target: "external"}},
		{name: "host without mode", dolt: config.DoltConfig{Host: "db.example"}, want: contract.InitIntent{Transport: "direct", Target: "external"}},
		{name: "endpoint without mode", dolt: config.DoltConfig{Host: "db.example", Port: 3306}, want: contract.InitIntent{Transport: "direct", Target: "external"}},
		{name: "endpoint with direct mode", dolt: config.DoltConfig{Host: "db.example", Port: 3306, Mode: "server"}, want: contract.InitIntent{Transport: "direct", Target: "external"}},
		{name: "endpoint with proxied mode", dolt: config.DoltConfig{Host: "db.example", Port: 3306, Mode: "proxied-server"}, want: contract.InitIntent{Transport: "proxied", Target: "external"}},
		{name: "port with proxied mode", dolt: config.DoltConfig{Port: 3307, Mode: "proxied-server"}, want: contract.InitIntent{Transport: "proxied", Target: "external"}},
		{name: "direct mode only", dolt: config.DoltConfig{Mode: "server"}, want: contract.InitIntent{Transport: "direct", Target: "local"}},
		{name: "proxied mode only", dolt: config.DoltConfig{Mode: " Proxied-Server "}, want: contract.InitIntent{Transport: "proxied", Target: "local"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := configDoltInitIntent(config.City{Dolt: tt.dolt})
			if got != tt.want {
				t.Fatalf("configDoltInitIntent(%+v) = %+v, want %+v", tt.dolt, got, tt.want)
			}
			if _, err := contract.ResolveInitIntent(contract.InitScopeState{}, contract.InitIntent{}, contract.InitIntent{}, got, contract.InitIntent{Transport: "proxied", Target: "local"}); err != nil {
				t.Fatalf("ResolveInitIntent rejected config intent %+v: %v", got, err)
			}
		})
	}
}

// TestHostedDoltInitOptionsApplySelectorOverConfiguredDolt covers `gc init
// --file` with a city.toml that already carries a legacy [dolt] endpoint or
// port pin and no mode: the CLI selector (or legacy --dolt-host) must still
// win instead of failing on the config layer's intent.
func TestHostedDoltInitOptionsApplySelectorOverConfiguredDolt(t *testing.T) {
	external := hostedDoltInitOptions{Host: "db.example", Port: "4406", Database: "bd_x", ProjectID: "x"}
	withSelector := func(o hostedDoltInitOptions, transport, target string) hostedDoltInitOptions {
		o.Transport, o.Target = transport, target
		return o
	}
	selectors := []struct {
		name     string
		opts     hostedDoltInitOptions
		wantMode string
		wantHost string
		wantPort int
	}{
		{name: "direct local", opts: withSelector(hostedDoltInitOptions{}, "direct", "local"), wantMode: "server"},
		{name: "proxied local", opts: withSelector(hostedDoltInitOptions{}, "proxied", "local"), wantMode: "proxied-server"},
		{name: "direct external", opts: withSelector(external, "direct", "external"), wantMode: "server", wantHost: "db.example", wantPort: 4406},
		{name: "proxied external", opts: withSelector(external, "proxied", "external"), wantMode: "proxied-server", wantHost: "db.example", wantPort: 4406},
		{name: "legacy dolt-host", opts: external, wantMode: "server", wantHost: "db.example", wantPort: 4406},
	}
	configs := []struct {
		name string
		dolt config.DoltConfig
	}{
		{name: "port only", dolt: config.DoltConfig{Port: 3307}},
		{name: "host without mode", dolt: config.DoltConfig{Host: "legacy.example", Port: 3306}},
	}
	for _, cc := range configs {
		for _, sel := range selectors {
			t.Run(cc.name+"/"+sel.name, func(t *testing.T) {
				cfg := &config.City{Dolt: cc.dolt}
				if err := sel.opts.applySelectorToCityConfig(cfg); err != nil {
					t.Fatalf("applySelectorToCityConfig() error = %v", err)
				}
				if cfg.Dolt.Mode != sel.wantMode || cfg.Dolt.Host != sel.wantHost || cfg.Dolt.Port != sel.wantPort {
					t.Fatalf("Dolt config = %+v, want mode=%q host=%q port=%d", cfg.Dolt, sel.wantMode, sel.wantHost, sel.wantPort)
				}
			})
		}
	}
}

// TestHostedDoltInitOptionsLegacyDoltHostKeepsConfiguredTransport pins that a
// bare legacy --dolt-host supplies only the external target. Over a
// gc init --file config, the transport stays the one [dolt] mode names, so a
// proxied-server selection is not silently downgraded to direct; a config
// that names no mode keeps the legacy direct meaning.
func TestHostedDoltInitOptionsLegacyDoltHostKeepsConfiguredTransport(t *testing.T) {
	legacy := hostedDoltInitOptions{Host: "db.example", Port: "4406", Database: "bd_x", ProjectID: "x"}
	for _, tc := range []struct {
		name     string
		dolt     string
		wantMode string
	}{
		{name: "configured proxied-server", dolt: "[dolt]\nmode = \"proxied-server\"\n", wantMode: "proxied-server"},
		{name: "configured server", dolt: "[dolt]\nmode = \"server\"\n", wantMode: "server"},
		{name: "no configured mode", wantMode: "server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Parse([]byte("[workspace]\nname = \"demo\"\n" + tc.dolt))
			if err != nil {
				t.Fatalf("config.Parse: %v", err)
			}
			if err := legacy.applySelectorToCityConfig(cfg); err != nil {
				t.Fatalf("applySelectorToCityConfig() error = %v", err)
			}
			if cfg.Dolt.Mode != tc.wantMode || cfg.Dolt.Host != "db.example" || cfg.Dolt.Port != 4406 {
				t.Fatalf("Dolt config = %+v, want mode=%q host=%q port=%d", cfg.Dolt, tc.wantMode, "db.example", 4406)
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

// A proxied-external hosted init (--beads-transport proxied --beads-target
// external) records the proxied endpoint in config.yaml and leaves
// metadata.json to bd init: gc-beads-bd.sh takes an existing metadata.json as
// its bd-init witness and the RC proxied initializer refuses one, so
// finalizeCanonicalBdScopeInit writes it after gc start's bd init. Until then
// config.yaml alone classifies the scope proxied and pins the hosted database
// for that init, and the provider env carries the upstream endpoint that bd
// fronts with its local proxy.
func TestDoInitHostedProxiedExternalLeavesMetadataToBdInit(t *testing.T) {
	cityPath, prefix := initHostedCity(t, "proxied")

	cfgPath := filepath.Join(cityPath, ".beads", "config.yaml")
	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, cfgPath)
	if err != nil || !ok {
		t.Fatalf("ReadConfigState ok=%v err=%v", ok, err)
	}
	if state.EndpointOrigin != contract.EndpointOriginCityCanonical || state.EndpointStatus != contract.EndpointStatusUnverified || state.DoltMode != "proxied-server" {
		t.Fatalf("config.yaml origin=%q status=%q dolt.mode=%q, want city_canonical unverified proxied-server", state.EndpointOrigin, state.EndpointStatus, state.DoltMode)
	}
	if state.DoltHost != "gateway.example.com" || state.DoltPort != "4406" || state.DoltUser != "eia" {
		t.Fatalf("config.yaml dolt host/port/user = %q/%q/%q, want gateway.example.com/4406/eia", state.DoltHost, state.DoltPort, state.DoltUser)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("metadata.json stat err = %v, want absent until gc start runs bd init", err)
	}
	if db, ok, err := contract.ReadPinnedDoltDatabase(fsys.OSFS{}, cfgPath); err != nil || !ok || db != "bd_prj_abc" {
		t.Fatalf("config.yaml pinned dolt database = %q (ok=%v err=%v), want bd_prj_abc", db, ok, err)
	}
	if got := canonicalScopeDoltDatabase(cityPath, cityPath, prefix); got != "bd_prj_abc" {
		t.Fatalf("canonicalScopeDoltDatabase = %q, want the pinned bd_prj_abc for the deferred bd init", got)
	}
	if id, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, cityPath); err != nil || !ok || id != "prj_abc" {
		t.Fatalf("ReadProjectIdentity id=%q ok=%v err=%v, want prj_abc", id, ok, err)
	}
	if !scopeUsesProxiedDoltMode(cityPath, cityPath) {
		t.Fatal("scopeUsesProxiedDoltMode = false for a proxied-external hosted init")
	}
	env := runtimeEnvEntriesToMap(providerLifecycleProcessEnvFromBase(cityPath, "bd", nil))
	for key, want := range map[string]string{
		"BEADS_DOLT_PROXIED_SERVER":    "1",
		"GC_BEADS_PROXY_EXTERNAL_HOST": "gateway.example.com",
		"GC_BEADS_PROXY_EXTERNAL_PORT": "4406",
	} {
		if got := env[key]; got != want {
			t.Errorf("provider env %s = %q, want %q", key, got, want)
		}
	}
}

// An unverified external endpoint must not require a live connection at init
// time (R5): initDirIfReady writes the canonical files and defers the bd init
// to gc start (which has credentials) instead of running it now. A proxied
// scope fronting the endpoint defers the same way with the endpoint unverified
// for gc start, but its metadata.json stays absent: that file is the bd-init
// witness, and finalizeCanonicalBdScopeInit writes it after bd init.
func TestInitDirIfReadyDefersUnverifiedExternalDolt(t *testing.T) {
	for _, tc := range []struct {
		name, transport, wantMode string
		// wantMetadataMode is metadata.json's dolt_mode after the deferral;
		// empty means metadata.json must be absent.
		wantMetadataMode string
	}{
		{name: "direct", wantMode: "server", wantMetadataMode: "server"},
		{name: "proxied", transport: "proxied", wantMode: "proxied-server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, prefix := initHostedCity(t, tc.transport)

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
			state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
			if err != nil || !ok {
				t.Fatalf("ReadConfigState ok=%v err=%v", ok, err)
			}
			if state.EndpointStatus != contract.EndpointStatusUnverified || state.DoltMode != tc.wantMode {
				t.Fatalf("config.yaml endpoint status=%q dolt.mode=%q, want unverified %s", state.EndpointStatus, state.DoltMode, tc.wantMode)
			}
			metadataPath := filepath.Join(cityPath, ".beads", "metadata.json")
			if tc.wantMetadataMode == "" {
				if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
					t.Fatalf("metadata.json stat err = %v, want absent until gc start runs bd init", err)
				}
				return
			}
			mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, metadataPath)
			if err != nil || !ok || mode != tc.wantMetadataMode {
				t.Fatalf("metadata.json dolt_mode = %q (ok=%v err=%v), want %s to match config.yaml", mode, ok, err, tc.wantMetadataMode)
			}
		})
	}
}

// A brand-new proxied scope deferred under an unverified external endpoint
// must keep metadata.json absent: gc-beads-bd.sh takes that file as the RC's
// initialization witness, so seeding it can let gc start skip bd init.
func TestInitDirIfReadyDeferredProxiedRigLeavesMetadataAbsent(t *testing.T) {
	cityPath, _ := initHostedCity(t, "proxied")
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	orig := initDirIfReadyInitAndHookDir
	t.Cleanup(func() { initDirIfReadyInitAndHookDir = orig })
	called := false
	initDirIfReadyInitAndHookDir = func(_, _, _ string) error { called = true; return nil }

	deferred, err := initDirIfReady(cityPath, rigDir, "fe")
	if err != nil {
		t.Fatalf("initDirIfReady: %v", err)
	}
	if !deferred || called {
		t.Fatalf("initDirIfReady deferred=%v init-and-hook ran=%v, want the proxied rig deferred", deferred, called)
	}
	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "config.yaml"))
	if err != nil || !ok || state.DoltMode != "proxied-server" {
		t.Fatalf("rig config.yaml dolt.mode = %q (ok=%v err=%v), want proxied-server", state.DoltMode, ok, err)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads", "metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("rig metadata.json stat err = %v, want absent until gc start runs bd init", err)
	}
}

// gc start must run the bd init that gc init deferred for a hosted proxied
// city. gc-beads-bd.sh skips bd init once metadata.json exists and bd context
// answers from it, and the RC proxied initializer refuses an existing
// metadata.json, so neither gc init's writers nor its preflight seed may
// create one first. This drives gc init's deferral and then gc start's
// initAndHookDir through the real shim, with a fake bd modeling both RC
// behaviors, and checks that bd init fronts the hosted endpoint with the
// pinned database and that finalize writes metadata.json afterwards.
func TestInitAndHookDirRunsDeferredHostedProxiedBdInit(t *testing.T) {
	cityPath, prefix := initHostedCity(t, "proxied")
	cfg, err := loadInitProviderPreflightConfig(cityPath)
	if err != nil {
		t.Fatalf("loadInitProviderPreflightConfig: %v", err)
	}
	if err := seedDeferredManagedBeadsBeforeProviderReadiness(cityPath, cfg); err != nil {
		t.Fatalf("seedDeferredManagedBeadsBeforeProviderReadiness: %v", err)
	}
	if deferred, err := initDirIfReady(cityPath, cityPath, prefix); err != nil || !deferred {
		t.Fatalf("initDirIfReady deferred=%v err=%v, want the unverified hosted city deferred", deferred, err)
	}
	metadataPath := filepath.Join(cityPath, ".beads", "metadata.json")
	if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
		t.Errorf("metadata.json after gc init stat err = %v, want absent until gc start runs bd init", err)
	}

	for _, entry := range gcBeadsBdTestHomeEnv(t) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "bd-init-args")
	bdScript := `#!/bin/sh
case "$1" in
context)
  test -f "$BEADS_DIR/metadata.json"
  ;;
init)
  if [ -f "$BEADS_DIR/metadata.json" ]; then
    echo "bd init: $BEADS_DIR/metadata.json already exists" >&2
    exit 1
  fi
  printf '%s\n' "$*" > "` + argsFile + `"
  ;;
esac
`
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(bdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	gcPath := filepath.Join(binDir, "gc")
	if err := os.WriteFile(gcPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", bdPath)
	t.Setenv("GC_BIN", gcPath)
	setScopedBeadsProviderForTest(t, cityPath, "exec:"+filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))

	if err := initAndHookDir(cityPath, cityPath, prefix); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("gc start skipped the deferred bd init (read recorded bd init args: %v)", err)
	}
	want := "init --quiet --proxied-server --proxied-server-external-host gateway.example.com --proxied-server-external-port 4406 -p " + prefix + " --database bd_prj_abc --skip-hooks --skip-agents " + cityPath
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("bd init args = %q, want %q", got, want)
	}
	metaRaw, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata.json after gc start: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	for k, wantValue := range map[string]string{"dolt_mode": "proxied-server", "dolt_database": "bd_prj_abc", "project_id": "prj_abc"} {
		if got, _ := meta[k].(string); got != wantValue {
			t.Errorf("metadata.json[%q] = %q, want %q (full: %s)", k, got, wantValue, metaRaw)
		}
	}
}

// Under GC_DOLT=skip no bd init ever runs, so the canonical metadata.json
// stands in for the one a hosted proxied city's bd init would have written:
// gc init's preflight seed writes it, or gc start's normalize does when a live
// gc init left it to the deferred bd init. Either must bind the hosted
// database pinned in config.yaml, not the managed default.
func TestDoltSkipHostedProxiedCityMetadataBindsPinnedDatabase(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(cityPath string, cfg *config.City) error
	}{
		{name: "gc init preflight seed", write: seedDeferredManagedBeadsBeforeProviderReadiness},
		{name: "gc start normalize", write: func(cityPath string, cfg *config.City) error {
			return normalizeCanonicalBdScopeFiles(cityPath, cfg, io.Discard)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, _ := initHostedCity(t, "proxied")
			t.Setenv("GC_DOLT", "skip")
			cfg, err := loadInitProviderPreflightConfig(cityPath)
			if err != nil {
				t.Fatalf("loadInitProviderPreflightConfig: %v", err)
			}
			if err := tc.write(cityPath, cfg); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			metaRaw, err := os.ReadFile(filepath.Join(cityPath, ".beads", "metadata.json"))
			if err != nil {
				t.Fatalf("read metadata.json: %v", err)
			}
			var meta map[string]any
			if err := json.Unmarshal(metaRaw, &meta); err != nil {
				t.Fatalf("parse metadata.json: %v", err)
			}
			for k, want := range map[string]string{"dolt_mode": "proxied-server", "dolt_database": "bd_prj_abc", "project_id": "prj_abc"} {
				if got, _ := meta[k].(string); got != want {
					t.Errorf("metadata.json[%q] = %q, want %q (full: %s)", k, got, want, metaRaw)
				}
			}
		})
	}
}

// A verified external endpoint keeps the existing behavior: init-and-hook runs
// now (credentials are presumed available), so this change does not regress
// `gc rig add` against an already-validated external city, direct or proxied.
func TestInitDirIfReadyInitsVerifiedExternalDolt(t *testing.T) {
	for _, tc := range []struct{ name, transport string }{
		{name: "direct"},
		{name: "proxied", transport: "proxied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, prefix := initHostedCity(t, tc.transport)

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
		})
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

// `gc init --file` pins a hosted endpoint the way doInit and --from do. The
// --dolt-* flags stay mutually exclusive with --file, but the GC_DOLT_*
// environment fallback and the --beads-* selectors reach it, so the full
// canonical config must land before finalizeInit: the L1 identity, the
// city_canonical/unverified config.yaml, and the hosted database (pinned in
// config.yaml for a proxied scope, whose metadata.json stays bd init's witness,
// and in metadata.json for a direct one). Otherwise the deferred bd init binds
// the derived default database on the hosted server.
func TestGcInitCommandFileHostedDoltEnvEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		args         []string
		wantMode     string
	}{
		{name: "direct", wantMode: "server"},
		{name: "proxied selector", args: []string{"--beads-transport", "proxied", "--beads-target", "external"}, wantMode: "proxied-server"},
		{name: "proxied template", source: "\n[dolt]\nmode = \"proxied-server\"\n", wantMode: "proxied-server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, stderr, err := runGcInitFileWithHostedEnv(t, map[string]string{"GC_BEADS": "bd"}, tc.source, tc.args...)
			if err != nil {
				t.Fatalf("gc init --file with a hosted env endpoint = %v, want success; stderr=%s", err, stderr)
			}

			cfgPath := filepath.Join(cityPath, ".beads", "config.yaml")
			state, ok, err := contract.ReadConfigState(fsys.OSFS{}, cfgPath)
			if err != nil || !ok {
				t.Fatalf("ReadConfigState ok=%v err=%v", ok, err)
			}
			if state.EndpointOrigin != contract.EndpointOriginCityCanonical || state.EndpointStatus != contract.EndpointStatusUnverified || state.DoltMode != tc.wantMode {
				t.Fatalf("config.yaml origin=%q status=%q dolt.mode=%q, want city_canonical unverified %s", state.EndpointOrigin, state.EndpointStatus, state.DoltMode, tc.wantMode)
			}
			if state.DoltHost != "gateway.example.com" || state.DoltPort != "4406" || state.DoltUser != "eia" {
				t.Fatalf("config.yaml dolt host/port/user = %q/%q/%q, want gateway.example.com/4406/eia", state.DoltHost, state.DoltPort, state.DoltUser)
			}
			if id, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, cityPath); err != nil || !ok || id != "prj_x" {
				t.Fatalf("ReadProjectIdentity id=%q ok=%v err=%v, want prj_x", id, ok, err)
			}
			cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
			if err != nil {
				t.Fatalf("load city config: %v", err)
			}
			if got := canonicalScopeDoltDatabase(cityPath, cityPath, config.EffectiveHQPrefix(cfg)); got != "bd_prj_x" {
				t.Fatalf("canonicalScopeDoltDatabase = %q, want the hosted bd_prj_x, not the derived default", got)
			}

			metaPath := filepath.Join(cityPath, ".beads", "metadata.json")
			if tc.wantMode == "proxied-server" {
				if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
					t.Fatalf("metadata.json stat err = %v, want absent until gc start runs bd init", err)
				}
				if db, ok, err := contract.ReadPinnedDoltDatabase(fsys.OSFS{}, cfgPath); err != nil || !ok || db != "bd_prj_x" {
					t.Fatalf("config.yaml pinned dolt database = %q (ok=%v err=%v), want bd_prj_x", db, ok, err)
				}
				return
			}
			metaRaw, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatalf("read metadata.json: %v", err)
			}
			var meta map[string]any
			if err := json.Unmarshal(metaRaw, &meta); err != nil {
				t.Fatalf("parse metadata.json: %v", err)
			}
			for k, want := range map[string]string{"backend": "dolt", "dolt_mode": "server", "dolt_database": "bd_prj_x", "project_id": "prj_x"} {
				if got, _ := meta[k].(string); got != want {
					t.Fatalf("metadata.json[%q] = %q, want %q (full: %s)", k, got, want, metaRaw)
				}
			}
		})
	}
}

// A hosted endpoint reaching `gc init --file` through the environment is held
// to the same backend contract as doInit's: a file or doltlite effective
// backend fails the command before any canonical ledger file is written.
func TestGcInitCommandFileHostedDoltRejectsIncompatibleBackend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "file", env: map[string]string{"GC_BEADS": "file"}, wantErr: "bd-backed"},
		{name: "doltlite", env: map[string]string{"GC_BEADS_BACKEND": "doltlite"}, wantErr: "doltlite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, stderr, err := runGcInitFileWithHostedEnv(t, tc.env, "")
			if err == nil {
				t.Fatalf("gc init --file with a hosted env endpoint on a %s backend = nil error, want failure; stderr=%s", tc.name, stderr)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Fatalf("stderr = %q, want a %q backend error", stderr, tc.wantErr)
			}
			assertNoHostedDoltStoreArtifacts(t, cityPath)
		})
	}
}

// A script or agent running `gc init --file` inside a city inherits the
// GC_DOLT_HOST/PORT/USER that gc projects into its sessions. With no database,
// project id, external target, or --dolt-* flag, that ambient endpoint is the
// parent city's connection, not an init request: init must succeed and pin no
// external endpoint, whichever backend the new city uses.
func TestGcInitCommandFileIgnoresProjectedDoltEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{name: "file backend", source: "\n[beads]\nprovider = \"file\"\n"},
		{name: "default backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The default backend's live bd init is not under test here.
			orig := initDirIfReadyInitAndHookDir
			t.Cleanup(func() { initDirIfReadyInitAndHookDir = orig })
			initDirIfReadyInitAndHookDir = func(_, _, _ string) error { return nil }

			cityPath, stderr, err := runGcInitFileWithHostedEnv(t, map[string]string{
				"GC_DOLT":         "skip",
				envDoltHost:       "127.0.0.1",
				envDoltPort:       "3307",
				envDoltUser:       "root",
				envDoltDatabase:   "",
				envBeadsProjectID: "",
			}, tc.source)
			if err != nil {
				t.Fatalf("gc init --file under a session-projected dolt endpoint = %v, want success; stderr=%s", err, stderr)
			}

			if cityExternalDoltEndpointUnverified(cityPath) {
				t.Fatal("gc init --file pinned the session-projected endpoint as an unverified external endpoint")
			}
			state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
			if err != nil {
				t.Fatalf("ReadConfigState: %v", err)
			}
			if ok && (state.EndpointOrigin == contract.EndpointOriginCityCanonical || state.DoltHost != "") {
				t.Fatalf("config.yaml origin=%q dolt.host=%q, want no city_canonical external endpoint", state.EndpointOrigin, state.DoltHost)
			}
			cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
			if err != nil {
				t.Fatalf("load city config: %v", err)
			}
			if cfg.Dolt.Host != "" || cfg.Dolt.Port != 0 {
				t.Fatalf("city.toml [dolt] host=%q port=%d, want no pinned endpoint", cfg.Dolt.Host, cfg.Dolt.Port)
			}
		})
	}
}

// runGcInitFileWithHostedEnv drives the real `gc init --file` command with a
// hosted endpoint supplied through the GC_DOLT_*/GC_BEADS_PROJECT_ID
// environment fallback. env is applied over the hosted values, sourceTOML is
// appended to the --file city config, and args are extra init flags.
func runGcInitFileWithHostedEnv(t *testing.T, env map[string]string, sourceTOML string, args ...string) (cityPath, stderr string, err error) {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BOOTSTRAP", "skip")
	t.Setenv(envDoltHost, "gateway.example.com")
	t.Setenv(envDoltPort, "4406")
	t.Setenv(envDoltUser, "eia")
	t.Setenv(envDoltDatabase, "bd_prj_x")
	t.Setenv(envBeadsProjectID, "prj_x")
	for k, v := range env {
		t.Setenv(k, v)
	}
	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{"user.name": "ci", "user.email": "ci@example.com"})

	src := filepath.Join(t.TempDir(), "hosted-city.toml")
	if err := os.WriteFile(src, []byte("[workspace]\nname = \"placeholder\"\n"+sourceTOML), 0o644); err != nil {
		t.Fatalf("write --file source: %v", err)
	}
	cityPath = filepath.Join(t.TempDir(), "file-hosted-city")
	var stdout, stderrBuf bytes.Buffer
	cmd := newInitCmd(&stdout, &stderrBuf)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append(append([]string{"--file", src}, args...), "--skip-provider-readiness", "--no-start", cityPath))
	err = cmd.Execute()
	return cityPath, stderrBuf.String(), err
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
