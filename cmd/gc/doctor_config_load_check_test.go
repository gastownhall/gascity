package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestConfigLoadCheckReportsFailureInsteadOfDisappearing(t *testing.T) {
	result := newConfigLoadCheck(errors.New(`city import "core": locked but not cached`)).Run(nil)
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error", result.Status)
	}
	if !strings.Contains(result.Message, "config-dependent checks did not run") {
		t.Errorf("message does not say which checks were lost: %q", result.Message)
	}
	if !strings.Contains(result.Message, "locked but not cached") {
		t.Errorf("message drops the underlying cause: %q", result.Message)
	}
}

func TestConfigLoadCheckPassesOnSuccess(t *testing.T) {
	result := newConfigLoadCheck(nil).Run(nil)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok", result.Status)
	}
}

// TestConfigLoadCheckWarnsWhenRepairedMidRunLeftCheckUnregistered is M3: the
// scenario the check's own comment cites, where an earlier check repairs the
// config on disk after config-dependent checks were already (not) registered
// for this run. Before this test, both existing tests passed ctx=nil, so the
// live-reload branch — the only interesting one — never ran, and it could
// report a clean pass here for a report that is still missing ~14 checks.
func TestConfigLoadCheckWarnsWhenRepairedMidRunLeftCheckUnregistered(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	// Registration-time load failed (config was broken then); the live
	// reload below will see the now-valid file an earlier check repaired.
	check := newConfigLoadCheck(errors.New("city import: locked but not cached"))
	result := check.Run(&doctor.CheckContext{CityPath: cityDir})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (registered=false but reload now succeeds)", result.Status)
	}
	if !strings.Contains(result.Message, "NOT registered") {
		t.Errorf("message does not say the checks were not registered this run: %q", result.Message)
	}
}
