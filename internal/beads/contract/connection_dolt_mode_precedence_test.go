package contract

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A scope migrated with `bd migrate from-server-to-proxied-server` keeps gc's
// legacy `dolt.mode: server` in config.yaml — bd rewrites metadata.json and
// nothing else. metadata.json is the topology authority (D1), so the stale
// config key must not shadow it and send resolution down the managed-runtime
// path (which then fails on the absent dolt-state.json).
func TestResolveDoltConnectionTargetMetadataModeBeatsStaleConfigMode(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "ci",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltMode:       "server",
	})
	writeMetadataWithMode(t, fs, city, "hq", "proxied-server")

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if !strings.EqualFold(target.DoltMode, "proxied-server") {
		t.Fatalf("DoltMode = %q, want proxied-server (target = %+v)", target.DoltMode, target)
	}
	if target.Host != "" || target.Port != "" {
		t.Fatalf("proxied target must not carry a managed endpoint: %+v", target)
	}
}

// The same stale key on an inherited rig must not drag the rig back onto the
// city's managed runtime port.
func TestResolveDoltConnectionTargetInheritedRigMetadataModeBeatsStaleConfigMode(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "ci",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltMode:       "server",
	})
	writeMetadataWithMode(t, fs, city, "hq", "proxied-server")

	rig := filepath.Join(city, "spike")
	writeCanonicalConfig(t, fs, rig, ConfigState{
		IssuePrefix:    "sp",
		EndpointOrigin: EndpointOriginInheritedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltMode:       "server",
	})
	writeMetadataWithMode(t, fs, rig, "sp", "proxied-server")

	target, err := ResolveDoltConnectionTarget(fs, city, rig)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if !strings.EqualFold(target.DoltMode, "proxied-server") {
		t.Fatalf("DoltMode = %q, want proxied-server (target = %+v)", target.DoltMode, target)
	}
	if target.Host != "" || target.Port != "" {
		t.Fatalf("proxied rig target must not carry a managed endpoint: %+v", target)
	}
}

// config.yaml stays a legacy fallback: a scope whose metadata predates
// dolt_mode still reads its mode from the config key.
func TestResolveDoltConnectionTargetFallsBackToConfigModeWithoutMetadataMode(t *testing.T) {
	fs := fsys.OSFS{}
	city := t.TempDir()
	writeCanonicalConfig(t, fs, city, ConfigState{
		IssuePrefix:    "ci",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
		DoltMode:       "server",
	})
	writeMetadataWithMode(t, fs, city, "hq", "")
	port := writeReachableRuntimeState(t, fs, city)

	target, err := ResolveDoltConnectionTarget(fs, city, city)
	if err != nil {
		t.Fatalf("ResolveDoltConnectionTarget() error = %v", err)
	}
	if target.DoltMode != "server" {
		t.Fatalf("DoltMode = %q, want server", target.DoltMode)
	}
	if target.Port != port {
		t.Fatalf("target = %+v, want managed port %s", target, port)
	}
}

// The raw-line fallback (malformed YAML) must drop the key too, or a repair
// write silently reinstates the shadowing value.
func TestEnsureCanonicalConfigFallbackDropsDoltModeWhenStateHasNone(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	beads := filepath.Join(dir, ".beads")
	if err := fs.MkdirAll(beads, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beads, "config.yaml")
	// A tab indent is not legal YAML, so this routes through
	// ensureCanonicalConfigFallback's raw-line rewriter.
	raw := "issue_prefix: ci\ndolt.mode: server\nbroken:\n\t- tabbed\n"
	if err := fs.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "ci",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	}); err != nil {
		t.Fatalf("EnsureCanonicalConfig() error = %v", err)
	}
	if got := readFileString(t, path); strings.Contains(got, "dolt.mode:") {
		t.Fatalf("dolt.mode survived the fallback write:\n%s", got)
	}
}

//nolint:unparam // helper keeps the FS explicit, matching its siblings in this package
func writeMetadataWithMode(t *testing.T, fs fsys.FS, dir, db, mode string) {
	t.Helper()
	if err := fs.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCanonicalMetadata(fs, filepath.Join(dir, ".beads", "metadata.json"), MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     mode,
		DoltDatabase: db,
	}); err != nil {
		t.Fatal(err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := fsys.OSFS{}.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
