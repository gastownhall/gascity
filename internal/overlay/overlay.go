// Package overlay copies directory trees into agent working directories.
package overlay

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PreserveExistingWarningPrefix prefixes nonfatal warnings for provider overlay
// files that intentionally preserve an existing destination file.
const PreserveExistingWarningPrefix = "overlay: preserving existing "

// IsPreserveExistingWarning reports whether line is a nonfatal preservation
// warning emitted by provider-aware overlay staging.
func IsPreserveExistingWarning(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), PreserveExistingWarningPrefix)
}

// CopyFileOrDir copies src into dst. If src is a directory, it recursively
// copies all files into dst (like CopyDir). If src is a single file, it
// copies the file to dst, creating parent directories as needed. When dst
// already exists as a directory, the source basename is preserved under dst.
func CopyFileOrDir(src, dst string, stderr io.Writer) error {
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("overlay: stat %q: %w", src, err)
	}
	if info.IsDir() {
		return CopyDir(src, dst, stderr)
	}
	if dstInfo, err := os.Stat(dst); err == nil && dstInfo.IsDir() {
		dst = filepath.Join(dst, filepath.Base(src))
	}
	return copyFile(src, dst)
}

// CopyDir recursively copies all files from srcDir into dstDir.
// Directory structure is preserved. File permissions are preserved.
// If srcDir does not exist, returns nil (no-op).
// Individual file copy failures are logged to stderr but don't abort.
func CopyDir(srcDir, dstDir string, stderr io.Writer) error {
	return copyDir(srcDir, dstDir, stderr, nil)
}

type preserveExistingFunc func(relPath string) bool

func copyDir(srcDir, dstDir string, stderr io.Writer, preserveExisting preserveExistingFunc) error {
	info, err := os.Stat(srcDir)
	if os.IsNotExist(err) {
		return nil // Missing source dir is a no-op (like Gas Town).
	}
	if err != nil {
		return fmt.Errorf("overlay: stat %q: %w", srcDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("overlay: %q is not a directory", srcDir)
	}
	return copyDirRecursive(srcDir, dstDir, "", stderr, preserveExisting)
}

// copyDirRecursive walks srcBase/rel and copies files into dstBase/rel.
func copyDirRecursive(srcBase, dstBase, rel string, stderr io.Writer, preserveExisting preserveExistingFunc) error {
	srcPath := srcBase
	if rel != "" {
		srcPath = filepath.Join(srcBase, rel)
	}

	entries, err := os.ReadDir(srcPath)
	if err != nil {
		return fmt.Errorf("overlay: reading %q: %w", srcPath, err)
	}

	for _, entry := range entries {
		entryRel := entry.Name()
		if rel != "" {
			entryRel = filepath.Join(rel, entry.Name())
		}

		if entry.IsDir() {
			// Create destination subdirectory and recurse.
			dstSubDir := filepath.Join(dstBase, entryRel)
			if err := os.MkdirAll(dstSubDir, 0o755); err != nil {
				fmt.Fprintf(stderr, "overlay: mkdir %q: %v\n", dstSubDir, err) //nolint:errcheck
				continue
			}
			if err := copyDirRecursive(srcBase, dstBase, entryRel, stderr, preserveExisting); err != nil {
				fmt.Fprintf(stderr, "overlay: %v\n", err) //nolint:errcheck
			}
			continue
		}

		// Copy file (merge if applicable).
		src := filepath.Join(srcBase, entryRel)
		dst := filepath.Join(dstBase, entryRel)
		if preserveExisting != nil && preserveExisting(entryRel) {
			if _, err := os.Stat(dst); err == nil {
				fmt.Fprintf(stderr, "%s%q; skipped %q\n", PreserveExistingWarningPrefix, dst, src) //nolint:errcheck
				continue
			} else if !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "overlay: stat %q: %v\n", dst, err) //nolint:errcheck
				continue
			}
		}
		if err := copyOrMergeFile(src, dst, IsMergeablePath(entryRel)); err != nil {
			fmt.Fprintf(stderr, "overlay: %v\n", err) //nolint:errcheck
		}
	}
	return nil
}

// SkipFunc reports whether a file or directory should be skipped during copy.
// relPath is relative to the source root. isDir indicates whether it's a directory.
type SkipFunc func(relPath string, isDir bool) bool

// CopyDirWithSkip recursively copies srcDir into dstDir, skipping entries
// where skip returns true. If skip is nil, copies everything.
// Unlike CopyDir, this function does not silently ignore errors on individual
// files — it returns on the first error encountered.
func CopyDirWithSkip(srcDir, dstDir string, skip SkipFunc, _ io.Writer) error {
	info, err := os.Stat(srcDir)
	if os.IsNotExist(err) {
		return nil // Missing source dir is a no-op (consistent with CopyDir).
	}
	if err != nil {
		return fmt.Errorf("overlay: stat %q: %w", srcDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("overlay: %q is not a directory", srcDir)
	}
	return copyDirWithSkipRecursive(srcDir, dstDir, "", skip)
}

// copyDirWithSkipRecursive walks srcBase/rel and copies files into dstBase/rel,
// consulting skip for each entry.
func copyDirWithSkipRecursive(srcBase, dstBase, rel string, skip SkipFunc) error {
	srcPath := srcBase
	if rel != "" {
		srcPath = filepath.Join(srcBase, rel)
	}

	entries, err := os.ReadDir(srcPath)
	if err != nil {
		return fmt.Errorf("overlay: reading %q: %w", srcPath, err)
	}

	for _, entry := range entries {
		entryRel := entry.Name()
		if rel != "" {
			entryRel = filepath.Join(rel, entry.Name())
		}

		if skip != nil && skip(entryRel, entry.IsDir()) {
			continue
		}

		if entry.IsDir() {
			dstSubDir := filepath.Join(dstBase, entryRel)
			if err := os.MkdirAll(dstSubDir, 0o755); err != nil {
				return fmt.Errorf("overlay: mkdir %q: %w", dstSubDir, err)
			}
			if err := copyDirWithSkipRecursive(srcBase, dstBase, entryRel, skip); err != nil {
				return err
			}
			continue
		}

		src := filepath.Join(srcBase, entryRel)
		dst := filepath.Join(dstBase, entryRel)
		if err := copyOrMergeFile(src, dst, IsMergeablePath(entryRel)); err != nil {
			return err
		}
	}
	return nil
}

// PerProviderDir is the conventional subdirectory name for provider-specific
// overlay files. Files in overlay/per-provider/<provider>/ are copied to the
// agent's working directory only when the agent's resolved provider matches.
const PerProviderDir = "per-provider"

// CopyDirForProvider copies overlay files with provider awareness:
//  1. Copies everything EXCEPT the per-provider/ subtree (universal files).
//  2. If per-provider/<providerName>/ exists, copies its contents into dst
//     (flattened — the per-provider/<provider>/ prefix is stripped).
//
// This implements the V2 overlay layering described in doc-agent-v2.md.
func CopyDirForProvider(srcDir, dstDir, providerName string, stderr io.Writer) error {
	info, err := os.Stat(srcDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("overlay: stat %q: %w", srcDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("overlay: %q is not a directory", srcDir)
	}

	// Step 1: copy universal files (skip per-provider/).
	skip := func(relPath string, _ bool) bool {
		// Skip the per-provider directory itself and all its contents.
		return relPath == PerProviderDir || filepath.Dir(relPath) == PerProviderDir ||
			len(relPath) > len(PerProviderDir)+1 && relPath[:len(PerProviderDir)+1] == PerProviderDir+string(filepath.Separator)
	}
	if err := CopyDirWithSkip(srcDir, dstDir, skip, stderr); err != nil {
		return err
	}

	// Step 2: copy provider-specific files (flattened into dst).
	if providerName != "" {
		providerDir := filepath.Join(srcDir, PerProviderDir, providerName)
		if err := copyDir(providerDir, dstDir, stderr, providerPreserveExisting(providerName)); err != nil {
			return err
		}
	}

	return nil
}

// CopyDirForProviders copies overlay files for multiple provider slots.
// Universal (non per-provider/) files are copied once, then per-provider/<p>/
// content is copied for each name in providers. Used when an agent has
// install_agent_hooks declaring additional provider hook slots beyond its
// resolved provider — e.g. an agent running Claude that wants the Gemini
// hook staged too.
//
// Duplicate provider names in the list are de-duped; empty strings are
// skipped. The order in providers determines which per-provider copy
// wins when two providers ship the same rel path (last-writer-wins via
// overwrite or JSON merge).
func CopyDirForProviders(srcDir, dstDir string, providers []string, stderr io.Writer) error {
	info, err := os.Stat(srcDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("overlay: stat %q: %w", srcDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("overlay: %q is not a directory", srcDir)
	}

	// Step 1: copy universal files (skip per-provider/).
	skip := func(relPath string, _ bool) bool {
		return relPath == PerProviderDir || filepath.Dir(relPath) == PerProviderDir ||
			len(relPath) > len(PerProviderDir)+1 && relPath[:len(PerProviderDir)+1] == PerProviderDir+string(filepath.Separator)
	}
	if err := CopyDirWithSkip(srcDir, dstDir, skip, stderr); err != nil {
		return err
	}

	// Step 2: copy per-provider slots in order, deduped.
	seen := make(map[string]bool, len(providers))
	for _, p := range providers {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		providerDir := filepath.Join(srcDir, PerProviderDir, p)
		if err := copyDir(providerDir, dstDir, stderr, providerPreserveExisting(p)); err != nil {
			return err
		}
	}
	return nil
}

func providerPreserveExisting(providerName string) preserveExistingFunc {
	if providerName != "kiro" {
		return nil
	}
	return func(relPath string) bool {
		// Kiro's AGENTS.md is a workspace-root instruction fallback. Once any
		// workspace, pack, or earlier overlay has provided it, later Kiro
		// overlays preserve that file instead of replacing instructions.
		return filepath.Clean(relPath) == "AGENTS.md"
	}
}

// copyOrMergeFile copies src to dst, optionally merging JSON if merge is true
// and dst already exists. Falls back to plain copy on any merge error.
func copyOrMergeFile(src, dst string, merge bool) error {
	if !merge {
		return copyFile(src, dst)
	}
	// Only merge if destination already exists.
	dstInfo, dstErr := os.Stat(dst)
	if dstErr != nil {
		// Destination doesn't exist or can't be stat'd — canonicalize the
		// mergeable source JSON before creating it.
		return copyCanonicalJSONFile(src, dst, 0)
	}
	dstData, err := os.ReadFile(dst)
	if err != nil {
		return copyCanonicalJSONFile(src, dst, 0)
	}
	srcData, err := os.ReadFile(src)
	if err != nil {
		return copyCanonicalJSONFile(src, dst, 0)
	}
	merged, err := MergeSettingsJSON(dstData, srcData)
	if err != nil {
		// Merge failed — fall back to overwrite.
		return copyFile(src, dst)
	}
	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating parent for %q: %w", dst, err)
	}
	// Preserve the destination file's permissions.
	return os.WriteFile(dst, merged, dstInfo.Mode().Perm())
}

func copyCanonicalJSONFile(src, dst string, mode fs.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return copyFile(src, dst)
	}
	canonical, err := CanonicalJSON(data)
	if err != nil {
		return copyFile(src, dst)
	}
	if mode == 0 {
		info, err := os.Stat(src)
		if err != nil {
			return copyFile(src, dst)
		}
		mode = info.Mode()
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating parent for %q: %w", dst, err)
	}
	return os.WriteFile(dst, canonical, mode.Perm())
}

// copyFile copies a single file preserving permissions.
func copyFile(src, dst string) error {
	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating parent for %q: %w", dst, err)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %q: %w", src, err)
	}
	defer srcFile.Close() //nolint:errcheck // read-only file

	info, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("creating %q: %w", dst, err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		closeErr := dstFile.Close()
		_ = closeErr
		return fmt.Errorf("copying %q → %q: %w", src, dst, err)
	}
	return dstFile.Close()
}
