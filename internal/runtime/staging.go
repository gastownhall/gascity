package runtime

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
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
			return hashHookSettingsData(data, relPath)
		}
	}
	return HashPathContent(path)
}

// hashHookSettingsData is the byte-snapshot form of
// HashHookSettingsContent. Mergeable JSON is canonicalized before hashing;
// other data is hashed as-is.
func hashHookSettingsData(data []byte, relPath string) string {
	if overlay.IsMergeablePath(relPath) {
		if canon, err := overlay.CanonicalJSON(data); err == nil {
			data = canon
		}
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
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
	if err := ValidateReconcilerOwnedCopyFiles(cfg); err != nil {
		return err
	}
	if cfg.WorkDir != "" {
		for _, od := range cfg.PackOverlayDirs {
			if err := StageConfiguredProviderOverlayDir(od, cfg.WorkDir, cfg, warnings); err != nil {
				return fmt.Errorf("pack overlay %q -> %q: %w", od, cfg.WorkDir, err)
			}
		}
		if cfg.OverlayDir != "" {
			if err := StageConfiguredProviderOverlayDir(cfg.OverlayDir, cfg.WorkDir, cfg, warnings); err != nil {
				return fmt.Errorf("overlay %q -> %q: %w", cfg.OverlayDir, cfg.WorkDir, err)
			}
		}
	}
	// Revalidate at the transport boundary. Overlay staging deliberately skips
	// owned paths, but a concurrent external edit must still prevent the
	// canonical self-copy/handoff from being treated as verified.
	if err := ValidateReconcilerOwnedCopyFiles(cfg); err != nil {
		return err
	}
	return stageCopyFiles(cfg.WorkDir, cfg.CopyFiles)
}

// ValidateReconcilerOwnedCopyFiles proves the fail-closed handoff between the
// settings reconciler and a runtime carrier. Every owned path must have exactly
// one probed CopyEntry sourced from the canonical regular file in WorkDir, and
// its current canonical content hash must still match the reconciled snapshot.
// Providers call this immediately before suppressing their normal overlay copy.
func ValidateReconcilerOwnedCopyFiles(cfg Config) error {
	if len(cfg.ReconcilerOwnedMergeablePaths) == 0 {
		return nil
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return errors.New("reconciler-owned mergeable paths require a workdir")
	}
	for _, rawOwned := range cfg.ReconcilerOwnedMergeablePaths {
		owned := filepath.Clean(filepath.FromSlash(rawOwned))
		if owned == "." || filepath.IsAbs(owned) || owned == ".." || strings.HasPrefix(owned, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid reconciler-owned mergeable path %q", rawOwned)
		}
		if !overlay.IsMergeablePath(owned) {
			return fmt.Errorf("reconciler-owned path %q is not a recognized mergeable hook/settings file", rawOwned)
		}
		var matches []CopyEntry
		for _, entry := range cfg.CopyFiles {
			if filepath.Clean(filepath.FromSlash(entry.RelDst)) == owned {
				matches = append(matches, entry)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("reconciler-owned mergeable path %q requires exactly one copy_file, got %d", rawOwned, len(matches))
		}
		entry := matches[0]
		if !entry.Probed || strings.TrimSpace(entry.ContentHash) == "" {
			return fmt.Errorf("reconciler-owned copy_file %q requires a probed content hash", rawOwned)
		}
		wantSrc := filepath.Join(cfg.WorkDir, owned)
		if !sameCleanPath(entry.Src, wantSrc) {
			return fmt.Errorf("reconciler-owned copy_file %q source %q is not canonical workdir path %q", rawOwned, entry.Src, wantSrc)
		}
		data, _, err := fsys.ReadRegularFileStable(fsys.OSFS{}, entry.Src)
		if err != nil {
			return fmt.Errorf("reading reconciler-owned copy_file %q safely: %w", rawOwned, err)
		}
		if current := fmt.Sprintf("%x", sha256.Sum256(data)); current != entry.ContentHash {
			return fmt.Errorf("reconciler-owned copy_file %q changed after reconciliation", rawOwned)
		}
	}
	return nil
}

func sameCleanPath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return filepath.Clean(absA) == filepath.Clean(absB)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// StageConfiguredProviderOverlayDir stages one provider-aware overlay source
// according to cfg's provider slots and mergeable-settings ownership policy.
// Reconciler-owned workdirs preserve mergeable settings installed before
// startup; isolated workdirs keep the legacy full-staging behavior.
func StageConfiguredProviderOverlayDir(srcDir, dstDir string, cfg Config, warnings io.Writer) error {
	providers := EffectiveOverlayProviderNames(cfg)
	if len(cfg.ReconcilerOwnedMergeablePaths) > 0 {
		return StageProviderOverlayDirSkippingPaths(srcDir, dstDir, providers, cfg.ReconcilerOwnedMergeablePaths, warnings)
	}
	return StageProviderOverlayDir(srcDir, dstDir, providers, warnings)
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

// StageProviderOverlayDirSkippingPaths stages a provider-aware overlay while
// preserving only the reconciler-owned mergeable files named by paths. Unknown
// and non-mergeable paths are ignored so a runtime hint cannot suppress an
// arbitrary overlay asset.
func StageProviderOverlayDirSkippingPaths(srcDir, dstDir string, providers, paths []string, warnings io.Writer) error {
	owned := make(map[string]struct{}, len(paths))
	for _, relPath := range paths {
		relPath = filepath.Clean(relPath)
		if overlay.IsMergeablePath(relPath) {
			owned[relPath] = struct{}{}
		}
	}
	skip := func(relPath string, isDir bool) bool {
		if isDir {
			return false
		}
		_, ok := owned[filepath.Clean(relPath)]
		return ok
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
