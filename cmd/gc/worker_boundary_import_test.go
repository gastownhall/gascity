package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGCNonTestFilesStayOnWorkerBoundary(t *testing.T) {
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
		for _, needle := range []string{
			`"github.com/gastownhall/gascity/internal/sessionlog"`,
			"worker.NewSessionHandle(",
			"worker.NewSessionCatalog(",
			"session.NewManager(",
			"session.NewManagerWithCityPath(",
			"session.NewManagerWithTransportResolverAndCityPath(",
		} {
			if strings.Contains(content, needle) {
				t.Fatalf("%s contains forbidden worker-boundary bypass %q", path, needle)
			}
		}
	}
}
