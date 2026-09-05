package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// --- RigDataPresenceCheck ---

func TestRigDataPresenceCheck_Name(t *testing.T) {
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: t.TempDir()}, nil)
	if got := c.Name(); got != "rig:myrig:data-presence" {
		t.Errorf("Name() = %q, want %q", got, "rig:myrig:data-presence")
	}
}

func TestRigDataPresenceCheck_WarmupEligible(t *testing.T) {
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: t.TempDir()}, nil)
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

func TestRigDataPresenceCheck_CanFix(t *testing.T) {
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: t.TempDir()}, nil)
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
}

func TestRigDataPresenceCheck_NoIdentity_OK(t *testing.T) {
	// No identity.toml → legacy rig, skip without error.
	rigDir := t.TempDir()
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: rigDir},
		func(_ string) (beads.Store, error) { return beads.NewMemStore(), nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (no identity = legacy rig); msg = %s", r.Status, r.Message)
	}
}

func TestRigDataPresenceCheck_StoreOpenFailure_Advisory(t *testing.T) {
	// Store open failure → advisory warning, not blocking.
	rigDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, rigDir, "gc-test-proj"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: rigDir},
		func(_ string) (beads.Store, error) { return nil, fmt.Errorf("dolt not running") })
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning (store open failure); msg = %s", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want Advisory (store-open failure must not block dispatch)", r.Severity)
	}
	if r.FixHint == "" {
		t.Error("FixHint is empty, want retry hint")
	}
}

func TestRigDataPresenceCheck_IssuesJSONLDeficit_Advisory(t *testing.T) {
	// Store has 2 rows but issues.jsonl has 5 lines → partial deficit. This is
	// advisory, not blocking: retention/archival (#3342/#3772) can legitimately
	// delete live rows while the export lags or keeps archived rows.
	rigDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, rigDir, "gc-test-proj"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	// Write 5-line issues.jsonl.
	jsonlPath := filepath.Join(rigDir, ".beads", "issues.jsonl")
	jsonlContent := "{\"id\":\"a\"}\n{\"id\":\"b\"}\n{\"id\":\"c\"}\n{\"id\":\"d\"}\n{\"id\":\"e\"}\n"
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0o644); err != nil {
		t.Fatalf("WriteFile(issues.jsonl): %v", err)
	}
	// Store has only 2 rows.
	store := beads.NewMemStore()
	for i := 0; i < 2; i++ {
		if _, err := store.Create(beads.Bead{Title: "bead"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: rigDir},
		func(_ string) (beads.Store, error) { return store, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning (partial deficit is advisory); msg = %s", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want Advisory (deficit must not gate dispatch)", r.Severity)
	}
}

func TestRigDataPresenceCheck_NonEmptyStore_OK(t *testing.T) {
	// Store has rows → OK.
	rigDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, rigDir, "gc-test-proj"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "some task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: rigDir},
		func(_ string) (beads.Store, error) { return store, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (non-empty store); msg = %s", r.Status, r.Message)
	}
}

func TestRigDataPresenceCheck_EmptyStoreWithExportHistory_Error(t *testing.T) {
	// identity present + empty live store + populated issues.jsonl → unambiguous
	// data loss (the va incident 2026-06-20). This is the only empty-store case
	// that blocks.
	rigDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, rigDir, "gc-test-proj"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	// Export history proves the rig previously held data.
	jsonlPath := filepath.Join(rigDir, ".beads", "issues.jsonl")
	if err := os.WriteFile(jsonlPath, []byte("{\"id\":\"a\"}\n{\"id\":\"b\"}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(issues.jsonl): %v", err)
	}
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: rigDir},
		func(_ string) (beads.Store, error) { return beads.NewMemStore(), nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error (empty store + export history); msg = %s", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %d, want Blocking", r.Severity)
	}
	if r.FixHint == "" {
		t.Error("FixHint is empty, want restore-from-backup hint")
	}
}

func TestRigDataPresenceCheck_EmptyStoreNoExportHistory_FreshRig_OK(t *testing.T) {
	// identity present (stamped at scope creation) + empty store + NO export
	// history → freshly created rig, not data loss. Must pass without gating
	// dispatch (the false-positive the review flagged).
	rigDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, rigDir, "gc-test-proj"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	c := NewRigDataPresenceCheck(t.TempDir(), config.Rig{Name: "myrig", Path: rigDir},
		func(_ string) (beads.Store, error) { return beads.NewMemStore(), nil })
	r := c.Run(&CheckContext{})
	// StatusOK never gates dispatch regardless of severity (only StatusError /
	// StatusWarning + SeverityBlocking does), so asserting OK is sufficient to
	// prove the fresh rig is not a false-positive — matching NonEmptyStore_OK.
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (fresh rig: empty store, no export); msg = %s", r.Status, r.Message)
	}
}
