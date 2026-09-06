//go:build linux

package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestGraphFenceReleaseRetainsCityClaimUntilComponentReleaseSucceeds(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	source := openGraphSource(t, graphDir)
	if _, err := source.Create(beads.Bead{Title: "release ordering"}); err != nil {
		t.Fatalf("creating graph source: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing graph source: %v", err)
	}

	inspection := inspectProcessGraph(t, root)
	cityGCDir := filepath.Join(root, ".gc")
	if err := os.MkdirAll(cityGCDir, 0o700); err != nil {
		t.Fatalf("creating city guard directory: %v", err)
	}
	scope, err := storebinding.NewMigrationGuardScope(cityGCDir)
	if err != nil {
		t.Fatalf("creating guard scope: %v", err)
	}
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, 41)
	if err != nil {
		t.Fatalf("acquiring guard: %v", err)
	}
	inspector, err := NewGraphInspector(storebinding.BindingSpec{Name: storebinding.BindingName("infra"), Provider: ProviderID, Path: root})
	if err != nil {
		t.Fatalf("creating graph inspector: %v", err)
	}
	capturer := &capturingSQLiteFenceAcquirer{delegate: inspector}
	acquired, err := storebinding.AcquireWriterFence(context.Background(), guard, capturer, storebinding.FenceRequest{
		Target:             inspection.Target,
		GuardScope:         scope,
		ExpectedGeneration: 41,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("acquiring graph fence: %v", err)
	}
	fence, ok := capturer.inner.(*graphFence)
	if !ok {
		t.Fatalf("captured graph fence type = %T, want *graphFence", capturer.inner)
	}
	if err := fence.reservation.Release(); err != nil {
		t.Fatalf("releasing real reservation before injection: %v", err)
	}
	retryReservation := &retrySQLiteWriterReservation{releaseErrors: []error{errors.New("transient close failure")}}
	fence.reservation = retryReservation
	t.Cleanup(func() {
		_ = acquired.Release(context.Background())
		_ = guard.Release()
	})

	if err := acquired.Release(context.Background()); err == nil {
		t.Fatal("first graph fence release unexpectedly succeeded")
	}
	held, err := fence.Held(context.Background())
	if err != nil || held {
		t.Fatalf("graph fence held after failed component release = %v, %v; want false, nil", held, err)
	}
	if _, err := InspectGraphFenced(context.Background(), storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              fence,
		ExpectedGeneration: 41,
	}); !errors.Is(err, storebinding.ErrFenceNotHeld) {
		t.Fatalf("InspectGraphFenced with cleanup-pending fence error = %v, want ErrFenceNotHeld", err)
	}
	if err := guard.Release(); !errors.Is(err, storebinding.ErrMigrationGuardClaimsHeld) {
		t.Fatalf("releasing city guard after failed component release = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	if err := acquired.Release(context.Background()); err != nil {
		t.Fatalf("retrying graph fence release: %v", err)
	}
	if got := retryReservation.releaseCalls; got != 2 {
		t.Fatalf("graph component release calls = %d, want 2", got)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("releasing city guard after graph component release: %v", err)
	}
}

func TestLegacyCombinedFenceReleaseRetainsCityClaimUntilComponentReleaseSucceeds(t *testing.T) {
	city := t.TempDir()
	source := openLegacyCombinedWriter(t, city)
	if _, err := source.Create(beads.Bead{Title: "legacy release ordering"}); err != nil {
		t.Fatalf("creating legacy source: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing legacy source: %v", err)
	}

	cityGCDir := filepath.Join(city, ".gc")
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, 42)
	if err != nil {
		t.Fatalf("acquiring guard: %v", err)
	}
	request, err := newLegacyCombinedFenceRequest(context.Background(), city, 42)
	if err != nil {
		t.Fatalf("creating legacy fence request: %v", err)
	}
	capturer := &capturingSQLiteFenceAcquirer{delegate: legacyCombinedFenceAcquirer{}}
	acquired, err := storebinding.AcquireWriterFence(context.Background(), guard, capturer, request)
	if err != nil {
		t.Fatalf("acquiring legacy fence: %v", err)
	}
	fence, ok := capturer.inner.(*legacyCombinedFence)
	if !ok {
		t.Fatalf("captured legacy fence type = %T, want *legacyCombinedFence", capturer.inner)
	}
	if err := fence.reservation.Release(); err != nil {
		t.Fatalf("releasing real reservation before injection: %v", err)
	}
	retryReservation := &retrySQLiteWriterReservation{releaseErrors: []error{errors.New("transient close failure")}}
	fence.reservation = retryReservation
	t.Cleanup(func() {
		_ = acquired.Release(context.Background())
		_ = guard.Release()
	})

	if err := acquired.Release(context.Background()); err == nil {
		t.Fatal("first legacy fence release unexpectedly succeeded")
	}
	held, err := fence.Held(context.Background())
	if err != nil || held {
		t.Fatalf("legacy fence held after failed component release = %v, %v; want false, nil", held, err)
	}
	if err := guard.Release(); !errors.Is(err, storebinding.ErrMigrationGuardClaimsHeld) {
		t.Fatalf("releasing city guard after failed component release = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	if err := acquired.Release(context.Background()); err != nil {
		t.Fatalf("retrying legacy fence release: %v", err)
	}
	if got := retryReservation.releaseCalls; got != 2 {
		t.Fatalf("legacy component release calls = %d, want 2", got)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("releasing city guard after legacy component release: %v", err)
	}
}

func TestLinuxSQLiteWriterReservationReleaseRetriesInjectedCloseFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), graphFilename)
	if err := os.WriteFile(path, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing SQLite source: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening SQLite source: %v", err)
	}
	reservation := &linuxSQLiteWriterReservation{database: file}
	originalClose := closeSQLiteReservationFile
	calls := 0
	closeSQLiteReservationFile = func(candidate *os.File) error {
		calls++
		if calls == 1 {
			return errors.New("injected close failure")
		}
		return originalClose(candidate)
	}
	t.Cleanup(func() {
		closeSQLiteReservationFile = originalClose
		_ = reservation.Release()
	})

	if err := reservation.Release(); err == nil {
		t.Fatal("first reservation release unexpectedly succeeded")
	}
	if reservation.database == nil {
		t.Fatal("reservation dropped its database descriptor after a failed close")
	}
	if err := reservation.Release(); err != nil {
		t.Fatalf("retrying reservation release: %v", err)
	}
	if reservation.database != nil {
		t.Fatal("reservation retained its database descriptor after a successful retry")
	}
	if calls != 2 {
		t.Fatalf("close calls = %d, want 2", calls)
	}
}

func TestLinuxSQLiteWriterReservationReleaseClosesDescriptorsInReverseAcquisitionOrder(t *testing.T) {
	directory := t.TempDir()
	paths := map[string]string{
		"database":       filepath.Join(directory, graphFilename),
		"wal":            filepath.Join(directory, graphFilename+"-wal"),
		"journal":        filepath.Join(directory, graphFilename+"-journal"),
		"sequence-floor": filepath.Join(directory, graphSequenceFloorFilename),
		"shm":            filepath.Join(directory, graphFilename+"-shm"),
	}
	for label, path := range paths {
		if err := os.WriteFile(path, []byte(label), 0o600); err != nil {
			t.Fatalf("writing %s source: %v", label, err)
		}
	}
	open := func(t *testing.T, path string) *os.File {
		t.Helper()
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("opening %s: %v", path, err)
		}
		return file
	}
	directoryFile := open(t, directory)
	database := open(t, paths["database"])
	pending := open(t, paths["database"])
	wal := open(t, paths["wal"])
	journal := open(t, paths["journal"])
	sequenceFloor := open(t, paths["sequence-floor"])
	shm := open(t, paths["shm"])
	reservation := &linuxSQLiteWriterReservation{
		directory:     directoryFile,
		database:      database,
		pending:       pending,
		wal:           wal,
		journal:       journal,
		sequenceFloor: sequenceFloor,
		shm:           shm,
	}
	labels := map[*os.File]string{
		directoryFile: "directory",
		database:      "database",
		pending:       "pending",
		wal:           "wal",
		journal:       "journal",
		sequenceFloor: "sequence-floor",
		shm:           "shm",
	}
	originalClose := closeSQLiteReservationFile
	var closed []string
	closeSQLiteReservationFile = func(file *os.File) error {
		closed = append(closed, labels[file])
		return originalClose(file)
	}
	t.Cleanup(func() {
		closeSQLiteReservationFile = originalClose
		_ = reservation.Release()
	})

	if err := reservation.Release(); err != nil {
		t.Fatalf("releasing reservation: %v", err)
	}
	want := []string{"shm", "pending", "sequence-floor", "journal", "wal", "database", "directory"}
	if len(closed) != len(want) {
		t.Fatalf("close order = %#v, want %#v", closed, want)
	}
	for index := range want {
		if closed[index] != want[index] {
			t.Fatalf("close order = %#v, want %#v", closed, want)
		}
	}
}

func TestSQLiteReservationDirectoryVerificationRejectionRetainsDescriptorForRetryableClose(t *testing.T) {
	directory := t.TempDir()
	opened, err := openSQLiteReservationDirectory(directory, storebinding.PhysicalIdentity("wrong-directory-identity"))
	if !errors.Is(err, errSQLiteSourceChanged) {
		t.Fatalf("opening mismatched source directory error = %v, want source changed", err)
	}
	if opened == nil {
		t.Fatal("opening mismatched source directory did not retain its descriptor")
	}
	reservation := &linuxSQLiteWriterReservation{directory: opened}
	closeFailure := errors.New("injected directory close failure")
	originalClose := closeSQLiteReservationFile
	failClose := true
	closeSQLiteReservationFile = func(file *os.File) error {
		if file == opened && failClose {
			return closeFailure
		}
		return originalClose(file)
	}
	t.Cleanup(func() {
		closeSQLiteReservationFile = originalClose
		_ = reservation.Release()
	})

	if err := reservation.Release(); !errors.Is(err, closeFailure) {
		t.Fatalf("releasing mismatched directory descriptor = %v, want close failure", err)
	}
	if reservation.directory == nil {
		t.Fatal("reservation dropped mismatched directory descriptor after failed close")
	}
	failClose = false
	if err := reservation.Release(); err != nil {
		t.Fatalf("retrying mismatched directory descriptor close: %v", err)
	}
	if reservation.directory != nil {
		t.Fatal("reservation retained mismatched directory descriptor after successful close")
	}
}

func TestSQLiteReservationComponentVerificationRejectionRetainsDescriptorForRetryableClose(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, graphFilename)
	if err := os.WriteFile(path, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing SQLite component: %v", err)
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stating source directory: %v", err)
	}
	openedDirectory, err := openSQLiteReservationDirectory(directory, physicalIdentity(directory, directoryInfo))
	if err != nil {
		t.Fatalf("opening source directory: %v", err)
	}
	component, err := openSQLiteReservationComponent(openedDirectory, graphFilename, os.O_RDONLY, path, storebinding.PhysicalIdentity("wrong-component-identity"))
	if !errors.Is(err, errSQLiteSourceChanged) {
		t.Fatalf("opening mismatched source component error = %v, want source changed", err)
	}
	if component == nil {
		t.Fatal("opening mismatched source component did not retain its descriptor")
	}
	reservation := &linuxSQLiteWriterReservation{directory: openedDirectory, database: component}
	closeFailure := errors.New("injected component close failure")
	originalClose := closeSQLiteReservationFile
	failComponentClose := true
	closeSQLiteReservationFile = func(file *os.File) error {
		if file == component && failComponentClose {
			return closeFailure
		}
		return originalClose(file)
	}
	t.Cleanup(func() {
		closeSQLiteReservationFile = originalClose
		_ = reservation.Release()
	})

	if err := reservation.Release(); !errors.Is(err, closeFailure) {
		t.Fatalf("releasing mismatched component descriptor = %v, want close failure", err)
	}
	if reservation.database == nil {
		t.Fatal("reservation dropped mismatched component descriptor after failed close")
	}
	if reservation.directory != nil {
		t.Fatal("reservation retained directory descriptor after its successful close")
	}
	failComponentClose = false
	if err := reservation.Release(); err != nil {
		t.Fatalf("retrying mismatched component descriptor close: %v", err)
	}
	if reservation.database != nil {
		t.Fatal("reservation retained mismatched component descriptor after successful close")
	}
}

func TestGraphFenceAcquireFailureRetainsPartialReservationCleanup(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	source := openGraphSource(t, graphDir)
	if _, err := source.Create(beads.Bead{Title: "partial acquisition source"}); err != nil {
		t.Fatalf("creating Graph source: %v", err)
	}
	if err := source.CloseStore(); err != nil {
		t.Fatalf("closing Graph source: %v", err)
	}

	inspection := inspectProcessGraph(t, root)
	cityGCDir := filepath.Join(root, ".gc")
	if err := os.MkdirAll(cityGCDir, 0o700); err != nil {
		t.Fatalf("creating city guard directory: %v", err)
	}
	scope, err := storebinding.NewMigrationGuardScope(cityGCDir)
	if err != nil {
		t.Fatalf("creating guard scope: %v", err)
	}
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityGCDir, 43)
	if err != nil {
		t.Fatalf("acquiring guard: %v", err)
	}

	lockFailure := errors.New("injected reservation lock failure")
	originalLock := lockSQLiteOFD
	lockSQLiteOFD = func(*os.File, int, int64, int64) error { return lockFailure }
	t.Cleanup(func() { lockSQLiteOFD = originalLock })

	closeFailure := errors.New("injected reservation close failure")
	originalClose := closeSQLiteReservationFile
	closeCalls := 0
	allowClose := false
	closeSQLiteReservationFile = func(file *os.File) error {
		closeCalls++
		if !allowClose {
			return closeFailure
		}
		return originalClose(file)
	}
	t.Cleanup(func() {
		closeSQLiteReservationFile = originalClose
	})

	inspector, err := NewGraphInspector(storebinding.BindingSpec{Name: storebinding.BindingName("infra"), Provider: ProviderID, Path: root})
	if err != nil {
		t.Fatalf("creating graph inspector: %v", err)
	}
	_, err = storebinding.AcquireWriterFence(context.Background(), guard, inspector, storebinding.FenceRequest{
		Target:             inspection.Target,
		GuardScope:         scope,
		ExpectedGeneration: 43,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if !errors.Is(err, lockFailure) || !errors.Is(err, closeFailure) {
		t.Fatalf("AcquireWriterFence error = %v, want acquisition and close failures", err)
	}
	var cleanup *storebinding.RejectedWriterFenceCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("AcquireWriterFence error = %T, want retryable cleanup error", err)
	}
	t.Cleanup(func() {
		allowClose = true
		if cleanup != nil {
			_ = cleanup.RetryCleanup(context.Background())
		}
		_ = guard.Release()
	})
	if err := guard.Release(); !errors.Is(err, storebinding.ErrMigrationGuardClaimsHeld) {
		t.Fatalf("guard release while partial reservation cleanup pending = %v, want ErrMigrationGuardClaimsHeld", err)
	}
	allowClose = true
	if err := cleanup.RetryCleanup(context.Background()); err != nil {
		t.Fatalf("retrying partial reservation cleanup: %v", err)
	}
	if closeCalls != 6 {
		t.Fatalf("reservation close calls = %d, want 6", closeCalls)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("releasing guard after partial reservation cleanup: %v", err)
	}
}

type retrySQLiteWriterReservation struct {
	releaseErrors []error
	releaseCalls  int
}

func (r *retrySQLiteWriterReservation) Release() error {
	r.releaseCalls++
	if len(r.releaseErrors) == 0 {
		return nil
	}
	err := r.releaseErrors[0]
	r.releaseErrors = r.releaseErrors[1:]
	return err
}

func (*retrySQLiteWriterReservation) snapshotFiles() sqliteSnapshotFiles {
	return sqliteSnapshotFiles{}
}
