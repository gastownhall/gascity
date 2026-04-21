package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDoRigAdd_Basic(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Adding rig 'my-frontend'") {
		t.Errorf("output missing rig name: %s", output)
	}
	if !strings.Contains(output, "Prefix: mf") {
		t.Errorf("output missing prefix: %s", output)
	}
	if !strings.Contains(output, "Rig added.") {
		t.Errorf("output missing completion: %s", output)
	}

	// Verify city.toml was updated with [[rigs]] entry.
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "my-frontend") {
		t.Errorf("city.toml should contain rig name:\n%s", data)
	}
}

func TestResolveRigAddPath(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	setCwd(t, cwd)
	absPath := filepath.Join(t.TempDir(), "frontend")

	tests := []struct {
		name string
		arg  string
		want string
	}{
		{
			name: "bare relative resolves against city",
			arg:  "frontend",
			want: filepath.Join(cityPath, "frontend"),
		},
		{
			name: "dot-prefixed resolves against cwd",
			arg:  "./frontend",
			want: filepath.Join(cwd, "frontend"),
		},
		{
			name: "absolute path stays absolute",
			arg:  absPath,
			want: absPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRigAddPath(cityPath, tt.arg)
			if err != nil {
				t.Fatalf("resolveRigAddPath(%q): %v", tt.arg, err)
			}
			if !samePath(got, filepath.Clean(tt.want)) {
				t.Fatalf("resolveRigAddPath(%q) = %q, want %q", tt.arg, got, filepath.Clean(tt.want))
			}
		})
	}
}

func TestDoRigAddWritesSiteBindingInsteadOfPath(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	cityData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cityData), rigPath) {
		t.Fatalf("city.toml should not persist rig path after writeCityConfigForEditFS:\n%s", cityData)
	}

	siteData, err := os.ReadFile(config.SiteBindingPath(cityPath))
	if err != nil {
		t.Fatalf("reading .gc/site.toml: %v", err)
	}
	if !strings.Contains(string(siteData), "my-frontend") || !strings.Contains(string(siteData), rigPath) {
		t.Fatalf(".gc/site.toml = %q, want rig binding for %q", siteData, rigPath)
	}
}

func TestDoRigAddRouteFailureRollsBackConfig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	brokenRigFile := filepath.Join(t.TempDir(), "broken-rig")
	if err := os.WriteFile(brokenRigFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityToml := strings.Join([]string{
		"[workspace]",
		`name = "test-city"`,
		"",
		"[[agent]]",
		`name = "mayor"`,
		"",
		"[[rigs]]",
		`name = "broken"`,
		`path = "` + brokenRigFile + `"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "new-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "writing routes") {
		t.Fatalf("stderr = %q, want route failure context", stderr.String())
	}

	cityData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cityData), "new-rig") {
		t.Fatalf("city.toml should roll back the new rig after route failure:\n%s", cityData)
	}
	if !strings.Contains(string(cityData), brokenRigFile) {
		t.Fatalf("city.toml should restore the original broken rig binding after rollback:\n%s", cityData)
	}
	if _, err := os.Stat(config.SiteBindingPath(cityPath)); err == nil {
		t.Fatalf(".gc/site.toml should not be left behind after rollback")
	}
}

func TestDoRigAdd_DuplicateNameDifferentPath(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd should fail for duplicate with different path, got code %d", code)
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "already registered") {
		t.Errorf("stderr should mention already registered: %s", errMsg)
	}
	if !strings.Contains(errMsg, "/some/path") {
		t.Errorf("stderr should mention existing path: %s", errMsg)
	}
}

func TestDoRigAdd_IdempotentSameNameSamePath(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Config already has this rig at the same path.
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Save original config content.
	origData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed for same name+path, got code %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Re-initializing rig") {
		t.Errorf("output should say re-initializing: %s", output)
	}
	if !strings.Contains(output, "Rig re-initialized.") {
		t.Errorf("output should say re-initialized: %s", output)
	}

	// city.toml must be unchanged (no duplicate rig or polecat added).
	newData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(newData) != string(origData) {
		t.Errorf("city.toml should be unchanged on re-add.\nBefore:\n%s\nAfter:\n%s", origData, newData)
	}
}

func TestDoRigAdd_DoesNotWritePortFileForFileBackedExternalRig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "test-external")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("file-backed external rig should not get dolt-server.port, stat err = %v", err)
	}
}

// Regression: re-add must use the rig's configured prefix, not re-derive it.
func TestDoRigAdd_ReAddUsesExistingPrefix(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Rig has explicit prefix "fe" (different from derived "mf").
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	// Must show the configured prefix "fe", not the derived "mf".
	if !strings.Contains(output, "Prefix: fe") {
		t.Errorf("output should show configured prefix 'fe': %s", output)
	}
	if strings.Contains(output, "Prefix: mf") {
		t.Errorf("output should NOT show derived prefix 'mf': %s", output)
	}
}

func TestDoRigAdd_ReAddWarnsDifferingFlags(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Existing rig is NOT suspended.
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with --start-suspended=true (differs from existing).
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/new"}, "", "", true, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "warning") {
		t.Errorf("stderr should warn about flag mismatch: %s", errMsg)
	}
	if !strings.Contains(errMsg, "--start-suspended") {
		t.Errorf("stderr should mention --start-suspended: %s", errMsg)
	}
	if !strings.Contains(errMsg, "--include") {
		t.Errorf("stderr should mention --include: %s", errMsg)
	}
}

func TestDoRigAdd_ReAddNoSpuriousWarning(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir()) // isolate global rig registry
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Existing rig IS suspended with includes.
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nsuspended = true\nincludes = [\"packs/old\"]\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with default flags (no --start-suspended, no --include).
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("stderr should NOT warn when using default flags: %s", stderr.String())
	}
}

func TestDoRigAdd_NotADirectory(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(filePath, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, filePath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for non-directory, got code %d", code)
	}
}

func TestDoRigAdd_RoutesGenerated(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	// Verify routes.jsonl was created for city.
	cityRoutes := filepath.Join(cityPath, ".beads", "routes.jsonl")
	if _, err := os.Stat(cityRoutes); err != nil {
		t.Errorf("city routes.jsonl not created: %v", err)
	}

	// Verify routes.jsonl was created for rig.
	rigRoutes := filepath.Join(rigPath, ".beads", "routes.jsonl")
	if _, err := os.Stat(rigRoutes); err != nil {
		t.Errorf("rig routes.jsonl not created: %v", err)
	}
}

// Regression: Bug 1 — city.toml must not be modified if rig infrastructure
// creation fails. This prevents phantom rigs in config.
func TestDoRigAdd_ConfigUnchangedOnInfraFailure(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalToml := "[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"
	tomlPath := filepath.Join(cityPath, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(originalToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use a fake FS that fails on beads init for the rig.
	f := fsys.NewFake()
	f.Dirs["/fake-rig"] = true
	f.Files[tomlPath] = []byte(originalToml)
	f.Errors[filepath.Join("/fake-rig", ".beads")] = os.ErrPermission

	var stdout, stderr bytes.Buffer
	code := doRigAdd(f, cityPath, "/fake-rig", nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure, got code %d", code)
	}

	// Verify city.toml was NOT modified.
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fake-rig") {
		t.Errorf("city.toml should be unchanged after infrastructure failure:\n%s", data)
	}
}

func TestDoRigList_WithRigs(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create .beads/metadata.json for HQ.
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "test-city (HQ)") {
		t.Errorf("output missing HQ: %s", output)
	}
	if !strings.Contains(output, "Prefix: tc") {
		t.Errorf("output missing HQ prefix: %s", output)
	}
	if !strings.Contains(output, "Beads:  initialized") {
		t.Errorf("output missing HQ beads status: %s", output)
	}
	if !strings.Contains(output, "my-frontend") {
		t.Errorf("output missing rig name: %s", output)
	}
	if !strings.Contains(output, "Prefix: fe") {
		t.Errorf("output missing rig prefix: %s", output)
	}
	if !strings.Contains(output, "not initialized") {
		t.Errorf("output missing rig beads status: %s", output)
	}
}

func TestDoRigList_Empty(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "test-city (HQ)") {
		t.Errorf("output missing HQ: %s", output)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "Path:") {
			t.Errorf("should have no rig paths when empty, got line: %s", line)
		}
	}
}

// Regression: Bug 6 — resolveRigForAgent should match agents to rigs.
func TestResolveRigForAgent(t *testing.T) {
	rigs := []config.Rig{
		{Name: "frontend", Path: "/home/user/frontend"},
		{Name: "backend", Path: "/home/user/backend"},
	}

	if got := resolveRigForAgent("/home/user/frontend", rigs); got != "frontend" {
		t.Errorf("resolveRigForAgent(frontend path) = %q, want %q", got, "frontend")
	}
	if got := resolveRigForAgent("/home/user/backend", rigs); got != "backend" {
		t.Errorf("resolveRigForAgent(backend path) = %q, want %q", got, "backend")
	}
	if got := resolveRigForAgent("/home/user/other", rigs); got != "" {
		t.Errorf("resolveRigForAgent(unmatched path) = %q, want empty", got)
	}
	if got := resolveRigForAgent("/home/user/frontend", nil); got != "" {
		t.Errorf("resolveRigForAgent(nil rigs) = %q, want empty", got)
	}
}

// Regression: trailing slash in rig path must still match.
func TestResolveRigForAgent_TrailingSlash(t *testing.T) {
	rigs := []config.Rig{
		{Name: "frontend", Path: "/home/user/frontend/"},
	}
	if got := resolveRigForAgent("/home/user/frontend", rigs); got != "frontend" {
		t.Errorf("resolveRigForAgent(no trailing slash) = %q, want %q", got, "frontend")
	}

	// Also test workDir with trailing slash, rig path without.
	rigs2 := []config.Rig{
		{Name: "backend", Path: "/home/user/backend"},
	}
	if got := resolveRigForAgent("/home/user/backend/", rigs2); got != "backend" {
		t.Errorf("resolveRigForAgent(trailing slash workDir) = %q, want %q", got, "backend")
	}
}

// ---------------------------------------------------------------------------
// gc rig suspend / resume tests
// ---------------------------------------------------------------------------

func TestDoRigSuspend(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSuspend returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Suspended rig 'frontend'") {
		t.Errorf("output = %q, want suspend message", stdout.String())
	}

	// Verify config written with suspended=true.
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 || !cfg.Rigs[0].Suspended {
		t.Errorf("rig should be suspended, got %+v", cfg.Rigs)
	}
}

func TestDoRigSuspendNotFound(t *testing.T) {
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(cityToml)

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(f, "/city", "nonexistent", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSuspend should fail for unknown rig, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want not found message", stderr.String())
	}
}

func TestDoRigSuspendAlreadySuspended(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\nsuspended = true\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSuspend should be idempotent, got code %d, stderr: %s", code, stderr.String())
	}
}

func TestDoRigResume(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\nsuspended = true\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigResume returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Resumed rig 'frontend'") {
		t.Errorf("output = %q, want resume message", stdout.String())
	}

	// Verify config written with suspended=false.
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].Suspended {
		t.Errorf("rig should not be suspended, got %+v", cfg.Rigs)
	}
}

func TestDoRigResumeNotFound(t *testing.T) {
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(cityToml)

	var stdout, stderr bytes.Buffer
	code := doRigResume(f, "/city", "nonexistent", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigResume should fail for unknown rig, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want not found message", stderr.String())
	}
}

func TestDoRigResumeNotSuspended(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigResume should be idempotent, got code %d, stderr: %s", code, stderr.String())
	}
}

func TestDoRigListShowsSuspended(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nsuspended = true\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "my-frontend (suspended)") {
		t.Errorf("output = %q, want suspended annotation", stdout.String())
	}
}

func TestDoRigAdd_WithPack(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/gastown"}, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/gastown") {
		t.Errorf("output missing include: %s", output)
	}

	// Verify city.toml has includes field.
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 1 || cfg.Rigs[0].Includes[0] != "packs/gastown" {
		t.Errorf("rig includes = %v, want [packs/gastown]; city.toml:\n%s", cfg.Rigs[0].Includes, data)
	}
}

func TestDoRigAdd_WithMultiplePacks(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/planner", "packs/architect"}, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/planner, packs/architect") {
		t.Errorf("output missing combined includes: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"packs/planner", "packs/architect"}
	if !reflect.DeepEqual(cfg.Rigs[0].Includes, want) {
		t.Errorf("rig includes = %v, want %v", cfg.Rigs[0].Includes, want)
	}
}

func TestNewRigAddCmdIncludeFlagIsRepeatable(t *testing.T) {
	cmd := newRigAddCmd(&bytes.Buffer{}, &bytes.Buffer{})
	flag := cmd.Flags().Lookup("include")
	if flag == nil {
		t.Fatal("include flag not registered")
	}
	if flag.Value.Type() != "stringArray" {
		t.Fatalf("include flag type = %q, want stringArray", flag.Value.Type())
	}
}

func TestNewRigCmdRegistersSetEndpointSubcommand(t *testing.T) {
	cmd := newRigCmd(&bytes.Buffer{}, &bytes.Buffer{})
	found := false
	for _, sub := range cmd.Commands() {
		if sub.Name() == "set-endpoint" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("set-endpoint subcommand not registered")
	}
}

func TestDoRigAdd_WithoutPack(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if strings.Contains(output, "Include:") {
		t.Errorf("output should not contain include line when not set: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 0 {
		t.Errorf("rig includes should be empty, got %v", cfg.Rigs[0].Includes)
	}
}

func TestDoRigAdd_DefaultRigIncludes(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// City with default_rig_includes set.
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/gastown\"]\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	// No --include flag → should fall back to default_rig_includes.
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/gastown (default)") {
		t.Errorf("output missing default include: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 1 || cfg.Rigs[0].Includes[0] != "packs/gastown" {
		t.Errorf("rig includes = %v, want [packs/gastown]", cfg.Rigs[0].Includes)
	}
}

func TestDoRigAdd_RootPackDefaultRigImports(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/city-pack\"]\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	packToml := `[pack]
name = "test-city"
schema = 2

[defaults.rig.imports.z-pack]
source = "packs/z-pack"

[defaults.rig.imports.a-pack]
source = "packs/a-pack"
`
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Import: z-pack=packs/z-pack, a-pack=packs/a-pack (default)") {
		t.Errorf("output missing root pack default imports in declaration order: %s", output)
	}
	if !strings.Contains(output, "Include: packs/city-pack (default)") {
		t.Errorf("output missing legacy default include fallback: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if want := []string{"packs/city-pack"}; !reflect.DeepEqual(cfg.Rigs[0].Includes, want) {
		t.Errorf("rig includes = %v, want %v", cfg.Rigs[0].Includes, want)
	}
	if len(cfg.Rigs[0].Imports) != 2 {
		t.Fatalf("len(rig imports) = %d, want 2", len(cfg.Rigs[0].Imports))
	}
	if got := cfg.Rigs[0].Imports["z-pack"].Source; got != "packs/z-pack" {
		t.Errorf("rig imports[z-pack] = %q, want packs/z-pack", got)
	}
	if got := cfg.Rigs[0].Imports["a-pack"].Source; got != "packs/a-pack" {
		t.Errorf("rig imports[a-pack] = %q, want packs/a-pack", got)
	}
}

func TestDoRigAdd_ExplicitIncludeOverridesDefault(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// City with default_rig_includes set.
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/gastown\"]\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	// Explicit --include should override default_rig_includes.
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/custom"}, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/custom") {
		t.Errorf("output missing explicit include: %s", output)
	}
	if strings.Contains(output, "(default)") {
		t.Errorf("output should not show (default) for explicit include: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 1 || cfg.Rigs[0].Includes[0] != "packs/custom" {
		t.Errorf("rig includes = %v, want [packs/custom]", cfg.Rigs[0].Includes)
	}
}

// Regression: doRigAdd must reject rigs with colliding prefixes.
func TestDoRigAdd_PrefixCollision(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// City "my-city" (prefix "mc") already has rig "my-frontend" (prefix "mf").
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"/some/path\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Try to add "my-foo" — derives prefix "mf", collides with "my-frontend".
	rigPath := filepath.Join(t.TempDir(), "my-foo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd should fail for prefix collision, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "collides") {
		t.Errorf("stderr should mention collision: %s", stderr.String())
	}
}

// Explicit --prefix resolves a collision that would otherwise fail.
func TestDoRigAdd_ExplicitPrefixResolvesCollision(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// City "my-city" already has rig "my-frontend" (derived prefix "mf").
	existingRigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(existingRigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf("[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = %q\n", existingRigPath)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// "my-foo" also derives "mf", but an explicit prefix avoids the collision.
	rigPath := filepath.Join(t.TempDir(), "my-foo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "mfoo", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	// Verify the explicit prefix is persisted in city.toml.
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range cfg.Rigs {
		if r.Name == "my-foo" {
			found = true
			if r.Prefix != "mfoo" {
				t.Errorf("rig prefix = %q, want %q", r.Prefix, "mfoo")
			}
			if r.EffectivePrefix() != "mfoo" {
				t.Errorf("EffectivePrefix() = %q, want %q", r.EffectivePrefix(), "mfoo")
			}
		}
	}
	if !found {
		t.Fatal("rig my-foo not found in config")
	}
}

// --prefix must be rejected when the rig's .beads/config.yaml has a different prefix.
func TestDoRigAdd_ExplicitPrefixConflictsWithExistingBeads(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rig already has .beads/config.yaml with prefix "ab".
	rigPath := filepath.Join(t.TempDir(), "alpha-beta")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: ab\nissue-prefix: ab\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "xx", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for conflicting prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "already has bead prefix") {
		t.Errorf("stderr should explain conflict: %s", stderr.String())
	}
}

// Auto-derived prefix must also be rejected when it conflicts with existing .beads.
func TestDoRigAdd_DerivedPrefixConflictsWithExistingBeads(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rig "alpha-beta" would derive prefix "ab", but .beads already has "zz".
	rigPath := filepath.Join(t.TempDir(), "alpha-beta")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: zz\nissue-prefix: zz\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for conflicting derived prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "already has bead prefix") {
		t.Errorf("stderr should explain conflict: %s", stderr.String())
	}
}

// A fresh "gc rig add" against a pre-existing .beads/ directory must fail
// fast and point the user at --adopt — even when the existing prefix would
// have matched the derived one. Falling through to bd init on a populated
// Dolt store produces confusing "signal: killed" failures (see fo-5zeij).
func TestDoRigAdd_ExistingBeadsRequiresAdopt(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rig "alpha-beta" derives prefix "ab", and .beads already has "ab"
	// — so the prefix-conflict guard does not trip and we reach the new
	// "exists without --adopt" guard.
	rigPath := filepath.Join(t.TempDir(), "alpha-beta")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: ab\nissue-prefix: ab\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for pre-existing .beads/ without --adopt, got code %d; stdout: %s", code, stdout.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, ".beads already exists") {
		t.Errorf("stderr should mention pre-existing .beads/: %s", errMsg)
	}
	if !strings.Contains(errMsg, "--adopt") {
		t.Errorf("stderr should hint at --adopt: %s", errMsg)
	}
}

func TestDoRigAdd_ExistingBeadsStatErrorFailsClosed(t *testing.T) {
	f := fsys.NewFake()
	cityPath := "/city"
	rigPath := "/alpha-beta"
	beadsPath := filepath.Join(rigPath, ".beads")

	f.Dirs[filepath.Join(cityPath, ".gc")] = true
	f.Dirs[rigPath] = true
	f.Files[filepath.Join(cityPath, "city.toml")] = []byte("[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n")
	f.Errors[beadsPath] = os.ErrPermission

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(f, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for .beads stat error, got code %d; stdout: %s", code, stdout.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "checking "+beadsPath) {
		t.Fatalf("stderr should identify the .beads stat failure, got: %s", errMsg)
	}
	if _, ok := f.Files[filepath.Join(cityPath, "city.toml")]; !ok {
		t.Fatal("city.toml missing from fake filesystem")
	}
}

func TestReadBeadsPrefix(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantOK  bool
	}{
		{"found", "issue_prefix: ab\n", "ab", true},
		{"with extra keys", "backend: dolt\nissue_prefix: xy\nissue-prefix: xy\n", "xy", true},
		{"missing", "backend: dolt\n", "", false},
		{"empty value", "issue_prefix: \n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			beadsDir := filepath.Join(dir, ".beads")
			if err := os.MkdirAll(beadsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok := readBeadsPrefix(fsys.OSFS{}, dir)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("readBeadsPrefix() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}

	t.Run("no .beads dir", func(t *testing.T) {
		got, ok := readBeadsPrefix(fsys.OSFS{}, t.TempDir())
		if ok || got != "" {
			t.Errorf("readBeadsPrefix() = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("dash form only", func(t *testing.T) {
		dir := t.TempDir()
		beadsDir := filepath.Join(dir, ".beads")
		if err := os.MkdirAll(beadsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue-prefix: zz\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := readBeadsPrefix(fsys.OSFS{}, dir)
		if !ok || got != "zz" {
			t.Errorf("readBeadsPrefix() = (%q, %v), want (\"zz\", true)", got, ok)
		}
	})
}

func TestDoRigAdd_ReAddWarnsDifferingPrefix(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nprefix = \"mf\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with differing --prefix should warn.
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "xx", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "--prefix=xx ignored") {
		t.Errorf("stderr should warn about --prefix mismatch: %s", errMsg)
	}
}

func TestDoRigAdd_PrefixCanonicalizedToLowercase(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "AB", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	// Output should show the lowercased prefix.
	if !strings.Contains(stdout.String(), "Prefix: ab") {
		t.Errorf("prefix should be lowercased to 'ab', got stdout: %s", stdout.String())
	}

	// Verify city.toml stores the lowercase prefix (not raw "AB").
	cfg, err := loadCityConfigFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v", err)
	}
	for _, r := range cfg.Rigs {
		if r.Name == "my-rig" {
			if r.Prefix != "ab" {
				t.Errorf("city.toml Prefix = %q, want %q", r.Prefix, "ab")
			}
			if r.EffectivePrefix() != "ab" {
				t.Errorf("EffectivePrefix() = %q, want %q", r.EffectivePrefix(), "ab")
			}
			break
		}
	}

	// Verify re-add succeeds (no false-positive conflict with .beads).
	var stdout2, stderr2 bytes.Buffer
	code2 := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout2, &stderr2)
	if code2 != 0 {
		t.Errorf("re-add should succeed, got code %d, stderr: %s", code2, stderr2.String())
	}
}

func TestDoRigAdd_PrefixRejectsHyphens(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "my-app", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for hyphenated prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "must not contain hyphens") {
		t.Errorf("expected hyphen error, got: %s", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Pack-preservation tests: write-back must NOT expand includes
// ---------------------------------------------------------------------------

func TestDoRigSuspendPreservesConfig(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(`include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"

[[rigs]]
name = "frontend"
path = "/some/path"
`)
	f.Files["/city/packs/mypack/agents.toml"] = []byte(`[[agent]]
name = "pack-worker"
dir = "myrig"
`)

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(f, "/city", "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	data := string(f.Files["/city/city.toml"])
	if !strings.Contains(data, "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include directive:\n%s", data)
	}
	if strings.Contains(data, "pack-worker") {
		t.Errorf("city.toml should NOT contain expanded pack agent:\n%s", data)
	}
}

func TestDoRigResumePreservesConfig(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(`include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"

[[rigs]]
name = "frontend"
path = "/some/path"
suspended = true
`)
	f.Files["/city/packs/mypack/agents.toml"] = []byte(`[[agent]]
name = "pack-worker"
dir = "myrig"
`)

	var stdout, stderr bytes.Buffer
	code := doRigResume(f, "/city", "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	data := string(f.Files["/city/city.toml"])
	if !strings.Contains(data, "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include directive:\n%s", data)
	}
	if strings.Contains(data, "pack-worker") {
		t.Errorf("city.toml should NOT contain expanded pack agent:\n%s", data)
	}
}

func TestDoRigAddPreservesConfig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create city.toml with include directive (must be top-level, before any [section]).
	cityToml := `include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create the pack fragment (so LoadWithIncludes would find it, but we don't use it).
	packDir := filepath.Join(cityPath, "packs", "mypack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "agents.toml"), []byte("[[agent]]\nname = \"pack-worker\"\ndir = \"myrig\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include directive:\n%s", data)
	}
	if strings.Contains(string(data), "pack-worker") {
		t.Errorf("city.toml should NOT contain expanded pack agent:\n%s", data)
	}
	if !strings.Contains(string(data), "my-rig") {
		t.Errorf("city.toml should contain new rig:\n%s", data)
	}
}

func TestDoRigAdd_AdoptExistingBeads(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "adopted-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"adopted-rig","issue_prefix":"ar"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	configYaml := "issue_prefix: ar\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(configYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ar", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd --adopt returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Adopted existing beads database") {
		t.Errorf("output should mention adoption: %s", output)
	}
	if strings.Contains(output, "Initialized beads database") {
		t.Errorf("output should NOT mention initialization when adopting: %s", output)
	}
}

func TestDoRigAdd_AdoptRequiresMetadataJSON(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "no-beads-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when .beads/metadata.json missing, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "--adopt requires .beads/metadata.json") {
		t.Errorf("error should mention missing metadata.json: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptRequiresExistingDir(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "does-not-exist")

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for non-existent dir with --adopt, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "--adopt requires an existing directory") {
		t.Errorf("error should mention existing directory requirement: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptNonGitDirSucceeds(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create rig without .git — should succeed with --adopt.
	rigPath := filepath.Join(t.TempDir(), "no-git-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"no-git-rig","issue_prefix":"ng"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	configYaml := "issue_prefix: ng\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(configYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ng", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd --adopt on non-git dir returned %d, stderr: %s", code, stderr.String())
	}

	// Non-git dirs should succeed without printing the git detection message.
	if strings.Contains(stdout.String(), "Detected git repo") {
		t.Errorf("non-git dir should not trigger git detection message, got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Adopted existing beads database") {
		t.Errorf("output should mention adoption: %s", stdout.String())
	}
}

func TestDoRigAdd_AdoptRequiresConfigYaml(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create rig with metadata.json but no config.yaml.
	rigPath := filepath.Join(t.TempDir(), "no-config-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"no-config-rig","issue_prefix":"nc"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "nc", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when .beads/config.yaml missing, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "valid issue_prefix") {
		t.Errorf("error should mention missing prefix: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptRejectsEmptyConfigYaml(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create rig with config.yaml that has no issue_prefix key.
	rigPath := filepath.Join(t.TempDir(), "empty-config-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"empty-config-rig"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	// config.yaml exists but has no issue_prefix
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("some_other_key: val\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ec", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when config.yaml lacks issue_prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "valid issue_prefix") {
		t.Errorf("error should mention missing prefix: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptWithoutPrefixMismatch(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create rig whose directory basename ("mismatch-rig") derives a prefix
	// ("mismatchrig") that differs from config.yaml's prefix ("xr").
	rigPath := filepath.Join(t.TempDir(), "mismatch-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"mismatch-rig","issue_prefix":"xr"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	configYaml := "issue_prefix: xr\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(configYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// No --prefix: derived prefix from basename "mismatch-rig" won't match "xr".
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected prefix mismatch failure, got code %d, stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "already has bead prefix") {
		t.Errorf("error should mention prefix mismatch: %s", stderr.String())
	}
}
