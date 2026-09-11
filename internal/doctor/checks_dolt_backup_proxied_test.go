package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// Every healthy proxied rig used to collect a `rig:<name>:dolt-backup` warning
// whose fix hint prescribed a managed-Dolt `dolt backup` invocation against a
// server gc does not own. Neither signal the check looks for can ever exist on
// a bd-owned proxy root — gc writes no <city>/.dolt-backup for it, and rc.2
// refuses `bd backup` on the proxied path outright.
func TestDoltBackupCheckReportsNotRequiredOnProxiedRig(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result := NewDoltBackupCheck(city, config.Rig{Name: "r1", Path: rig}, "").Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v (%q), want OK on a bd-owned proxied rig", result.Status, result.Message)
	}
	if result.FixHint != "" {
		t.Errorf("proxied rig got managed-Dolt backup guidance: %q", result.FixHint)
	}
	if !strings.Contains(result.Message, "proxied") {
		t.Errorf("message does not say why the check does not apply: %q", result.Message)
	}
}

// The same is true of a bd-owned DIRECT rig, which the transport selector
// produces: its Dolt repository lives under bd's root, gc writes no
// <city>/.dolt-backup for it, and the fix hint would prescribe a `dolt backup`
// against a server gc does not own. Only the transport differs from the
// proxied case; the ownership — the thing the check is actually about — is the
// same, and it is the city's ownership journal that records it.
func TestDoltBackupCheckReportsNotRequiredOnBdOwnedDirectRig(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDoctorOwnershipJournal(t, city, readyOwnershipJournal(t, "rig:r1", rig))

	result := NewDoltBackupCheck(city, config.Rig{Name: "r1", Path: rig}, "").Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v (%q), want OK on a bd-owned direct rig", result.Status, result.Message)
	}
	if result.FixHint != "" {
		t.Errorf("bd-owned direct rig got managed-Dolt backup guidance: %q", result.FixHint)
	}
	if !strings.Contains(result.Message, "bd-owned") {
		t.Errorf("message does not say why the check does not apply: %q", result.Message)
	}
}

// A legacy direct rig — one with no ownership record, whose Dolt is gc's own
// managed server — still warns: the check must not go quiet for every rig.
func TestDoltBackupCheckStillWarnsOnDirectRig(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result := NewDoltBackupCheck(city, config.Rig{Name: "r1", Path: rig}, "").Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v (%q), want a warning for an unregistered direct rig", result.Status, result.Message)
	}
}
