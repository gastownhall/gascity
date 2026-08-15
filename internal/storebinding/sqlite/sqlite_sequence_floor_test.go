package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSQLiteSequenceFloorRequiresCanonicalNonnegativeInteger(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		contents string
		want     int64
		wantErr  bool
	}{
		{name: "zero", contents: "0\n", want: 0},
		{name: "positive", contents: "42\n", want: 42},
		{name: "empty", contents: "", wantErr: true},
		{name: "negative", contents: "-1\n", wantErr: true},
		{name: "missing newline", contents: "42", wantErr: true},
		{name: "leading zero", contents: "042\n", wantErr: true},
		{name: "space", contents: " 42\n", wantErr: true},
		{name: "junk", contents: "42x\n", wantErr: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got, err := parseSQLiteSequenceFloor([]byte(scenario.contents))
			if scenario.wantErr {
				if err == nil {
					t.Fatalf("parseSQLiteSequenceFloor(%q) unexpectedly succeeded with %d", scenario.contents, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSQLiteSequenceFloor(%q): %v", scenario.contents, err)
			}
			if got != scenario.want {
				t.Fatalf("parseSQLiteSequenceFloor(%q) = %d, want %d", scenario.contents, got, scenario.want)
			}
		})
	}
}

func TestCaptureSQLiteSequenceFloorCensusUsesPinnedDescriptorForValueAndHash(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, graphSequenceFloorFilename)
	const censused = "7\n"
	if err := os.WriteFile(path, []byte(censused), 0o600); err != nil {
		t.Fatalf("writing censused floor: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening censused floor: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, []byte("8\n"), 0o600); err != nil {
		t.Fatalf("writing replacement floor: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replacing floor path: %v", err)
	}

	state, hash, err := captureSQLiteSequenceFloorCensusFromFile(context.Background(), file)
	if err != nil {
		t.Fatalf("capturing pinned sequence floor census: %v", err)
	}
	if !state.present || state.value != 7 {
		t.Fatalf("pinned sequence floor state = %#v, want present value 7", state)
	}
	wantHash := sha256.Sum256([]byte(censused))
	if hash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("pinned sequence floor hash = %q, want %x", hash, wantHash)
	}
}

func TestGraphAndLegacyTargetsBindSequenceFloorEvidence(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	if err := os.MkdirAll(graphDir, 0o700); err != nil {
		t.Fatalf("creating graph directory: %v", err)
	}
	databasePath := filepath.Join(graphDir, graphFilename)
	if err := os.WriteFile(databasePath, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing graph database: %v", err)
	}
	floorPath := filepath.Join(graphDir, graphSequenceFloorFilename)
	if err := os.WriteFile(floorPath, []byte("7\n"), 0o600); err != nil {
		t.Fatalf("writing graph sequence floor: %v", err)
	}
	before, err := captureGraphSource(databasePath)
	if err != nil {
		t.Fatalf("capturing graph source: %v", err)
	}
	beforeTarget, err := newGraphTarget(databasePath, before)
	if err != nil {
		t.Fatalf("creating graph target: %v", err)
	}
	if err := os.WriteFile(floorPath, []byte("8\n"), 0o600); err != nil {
		t.Fatalf("changing graph sequence floor: %v", err)
	}
	after, err := captureGraphSource(databasePath)
	if err != nil {
		t.Fatalf("recapturing graph source: %v", err)
	}
	afterTarget, err := newGraphTarget(databasePath, after)
	if err != nil {
		t.Fatalf("creating changed graph target: %v", err)
	}
	if beforeTarget.Equal(afterTarget) {
		t.Fatal("Graph target did not change when its sequence floor changed")
	}

	city := t.TempDir()
	legacyDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("resolving legacy source: %v", err)
	}
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("creating legacy source directory: %v", err)
	}
	legacyDB := filepath.Join(legacyDir, legacyCombinedDatabaseFilename)
	if err := os.WriteFile(legacyDB, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing legacy database: %v", err)
	}
	legacyFloor := filepath.Join(legacyDir, graphSequenceFloorFilename)
	if err := os.WriteFile(legacyFloor, []byte("7\n"), 0o600); err != nil {
		t.Fatalf("writing legacy sequence floor: %v", err)
	}
	legacyBefore, err := captureLegacyCombinedSource(legacyDir)
	if err != nil {
		t.Fatalf("capturing legacy source: %v", err)
	}
	legacyBeforeTarget, err := newLegacyCombinedTarget(legacyDB, legacyBefore)
	if err != nil {
		t.Fatalf("creating legacy target: %v", err)
	}
	if err := os.WriteFile(legacyFloor, []byte("8\n"), 0o600); err != nil {
		t.Fatalf("changing legacy sequence floor: %v", err)
	}
	legacyAfter, err := captureLegacyCombinedSource(legacyDir)
	if err != nil {
		t.Fatalf("recapturing legacy source: %v", err)
	}
	legacyAfterTarget, err := newLegacyCombinedTarget(legacyDB, legacyAfter)
	if err != nil {
		t.Fatalf("creating changed legacy target: %v", err)
	}
	if legacyBeforeTarget.Equal(legacyAfterTarget) {
		t.Fatal("legacy target did not change when its sequence floor changed")
	}
}

func TestCaptureSQLiteSequenceFloorRejectsMalformedOrHardLinkedSource(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		setup func(t *testing.T, directory string)
	}{
		{
			name: "malformed",
			setup: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, graphSequenceFloorFilename), []byte(" 7\n"), 0o600); err != nil {
					t.Fatalf("writing malformed floor: %v", err)
				}
			},
		},
		{
			name: "hard linked",
			setup: func(t *testing.T, directory string) {
				t.Helper()
				floor := filepath.Join(directory, graphSequenceFloorFilename)
				if err := os.WriteFile(floor, []byte("7\n"), 0o600); err != nil {
					t.Fatalf("writing floor: %v", err)
				}
				if err := os.Link(floor, filepath.Join(directory, "floor-alias")); err != nil {
					t.Fatalf("hard linking floor: %v", err)
				}
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			directory := t.TempDir()
			databasePath := filepath.Join(directory, graphFilename)
			if err := os.WriteFile(databasePath, sqliteHeaderForTest(1, 1), 0o600); err != nil {
				t.Fatalf("writing database: %v", err)
			}
			scenario.setup(t, directory)
			if _, err := captureGraphSource(databasePath); err == nil {
				t.Fatal("capturing malformed Graph sequence floor unexpectedly succeeded")
			}
		})
	}
}

func TestGraphAndLegacySnapshotsCopyPinnedSequenceFloor(t *testing.T) {
	graphRoot := t.TempDir()
	graphDir := filepath.Join(graphRoot, graphDirectoryName)
	if err := os.MkdirAll(graphDir, 0o700); err != nil {
		t.Fatalf("creating graph directory: %v", err)
	}
	graphDB := filepath.Join(graphDir, graphFilename)
	if err := os.WriteFile(graphDB, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing graph database: %v", err)
	}
	if err := os.WriteFile(filepath.Join(graphDir, graphSequenceFloorFilename), []byte("42\n"), 0o600); err != nil {
		t.Fatalf("writing graph sequence floor: %v", err)
	}
	graphState, err := captureGraphSource(graphDB)
	if err != nil {
		t.Fatalf("capturing graph source: %v", err)
	}
	graphSnapshot := filepath.Join(graphRoot, "snapshot", graphDirectoryName)
	if err := copyGraphSnapshot(context.Background(), graphDir, graphSnapshot, graphState, openSQLiteSnapshotFilesForTest(t, graphDir, graphFilename)); err != nil {
		t.Fatalf("copying graph snapshot: %v", err)
	}
	if copied, err := os.ReadFile(filepath.Join(graphSnapshot, graphSequenceFloorFilename)); err != nil {
		t.Fatalf("reading copied graph sequence floor: %v", err)
	} else if string(copied) != "42\n" {
		t.Fatalf("copied graph sequence floor = %q, want 42\\n", copied)
	}

	city := t.TempDir()
	legacyDir, err := LegacyCombinedSourceDir(city)
	if err != nil {
		t.Fatalf("resolving legacy directory: %v", err)
	}
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("creating legacy directory: %v", err)
	}
	legacyDB := filepath.Join(legacyDir, legacyCombinedDatabaseFilename)
	if err := os.WriteFile(legacyDB, sqliteHeaderForTest(1, 1), 0o600); err != nil {
		t.Fatalf("writing legacy database: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, graphSequenceFloorFilename), []byte("42\n"), 0o600); err != nil {
		t.Fatalf("writing legacy sequence floor: %v", err)
	}
	legacyState, err := captureLegacyCombinedSource(legacyDir)
	if err != nil {
		t.Fatalf("capturing legacy source: %v", err)
	}
	legacySnapshot := filepath.Join(city, "snapshot", ".beads")
	if err := copyLegacyCombinedSnapshot(context.Background(), legacySnapshot, legacyState, openSQLiteSnapshotFilesForTest(t, legacyDir, legacyCombinedDatabaseFilename)); err != nil {
		t.Fatalf("copying legacy snapshot: %v", err)
	}
	if copied, err := os.ReadFile(filepath.Join(legacySnapshot, graphSequenceFloorFilename)); err != nil {
		t.Fatalf("reading copied legacy sequence floor: %v", err)
	} else if string(copied) != "42\n" {
		t.Fatalf("copied legacy sequence floor = %q, want 42\\n", copied)
	}
}
