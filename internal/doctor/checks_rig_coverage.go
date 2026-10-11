package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
)

// RigPackCoverageCheck warns when a city-level pack declares rig-scoped
// always-mode named_sessions but no non-suspended rig includes that pack.
type RigPackCoverageCheck struct {
	cfg      *config.City
	cityPath string
}

// NewRigPackCoverageCheck creates a check for rig pack coverage.
func NewRigPackCoverageCheck(cfg *config.City, cityPath string) *RigPackCoverageCheck {
	return &RigPackCoverageCheck{cfg: cfg, cityPath: cityPath}
}

// Name returns the check identifier.
func (c *RigPackCoverageCheck) Name() string { return "rig-pack-coverage" }

// CanFix returns false — this requires pack/rig config changes.
func (c *RigPackCoverageCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigPackCoverageCheck) Fix(_ *CheckContext) error { return nil }

type partialPackForCoverage struct {
	Pack struct {
		Name string `toml:"name"`
	} `toml:"pack"`
	NamedSessions []config.NamedSession `toml:"named_session"`
}

// Run checks that every non-suspended rig imports the city packs which
// declare rig-scoped always-mode named_sessions. A warning fires when any
// active rig is missing such a pack, on the theory that a "mode=always"
// rig-scoped session is intended to be present on every rig that runs
// this kind of work — partial coverage is almost always a config error.
//
// Unreadable or malformed pack.toml files are surfaced as warnings
// alongside any coverage gaps, rather than silently skipped, so a
// misconfigured pack does not hide its own diagnostic.
func (c *RigPackCoverageCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	activeRigs := c.activeRigs()

	var issues []string
	for _, packDir := range c.cfg.PackDirs {
		sessions, err := rigAlwaysSessions(packDir)
		if err != nil {
			issues = append(issues, fmt.Sprintf(
				"pack %q: %v (unable to evaluate rig-scoped named_session coverage)",
				packDir, err))
			continue
		}
		if len(sessions) == 0 {
			continue
		}

		packName := sessions[0].packName
		if packName == "" {
			packName = filepath.Base(packDir)
		}

		if len(activeRigs) == 0 {
			for _, s := range sessions {
				issues = append(issues, fmt.Sprintf(
					"pack %q declares rig-scoped named_session %q (mode=always) but no non-suspended rigs exist",
					packName, s.template))
			}
			continue
		}

		var uncovered []string
		for _, rig := range activeRigs {
			if !c.rigCoversPack(rig.Name, packDir, packName, sessions) {
				uncovered = append(uncovered, rig.Name)
			}
		}
		if len(uncovered) == 0 {
			continue
		}

		for _, s := range sessions {
			if len(uncovered) == len(activeRigs) {
				issues = append(issues, fmt.Sprintf(
					"pack %q declares rig-scoped named_session %q (mode=always) but no rig imports this pack",
					packName, s.template))
			} else {
				issues = append(issues, fmt.Sprintf(
					"pack %q declares rig-scoped named_session %q (mode=always) — missing from rig(s): %s",
					packName, s.template, strings.Join(uncovered, ", ")))
			}
		}
	}

	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "all rig-scoped named_sessions covered by rig imports"
		return r
	}
	sort.Strings(issues)
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d rig-scoped named_session(s) not covered by rig imports", len(issues))
	r.Details = issues
	r.FixHint = "add [defaults.rig.imports.<pack>] to city.toml or add the pack to each rig's [imports]"
	return r
}

func (c *RigPackCoverageCheck) activeRigs() []config.Rig {
	var rigs []config.Rig
	for _, rig := range c.cfg.Rigs {
		if !rig.Suspended {
			rigs = append(rigs, rig)
		}
	}
	return rigs
}

type rigAlwaysSession struct {
	template string
	packName string
}

func rigAlwaysSessions(packDir string) ([]rigAlwaysSession, error) {
	tomlPath := filepath.Join(packDir, "pack.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		// A pack directory with no pack.toml at all is not the case
		// the check is designed to flag — those are caught by other
		// checks. Only return an error for "exists but unreadable".
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", tomlPath, err)
	}
	var pc partialPackForCoverage
	if _, err := toml.Decode(string(data), &pc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", tomlPath, err)
	}
	var sessions []rigAlwaysSession
	for _, ns := range pc.NamedSessions {
		if ns.Scope == "rig" && ns.ModeOrDefault() == "always" {
			sessions = append(sessions, rigAlwaysSession{
				template: ns.Template,
				packName: pc.Pack.Name,
			})
		}
	}
	return sessions, nil
}

// absOrClean returns filepath.Abs(p) when that succeeds, and falls back
// to filepath.Clean(p) otherwise so a transient os.Getwd failure cannot
// silently turn a real path into the empty string (which would never
// match a configured pack dir).
func absOrClean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

func rigHasPackDir(rigPackDirs map[string][]string, rigName, packDir string) bool {
	dirs := rigPackDirs[rigName]
	target := absOrClean(packDir)
	for _, d := range dirs {
		if absOrClean(d) == target {
			return true
		}
	}
	return false
}

// rigCoversPack reports whether rig rigName satisfies the always-session
// coverage that packDir/packName declares — either by importing packDir
// exactly, or by importing a rig-local pack with the same [pack].name
// that declares at least the same rig-scoped always named_session
// template(s). A rig-local replacement (a distinct namepool, local
// prompt/formula changes, and so on) is a deliberate override, not a
// coverage gap (#3907); exact-dir comparison alone can never recognize
// one, since a real replacement's whole point is to live at a different
// path than the pack it replaces.
func (c *RigPackCoverageCheck) rigCoversPack(rigName, packDir, packName string, want []rigAlwaysSession) bool {
	if rigHasPackDir(c.cfg.RigPackDirs, rigName, packDir) {
		return true
	}
	for _, dir := range c.cfg.RigPackDirs[rigName] {
		have, err := rigAlwaysSessions(dir)
		if err != nil || len(have) == 0 {
			continue
		}
		if have[0].packName != packName {
			continue
		}
		if alwaysSessionTemplatesCovered(want, have) {
			return true
		}
	}
	return false
}

// alwaysSessionTemplatesCovered reports whether have declares at least
// every template want requires. A same-named local replacement may add
// extra sessions of its own; it just needs to keep the ones the city
// pack's coverage check is tracking.
func alwaysSessionTemplatesCovered(want, have []rigAlwaysSession) bool {
	haveTemplates := make(map[string]bool, len(have))
	for _, s := range have {
		haveTemplates[s.template] = true
	}
	for _, s := range want {
		if !haveTemplates[s.template] {
			return false
		}
	}
	return true
}
