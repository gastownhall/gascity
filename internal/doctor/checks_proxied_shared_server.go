package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ProxiedSharedServerCheck reports gc-owned proxied scopes exposed to bd's
// user-level shared-server mode.
//
// bd resolves dolt.shared-server through layered config: env, then the scope's
// .beads/config.yaml, then ~/.config/bd/config.yaml, then ~/.beads/config.yaml.
// With the mode on, bd roots the scope's proxy and Dolt child in
// ~/.beads/shared-server — one Dolt root for every workspace on the host, so two
// cities' `hq` stores become one database. gc pins the mode off for every bd
// process it spawns and in each gc-owned proxied scope's config.yaml (written by
// `gc init`, `gc rig add` and every `gc start`); this check reports where that
// pin is missing or contradicted, and whether the user-level config is the
// kind that makes it matter.
type ProxiedSharedServerCheck struct {
	cityPath   string
	scopeRoots []string
	// userMode reports the user-level shared-server setting bd would apply to
	// a scope with no opinion of its own, and where it came from. Injected so
	// tests never read the real home directory.
	userMode func() (bool, string)
}

// NewProxiedSharedServerCheck returns the check for the given gc-owned proxied
// scope roots, or nil when there are none (nothing changes for a direct or
// external city). The caller decides ownership: gc's ownership journal, a
// committed ownership handoff, or gc's canonical endpoint marker.
func NewProxiedSharedServerCheck(cityPath string, scopeRoots []string) *ProxiedSharedServerCheck {
	if len(scopeRoots) == 0 {
		return nil
	}
	return &ProxiedSharedServerCheck{
		cityPath:   cityPath,
		scopeRoots: append([]string(nil), scopeRoots...),
		userMode:   UserLevelBdSharedServerMode,
	}
}

// Name returns the check identifier.
func (c *ProxiedSharedServerCheck) Name() string { return "proxied-shared-server" }

// CanFix returns true: the repair is the same pin `gc start` writes.
func (c *ProxiedSharedServerCheck) CanFix() bool { return true }

// WarmupEligible returns false; this check is not part of the `gc start`
// warm-up scan (start writes the pin itself).
func (c *ProxiedSharedServerCheck) WarmupEligible() bool { return false }

func (c *ProxiedSharedServerCheck) configPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".beads", "config.yaml")
}

// Run classifies each scope's pin against the user-level setting.
func (c *ProxiedSharedServerCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	userOn, userSource := c.userMode()

	var boundOn, unpinned, unreadable []string
	pinned := 0
	for _, scopeRoot := range c.scopeRoots {
		label := proxiedScopeLabel(c.cityPath, scopeRoot)
		pin, err := contract.ReadSharedServerPin(fsys.OSFS{}, c.configPath(scopeRoot))
		switch {
		case err != nil:
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", label, err))
		case pin == contract.SharedServerPinnedOn:
			boundOn = append(boundOn, label)
		case pin == contract.SharedServerPinnedOff:
			pinned++
		default:
			unpinned = append(unpinned, label)
		}
	}

	const fixHint = "run `gc doctor --fix` (or `gc start`) to pin dolt.shared-server: false into each gc-owned proxied scope's .beads/config.yaml"
	switch {
	case len(boundOn) > 0:
		r.Status = StatusError
		r.Message = fmt.Sprintf("%d gc-owned proxied scope(s) are bound to bd's host-wide shared server (dolt.shared-server: true in the scope config): %s — their store lives in ~/.beads/shared-server, shared with every other workspace on the host",
			len(boundOn), strings.Join(boundOn, ", "))
		r.FixHint = fixHint + "; beads written while bound stay in the shared server's Dolt root and are not moved back"
	case userOn && len(unpinned) > 0:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%s enables bd's shared-server mode and %d gc-owned proxied scope(s) do not pin it off: %s — a bd process gc does not spawn (e.g. an agent running `bd` in its shell) would move the store into ~/.beads/shared-server",
			userSource, len(unpinned), strings.Join(unpinned, ", "))
		r.FixHint = fixHint
	case len(unreadable) > 0:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("could not read the shared-server pin of %d gc-owned proxied scope(s)", len(unreadable))
		r.FixHint = "inspect <scope>/.beads/config.yaml"
	default:
		r.Status = StatusOK
		switch {
		case userOn:
			r.Message = fmt.Sprintf("%s enables bd's shared-server mode; all %d gc-owned proxied scope(s) pin it off", userSource, pinned)
		case len(unpinned) > 0:
			r.Message = fmt.Sprintf("bd shared-server mode is off at user level; %d of %d gc-owned proxied scope(s) pin it off (gc start pins the rest)", pinned, len(c.scopeRoots))
		default:
			r.Message = fmt.Sprintf("%d gc-owned proxied scope(s) pin bd's shared-server mode off", pinned)
		}
	}
	r.Details = append(append(r.Details, boundOn...), unreadable...)
	return r
}

// Fix pins every gc-owned proxied scope off.
func (c *ProxiedSharedServerCheck) Fix(_ *CheckContext) error {
	var errs []string
	for _, scopeRoot := range c.scopeRoots {
		if _, _, err := contract.EnsureSharedServerDisabled(fsys.OSFS{}, c.configPath(scopeRoot)); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", proxiedScopeLabel(c.cityPath, scopeRoot), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pinning dolt.shared-server off: %s", strings.Join(errs, "; "))
	}
	return nil
}

// UserLevelBdSharedServerMode reports whether bd's user-level configuration
// turns shared-server mode on for a workspace with no opinion of its own, and
// names the source. It mirrors bd's precedence for this one key:
// BEADS_DOLT_SHARED_SERVER (only "1"/"true" force it on), then
// BD_DOLT_SHARED_SERVER, then ~/.config/bd/config.yaml (and the platform user
// config dir), then the legacy ~/.beads/config.yaml.
func UserLevelBdSharedServerMode() (bool, string) {
	if v := os.Getenv("BEADS_DOLT_SHARED_SERVER"); v == "1" || strings.EqualFold(v, "true") {
		return true, "BEADS_DOLT_SHARED_SERVER"
	}
	if v, ok := os.LookupEnv("BD_DOLT_SHARED_SERVER"); ok && v != "" {
		on, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && on, "BD_DOLT_SHARED_SERVER"
	}
	var paths []string
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "bd", "config.yaml"))
	}
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		paths = append(paths, filepath.Join(home, ".config", "bd", "config.yaml"), filepath.Join(home, ".beads", "config.yaml"))
	}
	for _, path := range paths {
		pin, err := contract.ReadSharedServerPin(fsys.OSFS{}, path)
		if err != nil || pin == contract.SharedServerUnset {
			continue
		}
		return pin == contract.SharedServerPinnedOn, path
	}
	return false, ""
}
