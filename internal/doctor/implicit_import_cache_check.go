package doctor

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/bootstrap"
	"github.com/gastownhall/gascity/internal/config"
)

// ImplicitImportCacheCheck verifies that bootstrap-managed implicit imports
// resolve to the canonical user-global repo cache path. Older builds used a
// pre-normalization cache key, which leaves bootstrap packs under stale
// ~/.gc/cache/repos/<hash>/ directories that newer loaders will not read.
type ImplicitImportCacheCheck struct{}

type implicitImportCacheIssue struct {
	name          string
	canonicalPath string
	legacyPath    string
	status        CheckStatus
	message       string
}

// Name returns the check identifier.
func (c *ImplicitImportCacheCheck) Name() string { return "implicit-import-cache" }

// Run checks bootstrap-managed implicit import cache paths.
func (c *ImplicitImportCacheCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if len(bootstrap.BootstrapPacks) == 0 {
		r.Status = StatusOK
		r.Message = "no bootstrap implicit imports configured"
		return r
	}

	imports, implicitPath, err := config.ReadImplicitImports()
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("reading implicit imports: %v", err)
		return r
	}
	if implicitPath == "" {
		r.Status = StatusOK
		r.Message = "implicit import home unavailable"
		return r
	}

	issues := inspectBootstrapImplicitImportCaches(filepath.Dir(implicitPath), imports)
	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "bootstrap implicit import caches present"
		return r
	}

	errors := 0
	for _, issue := range issues {
		r.Details = append(r.Details, issue.message)
		if issue.status == StatusError {
			errors++
		}
	}

	if errors > 0 {
		r.Status = StatusError
		r.Message = fmt.Sprintf("%d bootstrap implicit import cache issue(s)", len(issues))
		r.FixHint = `run "gc doctor --fix" to backfill bootstrap implicit import caches`
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d stale bootstrap implicit cache %s", len(issues), pluralEntry(len(issues)))
	r.FixHint = `run "gc doctor --fix" to prune stale legacy bootstrap implicit caches`
	return r
}

// CanFix returns true — the bootstrap manager can re-materialize canonical
// caches and doctor can prune stale legacy cache keys.
func (c *ImplicitImportCacheCheck) CanFix() bool { return true }

// Fix re-materializes bootstrap-managed implicit imports and prunes stale
// legacy-key cache directories when a canonical cache is present.
func (c *ImplicitImportCacheCheck) Fix(_ *CheckContext) error {
	if len(bootstrap.BootstrapPacks) == 0 {
		return nil
	}
	importsBefore, implicitPath, err := config.ReadImplicitImports()
	if err != nil {
		return err
	}
	if implicitPath == "" {
		return nil
	}

	gcHome := filepath.Dir(implicitPath)
	issuesBefore := inspectBootstrapImplicitImportCaches(gcHome, importsBefore)
	if err := ensureBootstrapForDoctor(gcHome); err != nil {
		return err
	}

	importsAfter, implicitPathAfter, err := config.ReadImplicitImports()
	if err != nil {
		return err
	}
	if implicitPathAfter == "" {
		return nil
	}
	gcHome = filepath.Dir(implicitPathAfter)

	for _, issue := range issuesBefore {
		if issue.legacyPath == "" || issue.legacyPath == issue.canonicalPath {
			continue
		}
		imp, ok := importsAfter[issue.name]
		if !ok {
			continue
		}
		canonical := config.GlobalRepoCachePath(gcHome, imp.Source, imp.Commit)
		if !hasImplicitImportPack(canonical) {
			continue
		}
		if err := os.RemoveAll(issue.legacyPath); err != nil {
			return fmt.Errorf("removing stale legacy cache for %q: %w", issue.name, err)
		}
	}

	return nil
}

func inspectBootstrapImplicitImportCaches(gcHome string, imports map[string]config.ImplicitImport) []implicitImportCacheIssue {
	if gcHome == "" || len(imports) == 0 {
		return nil
	}

	bootstrapByName := make(map[string]bootstrap.Entry, len(bootstrap.BootstrapPacks))
	for _, entry := range bootstrap.BootstrapPacks {
		bootstrapByName[entry.Name] = entry
	}

	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)

	var issues []implicitImportCacheIssue
	for _, name := range names {
		imp := imports[name]
		if imp.Commit == "" {
			continue
		}

		entry, ok := bootstrapByName[name]
		if !ok {
			continue
		}
		if config.NormalizeRemoteSource(entry.Source) != config.NormalizeRemoteSource(imp.Source) {
			continue
		}

		canonical := config.GlobalRepoCachePath(gcHome, imp.Source, imp.Commit)
		hasCanonical := hasImplicitImportPack(canonical)
		legacy := legacyImplicitImportCachePath(gcHome, entry.Source, imp.Commit)
		hasLegacy := legacy != canonical && hasImplicitImportPack(legacy)

		switch {
		case !hasCanonical && hasLegacy:
			issues = append(issues, implicitImportCacheIssue{
				name:          name,
				canonicalPath: canonical,
				legacyPath:    legacy,
				status:        StatusError,
				message: fmt.Sprintf(
					"implicit import %q is cached only under legacy key %s; expected canonical cache at %s",
					name, legacy, canonical,
				),
			})
		case !hasCanonical:
			issues = append(issues, implicitImportCacheIssue{
				name:          name,
				canonicalPath: canonical,
				status:        StatusError,
				message: fmt.Sprintf(
					"implicit import %q is missing bootstrap cache at %s",
					name, canonical,
				),
			})
		case hasLegacy:
			issues = append(issues, implicitImportCacheIssue{
				name:          name,
				canonicalPath: canonical,
				legacyPath:    legacy,
				status:        StatusWarning,
				message: fmt.Sprintf(
					"implicit import %q has stale legacy cache %s alongside canonical cache %s",
					name, legacy, canonical,
				),
			})
		}
	}

	return issues
}

func ensureBootstrapForDoctor(gcHome string) error {
	prev, hadPrev := os.LookupEnv("GC_BOOTSTRAP")
	if err := os.Unsetenv("GC_BOOTSTRAP"); err != nil {
		return err
	}
	defer func() {
		if hadPrev {
			_ = os.Setenv("GC_BOOTSTRAP", prev)
			return
		}
		_ = os.Unsetenv("GC_BOOTSTRAP")
	}()

	return bootstrap.EnsureBootstrap(gcHome)
}

func hasImplicitImportPack(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "pack.toml"))
	return err == nil
}

func legacyImplicitImportCachePath(gcHome, source, commit string) string {
	sum := sha256.Sum256([]byte(source + commit))
	return filepath.Join(gcHome, "cache", "repos", fmt.Sprintf("%x", sum[:]))
}

func pluralEntry(n int) string {
	if n == 1 {
		return "entry"
	}
	return "entries"
}
