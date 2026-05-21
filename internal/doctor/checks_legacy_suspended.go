package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// LegacySuspendedFieldCheck warns when city.toml carries the
// deprecated `suspended` boolean on `[workspace]` or any `[[rig]]`
// entry. The field is honored as an alias for `suspended_on_start`
// by the read sites (so existing cities keep their behavior on
// upgrade), but the new spelling is `suspended_on_start` and live
// suspend/resume state moved to .gc/runtime/suspension-state.json.
// Surfacing the legacy field, and offering `--fix` to rename it,
// lets users migrate cleanly.
//
// Agent-scope `suspended` migration is out of scope here and tracked
// separately under issue #2407.
type LegacySuspendedFieldCheck struct {
	cfg *config.City
}

// NewLegacySuspendedFieldCheck creates a check that flags legacy
// `suspended` fields in city.toml.
func NewLegacySuspendedFieldCheck(cfg *config.City) *LegacySuspendedFieldCheck {
	return &LegacySuspendedFieldCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *LegacySuspendedFieldCheck) Name() string { return "legacy-suspended-field" }

// Run inspects [workspace] and each [[rig]] for the deprecated
// `suspended` field. Returns a warning when any is set.
func (c *LegacySuspendedFieldCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no city config loaded"
		return r
	}

	var issues []string
	if c.cfg.Workspace.Suspended {
		issues = append(issues,
			`[workspace] suspended is deprecated; it is honored as an alias for suspended_on_start so existing configs keep working, but new spelling is suspended_on_start. Live suspend/resume now lives in .gc/runtime/suspension-state.json.`)
	}
	for i := range c.cfg.Rigs {
		rig := &c.cfg.Rigs[i]
		if !rig.Suspended {
			continue
		}
		issues = append(issues, fmt.Sprintf(
			`[[rig]] %q suspended is deprecated; honored as alias for suspended_on_start. New spelling is suspended_on_start; live suspend/resume now lives in .gc/runtime/suspension-state.json.`,
			rig.Name,
		))
	}

	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "no deprecated suspended fields in city.toml"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d deprecated suspended field(s) found in city.toml", len(issues))
	r.Details = issues
	r.FixHint = `run "gc doctor --fix" to rename "suspended" → "suspended_on_start" in city.toml (preserves comments and formatting)`
	return r
}

// CanFix reports whether automatic remediation is supported. The
// rewrite is mechanical (line-based: rename the key, keep the value,
// indentation, and trailing comment) so the doctor offers it as
// auto-fix. The fix touches only `[workspace]` and `[[rig]]` /
// `[[rigs]]` sections — agent-scope migration is tracked in #2407.
func (c *LegacySuspendedFieldCheck) CanFix() bool { return true }

// Fix renames `suspended` → `suspended_on_start` in city.toml in
// every `[workspace]` and `[[rig]]` section that carries it. The
// rewrite is line-based to preserve comments, blank lines, and key
// ordering that the BurntSushi/toml round-trip would otherwise lose.
//
// When a section already has both `suspended` and `suspended_on_start`,
// the rewrite is skipped for that section: the user has to pick which
// one wins manually (the legacy field's value is honored at read time
// regardless, so behavior is preserved).
func (c *LegacySuspendedFieldCheck) Fix(ctx *CheckContext) error {
	if ctx == nil || ctx.CityPath == "" {
		return nil
	}
	path := filepath.Join(ctx.CityPath, "city.toml")
	_, err := renameLegacySuspendedInCityTOML(path)
	return err
}

// WarmupEligible reports whether this check runs during `gc start`'s
// warm-up scan. It does — the warning is most useful right when the
// user is about to act on a stale-config view.
func (c *LegacySuspendedFieldCheck) WarmupEligible() bool { return true }

// suspendedLineRE matches a `suspended = <bool>` assignment with
// optional indentation and trailing whitespace/comment. The boolean
// value is captured to preserve true/false on rewrite.
var suspendedLineRE = regexp.MustCompile(`^(\s*)suspended(\s*=\s*)(true|false)(\s*(?:#.*)?)$`)

// suspendedOnStartLineRE matches an existing `suspended_on_start = ...`
// assignment. Used to detect the both-fields-present case so the
// rewrite refuses to clobber an explicit suspended_on_start value.
var suspendedOnStartLineRE = regexp.MustCompile(`^\s*suspended_on_start\s*=\s*(true|false)\s*(?:#.*)?$`)

// sectionHeaderRE captures TOML section headers (both table and
// array-of-tables forms). The first submatch is the canonicalized
// name (no brackets or whitespace).
var sectionHeaderRE = regexp.MustCompile(`^\s*\[\[?\s*([^\]\s]+)\s*\]\]?\s*(?:#.*)?$`)

// renameLegacySuspendedInCityTOML rewrites the file at path, renaming
// every `suspended = X` line inside a `[workspace]` or `[[rig]]` /
// `[[rigs]]` section to `suspended_on_start = X`. Pre-existing
// comments, indentation, and unrelated content are preserved. Other
// section types (e.g. `[[agent]]`, `[[patches.agent]]`) are left
// alone — agent-scope migration is tracked in #2407.
//
// Returns the number of lines rewritten. Sections that already carry
// both `suspended` and `suspended_on_start` are skipped (no rewrite)
// so a fix can never collide an explicit user value.
func renameLegacySuspendedInCityTOML(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	out, changes := rewriteLegacySuspended(string(data))
	if changes == 0 {
		return 0, nil
	}
	return changes, writeFileAtomic(path, []byte(out), 0o644)
}

// rewriteLegacySuspended is the pure-string form of
// renameLegacySuspendedInCityTOML, kept package-internal so tests can
// exercise it without touching disk. Returns the rewritten document
// and the number of lines changed.
func rewriteLegacySuspended(src string) (string, int) {
	lines := strings.Split(src, "\n")

	// First pass: bucket each line into the section that contains it
	// and collect, per section, the legacy `suspended` line indices
	// plus whether a `suspended_on_start` line already exists.
	type section struct {
		kind            sectionKind
		hasOnStart      bool
		legacyLineIdxes []int
	}
	var sections []section
	cur := section{kind: sectionUnknown}
	for i, line := range lines {
		if m := sectionHeaderRE.FindStringSubmatch(line); m != nil {
			sections = append(sections, cur)
			isArray := strings.HasPrefix(strings.TrimSpace(line), "[[")
			cur = section{kind: classifySection(m[1], isArray)}
			continue
		}
		switch cur.kind {
		case sectionWorkspace, sectionRig:
			switch {
			case suspendedLineRE.MatchString(line):
				cur.legacyLineIdxes = append(cur.legacyLineIdxes, i)
			case suspendedOnStartLineRE.MatchString(line):
				cur.hasOnStart = true
			}
		}
	}
	sections = append(sections, cur)

	// Second pass: rewrite eligible legacy lines. A section with both
	// `suspended` and `suspended_on_start` is skipped — the explicit
	// suspended_on_start value wins by alias semantics at read time
	// anyway, and the user can drop the legacy line by hand.
	changes := 0
	for _, s := range sections {
		if s.kind != sectionWorkspace && s.kind != sectionRig {
			continue
		}
		if s.hasOnStart {
			continue
		}
		for _, idx := range s.legacyLineIdxes {
			lines[idx] = suspendedLineRE.ReplaceAllString(lines[idx], "${1}suspended_on_start${2}${3}${4}")
			changes++
		}
	}
	if changes == 0 {
		return src, 0
	}
	return strings.Join(lines, "\n"), changes
}

// sectionKind tags TOML sections so the rewriter only edits the
// scopes covered by this PR.
type sectionKind int

const (
	sectionUnknown sectionKind = iota
	sectionWorkspace
	sectionRig
	sectionOther
)

// classifySection maps a TOML section header to a sectionKind. Only
// `[workspace]` and `[[rig]]` / `[[rigs]]` are touchable. Subsections
// like `[workspace.foo]` and unrelated kinds (e.g. `[[agent]]`,
// `[[patches.agent]]`) fall into sectionOther.
func classifySection(name string, isArrayOfTables bool) sectionKind {
	name = strings.TrimSpace(name)
	switch {
	case !isArrayOfTables && name == "workspace":
		return sectionWorkspace
	case isArrayOfTables && (name == "rig" || name == "rigs"):
		return sectionRig
	default:
		return sectionOther
	}
}

// writeFileAtomic writes data to path via temp + rename so a crash
// mid-write can't leave a half-rewritten city.toml. Sibling-file path
// (no leading dot) so the write lands on the same filesystem as the
// target and rename is atomic.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".doctorfix.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
