package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// instructionsFileFallbacks lists the common project-instruction filenames
// in priority order. Providers point at one canonical name via
// `ProviderSpec.InstructionsFile` (defaulting to "AGENTS.md" via
// ResolveProvider). When the canonical file is missing but a fallback is
// present in the rig, the agent silently starts without project context;
// this check surfaces that mismatch.
var instructionsFileFallbacks = []string{
	"AGENTS.md",
	"CLAUDE.md",
	"INSTRUCTIONS.md",
}

// InstructionsFileCheck warns when a rig hosts an agent whose provider's
// `InstructionsFile` is missing from the rig directory while a known
// fallback file (CLAUDE.md ↔ AGENTS.md ↔ INSTRUCTIONS.md) exists in the
// same directory. Silently starting an agent with no project instructions
// is one of the show-stoppers from the non-Claude provider parity audit
// (Gap 7 in docs/research/w-7ed35a727f-parity-audit-classification.md /
// gastownhall/gascity#672).
//
// The check is read-only by default. `gc doctor --fix` symlinks the
// existing fallback to the expected name — symlink rather than copy so
// the source of truth stays in one place.
//
// Scope:
//   - One warning per (scope root × expected filename). The agent providers
//     associated with each root populate that root's "expected filenames"
//     set; the actual content of those files is not inspected.
//   - Agents with no dir or work_dir check the city root. Agents with only
//     work_dir check the same resolved root runtime uses. Rigs with no Path
//     are skipped (covered by the site-binding migration work; this check
//     fires once the binding lands).
//   - Empty fallback files are treated as present — if a user intentionally
//     ships empty AGENTS.md and CLAUDE.md, the check should not nag.
type InstructionsFileCheck struct {
	cfg      *config.City
	cityPath string
}

// NewInstructionsFileCheck creates a check that surfaces missing
// per-provider instruction files in each rig.
func NewInstructionsFileCheck(cfg *config.City, cityPath string) *InstructionsFileCheck {
	return &InstructionsFileCheck{cfg: cfg, cityPath: cityPath}
}

// Name returns the check identifier.
func (c *InstructionsFileCheck) Name() string { return "instructions-file" }

// Run reports any (rig, expected-filename) pairs where the file is missing
// but a known fallback exists. The fix hint is shown when at least one
// pair is auto-repairable via symlinking.
func (c *InstructionsFileCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no config; nothing to check"
		return r
	}
	gaps := c.collectGaps()
	if len(gaps) == 0 {
		r.Status = StatusOK
		r.Message = "instructions files look complete"
		return r
	}
	details := make([]string, 0, len(gaps))
	for _, g := range gaps {
		details = append(details, fmt.Sprintf(
			"%s: expected %s for %s is missing; %s is present",
			instructionsScopeDescription(g), g.expected, formatInstructionProviders(g.providers), g.fallback,
		))
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d instructions-file gap(s)", len(gaps))
	r.Details = details
	r.FixHint = "run `gc doctor --fix` to symlink the present fallback to the expected filename, or copy the file manually"
	return r
}

// CanFix reports that the check can symlink the fallback to the expected
// name when --fix is requested.
func (c *InstructionsFileCheck) CanFix() bool { return true }

// Fix symlinks the existing fallback to the expected filename for each
// recorded gap. Symlink failures are aggregated and returned as a single
// error; partial success is preserved (we do not roll back files that
// linked successfully).
func (c *InstructionsFileCheck) Fix(_ *CheckContext) error {
	if c.cfg == nil {
		return nil
	}
	gaps := c.collectGaps()
	var errs []error
	for _, g := range gaps {
		target := filepath.Join(g.rigPath, g.expected)
		// Re-check target state in case another check already fixed it.
		info, err := os.Lstat(target)
		if err == nil {
			if info.IsDir() {
				errs = append(errs, fmt.Errorf("%s: %s exists as a directory; remove it or replace it with a file before symlinking %s", instructionsScopeDescription(g), target, g.fallback))
			}
			continue
		}
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("%s: inspect %s: %w", instructionsScopeDescription(g), target, err))
			continue
		}
		if err := os.Symlink(g.fallback, target); err != nil {
			errs = append(errs, fmt.Errorf("%s: symlink %s -> %s: %w", instructionsScopeDescription(g), g.fallback, target, err))
		}
	}
	return errors.Join(errs...)
}

// instructionsFileGap is one missing-but-recoverable expected file in a
// city or rig scope root.
type instructionsFileGap struct {
	rigName   string
	rigPath   string
	scopeName string
	providers []string
	expected  string // canonical filename the provider wants
	fallback  string // name of a sibling file actually present in rigPath
}

type instructionsProviderExpectation struct {
	rigName   string
	rigPath   string
	scopeName string
	provider  string
	expected  string
}

type instructionsRigEntry struct {
	name      string
	path      string
	scopeName string
}

// collectGaps walks every agent-to-scope provider association and records
// the gaps. Output is sorted (scope, expected) and provider names are sorted
// inside each gap so warnings are stable across runs.
func (c *InstructionsFileCheck) collectGaps() []instructionsFileGap {
	expectations := c.instructionsProviderExpectations()
	if len(expectations) == 0 {
		return nil
	}

	gapsByKey := map[string]*instructionsFileGap{}
	for _, exp := range expectations {
		if instructionsFileExists(exp.rigPath, exp.expected) {
			continue
		}
		fb := firstFallback(exp.rigPath, exp.expected)
		if fb == "" {
			continue
		}
		key := exp.rigPath + "\x00" + exp.expected
		g, ok := gapsByKey[key]
		if !ok {
			g = &instructionsFileGap{
				rigName:   exp.rigName,
				rigPath:   exp.rigPath,
				scopeName: exp.scopeName,
				expected:  exp.expected,
				fallback:  fb,
			}
			gapsByKey[key] = g
		}
		if !slices.Contains(g.providers, exp.provider) {
			g.providers = append(g.providers, exp.provider)
		}
	}
	if len(gapsByKey) == 0 {
		return nil
	}

	gaps := make([]instructionsFileGap, 0, len(gapsByKey))
	for _, gap := range gapsByKey {
		sort.Strings(gap.providers)
		gaps = append(gaps, *gap)
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].rigName != gaps[j].rigName {
			return gaps[i].rigName < gaps[j].rigName
		}
		if gaps[i].rigPath != gaps[j].rigPath {
			return gaps[i].rigPath < gaps[j].rigPath
		}
		return gaps[i].expected < gaps[j].expected
	})
	return gaps
}

func (c *InstructionsFileCheck) instructionsProviderExpectations() []instructionsProviderExpectation {
	rigs := c.rigsByName()
	var out []instructionsProviderExpectation
	for _, a := range c.cfg.Agents {
		if a.StartCommand != "" {
			continue
		}
		if strings.TrimSpace(a.Provider) == "" && strings.TrimSpace(c.cfg.Workspace.Provider) == "" {
			continue
		}
		rig, ok := c.instructionsAgentScope(a, rigs)
		if !ok {
			continue
		}
		resolved, err := config.ResolveProvider(&a, &c.cfg.Workspace, c.cfg.Providers, providerParityLookPath)
		if err != nil {
			continue
		}
		provider := strings.TrimSpace(resolved.Name)
		expected, ok := safeInstructionsFilename(resolved.InstructionsFile)
		if provider == "" || !ok {
			continue
		}
		out = append(out, instructionsProviderExpectation{
			rigName:   rig.name,
			rigPath:   rig.path,
			scopeName: rig.scopeName,
			provider:  provider,
			expected:  expected,
		})
	}
	return out
}

func (c *InstructionsFileCheck) instructionsAgentScope(a config.Agent, rigs map[string]instructionsRigEntry) (instructionsRigEntry, bool) {
	if strings.TrimSpace(a.Scope) == "city" {
		return instructionsRigEntry{path: filepath.Clean(c.cityPath), scopeName: "city root"}, true
	}
	if strings.TrimSpace(a.Dir) == "" && strings.TrimSpace(a.WorkDir) == "" {
		return instructionsRigEntry{path: filepath.Clean(c.cityPath), scopeName: "city root"}, true
	}
	if strings.TrimSpace(a.Dir) == "" {
		workDir, err := workdirutil.ResolveWorkDirPathStrict(
			c.cityPath,
			workdirutil.CityName(c.cityPath, c.cfg),
			a.QualifiedName(),
			a,
			c.cfg.Rigs,
		)
		if err != nil {
			return instructionsRigEntry{}, false
		}
		return c.scopeForResolvedPath(filepath.Clean(workDir), rigs), true
	}
	rigName := workdirutil.ConfiguredRigName(c.cityPath, a, c.cfg.Rigs)
	if rigName == "" {
		return instructionsRigEntry{}, false
	}
	rig, ok := rigs[rigName]
	return rig, ok
}

func (c *InstructionsFileCheck) scopeForResolvedPath(path string, rigs map[string]instructionsRigEntry) instructionsRigEntry {
	if filepath.Clean(path) == filepath.Clean(c.cityPath) {
		return instructionsRigEntry{path: filepath.Clean(c.cityPath), scopeName: "city root"}
	}
	names := make([]string, 0, len(rigs))
	for name := range rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rig := rigs[name]
		if filepath.Clean(rig.path) == filepath.Clean(path) {
			return rig
		}
	}
	return instructionsRigEntry{path: filepath.Clean(path), scopeName: "work_dir"}
}

func (c *InstructionsFileCheck) rigsByName() map[string]instructionsRigEntry {
	rigs := map[string]instructionsRigEntry{}
	for _, r := range c.cfg.Rigs {
		p := strings.TrimSpace(r.Path)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.cityPath, p)
		}
		name := strings.TrimSpace(r.Name)
		if name == "" {
			continue
		}
		rigs[name] = instructionsRigEntry{name: name, path: filepath.Clean(p)}
	}
	return rigs
}

// instructionsFileExists reports whether name (a filename, not a path)
// exists as a regular file or symlink inside dir. Empty files count as
// present — users sometimes ship empty placeholders intentionally. This
// deliberately uses Lstat so an existing expected-name symlink, even if
// dangling, is treated as user-owned state rather than a missing file.
func instructionsFileExists(dir, name string) bool {
	name, ok := safeInstructionsFilename(name)
	if !ok {
		return false
	}
	info, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// firstFallback returns the name of the first known instruction file
// present in dir that is NOT want, in priority order. Empty string when
// no fallback is present. Fallbacks must resolve to usable files because
// Fix links the missing expected name to the fallback.
func firstFallback(dir, want string) string {
	want, ok := safeInstructionsFilename(want)
	if !ok {
		return ""
	}
	for _, name := range instructionsFileFallbacks {
		if name == want {
			continue
		}
		if instructionsFallbackUsable(dir, name) {
			return name
		}
	}
	return ""
}

func instructionsFallbackUsable(dir, name string) bool {
	name, ok := safeInstructionsFilename(name)
	if !ok {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func safeInstructionsFilename(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", false
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", false
	}
	return name, true
}

func instructionsScopeDescription(g instructionsFileGap) string {
	if g.rigName == "" {
		scopeName := g.scopeName
		if scopeName == "" {
			scopeName = "city root"
		}
		return fmt.Sprintf("%s (%s)", scopeName, g.rigPath)
	}
	return fmt.Sprintf("rig %q (%s)", g.rigName, g.rigPath)
}

func formatInstructionProviders(providers []string) string {
	if len(providers) == 1 {
		return fmt.Sprintf("provider %q", providers[0])
	}
	quoted := make([]string, 0, len(providers))
	for _, provider := range providers {
		quoted = append(quoted, fmt.Sprintf("%q", provider))
	}
	return "providers " + strings.Join(quoted, ", ")
}
