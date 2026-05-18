package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestStaleLocalPackDirCheckWarnsForConfiguredActualBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "actual"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
	}, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result=%+v", result.Status, result)
	}
	if !strings.Contains(result.Message, "delete `packs/actual/` (it's stale); edits go via PR on gc-actual-packs") {
		t.Fatalf("message = %q, want operator action", result.Message)
	}
}

func TestStaleLocalPackDirCheckOKWhenConfiguredActualBindingHasNoLocalDir(t *testing.T) {
	cityPath := t.TempDir()

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
	}, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; result=%+v", result.Status, result)
	}
}

func TestStaleLocalPackDirCheckIgnoresActualDirWithoutConfiguredBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "actual"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		"other": {Source: "https://github.com/gastownhall/other-packs"},
	}, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; result=%+v", result.Status, result)
	}
}
