package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestInspectGraphEscapesURIAndDoesNotMutateStaticSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source ? # % spaces")
	graphDir := filepath.Join(root, "graph")
	writer := openGraphSource(t, graphDir)
	if _, err := writer.Create(beads.Bead{Title: "static graph source"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
	}

	if err := os.Chmod(graphDir, 0o500); err != nil {
		t.Fatalf("making source directory non-writable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(graphDir, 0o700) })
	before := snapshotGraphSource(t, graphDir)

	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	if !inspection.Complete() {
		t.Fatal("InspectGraph returned incomplete for a static source")
	}
	if inspection.Descriptor == nil || len(inspection.Descriptor.Components) != 1 {
		t.Fatalf("InspectGraph descriptor = %#v, want one graph component", inspection.Descriptor)
	}

	after := snapshotGraphSource(t, graphDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("InspectGraph mutated the source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
}

func TestInspectGraphLeavesAnyRollbackJournalForFencedInspection(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	writer := openGraphSource(t, graphDir)
	if _, err := writer.Create(beads.Bead{Title: "rollback journal source"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
	}
	journalPath := filepath.Join(graphDir, graphFilename+"-journal")
	if err := os.WriteFile(journalPath, nil, 0o600); err != nil {
		t.Fatalf("creating rollback journal residue: %v", err)
	}
	before := snapshotGraphSource(t, graphDir)

	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	if inspection.Complete() {
		t.Fatal("rollback-journal source unexpectedly completed without a fence")
	}

	after := snapshotGraphSource(t, graphDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("InspectGraph mutated rollback-journal source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
}

func TestInspectGraphRefusesLiveSourceDescriptorWithoutOpeningWAL(t *testing.T) {
	if !sqliteSourceDescriptorDetectionSupported() {
		t.Skip("detecting already-open SQLite source descriptors requires Linux /proc")
	}
	root := t.TempDir()
	graphDir := filepath.Join(root, "graph")
	writer := openGraphSource(t, graphDir)
	t.Cleanup(func() { _ = writer.CloseStore() })
	if _, err := writer.Create(beads.Bead{Title: "WAL-resident graph source"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if _, err := os.Stat(filepath.Join(graphDir, "beads.sqlite-wal")); err != nil {
		t.Fatalf("live graph source has no WAL: %v", err)
	}
	before := snapshotGraphSource(t, graphDir)

	_, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if !errors.Is(err, ErrSQLiteSourceOpenInProcess) {
		t.Fatalf("InspectGraph error = %v, want ErrSQLiteSourceOpenInProcess", err)
	}

	after := snapshotGraphSource(t, graphDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("InspectGraph touched a live-WAL source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
}

func TestCopyGraphSnapshotRejectsReplacementAfterSourceCensus(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, graphDirectoryName)
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatalf("creating Graph source directory: %v", err)
	}
	databasePath := filepath.Join(sourceDir, graphFilename)
	if err := os.WriteFile(databasePath, sqliteRollbackHeaderForTest(), 0o600); err != nil {
		t.Fatalf("writing Graph source database: %v", err)
	}
	state, err := captureGraphSource(databasePath)
	if err != nil {
		t.Fatalf("capturing Graph source: %v", err)
	}

	replacement := filepath.Join(root, "replacement.sqlite")
	replacementBytes := sqliteRollbackHeaderForTest()
	replacementBytes[20] = 1
	if err := os.WriteFile(replacement, replacementBytes, 0o600); err != nil {
		t.Fatalf("writing replacement Graph database: %v", err)
	}
	if err := os.Rename(replacement, databasePath); err != nil {
		t.Fatalf("replacing Graph database after census: %v", err)
	}

	err = copyGraphSnapshot(context.Background(), sourceDir, filepath.Join(root, "snapshot", graphDirectoryName), state, openSQLiteSnapshotFilesForTest(t, sourceDir, graphFilename))
	if err == nil || !strings.Contains(err.Error(), "changed after source census") {
		t.Fatalf("copyGraphSnapshot() error = %v, want replacement rejection", err)
	}
}

func TestCopyGraphSnapshotOmitsDerivedWALIndex(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, graphDirectoryName)
	source := openGraphSource(t, sourceDir)
	t.Cleanup(func() { _ = source.CloseStore() })
	if _, err := source.Create(beads.Bead{Title: "WAL snapshot"}); err != nil {
		t.Fatalf("creating live-WAL Graph source: %v", err)
	}
	databasePath := filepath.Join(sourceDir, graphFilename)
	state, err := captureGraphSource(databasePath)
	if err != nil {
		t.Fatalf("capturing live-WAL Graph source: %v", err)
	}
	if _, ok := state.files[graphFilename+"-shm"]; !ok {
		t.Fatal("live-WAL source did not create a shared-memory index")
	}

	destinationDir := filepath.Join(root, "snapshot", graphDirectoryName)
	if err := copyGraphSnapshot(context.Background(), sourceDir, destinationDir, state, openSQLiteSnapshotFilesForTest(t, sourceDir, graphFilename)); err != nil {
		t.Fatalf("copying Graph snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destinationDir, graphFilename+"-shm")); !os.IsNotExist(err) {
		t.Fatalf("snapshot copied derived source WAL index, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destinationDir, graphFilename+"-wal")); err != nil {
		t.Fatalf("snapshot omitted authoritative WAL: %v", err)
	}
}

func TestGraphSourceFenceComparisonAllowsOnlyWALIndexReaderMarkChurn(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	if err := os.MkdirAll(graphDir, 0o700); err != nil {
		t.Fatalf("creating Graph source directory: %v", err)
	}
	databasePath := filepath.Join(graphDir, graphFilename)
	if err := os.WriteFile(databasePath, sqliteRollbackHeaderForTest(), 0o600); err != nil {
		t.Fatalf("writing Graph database: %v", err)
	}
	if err := os.WriteFile(databasePath+"-wal", []byte("authoritative WAL"), 0o600); err != nil {
		t.Fatalf("writing Graph WAL: %v", err)
	}
	shmPath := databasePath + "-shm"
	if err := os.WriteFile(shmPath, sqliteSHMHeaderForTest(), 0o600); err != nil {
		t.Fatalf("writing Graph WAL index: %v", err)
	}
	before, err := captureGraphSource(databasePath)
	if err != nil {
		t.Fatalf("capturing Graph source before read mark: %v", err)
	}
	shm, err := os.OpenFile(shmPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening Graph WAL index: %v", err)
	}
	defer shm.Close() //nolint:errcheck
	if _, err := shm.WriteAt([]byte{1}, 100); err != nil {
		t.Fatalf("changing Graph WAL-index reader mark: %v", err)
	}
	// The reader-mark region is deliberately excluded from the WAL-index hash,
	// so the STRICT comparison's sensitivity to this write rests on ModTime
	// alone. Move the mtime explicitly so the assertion does not depend on the
	// filesystem's timestamp granularity (coarse on CI runners).
	markTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(shmPath, markTime, markTime); err != nil {
		t.Fatalf("bumping Graph WAL-index mtime: %v", err)
	}
	after, err := captureGraphSource(databasePath)
	if err != nil {
		t.Fatalf("capturing Graph source after read mark: %v", err)
	}
	if before.equal(after) {
		t.Fatal("strict Graph source comparison ignored WAL-index churn")
	}
	if !before.equalForFence(after) {
		t.Fatal("fenced Graph source comparison rejected WAL-index reader-mark churn")
	}
	if _, err := shm.WriteAt([]byte{1}, 120); err != nil {
		t.Fatalf("changing stable Graph WAL-index payload: %v", err)
	}
	stableChanged, err := captureGraphSource(databasePath)
	if err != nil {
		t.Fatalf("capturing Graph source after stable WAL-index change: %v", err)
	}
	if after.equalForFence(stableChanged) {
		t.Fatal("fenced Graph source comparison ignored stable WAL-index payload change")
	}
}

func TestGraphFenceCompletesInspectionFromTemporarySnapshot(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	root := t.TempDir()
	graphDir := filepath.Join(root, "graph")
	writer := openGraphSource(t, graphDir)
	t.Cleanup(func() { _ = writer.CloseStore() })
	if _, err := writer.Create(beads.Bead{Title: "fenced graph source"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
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
		ExpectedGeneration: 7,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireGraphFence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	coverage, ok := fence.(interface {
		CoveredComponents() []storebinding.ComponentID
	})
	if !ok {
		t.Fatalf("Graph fence %T does not expose covered components", fence)
	}
	covered := coverage.CoveredComponents()
	if !reflect.DeepEqual(covered, []storebinding.ComponentID{GraphComponentID}) {
		t.Fatalf("fence covered components = %v, want [%s]", covered, GraphComponentID)
	}
	covered[0] = "mutated"
	if got := coverage.CoveredComponents(); !reflect.DeepEqual(got, []storebinding.ComponentID{GraphComponentID}) {
		t.Fatalf("fence coverage leaked mutable state: %v", got)
	}

	descriptor, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              fence,
		ExpectedGeneration: 7,
	})
	if err != nil {
		t.Fatalf("InspectGraphFenced: %v", err)
	}
	if len(descriptor.Components) != 1 || descriptor.Components[0].ID != GraphComponentID {
		t.Fatalf("fenced descriptor components = %#v, want graph component", descriptor.Components)
	}
	if descriptor.Components[0].PhysicalIdentity != inspection.Target.Components[0].PhysicalIdentity {
		t.Fatalf("fenced descriptor identity = %q, want target identity %q", descriptor.Components[0].PhysicalIdentity, inspection.Target.Components[0].PhysicalIdentity)
	}

	if err := fence.Release(context.Background()); err != nil {
		t.Fatalf("first fence release: %v", err)
	}
	if err := fence.Release(context.Background()); err != nil {
		t.Fatalf("idempotent fence release: %v", err)
	}
	if held, err := fence.Held(context.Background()); err != nil || held {
		t.Fatalf("fence held after release = %v, %v; want false, nil", held, err)
	}
}

func TestAcquireGraphFenceRejectsTargetMovedAfterInspection(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, "graph")
	writer := openGraphSource(t, graphDir)
	if _, err := writer.Create(beads.Bead{Title: "original graph source"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
	}

	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	if err := os.Rename(graphDir, graphDir+"-replaced"); err != nil {
		t.Fatalf("moving original graph directory: %v", err)
	}
	replacement := openGraphSource(t, graphDir)
	if err := replacement.CloseStore(); err != nil {
		t.Fatalf("closing replacement graph writer: %v", err)
	}

	_, err = acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
		Target:             inspection.Target,
		ExpectedGeneration: 8,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if !errors.Is(err, storebinding.ErrFenceTargetMoved) {
		t.Fatalf("AcquireGraphFence error = %v, want target-moved error", err)
	}
}

func TestGraphFenceSnapshotsSQLiteComponentsWithoutCopyingCloneLocalSidecars(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	root := t.TempDir()
	graphDir := filepath.Join(root, "graph")
	writer := openGraphSource(t, graphDir)
	t.Cleanup(func() { _ = writer.CloseStore() })
	bead, err := writer.Create(beads.Bead{Title: "graph with clone-local state"})
	if err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.SetLocalString(bead.ID, "last_woke_at", "2026-07-30T00:00:00Z"); err != nil {
		t.Fatalf("writing clone-local sidecar: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
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
		ExpectedGeneration: 9,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireGraphFence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	if _, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              fence,
		ExpectedGeneration: 9,
	}); err != nil {
		t.Fatalf("InspectGraphFenced with clone-local sidecar: %v", err)
	}
}

func TestGraphInspectorRetainsResolvedBindingPathAcrossFencedInspection(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	root := t.TempDir()
	writer := openGraphSource(t, filepath.Join(root, "graph"))
	t.Cleanup(func() { _ = writer.CloseStore() })
	if _, err := writer.Create(beads.Bead{Title: "configured fenced graph"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
	}
	spec := storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	}
	inspector, err := NewGraphInspector(spec)
	if err != nil {
		t.Fatalf("NewGraphInspector: %v", err)
	}
	inspection, err := inspector.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	fence, err := acquireTestGraphFence(t, inspector, storebinding.FenceRequest{
		Target:             inspection.Target,
		ExpectedGeneration: 10,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireFence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	descriptor, err := inspector.InspectFenced(context.Background(), storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              fence,
		ExpectedGeneration: 10,
	})
	if err != nil {
		t.Fatalf("InspectFenced: %v", err)
	}
	sum := sha256.Sum256([]byte("gascity.sqlite.binding-config.v2\x00" + root + "\x00"))
	want := storebinding.ConfigRefDigest("sha256:" + hex.EncodeToString(sum[:]))
	if descriptor.ConfigRefDigest != want {
		t.Fatalf("fenced descriptor config digest = %q, want %q", descriptor.ConfigRefDigest, want)
	}
}

func TestGraphFenceRejectsComponentStateChangedDuringSnapshotCopy(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	root := t.TempDir()
	graphDir := filepath.Join(root, "graph")
	writer := openGraphSource(t, graphDir)
	t.Cleanup(func() { _ = writer.CloseStore() })
	if _, err := writer.Create(beads.Bead{Title: "source with an adversarial copy window"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
	}
	inspector, err := NewGraphInspector(storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("NewGraphInspector: %v", err)
	}
	inspection, err := inspector.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	inspector.beforeSnapshotCopy = func() {
		if err := os.WriteFile(filepath.Join(graphDir, "graph.seqfloor"), []byte("42\n"), 0o644); err != nil {
			t.Fatalf("changing Graph sidecar during snapshot copy: %v", err)
		}
	}
	fence, err := acquireTestGraphFence(t, inspector, storebinding.FenceRequest{
		Target:             inspection.Target,
		ExpectedGeneration: 11,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireFence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	_, err = inspector.InspectFenced(context.Background(), storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              fence,
		ExpectedGeneration: 11,
	})
	if !errors.Is(err, storebinding.ErrFenceTargetMoved) {
		t.Fatalf("InspectFenced error = %v, want ErrFenceTargetMoved", err)
	}
}

func TestGraphFencedInspectionCancellationJoinsSnapshotCleanupFailure(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	writer := openGraphSource(t, graphDir)
	if _, err := writer.Create(beads.Bead{Title: "cancellable fenced snapshot"}); err != nil {
		t.Fatalf("creating Graph source: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing Graph source: %v", err)
	}
	inspector, err := NewGraphInspector(storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("NewGraphInspector: %v", err)
	}
	inspection, err := inspector.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	inspector.beforeSnapshotCopy = cancel
	fence, err := acquireTestGraphFence(t, inspector, storebinding.FenceRequest{
		Target:             inspection.Target,
		ExpectedGeneration: 52,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("AcquireFence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })
	before := snapshotGraphSource(t, graphDir)

	temporaryParent := t.TempDir()
	originalMakeTemp := makeGraphInspectionTempDir
	makeGraphInspectionTempDir = func(_ string, pattern string) (string, error) {
		return os.MkdirTemp(temporaryParent, pattern)
	}
	t.Cleanup(func() { makeGraphInspectionTempDir = originalMakeTemp })
	cleanupFailure := errors.New("injected Graph inspection snapshot cleanup failure")
	originalRemove := removeGraphInspectionTempRoot
	removeGraphInspectionTempRoot = func(path string) error {
		if err := originalRemove(path); err != nil {
			return err
		}
		return cleanupFailure
	}
	t.Cleanup(func() { removeGraphInspectionTempRoot = originalRemove })
	_, err = inspector.InspectFenced(ctx, storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              fence,
		ExpectedGeneration: 52,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("InspectFenced cancellation error = %v, want context.Canceled", err)
	}
	if !errors.Is(err, cleanupFailure) {
		t.Fatalf("InspectFenced cancellation error = %v, want cleanup failure joined", err)
	}
	entries, err := os.ReadDir(temporaryParent)
	if err != nil {
		t.Fatalf("reading temporary snapshot parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled inspection left private snapshot entries: %#v", entries)
	}
	after := snapshotGraphSource(t, graphDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("canceled inspection changed source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
	}
}

func TestGraphFenceRejectsMarkerChangesDuringSnapshotCopy(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	for _, scenario := range []struct {
		name       string
		initial    *string
		changeMark func(t *testing.T, markerPath string)
	}{
		{
			name: "create",
			changeMark: func(t *testing.T, markerPath string) {
				t.Helper()
				if err := os.WriteFile(markerPath, []byte("migrated\n"), 0o644); err != nil {
					t.Fatalf("creating Graph marker: %v", err)
				}
			},
		},
		{
			name:    "remove",
			initial: stringPointer("migrated\n"),
			changeMark: func(t *testing.T, markerPath string) {
				t.Helper()
				if err := os.Remove(markerPath); err != nil {
					t.Fatalf("removing Graph marker: %v", err)
				}
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			markerPath := filepath.Join(root, "graph.migrated")
			if scenario.initial != nil {
				if err := os.WriteFile(markerPath, []byte(*scenario.initial), 0o644); err != nil {
					t.Fatalf("seeding Graph marker: %v", err)
				}
			}
			writer := openGraphSource(t, filepath.Join(root, "graph"))
			t.Cleanup(func() { _ = writer.CloseStore() })
			if _, err := writer.Create(beads.Bead{Title: "marker race"}); err != nil {
				t.Fatalf("creating graph bead: %v", err)
			}
			if err := writer.CloseStore(); err != nil {
				t.Fatalf("closing graph writer: %v", err)
			}
			inspector, err := NewGraphInspector(storebinding.BindingSpec{
				Name:     storebinding.BindingName("infra"),
				Provider: ProviderID,
				Path:     root,
			})
			if err != nil {
				t.Fatalf("NewGraphInspector: %v", err)
			}
			inspection, err := inspector.Inspect(context.Background())
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			inspector.beforeSnapshotCopy = func() { scenario.changeMark(t, markerPath) }
			fence, err := acquireTestGraphFence(t, inspector, storebinding.FenceRequest{
				Target:             inspection.Target,
				ExpectedGeneration: 12,
				Components:         []storebinding.ComponentID{GraphComponentID},
				Role:               storebinding.FenceRoleSource,
			})
			if err != nil {
				t.Fatalf("AcquireFence: %v", err)
			}
			t.Cleanup(func() { _ = fence.Release(context.Background()) })
			_, err = inspector.InspectFenced(context.Background(), storebinding.FencedInspectionRequest{
				Target:             inspection.Target,
				Fence:              fence,
				ExpectedGeneration: 12,
			})
			if !errors.Is(err, storebinding.ErrFenceTargetMoved) {
				t.Fatalf("InspectFenced error = %v, want ErrFenceTargetMoved", err)
			}
		})
	}
}

func TestGraphInspectionCanonicalizesEquivalentBindingRoots(t *testing.T) {
	root := t.TempDir()
	writer := openGraphSource(t, filepath.Join(root, "graph"))
	if _, err := writer.Create(beads.Bead{Title: "canonical root"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
	}
	base, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil || base.Descriptor == nil {
		t.Fatalf("base InspectGraph = %#v, %v", base, err)
	}
	equivalent, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     filepath.Join(root, "."),
	})
	if err != nil || equivalent.Descriptor == nil {
		t.Fatalf("equivalent InspectGraph = %#v, %v", equivalent, err)
	}
	if base.Descriptor.ConfigRefDigest != equivalent.Descriptor.ConfigRefDigest {
		t.Fatalf("equivalent roots produced digests %q and %q", base.Descriptor.ConfigRefDigest, equivalent.Descriptor.ConfigRefDigest)
	}
}

func TestInspectGraphFencedRejectsForeignWriterFence(t *testing.T) {
	root := t.TempDir()
	writer := openGraphSource(t, filepath.Join(root, "graph"))
	if _, err := writer.Create(beads.Bead{Title: "static graph source"}); err != nil {
		t.Fatalf("creating graph bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing graph writer: %v", err)
	}

	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	if !inspection.Complete() {
		t.Fatal("static source unexpectedly requires a fence")
	}

	_, err = InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
		Target: inspection.Target,
		Fence: foreignWriterFence{
			target:     inspection.Target,
			components: []storebinding.ComponentID{GraphComponentID},
			role:       storebinding.FenceRoleSource,
			generation: 13,
		},
		ExpectedGeneration: 13,
	})
	if !errors.Is(err, storebinding.ErrInvalidFence) {
		t.Fatalf("InspectGraphFenced error = %v, want ErrInvalidFence", err)
	}
}

func TestGraphFenceInspectionOperationRetainsNoFenceOrCallbackCapability(t *testing.T) {
	writerFenceType := reflect.TypeOf((*storebinding.WriterFence)(nil)).Elem()
	visited := make(map[reflect.Type]bool)
	var inspectType func(reflect.Type, string)
	inspectType = func(valueType reflect.Type, path string) {
		if valueType == nil || visited[valueType] {
			return
		}
		visited[valueType] = true
		if valueType.Implements(writerFenceType) {
			t.Errorf("%s retains writer-fence capability through %s", path, valueType)
			return
		}
		switch valueType.Kind() {
		case reflect.Func:
			t.Errorf("%s retains callback capability through %s", path, valueType)
		case reflect.Pointer, reflect.Slice, reflect.Array:
			inspectType(valueType.Elem(), path)
		case reflect.Map:
			inspectType(valueType.Key(), path)
			inspectType(valueType.Elem(), path)
		case reflect.Struct:
			for index := 0; index < valueType.NumField(); index++ {
				field := valueType.Field(index)
				inspectType(field.Type, path+"."+field.Name)
			}
		}
	}

	inspectType(reflect.TypeOf(graphFenceInspectionOperation{}), "graphFenceInspectionOperation")
}

func TestGraphFencedInspectionValidatesExactRequestBeforeSnapshot(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing is only available on Linux")
	}
	rootA := t.TempDir()
	writerA := openGraphSource(t, filepath.Join(rootA, graphDirectoryName))
	if _, err := writerA.Create(beads.Bead{Title: "graph A"}); err != nil {
		t.Fatalf("creating Graph A bead: %v", err)
	}
	if err := writerA.CloseStore(); err != nil {
		t.Fatalf("closing Graph A writer: %v", err)
	}
	inspectionA, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra-a"),
		Provider: ProviderID,
		Path:     rootA,
	})
	if err != nil {
		t.Fatalf("inspecting Graph A: %v", err)
	}

	rootB := t.TempDir()
	writerB := openGraphSource(t, filepath.Join(rootB, graphDirectoryName))
	if _, err := writerB.Create(beads.Bead{Title: "graph B"}); err != nil {
		t.Fatalf("creating Graph B bead: %v", err)
	}
	if err := writerB.CloseStore(); err != nil {
		t.Fatalf("closing Graph B writer: %v", err)
	}
	inspectionB, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra-b"),
		Provider: ProviderID,
		Path:     rootB,
	})
	if err != nil {
		t.Fatalf("inspecting Graph B: %v", err)
	}

	inspector, err := NewGraphInspector(storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra-a"),
		Provider: ProviderID,
		Path:     rootA,
	})
	if err != nil {
		t.Fatalf("creating Graph A inspector: %v", err)
	}
	snapshotCopies := 0
	inspector.beforeSnapshotCopy = func() { snapshotCopies++ }
	fence, err := acquireTestGraphFence(t, inspector, storebinding.FenceRequest{
		Target:             inspectionA.Target,
		ExpectedGeneration: 71,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("acquiring Graph A fence: %v", err)
	}
	t.Cleanup(func() { _ = fence.Release(context.Background()) })

	for _, test := range []struct {
		name    string
		request storebinding.FencedInspectionRequest
		want    error
	}{
		{
			name: "wrong target",
			request: storebinding.FencedInspectionRequest{
				Target:             inspectionB.Target,
				Fence:              fence,
				ExpectedGeneration: 71,
			},
			want: storebinding.ErrInvalidFence,
		},
		{
			name: "wrong generation",
			request: storebinding.FencedInspectionRequest{
				Target:             inspectionA.Target,
				Fence:              fence,
				ExpectedGeneration: 72,
			},
			want: storebinding.ErrInvalidFence,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshotCopies = 0
			if _, err := inspector.InspectFenced(context.Background(), test.request); !errors.Is(err, test.want) {
				t.Fatalf("GraphInspector.InspectFenced() error = %v, want %v", err, test.want)
			}
			if snapshotCopies != 0 {
				t.Fatalf("GraphInspector.InspectFenced() copied %d snapshots for invalid request", snapshotCopies)
			}
			if _, err := InspectGraphFenced(context.Background(), test.request); !errors.Is(err, test.want) {
				t.Fatalf("InspectGraphFenced() error = %v, want %v", err, test.want)
			}
		})
	}

	coverageFence := foreignWriterFence{
		target:     inspectionA.Target,
		components: []storebinding.ComponentID{"other"},
		role:       storebinding.FenceRoleSource,
		generation: 71,
	}
	badCoverage := storebinding.FencedInspectionRequest{
		Target:             inspectionA.Target,
		Fence:              coverageFence,
		ExpectedGeneration: 71,
	}
	snapshotCopies = 0
	if _, err := inspector.InspectFenced(context.Background(), badCoverage); !errors.Is(err, storebinding.ErrInvalidFence) {
		t.Fatalf("GraphInspector.InspectFenced(bad coverage) error = %v, want ErrInvalidFence", err)
	}
	if _, err := InspectGraphFenced(context.Background(), badCoverage); !errors.Is(err, storebinding.ErrInvalidFence) {
		t.Fatalf("InspectGraphFenced(bad coverage) error = %v, want ErrInvalidFence", err)
	}
	if snapshotCopies != 0 {
		t.Fatalf("GraphInspector.InspectFenced() copied %d snapshots for bad coverage", snapshotCopies)
	}

	if err := fence.Release(context.Background()); err != nil {
		t.Fatalf("releasing Graph A fence: %v", err)
	}
	released := storebinding.FencedInspectionRequest{
		Target:             inspectionA.Target,
		Fence:              fence,
		ExpectedGeneration: 71,
	}
	snapshotCopies = 0
	if _, err := inspector.InspectFenced(context.Background(), released); !errors.Is(err, storebinding.ErrFenceNotHeld) {
		t.Fatalf("GraphInspector.InspectFenced(released) error = %v, want ErrFenceNotHeld", err)
	}
	if _, err := InspectGraphFenced(context.Background(), released); !errors.Is(err, storebinding.ErrFenceNotHeld) {
		t.Fatalf("InspectGraphFenced(released) error = %v, want ErrFenceNotHeld", err)
	}
	if snapshotCopies != 0 {
		t.Fatalf("GraphInspector.InspectFenced() copied %d snapshots for released fence", snapshotCopies)
	}
}

// foreignWriterFence satisfies the generic contract but does not hold this
// package's concrete operating-system reservation.
type foreignWriterFence struct {
	target     storebinding.FenceTarget
	components []storebinding.ComponentID
	role       storebinding.FenceRole
	generation storebinding.Generation
}

func (f foreignWriterFence) Target() storebinding.FenceTarget { return f.target.Clone() }

func (f foreignWriterFence) CoveredComponents() []storebinding.ComponentID {
	return append([]storebinding.ComponentID(nil), f.components...)
}

func (f foreignWriterFence) Role() storebinding.FenceRole { return f.role }

func (f foreignWriterFence) Generation() storebinding.Generation { return f.generation }

func (foreignWriterFence) Held(context.Context) (bool, error) { return true, nil }

func (foreignWriterFence) Release(context.Context) error { return nil }

func stringPointer(value string) *string { return &value }

func acquireTestGraphFence(t *testing.T, inspector *GraphInspector, request storebinding.FenceRequest) (storebinding.WriterFence, error) {
	t.Helper()
	path, _, err := graphTargetPath(request.Target)
	if err != nil {
		return nil, err
	}
	cityGCDir := filepath.Join(filepath.Dir(filepath.Dir(path)), ".gc")
	if err := os.MkdirAll(cityGCDir, 0o700); err != nil {
		return nil, err
	}
	scope, err := storebinding.NewMigrationGuardScope(cityGCDir)
	if err != nil {
		return nil, err
	}
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, request.ExpectedGeneration)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil {
			t.Errorf("releasing test Graph migration guard: %v", err)
		}
	})
	request.GuardScope = scope
	return storebinding.AcquireWriterFence(context.Background(), guard, inspector, request)
}

func acquireTestGraphFenceForRoot(t *testing.T, root string, request storebinding.FenceRequest) (storebinding.WriterFence, error) {
	t.Helper()
	inspector, err := NewGraphInspector(storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		return nil, err
	}
	return acquireTestGraphFence(t, inspector, request)
}

func openGraphSource(t *testing.T, graphDir string) *beads.SQLiteStore {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(graphDir, beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening graph source: %v", err)
	}
	store, ok := opened.(*beads.SQLiteStore)
	if !ok {
		t.Fatalf("graph source type = %T, want *beads.SQLiteStore", opened)
	}
	return store
}

type graphSourceSnapshot struct {
	Directory graphSourceSnapshotFile
	Entries   []string
	Files     map[string]graphSourceSnapshotFile
}

type graphSourceSnapshotFile struct {
	Mode    os.FileMode
	Size    int64
	ModTime time.Time
	Hash    string
}

func snapshotGraphSource(t *testing.T, dir string) graphSourceSnapshot {
	t.Helper()
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat graph source directory: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read graph source directory: %v", err)
	}
	snapshot := graphSourceSnapshot{
		Directory: graphSourceSnapshotFile{Mode: dirInfo.Mode(), ModTime: dirInfo.ModTime()},
		Files:     make(map[string]graphSourceSnapshotFile, len(entries)),
	}
	for _, entry := range entries {
		snapshot.Entries = append(snapshot.Entries, entry.Name())
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat graph source entry %q: %v", entry.Name(), err)
		}
		file := graphSourceSnapshotFile{Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime()}
		if info.Mode().IsRegular() {
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read graph source entry %q: %v", entry.Name(), err)
			}
			sum := sha256.Sum256(contents)
			file.Hash = hex.EncodeToString(sum[:])
		}
		snapshot.Files[entry.Name()] = file
	}
	sort.Strings(snapshot.Entries)
	return snapshot
}
