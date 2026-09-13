package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSameCommitMatchesFullAndLongPrefix(t *testing.T) {
	if !SameCommit("abcdef1234567890", "abcdef1234567890") {
		t.Fatal("SameCommit rejected identical commits")
	}
	if !SameCommit("ABCDEF1234567890", "abcdef1") {
		t.Fatal("SameCommit rejected a case-insensitive seven-character prefix")
	}
}

func TestSameCommitRejectsTooShortPrefix(t *testing.T) {
	if SameCommit("abcdef1234567890", "abcdef") {
		t.Fatal("SameCommit accepted a six-character prefix")
	}
}

func TestMissingCheckoutMarkerRecognizesNonCheckoutStates(t *testing.T) {
	dir := t.TempDir()
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if MissingCheckoutMarker(dirInfo, nil) {
		t.Fatal("MissingCheckoutMarker treated .git directory as missing")
	}

	file := filepath.Join(t.TempDir(), ".git")
	if err := os.WriteFile(file, []byte("not a checkout marker"), 0o644); err != nil {
		t.Fatalf("WriteFile(.git): %v", err)
	}
	fileInfo, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat(file): %v", err)
	}
	if !MissingCheckoutMarker(fileInfo, nil) {
		t.Fatal("MissingCheckoutMarker accepted regular .git file as checkout marker")
	}

	if _, err := os.Stat(filepath.Join(t.TempDir(), ".git")); !MissingCheckoutMarker(nil, err) {
		t.Fatalf("MissingCheckoutMarker(%v) = false, want true for missing marker", err)
	}

	parentFile := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(parentFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(cache): %v", err)
	}
	if _, err := os.Stat(filepath.Join(parentFile, ".git")); !MissingCheckoutMarker(nil, err) {
		t.Fatalf("MissingCheckoutMarker(%v) = false, want true for ENOTDIR marker", err)
	}
}
