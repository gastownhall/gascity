package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

type mkdirAllErrorFS struct {
	fsys.FS
	path string
	err  error
}

func (f mkdirAllErrorFS) MkdirAll(path string, perm os.FileMode) error {
	if path == f.path {
		return f.err
	}
	return f.FS.MkdirAll(path, perm)
}

func writeSchema2RigCity(t *testing.T, cityPath, workspaceName, cityToml, siteToml string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	packToml := fmt.Sprintf("[pack]\nname = %q\nschema = 2\n", workspaceName)
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if cityToml == "" {
		cityToml = "[workspace]\n"
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if siteToml == "" {
		siteToml = fmt.Sprintf("workspace_name = %q\n", workspaceName)
	}
	if err := os.WriteFile(config.SiteBindingPath(cityPath), []byte(siteToml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSchema2RigCityFS(t *testing.T, f *fsys.Fake, cityPath, workspaceName, cityToml string) {
	t.Helper()
	f.Dirs[cityPath] = true
	f.Dirs[filepath.Join(cityPath, ".gc")] = true
	f.Files[filepath.Join(cityPath, "pack.toml")] = []byte(fmt.Sprintf("[pack]\nname = %q\nschema = 2\n", workspaceName))
	if cityToml == "" {
		cityToml = "[workspace]\n"
	}
	f.Files[filepath.Join(cityPath, "city.toml")] = []byte(cityToml)
	f.Files[config.SiteBindingPath(cityPath)] = []byte(fmt.Sprintf("workspace_name = %q\n", workspaceName))
}

func TestDoRigAdd_Basic(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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

func TestDoRigAdd_PreservesComments(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := `# This is a city-level rationale comment.
[workspace]
name = "my-city"

# Pack rationale: core tools are required.
[packs.core]
source = ".gc/system/packs/core"
`
	writeSchema2RigCity(t, cityPath, "my-city", cityToml, "")

	rigPath := filepath.Join(t.TempDir(), "backend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"# This is a city-level rationale comment.",
		"# Pack rationale: core tools are required.",
		"backend",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("city.toml missing %q:\n%s", want, got)
		}
	}
}

func TestDoRigAddSqliteCityCreatesBdBackedRig(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "tincan")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "sqlite-city"
prefix = "ga"

[beads]
provider = "sqlite"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "bd.log")
	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	script := fmt.Sprintf(`#!/bin/sh
printf 'pwd=%%s BEADS_DIR=%%s args=%%s\n' "$PWD" "${BEADS_DIR:-}" "$*" >> %q
case "$1" in
  init)
    mkdir -p "${BEADS_DIR:-$PWD/.beads}"
    exit 0
    ;;
  list)
    printf '[]\n'
    exit 0
    ;;
  update)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, logFile)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "tc", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if got := rawBeadsProviderForScope(rigPath, cityPath); got != "bd" {
		t.Fatalf("rawBeadsProviderForScope(rig) = %q, want bd", got)
	}
	if got := rawBeadsProviderForScope(cityPath, cityPath); got != "sqlite" {
		t.Fatalf("rawBeadsProviderForScope(city) = %q, want sqlite", got)
	}
	metaData, err := os.ReadFile(filepath.Join(rigPath, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("ReadFile(metadata): %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata): %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["dolt_mode"])); got != "server" {
		t.Fatalf("metadata dolt_mode = %q, want server", got)
	}

	store, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if err := store.SetMetadata("tc-1", "gc.routed_to", "sample/session-a"); err != nil {
		t.Fatalf("SetMetadata through rig store: %v", err)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	log := string(logData)
	for _, want := range []string{
		"init --server -p tc --skip-hooks --database tc",
		"update --json tc-1 --set-metadata gc.routed_to=sample/session-a",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("bd log missing %q:\n%s", want, log)
		}
	}
}

func runGitInTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func makeMasterRig(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	runGitInTest(t, bare, "init", "--bare")

	rigPath := filepath.Join(t.TempDir(), "master-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitInTest(t, rigPath, "init")
	runGitInTest(t, rigPath, "config", "user.email", "test@test.com")
	runGitInTest(t, rigPath, "config", "user.name", "Test")
	runGitInTest(t, rigPath, "checkout", "-b", "master")
	runGitInTest(t, rigPath, "commit", "--allow-empty", "-m", "init")
	runGitInTest(t, rigPath, "remote", "add", "origin", bare)
	runGitInTest(t, rigPath, "push", "-u", "origin", "master")
	runGitInTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/master")
	runGitInTest(t, rigPath, "remote", "set-head", "origin", "master")
	return rigPath
}

func TestDoRigAdd_DetectsDefaultBranchFromOriginHEAD(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := makeMasterRig(t)

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Default branch: master") {
		t.Errorf("output should report detected default branch master:\n%s", stdout.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `default_branch = "master"`) {
		t.Errorf("city.toml should record default_branch=master:\n%s", data)
	}
}

func TestDoRigAdd_DefaultBranchFlagOverridesProbe(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := makeMasterRig(t)

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "develop", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Default branch: develop") {
		t.Errorf("output should report flag-supplied default branch develop:\n%s", stdout.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `default_branch = "develop"`) {
		t.Errorf("city.toml should record flag-supplied default_branch=develop:\n%s", data)
	}
}

func TestDoRigAdd_BackfillsExistingRigDefaultBranch(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := makeMasterRig(t)
	writeSchema2RigCity(
		t,
		cityPath,
		"test-city",
		"[workspace]\n\n[[rigs]]\nname = \"master-rig\"\n",
		fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"master-rig\"\npath = %q\n", rigPath),
	)

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `default_branch = "master"`) {
		t.Errorf("city.toml should backfill default_branch=master on re-add:\n%s", data)
	}
}

func TestDoRigAdd_NonGitDirOmitsDefaultBranch(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "no-git")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	if strings.Contains(stdout.String(), "Default branch:") {
		t.Errorf("output should not report a default branch for non-git dir:\n%s", stdout.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "default_branch") {
		t.Errorf("city.toml should not record default_branch when probe finds nothing:\n%s", data)
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
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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

	brokenRigFile := filepath.Join(t.TempDir(), "broken-rig")
	if err := os.WriteFile(brokenRigFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeSchema2RigCity(
		t,
		cityPath,
		"test-city",
		"[workspace]\n\n[[rigs]]\nname = \"broken\"\n",
		fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"broken\"\npath = %q\n", brokenRigFile),
	)

	rigPath := filepath.Join(t.TempDir(), "new-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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
	siteData, err := os.ReadFile(config.SiteBindingPath(cityPath))
	if err != nil {
		t.Fatalf("reading .gc/site.toml after rollback: %v", err)
	}
	if !strings.Contains(string(siteData), brokenRigFile) {
		t.Fatalf(".gc/site.toml should restore the original broken rig binding after rollback:\n%s", siteData)
	}
}

func TestDoRigAdd_DuplicateNameDifferentPath(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(
		t,
		cityPath,
		"test-city",
		"[workspace]\n\n[[rigs]]\nname = \"frontend\"\n",
		"workspace_name = \"test-city\"\n\n[[rig]]\nname = \"frontend\"\npath = \"/some/path\"\n",
	)

	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Config already has this rig at the same path.
	writeSchema2RigCity(
		t,
		cityPath,
		"test-city",
		"[workspace]\n\n[[rigs]]\nname = \"my-frontend\"\n",
		fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"my-frontend\"\npath = %q\n", rigPath),
	)

	// Save original config content.
	origData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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

	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "workspace_name = \"test-city\"\n")

	rigPath := filepath.Join(t.TempDir(), "test-external")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Rig has explicit prefix "fe" (different from derived "mf").
	writeSchema2RigCity(
		t,
		cityPath,
		"test-city",
		"[workspace]\n\n[[rigs]]\nname = \"my-frontend\"\nprefix = \"fe\"\n",
		fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"my-frontend\"\npath = %q\n", rigPath),
	)

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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

func TestDoRigAdd_ReAddMissingPathUsesCandidateConfig(t *testing.T) {
	cityPath := t.TempDir()

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	writeSchema2RigCity(
		t,
		cityPath,
		"test-city",
		"[workspace]\n\n[[rigs]]\nname = \"my-frontend\"\n",
		"workspace_name = \"test-city\"\n",
	)

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Rig re-initialized.") {
		t.Fatalf("stdout should report re-initialization, got: %s", stdout.String())
	}
}

func TestDoRigAdd_ReAddWarnsDifferingFlags(t *testing.T) {
	cityPath := t.TempDir()

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Existing rig is NOT suspended.
	writeSchema2RigCity(
		t,
		cityPath,
		"test-city",
		"[workspace]\n\n[[rigs]]\nname = \"my-frontend\"\n",
		fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"my-frontend\"\npath = %q\n", rigPath),
	)

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with --start-suspended=true (differs from existing).
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/new"}, "", "", "", true, false, &stdout, &stderr)
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

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Existing rig IS suspended with includes.
	writeSchema2RigCity(
		t,
		cityPath,
		"test-city",
		"[workspace]\n\n[[rigs]]\nname = \"my-frontend\"\nsuspended = true\nincludes = [\"packs/old\"]\n",
		fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"my-frontend\"\npath = %q\n", rigPath),
	)

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with default flags (no --start-suspended, no --include).
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("stderr should NOT warn when using default flags: %s", stderr.String())
	}
}

func TestDoRigAdd_NotADirectory(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test", "[workspace]\n", "")

	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(filePath, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, filePath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for non-directory, got code %d", code)
	}
}

func TestDoRigAdd_RoutesGenerated(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "my-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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
	writeSchema2RigCity(t, cityPath, "test", "[workspace]\n", "")
	tomlPath := filepath.Join(cityPath, "city.toml")
	originalBytes, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	originalToml := string(originalBytes)

	// Use a fake FS that fails on beads init for the rig.
	f := fsys.NewFake()
	writeSchema2RigCityFS(t, f, cityPath, "test", originalToml)
	f.Dirs["/fake-rig"] = true
	f.Errors[filepath.Join("/fake-rig", ".beads")] = os.ErrPermission

	var stdout, stderr bytes.Buffer
	code := doRigAdd(f, cityPath, "/fake-rig", nil, "", "", "", false, false, &stdout, &stderr)
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

func TestDoRigAdd_RootPackDefaultRigImportsErrorDoesNotMutateRig(t *testing.T) {
	f := fsys.NewFake()
	cityPath := "/city"
	rigPath := "/rigs/my-project"
	writeSchema2RigCityFS(t, f, cityPath, "test", "[workspace]\n")
	originalToml := string(f.Files[filepath.Join(cityPath, "city.toml")])
	f.Errors[filepath.Join(cityPath, "pack.toml")] = errors.New("read denied")

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(f, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure, got code %d; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc rig add: loading root pack defaults: loading city pack.toml") {
		t.Fatalf("stderr should mention root pack defaults load failure, got: %s", stderr.String())
	}
	if f.Dirs[rigPath] {
		t.Fatalf("rig directory should not be created before root pack defaults load succeeds")
	}
	if got := string(f.Files[filepath.Join(cityPath, "city.toml")]); got != originalToml {
		t.Fatalf("city.toml changed unexpectedly:\n%s", got)
	}
}

func TestDoRigAdd_ExplicitIncludeSkipsUnusedDefaultRigImportErrors(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/one/shared\", \"packs/two/shared\"]\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("not = [valid\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/custom"}, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "default rig imports") || strings.Contains(stderr.String(), "pack.toml") {
		t.Fatalf("explicit include should not load unused defaults; stderr: %s", stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Rigs[0].Imports["custom"].Source; got != "./packs/custom" {
		t.Fatalf("rig imports[custom] = %q, want ./packs/custom", got)
	}
}

func TestDoRigAdd_CandidateValidationErrorDoesNotCreateMissingRig(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\n\n[[rigs]]\nname = \"registered\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, "")
	cityTomlPath := filepath.Join(cityPath, "city.toml")

	rigPath := filepath.Join(t.TempDir(), "my-project")

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected validation failure, got code %d; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), `rig "registered": path is required`) {
		t.Fatalf("stderr should mention rig validation failure, got: %s", stderr.String())
	}
	if _, err := os.Stat(rigPath); !os.IsNotExist(err) {
		t.Fatalf("rig directory should not be created before candidate validation succeeds, stat err: %v", err)
	}
	data, err := os.ReadFile(cityTomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != cityToml {
		t.Fatalf("city.toml changed unexpectedly:\n%s", data)
	}
}

func TestDoRigAdd_CreateMissingRigDirectoryError(t *testing.T) {
	base := fsys.NewFake()
	cityPath := "/city"
	rigPath := "/rigs/my-project"
	writeSchema2RigCityFS(t, base, cityPath, "test", "[workspace]\n")
	originalToml := string(base.Files[filepath.Join(cityPath, "city.toml")])
	mkdirErr := errors.New("mkdir denied")

	f := mkdirAllErrorFS{FS: base, path: rigPath, err: mkdirErr}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(f, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected mkdir failure, got code %d; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc rig add: creating "+rigPath+": mkdir denied") {
		t.Fatalf("stderr should mention rig directory create failure, got: %s", stderr.String())
	}
	if base.Dirs[rigPath] {
		t.Fatalf("rig directory should not be recorded after MkdirAll failure")
	}
	if got := string(base.Files[filepath.Join(cityPath, "city.toml")]); got != originalToml {
		t.Fatalf("city.toml changed unexpectedly:\n%s", got)
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

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nprefix = \"fe\"\ndefault_branch = \"develop\"\n"
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
	if !strings.Contains(output, "Default branch: develop") {
		t.Errorf("output missing rig default branch: %s", output)
	}
	if !strings.Contains(output, "not initialized") {
		t.Errorf("output missing rig beads status: %s", output)
	}
}

func TestDoRigListJSONShowsDefaultBranch(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\ndefault_branch = \"develop\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}

	var got struct {
		Rigs []struct {
			Name          string `json:"name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"rigs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode rig list JSON: %v\n%s", err, stdout.String())
	}
	for _, rig := range got.Rigs {
		if rig.Name == "my-frontend" {
			if rig.DefaultBranch != "develop" {
				t.Fatalf("default_branch = %q, want develop\n%s", rig.DefaultBranch, stdout.String())
			}
			return
		}
	}
	t.Fatalf("rig my-frontend not found in JSON:\n%s", stdout.String())
}

// TestDoRigListJSONBuildsSessionProviderOnce guards against the rig-list --json
// N+1: the running-status probe must build a single session provider and reuse
// it across all rigs, not reconstruct one per rig.
func TestDoRigListJSONBuildsSessionProviderOnce(t *testing.T) {
	cityPath := t.TempDir()

	var rigBlocks strings.Builder
	for _, name := range []string{"rig-a", "rig-b", "rig-c"} {
		rigPath := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&rigBlocks, "\n[[rigs]]\nname = %q\npath = %q\n", name, rigPath)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n" + rigBlocks.String()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls int
	orig := rigListSessionProvider
	rigListSessionProvider = func() (runtime.Provider, error) {
		calls++
		return &fakeAdoptionProvider{}, nil
	}
	t.Cleanup(func() { rigListSessionProvider = orig })

	var stdout, stderr bytes.Buffer
	if code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}

	if calls != 1 {
		t.Fatalf("session provider constructed %d times across 3 rigs, want exactly 1", calls)
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
	siteToml := "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n\n[[rigs]]\nname = \"frontend\"\n", siteToml)

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSuspend returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Suspended rig 'frontend'") {
		t.Errorf("output = %q, want suspend message", stdout.String())
	}

	// Verify suspension recorded in runtime state, not city.toml.
	st, err := suspensionstate.Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !suspensionstate.IsRigSuspended(st, "frontend") {
		t.Errorf("rig should be suspended in runtime state, got %+v", st)
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].Suspended {
		t.Errorf("city.toml should NOT have suspended=true, got %+v", cfg.Rigs)
	}
}

func TestDoRigSuspendNotFound(t *testing.T) {
	f := fsys.NewFake()
	writeSchema2RigCityFS(t, f, "/city", "test-city", "[workspace]\n")

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
	cityToml := "[workspace]\n\n[[rigs]]\nname = \"frontend\"\nsuspended = true\n"
	siteToml := "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, siteToml)

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSuspend should be idempotent, got code %d, stderr: %s", code, stderr.String())
	}
}

func TestDoRigResume(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\n\n[[rigs]]\nname = \"frontend\"\nsuspended_on_start = true\n"
	siteToml := "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, siteToml)

	var stdout, stderr bytes.Buffer
	code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigResume returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Resumed rig 'frontend'") {
		t.Errorf("output = %q, want resume message", stdout.String())
	}

	// city.toml is intentionally NOT mutated; the explicit resume is
	// recorded in .gc/runtime/rig-state.json and beats SuspendedOnStart.
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 || !cfg.Rigs[0].SuspendedOnStart {
		t.Errorf("city.toml suspended_on_start should remain set, got %+v", cfg.Rigs)
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("suspensionstate.Load: %v", err)
	}
	if v, ok := suspensionstate.ExplicitRig(st, "frontend"); !ok || v {
		t.Errorf("frontend should have explicit resume in runtime state, got (%v, %v)", v, ok)
	}
	if suspensionstate.EffectiveRigSuspended(st, "frontend", cfg.Rigs[0].SuspendedOnStart) {
		t.Error("explicit resume in runtime state must beat suspended_on_start=true")
	}
}

func TestDoRigResumeNotFound(t *testing.T) {
	f := fsys.NewFake()
	writeSchema2RigCityFS(t, f, "/city", "test-city", "[workspace]\n")

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
	siteToml := "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n\n[[rigs]]\nname = \"frontend\"\n", siteToml)

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

	cityToml := "[workspace]\n\n[[rigs]]\nname = \"my-frontend\"\nsuspended_on_start = true\n"
	siteToml := fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"my-frontend\"\npath = %q\n", rigPath)
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, siteToml)

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
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	// A real local pack at packs/gastown: the include token must stay a
	// local import instead of canonicalizing to the bundled source.
	if err := os.MkdirAll(filepath.Join(cityPath, "packs", "gastown"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "packs", "gastown", "pack.toml"), []byte("[pack]\nname = \"gastown\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/gastown"}, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Import: gastown=./packs/gastown") {
		t.Errorf("output missing import: %s", output)
	}

	// Verify city.toml stores canonical rig imports instead of legacy includes.
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
	if len(cfg.Rigs[0].Includes) != 0 {
		t.Errorf("rig includes should stay empty, got %v; city.toml:\n%s", cfg.Rigs[0].Includes, data)
	}
	if got := cfg.Rigs[0].Imports["gastown"].Source; got != "./packs/gastown" {
		t.Errorf("rig imports[gastown] = %q, want ./packs/gastown; city.toml:\n%s", got, data)
	}
}

func TestDoRigAdd_ExplicitIncludeResolvesPackAlias(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"

[packs.ops]
source = "https://github.com/acme/ops-pack.git"
path = "roles"
ref = "v1.2.3"
`
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"ops"}, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	wantSource := "https://github.com/acme/ops-pack/tree/v1.2.3/roles"
	if !strings.Contains(stdout.String(), "Import: ops="+wantSource) {
		t.Fatalf("output missing resolved import: %s", stdout.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Rigs[0].Imports["ops"].Source; got != wantSource {
		t.Fatalf("rig imports[ops] = %q, want %q", got, wantSource)
	}
}

func TestDoRigAdd_WithMultiplePacks(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/planner", "packs/architect"}, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Import: architect=./packs/architect, planner=./packs/planner") {
		t.Errorf("output missing combined imports: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs[0].Includes) != 0 {
		t.Errorf("rig includes should stay empty, got %v", cfg.Rigs[0].Includes)
	}
	want := map[string]string{"planner": "./packs/planner", "architect": "./packs/architect"}
	if len(cfg.Rigs[0].Imports) != len(want) {
		t.Fatalf("rig imports = %#v, want %d entries", cfg.Rigs[0].Imports, len(want))
	}
	for binding, source := range want {
		if got := cfg.Rigs[0].Imports[binding].Source; got != source {
			t.Errorf("rig imports[%s] = %q, want %q", binding, got, source)
		}
	}
}

func TestDoRigAdd_DefaultRigIncludesResolvePackAlias(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"
default_rig_includes = ["ops"]

[[agent]]
name = "mayor"

[packs.ops]
source = "https://github.com/acme/ops-pack.git"
path = "roles"
ref = "v1.2.3"
`
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	wantSource := "https://github.com/acme/ops-pack/tree/v1.2.3/roles"
	if !strings.Contains(stdout.String(), "Import: ops="+wantSource+" (default)") {
		t.Fatalf("output missing resolved default import: %s", stdout.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Rigs[0].Imports["ops"].Source; got != wantSource {
		t.Fatalf("rig imports[ops] = %q, want %q", got, wantSource)
	}
}

func TestNewRigAddCmdIncludeFlagIsRepeatable(t *testing.T) {
	cmd := newRigAddCmd(&bytes.Buffer{}, &bytes.Buffer{})
	flag := cmd.Flags().Lookup("include")
	switch {
	case flag == nil:
		t.Fatal("include flag not registered")
	case flag.Value.Type() != "stringArray":
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
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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
	// No --include flag → should convert legacy default_rig_includes into
	// canonical rig imports for this compatibility wave.
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Import: gastown=./packs/gastown (default)") {
		t.Errorf("output missing default import: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 0 {
		t.Errorf("rig includes should stay empty, got %v", cfg.Rigs[0].Includes)
	}
	if got := cfg.Rigs[0].Imports["gastown"].Source; got != "./packs/gastown" {
		t.Errorf("rig imports[gastown] = %q, want ./packs/gastown", got)
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Import: a-pack=packs/a-pack, city-pack=./packs/city-pack, z-pack=packs/z-pack (default)") {
		t.Errorf("output missing merged default imports: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 0 {
		t.Errorf("rig includes should stay empty, got %v", cfg.Rigs[0].Includes)
	}
	if len(cfg.Rigs[0].Imports) != 3 {
		t.Fatalf("len(rig imports) = %d, want 3", len(cfg.Rigs[0].Imports))
	}
	if got := cfg.Rigs[0].Imports["z-pack"].Source; got != "packs/z-pack" {
		t.Errorf("rig imports[z-pack] = %q, want packs/z-pack", got)
	}
	if got := cfg.Rigs[0].Imports["a-pack"].Source; got != "packs/a-pack" {
		t.Errorf("rig imports[a-pack] = %q, want packs/a-pack", got)
	}
	if got := cfg.Rigs[0].Imports["city-pack"].Source; got != "./packs/city-pack" {
		t.Errorf("rig imports[city-pack] = %q, want ./packs/city-pack", got)
	}
}

func TestDoRigAdd_RootPackDefaultRigImportPackAliasCollisionUniquifiesLegacy(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"
default_rig_includes = ["shared"]

[[agent]]
name = "mayor"

[packs.shared]
source = "github.com/bar/B"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	packToml := `[pack]
name = "test-city"
schema = 2

[defaults.rig.imports.shared]
source = "github.com/foo/A"
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Import: shared=github.com/foo/A, shared-2=github.com/bar/B (default)") {
		t.Fatalf("output missing uniquified default imports: %s", stdout.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if got := cfg.Rigs[0].Imports["shared"].Source; got != "github.com/foo/A" {
		t.Fatalf("rig imports[shared] = %q, want github.com/foo/A", got)
	}
	if got := cfg.Rigs[0].Imports["shared-2"].Source; got != "github.com/bar/B" {
		t.Fatalf("rig imports[shared-2] = %q, want github.com/bar/B", got)
	}
}

func TestDoRigAdd_DefaultRigIncludesUniquifyDuplicateDerivedBindings(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"
default_rig_includes = ["github.com/acme/shared", "github.com/other/shared"]

[[agent]]
name = "mayor"
`
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "Import: shared=github.com/acme/shared, shared-2=github.com/other/shared (default)") {
		t.Fatalf("output missing uniquified default imports: %s", stdout.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if got := cfg.Rigs[0].Imports["shared"].Source; got != "github.com/acme/shared" {
		t.Fatalf("rig imports[shared] = %q, want github.com/acme/shared", got)
	}
	if got := cfg.Rigs[0].Imports["shared-2"].Source; got != "github.com/other/shared" {
		t.Fatalf("rig imports[shared-2] = %q, want github.com/other/shared", got)
	}
}

func TestDoRigAdd_RealGastownExampleRootPackDefaultRigImport(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	examplePath, err := filepath.Abs(filepath.Join("..", "..", "examples", "gastown"))
	if err != nil {
		t.Fatalf("resolving examples/gastown: %v", err)
	}
	cityPath := filepath.Join(t.TempDir(), "city")

	var initStdout, initStderr bytes.Buffer
	code := doInitFromDirWithOptions(examplePath, cityPath, "", &initStdout, &initStderr, true)
	if code != 0 {
		t.Fatalf("doInitFromDirWithOptions = %d, want 0; stderr: %s", code, initStderr.String())
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code = doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	// The example city composes gastown via the pinned public import, not a
	// checked-in packs/gastown copy, so the default rig import carries the
	// public source.
	if !strings.Contains(stdout.String(), "Import: gastown="+config.PublicGastownPackSource+" (default)") {
		t.Fatalf("output missing gastown default import: %s", stdout.String())
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("len(Rigs) = %d, want 1", len(cfg.Rigs))
	}
	if got := cfg.Rigs[0].Imports["gastown"].Source; got != config.PublicGastownPackSource {
		t.Fatalf("rig gastown import source = %q, want %q", got, config.PublicGastownPackSource)
	}
	if got := cfg.Rigs[0].Imports["gastown"].Version; got != config.PublicGastownPackVersion {
		t.Fatalf("rig gastown import version = %q, want %q", got, config.PublicGastownPackVersion)
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
	// Explicit --include should override default_rig_includes while still
	// writing canonical rig imports.
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/custom"}, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Import: custom=./packs/custom") {
		t.Errorf("output missing explicit import: %s", output)
	}
	if strings.Contains(output, "(default)") {
		t.Errorf("output should not show (default) for explicit import: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 0 {
		t.Errorf("rig includes should stay empty, got %v", cfg.Rigs[0].Includes)
	}
	if got := cfg.Rigs[0].Imports["custom"].Source; got != "./packs/custom" {
		t.Errorf("rig imports[custom] = %q, want ./packs/custom", got)
	}
}

func TestBoundImportsFromLegacySources(t *testing.T) {
	tests := []struct {
		name        string
		sources     []string
		wantSources []string
	}{
		{
			name:        "stable ordering",
			sources:     []string{"packs/zeta", "packs/alpha"},
			wantSources: []string{"alpha=./packs/alpha", "zeta=./packs/zeta"},
		},
		{
			name:        "deduplicates duplicate source",
			sources:     []string{" packs/alpha ", "packs/alpha", "packs/beta"},
			wantSources: []string{"alpha=./packs/alpha", "beta=./packs/beta"},
		},
		{
			name:        "uniquifies binding collision",
			sources:     []string{"packs/one/shared", "packs/two/shared"},
			wantSources: []string{"shared=./packs/one/shared", "shared-2=./packs/two/shared"},
		},
		{
			name:        "uses fallback binding for empty derived binding",
			sources:     []string{"/"},
			wantSources: []string{"import=/"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boundImportsFromLegacySources(tt.sources, nil)
			if got := renderBoundImportsInOrder(got); got != strings.Join(tt.wantSources, ", ") {
				t.Fatalf("boundImportsFromLegacySources() = %q, want %q", got, strings.Join(tt.wantSources, ", "))
			}
		})
	}
}

func TestMergeBoundImports(t *testing.T) {
	tests := []struct {
		name        string
		primary     []config.BoundImport
		secondary   []config.BoundImport
		wantSources []string
		wantErr     string
	}{
		{
			name: "stable ordering and identical deduplication",
			primary: []config.BoundImport{
				{Binding: "zeta", Import: config.Import{Source: "packs/zeta"}},
				{Binding: "alpha", Import: config.Import{Source: "packs/alpha"}},
			},
			secondary: []config.BoundImport{
				{Binding: "alpha", Import: config.Import{Source: "packs/alpha"}},
				{Binding: "beta", Import: config.Import{Source: "packs/beta"}},
			},
			wantSources: []string{"alpha=packs/alpha", "beta=packs/beta", "zeta=packs/zeta"},
		},
		{
			name: "rejects binding source disagreement for already-bound imports",
			primary: []config.BoundImport{
				{Binding: "shared", Import: config.Import{Source: "packs/one"}},
			},
			secondary: []config.BoundImport{
				{Binding: "shared", Import: config.Import{Source: "packs/two"}},
			},
			wantErr: "maps to both",
		},
		{
			name: "same source keeps typed import options",
			primary: []config.BoundImport{
				{Binding: "shared", Import: config.Import{Source: "packs/shared", Transitive: boolPtr(false)}},
			},
			secondary: []config.BoundImport{
				{Binding: "shared", Import: config.Import{Source: "packs/shared"}},
			},
			wantSources: []string{"shared=packs/shared"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mergeBoundImports(tt.primary, tt.secondary)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("mergeBoundImports() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("mergeBoundImports() error = %v", err)
			}
			if got := renderBoundImportsInOrder(got); got != strings.Join(tt.wantSources, ", ") {
				t.Fatalf("mergeBoundImports() = %q, want %q", got, strings.Join(tt.wantSources, ", "))
			}
			if tt.name == "same source keeps typed import options" && got[0].Import.Transitive == nil {
				t.Fatalf("mergeBoundImports() dropped typed import options: %#v", got[0].Import)
			}
		})
	}
}

func renderBoundImportsInOrder(imports []config.BoundImport) string {
	parts := make([]string, 0, len(imports))
	for _, bound := range imports {
		parts = append(parts, bound.Binding+"="+bound.Import.Source)
	}
	return strings.Join(parts, ", ")
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd should fail for prefix collision, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "collides") {
		t.Errorf("stderr should mention collision: %s", stderr.String())
	}
}

func TestDoRigAdd_HQPrefixCollisionDoesNotMutateRig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"gas-city\"\nprefix = \"tf\"\n\n[[agent]]\nname = \"mayor\"\n"
	cityTomlPath := filepath.Join(cityPath, "city.toml")
	if err := os.WriteFile(cityTomlPath, []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "token-flames")

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd should fail for HQ prefix collision, got code %d; stdout: %s", code, stdout.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, `rig "token-flames": prefix "tf" collides with HQ`) {
		t.Fatalf("stderr should mention HQ collision, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "Use --prefix to specify a different prefix.") {
		t.Fatalf("stderr should include --prefix hint, got: %s", errMsg)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout should be empty before mutation starts, got: %s", stdout.String())
	}
	if _, err := os.Stat(rigPath); !os.IsNotExist(err) {
		t.Fatalf("rig directory should not be created, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("rig .beads should not be created, stat err: %v", err)
	}
	data, err := os.ReadFile(cityTomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != cityToml {
		t.Fatalf("city.toml changed unexpectedly:\n%s", data)
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "mfoo", "", false, false, &stdout, &stderr)
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
	writeSchema2RigCity(t, cityPath, "my-city", "[workspace]\n", "")

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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "xx", "", false, false, &stdout, &stderr)
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
	writeSchema2RigCity(t, cityPath, "my-city", "[workspace]\n", "")

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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for conflicting derived prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "already has bead prefix") {
		t.Errorf("stderr should explain conflict: %s", stderr.String())
	}
}

// A fresh "gc rig add" against a pre-existing .beads/ store must fail fast
// and point the user at --adopt — even when the existing prefix would have
// matched the derived one. Falling through to bd init on a populated Dolt
// store produces confusing "signal: killed" failures (see fo-5zeij).
func TestDoRigAdd_ExistingBeadsRequiresAdopt(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "my-city", "[workspace]\n", "")

	// Rig "alpha-beta" derives prefix "ab", and .beads already has "ab"
	// — so the prefix-conflict guard does not trip and we reach the
	// "store already exists without --adopt" guard.
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for pre-existing .beads/ store without --adopt, got code %d; stdout: %s", code, stdout.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "already contains a beads store") {
		t.Errorf("stderr should identify existing store: %s", errMsg)
	}
	if !strings.Contains(errMsg, "--adopt") {
		t.Errorf("stderr should hint at --adopt: %s", errMsg)
	}
}

// A .beads/ directory containing only metadata.json (no config.yaml) is
// still recognized as an existing store — bd init creates both files,
// and either one is sufficient evidence that a real store is present.
func TestDoRigAdd_ExistingBeadsMetadataOnlyRequiresAdopt(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "alpha-beta")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"name":"alpha-beta","issue_prefix":"ab"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for pre-existing .beads/ store without --adopt, got code %d; stdout: %s", code, stdout.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "already contains a beads store") {
		t.Errorf("stderr should identify existing store: %s", errMsg)
	}
	if !strings.Contains(errMsg, "--adopt") {
		t.Errorf("stderr should hint at --adopt: %s", errMsg)
	}
}

// A target directory whose .beads/ subdir contains only unrelated content
// (no metadata.json or config.yaml) is NOT a beads store. Common in the
// wild: the beads project itself uses .beads/formulas/ for unrelated
// formula source files. gc rig add must proceed in this case, initializing
// the store alongside the existing content without disturbing it.
func TestDoRigAdd_BeadsDirWithUnrelatedContentSucceeds(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "beads-project")
	formulasDir := filepath.Join(rigPath, ".beads", "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	formulaPath := filepath.Join(formulasDir, "example.toml")
	if err := os.WriteFile(formulaPath, []byte("# unrelated formula source\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed when .beads/ has only unrelated content, got code %d; stderr: %s", code, stderr.String())
	}

	// Pre-existing unrelated content must be left untouched.
	if _, err := os.Stat(formulaPath); err != nil {
		t.Errorf(".beads/formulas/example.toml should be preserved: %v", err)
	}

	// city.toml must list the new rig.
	cityTomlBytes, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cityTomlBytes), "beads-project") {
		t.Errorf("city.toml should contain rig name: %s", cityTomlBytes)
	}
}

func TestDoRigAdd_ExistingBeadsStatErrorFailsClosed(t *testing.T) {
	f := fsys.NewFake()
	cityPath := "/city"
	rigPath := "/alpha-beta"
	beadsPath := filepath.Join(rigPath, ".beads")

	writeSchema2RigCityFS(t, f, cityPath, "my-city", "[workspace]\n")
	f.Dirs[rigPath] = true
	f.Errors[beadsPath] = os.ErrPermission

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(f, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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

func TestDoRigAdd_ExistingBeadsMarkerStatErrorFailsClosed(t *testing.T) {
	f := fsys.NewFake()
	cityPath := "/city"
	rigPath := "/alpha-beta"
	beadsPath := filepath.Join(rigPath, ".beads")
	markerPath := filepath.Join(beadsPath, "metadata.json")

	f.Dirs[filepath.Join(cityPath, ".gc")] = true
	f.Dirs[rigPath] = true
	f.Dirs[beadsPath] = true
	f.Files[filepath.Join(cityPath, "city.toml")] = []byte("[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n")
	f.Errors[markerPath] = os.ErrPermission

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(f, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for .beads marker stat error, got code %d; stdout: %s", code, stdout.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "checking "+markerPath) {
		t.Fatalf("stderr should identify the marker stat failure, got: %s", errMsg)
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

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\n\n[[rigs]]\nname = \"my-frontend\"\nprefix = \"mf\"\n"
	siteToml := fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"my-frontend\"\npath = %q\n", rigPath)
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, siteToml)

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with differing --prefix should warn.
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "xx", "", false, false, &stdout, &stderr)
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

	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "AB", "", false, false, &stdout, &stderr)
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
	code2 := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout2, &stderr2)
	if code2 != 0 {
		t.Errorf("re-add should succeed, got code %d, stderr: %s", code2, stderr2.String())
	}
}

func TestDoRigAdd_PrefixAllowsHyphens(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "my-app", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected success for hyphenated prefix, got code %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Prefix: my-app") {
		t.Errorf("expected prefix my-app in output: %s", stdout.String())
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
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
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ar", "", false, true, &stdout, &stderr)
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
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "no-beads-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when .beads/metadata.json missing, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "--adopt requires .beads/metadata.json") {
		t.Errorf("error should mention missing metadata.json: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptRequiresExistingDir(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "does-not-exist")

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for non-existent dir with --adopt, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "--adopt requires an existing directory") {
		t.Errorf("error should mention existing directory requirement: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptNonGitDirSucceeds(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ng", "", false, true, &stdout, &stderr)
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
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "nc", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when .beads/config.yaml missing, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "valid issue_prefix") {
		t.Errorf("error should mention missing prefix: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptRejectsEmptyConfigYaml(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ec", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when config.yaml lacks issue_prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "valid issue_prefix") {
		t.Errorf("error should mention missing prefix: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptWithoutPrefixMismatch(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected prefix mismatch failure, got code %d, stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "already has bead prefix") {
		t.Errorf("error should mention prefix mismatch: %s", stderr.String())
	}
}

// TestDoRigAdd_AdoptWithBdContractInvokesInitAndHook verifies that --adopt on
// a managed-Dolt (bd-contract) city invokes the init/hook machinery to
// register types.custom and other config into the adopted store's DB.
// Regression for ga-fg1ht3: the adopt branch previously skipped the init
// path entirely, leaving the DB config table out of sync with config.yaml.
func TestDoRigAdd_AdoptWithBdContractInvokesInitAndHook(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	t.Setenv("GC_BEADS", "exec:"+filepath.Join(cityPath, "gc-beads-bd"))
	t.Setenv("GC_DOLT", "")

	// Stub lifecycle hooks — no real Dolt in unit tests. Record that the
	// init path was invoked with the adopted rig dir.
	origEnsure := initDirIfReadyEnsureBeadsProvider
	origInit := initDirIfReadyInitAndHookDir
	origWait := initDirIfReadyWaitForManagedDolt
	t.Cleanup(func() {
		initDirIfReadyEnsureBeadsProvider = origEnsure
		initDirIfReadyInitAndHookDir = origInit
		initDirIfReadyWaitForManagedDolt = origWait
	})
	initDirIfReadyEnsureBeadsProvider = func(_ string) error { return nil }
	initDirIfReadyWaitForManagedDolt = func(_ string, _ time.Duration) error { return nil }

	var initCalls []string
	initDirIfReadyInitAndHookDir = func(_, dir, _ string) error {
		initCalls = append(initCalls, dir)
		return nil
	}

	rigPath := filepath.Join(t.TempDir(), "adopted-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"adopted-rig","issue_prefix":"ar"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	configYaml := "issue_prefix: ar\n" +
		"types.custom: molecule,convoy,session,merge-request,agent,role,rig,spec,convergence,step\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(configYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ar", "", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd --adopt returned %d, stderr: %s", code, stderr.String())
	}
	if len(initCalls) == 0 {
		t.Fatalf("ga-fg1ht3: --adopt on bd-contract city must invoke initAndHookDir to register types.custom; initCalls=%v", initCalls)
	}
	found := false
	for _, dir := range initCalls {
		if samePath(dir, rigPath) {
			found = true
		}
	}
	if !found {
		t.Errorf("initAndHookDir called but not for adopted rig path %q; calls=%v", rigPath, initCalls)
	}
	if !strings.Contains(stdout.String(), "Adopted existing beads database") {
		t.Errorf("stdout should mention adoption: %s", stdout.String())
	}
}

// TestDoRigAdd_AdoptWithBdContractProvider_NonAdoptControlInvokesInit is a
// control: the same stubbed harness wired through a NON-adopt add also calls
// initAndHookDir, proving that the empty-initCalls failure above (pre-fix) was
// real (adopt invoked init 0 times), not a harness artifact.
func TestDoRigAdd_AdoptWithBdContractProvider_NonAdoptControlInvokesInit(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	t.Setenv("GC_BEADS", "exec:"+filepath.Join(cityPath, "gc-beads-bd"))
	t.Setenv("GC_DOLT", "")

	origEnsure := initDirIfReadyEnsureBeadsProvider
	origInit := initDirIfReadyInitAndHookDir
	origWait := initDirIfReadyWaitForManagedDolt
	t.Cleanup(func() {
		initDirIfReadyEnsureBeadsProvider = origEnsure
		initDirIfReadyInitAndHookDir = origInit
		initDirIfReadyWaitForManagedDolt = origWait
	})
	initDirIfReadyEnsureBeadsProvider = func(_ string) error { return nil }
	initDirIfReadyWaitForManagedDolt = func(_ string, _ time.Duration) error { return nil }

	var initCalls []string
	initDirIfReadyInitAndHookDir = func(_, dir, _ string) error {
		initCalls = append(initCalls, dir)
		return nil
	}

	rigPath := filepath.Join(t.TempDir(), "fresh-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "fr", "", false, false, &stdout, &stderr)
	if len(initCalls) == 0 {
		t.Fatalf("control: non-adopt rig add invoked initAndHookDir 0 times; stub not wired in? stderr=%s", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Six-row read-path routing matrix for `gc rig list` (ADR 0001, ga-h6w).
// ---------------------------------------------------------------------------
//
// Each row exercises one branch of routeRigList:
//
//   api-happy-path       API returns 200 with items         route=api, exit 0
//   api-cache-not-live   API returns 503 cache_not_live     fallback, exit 0
//   api-500-fallback     API returns generic 500            fallback (conn-refused), exit 0
//   api-404-error        API returns 404                    no fallback, exit 1
//   controller-down      apiClient returns nil (no env)     fallback (controller-down), exit 0
//   escape-hatch         GC_NO_API truthy                   fallback (escape-hatch), exit 0
//
// Tests invoke routeRigList directly with an injected api.Client or nil +
// reason so no tmux / controller process is needed.

// rigListMatrixHandler returns the http.Handler to install for one matrix
// row, or nil when the row exercises the apiClient-nil branch.
type rigListMatrixHandler func(t *testing.T) http.Handler

// okRigsHandler serves a non-stale rig list matching the test city config.
func okRigsHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rigs") {
			http.NotFound(w, r)
			return
		}
		prefix := "fe"
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"name": "frontend", "path": "/abs/frontend", "prefix": prefix, "suspended": false, "agent_count": 0, "running_count": 0},
			},
			"total": 1,
		})
	})
}

func problemHandler(status int, detail string) rigListMatrixHandler {
	return func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": status,
				"title":  http.StatusText(status),
				"detail": detail,
			})
		})
	}
}

func TestRouteRigList_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      rigListMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		// Extra stderr assertions — e.g. "not found" for the 404 row.
		wantStderr string
		// When non-empty, assert stdout contains the substring.
		wantStdout string
	}{
		{
			name:       "api-happy-path",
			handler:    okRigsHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "frontend",
		},
		{
			name:       "api-cache-not-live",
			handler:    problemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    problemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    problemHandler(http.StatusNotFound, "not_found: city not configured"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1") // force route=... lines into stderr buffer

			cityPath := writeRigListTestCity(t)

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeRigList(cityPath, c, tc.nilReason, false, &stdout, &stderr)

			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			if tc.wantRoute != "" {
				want := "route=" + tc.wantRoute
				if tc.wantReason != "" {
					want += " reason=" + tc.wantReason
				}
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
				// Exactly one route=... line per exit path.
				if n := strings.Count(stderr.String(), "route="); n != 1 {
					t.Errorf("route=... lines = %d, want 1:\n%s", n, stderr.String())
				}
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
			// Fallback rows must produce the same rendered output as the
			// direct path (doRigList). Happy-path API rows render via
			// renderRigListFromAPI.
			if tc.wantRoute == "fallback" {
				if !strings.Contains(stdout.String(), "test-city (HQ)") {
					t.Errorf("fallback stdout missing HQ header:\n%s", stdout.String())
				}
			}
		})
	}
}

// writeRigListTestCity creates a minimal city directory with a city.toml
// that declares one rig so both the API-render path (renderRigListFromAPI)
// and the fallback path (doRigList) have something to format.
func writeRigListTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "coordinator"

[[rigs]]
name = "frontend"
path = "/abs/frontend"
prefix = "fe"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func TestRouteRigList_APIJSONIncludesCacheAge(t *testing.T) {
	// API-path --json output must carry _cache_age_s; fallback must not.
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeRigListTestCity(t)

	srv := httptest.NewServer(okRigsHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeRigList(cityPath, c, "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal stdout: %v\n%s", err, stdout.String())
	}
	if _, ok := out["_cache_age_s"]; !ok {
		t.Errorf("_cache_age_s missing from API --json:\n%s", stdout.String())
	}
	// Fallback path must omit the field.
	stdout.Reset()
	stderr.Reset()
	if code := routeRigList(cityPath, nil, "controller-down", true, &stdout, &stderr); code != 0 {
		t.Fatalf("fallback exit = %d, stderr=%q", code, stderr.String())
	}
	var fb map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &fb); err != nil {
		t.Fatalf("unmarshal fallback stdout: %v\n%s", err, stdout.String())
	}
	if _, ok := fb["_cache_age_s"]; ok {
		t.Errorf("_cache_age_s must be absent on fallback:\n%s", stdout.String())
	}
}

func TestRouteRigList_APIJSONPreservesFallbackContract(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "coordinator"

[[rigs]]
name = "frontend"
path = "/abs/frontend"
prefix = "fe"
default_branch = "trunk"
default_sling_target = "frontend/worker"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		prefix := "fe"
		w.Header().Set("X-GC-Cache-Age-S", "3")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"name": "frontend", "path": "/abs/frontend", "prefix": prefix, "suspended": false, "agent_count": 1, "running_count": 1},
			},
			"total": 1,
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeRigList(cityPath, c, "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal stdout: %v\n%s", err, stdout.String())
	}
	if got := out["schema_version"]; got != "1" {
		t.Fatalf("schema_version = %#v, want 1; output=%s", got, stdout.String())
	}
	if _, ok := out["summary"].(map[string]any); !ok {
		t.Fatalf("summary missing or wrong type: %#v; output=%s", out["summary"], stdout.String())
	}
	rigs, ok := out["rigs"].([]any)
	if !ok || len(rigs) != 2 {
		t.Fatalf("rigs = %#v, want HQ + frontend", out["rigs"])
	}
	hq, ok := rigs[0].(map[string]any)
	if !ok {
		t.Fatalf("HQ row = %#v", rigs[0])
	}
	if got := hq["running"]; got != true {
		t.Fatalf("HQ running = %#v, want true; row=%#v", got, hq)
	}
	frontend, ok := rigs[1].(map[string]any)
	if !ok {
		t.Fatalf("frontend row = %#v", rigs[1])
	}
	if got := frontend["default_branch"]; got != "trunk" {
		t.Fatalf("default_branch = %#v, want trunk; row=%#v", got, frontend)
	}
	if got := frontend["default_sling_target"]; got != "frontend/worker" {
		t.Fatalf("default_sling_target = %#v, want frontend/worker; row=%#v", got, frontend)
	}
	if got := frontend["running"]; got != true {
		t.Fatalf("running = %#v, want true; row=%#v", got, frontend)
	}
	if _, ok := out["_cache_age_s"]; !ok {
		t.Fatalf("_cache_age_s missing; output=%s", stdout.String())
	}
	validateJSONResultSchema(t, []string{"rig", "list"}, stdout.Bytes())
}

func TestRouteRigList_APIHumanPreservesFallbackContract(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	configRigPath := filepath.Join(t.TempDir(), "frontend-from-config")
	if err := os.MkdirAll(filepath.Join(configRigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRigPath, ".beads", "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	staleAPIPath := filepath.Join(t.TempDir(), "frontend-from-api")
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "coordinator"

[[rigs]]
name = "frontend"
path = %q
prefix = "cfg"
default_branch = "trunk"
`, configRigPath)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "3")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"name":           "frontend",
					"path":           staleAPIPath,
					"prefix":         "api",
					"default_branch": "api-main",
					"suspended":      false,
					"agent_count":    1,
					"running_count":  1,
				},
			},
			"total": 1,
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeRigList(cityPath, c, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"Path:   " + configRigPath,
		"Prefix: cfg",
		"Default branch: trunk",
		"Beads:  initialized",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("human API output missing %q:\n%s", want, output)
		}
	}
	for _, unwanted := range []string{
		staleAPIPath,
		"Prefix: api",
		"Default branch: api-main",
	} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("human API output used stale API row value %q:\n%s", unwanted, output)
		}
	}
}

func TestRouteRigList_StaleBannerOver30s(t *testing.T) {
	// Human output must append a staleness banner when the server reports
	// a cache age > 30 s.
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeRigListTestCity(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "45")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "total": 0})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeRigList(cityPath, c, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cache age: 45s") {
		t.Errorf("stale banner missing from human output:\n%s", stdout.String())
	}
}

// Regression test for the ga-lurp5d follow-up: a failed rig add must roll
// back a symlinked city.toml by restoring the link target, not by replacing
// the link with a regular file.
func TestRigAddRollbackRestoresThroughCityTomlSymlink(t *testing.T) {
	fs := fsys.OSFS{}
	cityDir, link, target := setupSymlinkedCityToml(t)

	snapshots, err := snapshotRigAddTopologyFiles(fs, cityDir, &config.City{})
	if err != nil {
		t.Fatalf("snapshotRigAddTopologyFiles: %v", err)
	}
	if err := os.WriteFile(target, []byte("[workspace]\nname = \"mutated\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}
	assertCityTomlSymlinkRestored(t, link, target, symlinkedCityTomlOriginal)
}
