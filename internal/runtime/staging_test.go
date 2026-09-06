package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageDirPreservesBestEffortOverlayWarnings(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	dstDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "ok.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatalf("write ok overlay file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "blocked", "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write blocked overlay file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocked dst file: %v", err)
	}

	if err := StageDir(srcDir, dstDir); err != nil {
		t.Fatalf("StageDir() error = %v, want nil", err)
	}

	data, err := os.ReadFile(filepath.Join(dstDir, "ok.txt"))
	if err != nil {
		t.Fatalf("read copied overlay file: %v", err)
	}
	if string(data) != "copied" {
		t.Fatalf("copied overlay file = %q, want %q", string(data), "copied")
	}
}

func TestStageWorkDirSkipsCopyWhenSourceAlreadyMatchesResolvedDestination(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	src := filepath.Join(workDir, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	if err := StageWorkDir(workDir, "", []CopyEntry{{Src: src}}); err != nil {
		t.Fatalf("StageWorkDir() error = %v, want nil", err)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read staged source file: %v", err)
	}
	if string(data) != "seed" {
		t.Fatalf("staged source file = %q, want %q", string(data), "seed")
	}
}

func TestValidateLocalWorkDirRejectsUnsafePath(t *testing.T) {
	root := t.TempDir()
	notDirectory := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("file"), 0o644); err != nil {
		t.Fatalf("write non-directory fixture: %v", err)
	}

	tests := []struct {
		name    string
		workDir string
		want    string
	}{
		{name: "relative", workDir: filepath.Join("relative", "workdir"), want: "absolute"},
		{name: "missing", workDir: filepath.Join(root, "missing"), want: "does not exist"},
		{name: "not directory", workDir: notDirectory, want: "not a directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLocalWorkDir(tt.workDir)
			if err == nil {
				t.Fatal("ValidateLocalWorkDir() succeeded, want validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateLocalWorkDir() error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestStageSessionWorkDirAllowsDirectoryPreparedByPreStart(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "not-created-yet")
	if err := StageSessionWorkDir(Config{WorkDir: workDir, PreStart: []string{"prepare-workdir"}}); !errors.Is(err, ErrWorkDirPendingPreStart) {
		t.Fatalf("StageSessionWorkDir() error = %v, want ErrWorkDirPendingPreStart", err)
	}
}

func TestStageSessionWorkDirDefersFilesUntilPreStartCreatesDirectory(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "not-created-yet")
	src := filepath.Join(root, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:   workDir,
		PreStart:  []string{"prepare-workdir"},
		CopyFiles: []CopyEntry{{Src: src, RelDst: "seed.txt"}},
	})
	if !errors.Is(err, ErrWorkDirPendingPreStart) {
		t.Fatalf("StageSessionWorkDir() error = %v, want ErrWorkDirPendingPreStart", err)
	}
	if _, statErr := os.Stat(workDir); !os.IsNotExist(statErr) {
		t.Fatalf("workdir stat error = %v, want not exist", statErr)
	}
}

func TestStageSessionWorkDirRejectsEscapingCopyDestination(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	src := filepath.Join(root, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	tests := []struct {
		name   string
		relDst string
		path   string
	}{
		{name: "parent", relDst: filepath.Join("..", "outside-parent.txt"), path: filepath.Join(root, "outside-parent.txt")},
		{name: "absolute", relDst: filepath.Join(root, "outside-absolute.txt"), path: filepath.Join(root, "outside-absolute.txt")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := StageSessionWorkDir(Config{
				WorkDir:   workDir,
				CopyFiles: []CopyEntry{{Src: src, RelDst: tt.relDst}},
			})
			if err == nil {
				t.Fatal("StageSessionWorkDir() succeeded, want copy destination error")
			}
			if _, statErr := os.Stat(tt.path); !os.IsNotExist(statErr) {
				t.Fatalf("outside destination stat error = %v, want not exist", statErr)
			}
		})
	}
}

func TestStageSessionWorkDirPreflightsAllCopiesBeforeMutation(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	overlayDir := filepath.Join(root, "overlay")
	for _, dir := range []string{workDir, overlayDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "overlay.txt"), []byte("overlay"), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	validSrc := filepath.Join(root, "valid.txt")
	invalidSrc := filepath.Join(root, "invalid.txt")
	for _, path := range []string{validSrc, invalidSrc} {
		if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
			t.Fatalf("write source %q: %v", path, err)
		}
	}

	err := StageSessionWorkDir(Config{
		WorkDir:    workDir,
		OverlayDir: overlayDir,
		CopyFiles: []CopyEntry{
			{Src: validSrc, RelDst: "valid.txt"},
			{Src: invalidSrc, RelDst: filepath.Join("..", "outside.txt")},
		},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want malformed copy destination error")
	}
	for _, path := range []string{
		filepath.Join(workDir, "overlay.txt"),
		filepath.Join(workDir, "valid.txt"),
		filepath.Join(root, "outside.txt"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("destination %q stat error = %v, want not exist", path, statErr)
		}
	}
}

func TestStageSessionWorkDirRejectsSymlinkCopyDestinationEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	for _, dir := range []string{workDir, outsideDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	link := filepath.Join(workDir, "linked")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	src := filepath.Join(root, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:   workDir,
		CopyFiles: []CopyEntry{{Src: src, RelDst: filepath.Join("linked", "copied.txt")}},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want symlink escape error")
	}
	if _, statErr := os.Stat(filepath.Join(outsideDir, "copied.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("outside destination stat error = %v, want not exist", statErr)
	}
}

func TestStageSessionWorkDirRejectsNestedSymlinkDestinationEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	srcDir := filepath.Join(root, "source")
	for _, dir := range []string{
		filepath.Join(workDir, "dest"),
		outsideDir,
		filepath.Join(srcDir, "nested"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "dest", "nested")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "nested", "pwn"), []byte("escaped"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:   workDir,
		CopyFiles: []CopyEntry{{Src: srcDir, RelDst: "dest"}},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want nested symlink escape error")
	}
	if _, statErr := os.Stat(filepath.Join(outsideDir, "pwn")); !os.IsNotExist(statErr) {
		t.Fatalf("outside destination stat error = %v, want not exist", statErr)
	}
}

func TestStageSessionWorkDirRejectsNestedSymlinkDestinationForSymlinkedSourceRoot(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	realSrcDir := filepath.Join(root, "real-source")
	linkedSrcDir := filepath.Join(root, "linked-source")
	for _, dir := range []string{
		filepath.Join(workDir, "dest"),
		outsideDir,
		filepath.Join(realSrcDir, "nested"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.Symlink(realSrcDir, linkedSrcDir); err != nil {
		t.Skipf("source symlink fixture unavailable: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "dest", "nested")); err != nil {
		t.Skipf("destination symlink fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realSrcDir, "nested", "pwn"), []byte("escaped"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:   workDir,
		CopyFiles: []CopyEntry{{Src: linkedSrcDir, RelDst: "dest"}},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want nested symlink escape error")
	}
	if _, statErr := os.Stat(filepath.Join(outsideDir, "pwn")); !os.IsNotExist(statErr) {
		t.Fatalf("outside destination stat error = %v, want not exist", statErr)
	}
}

func TestStageSessionWorkDirPreflightsNestedDestinationsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	treeSrc := filepath.Join(root, "tree-source")
	overlayDir := filepath.Join(root, "overlay")
	for _, dir := range []string{
		filepath.Join(workDir, "dest"),
		outsideDir,
		filepath.Join(treeSrc, "nested"),
		overlayDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "dest", "nested")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	validSrc := filepath.Join(root, "valid.txt")
	if err := os.WriteFile(validSrc, []byte("valid"), 0o644); err != nil {
		t.Fatalf("write valid source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(treeSrc, "nested", "pwn"), []byte("escaped"), 0o644); err != nil {
		t.Fatalf("write tree source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "overlay.txt"), []byte("overlay"), 0o644); err != nil {
		t.Fatalf("write overlay source: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:    workDir,
		OverlayDir: overlayDir,
		CopyFiles: []CopyEntry{
			{Src: validSrc, RelDst: "valid.txt"},
			{Src: treeSrc, RelDst: "dest"},
		},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want nested symlink escape error")
	}
	for _, path := range []string{
		filepath.Join(workDir, "overlay.txt"),
		filepath.Join(workDir, "valid.txt"),
		filepath.Join(outsideDir, "pwn"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("destination %q stat error = %v, want not exist", path, statErr)
		}
	}
}

func TestStageSessionWorkDirPreflightsCrossLayerFileDirectoryConflict(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	overlayDir := filepath.Join(root, "overlay")
	src := filepath.Join(root, "seed.txt")
	for _, dir := range []string{workDir, overlayDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "blocked"), []byte("overlay"), 0o644); err != nil {
		t.Fatalf("write overlay file: %v", err)
	}
	if err := os.WriteFile(src, []byte("copy"), 0o644); err != nil {
		t.Fatalf("write copy source: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:    workDir,
		OverlayDir: overlayDir,
		CopyFiles:  []CopyEntry{{Src: src, RelDst: filepath.Join("blocked", "seed.txt")}},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want cross-layer type conflict")
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "blocked")); !os.IsNotExist(statErr) {
		t.Fatalf("overlay was partially staged: stat error = %v", statErr)
	}
}

func TestStageSessionWorkDirPreflightsContainedSymlinkAliasesAcrossLayers(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	overlayDir := filepath.Join(root, "overlay")
	realDir := filepath.Join(workDir, "real")
	alias := filepath.Join(workDir, "alias")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real destination: %v", err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(overlayDir, "alias"), 0o755); err != nil {
		t.Fatalf("mkdir overlay alias: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "alias", "blocked"), []byte("overlay"), 0o644); err != nil {
		t.Fatalf("write overlay file: %v", err)
	}
	src := filepath.Join(root, "copy.txt")
	if err := os.WriteFile(src, []byte("copy"), 0o644); err != nil {
		t.Fatalf("write copy source: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:    workDir,
		OverlayDir: overlayDir,
		CopyFiles:  []CopyEntry{{Src: src, RelDst: filepath.Join("real", "blocked", "nested.txt")}},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want contained symlink alias conflict")
	}
	if _, statErr := os.Stat(filepath.Join(realDir, "blocked")); !os.IsNotExist(statErr) {
		t.Fatalf("overlay was partially staged through alias: stat error = %v, want not exist", statErr)
	}
}

func TestStageSessionWorkDirPreflightsOverlayLayerTypeConflict(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, dir := range []string{workDir, first, second} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "blocked"), []byte("first"), 0o644); err != nil {
		t.Fatalf("write first overlay: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(second, "blocked"), 0o755); err != nil {
		t.Fatalf("MkdirAll second nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "blocked", "seed.txt"), []byte("second"), 0o644); err != nil {
		t.Fatalf("write second overlay: %v", err)
	}

	err := StageSessionWorkDir(Config{WorkDir: workDir, PackOverlayDirs: []string{first, second}})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want overlay layer type conflict")
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "blocked")); !os.IsNotExist(statErr) {
		t.Fatalf("overlay was partially staged: stat error = %v", statErr)
	}
}

func TestStageSessionWorkDirPreflightsCopyLayerTypeConflict(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workdir: %v", err)
	}
	first := filepath.Join(root, "first.txt")
	second := filepath.Join(root, "second.txt")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatalf("write source %q: %v", path, err)
		}
	}

	err := StageSessionWorkDir(Config{
		WorkDir: workDir,
		CopyFiles: []CopyEntry{
			{Src: first, RelDst: "blocked"},
			{Src: second, RelDst: filepath.Join("blocked", "nested.txt")},
		},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want copy layer type conflict")
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "blocked")); !os.IsNotExist(statErr) {
		t.Fatalf("copy was partially staged: stat error = %v", statErr)
	}
}

func TestStageSessionWorkDirRejectsNestedSymlinkOverlayDestinationEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	overlayDir := filepath.Join(root, "overlay")
	for _, dir := range []string{
		workDir,
		outsideDir,
		filepath.Join(overlayDir, "nested"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "nested")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "nested", "pwn"), []byte("escaped"), 0o644); err != nil {
		t.Fatalf("write overlay source: %v", err)
	}

	err := StageSessionWorkDir(Config{WorkDir: workDir, OverlayDir: overlayDir})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want nested overlay symlink escape error")
	}
	if _, statErr := os.Stat(filepath.Join(outsideDir, "pwn")); !os.IsNotExist(statErr) {
		t.Fatalf("outside destination stat error = %v, want not exist", statErr)
	}
}

func TestStageSessionWorkDirPreflightsAllOverlaysBeforeMutation(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	firstOverlay := filepath.Join(root, "first-overlay")
	secondOverlay := filepath.Join(root, "second-overlay")
	for _, dir := range []string{
		workDir,
		outsideDir,
		firstOverlay,
		filepath.Join(secondOverlay, "nested"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "nested")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(firstOverlay, "first.txt"), []byte("first"), 0o644); err != nil {
		t.Fatalf("write first overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secondOverlay, "nested", "pwn"), []byte("escaped"), 0o644); err != nil {
		t.Fatalf("write second overlay: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:         workDir,
		PackOverlayDirs: []string{firstOverlay, secondOverlay},
	})
	if err == nil {
		t.Fatal("StageSessionWorkDir() succeeded, want nested overlay symlink escape error")
	}
	for _, path := range []string{
		filepath.Join(workDir, "first.txt"),
		filepath.Join(outsideDir, "pwn"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("destination %q stat error = %v, want not exist", path, statErr)
		}
	}
}

func TestStageSessionWorkDirAllowsContainedSymlinkCopyDestination(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	targetDir := filepath.Join(workDir, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(targetDir, filepath.Join(workDir, "linked")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	src := filepath.Join(root, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := StageSessionWorkDir(Config{
		WorkDir:   workDir,
		CopyFiles: []CopyEntry{{Src: src, RelDst: filepath.Join("linked", "copied.txt")}},
	}); err != nil {
		t.Fatalf("StageSessionWorkDir() error = %v, want contained symlink allowed", err)
	}
	if data, err := os.ReadFile(filepath.Join(targetDir, "copied.txt")); err != nil {
		t.Fatalf("read copied file: %v", err)
	} else if string(data) != "seed" {
		t.Fatalf("copied data = %q, want seed", data)
	}
}

func TestStageSessionWorkDirDoesNotRestageProbedFileAlreadyInWorkDir(t *testing.T) {
	workDir := t.TempDir()
	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir hook directory: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	if err := StageSessionWorkDir(Config{
		WorkDir: workDir,
		CopyFiles: []CopyEntry{{
			Src: hookPath, RelDst: filepath.Join("worker", ".codex", "hooks.json"), Probed: true,
		}},
	}); err != nil {
		t.Fatalf("StageSessionWorkDir() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "worker")); !os.IsNotExist(err) {
		t.Fatalf("nested restaging path stat error = %v, want not exist", err)
	}
}

func TestStageWorkDirRejectsOverlayWarningBeforePartialCopy(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	workDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "ok.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatalf("write ok overlay file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "blocked", "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write blocked overlay file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocked dst file: %v", err)
	}

	err := StageWorkDir(workDir, srcDir, nil)
	if err == nil {
		t.Fatal("StageWorkDir() succeeded, want overlay staging error")
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "ok.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("copied overlay stat error = %v, want not exist", statErr)
	}
}

func TestStageWorkDirPreflightsCopiesBeforeOverlayMutation(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	overlayDir := filepath.Join(root, "overlay")
	for _, dir := range []string{workDir, overlayDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "overlay.txt"), []byte("overlay"), 0o644); err != nil {
		t.Fatalf("write overlay source: %v", err)
	}

	err := StageWorkDir(workDir, overlayDir, []CopyEntry{{RelDst: filepath.Join("..", "outside")}})
	if err == nil {
		t.Fatal("StageWorkDir() succeeded, want malformed copy destination error")
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "overlay.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("overlay destination stat error = %v, want not exist", statErr)
	}
}

func TestStageSessionWorkDirUsesConcreteProviderOverlayName(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	kiroConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(kiroConfig), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(kiroConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("write Kiro overlay: %v", err)
	}
	claudeConfig := filepath.Join(packOverlay, "per-provider", "claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudeConfig), 0o755); err != nil {
		t.Fatalf("mkdir Claude overlay: %v", err)
	}
	if err := os.WriteFile(claudeConfig, []byte("claude instructions"), 0o644); err != nil {
		t.Fatalf("write Claude overlay: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:             workDir,
		ProviderName:        "claude",
		ProviderOverlayName: "kiro",
		PackOverlayDirs:     []string{packOverlay},
	})
	if err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, ".kiro", "agents", "gascity.json")); err != nil {
		t.Fatalf("read staged Kiro config: %v", err)
	} else if string(got) != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro config = %q, want gascity config", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, "CLAUDE.md")); err == nil {
		t.Fatal("staged Claude overlay for Kiro provider inheriting Claude launch behavior")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat Claude overlay: %v", err)
	}
}

// TestStageSessionWorkDirFallsBackToFamilyOverlayWhenConcreteAbsent guards
// gc-6bw8o: a custom provider with base="builtin:pi" resolves
// ProviderOverlayName="pi-vllm", which has no per-provider/pi-vllm/ overlay dir.
// The family overlay (per-provider/pi/, where gc-hooks.js lives) must still
// stage, otherwise the harness never signals ready and the agent churns. Unlike
// Kiro, the concrete overlay is absent, so the launch family is used.
func TestStageSessionWorkDirFallsBackToFamilyOverlayWhenConcreteAbsent(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	hook := filepath.Join(packOverlay, "per-provider", "pi", ".pi", "extensions", "gc-hooks.js")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatalf("mkdir pi overlay: %v", err)
	}
	if err := os.WriteFile(hook, []byte("// gc hook"), 0o644); err != nil {
		t.Fatalf("write pi hook: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:             workDir,
		ProviderName:        "pi",
		ProviderOverlayName: "pi-vllm",
		PackOverlayDirs:     []string{packOverlay},
	})
	if err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".pi", "extensions", "gc-hooks.js")); err != nil {
		t.Fatalf("family pi overlay not staged for custom pi-vllm provider (gc-6bw8o): %v", err)
	}
}

func TestStageSessionWorkDirWithWarningsSurfacesKiroPreservationWarning(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(fallbackInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro fallback instructions: %v", err)
	}
	projectInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(projectInstructions, []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	var warnings bytes.Buffer
	err := StageSessionWorkDirWithWarnings(Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("StageSessionWorkDirWithWarnings: %v", err)
	}
	if got := warnings.String(); !strings.Contains(got, "overlay: preserving existing") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("warnings = %q, want Kiro preservation warning", got)
	}
	data, err := os.ReadFile(projectInstructions)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "project instructions" {
		t.Fatalf("AGENTS.md = %q, want project instructions preserved", string(data))
	}
}

func TestStageProviderOverlayDirIgnoresWarningWriterFailure(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(fallbackInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro fallback instructions: %v", err)
	}
	projectInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(projectInstructions, []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	err := StageProviderOverlayDir(packOverlay, workDir, []string{"kiro"}, failingWriter{})
	if err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	data, err := os.ReadFile(projectInstructions)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "project instructions" {
		t.Fatalf("AGENTS.md = %q, want project instructions preserved", string(data))
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("writer unavailable")
}
