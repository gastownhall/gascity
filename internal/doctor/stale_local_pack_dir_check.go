package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// StaleLocalPackDirCheck warns when a remote pack binding has a same-named
// local packs/<binding>/ directory that can mislead operators into editing a
// stale copy instead of the configured remote pack source.
type StaleLocalPackDirCheck struct {
	packs    map[string]config.PackSource
	cityPath string
}

// NewStaleLocalPackDirCheck creates a stale local pack directory check.
func NewStaleLocalPackDirCheck(packs map[string]config.PackSource, cityPath string) *StaleLocalPackDirCheck {
	return &StaleLocalPackDirCheck{packs: packs, cityPath: cityPath}
}

// Name returns the check identifier.
func (c *StaleLocalPackDirCheck) Name() string { return "stale-local-pack-dirs" }

// Run checks for local packs/<binding>/ directories alongside remote bindings.
func (c *StaleLocalPackDirCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if len(c.packs) == 0 {
		r.Status = StatusOK
		r.Message = "no remote pack bindings configured"
		return r
	}

	var stale []staleLocalPackDir
	for _, name := range sortedPackBindingNames(c.packs) {
		rel, path, ok := localPackBindingPath(c.cityPath, name)
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("could not inspect local pack directory %s", rel)
			r.Details = []string{err.Error()}
			return r
		}
		if !info.IsDir() {
			continue
		}
		stale = append(stale, staleLocalPackDir{
			binding: name,
			rel:     rel,
			source:  c.packs[name],
		})
	}

	if len(stale) == 0 {
		r.Status = StatusOK
		r.Message = "no stale local pack directories"
		return r
	}

	r.Status = StatusWarning
	for _, hit := range stale {
		r.Details = append(r.Details, fmt.Sprintf("%s exists while [packs.%s] points at %s", hit.rel, hit.binding, hit.source.Source))
	}
	if len(stale) == 1 {
		r.Message = stale[0].operatorAction()
		r.FixHint = stale[0].operatorAction()
		return r
	}
	r.Message = fmt.Sprintf("%d stale local pack directories", len(stale))
	r.FixHint = "delete each stale packs/<binding>/ directory; edits go via PR on the corresponding remote pack repository"
	return r
}

// CanFix returns false; deleting local pack directories is an operator choice.
func (c *StaleLocalPackDirCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *StaleLocalPackDirCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this guard is informational and not a startup
// prerequisite.
func (c *StaleLocalPackDirCheck) WarmupEligible() bool { return false }

type staleLocalPackDir struct {
	binding string
	rel     string
	source  config.PackSource
}

func (d staleLocalPackDir) operatorAction() string {
	return fmt.Sprintf("delete `%s/` (it's stale); edits go via PR on %s", filepath.ToSlash(d.rel), packSourceRepoName(d.source))
}

func sortedPackBindingNames(packs map[string]config.PackSource) []string {
	names := make([]string, 0, len(packs))
	for name := range packs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func localPackBindingPath(cityPath, binding string) (rel string, path string, ok bool) {
	if binding == "" || filepath.IsAbs(binding) {
		return "", "", false
	}
	rel = filepath.Clean(filepath.Join("packs", filepath.FromSlash(binding)))
	packRoot := "packs" + string(filepath.Separator)
	if !strings.HasPrefix(rel, packRoot) {
		return "", "", false
	}
	return rel, filepath.Join(cityPath, rel), true
}

func packSourceRepoName(src config.PackSource) string {
	source := strings.TrimSuffix(strings.TrimRight(src.Source, "/"), ".git")
	if source == "" {
		return "the remote pack repository"
	}
	if i := strings.LastIndexAny(source, "/:"); i >= 0 && i+1 < len(source) {
		return source[i+1:]
	}
	return source
}
