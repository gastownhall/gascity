package doctor

import (
	"errors"
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
	// "not gc's to register" on its own implies somebody else registers it.
	// Nobody does on rc.2, and no other check says so, so this message has to.
	if !strings.Contains(result.Message, "no gc or bd backup exists") {
		t.Errorf("message reads as coverage rather than naming the gap: %q", result.Message)
	}
	if !strings.Contains(result.Message, "1.3.0-rc.2") {
		t.Errorf("message does not name the bd version that refuses backup: %q", result.Message)
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

// One city-level advisory carries the fact the per-scope checks cannot: a
// proxied city has no backup at all. It is StatusOK because a proxied city is
// a healthy city on this branch (R3) and rc.2 offers the operator no action —
// a warning would be a permanent red line — but it is an advisory that names
// every proxied scope, not another line that reads as coverage.
func TestProxiedBackupCoverageAdvisoryNamesEveryProxiedScope(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	for _, scope := range []string{city, rig} {
		if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"),
			[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"d"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	check := NewProxiedBackupCoverageCheckForConfig(city, nil, errors.New("no city.toml"))
	if check == nil {
		t.Fatal("no advisory registered for a city with two proxied scopes")
	}
	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v (%q), want OK", result.Status, result.Message)
	}
	if result.Severity != SeverityAdvisory {
		t.Errorf("severity = %v, want advisory", result.Severity)
	}
	if result.FixHint != "" {
		t.Errorf("advisory prescribes a fix that does not exist on rc.2: %q", result.FixHint)
	}
	for _, want := range []string{"city", filepath.Join("rigs", "r1"), "no backup", "1.3.0-rc.2"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q does not mention %q", result.Message, want)
		}
	}
}

// A direct or external city has no proxied gap, so it gets no line at all —
// an advisory that fires everywhere is noise, and doctor output nobody reads
// is the failure mode this whole check exists to avoid.
func TestProxiedBackupCoverageAdvisoryIsNotRegisteredForADirectCity(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if check := NewProxiedBackupCoverageCheckForConfig(city, nil, errors.New("no city.toml")); check != nil {
		t.Fatalf("a direct city got the proxied advisory: %q", check.Run(&CheckContext{}).Message)
	}
}
