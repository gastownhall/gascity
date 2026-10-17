package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestRigStoreBindingCheckProxiedLocalRigIsNotQueried pins guard (b): a
// managed proxied target resolves with an empty Port and its server belongs
// to bd, so the check stands down rather than failing strconv.Atoi on the
// empty port or starting a stopped store to open a connection. The shape is
// main's writeProxiedLocalScope fixture, the one RigDoltServerCheck reports
// "not required" for.
func TestRigStoreBindingCheckProxiedLocalRigIsNotQueried(t *testing.T) {
	clearInheritedBeadsEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	buildProxiedLocalRig(t, cityDir, rigDir)

	r := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not checked (bd backend=dolt proxied-server") {
		t.Errorf("message = %q, want the proxied-server skip", r.Message)
	}
}

// TestRigStoreBindingCheckInheritedRigWithoutRuntimeIsOK pins guard (c): a
// rig that inherits the city endpoint while the city has no gc-managed
// runtime has no endpoint to query, which is not a store failure.
func TestRigStoreBindingCheckInheritedRigWithoutRuntimeIsOK(t *testing.T) {
	clearInheritedBeadsEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "inherits city dolt endpoint") {
		t.Errorf("message = %q, want the inherited-endpoint skip", r.Message)
	}
}

// TestRigStoreBindingCheckPendingScopeInitializationWarns pins guard (d): a
// rig whose provider-owned init has not finished has no database to find yet,
// so the check reports the shared pending warning that RigDoltServerCheck
// reports for the same scope, ahead of any endpoint resolution.
func TestRigStoreBindingCheckPendingScopeInitializationWarns(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoctorOwnershipJournal(t, cityDir, pendingOwnershipJournal(t, "rig:demo", rigDir))

	r := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "beads scope initialisation pending") {
		t.Errorf("message = %q, want the pending-initialisation message", r.Message)
	}
}

// TestRigStoreBindingCheckStillFiresOnAResolvableUnreachableEndpoint keeps the
// guards from turning into a blanket skip: a rig whose endpoint resolves to a
// host and port but answers nothing is the 2026-08-10 shape the check exists
// to catch, so it stays an error.
func TestRigStoreBindingCheckStillFiresOnAResolvableUnreachableEndpoint(t *testing.T) {
	clearInheritedBeadsEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	buildDirectLocalRig(t, cityDir, rigDir)

	r := NewRigStoreBindingCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false).Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
}
