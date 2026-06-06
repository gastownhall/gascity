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
	}, nil, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result=%+v", result.Status, result)
	}
	if result.Message != "stale local pack directory: packs/actual" {
		t.Fatalf("message = %q, want stale local pack directory summary", result.Message)
	}
	if !strings.Contains(result.FixHint, "delete `packs/actual/` (it's stale); edits go via PR on gc-actual-packs") {
		t.Fatalf("fix hint = %q, want operator action", result.FixHint)
	}
}

func TestStaleLocalPackDirCheckOKWhenConfiguredActualBindingHasNoLocalDir(t *testing.T) {
	cityPath := t.TempDir()

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
	}, nil, nil, cityPath)

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
	}, nil, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; result=%+v", result.Status, result)
	}
}

func TestStaleLocalPackDirCheckWarnsForRemoteImportBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "actual"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	check := NewStaleLocalPackDirCheck(nil, map[string]config.Import{
		"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
	}, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result=%+v", result.Status, result)
	}
	if result.Message != "stale local pack directory: packs/actual" {
		t.Fatalf("message = %q, want stale local pack directory summary", result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "[imports.actual]") {
		t.Fatalf("details = %#v, want import binding detail", result.Details)
	}
}

func TestStaleLocalPackDirCheckWarnsForDefaultRigRemoteImportBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "actual"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	check := NewStaleLocalPackDirCheck(nil, nil, map[string]config.Import{
		"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
	}, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result=%+v", result.Status, result)
	}
	if result.Message != "stale local pack directory: packs/actual" {
		t.Fatalf("message = %q, want stale local pack directory summary", result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "[defaults.rig.imports.actual]") {
		t.Fatalf("details = %#v, want default rig import binding detail", result.Details)
	}
}

func TestStaleLocalPackDirCheckWarnsForRigRemoteImportBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "actual"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	rig := config.Rig{
		Name: "demo-rig",
		Imports: map[string]config.Import{
			"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
		},
	}
	check := NewStaleLocalPackDirCheck(nil, nil, nil, cityPath, rig)

	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result=%+v", result.Status, result)
	}
	if result.Message != "stale local pack directory: packs/actual" {
		t.Fatalf("message = %q, want stale local pack directory summary", result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "[rigs.demo-rig.imports.actual]") {
		t.Fatalf("details = %#v, want rig import binding detail", result.Details)
	}
}

func TestStaleLocalPackDirCheckDedupesSameLocalPackDirForPackAndImport(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "actual"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
	}, map[string]config.Import{
		"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
	}, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result=%+v", result.Status, result)
	}
	if result.Message != "stale local pack directory: packs/actual" {
		t.Fatalf("message = %q, want single stale directory summary", result.Message)
	}
	if len(result.Details) != 1 {
		t.Fatalf("details = %#v, want one entry for one physical directory", result.Details)
	}
	if !strings.Contains(result.Details[0], "[imports.actual]") || !strings.Contains(result.Details[0], "[packs.actual]") {
		t.Fatalf("details = %#v, want both config references", result.Details)
	}
}

func TestStaleLocalPackDirCheckIgnoresLocalImportBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "actual"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	check := NewStaleLocalPackDirCheck(nil, map[string]config.Import{
		"actual": {Source: "./packs/actual"},
	}, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; result=%+v", result.Status, result)
	}
}

func TestStaleLocalPackDirCheckReportsMultipleBindingsInSortedOrder(t *testing.T) {
	cityPath := t.TempDir()
	for _, name := range []string{"beta", "alpha"} {
		if err := os.MkdirAll(filepath.Join(cityPath, "packs", name), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", name, err)
		}
	}

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		"beta":  {Source: "https://github.com/gastownhall/beta-packs"},
		"alpha": {Source: "https://github.com/gastownhall/alpha-packs"},
	}, nil, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result=%+v", result.Status, result)
	}
	if result.Message != "2 stale local pack directories" {
		t.Fatalf("message = %q, want multi-binding summary", result.Message)
	}
	if len(result.Details) != 2 {
		t.Fatalf("details = %#v, want two entries", result.Details)
	}
	if !strings.Contains(result.Details[0], "packs/alpha") || !strings.Contains(result.Details[1], "packs/beta") {
		t.Fatalf("details = %#v, want sorted alpha then beta", result.Details)
	}
}

func TestStaleLocalPackDirCheckIgnoresFileInsteadOfDirectory(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "packs", "actual"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		"actual": {Source: "https://github.com/gastownhall/gc-actual-packs"},
	}, nil, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; result=%+v", result.Status, result)
	}
}

func TestStaleLocalPackDirCheckIgnoresUnsafeBindingNames(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "actual"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		"..":        {Source: "https://github.com/gastownhall/parent-packs"},
		"../escape": {Source: "https://github.com/gastownhall/escape-packs"},
		"/absolute": {Source: "https://github.com/gastownhall/absolute-packs"},
		".":         {Source: "https://github.com/gastownhall/current-packs"},
	}, nil, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; result=%+v", result.Status, result)
	}
}

func TestStaleLocalPackDirCheckContinuesAfterStatError(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "zeta"), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	tooLongBinding := strings.Repeat("a", 5000)

	check := NewStaleLocalPackDirCheck(map[string]config.PackSource{
		tooLongBinding: {Source: "https://github.com/gastownhall/too-long-packs"},
		"zeta":         {Source: "https://github.com/gastownhall/zeta-packs"},
	}, nil, nil, cityPath)

	result := check.Run(&CheckContext{})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result=%+v", result.Status, result)
	}
	if result.Message != "stale local pack directory: packs/zeta" {
		t.Fatalf("message = %q, want stale zeta summary", result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "could not inspect [packs.") {
		t.Fatalf("details = %#v, want inspection failure detail", result.Details)
	}
	if !strings.Contains(joined, "packs/zeta exists while [packs.zeta]") {
		t.Fatalf("details = %#v, want stale zeta detail after stat failure", result.Details)
	}
}
