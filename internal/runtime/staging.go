package runtime

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/overlay"
)

// StageWorkDir applies overlay and CopyFiles staging before a provider starts
// the session process.
func StageWorkDir(workDir, overlayDir string, copyFiles []CopyEntry) error {
	if overlayDir != "" && workDir != "" {
		if err := StageDir(overlayDir, workDir); err != nil {
			return fmt.Errorf("overlay %q -> %q: %w", overlayDir, workDir, err)
		}
	}

	for _, cf := range copyFiles {
		dst := workDir
		if cf.RelDst != "" {
			dst = filepath.Join(workDir, cf.RelDst)
		}
		if absSrc, err := filepath.Abs(cf.Src); err == nil {
			if absDst, err := filepath.Abs(dst); err == nil && absSrc == absDst {
				continue
			}
		}
		if err := StagePath(cf.Src, dst); err != nil {
			return fmt.Errorf("copy file %q -> %q: %w", cf.Src, dst, err)
		}
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
