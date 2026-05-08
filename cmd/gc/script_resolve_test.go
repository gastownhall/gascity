package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func writeLegacyScriptLink(t *testing.T, dir, relPath, target string) {
	t.Helper()
	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func writeLegacyScriptFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestPruneLegacyScripts_RemovesSymlinkOnlyTree(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	packScripts := filepath.Join(dir, "packs/base/assets/scripts")
	if err := os.MkdirAll(packScripts, 0o755); err != nil {
		t.Fatalf("MkdirAll pack scripts: %v", err)
	}
	srcFile := filepath.Join(packScripts, "helper.sh")
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}
	reviewSrc := filepath.Join(packScripts, "checks", "review.sh")
	if err := os.MkdirAll(filepath.Dir(reviewSrc), 0o755); err != nil {
		t.Fatalf("MkdirAll review src: %v", err)
	}
	if err := os.WriteFile(reviewSrc, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile review src: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/helper.sh", srcFile)
	writeLegacyScriptLink(t, cityPath, "scripts/checks/review.sh", reviewSrc)

	if err := pruneLegacyScripts(cityPath, []string{packScripts}); err != nil {
		t.Fatalf("pruneLegacyScripts: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityPath, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("scripts/ should be removed after pruning, err=%v", err)
	}
}

func TestPruneLegacyScripts_LeavesRealFilesAlone(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	writeLegacyScriptFile(t, cityPath, "scripts/run.sh", "#!/bin/sh\necho run\n")

	if err := pruneLegacyScripts(cityPath, nil); err != nil {
		t.Fatalf("pruneLegacyScripts: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityPath, "scripts", "run.sh")); err != nil {
		t.Fatalf("real scripts/run.sh should remain, err=%v", err)
	}
}

func TestPruneLegacyScripts_LeavesMixedTreeUntouched(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	packScripts := filepath.Join(dir, "packs/base/assets/scripts")
	if err := os.MkdirAll(packScripts, 0o755); err != nil {
		t.Fatalf("MkdirAll pack scripts: %v", err)
	}
	srcFile := filepath.Join(packScripts, "helper.sh")
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}

	writeLegacyScriptFile(t, cityPath, "scripts/run.sh", "#!/bin/sh\necho run\n")
	writeLegacyScriptLink(t, cityPath, "scripts/helper.sh", srcFile)

	if err := pruneLegacyScripts(cityPath, []string{packScripts}); err != nil {
		t.Fatalf("pruneLegacyScripts: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityPath, "scripts", "run.sh")); err != nil {
		t.Fatalf("real scripts/run.sh should remain, err=%v", err)
	}
	fi, err := os.Lstat(filepath.Join(cityPath, "scripts", "helper.sh"))
	if err != nil {
		t.Fatalf("symlink helper.sh should remain in mixed tree, err=%v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("helper.sh should remain a symlink in mixed tree, mode=%v", fi.Mode())
	}
}

func TestPruneLegacyScripts_LeavesForeignSymlinkOnlyTreeUntouched(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	foreignDir := filepath.Join(dir, "foreign")
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatalf("MkdirAll foreign: %v", err)
	}
	foreignFile := filepath.Join(foreignDir, "helper.sh")
	if err := os.WriteFile(foreignFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile foreign: %v", err)
	}
	legacyOrigin := filepath.Join(dir, "packs/base/assets/scripts")

	writeLegacyScriptLink(t, cityPath, "scripts/helper.sh", foreignFile)

	if err := pruneLegacyScripts(cityPath, []string{legacyOrigin}); err != nil {
		t.Fatalf("pruneLegacyScripts: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(cityPath, "scripts", "helper.sh")); err != nil {
		t.Fatalf("foreign symlink should remain, err=%v", err)
	}
}

func TestPruneLegacyScripts_LeavesUserManagedRelayoutUntouched(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	packScripts := filepath.Join(dir, "packs/base/assets/scripts")
	if err := os.MkdirAll(packScripts, 0o755); err != nil {
		t.Fatalf("MkdirAll pack scripts: %v", err)
	}
	srcFile := filepath.Join(packScripts, "helper.sh")
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/custom.sh", srcFile)

	if err := pruneLegacyScripts(cityPath, []string{packScripts}); err != nil {
		t.Fatalf("pruneLegacyScripts: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, "scripts", "custom.sh")); err != nil {
		t.Fatalf("user-managed relayout symlink should remain, err=%v", err)
	}
}

func TestPruneLegacyScripts_LeavesSubsetOfLegacyOriginUntouched(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	packScripts := filepath.Join(dir, "packs/base/assets/scripts")
	if err := os.MkdirAll(packScripts, 0o755); err != nil {
		t.Fatalf("MkdirAll pack scripts: %v", err)
	}
	helperSrc := filepath.Join(packScripts, "helper.sh")
	if err := os.WriteFile(helperSrc, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile helper src: %v", err)
	}
	extraSrc := filepath.Join(packScripts, "extra.sh")
	if err := os.WriteFile(extraSrc, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile extra src: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/helper.sh", helperSrc)

	if err := pruneLegacyScripts(cityPath, []string{packScripts}); err != nil {
		t.Fatalf("pruneLegacyScripts: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, "scripts", "helper.sh")); err != nil {
		t.Fatalf("subset of legacy origin should remain user-owned, err=%v", err)
	}
}

func TestPruneLegacyScripts_RemovesStaleLegacyShapeWhenOriginsMissing(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	writeLegacyScriptLink(t, cityPath, "scripts/helper.sh", filepath.Join(cityPath, "packs", "removed", "assets", "scripts", "helper.sh"))

	if err := pruneLegacyScripts(cityPath, nil); err != nil {
		t.Fatalf("pruneLegacyScripts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("stale legacy-shaped scripts/ should be removed, err=%v", err)
	}
}

func TestPruneLegacyConfiguredScripts_PrunesCityAndRigOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	cityPath := filepath.Join(dir, "city")
	rigPath := filepath.Join(cityPath, "rig")
	cityPackScripts := filepath.Join(dir, "packs/city/assets/scripts")
	rigPackScripts := filepath.Join(dir, "packs/rig/assets/scripts")
	if err := os.MkdirAll(cityPackScripts, 0o755); err != nil {
		t.Fatalf("MkdirAll city pack scripts: %v", err)
	}
	if err := os.MkdirAll(rigPackScripts, 0o755); err != nil {
		t.Fatalf("MkdirAll rig pack scripts: %v", err)
	}
	citySrcFile := filepath.Join(cityPackScripts, "city.sh")
	if err := os.WriteFile(citySrcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile city src: %v", err)
	}
	rigSrcFile := filepath.Join(rigPackScripts, "rig.sh")
	if err := os.WriteFile(rigSrcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile rig src: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/city.sh", citySrcFile)
	writeLegacyScriptLink(t, rigPath, "scripts/rig.sh", rigSrcFile)
	writeLegacyScriptLink(t, dir, "scripts/cwd.sh", citySrcFile)

	cfg := &config.City{
		PackDirs: []string{filepath.Join(dir, "packs/city")},
		Rigs: []config.Rig{
			{Name: "app", Path: "rig"},
			{Name: "unbound"},
		},
		RigPackDirs: map[string][]string{
			"app": {filepath.Join(dir, "packs/rig")},
		},
	}

	var warnings []string
	pruneLegacyConfiguredScripts(cityPath, cfg, func(scope string, err error) {
		warnings = append(warnings, scope+": "+err.Error())
	})
	if len(warnings) > 0 {
		t.Fatalf("pruneLegacyConfiguredScripts warnings: %v", warnings)
	}

	if _, err := os.Stat(filepath.Join(cityPath, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("city scripts/ should be pruned, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("rig scripts/ should be pruned, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "scripts", "cwd.sh")); err != nil {
		t.Fatalf("blank rig path should not prune cwd scripts, err=%v", err)
	}
}

func TestPruneLegacyConfiguredScripts_FallsBackToScopeAssetsWhenPackDirsMissing(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	rigPath := filepath.Join(cityPath, "rig")

	cityAsset := filepath.Join(cityPath, "assets", "scripts", "city.sh")
	if err := os.MkdirAll(filepath.Dir(cityAsset), 0o755); err != nil {
		t.Fatalf("MkdirAll city assets: %v", err)
	}
	if err := os.WriteFile(cityAsset, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile city asset: %v", err)
	}
	rigAsset := filepath.Join(rigPath, "assets", "scripts", "rig.sh")
	if err := os.MkdirAll(filepath.Dir(rigAsset), 0o755); err != nil {
		t.Fatalf("MkdirAll rig assets: %v", err)
	}
	if err := os.WriteFile(rigAsset, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile rig asset: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/city.sh", cityAsset)
	writeLegacyScriptLink(t, rigPath, "scripts/city.sh", cityAsset)
	writeLegacyScriptLink(t, rigPath, "scripts/rig.sh", rigAsset)

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "app", Path: rigPath}},
	}

	var warnings []string
	pruneLegacyConfiguredScripts(cityPath, cfg, func(scope string, err error) {
		warnings = append(warnings, scope+": "+err.Error())
	})
	if len(warnings) > 0 {
		t.Fatalf("pruneLegacyConfiguredScripts warnings: %v", warnings)
	}
	if _, err := os.Stat(filepath.Join(cityPath, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("city scripts/ should be pruned via local assets fallback, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("rig scripts/ should be pruned via local assets fallback, err=%v", err)
	}
}

func TestPruneLegacyConfiguredScripts_FallbackPreservesTopLevelScriptsTargets(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	writeLegacyScriptLink(t, cityPath, "scripts/helper.sh", filepath.Join(cityPath, "scripts", "generated", "helper.sh"))

	var warnings []string
	pruneLegacyConfiguredScripts(cityPath, &config.City{}, func(scope string, err error) {
		warnings = append(warnings, scope+": "+err.Error())
	})
	if len(warnings) > 0 {
		t.Fatalf("pruneLegacyConfiguredScripts warnings: %v", warnings)
	}
	if _, err := os.Lstat(filepath.Join(cityPath, "scripts", "helper.sh")); err != nil {
		t.Fatalf("user-managed top-level scripts symlink should remain, err=%v", err)
	}
}

func TestPrepareCityForSupervisorPrunesLegacyScripts(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	cleanupManagedDoltTestCity(t, cityPath)
	rigPath := filepath.Join(dir, "rig")
	cityPackScripts := filepath.Join(dir, "packs/city/assets/scripts")
	rigPackScripts := filepath.Join(dir, "packs/rig/assets/scripts")
	if err := os.MkdirAll(cityPackScripts, 0o755); err != nil {
		t.Fatalf("MkdirAll city pack scripts: %v", err)
	}
	if err := os.MkdirAll(rigPackScripts, 0o755); err != nil {
		t.Fatalf("MkdirAll rig pack scripts: %v", err)
	}
	citySrcFile := filepath.Join(cityPackScripts, "city.sh")
	if err := os.WriteFile(citySrcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile city src: %v", err)
	}
	rigSrcFile := filepath.Join(rigPackScripts, "rig.sh")
	if err := os.WriteFile(rigSrcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile rig src: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/city.sh", citySrcFile)
	writeLegacyScriptLink(t, rigPath, "scripts/city.sh", citySrcFile)
	writeLegacyScriptLink(t, rigPath, "scripts/rig.sh", rigSrcFile)

	logFile := filepath.Join(t.TempDir(), "beads.log")
	t.Setenv("GC_BEADS", "exec:"+writeSpyScript(t, logFile))

	cfg := config.DefaultCity("bright-lights")
	cfg.Rigs = []config.Rig{{Name: "app", Path: rigPath}}
	cfg.PackDirs = []string{filepath.Join(dir, "packs/city")}
	cfg.RigPackDirs = map[string][]string{
		"app": {filepath.Join(dir, "packs/rig")},
	}

	if err := prepareCityForSupervisor(cityPath, "bright-lights", &cfg, io.Discard, nil); err != nil {
		t.Fatalf("prepareCityForSupervisor: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityPath, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("city scripts/ should be pruned by supervisor start path, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, "scripts")); !os.IsNotExist(err) {
		t.Fatalf("rig scripts/ should be pruned by supervisor start path, err=%v", err)
	}
}
