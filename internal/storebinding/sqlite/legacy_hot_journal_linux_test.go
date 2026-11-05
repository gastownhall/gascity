//go:build linux

package sqlite

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestLegacyCombinedSourceRecoversHotRollbackJournalInPrivateSnapshot(t *testing.T) {
	city := t.TempDir()
	sourceDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("LegacyCombinedSourceDir(): %v", err)
	}
	databasePath := filepath.Join(sourceDir, legacyCombinedDatabaseFilename)
	child := startSQLiteFenceChild(t,
		exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
		sqliteFenceChildRollbackCrash,
		databasePath,
		true,
	)
	child.kill(t)
	if err := validateSQLiteRollbackJournalPathForTest(databasePath + "-journal"); err != nil {
		t.Fatalf("crash-produced rollback journal failed validation: %v", err)
	}
	before := snapshotLegacyCombinedSource(t, sourceDir)

	reader, err := openTestLegacyCombinedSource(t, city, 82, nil)
	if err != nil {
		t.Fatalf("opening hot-journal legacy source through production path: %v", err)
	}
	records, err := reader.ReadClass(coordclass.ClassWork)
	if err != nil {
		_ = reader.Close()
		t.Fatalf("reading recovered private legacy snapshot: %v", err)
	}
	found := false
	for _, record := range records {
		if record.ID == "gcg-rollback-crash" {
			found = true
		}
	}
	if !found {
		_ = reader.Close()
		t.Fatalf("recovered private legacy records = %#v, want gcg-rollback-crash", records)
	}
	privateJournal := filepath.Join(reader.snapshotDir, ".beads", legacyCombinedDatabaseFilename+"-journal")
	if _, err := os.Stat(privateJournal); !os.IsNotExist(err) {
		_ = reader.Close()
		t.Fatalf("private hot journal remained after recovery: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("closing recovered private legacy source: %v", err)
	}

	after := snapshotLegacyCombinedSource(t, sourceDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("private legacy recovery mutated source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
}
