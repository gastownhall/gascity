//go:build linux

package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestGraphFenceAcceptsRollbackSourceInNonWritableDirectory(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	writer := openGraphSource(t, graphDir)
	if _, err := writer.Create(beads.Bead{Title: "non-writable-directory rollback source"}); err != nil {
		t.Fatalf("creating Graph source: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing Graph source: %v", err)
	}
	databasePath := filepath.Join(graphDir, graphFilename)
	configuration := openFastSQLiteWriter(t, databasePath)
	var journalMode string
	if err := configuration.QueryRowContext(context.Background(), `PRAGMA journal_mode=DELETE`).Scan(&journalMode); err != nil {
		_ = configuration.Close()
		t.Fatalf("switching Graph source to rollback journaling: %v", err)
	}
	if err := configuration.Close(); err != nil {
		t.Fatalf("closing rollback-journal configuration: %v", err)
	}
	if journalMode != "delete" {
		t.Fatalf("rollback journal mode = %q, want delete", journalMode)
	}
	removeSQLiteSidecars(t, databasePath)
	if err := os.Chmod(graphDir, 0o500); err != nil {
		t.Fatalf("making Graph source directory non-writable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(graphDir, 0o700) })

	inspection := inspectProcessGraph(t, root)
	fence, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
		Target:             inspection.Target,
		ExpectedGeneration: 51,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("acquiring Graph fence in non-writable directory: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	if _, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              fence,
		ExpectedGeneration: 51,
	}); err != nil {
		t.Fatalf("inspecting Graph source under fence: %v", err)
	}
}
