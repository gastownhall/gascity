package runtime

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/overlay"
)

// HashHookSettingsContent returns a content hash for a probed hook/settings
// file that is stable across JSON serialization differences. For reconciler-owned
// mergeable settings files (overlay.IsMergeablePath — .gemini/settings.json,
// .codex/hooks.json, etc.) it hashes the canonical JSON form, so a compact
// document and its pretty-printed equivalent fingerprint identically.
//
// This keeps the CopyFiles fingerprint deterministic even though these files
// are rewritten into canonical form out of band by the reconciler — runtime
// overlay staging (StageProviderOverlayDir → MergeSettingsJSON) or hooks.Install.
// Without canonicalization the pre-fingerprint probe could hash a raw
// non-canonical document on one tick and its canonical rewrite on the next,
// producing spurious core-fingerprint drift. Non-mergeable paths, unreadable
// files, and non-JSON content fall back to raw content hashing (HashPathContent).
func HashHookSettingsContent(path, relPath string) string {
	if overlay.IsMergeablePath(relPath) {
		if data, err := os.ReadFile(path); err == nil {
			if canon, cErr := overlay.CanonicalJSON(data); cErr == nil {
				sum := sha256.Sum256(canon)
				return fmt.Sprintf("%x", sum)
			}
		}
	}
	return HashPathContent(path)
}

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
	return StageSessionWorkDirWithWarnings(cfg, os.Stderr)
}

// StageSessionWorkDirWithWarnings applies provider-aware pack overlays, the
// agent overlay, and CopyFiles staging before a provider starts the session
// process. Nonfatal overlay preservation warnings are written to warnings.
func StageSessionWorkDirWithWarnings(cfg Config, warnings io.Writer) error {
	if cfg.WorkDir != "" {
		overlayProviders := EffectiveOverlayProviderNames(cfg)
		for _, od := range cfg.PackOverlayDirs {
			if err := StageProviderOverlayDir(od, cfg.WorkDir, overlayProviders, warnings); err != nil {
				return fmt.Errorf("pack overlay %q -> %q: %w", od, cfg.WorkDir, err)
			}
		}
		if cfg.OverlayDir != "" {
			if err := StageProviderOverlayDir(cfg.OverlayDir, cfg.WorkDir, overlayProviders, warnings); err != nil {
				return fmt.Errorf("overlay %q -> %q: %w", cfg.OverlayDir, cfg.WorkDir, err)
			}
		}
	}
	return stageCopyFiles(cfg.WorkDir, cfg.CopyFiles)
}

// EffectiveOverlayProviderNames returns the provider overlay slots to stage for
// cfg, resolving the concrete-vs-family primary against cfg's overlay sources.
// The concrete cfg.ProviderOverlayName is honored only when a
// per-provider/<concrete>/ directory exists in one of cfg's overlay source dirs
// (PackOverlayDirs or OverlayDir); otherwise it is dropped so the slot list
// falls back to the launch family cfg.ProviderName. This keeps a provider that
// ships its own overlay (e.g. Kiro) on its concrete overlay, while letting a
// custom provider with no concrete overlay dir (e.g. base="builtin:pi"
// "pi-vllm", which has no per-provider/pi-vllm/) fall back to the family overlay
// (per-provider/pi/) where its lifecycle hooks live (gc-6bw8o).
//
// The pure OverlayProviderNames is retained for fingerprinting, which must stay
// filesystem-independent.
func EffectiveOverlayProviderNames(cfg Config) []string {
	overlayName := strings.TrimSpace(cfg.ProviderOverlayName)
	if overlayName != "" && !overlayProviderDirExists(cfg, overlayName) {
		overlayName = ""
	}
	return OverlayProviderNamesFromParts(cfg.ProviderName, overlayName, cfg.InstallAgentHooks)
}

// overlayProviderDirExists reports whether any of cfg's overlay source dirs
// contains a per-provider/<providerName>/ overlay directory.
func overlayProviderDirExists(cfg Config, providerName string) bool {
	for _, od := range cfg.PackOverlayDirs {
		if overlay.HasProviderDir(od, providerName) {
			return true
		}
	}
	return cfg.OverlayDir != "" && overlay.HasProviderDir(cfg.OverlayDir, providerName)
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

// StageProviderOverlayDir copies a provider-aware overlay directory into a
// work directory and writes nonfatal preservation warnings to warnings. This is
// the runtime task-worktree staging path: it stages every overlay file
// (including reconciler-owned mergeable hook files) because staging is the sole
// writer for live task sessions — hooks.Install never runs against these dirs.
func StageProviderOverlayDir(srcDir, dstDir string, providers []string, warnings io.Writer) error {
	return stageProviderOverlayDir(srcDir, dstDir, providers, nil, warnings)
}

// StageProviderOverlayDirSkippingMergeable copies a provider-aware overlay
// directory into a work directory like StageProviderOverlayDir, but skips
// reconciler-owned mergeable settings/hook files (overlay.IsMergeablePath —
// .codex/hooks.json, .claude/settings.json, etc.).
//
// It is used only by the build_desired_state home-dir staging path,
// which stages overlays and then immediately runs hooks.Install on the SAME
// directory. Skipping the mergeable files here makes hooks.Install the sole
// writer, so the two writers can no longer disagree on hook-entry matchers and
// leave a permanent codex-hooks-drift hybrid.
func StageProviderOverlayDirSkippingMergeable(srcDir, dstDir string, providers []string, warnings io.Writer) error {
	skip := func(relPath string, isDir bool) bool {
		return !isDir && overlay.IsMergeablePath(relPath)
	}
	return stageProviderOverlayDir(srcDir, dstDir, providers, skip, warnings)
}

// stageProviderOverlayDir stages srcDir into dstDir for the given provider
// slots, omitting any entry for which skip returns true (nil skips nothing).
//
// skip is spelled as an unnamed func type rather than overlay.SkipFunc — to
// which it stays assignable — because every declaration in package runtime must
// type-check with module-local imports stubbed out: the provider-double
// boundary guard (internal/testutil/providerledger) checks this package
// hermetically and requires module-local references to stay inside function
// bodies.
func stageProviderOverlayDir(srcDir, dstDir string, providers []string, skip func(relPath string, isDir bool) bool, warnings io.Writer) error {
	var stderr bytes.Buffer
	if err := overlay.CopyDirForProvidersWithSkip(srcDir, dstDir, providers, skip, &stderr); err != nil {
		return err
	}
	nonfatal, fatal := splitOverlayWarnings(stderr.String())
	if nonfatal != "" && warnings != nil {
		fmt.Fprintln(warnings, nonfatal) //nolint:errcheck // best-effort warning emission
	}
	if fatal != "" {
		return fmt.Errorf("%s", fatal)
	}
	return nil
}

func splitOverlayWarnings(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	var nonfatal []string
	var fatal []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if overlay.IsPreserveExistingWarning(line) {
			nonfatal = append(nonfatal, line)
			continue
		}
		fatal = append(fatal, line)
	}
	return strings.Join(nonfatal, "\n"), strings.Join(fatal, "\n")
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
