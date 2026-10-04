package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeMigratedProxiedCity reproduces the on-disk shape
// `gc beads city migrate-proxied` leaves behind: bd's metadata binds the scope
// to proxied-server, bd's proxy root exists under <city>/.beads/dolt, the
// canonical config still records gc.endpoint_origin: managed_city (the city is
// still what its rigs inherit from), and gc's own dolt-config.yaml was retired
// on purpose because gc will never start a server for this scope again.
func writeMigratedProxiedCity(t *testing.T) string {
	t.Helper()
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"migrated\"\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(city, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(beadsDir, "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(beadsDir, "config.yaml"), contract.ConfigState{
		IssuePrefix:    "hq",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	}); err != nil {
		t.Fatal(err)
	}
	return city
}

// TestMigratedProxiedCityIsNotManagedLocal pins the classification behind
// doctor's managed-Dolt checks.
//
// A migrated city carries managed_city in its canonical config and a .beads/dolt
// directory that is bd's proxy root, so the gate read it as managed-local and
// DoltConfigCheck then warned that the dolt-config.yaml migrate-proxied
// deliberately retired was "not found" — with a fix hint (gc start / gc dolt
// restart) that is a typed no-op on a proxied scope. Every operator who followed
// the documented migration ended with a warning nothing could clear.
func TestMigratedProxiedCityIsNotManagedLocal(t *testing.T) {
	city := writeMigratedProxiedCity(t)

	if managedLocalDoltChecksApplicable(city) {
		t.Fatal("a migrated proxied city classified as managed-local")
	}
	result := NewDoltConfigCheck(city, false).Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("dolt-config on a migrated proxied city = %v (%q), want ok", result.Status, result.Message)
	}
	if result.FixHint != "" {
		t.Fatalf("dolt-config emitted guidance for a scope gc does not manage: %q", result.FixHint)
	}
}

// A legacy GC-managed city must keep its managed-Dolt checks: the skip is for
// bd-owned proxied scopes, not for every managed_city config.
func TestLegacyManagedCityKeepsManagedLocalDoltChecks(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"legacy\"\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beadsDir := filepath.Join(city, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(beadsDir, "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(beadsDir, "config.yaml"), contract.ConfigState{
		IssuePrefix:    "hq",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	}); err != nil {
		t.Fatal(err)
	}

	if !managedLocalDoltChecksApplicable(city) {
		t.Fatal("a legacy GC-managed city stopped running its managed-Dolt checks")
	}
}
