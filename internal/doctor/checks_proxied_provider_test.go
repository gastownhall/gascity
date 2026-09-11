package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func writeDoctorOwnershipJournal(t *testing.T, cityPath, body string) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scope-ownership.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pendingOwnershipJournal(t *testing.T, key, scopePath string) string {
	t.Helper()
	return `{"version":1,"scopes":{"` + key + `":{"scope_path":"` + scopePath +
		`","lifecycle_owner":"provider","state":"provider_initializing",` +
		`"intent":{"transport":"proxied","target":"local"}}}}`
}

// TestBeadsStoreCheck_ProxiedProviderGateIsOK covers R3: the bd-owned proxied
// store is the intended selection for a proxied scope, so the BdStore
// diagnostic must not read as a degraded native-store fallback.
func TestBeadsStoreCheck_ProxiedProviderGateIsOK(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n\n[beads]\nprovider = \"file\"\n")
	spy := &spyPingStore{pingFunc: func() error { return nil }}
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{
			Store: spy,
			Diagnostic: beads.BeadsDiagnostic{
				Store:               beads.BeadsStoreNameBdStore,
				NativeStoreEligible: false,
				PreflightGate:       beads.BeadsGateProxiedProvider,
				PreflightReason:     "proxied-server mode is owned by the bd provider",
			},
		}, nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "bd-owned proxied store") {
		t.Errorf("message = %q, want it to name the bd-owned proxied store", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty: nothing to repair", r.FixHint)
	}
}

// TestBeadsStoreCheck_UnknownGateStillWarns pins that R3 narrows only the
// proxied gate; every other fallback keeps its repair warning.
func TestBeadsStoreCheck_UnknownGateStillWarns(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n\n[beads]\nprovider = \"file\"\n")
	spy := &spyPingStore{pingFunc: func() error { return nil }}
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{
			Store: spy,
			Diagnostic: beads.BeadsDiagnostic{
				Store:               beads.BeadsStoreNameBdStore,
				NativeStoreEligible: false,
				PreflightGate:       "unsupported_dolt_mode",
				PreflightReason:     `unsupported persisted dolt_mode "weird"`,
			},
		}, nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint is empty, want repair guidance")
	}
}

// TestRigBeadsCheck_ProxiedScopeReportsBdOwnedStore is the rig-side R3 case:
// the rig store opened through the bd front door, and the message says so.
func TestRigBeadsCheck_ProxiedScopeReportsBdOwnedStore(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{EndpointOrigin: contract.EndpointOriginManagedCity, DoltMode: "proxied-server"})
	writeDoctorProxiedMetadata(t, cityDir, "hq")
	// A bd-initialized proxied rig carries the generic rc.2 config template:
	// the mode lives only in metadata.json, and there is no endpoint to mirror.
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: de\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDoctorProxiedMetadata(t, rigDir, "de")
	spy := &spyPingStore{pingFunc: func() error { return nil }}
	c := NewRigBeadsCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, func(string) (beads.Store, error) { return spy, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "bd-owned proxied store") {
		t.Errorf("message = %q, want it to name the bd-owned proxied store", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty", r.FixHint)
	}
}

// TestRigBeadsCheck_DirectScopeMessageUnchanged guards the non-proxied lens.
func TestRigBeadsCheck_DirectScopeMessageUnchanged(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	spy := &spyPingStore{pingFunc: func() error { return nil }}
	c := NewRigBeadsCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, func(string) (beads.Store, error) { return spy, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if r.Message != "store accessible" {
		t.Errorf("message = %q, want the unchanged direct-scope message", r.Message)
	}
}

// TestBeadsStoreCheck_PendingScopeInitializationWarns covers the ownership
// journal lens: a scope still mid-initialisation must not be diagnosed
// through the legacy managed-Dolt path.
func TestBeadsStoreCheck_PendingScopeInitializationWarns(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	writeDoctorOwnershipJournal(t, dir, pendingOwnershipJournal(t, "city", dir))
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.StoreOpenResult, error) {
		t.Fatal("store must not be opened while initialisation is pending")
		return beads.StoreOpenResult{}, nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "beads scope initialisation pending") {
		t.Errorf("message = %q, want the pending-initialisation message", r.Message)
	}
	if !strings.Contains(r.Message, "gc start") {
		t.Errorf("message = %q, want it to name the repair command", r.Message)
	}
}

// TestDoltServerCheck_PendingScopeInitializationWarns is the dolt-side twin.
func TestDoltServerCheck_PendingScopeInitializationWarns(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	writeDoctorOwnershipJournal(t, dir, pendingOwnershipJournal(t, "city", dir))
	r := NewDoltServerCheck(dir, false).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "beads scope initialisation pending") {
		t.Errorf("message = %q, want the pending-initialisation message", r.Message)
	}
}

// TestRigDoltServerCheck_PendingScopeInitializationWarns pins the rig entry
// key form ("rig:<name>") as well as the scope-path match.
func TestRigDoltServerCheck_PendingScopeInitializationWarns(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoctorOwnershipJournal(t, cityDir, pendingOwnershipJournal(t, "rig:demo", rigDir))
	r := NewRigDoltServerCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "beads scope initialisation pending") {
		t.Errorf("message = %q, want the pending-initialisation message", r.Message)
	}
}

// TestPendingScopeInitialization_ReadyEntryIsNotPending keeps a completed
// journal entry out of the pending lens.
func TestPendingScopeInitialization_ReadyEntryIsNotPending(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	writeDoctorOwnershipJournal(t, dir, `{"version":1,"scopes":{"city":{"scope_path":"`+dir+
		`","lifecycle_owner":"provider","state":"ready"}}}`)
	if scopeInitializationPending(dir, dir) {
		t.Fatal("ready scope reported as pending initialisation")
	}
}

// TestPendingScopeInitialization_MalformedJournalIsNotPending keeps doctor
// from inventing a pending state out of unreadable journal bytes.
func TestPendingScopeInitialization_MalformedJournalIsNotPending(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	writeDoctorOwnershipJournal(t, dir, "{not json")
	if scopeInitializationPending(dir, dir) {
		t.Fatal("malformed journal reported as pending initialisation")
	}
}

// TestPendingScopeInitialization_OtherScopeDoesNotLeak keeps a pending rig
// from marking the city (or a sibling rig) as pending.
func TestPendingScopeInitialization_OtherScopeDoesNotLeak(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoctorOwnershipJournal(t, cityDir, pendingOwnershipJournal(t, "rig:demo", rigDir))
	if scopeInitializationPending(cityDir, cityDir) {
		t.Fatal("pending rig leaked into the city scope")
	}
	if !scopeInitializationPending(cityDir, rigDir) {
		t.Fatal("pending rig not reported for the rig scope")
	}
}
