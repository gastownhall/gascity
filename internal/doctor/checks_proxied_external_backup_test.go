package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeProxiedExternalScope builds the M4 shape: bd's metadata binds the scope
// to the proxied path and its sidecar records the external upstream the proxy
// fronts. The local .beads/dolt holds no data — gc's own init code calls it
// "scaffolding bd recreates on demand".
func writeProxiedExternalScope(t *testing.T, scopeRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "proxied_server_client_info.json"),
		[]byte(`{"external":{"host":"db.example","port":4406}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The advisory told M4 operators that the local proxy root is the only copy of
// data that lives on their external server, and to copy a directory holding
// nothing. Both claims key off metadata alone, which cannot tell M1 from M4;
// the sidecar's external block can.
func TestProxiedBackupCoverageAdvisorySkipsAnExternalUpstream(t *testing.T) {
	city := t.TempDir()
	writeProxiedExternalScope(t, city)

	if check := NewProxiedBackupCoverageCheckForConfig(city, nil, errors.New("no city.toml")); check != nil {
		t.Fatalf("a proxied-external city got the local-store advisory: %q", check.Run(&CheckContext{}).Message)
	}
}

// A city with both shapes still gets the advisory, naming only the scope whose
// data is actually here: suppressing the line for a mixed city would lose a real
// exposure.
func TestProxiedBackupCoverageAdvisoryNamesOnlyLocallyStoredScopes(t *testing.T) {
	city := t.TempDir()
	local := filepath.Join(city, "rigs", "local")
	external := filepath.Join(city, "rigs", "hosted")
	writeProxiedExternalScope(t, city)
	writeProxiedExternalScope(t, external)
	if err := os.MkdirAll(filepath.Join(local, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"lo"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	check := NewProxiedBackupCoverageCheckForConfig(city, nil, errors.New("no city.toml"))
	if check == nil {
		t.Fatal("no advisory registered for a city with a locally stored proxied rig")
	}
	message := check.Run(&CheckContext{}).Message
	if !strings.Contains(message, filepath.Join("rigs", "local")) {
		t.Errorf("advisory does not name the locally stored scope: %q", message)
	}
	for _, unwanted := range []string{filepath.Join("rigs", "hosted"), "(city"} {
		if strings.Contains(message, unwanted) {
			t.Errorf("advisory claims %q has no other copy, but its data is on an external server: %q", unwanted, message)
		}
	}
	if !strings.Contains(message, "1 bd-owned proxied scope") {
		t.Errorf("advisory counts external-upstream scopes as unbacked local stores: %q", message)
	}
}

// The per-rig message has the same defect from the other side: the
// provider-owned branch returns before the check's external-endpoint branch, so
// an M4 rig was told no backup of it exists anywhere rather than that its
// endpoint owns them — which is what the direct-external branch says for the
// same fact.
func TestDoltBackupCheckReportsSelfManagedBackupsOnAProxiedExternalRig(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	writeProxiedExternalScope(t, rig)

	result := NewDoltBackupCheck(city, config.Rig{Name: "r1", Path: rig}, "").Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v (%q), want OK on a proxied-external rig", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, externalUpstreamBackupNote) {
		t.Errorf("message does not defer backups to the endpoint: %q", result.Message)
	}
	if strings.Contains(result.Message, "no gc or bd backup exists") {
		t.Errorf("message claims no copy exists of data held on an external server: %q", result.Message)
	}
}

// Nothing above may soften the M1 case: a proxied scope whose sidecar records
// only proxy lifecycle fields does keep its beads under the local root, and that
// root really is the only copy.
func TestProxiedLocalScopeKeepsTheOnlyCopyWording(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "proxied_server_client_info.json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"port":3307}`, os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	result := NewDoltBackupCheck(city, config.Rig{Name: "r1", Path: rig}, "").Run(&CheckContext{})
	if !strings.Contains(result.Message, "no gc or bd backup exists") {
		t.Fatalf("a proxied-local rig lost the gap it really has: %q", result.Message)
	}
}
