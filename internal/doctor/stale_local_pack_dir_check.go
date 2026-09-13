package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// StaleLocalPackDirCheck warns when a remote pack binding has a same-named
// local packs/<binding>/ directory that can mislead operators into editing a
// stale copy instead of the configured remote pack source.
type StaleLocalPackDirCheck struct {
	bindings []staleLocalPackBinding
	cityPath string
}

// NewStaleLocalPackDirCheck creates a stale local pack directory check.
func NewStaleLocalPackDirCheck(packs map[string]config.PackSource, imports map[string]config.Import, defaultRigImports map[string]config.Import, cityPath string, rigs ...config.Rig) *StaleLocalPackDirCheck {
	return &StaleLocalPackDirCheck{
		bindings: staleLocalPackBindings(packs, imports, defaultRigImports, rigs),
		cityPath: cityPath,
	}
}

// Name returns the check identifier.
func (c *StaleLocalPackDirCheck) Name() string { return "stale-local-pack-dirs" }

// Run checks for local packs/<binding>/ directories alongside remote bindings.
func (c *StaleLocalPackDirCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if len(c.bindings) == 0 {
		r.Status = StatusOK
		r.Message = "no remote pack bindings configured"
		return r
	}

	var stale []staleLocalPackDir
	staleByRel := make(map[string]int)
	var inspectErrors []string
	for _, binding := range c.bindings {
		rel, path, ok := localPackBindingPath(c.cityPath, binding.name)
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			inspectErrors = append(inspectErrors, fmt.Sprintf("could not inspect %s at %s: %v", binding.configRef, filepath.ToSlash(rel), err))
			continue
		}
		if !info.IsDir() {
			continue
		}
		if idx, ok := staleByRel[rel]; ok {
			stale[idx].bindings = append(stale[idx].bindings, binding)
			continue
		}
		staleByRel[rel] = len(stale)
		stale = append(stale, staleLocalPackDir{
			bindings: []staleLocalPackBinding{binding},
			rel:      rel,
		})
	}

	if len(stale) == 0 && len(inspectErrors) == 0 {
		r.Status = StatusOK
		r.Message = "no stale local pack directories"
		return r
	}

	r.Status = StatusWarning
	r.Details = append(r.Details, inspectErrors...)
	for _, hit := range stale {
		r.Details = append(r.Details, hit.detail())
	}
	if len(stale) == 0 {
		r.Message = fmt.Sprintf("could not inspect %d local pack %s", len(inspectErrors), localPackDirectoryNoun(len(inspectErrors)))
		return r
	}
	if len(stale) == 1 {
		r.Message = fmt.Sprintf("stale local pack directory: %s", filepath.ToSlash(stale[0].rel))
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
	bindings []staleLocalPackBinding
	rel      string
}

func (d staleLocalPackDir) operatorAction() string {
	source := ""
	if len(d.bindings) > 0 {
		source = d.bindings[0].source
	}
	return fmt.Sprintf("delete `%s/` (it's stale); edits go via PR on %s", filepath.ToSlash(d.rel), packSourceRepoName(source))
}

func (d staleLocalPackDir) detail() string {
	refs := make([]string, 0, len(d.bindings))
	for _, binding := range d.bindings {
		refs = append(refs, fmt.Sprintf("%s points at %s", binding.configRef, binding.source))
	}
	return fmt.Sprintf("%s exists while %s", filepath.ToSlash(d.rel), strings.Join(refs, "; "))
}

type staleLocalPackBinding struct {
	name      string
	source    string
	configRef string
}

func staleLocalPackBindings(packs map[string]config.PackSource, imports map[string]config.Import, defaultRigImports map[string]config.Import, rigs []config.Rig) []staleLocalPackBinding {
	bindings := make([]staleLocalPackBinding, 0, len(packs)+len(imports)+len(defaultRigImports))
	for name, src := range packs {
		bindings = append(bindings, staleLocalPackBinding{
			name:      name,
			source:    src.Source,
			configRef: fmt.Sprintf("[packs.%s]", name),
		})
	}
	for name, imp := range imports {
		if !remotesource.IsRemote(imp.Source) {
			continue
		}
		bindings = append(bindings, staleLocalPackBinding{
			name:      name,
			source:    imp.Source,
			configRef: fmt.Sprintf("[imports.%s]", name),
		})
	}
	for name, imp := range defaultRigImports {
		if !remotesource.IsRemote(imp.Source) {
			continue
		}
		bindings = append(bindings, staleLocalPackBinding{
			name:      name,
			source:    imp.Source,
			configRef: fmt.Sprintf("[defaults.rig.imports.%s]", name),
		})
	}
	for _, rig := range rigs {
		for name, imp := range rig.Imports {
			if !remotesource.IsRemote(imp.Source) {
				continue
			}
			bindings = append(bindings, staleLocalPackBinding{
				name:      name,
				source:    imp.Source,
				configRef: fmt.Sprintf("[rigs.%s.imports.%s]", rig.Name, name),
			})
		}
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].name == bindings[j].name {
			return bindings[i].configRef < bindings[j].configRef
		}
		return bindings[i].name < bindings[j].name
	})
	return bindings
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

func packSourceRepoName(src string) string {
	source := strings.TrimSuffix(strings.TrimRight(src, "/"), ".git")
	if source == "" {
		return "the remote pack repository"
	}
	if i := strings.LastIndexAny(source, "/:"); i >= 0 && i+1 < len(source) {
		return source[i+1:]
	}
	return source
}

func localPackDirectoryNoun(n int) string {
	if n == 1 {
		return "directory"
	}
	return "directories"
}
