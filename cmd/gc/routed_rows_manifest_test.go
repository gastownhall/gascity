package main

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sixRowMatrixMarkers mirrors required_rows in scripts/check-routed-test-rows.sh.
var sixRowMatrixMarkers = []string{
	"api-happy-path",
	"api-cache-not-live",
	"api-500-fallback",
	"api-404-error",
	"controller-down",
	"escape-hatch",
}

// TestRoutedRowsManifestFullyCovered is the Go mirror of the manifested
// six-row-matrix lint (scripts/check-routed-test-rows.sh). It fails if the
// manifest is empty or any listed file no longer carries all six rows — so the
// guard cannot be silently disabled by a marker rename even when only `go test`
// runs (not the shell lint).
func TestRoutedRowsManifestFullyCovered(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	files := readRoutedRowsManifest(t, filepath.Join(repoRoot, "scripts", "routed-test-rows.manifest"))
	if len(files) == 0 {
		t.Fatal("routed-test-rows.manifest lists no files — the six-row guard would police nothing")
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Errorf("manifest file %s: %v", rel, err)
			continue
		}
		if n := countRoutedRowMarkers(string(data)); n != 6 {
			t.Errorf("manifest file %s has %d/6 six-row markers (a marker rename or a dropped row?)", rel, n)
		}
	}
}

func readRoutedRowsManifest(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan manifest: %v", err)
	}
	return out
}

func countRoutedRowMarkers(s string) int {
	n := 0
	for _, m := range sixRowMatrixMarkers {
		if strings.Contains(s, m) {
			n++
		}
	}
	return n
}
