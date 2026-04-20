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
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll src: %v", err)
	}
	srcFile := filepath.Join(srcDir, "helper.sh")
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/helper.sh", srcFile)
	writeLegacyScriptLink(t, cityPath, "scripts/checks/review.sh", srcFile)

	if err := pruneLegacyScripts(cityPath); err != nil {
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

	if err := pruneLegacyScripts(cityPath); err != nil {
		t.Fatalf("pruneLegacyScripts: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityPath, "scripts", "run.sh")); err != nil {
		t.Fatalf("real scripts/run.sh should remain, err=%v", err)
	}
}

func TestPruneLegacyScripts_LeavesMixedTreeUntouched(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll src: %v", err)
	}
	srcFile := filepath.Join(srcDir, "helper.sh")
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}

	writeLegacyScriptFile(t, cityPath, "scripts/run.sh", "#!/bin/sh\necho run\n")
	writeLegacyScriptLink(t, cityPath, "scripts/helper.sh", srcFile)

	if err := pruneLegacyScripts(cityPath); err != nil {
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

func TestPruneLegacyConfiguredScripts_PrunesCityAndRigOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	cityPath := filepath.Join(dir, "city")
	rigPath := filepath.Join(cityPath, "rig")
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll src: %v", err)
	}
	srcFile := filepath.Join(srcDir, "helper.sh")
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/city.sh", srcFile)
	writeLegacyScriptLink(t, rigPath, "scripts/rig.sh", srcFile)
	writeLegacyScriptLink(t, dir, "scripts/cwd.sh", srcFile)

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "app", Path: "rig"},
			{Name: "unbound"},
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

func TestPrepareCityForSupervisorPrunesLegacyScripts(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city")
	rigPath := filepath.Join(dir, "rig")
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll src: %v", err)
	}
	srcFile := filepath.Join(srcDir, "helper.sh")
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}

	writeLegacyScriptLink(t, cityPath, "scripts/city.sh", srcFile)
	writeLegacyScriptLink(t, rigPath, "scripts/rig.sh", srcFile)

	logFile := filepath.Join(t.TempDir(), "beads.log")
	t.Setenv("GC_BEADS", "exec:"+writeSpyScript(t, logFile))

	cfg := config.DefaultCity("bright-lights")
	cfg.Rigs = []config.Rig{{Name: "app", Path: rigPath}}

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
