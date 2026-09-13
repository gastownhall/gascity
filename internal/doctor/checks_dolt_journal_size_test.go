package doctor

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDoltJournalSizeCheck_Skipped(t *testing.T) {
	c := NewDoltJournalSizeCheck(t.TempDir(), true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("message = %q, want skipped", r.Message)
	}
}

func TestDoltJournalSizeCheck_NonManagedDolt(t *testing.T) {
	dir := setupFreshManagedDoltCity(t)
	c := NewDoltJournalSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("message = %q, want skipped", r.Message)
	}
}

func TestDoltJournalSizeCheck_NoJournalFiles(t *testing.T) {
	dir := setupManagedDoltCity(t)
	// Create the noms dir but only non-journal files — check must return OK.
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "manifest"), 1024)
	c := NewDoltJournalSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestDoltJournalSizeCheck_OKUnderThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "vvvv1.journal"), 1024*1024) // 1 MB
	c := NewDoltJournalSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "dolt-journal-size") {
		t.Errorf("message = %q, want dolt-journal-size prefix", r.Message)
	}
}

func TestDoltJournalSizeCheck_WarnAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	// 5 GB — above warn (4 GB), below error (6 GB). Sparse file via Truncate.
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "vvvv1.journal"), 5*1024*1024*1024)
	c := NewDoltJournalSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "approaching compaction threshold") {
		t.Errorf("message = %q, want approaching-threshold text", r.Message)
	}
	if !strings.Contains(r.Message, "gc dolt compact") {
		t.Errorf("message = %q, want gc dolt compact", r.Message)
	}
}

func TestDoltJournalSizeCheck_ErrorAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	// 7 GB — above error (6 GB). Sparse file via Truncate.
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "vvvv1.journal"), 7*1024*1024*1024)
	c := NewDoltJournalSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "corruption risk") {
		t.Errorf("message = %q, want corruption-risk text", r.Message)
	}
	if !strings.Contains(r.Message, "ga-pqfk8t") {
		t.Errorf("message = %q, want ga-pqfk8t incident reference", r.Message)
	}
}

func TestDoltJournalSizeCheck_EnvThresholdOverride(t *testing.T) {
	t.Setenv("GC_DOLT_JOURNAL_WARN_BYTES", "1048576")  // override to 1 MB
	t.Setenv("GC_DOLT_JOURNAL_ERROR_BYTES", "2097152") // override to 2 MB
	dir := setupManagedDoltCity(t)
	// 1.5 MB — above overridden warn (1 MB), below overridden error (2 MB).
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "vvvv1.journal"), 1536*1024)
	c := NewDoltJournalSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning with overridden thresholds; msg = %s", r.Status, r.Message)
	}
}

func TestDoltJournalSizeCheck_MultiDBLargestDrives(t *testing.T) {
	dir := setupManagedDoltCity(t)
	dataDir := filepath.Join(dir, ".beads", "dolt")
	// hq: small journal (1 MB).
	writeFakeFile(t, filepath.Join(dataDir, "hq", ".dolt", "noms", "vvvv1.journal"), 1024*1024)
	// de: large journal (5 GB) — orphan directory, picked up from the data dir scan.
	writeFakeFile(t, filepath.Join(dataDir, "de", ".dolt", "noms", "vvvv1.journal"), 5*1024*1024*1024)
	c := NewDoltJournalSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning driven by largest DB; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "de") {
		t.Errorf("message = %q, want largest database name 'de' in message", r.Message)
	}
}
