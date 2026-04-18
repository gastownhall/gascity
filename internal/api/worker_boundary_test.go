package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAPINonTestFilesStayOnWorkerBoundary(t *testing.T) {
	assertNoForbiddenWorkerBypass(t, []string{
		`"github.com/gastownhall/gascity/internal/sessionlog"`,
		"worker.NewSessionHandle(",
		"worker.NewSessionCatalog(",
		"session.NewManager(",
		"session.NewManagerWithCityPath(",
		"session.NewManagerWithTransportResolverAndCityPath(",
	})
}

func assertNoForbiddenWorkerBypass(t *testing.T, forbidden []string) {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range forbidden {
			if strings.Contains(content, needle) {
				t.Fatalf("%s contains forbidden worker-boundary bypass %q", path, needle)
			}
		}
	}
}
