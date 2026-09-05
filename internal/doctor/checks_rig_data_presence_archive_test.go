package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// archiveTestRig builds a rig with identity stamped, an empty live store, and
// NO local issues.jsonl — the shape a gc-managed scope actually has on disk,
// since gc deletes that file (reapStaleBdExportJSONL) once export.auto is off.
func archiveTestRig(t *testing.T) (*RigDataPresenceCheck, string) {
	t.Helper()
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "identity.toml"),
		[]byte("[project]\nid = \"proj-1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	c := NewRigDataPresenceCheck(cityDir, config.Rig{Name: "myrig", Path: rigDir},
		func(string) (beads.Store, error) { return store, nil })
	return c, rigDir
}

// TestRigDataPresenceUsesArchivedExportWhenLocalIsReaped is the regression for
// the check being structurally blind on the configuration it targets.
//
// On a gc-managed rig the local .beads/issues.jsonl is deleted, so the empty
// store looked identical to a brand-new rig and the check reported "fresh rig,
// skip". A rig that had just lost every row passed silently — the va 2026-06-20
// shape (803 exported rows, 0 live) would not have fired.
func TestRigDataPresenceUsesArchivedExportWhenLocalIsReaped(t *testing.T) {
	c, _ := archiveTestRig(t) // empty live store, no local export
	c.WithArchiveExportRows(func(string) (int, bool) { return 803, true })

	r := c.Run(nil)
	if r.Status != StatusError || r.Severity != SeverityBlocking {
		t.Fatalf("empty store with an archived export must block as data loss; got status=%v severity=%v msg=%q",
			r.Status, r.Severity, r.Message)
	}
	if !strings.Contains(r.Message, "803") {
		t.Errorf("message should report the archived row count; got %q", r.Message)
	}
	if !strings.Contains(r.Message, "archived export") {
		t.Errorf("message should name which evidence was used, so an operator can tell it came from the archive; got %q", r.Message)
	}
}

// TestRigDataPresenceFreshRigStillSkipsWithoutAnyExport keeps the false-positive
// fix intact: a genuinely fresh rig has neither a local nor an archived export
// and must not gate dispatch.
func TestRigDataPresenceFreshRigStillSkipsWithoutAnyExport(t *testing.T) {
	c, _ := archiveTestRig(t)
	c.WithArchiveExportRows(func(string) (int, bool) { return 0, false })

	r := c.Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("fresh rig with no export of any kind must pass; got status=%v msg=%q", r.Status, r.Message)
	}
}

// TestRigDataPresenceLocalExportWinsOverArchive pins the precedence: the archive
// is a fallback for an absent local export, never an override of a present one.
// The local file is the more current signal, and consulting the archive first
// would compare live rows against a staler snapshot.
func TestRigDataPresenceLocalExportWinsOverArchive(t *testing.T) {
	c, rigDir := archiveTestRig(t)
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "issues.jsonl"),
		[]byte("{\"id\":\"a\"}\n{\"id\":\"b\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.WithArchiveExportRows(func(string) (int, bool) {
		t.Error("archive lookup must not be consulted when a local export exists")
		return 803, true
	})

	r := c.Run(nil)
	if !strings.Contains(r.Message, "issues.jsonl") || strings.Contains(r.Message, "archived") {
		t.Errorf("local export must be the reported evidence; got %q", r.Message)
	}
}

// TestRigDataPresenceNilArchiveLookupKeepsOldBehaviour guards the injection
// point: with no oracle supplied the check behaves exactly as before, so the
// fallback cannot change results for callers that do not opt in.
func TestRigDataPresenceNilArchiveLookupKeepsOldBehaviour(t *testing.T) {
	c, _ := archiveTestRig(t) // archiveExportRows left nil

	r := c.Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("with no archive oracle an empty store and no local export must still pass; got status=%v msg=%q", r.Status, r.Message)
	}
}
