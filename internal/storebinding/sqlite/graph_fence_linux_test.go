//go:build linux

package sqlite

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestGraphAdmissionRejectsMalformedWALIndexStateWithoutSourceMutation(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	if err := os.MkdirAll(graphDir, 0o700); err != nil {
		t.Fatalf("creating Graph directory: %v", err)
	}
	databasePath := filepath.Join(graphDir, graphFilename)
	if err := os.WriteFile(databasePath, sqliteHeaderForTest(2, 2), 0o600); err != nil {
		t.Fatalf("writing Graph database: %v", err)
	}
	if err := os.WriteFile(databasePath+"-wal", []byte("truncated WAL"), 0o600); err != nil {
		t.Fatalf("writing truncated WAL: %v", err)
	}
	if err := os.WriteFile(databasePath+"-shm", []byte("truncated SHM"), 0o600); err != nil {
		t.Fatalf("writing truncated SHM: %v", err)
	}
	before := snapshotGraphSource(t, graphDir)
	_, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err == nil || !strings.Contains(err.Error(), "SQLite WAL") {
		t.Fatalf("InspectGraph error = %v, want malformed SQLite WAL rejection", err)
	}
	after := snapshotGraphSource(t, graphDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("malformed WAL rejection mutated source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
}

func TestGraphFenceExcludesCompetingSQLiteWriter(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, "graph")
	source := openGraphSource(t, graphDir)
	t.Cleanup(func() { _ = source.CloseStore() })
	if _, err := source.Create(beads.Bead{Title: "writer-fence source"}); err != nil {
		t.Fatalf("creating Graph source bead: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing Graph source writer: %v", err)
	}

	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
		Target:             inspection.Target,
		ExpectedGeneration: 14,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireGraphFence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	databasePath := filepath.Join(graphDir, graphFilename)
	outcome := runSQLiteFenceChild(t,
		exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
		sqliteFenceChildWriter, databasePath,
	)
	requireSQLiteFenceChildBusy(t, outcome)

	if err := fence.Release(context.Background()); err != nil {
		t.Fatalf("releasing Graph fence: %v", err)
	}
	outcome = runSQLiteFenceChild(t,
		exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
		sqliteFenceChildWriter, databasePath,
	)
	requireSQLiteFenceChildOK(t, outcome)
}

func TestGraphFenceBlocksNewWALWriterWithoutSidecars(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, "graph")
	source := openGraphSource(t, graphDir)
	if _, err := source.Create(beads.Bead{Title: "sidecar-free WAL source"}); err != nil {
		t.Fatalf("creating Graph source bead: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing Graph source writer: %v", err)
	}
	databasePath := filepath.Join(graphDir, graphFilename)
	removeSQLiteSidecars(t, databasePath)

	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	if !inspection.Complete() {
		t.Fatal("sidecar-free source unexpectedly requires a WAL fence before acquisition")
	}
	before := snapshotGraphSource(t, graphDir)

	fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
		Target:             inspection.Target,
		ExpectedGeneration: 15,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireGraphFence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	outcome := runSQLiteFenceChild(t,
		exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
		sqliteFenceChildWriter, databasePath,
	)
	requireSQLiteFenceChildBusy(t, outcome)
	after := snapshotGraphSource(t, graphDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("new WAL writer changed the source while the fence was held:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}

	if err := fence.Release(context.Background()); err != nil {
		t.Fatalf("releasing Graph fence: %v", err)
	}
	outcome = runSQLiteFenceChild(t,
		exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
		sqliteFenceChildWriter, databasePath,
	)
	requireSQLiteFenceChildOK(t, outcome)
}

func TestLegacyCombinedStaticSnapshotHoldsConcreteWriterFence(t *testing.T) {
	city := t.TempDir()
	source := openLegacyCombinedWriter(t, city)
	bead, err := source.Create(beads.Bead{ID: "gcg-static-fence", Title: "static fence", Type: "task"})
	if err != nil {
		t.Fatalf("creating legacy source bead: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing legacy source writer: %v", err)
	}
	sourceDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("LegacyCombinedSourceDir: %v", err)
	}
	databasePath := filepath.Join(sourceDir, legacyCombinedDatabaseFilename)
	removeSQLiteSidecars(t, databasePath)
	before := snapshotLegacyCombinedSource(t, sourceDir)

	reader, err := openTestLegacyCombinedSource(t, city, 20, func() {
		outcome := runSQLiteFenceChild(t,
			exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
			sqliteFenceChildWriter, databasePath,
		)
		requireSQLiteFenceChildBusy(t, outcome)
	})
	if err != nil {
		t.Fatalf("opening fenced static legacy source: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	records, err := reader.ReadClass(coordclass.ClassWork)
	if err != nil {
		t.Fatalf("reading fenced static legacy source: %v", err)
	}
	if len(records) != 1 || records[0].ID != bead.ID {
		t.Fatalf("legacy records = %#v, want %q", records, bead.ID)
	}
	after := snapshotLegacyCombinedSource(t, sourceDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("static legacy snapshot changed its source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
	outcome := runSQLiteFenceChild(t,
		exec.Command(os.Args[0], "-test.run=^TestSQLiteFenceHelperProcess$"),
		sqliteFenceChildWriter, databasePath,
	)
	requireSQLiteFenceChildOK(t, outcome)
}

func openFastSQLiteWriter(t *testing.T, databasePath string) *sql.DB {
	t.Helper()
	query := url.Values{}
	query.Set("mode", "rw")
	query.Add("_pragma", "busy_timeout(0)")
	database, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: databasePath, RawQuery: query.Encode()}).String())
	if err != nil {
		t.Fatalf("opening competing SQLite database: %v", err)
	}
	database.SetMaxOpenConns(1)
	return database
}

func removeSQLiteSidecars(t *testing.T, databasePath string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(databasePath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("removing SQLite sidecar %q: %v", suffix, err)
		}
	}
}
