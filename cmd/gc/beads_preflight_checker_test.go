package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
)

// TestNewBeadsPreflightCheckerWiresDatabaseSchemaCursors pins the
// constructor's wiring: newBeadsPreflightChecker, the production
// contract.PreflightChecker constructor, must wire a non-nil
// DatabaseSchemaCursors reader. A mutation that un-wires that field (deletes
// its line from the constructor, or wires it to nil) must fail this test — the
// resulting checker would silently skip the schema gate for every city it
// composes, exactly as if the database could never answer.
func TestNewBeadsPreflightCheckerWiresDatabaseSchemaCursors(t *testing.T) {
	checker := newBeadsPreflightChecker("/city", "", nil)

	if checker.DatabaseSchemaCursors == nil {
		t.Fatal("newBeadsPreflightChecker left DatabaseSchemaCursors nil: the schema gate would never run")
	}
	if checker.DatabaseProjectID == nil {
		t.Fatal("newBeadsPreflightChecker left DatabaseProjectID nil")
	}
	if checker.DeferIdentityToNativeOpen == nil {
		t.Fatal("newBeadsPreflightChecker left DeferIdentityToNativeOpen nil")
	}
	if checker.AllowSchemaBehindMigrate == nil {
		t.Fatal("newBeadsPreflightChecker left AllowSchemaBehindMigrate nil")
	}
	if checker.SchemaLatestIgnoredVersion != beads.SchemaCursorIgnored {
		t.Fatalf("SchemaLatestIgnoredVersion = %d, want beads.SchemaCursorIgnored (%d)", checker.SchemaLatestIgnoredVersion, beads.SchemaCursorIgnored)
	}
	if checker.BDContext == nil {
		t.Fatal("newBeadsPreflightChecker left BDContext nil")
	}
}

// TestPreflightSchemaCursorsFromReportUsesTheClampedIgnoredValue pins the
// ignored-lane mapping: preflightSchemaCursorsFromReport (and so
// preflightDatabaseSchemaCursorsReader, which delegates to it) must report the
// CLAMPED ignored-lane cursor (CursorReality.EffectiveIgnored), not the raw
// on-disk counter, whenever the reality is Limited and its Floor sits below
// the raw value. A mutation that compares/returns the raw
// report.Cursors.Ignored instead must fail this test.
func TestPreflightSchemaCursorsFromReportUsesTheClampedIgnoredValue(t *testing.T) {
	const rawIgnored = 42
	const floor = 7

	report := proxyendpoint.CursorReportForTest(
		proxyendpoint.Cursors{Main: 5, Ignored: rawIgnored},
		proxyendpoint.CursorReality{Limited: true, Floor: floor, Missing: "dolt_ignore"},
	)

	got := preflightSchemaCursorsFromReport(report)

	if got.Main != 5 {
		t.Errorf("Main = %d, want 5", got.Main)
	}
	if got.Ignored != floor {
		t.Errorf("Ignored = %d, want the clamped floor %d (raw was %d): a schema gate that reads the raw counter trusts a cursor the live schema does not corroborate", got.Ignored, floor, rawIgnored)
	}
	if !got.IgnoredChecked {
		t.Error("IgnoredChecked = false, want true: CursorReportForTest marks the reality checked")
	}
}

// TestPreflightSchemaCursorsFromReportTrustsAnUnlimitedRealityAsIs guards the
// other half of EffectiveIgnored's contract: when nothing was missing
// (Limited=false), the raw counter IS the believed one, so clamping must be a
// no-op rather than always substituting Floor.
func TestPreflightSchemaCursorsFromReportTrustsAnUnlimitedRealityAsIs(t *testing.T) {
	const rawIgnored = 42

	report := proxyendpoint.CursorReportForTest(
		proxyendpoint.Cursors{Main: 5, Ignored: rawIgnored},
		proxyendpoint.CursorReality{},
	)

	got := preflightSchemaCursorsFromReport(report)

	if got.Ignored != rawIgnored {
		t.Errorf("Ignored = %d, want the raw value %d unchanged: nothing was missing", got.Ignored, rawIgnored)
	}
}

// writeAllowSchemaBehindMigrateCity writes a city with one rig registered
// outside the city's directory tree, the shape in which a store's scope root
// is not the city path. extraTOML is placed between the city.toml's
// [workspace] header and its [[rigs]] entry.
func writeAllowSchemaBehindMigrateCity(t *testing.T, extraTOML string) (cityDir, rigDir string) {
	t.Helper()
	cityDir = t.TempDir()
	rigDir = filepath.Join(t.TempDir(), "my-rig")
	for _, scope := range []struct{ dir, config string }{
		{dir: cityDir, config: "issue_prefix: gc\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.host: city-db.example.com\ndolt.port: 3307\n"},
		{dir: rigDir, config: "issue_prefix: myrig\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: rig-db.example.com\ndolt.port: 4407\n"},
	} {
		if err := os.MkdirAll(filepath.Join(scope.dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scope.dir, ".beads", "config.yaml"), []byte(scope.config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cityTOML := "[workspace]\nname = \"demo\"\n\n" + extraTOML + "\n[[rigs]]\nname = \"my-rig\"\npath = \"" + rigDir + "\"\nprefix = \"mr\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir, rigDir
}

// TestNativeOpenEnvCarriesTheCityAllowSchemaBehindMigrateDecision pins the
// beads.allow_schema_behind_migrate opt-in to one answer across its two
// readers, the native-store preflight and the native open env, for the city's
// own scope and for a rig outside the city's directory tree. A behind schema
// the preflight passes on the opt-in must open with BD_ALLOW_REMOTE_MIGRATE
// set, or the linked library refuses the migration the preflight admitted;
// without the opt-in the open must withhold it, whatever the ambient or
// workspace environment carries.
func TestNativeOpenEnvCarriesTheCityAllowSchemaBehindMigrateDecision(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extraTOML string
		// configInHand passes the loaded city config to both readers, as a
		// store open does; otherwise they get nil and read it themselves, as
		// the native reopen hook does.
		configInHand bool
		ambient      string
		want         bool
	}{
		{name: "config opt-in, config in hand", extraTOML: "[beads]\nallow_schema_behind_migrate = true\n", configInHand: true, want: true},
		{name: "config opt-in, config reloaded", extraTOML: "[beads]\nallow_schema_behind_migrate = true\n", want: true},
		{name: "workspace env break-glass", extraTOML: "[workspace.env]\nGC_BEADS_ALLOW_SCHEMA_BEHIND_MIGRATE = \"1\"\n", want: true},
		{name: "no opt-in withholds an inherited unlock", extraTOML: "[workspace.env]\nBD_ALLOW_REMOTE_MIGRATE = \"1\"\n", ambient: "1", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ambient == "" {
				unsetTestEnv(t, "BD_ALLOW_REMOTE_MIGRATE")
			} else {
				t.Setenv("BD_ALLOW_REMOTE_MIGRATE", tc.ambient)
			}
			cityDir, rigDir := writeAllowSchemaBehindMigrateCity(t, tc.extraTOML)
			var cfg *config.City
			if tc.configInHand {
				loaded, err := loadCityConfig(cityDir, io.Discard)
				if err != nil {
					t.Fatalf("loadCityConfig: %v", err)
				}
				cfg = loaded
			}
			for _, scope := range []struct{ name, root string }{
				{name: "city scope", root: cityDir},
				{name: "out-of-tree rig scope", root: rigDir},
			} {
				if got := newBeadsPreflightChecker(cityDir, "", cfg).AllowSchemaBehindMigrate(scope.root); got != tc.want {
					t.Errorf("%s: preflight AllowSchemaBehindMigrate = %v, want %v", scope.name, got, tc.want)
				}
				env, err := nativeDoltOpenEnvForScope(cityDir, cfg, scope.root)
				if err != nil {
					t.Fatalf("%s: nativeDoltOpenEnvForScope: %v", scope.name, err)
				}
				got, set := env["BD_ALLOW_REMOTE_MIGRATE"]
				if tc.want && got != "1" {
					t.Errorf("%s: open env BD_ALLOW_REMOTE_MIGRATE = %q (set=%v), want %q: the open must honor the opt-in the preflight admitted", scope.name, got, set, "1")
				}
				if !tc.want && set {
					t.Errorf("%s: open env carries BD_ALLOW_REMOTE_MIGRATE = %q, want it absent: without the city's opt-in the open must withhold the unlock", scope.name, got)
				}
			}
		})
	}
}

// captureAllowSchemaBehindMigrateWarnings redirects the unresolved-opt-in
// warning to a buffer and resets its per-process dedupe so each test sees its
// own line.
func captureAllowSchemaBehindMigrateWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := allowSchemaBehindMigrateWarnOut
	allowSchemaBehindMigrateWarnOut = &buf
	allowSchemaBehindMigrateWarned.Clear()
	t.Cleanup(func() {
		allowSchemaBehindMigrateWarnOut = old
		allowSchemaBehindMigrateWarned.Clear()
	})
	return &buf
}

// TestCityAllowSchemaBehindMigrateWithoutConfigInHand pins how the opt-in
// resolves when the caller has no config in hand. An unreadable city.toml, or
// one naming an include that is missing, leaves the opt-in off and says so
// once per city and error, however many opens ask; only a city with no
// city.toml has no config opt-in and no warning, and it still honors the
// break-glass override from the environment.
func TestCityAllowSchemaBehindMigrateWithoutConfigInHand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cityTOML string // "" writes no city.toml
		ambient  string // GC_BEADS_ALLOW_SCHEMA_BEHIND_MIGRATE; "" leaves it unset
		want     bool
		warns    bool
	}{
		{name: "unreadable city.toml", cityTOML: "[workspace\n", warns: true},
		{
			name:     "city.toml opts in but its include is missing",
			cityTOML: "include = [\"missing.toml\"]\n\n[workspace]\nname = \"test\"\n\n[beads]\nallow_schema_behind_migrate = true\n",
			warns:    true,
		},
		{name: "no city.toml"},
		{name: "no city.toml, break-glass override", ambient: "1", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureAllowSchemaBehindMigrateWarnings(t)
			if tc.ambient != "" {
				t.Setenv("GC_BEADS_ALLOW_SCHEMA_BEHIND_MIGRATE", tc.ambient)
			}
			cityDir := t.TempDir()
			if tc.cityTOML != "" {
				if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(tc.cityTOML), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			checker := newBeadsPreflightChecker(cityDir, "", nil)
			for call := 1; call <= 2; call++ {
				if got := checker.AllowSchemaBehindMigrate(cityDir); got != tc.want {
					t.Errorf("call %d: AllowSchemaBehindMigrate = %v, want %v", call, got, tc.want)
				}
			}
			line := "gc: warning: resolving beads.allow_schema_behind_migrate for " + cityDir + ": "
			wantLines := 0
			if tc.warns {
				wantLines = 1
			}
			if got := strings.Count(warnings.String(), line); got != wantLines {
				t.Errorf("warning %q printed %d times, want %d; output:\n%s", line, got, wantLines, warnings.String())
			}
		})
	}
}

// TestPreflightIdentityProbeSkipNarrowerThanDeferral locks the split between
// the two identity gates (gastownhall/gascity#5965). Both a loopback and a
// remote external endpoint defer an unconfirmed probe to native-open
// verification, but only the remote one — a hosted beads-gateway or hub the
// root/plaintext probe cannot authenticate — skips the probe outright. A
// loopback external endpoint speaks plaintext, so its probe still runs and a
// genuine project_id mismatch still blocks native activation.
func TestPreflightIdentityProbeSkipNarrowerThanDeferral(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		wantDefer bool
		wantSkip  bool
	}{
		{name: "loopback external", host: "127.0.0.1", wantDefer: true, wantSkip: false},
		{name: "remote external", host: "gw.beads.example", wantDefer: true, wantSkip: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeCanonicalScopeConfig(t, cityPath, contract.ConfigState{
				IssuePrefix:    "gc",
				EndpointOrigin: contract.EndpointOriginCityCanonical,
				EndpointStatus: contract.EndpointStatusVerified,
				DoltHost:       tc.host,
				DoltPort:       "3307",
			})
			target, ok, err := canonicalScopeDoltTarget(cityPath, cityPath)
			if err != nil || !ok {
				t.Fatalf("canonicalScopeDoltTarget() = ok %v, err %v; want an authoritative target", ok, err)
			}
			if !target.External {
				t.Fatalf("target.External = false for %s; fixture must resolve an external endpoint", tc.host)
			}

			if got := preflightIdentityDeferredReader(cityPath)(cityPath); got != tc.wantDefer {
				t.Errorf("preflightIdentityDeferredReader(%s) = %v, want %v", tc.host, got, tc.wantDefer)
			}
			if got := preflightIdentityProbeSkipReader(cityPath)(cityPath); got != tc.wantSkip {
				t.Errorf("preflightIdentityProbeSkipReader(%s) = %v, want %v", tc.host, got, tc.wantSkip)
			}
		})
	}
}
