package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// CheckSeverity classifies an import state validation issue.
type CheckSeverity string

const (
	// CheckSeverityError means the import state is not usable as-is.
	CheckSeverityError CheckSeverity = "error"
)

// CheckIssue describes one read-only import state validation finding.
type CheckIssue struct {
	Severity   CheckSeverity
	Code       string
	ImportName string
	Source     string
	Commit     string
	Path       string
	Message    string
	RepairHint string
}

// CheckReport summarizes the read-only validation of a city's import state.
type CheckReport struct {
	CheckedSources int
	Issues         []CheckIssue
}

// ErrorCount returns the number of error-severity issues in the report.
func (r *CheckReport) ErrorCount() int {
	if r == nil {
		return 0
	}
	count := 0
	for _, issue := range r.Issues {
		if issue.Severity == CheckSeverityError {
			count++
		}
	}
	return count
}

// HasIssues reports whether validation found any issue.
func (r *CheckReport) HasIssues() bool {
	return r != nil && len(r.Issues) > 0
}

// CheckInstalled validates that declared remote imports are represented by
// packs.lock and by already-materialized local cache entries. It does not
// resolve versions, clone repositories, fetch, or mutate disk state.
func CheckInstalled(cityRoot string, imports map[string]config.Import) (*CheckReport, error) {
	report := &CheckReport{}

	lockExists, err := lockfileExists(cityRoot)
	if err != nil {
		return nil, err
	}
	lock, err := ReadLockfile(fsys.OSFS{}, cityRoot)
	if err != nil {
		return nil, err
	}

	if !lockExists && countRemoteImports(imports) > 0 {
		report.addIssue(CheckIssue{
			Code:       "missing-lockfile",
			Path:       filepath.Join(cityRoot, LockfileName),
			Message:    fmt.Sprintf("%s is missing for declared remote imports", LockfileName),
			RepairHint: `run "gc import install"`,
		})
		return report, nil
	}

	if countRemoteImports(imports) > 0 || len(lock.Packs) > 0 {
		if err := withRepoCacheReadLock(func() error {
			checkLockedImports(report, lock, imports)
			return nil
		}); err != nil {
			return nil, err
		}
	} else {
		checkLockedImports(report, lock, imports)
	}
	return report, nil
}

func checkLockedImports(report *CheckReport, lock *Lockfile, imports map[string]config.Import) {
	state := &importCheckState{
		lock:              lock,
		report:            report,
		constraints:       make(map[string]string),
		reachable:         make(map[string]struct{}),
		seen:              make(map[string]bool),
		reportedIssueKeys: make(map[string]struct{}),
	}

	names := sortedImportNames(imports)
	for _, name := range names {
		state.walkImport(name, imports[name])
	}

	state.reportStaleLockEntries()
}

type importCheckState struct {
	lock              *Lockfile
	report            *CheckReport
	constraints       map[string]string
	reachable         map[string]struct{}
	seen              map[string]bool
	reportedIssueKeys map[string]struct{}
	closureIncomplete bool
}

func (s *importCheckState) walkImport(name string, imp config.Import) {
	if !isRemoteSource(imp.Source) {
		return
	}

	mergedConstraint, err := mergeConstraints(s.constraints[imp.Source], imp.Version)
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "conflicting-constraints",
			ImportName: name,
			Source:     imp.Source,
			Message:    fmt.Sprintf("import constraints cannot be merged: %v", err),
			RepairHint: "edit imports to use compatible version constraints",
		})
		return
	}
	s.constraints[imp.Source] = mergedConstraint
	if _, ok := s.reachable[imp.Source]; !ok {
		s.report.CheckedSources++
	}
	s.reachable[imp.Source] = struct{}{}

	locked, ok := s.lock.Packs[imp.Source]
	if !ok {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "missing-lock-entry",
			ImportName: name,
			Source:     imp.Source,
			Message:    "declared remote import is not present in packs.lock",
			RepairHint: `run "gc import install"`,
		})
		return
	}
	if strings.TrimSpace(locked.Commit) == "" {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "missing-lock-commit",
			ImportName: name,
			Source:     imp.Source,
			Message:    "packs.lock entry is missing a commit",
			RepairHint: `run "gc import install"`,
		})
		return
	}
	if !matchesExisting(locked, mergedConstraint) {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "lock-constraint-mismatch",
			ImportName: name,
			Source:     imp.Source,
			Commit:     locked.Commit,
			Message:    fmt.Sprintf("packs.lock entry version %q does not satisfy constraint %q", locked.Version, mergedConstraint),
			RepairHint: `run "gc import install"`,
		})
		return
	}

	packDir, ok := s.validateCachedPack(name, imp.Source, locked.Commit)
	if !ok {
		return
	}
	nested, err := readPackImports(packDir)
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "invalid-cached-pack",
			ImportName: name,
			Source:     imp.Source,
			Commit:     locked.Commit,
			Path:       filepath.Join(packDir, "pack.toml"),
			Message:    err.Error(),
			RepairHint: `run "gc import install"`,
		})
		return
	}
	if !imp.ImportIsTransitive() {
		return
	}
	if s.seen[imp.Source] {
		return
	}
	s.seen[imp.Source] = true
	for _, nestedName := range sortedImportNames(nested) {
		s.walkImport(name+"/"+nestedName, nested[nestedName])
	}
}

func (s *importCheckState) validateCachedPack(name, source, commit string) (string, bool) {
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "invalid-cache-path",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Message:    err.Error(),
			RepairHint: `run "gc import install"`,
		})
		return "", false
	}

	gitPath := filepath.Join(cachePath, ".git")
	if st, err := os.Stat(gitPath); err != nil {
		if os.IsNotExist(err) {
			s.closureIncomplete = true
			s.addIssue(CheckIssue{
				Code:       "missing-cache",
				ImportName: name,
				Source:     source,
				Commit:     commit,
				Path:       cachePath,
				Message:    "locked import is missing from the local repo cache",
				RepairHint: `run "gc import install"`,
			})
			return "", false
		}
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "unreadable-cache",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       gitPath,
			Message:    fmt.Sprintf("cannot inspect cached repository: %v", err),
			RepairHint: `run "gc import install"`,
		})
		return "", false
	} else if st.IsDir() || st.Mode().IsRegular() {
		head, err := runGit(cachePath, "rev-parse", "HEAD")
		if err != nil {
			s.closureIncomplete = true
			s.addIssue(CheckIssue{
				Code:       "unreadable-cache-git",
				ImportName: name,
				Source:     source,
				Commit:     commit,
				Path:       cachePath,
				Message:    fmt.Sprintf("cannot read cached repository HEAD: %v", err),
				RepairHint: `run "gc import install"`,
			})
			return "", false
		}
		if !sameCommit(head, commit) {
			s.closureIncomplete = true
			s.addIssue(CheckIssue{
				Code:       "cache-checkout-mismatch",
				ImportName: name,
				Source:     source,
				Commit:     commit,
				Path:       cachePath,
				Message:    fmt.Sprintf("cached repository is checked out at %s, expected %s", strings.TrimSpace(head), commit),
				RepairHint: `run "gc import install"`,
			})
			return "", false
		}
		dirty, err := cachedRepoDirty(cachePath)
		if err != nil {
			s.closureIncomplete = true
			s.addIssue(CheckIssue{
				Code:       "unreadable-cache-git",
				ImportName: name,
				Source:     source,
				Commit:     commit,
				Path:       cachePath,
				Message:    fmt.Sprintf("cannot read cached repository status: %v", err),
				RepairHint: `run "gc import install"`,
			})
			return "", false
		}
		if dirty {
			s.closureIncomplete = true
			s.addIssue(CheckIssue{
				Code:       "cache-worktree-dirty",
				ImportName: name,
				Source:     source,
				Commit:     commit,
				Path:       cachePath,
				Message:    "cached repository has local worktree changes",
				RepairHint: `run "gc import install"`,
			})
			return "", false
		}
	} else {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "invalid-cache",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       gitPath,
			Message:    "cached repository .git entry is not a file or directory",
			RepairHint: `run "gc import install"`,
		})
		return "", false
	}

	packDir := cachedPackDir(source, cachePath)
	if st, err := os.Stat(filepath.Join(packDir, "pack.toml")); err != nil {
		if os.IsNotExist(err) {
			s.closureIncomplete = true
			s.addIssue(CheckIssue{
				Code:       "missing-cached-pack",
				ImportName: name,
				Source:     source,
				Commit:     commit,
				Path:       filepath.Join(packDir, "pack.toml"),
				Message:    "cached import is missing pack.toml",
				RepairHint: `run "gc import install"`,
			})
			return "", false
		}
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "unreadable-cached-pack",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       filepath.Join(packDir, "pack.toml"),
			Message:    fmt.Sprintf("cannot inspect cached pack.toml: %v", err),
			RepairHint: `run "gc import install"`,
		})
		return "", false
	} else if st.IsDir() {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "invalid-cached-pack",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       filepath.Join(packDir, "pack.toml"),
			Message:    "cached pack.toml is a directory",
			RepairHint: `run "gc import install"`,
		})
		return "", false
	}

	return packDir, true
}

func (s *importCheckState) reportStaleLockEntries() {
	if s.closureIncomplete {
		return
	}
	sources := make([]string, 0, len(s.lock.Packs))
	for source := range s.lock.Packs {
		if _, ok := s.reachable[source]; !ok {
			sources = append(sources, source)
		}
	}
	sort.Strings(sources)
	for _, source := range sources {
		pack := s.lock.Packs[source]
		s.addIssue(CheckIssue{
			Code:       "stale-lock-entry",
			Source:     source,
			Commit:     pack.Commit,
			Message:    "packs.lock contains a source that is not reachable from declared imports",
			RepairHint: `run "gc import install"`,
		})
	}
}

func (s *importCheckState) addIssue(issue CheckIssue) {
	key := issue.Code + "\x00" + issue.Source + "\x00" + issue.Commit + "\x00" + issue.Path
	if _, ok := s.reportedIssueKeys[key]; ok {
		return
	}
	s.reportedIssueKeys[key] = struct{}{}
	s.report.addIssue(issue)
}

func (r *CheckReport) addIssue(issue CheckIssue) {
	if issue.Severity == "" {
		issue.Severity = CheckSeverityError
	}
	r.Issues = append(r.Issues, issue)
}

func lockfileExists(cityRoot string) (bool, error) {
	_, err := os.Stat(filepath.Join(cityRoot, LockfileName))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking %s: %w", LockfileName, err)
}

func countRemoteImports(imports map[string]config.Import) int {
	count := 0
	for _, imp := range imports {
		if isRemoteSource(imp.Source) {
			count++
		}
	}
	return count
}

func sortedImportNames(imports map[string]config.Import) []string {
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cachedPackDir(source, cachePath string) string {
	if subpath := normalizeRemoteSource(source).Subpath; subpath != "" {
		return filepath.Join(cachePath, subpath)
	}
	return cachePath
}

func sameCommit(actual, expected string) bool {
	actual = strings.TrimSpace(actual)
	expected = strings.TrimSpace(expected)
	if actual == "" || expected == "" {
		return false
	}
	if strings.EqualFold(actual, expected) {
		return true
	}
	return len(expected) < len(actual) && strings.HasPrefix(strings.ToLower(actual), strings.ToLower(expected))
}
