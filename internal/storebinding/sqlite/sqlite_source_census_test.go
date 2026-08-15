package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteSourceCensusJoinsDescriptorCloseFailures(t *testing.T) {
	directory := t.TempDir()
	regularPath := filepath.Join(directory, "regular")
	if err := os.WriteFile(regularPath, []byte("contents"), 0o600); err != nil {
		t.Fatalf("writing regular census source: %v", err)
	}
	regularInfo, err := os.Stat(regularPath)
	if err != nil {
		t.Fatalf("stating regular census source: %v", err)
	}
	floorPath := filepath.Join(directory, graphSequenceFloorFilename)
	if err := os.WriteFile(floorPath, []byte("7\n"), 0o600); err != nil {
		t.Fatalf("writing sequence-floor census source: %v", err)
	}
	floorInfo, err := os.Stat(floorPath)
	if err != nil {
		t.Fatalf("stating sequence-floor census source: %v", err)
	}

	closeFailure := errors.New("injected census descriptor close failure")
	originalClose := closeSQLiteCensusFile
	var retained []*os.File
	closeSQLiteCensusFile = func(file *os.File) error {
		retained = append(retained, file)
		return closeFailure
	}
	t.Cleanup(func() {
		closeSQLiteCensusFile = originalClose
		for _, file := range retained {
			_ = originalClose(file)
		}
	})

	if _, err := captureGraphFileContext(context.Background(), regularPath, regularInfo); !errors.Is(err, closeFailure) {
		t.Fatalf("Graph census error = %v, want close failure", err)
	}
	if _, err := captureLegacyCombinedFileContext(context.Background(), regularPath, regularInfo); !errors.Is(err, closeFailure) {
		t.Fatalf("legacy census error = %v, want close failure", err)
	}
	if _, err := captureSQLiteSequenceFloorContext(context.Background(), floorPath, true, 0o600); !errors.Is(err, closeFailure) {
		t.Fatalf("sequence-floor census error = %v, want close failure", err)
	}
	if _, _, err := captureGraphSequenceFloorFileContext(context.Background(), floorPath, floorInfo); !errors.Is(err, closeFailure) {
		t.Fatalf("Graph sequence-floor census error = %v, want close failure", err)
	}
	if _, _, err := captureLegacyCombinedSequenceFloorFileContext(context.Background(), floorPath, floorInfo); !errors.Is(err, closeFailure) {
		t.Fatalf("legacy sequence-floor census error = %v, want close failure", err)
	}
}
