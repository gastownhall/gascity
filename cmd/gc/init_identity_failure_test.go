package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

type recordingFS struct {
	fsys.OSFS
	Calls            []fsys.Call
	failRenameTarget string
	failedRename     bool
}

func (f *recordingFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	f.Calls = append(f.Calls, fsys.Call{Method: "WriteFile", Path: name})
	return f.OSFS.WriteFile(name, data, perm)
}

func (f *recordingFS) Rename(oldpath, newpath string) error {
	f.Calls = append(f.Calls, fsys.Call{Method: "Rename", Path: oldpath})
	if !f.failedRename && f.failRenameTarget != "" && filepath.Clean(newpath) == filepath.Clean(f.failRenameTarget) {
		f.failedRename = true
		return errors.New("injected site binding failure")
	}
	return f.OSFS.Rename(oldpath, newpath)
}

type failSiteBindingRenameFS struct {
	fsys.OSFS
	target string
	failed bool
}

func (f *failSiteBindingRenameFS) Rename(oldpath, newpath string) error {
	if !f.failed && filepath.Clean(newpath) == filepath.Clean(f.target) {
		f.failed = true
		return errors.New("injected site binding failure")
	}
	return f.OSFS.Rename(oldpath, newpath)
}

func TestDoInitRestoresLegacyIdentityWhenSiteBindingWriteFails(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "target-city")
	fs := &failSiteBindingRenameFS{target: filepath.Join(cityPath, ".gc", "site.toml")}

	var stdout, stderr bytes.Buffer
	code := doInit(fs, cityPath, defaultWizardConfig(), "machine-alias", &stdout, &stderr, false)
	if code == 0 {
		t.Fatalf("doInit = %d, want failure", code)
	}
	if !strings.Contains(stderr.String(), "injected site binding failure") {
		t.Fatalf("stderr = %q, want injected site binding failure", stderr.String())
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load config after rollback: %v", err)
	}
	if got := config.EffectiveCityName(cfg, filepath.Base(cityPath)); got != "machine-alias" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "machine-alias")
	}
	if got := config.EffectiveHQPrefix(cfg); got != "ma" {
		t.Fatalf("EffectiveHQPrefix() = %q, want %q", got, "ma")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "site.toml")); !os.IsNotExist(err) {
		t.Fatalf("site binding stat err = %v, want not exist", err)
	}
}

func TestDoInitFromFileRestoresLegacyIdentityWhenSiteBindingWriteFails(t *testing.T) {
	srcDir := t.TempDir()
	srcCfg := config.DefaultCity("declared-city")
	srcCfg.Workspace.Prefix = "dc"
	srcData, err := srcCfg.Marshal()
	if err != nil {
		t.Fatalf("marshal source config: %v", err)
	}
	srcToml := filepath.Join(srcDir, "source.toml")
	if err := os.WriteFile(srcToml, srcData, 0o644); err != nil {
		t.Fatalf("write source.toml: %v", err)
	}

	cityPath := filepath.Join(t.TempDir(), "target-city")
	fs := &failSiteBindingRenameFS{target: filepath.Join(cityPath, ".gc", "site.toml")}

	var stdout, stderr bytes.Buffer
	code := cmdInitFromTOMLFileWithOptions(fs, srcToml, cityPath, "machine-alias", &stdout, &stderr, true, false)
	if code == 0 {
		t.Fatalf("cmdInitFromTOMLFileWithOptions = %d, want failure", code)
	}
	if !strings.Contains(stderr.String(), "injected site binding failure") {
		t.Fatalf("stderr = %q, want injected site binding failure", stderr.String())
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load config after rollback: %v", err)
	}
	if got := config.EffectiveCityName(cfg, filepath.Base(cityPath)); got != "machine-alias" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "machine-alias")
	}
	if got := config.EffectiveHQPrefix(cfg); got != "dc" {
		t.Fatalf("EffectiveHQPrefix() = %q, want %q", got, "dc")
	}
}

func TestDoInitFromDirRestoresLegacyIdentityWhenSiteBindingWriteFails(t *testing.T) {
	srcDir := t.TempDir()
	srcData := []byte("[workspace]\nname = \"declared-city\"\nprefix = \"dc\"\n")
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"), srcData, 0o644); err != nil {
		t.Fatalf("write source city.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "pack.toml"), []byte("[pack]\nname = \"declared-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write source pack.toml: %v", err)
	}

	cityPath := filepath.Join(t.TempDir(), "target-city")
	fs := &failSiteBindingRenameFS{target: filepath.Join(cityPath, ".gc", "site.toml")}

	var stdout, stderr bytes.Buffer
	code := doInitFromDirWithOptionsFS(fs, srcDir, cityPath, "machine-alias", &stdout, &stderr, true)
	if code == 0 {
		t.Fatalf("doInitFromDirWithOptionsFS = %d, want failure", code)
	}
	if !strings.Contains(stderr.String(), "injected site binding failure") {
		t.Fatalf("stderr = %q, want injected site binding failure", stderr.String())
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load config after rollback: %v", err)
	}
	if got := config.EffectiveCityName(cfg, filepath.Base(cityPath)); got != "machine-alias" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "machine-alias")
	}
	if got := config.EffectiveHQPrefix(cfg); got != "dc" {
		t.Fatalf("EffectiveHQPrefix() = %q, want %q", got, "dc")
	}
}

func TestDoInitFromDirMigratesRigPathsToSiteBinding(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "frontend"
path = "/srv/frontend"
`)
	fs.Files["/city/pack.toml"] = []byte("[pack]\nname = \"declared-city\"\nschema = 2\n")

	cfg, _, _, persistSiteIdentity, err := rewriteCopiedInitFromIdentity(fs, "/city", "")
	if err != nil {
		t.Fatalf("rewriteCopiedInitFromIdentity: %v", err)
	}
	if !persistSiteIdentity {
		t.Fatal("persistSiteIdentity = false, want true for copied pack city")
	}

	cityData := fs.Files["/city/city.toml"]
	if strings.Contains(string(cityData), "path =") {
		t.Fatalf("target city.toml still contains rig path:\n%s", cityData)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].Path != "" {
		t.Fatalf("rewritten cfg rigs = %#v, want path stripped", cfg.Rigs)
	}

	siteData := fs.Files["/city/.gc/site.toml"]
	if !strings.Contains(string(siteData), `name = "frontend"`) || !strings.Contains(string(siteData), `path = "/srv/frontend"`) {
		t.Fatalf("site.toml missing rig binding:\n%s", siteData)
	}

	loaded, _, err := config.LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes target city: %v", err)
	}
	if len(loaded.Rigs) != 1 || loaded.Rigs[0].Path != "/srv/frontend" {
		t.Fatalf("loaded rigs = %#v, want frontend path from site binding", loaded.Rigs)
	}
}

func TestDoInitFromFileWritesCityBeforeRigSiteBindings(t *testing.T) {
	srcDir := t.TempDir()
	rigPath := filepath.Join(srcDir, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	srcToml := filepath.Join(srcDir, "source.toml")
	if err := os.WriteFile(srcToml, []byte(fmt.Sprintf(`[workspace]
name = "declared-city"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "frontend"
path = %q
`, rigPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(t.TempDir(), "city")
	fs := &recordingFS{failRenameTarget: filepath.Join(cityPath, ".gc", "site.toml")}
	var stdout, stderr bytes.Buffer
	code := cmdInitFromTOMLFileWithOptions(fs, srcToml, cityPath, "", &stdout, &stderr, true, false)
	if code == 0 {
		t.Fatal("cmdInitFromTOMLFileWithOptions = 0, want injected site binding failure")
	}
	if !strings.Contains(stderr.String(), "injected site binding failure") {
		t.Fatalf("stderr = %q, want injected site binding failure", stderr.String())
	}

	cityWrite := firstCallIndex(fs.Calls, isCityConfigWriteCallFor(cityPath))
	siteWrite := firstCallIndex(fs.Calls, isSiteBindingWriteCallFor(cityPath))
	if cityWrite < 0 || siteWrite < 0 {
		t.Fatalf("calls missing city or site write: %+v", fs.Calls)
	}
	if cityWrite > siteWrite {
		t.Fatalf("write order = %+v, want city.toml before .gc/site.toml", fs.Calls)
	}
}

func TestDoInitFromFileRestoresCityWhenRigSiteBindingWriteFails(t *testing.T) {
	srcDir := t.TempDir()
	rigPath := filepath.Join(srcDir, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	srcToml := filepath.Join(srcDir, "source.toml")
	if err := os.WriteFile(srcToml, []byte(fmt.Sprintf(`[workspace]
name = "declared-city"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "frontend"
path = %q
`, rigPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(t.TempDir(), "city")
	fs := &recordingFS{failRenameTarget: filepath.Join(cityPath, ".gc", "site.toml")}
	var stdout, stderr bytes.Buffer
	code := cmdInitFromTOMLFileWithOptions(fs, srcToml, cityPath, "", &stdout, &stderr, true, false)
	if code == 0 {
		t.Fatal("cmdInitFromTOMLFileWithOptions = 0, want injected site binding failure")
	}
	for _, want := range []string{"injected site binding failure", "restored"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	cityToml := filepath.Join(cityPath, "city.toml")
	cityBytes, err := os.ReadFile(cityToml)
	if err == nil && !strings.Contains(string(cityBytes), fmt.Sprintf("path = %q", rigPath)) {
		t.Fatalf("city.toml = %q, want restored rig path or no city.toml", cityBytes)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read city.toml after rollback: %v", err)
	}
}

func TestDoInitFromFileMigratesRigPathsToSiteBinding(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	srcDir := t.TempDir()
	srcToml := filepath.Join(t.TempDir(), "source.toml")
	rigPath := filepath.Join(srcDir, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcToml, []byte(fmt.Sprintf(`[workspace]
name = "declared-city"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "frontend"
path = %q
`, rigPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(t.TempDir(), "city")
	var stdout, stderr bytes.Buffer
	code := cmdInitFromTOMLFileWithOptions(fsys.OSFS{}, srcToml, cityPath, "", &stdout, &stderr, true, false)
	if code != 0 {
		t.Fatalf("cmdInitFromTOMLFileWithOptions = %d, want success; stderr=%s", code, stderr.String())
	}

	cityData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if strings.Contains(string(cityData), "path =") {
		t.Fatalf("target city.toml still contains rig path:\n%s", cityData)
	}
	siteData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "site.toml"))
	if err != nil {
		t.Fatalf("read site.toml: %v", err)
	}
	if !strings.Contains(string(siteData), `name = "frontend"`) || !strings.Contains(string(siteData), fmt.Sprintf("path = %q", rigPath)) {
		t.Fatalf("site.toml missing rig binding:\n%s", siteData)
	}
	loaded, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes target city: %v", err)
	}
	if len(loaded.Rigs) != 1 || loaded.Rigs[0].Path != rigPath {
		t.Fatalf("loaded rigs = %#v, want frontend path from site binding", loaded.Rigs)
	}
}

func TestDoInitFromDirWritesCityBeforeRigSiteBindings(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "frontend"
path = "/srv/frontend"
`)
	fs.Files["/city/pack.toml"] = []byte("[pack]\nname = \"declared-city\"\nschema = 2\n")

	if _, _, _, _, err := rewriteCopiedInitFromIdentity(fs, "/city", ""); err != nil {
		t.Fatalf("rewriteCopiedInitFromIdentity: %v", err)
	}

	cityWrite := firstCallIndex(fs.Calls, isCityConfigWriteCallFor("/city"))
	siteWrite := firstCallIndex(fs.Calls, isSiteBindingWriteCallFor("/city"))
	if cityWrite < 0 || siteWrite < 0 {
		t.Fatalf("calls missing city or site write: %+v", fs.Calls)
	}
	if cityWrite > siteWrite {
		t.Fatalf("write order = %+v, want city.toml before .gc/site.toml", fs.Calls)
	}
}

func TestRewriteCopiedInitFromIdentityRestoresRigPathsWhenSiteBindingFails(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	fs := &recordingFS{failRenameTarget: filepath.Join(cityPath, ".gc", "site.toml")}
	if err := fs.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "frontend"
path = "/srv/frontend"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"declared-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := rewriteCopiedInitFromIdentity(fs, cityPath, "")
	if err == nil {
		t.Fatal("rewriteCopiedInitFromIdentity succeeded, want injected site binding failure")
	}
	for _, want := range []string{"injected site binding failure", "restored"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	cityBytes, readErr := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if readErr != nil {
		t.Fatalf("read city.toml: %v", readErr)
	}
	cityData := string(cityBytes)
	if !strings.Contains(cityData, `path = "/srv/frontend"`) {
		t.Fatalf("city.toml = %q, want restored legacy rig path", cityData)
	}
}

func firstCallIndex(calls []fsys.Call, match func(fsys.Call) bool) int {
	for i, call := range calls {
		if match(call) {
			return i
		}
	}
	return -1
}

func isSiteBindingWriteCallFor(cityPath string) func(fsys.Call) bool {
	siteTempPrefix := filepath.Join(cityPath, ".gc", "site.toml.")
	return func(call fsys.Call) bool {
		if call.Method != "WriteFile" {
			return false
		}
		return strings.HasPrefix(call.Path, siteTempPrefix)
	}
}

func isCityConfigWriteCallFor(cityPath string) func(fsys.Call) bool {
	cityTempPrefix := filepath.Join(cityPath, "city.toml.")
	cityPath = filepath.Join(cityPath, "city.toml")
	return func(call fsys.Call) bool {
		if call.Method != "WriteFile" {
			return false
		}
		return call.Path == cityPath || strings.HasPrefix(call.Path, cityTempPrefix)
	}
}
