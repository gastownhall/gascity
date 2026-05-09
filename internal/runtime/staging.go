package runtime

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/overlay"
)

// StageWorkDir applies a legacy overlay directory and CopyFiles staging before
// a provider starts the session process.
func StageWorkDir(workDir, overlayDir string, copyFiles []CopyEntry) error {
	if overlayDir != "" && workDir != "" {
		if err := stageDirStrict(overlayDir, workDir); err != nil {
			return fmt.Errorf("overlay %q -> %q: %w", overlayDir, workDir, err)
		}
	}
	return stageCopyFiles(workDir, copyFiles)
}

// StageSessionWorkDir applies provider-aware pack overlays, the agent overlay,
// and CopyFiles staging before a provider starts the session process.
func StageSessionWorkDir(cfg Config) error {
	if cfg.WorkDir != "" {
		overlayProviders := append([]string{cfg.ProviderName}, cfg.InstallAgentHooks...)
		for _, od := range cfg.PackOverlayDirs {
			if err := stageProviderOverlayStrict(od, cfg.WorkDir, overlayProviders); err != nil {
				return fmt.Errorf("pack overlay %q -> %q: %w", od, cfg.WorkDir, err)
			}
		}
		if cfg.OverlayDir != "" {
			if err := stageProviderOverlayStrict(cfg.OverlayDir, cfg.WorkDir, overlayProviders); err != nil {
				return fmt.Errorf("overlay %q -> %q: %w", cfg.OverlayDir, cfg.WorkDir, err)
			}
		}
	}
	return stageCopyFiles(cfg.WorkDir, cfg.CopyFiles)
}

func stageCopyFiles(workDir string, copyFiles []CopyEntry) error {
	for _, cf := range copyFiles {
		dst := workDir
		if cf.RelDst != "" {
			dst = filepath.Join(workDir, cf.RelDst)
		}
		effectiveDst, err := effectiveStageDestination(cf.Src, dst)
		if err != nil {
			return fmt.Errorf("resolving copy destination %q -> %q: %w", cf.Src, dst, err)
		}
		if sameFile(cf.Src, effectiveDst) {
			continue
		}
		if err := StagePath(cf.Src, dst); err != nil {
			return fmt.Errorf("copy file %q -> %q: %w", cf.Src, dst, err)
		}
	}

	return nil
}

func stageProviderOverlayStrict(srcDir, dstDir string, providers []string) error {
	var stderr bytes.Buffer
	if err := overlay.CopyDirForProviders(srcDir, dstDir, providers, &stderr); err != nil {
		return err
	}
	if stderr.Len() > 0 {
		return fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

func stageDirStrict(srcDir, dstDir string) error {
	var stderr bytes.Buffer
	if err := overlay.CopyDir(srcDir, dstDir, &stderr); err != nil {
		return err
	}
	if stderr.Len() > 0 {
		return fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// StageDir copies a directory overlay while preserving CopyDir's historical
// best-effort behavior for per-path warnings.
func StageDir(srcDir, dstDir string) error {
	return overlay.CopyDir(srcDir, dstDir, &bytes.Buffer{})
}

// StagePath copies a file or directory and returns any per-file warnings as an
// error so callers can fail fast instead of ignoring partial staging.
func StagePath(src, dst string) error {
	var stderr bytes.Buffer
	if err := overlay.CopyFileOrDir(src, dst, &stderr); err != nil {
		return err
	}
	if stderr.Len() > 0 {
		return fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

func effectiveStageDestination(src, dst string) (string, error) {
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return dst, nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return dst, nil
	}
	if dstInfo, err := os.Stat(dst); err == nil && dstInfo.IsDir() {
		return filepath.Join(dst, filepath.Base(src)), nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return dst, nil
}

func sameFile(src, dst string) bool {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return false
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		return false
	}
	return os.SameFile(srcInfo, dstInfo)
}
