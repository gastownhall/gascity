package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	if workDir == "" {
		if overlayDir != "" || len(copyFiles) > 0 {
			return fmt.Errorf("staging files requires a non-empty workdir")
		}
		return nil
	}
	if err := ValidateLocalWorkDir(workDir); err != nil {
		return fmt.Errorf("invalid workdir %q: %w", workDir, err)
	}
	copyPlans, err := planCopyFiles(workDir, copyFiles)
	if err != nil {
		return err
	}
	if overlayDir != "" {
		if err := overlay.ValidateCopyDirDestination(overlayDir, workDir); err != nil {
			return fmt.Errorf("preflight overlay %q -> %q: %w", overlayDir, workDir, err)
		}
	}
	if err := validateLegacyStagingDestinationTypes(workDir, overlayDir, copyPlans); err != nil {
		return err
	}
	if overlayDir != "" {
		if err := stageDirStrict(overlayDir, workDir); err != nil {
			return fmt.Errorf("overlay %q -> %q: %w", overlayDir, workDir, err)
		}
	}
	return stageCopyPlans(copyPlans)
}

// StageSessionWorkDir applies provider-aware pack overlays, the agent overlay,
// and CopyFiles staging before a provider starts the session process.
func StageSessionWorkDir(cfg Config) error {
	return StageSessionWorkDirWithWarnings(cfg, os.Stderr)
}

// ValidateStageSessionWorkDir performs the complete local staging preflight
// without changing the work directory. It is used by providers that must
// configure external session infrastructure before their normal staging step,
// so a symlink escape or malformed batch is rejected before that mutation.
// Missing workdirs with staging and PreStart are reported as
// ErrWorkDirPendingPreStart; the caller may run PreStart and invoke this
// validator again before committing to infrastructure.
func ValidateStageSessionWorkDir(cfg Config) error {
	if cfg.WorkDir == "" {
		if len(cfg.PackOverlayDirs) > 0 || cfg.OverlayDir != "" || len(cfg.CopyFiles) > 0 {
			return fmt.Errorf("staging files requires a non-empty workdir")
		}
		return nil
	}
	if err := validateLocalWorkDirPath(cfg.WorkDir); err != nil {
		return fmt.Errorf("invalid workdir %q: %w", cfg.WorkDir, err)
	}
	if err := ValidateCopyEntries(cfg.CopyFiles); err != nil {
		return err
	}
	if len(cfg.PackOverlayDirs) == 0 && cfg.OverlayDir == "" && len(cfg.CopyFiles) == 0 {
		// A local workdir is still a launch boundary even when there is nothing
		// to copy. The sole deferred case is a missing directory that an
		// explicit pre_start is going to create.
		if err := ValidateLocalWorkDir(cfg.WorkDir); err != nil {
			if errors.Is(err, os.ErrNotExist) && len(cfg.PreStart) > 0 {
				return fmt.Errorf("%w: %q", ErrWorkDirPendingPreStart, cfg.WorkDir)
			}
			return fmt.Errorf("invalid workdir before staging: %w", err)
		}
		return nil
	}
	if err := ValidateLocalWorkDir(cfg.WorkDir); err != nil {
		if errors.Is(err, os.ErrNotExist) && len(cfg.PreStart) > 0 {
			return fmt.Errorf("%w: %q", ErrWorkDirPendingPreStart, cfg.WorkDir)
		}
		return fmt.Errorf("invalid workdir %q before staging: %w", cfg.WorkDir, err)
	}
	copyPlans, err := planCopyFiles(cfg.WorkDir, cfg.CopyFiles)
	if err != nil {
		return err
	}

	overlayProviders := EffectiveOverlayProviderNames(cfg)
	for _, od := range cfg.PackOverlayDirs {
		if err := overlay.ValidateCopyDirForProvidersDestination(od, cfg.WorkDir, overlayProviders, nil); err != nil {
			return fmt.Errorf("preflight pack overlay %q -> %q: %w", od, cfg.WorkDir, err)
		}
	}
	if cfg.OverlayDir != "" {
		if err := overlay.ValidateCopyDirForProvidersDestination(cfg.OverlayDir, cfg.WorkDir, overlayProviders, nil); err != nil {
			return fmt.Errorf("preflight overlay %q -> %q: %w", cfg.OverlayDir, cfg.WorkDir, err)
		}
	}
	if err := validateStagingDestinationTypes(cfg.WorkDir, cfg.PackOverlayDirs, cfg.OverlayDir, overlayProviders, copyPlans); err != nil {
		return err
	}
	return nil
}

// stagingDestinationPlan is a sparse, non-writing model of the destination
// tree. It starts from the existing filesystem and records directories/files
// that earlier overlay or copy layers would create. This catches conflicts
// between layers (for example an overlay file at "blocked" followed by a copy
// into "blocked/seed.txt") before the first real write occurs.
type stagingDestinationPlan struct {
	root  string
	kinds map[string]stagingPathKind
}

type stagingPathKind uint8

const (
	stagingPathMissing stagingPathKind = iota
	stagingPathDirectory
	stagingPathFile
)

func validateStagingDestinationTypes(workDir string, packOverlays []string, overlayDir string, providers []string, copyPlans []localCopyPlan) error {
	root, err := fsys.ResolveSymlinks(fsys.OSFS{}, filepath.Clean(workDir))
	if err != nil {
		return fmt.Errorf("preflight staging destinations: resolve workdir: %w", err)
	}
	plan := &stagingDestinationPlan{root: root, kinds: map[string]stagingPathKind{root: stagingPathDirectory}}
	for _, srcDir := range packOverlays {
		if err := planOverlayTree(srcDir, root, providers, plan); err != nil {
			return fmt.Errorf("preflight pack overlay %q: %w", srcDir, err)
		}
	}
	if overlayDir != "" {
		if err := planOverlayTree(overlayDir, root, providers, plan); err != nil {
			return fmt.Errorf("preflight overlay %q: %w", overlayDir, err)
		}
	}
	for _, copyPlan := range copyPlans {
		if err := planCopyTree(copyPlan.src, copyPlan.dst, plan); err != nil {
			return fmt.Errorf("preflight copy %q -> %q: %w", copyPlan.src, copyPlan.dst, err)
		}
	}
	return nil
}

func validateLegacyStagingDestinationTypes(workDir, overlayDir string, copyPlans []localCopyPlan) error {
	if overlayDir == "" && len(copyPlans) == 0 {
		return nil
	}
	root, err := fsys.ResolveSymlinks(fsys.OSFS{}, filepath.Clean(workDir))
	if err != nil {
		return fmt.Errorf("preflight staging destinations: resolve workdir: %w", err)
	}
	plan := &stagingDestinationPlan{root: root, kinds: map[string]stagingPathKind{root: stagingPathDirectory}}
	if overlayDir != "" {
		if err := planOverlayTreePhase(overlayDir, root, plan, func(relPath string, _ bool) bool {
			return isRuntimeMirrorPath(relPath)
		}, ""); err != nil {
			return fmt.Errorf("preflight overlay %q: %w", overlayDir, err)
		}
	}
	for _, copyPlan := range copyPlans {
		if err := planCopyTree(copyPlan.src, copyPlan.dst, plan); err != nil {
			return fmt.Errorf("preflight copy %q -> %q: %w", copyPlan.src, copyPlan.dst, err)
		}
	}
	return nil
}

func (p *stagingDestinationPlan) kind(path string) (stagingPathKind, error) {
	canonical, err := p.canonical(path)
	if err != nil {
		return stagingPathMissing, err
	}
	return p.kindCanonical(canonical)
}

// canonical maps lexical aliases onto the physical destination key used by
// the plan. A contained symlink (for example workdir/alias -> workdir/real)
// must not let one layer plan alias/blocked while a later layer plans
// real/blocked/nested: those are the same kernel path and the latter would
// fail after the former had already been written. ResolveSymlinks preserves a
// missing tail, so planned-but-not-yet-created components still participate in
// the same physical key space.
func (p *stagingDestinationPlan) canonical(path string) (string, error) {
	path = filepath.Clean(path)
	resolved, err := fsys.ResolveSymlinks(fsys.OSFS{}, path)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)
	if resolved != p.root && !pathContained(p.root, resolved) {
		return "", fmt.Errorf("destination %q escapes workdir", path)
	}
	return resolved, nil
}

func (p *stagingDestinationPlan) kindCanonical(path string) (stagingPathKind, error) {
	if kind, ok := p.kinds[path]; ok {
		return kind, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return stagingPathMissing, nil
	}
	if err != nil {
		return stagingPathMissing, err
	}
	if info.IsDir() {
		return stagingPathDirectory, nil
	}
	return stagingPathFile, nil
}

func (p *stagingDestinationPlan) ensureParents(path string) error {
	canonical, err := p.canonical(path)
	if err != nil {
		return err
	}
	parent := filepath.Clean(filepath.Dir(canonical))
	rel, err := filepath.Rel(p.root, parent)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	current := p.root
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		kind, err := p.kind(current)
		if err != nil {
			return err
		}
		switch kind {
		case stagingPathFile:
			return fmt.Errorf("destination parent %q is not a directory", current)
		case stagingPathMissing:
			p.kinds[current] = stagingPathDirectory
		}
	}
	return nil
}

func (p *stagingDestinationPlan) mkdir(path string) error {
	canonical, err := p.canonical(path)
	if err != nil {
		return err
	}
	kind, err := p.kindCanonical(canonical)
	if err != nil {
		return err
	}
	if kind == stagingPathFile {
		return fmt.Errorf("destination %q is a file, want directory", path)
	}
	if err := p.ensureParents(canonical); err != nil {
		return err
	}
	p.kinds[canonical] = stagingPathDirectory
	return nil
}

func (p *stagingDestinationPlan) write(path string) error {
	canonical, err := p.canonical(path)
	if err != nil {
		return err
	}
	kind, err := p.kindCanonical(canonical)
	if err != nil {
		return err
	}
	if kind == stagingPathDirectory {
		return fmt.Errorf("destination %q is a directory, want file", path)
	}
	if err := p.ensureParents(canonical); err != nil {
		return err
	}
	p.kinds[canonical] = stagingPathFile
	return nil
}

func planOverlayTree(srcDir, dstDir string, providers []string, plan *stagingDestinationPlan) error {
	if err := planOverlayTreePhase(srcDir, dstDir, plan, func(relPath string, _ bool) bool {
		return isRuntimeMirrorPath(relPath) || isPerProviderPath(relPath)
	}, ""); err != nil {
		return err
	}
	seen := make(map[string]bool, len(providers))
	for _, provider := range providers {
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		providerDir := filepath.Join(srcDir, overlay.PerProviderDir, provider)
		if err := planOverlayTreePhase(providerDir, dstDir, plan, func(relPath string, _ bool) bool {
			return isRuntimeMirrorPath(relPath)
		}, provider); err != nil {
			return err
		}
	}
	return nil
}

func planOverlayTreePhase(srcDir, dstDir string, plan *stagingDestinationPlan, skip func(string, bool) bool, provider string) error {
	info, err := os.Stat(srcDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", srcDir)
	}
	walkRoot, err := filepath.EvalSymlinks(srcDir)
	if err != nil {
		return err
	}
	return filepath.WalkDir(walkRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relPath, err := filepath.Rel(walkRoot, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		if skip(relPath, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dst := filepath.Join(dstDir, relPath)
		if provider == "kiro" && filepath.Clean(relPath) == "AGENTS.md" {
			kind, err := plan.kind(dst)
			if err != nil {
				return err
			}
			if kind != stagingPathMissing {
				return nil
			}
		}
		if entry.IsDir() {
			return plan.mkdir(dst)
		}
		return plan.write(dst)
	})
}

func planCopyTree(src, dst string, plan *stagingDestinationPlan) error {
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		kind, err := plan.kind(dst)
		if err != nil {
			return err
		}
		if kind == stagingPathDirectory {
			dst = filepath.Join(dst, filepath.Base(src))
		}
		return plan.write(dst)
	}
	walkRoot, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	return filepath.WalkDir(walkRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relPath, err := filepath.Rel(walkRoot, path)
		if err != nil {
			return err
		}
		if relPath == "." || isRuntimeMirrorPath(relPath) {
			if entry.IsDir() && relPath != "." {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, relPath)
		if entry.IsDir() {
			return plan.mkdir(target)
		}
		return plan.write(target)
	})
}

func isRuntimeMirrorPath(relPath string) bool {
	clean := filepath.Clean(relPath)
	return clean == ".gc" || strings.HasPrefix(clean, ".gc"+string(filepath.Separator))
}

func isPerProviderPath(relPath string) bool {
	clean := filepath.Clean(relPath)
	return clean == overlay.PerProviderDir || clean == overlay.PerProviderDir+string(filepath.Separator) || strings.HasPrefix(clean, overlay.PerProviderDir+string(filepath.Separator))
}

// PrepareSessionWorkDir performs the local provider startup boundary in one
// place: validate the complete staging batch, apply it, run pre_start, and
// validate the resulting workdir before a model process is created. A
// pre_start may create a previously missing workdir; in that case staging is
// deferred until the command succeeds. The returned config has PreStart
// cleared because the commands have already run.
func PrepareSessionWorkDir(ctx context.Context, cfg Config) (Config, error) {
	if err := ValidateStageSessionWorkDir(cfg); err != nil {
		if !errors.Is(err, ErrWorkDirPendingPreStart) {
			return cfg, err
		}
		for i, command := range cfg.PreStart {
			if err := RunPreStart(ctx, command, preStartEnvironment(cfg), defaultPreStartTimeout); err != nil {
				return cfg, fmt.Errorf("pre_start[%d]: %w", i, err)
			}
		}
		cfg.PreStart = nil
		if err := StageSessionWorkDir(cfg); err != nil {
			return cfg, err
		}
	} else {
		if err := StageSessionWorkDir(cfg); err != nil {
			return cfg, err
		}
		for i, command := range cfg.PreStart {
			if err := RunPreStart(ctx, command, preStartEnvironment(cfg), defaultPreStartTimeout); err != nil {
				return cfg, fmt.Errorf("pre_start[%d]: %w", i, err)
			}
		}
		cfg.PreStart = nil
	}
	if cfg.WorkDir != "" {
		if err := ValidateLocalWorkDir(cfg.WorkDir); err != nil {
			return cfg, fmt.Errorf("invalid workdir after pre_start: %w", err)
		}
	}
	return cfg, nil
}

const defaultPreStartTimeout = 30 * time.Second

func preStartEnvironment(cfg Config) map[string]string {
	env := make(map[string]string, len(cfg.Env)+1)
	for key, value := range cfg.Env {
		env[key] = value
	}
	if _, ok := env["GC_DIR"]; !ok && cfg.WorkDir != "" {
		env["GC_DIR"] = cfg.WorkDir
	}
	return env
}

// StageSessionWorkDirWithWarnings applies provider-aware pack overlays, the
// agent overlay, and CopyFiles staging before a provider starts the session
// process. Nonfatal overlay preservation warnings are written to warnings.
func StageSessionWorkDirWithWarnings(cfg Config, warnings io.Writer) error {
	if err := ValidateStageSessionWorkDir(cfg); err != nil {
		return err
	}
	if cfg.WorkDir == "" || (len(cfg.PackOverlayDirs) == 0 && cfg.OverlayDir == "" && len(cfg.CopyFiles) == 0) {
		return nil
	}
	copyPlans, err := planCopyFiles(cfg.WorkDir, cfg.CopyFiles)
	if err != nil {
		return err
	}

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
	return stageCopyPlans(copyPlans)
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

func stageCopyPlans(plans []localCopyPlan) error {
	for _, plan := range plans {
		if err := StagePath(plan.src, plan.dst); err != nil {
			return fmt.Errorf("copy file %q -> %q: %w", plan.src, plan.dst, err)
		}
	}
	return nil
}

type localCopyPlan struct {
	src string
	dst string
}

func planCopyFiles(workDir string, copyFiles []CopyEntry) ([]localCopyPlan, error) {
	if err := ValidateCopyEntries(copyFiles); err != nil {
		return nil, err
	}
	plans := make([]localCopyPlan, 0, len(copyFiles))
	for _, cf := range copyFiles {
		// CopyTo has always been best-effort for a source that disappeared
		// between discovery and staging. Keep that contract for batch callers,
		// while ValidateCopyEntries above still rejects every malformed
		// destination before returning.
		if _, statErr := os.Stat(cf.Src); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return nil, fmt.Errorf("stat copy source %q: %w", cf.Src, statErr)
		}
		dst, err := ResolveLocalCopyDestination(workDir, cf.RelDst)
		if err != nil {
			return nil, fmt.Errorf("resolving copy destination %q relative to %q: %w", cf.RelDst, workDir, err)
		}
		// Probed entries discovered inside the workdir already exist at the
		// launch boundary. They participate in the fingerprint, but copying them
		// to a separately projected remote-runtime path must not duplicate them
		// under a local workdir.
		if cf.Probed && localPathContained(workDir, cf.Src) {
			continue
		}
		effectiveDst, err := effectiveStageDestination(cf.Src, dst)
		if err != nil {
			return nil, fmt.Errorf("resolving copy destination %q -> %q: %w", cf.Src, dst, err)
		}
		if err := validateStagePathDestinations(workDir, cf.Src, dst); err != nil {
			return nil, fmt.Errorf("validating copy destination %q -> %q: %w", cf.Src, dst, err)
		}
		if sameFile(cf.Src, effectiveDst) {
			continue
		}
		plans = append(plans, localCopyPlan{src: cf.Src, dst: dst})
	}
	return plans, nil
}

func validateStagePathDestinations(workDir, src, dst string) error {
	root, err := fsys.ResolveSymlinks(fsys.OSFS{}, filepath.Clean(workDir))
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	validate := func(candidate string) error {
		resolved, err := fsys.ResolveSymlinks(fsys.OSFS{}, candidate)
		if err != nil {
			return fmt.Errorf("resolve destination %q: %w", candidate, err)
		}
		if !pathContained(root, resolved) {
			return fmt.Errorf("destination %q escapes workdir through symlink", candidate)
		}
		return nil
	}

	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		effectiveDst, err := effectiveStageDestination(src, dst)
		if err != nil {
			return err
		}
		return validate(effectiveDst)
	}

	walkRoot, err := filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("resolve source directory %q: %w", src, err)
	}
	return filepath.WalkDir(walkRoot, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(walkRoot, path)
		if err != nil {
			return err
		}
		return validate(filepath.Join(dst, rel))
	})
}

// ValidateLocalWorkDir rejects a local session workdir unless it is an
// existing absolute directory. Local providers call this before any staging or
// model process starts; remote providers validate their target filesystem at
// their own boundary.
func ValidateLocalWorkDir(workDir string) error {
	if err := validateLocalWorkDirPath(workDir); err != nil {
		return err
	}
	info, err := os.Stat(filepath.Clean(workDir))
	if os.IsNotExist(err) {
		return fmt.Errorf("workdir does not exist: %w", err)
	}
	if err != nil {
		return fmt.Errorf("stat workdir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workdir is not a directory")
	}
	if _, err := fsys.ResolveSymlinks(fsys.OSFS{}, filepath.Clean(workDir)); err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	return nil
}

func validateLocalWorkDirPath(workDir string) error {
	if workDir == "" {
		return fmt.Errorf("workdir is empty")
	}
	if !filepath.IsAbs(workDir) {
		return fmt.Errorf("workdir must be absolute")
	}
	return nil
}

// ValidateCopyRelDst rejects a CopyEntry destination that is absolute or can
// lexically escape its session workdir. Symlink containment is checked by
// ResolveLocalCopyDestination for local providers.
func ValidateCopyRelDst(relDst string) error {
	if relDst == "" || relDst == "." {
		return nil
	}
	if filepath.IsAbs(relDst) || filepath.VolumeName(relDst) != "" {
		return fmt.Errorf("copy destination must be relative")
	}
	clean := filepath.Clean(relDst)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("copy destination escapes workdir")
	}
	return nil
}

// ValidateCopyEntries validates every destination before a remote provider
// creates or mutates session infrastructure.
func ValidateCopyEntries(entries []CopyEntry) error {
	for i, entry := range entries {
		if err := ValidateCopyRelDst(entry.RelDst); err != nil {
			return fmt.Errorf("copy_files[%d] destination %q: %w", i, entry.RelDst, err)
		}
	}
	return nil
}

// ResolveLocalCopyDestination validates a local workdir and resolves relDst to
// a contained destination. Existing symlink components are resolved before the
// containment check, including when the final destination does not yet exist.
func ResolveLocalCopyDestination(workDir, relDst string) (string, error) {
	if err := ValidateLocalWorkDir(workDir); err != nil {
		return "", err
	}
	if err := ValidateCopyRelDst(relDst); err != nil {
		return "", err
	}

	root, err := fsys.ResolveSymlinks(fsys.OSFS{}, filepath.Clean(workDir))
	if err != nil {
		return "", fmt.Errorf("resolve workdir: %w", err)
	}
	target := root
	if relDst != "" && relDst != "." {
		target = filepath.Join(root, filepath.Clean(relDst))
	}
	resolved, err := fsys.ResolveSymlinks(fsys.OSFS{}, target)
	if err != nil {
		return "", fmt.Errorf("resolve copy destination: %w", err)
	}
	if !pathContained(root, resolved) {
		return "", fmt.Errorf("copy destination escapes workdir through symlink")
	}
	return resolved, nil
}

func localPathContained(root, candidate string) bool {
	rootResolved, err := fsys.ResolveSymlinks(fsys.OSFS{}, filepath.Clean(root))
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	candidateResolved, err := fsys.ResolveSymlinks(fsys.OSFS{}, candidateAbs)
	if err != nil {
		return false
	}
	return pathContained(rootResolved, candidateResolved)
}

func pathContained(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
// writer ON THE RECONCILE TICK, so the two writers can no longer disagree on
// hook-entry matchers and leave a permanent codex-hooks-drift hybrid.
//
// Not a global invariant: for a persistent (non-task) agent the home dir is
// also the session workDir, and session-start staging reaches these same paths
// through the non-skipping StageProviderOverlayDir (tmux.stageStartFiles,
// StageSessionWorkDir). A hybrid can therefore reappear at session start and is
// converged by the next tick — permanent drift becomes transient.
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
