package doctor

import (
	"encoding/json"
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
	// classifyErrs are the scopes whose gc ownership could not be decided;
	// they are reported rather than silently dropped.
	classifyErrs []string
	// userMode reports the user-level shared-server setting bd would apply to
	// a scope with no opinion of its own, and where it came from. Injected so
	// tests never read the real home directory.
	userMode func() (bool, string)
	// sharedRoot is bd's shared-server directory (BEADS_SHARED_SERVER_DIR or
	// ~/.beads/shared-server). Injected for the same reason.
	sharedRoot func() string
}

// NewProxiedSharedServerCheck returns the check for the given gc-owned proxied
// scope roots, or nil when there are none and no classification failed
// (nothing changes for a direct or external city). The caller decides
// ownership: gc's ownership journal, a committed ownership handoff, or gc's
// canonical endpoint marker; classifyErrs are the scopes it could not decide.
func NewProxiedSharedServerCheck(cityPath string, scopeRoots []string, classifyErrs []error) *ProxiedSharedServerCheck {
	if len(scopeRoots) == 0 && len(classifyErrs) == 0 {
		return nil
	}
	c := &ProxiedSharedServerCheck{
		cityPath:   cityPath,
		scopeRoots: append([]string(nil), scopeRoots...),
		userMode:   UserLevelBdSharedServerMode,
		sharedRoot: BdSharedServerDir,
	}
	for _, err := range classifyErrs {
		c.classifyErrs = append(c.classifyErrs, err.Error())
	}
	return c
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

// materialized reports whether bd has created the scope's .beads directory. A
// journaled scope whose init has not run yet has nothing to pin or report.
func materialized(scopeRoot string) bool {
	info, err := os.Stat(filepath.Join(scopeRoot, ".beads"))
	return err == nil && info.IsDir()
}

// strandedSharedServerDatabase returns the shared-server database directory
// that carries this scope's Dolt database name, if one exists. A scope that
// was ever bound to bd's shared server wrote its rows there; flipping the pin
// off does not move them back.
func (c *ProxiedSharedServerCheck) strandedSharedServerDatabase(scopeRoot string) string {
	root := c.sharedRoot()
	if root == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "metadata.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if json.Unmarshal(data, &meta) != nil || strings.TrimSpace(meta.DoltDatabase) == "" {
		return ""
	}
	dir := filepath.Join(root, "dolt", strings.TrimSpace(meta.DoltDatabase))
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}

// Run classifies each scope's pin against the user-level setting.
func (c *ProxiedSharedServerCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	userOn, userSource := c.userMode()

	var boundOn, unpinned, stranded []string
	unreadable := append([]string(nil), c.classifyErrs...)
	pinned, scopes := 0, 0
	for _, scopeRoot := range c.scopeRoots {
		if !materialized(scopeRoot) {
			continue
		}
		scopes++
		label := proxiedScopeLabel(c.cityPath, scopeRoot)
		if dir := c.strandedSharedServerDatabase(scopeRoot); dir != "" {
			stranded = append(stranded, fmt.Sprintf("%s: %s", label, dir))
		}
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
	case len(stranded) > 0:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("bd's shared server holds a database named like %d gc-owned proxied scope(s) and may hold stranded beads: %s — if a scope was ever bound there (dolt.shared-server: true), beads it wrote then stay in the shared server and were not migrated back",
			len(stranded), strings.Join(stranded, ", "))
		r.FixHint = "inspect that database (another workspace on the host may legitimately share the name); gc does not move or delete it. Copy any stranded beads back into the scope, then remove or rename the shared-server database directory to clear this warning"
	case userOn && len(unpinned) > 0:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%s enables bd's shared-server mode and %d gc-owned proxied scope(s) do not pin it off: %s — a bd process gc does not spawn (e.g. an agent running `bd` in its shell) would move the store into ~/.beads/shared-server",
			userSource, len(unpinned), strings.Join(unpinned, ", "))
		r.FixHint = fixHint
	case len(unreadable) > 0:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("could not read or classify the shared-server pin of %d proxied scope(s)", len(unreadable))
		r.FixHint = "inspect <scope>/.beads/config.yaml"
	default:
		r.Status = StatusOK
		switch {
		case userOn:
			r.Message = fmt.Sprintf("%s enables bd's shared-server mode; all %d gc-owned proxied scope(s) pin it off", userSource, pinned)
		case len(unpinned) > 0:
			r.Message = fmt.Sprintf("bd shared-server mode is off at user level; %d of %d gc-owned proxied scope(s) pin it off (gc start pins the rest)", pinned, scopes)
		default:
			r.Message = fmt.Sprintf("%d gc-owned proxied scope(s) pin bd's shared-server mode off", pinned)
		}
	}
	r.Details = append(append(append(r.Details, boundOn...), stranded...), unreadable...)
	return r
}

// Fix pins every gc-owned proxied scope off.
func (c *ProxiedSharedServerCheck) Fix(_ *CheckContext) error {
	var errs []string
	for _, scopeRoot := range c.scopeRoots {
		if !materialized(scopeRoot) {
			continue
		}
		if _, _, err := contract.EnsureSharedServerDisabled(fsys.OSFS{}, c.configPath(scopeRoot)); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", proxiedScopeLabel(c.cityPath, scopeRoot), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pinning dolt.shared-server off: %s", strings.Join(errs, "; "))
	}
	return nil
}

// BdSharedServerDir returns bd's shared-server directory: BEADS_SHARED_SERVER_DIR
// when set, else ~/.beads/shared-server (bd doltserver.SharedServerDir).
func BdSharedServerDir() string {
	if dir := strings.TrimSpace(os.Getenv("BEADS_SHARED_SERVER_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".beads", "shared-server")
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
