package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/deps"
	"gopkg.in/yaml.v3"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltversion"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// --- Core checks ---

// CityStructureCheck verifies city.toml exists and reports legacy-only layouts.
type CityStructureCheck struct{}

// Name returns the check identifier.
func (c *CityStructureCheck) Name() string { return "city-structure" }

// Run checks that the city directory has the expected structure.
func (c *CityStructureCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	toml := filepath.Join(ctx.CityPath, "city.toml")

	if _, err := os.Stat(toml); err != nil {
		if citylayout.HasRuntimeRoot(ctx.CityPath) {
			r.Status = StatusWarning
			r.Message = "legacy .gc/ layout detected; city.toml missing"
			return r
		}
		r.Status = StatusError
		r.Message = "city.toml missing"
		return r
	}
	r.Status = StatusOK
	r.Message = "city.toml present"
	return r
}

// CanFix returns false — structure must be created by gc init.
func (c *CityStructureCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *CityStructureCheck) Fix(_ *CheckContext) error { return nil }

// CityConfigCheck verifies city.toml parses and an effective workspace name can
// be resolved.
type CityConfigCheck struct{}

// Name returns the check identifier.
func (c *CityConfigCheck) Name() string { return "city-config" }

// Run parses city.toml and checks effective workspace identity.
func (c *CityConfigCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(ctx.CityPath, "city.toml"))
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("city.toml parse error: %v", err)
		return r
	}
	if cfg.Workspace.Name == "" && cfg.ResolvedWorkspaceName == "" {
		r.Status = StatusError
		r.Message = "workspace.name not set (and could not derive from path)"
		return r
	}
	summary := fmt.Sprintf("city.toml loaded (%d agents, %d rigs); effective city name %q", len(cfg.Agents), len(cfg.Rigs), cfg.ResolvedWorkspaceName)
	r.Status = StatusOK
	r.Message = summary
	return r
}

// CanFix returns false.
func (c *CityConfigCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *CityConfigCheck) Fix(_ *CheckContext) error { return nil }

// ConfigValidCheck runs ValidateAgents and ValidateRigs.
type ConfigValidCheck struct {
	cfg *config.City
}

// NewConfigValidCheck creates a check that validates the parsed config.
func NewConfigValidCheck(cfg *config.City) *ConfigValidCheck {
	return &ConfigValidCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *ConfigValidCheck) Name() string { return "config-valid" }

// Run validates agents and rigs in the config.
func (c *ConfigValidCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if err := config.ValidateAgents(c.cfg.Agents); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("agent validation: %v", err)
		return r
	}
	if err := config.ValidateRigs(c.cfg.Rigs, config.EffectiveHQPrefix(c.cfg)); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig validation: %v", err)
		return r
	}
	if err := config.ValidateServices(c.cfg.Services); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("service validation: %v", err)
		return r
	}
	if err := workspacesvc.ValidateRuntimeSupport(c.cfg.Services); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("service validation: %v", err)
		return r
	}
	r.Status = StatusOK
	r.Message = "agents, rigs, and services valid"
	return r
}

// CanFix returns false.
func (c *ConfigValidCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ConfigValidCheck) Fix(_ *CheckContext) error { return nil }

// ConfigRefsCheck validates that file/directory paths referenced in agent
// config (prompt_template, session_setup_script, overlay_dir) actually exist,
// and that provider names reference defined providers.
type ConfigRefsCheck struct {
	cfg      *config.City
	cityPath string
}

// NewConfigRefsCheck creates a check for config reference validity.
func NewConfigRefsCheck(cfg *config.City, cityPath string) *ConfigRefsCheck {
	return &ConfigRefsCheck{cfg: cfg, cityPath: cityPath}
}

// Name returns the check identifier.
func (c *ConfigRefsCheck) Name() string { return "config-refs" }

// Run validates that referenced paths exist and provider names are defined.
func (c *ConfigRefsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	var issues []string

	for _, a := range c.cfg.Agents {
		qn := a.QualifiedName()
		if a.PromptTemplate != "" {
			path := resolveConfigRefPath(c.cityPath, a.PromptTemplate)
			if _, err := os.Stat(path); err != nil {
				issues = append(issues, fmt.Sprintf("agent %q: prompt_template %q not found", qn, path))
			}
		}
		if a.SessionSetupScript != "" {
			path := resolveConfigRefPath(c.cityPath, a.SessionSetupScript)
			if _, err := os.Stat(path); err != nil {
				issues = append(issues, fmt.Sprintf("agent %q: session_setup_script %q not found", qn, path))
			}
		}
		if a.OverlayDir != "" {
			path := resolveConfigRefPath(c.cityPath, a.OverlayDir)
			if fi, err := os.Stat(path); err != nil {
				issues = append(issues, fmt.Sprintf("agent %q: overlay_dir %q not found", qn, path))
			} else if !fi.IsDir() {
				issues = append(issues, fmt.Sprintf("agent %q: overlay_dir %q is not a directory", qn, path))
			}
		}
		if a.Provider != "" && len(c.cfg.Providers) > 0 {
			_, declared := c.cfg.Providers[a.Provider]
			_, builtin := config.BuiltinProviders()[a.Provider]
			if !declared && !builtin {
				issues = append(issues, fmt.Sprintf("agent %q: provider %q not defined in [providers]", qn, a.Provider))
			}
		}
	}

	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "all config references valid"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d config reference issue(s)", len(issues))
	r.Details = issues
	return r
}

// CanFix returns false — missing files must be created by the user.
func (c *ConfigRefsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ConfigRefsCheck) Fix(_ *CheckContext) error { return nil }

// resolveConfigRefPath resolves an agent config path reference against the
// city root. Schema=2 packs emit absolute paths; legacy [[agent]] tables
// use city-relative paths, so guard against double-rooting before joining.
func resolveConfigRefPath(cityPath, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cityPath, p)
}

// BuiltinPackFamilyCheck fails when a city overrides only one member of the
// builtin bd/dolt pack family. Mixed system/user families are unsupported.
type BuiltinPackFamilyCheck struct {
	cfg      *config.City
	cityPath string
}

// NewBuiltinPackFamilyCheck creates a check for builtin bd/dolt family
// overrides in non-system pack roots.
func NewBuiltinPackFamilyCheck(cfg *config.City, cityPath string) *BuiltinPackFamilyCheck {
	return &BuiltinPackFamilyCheck{cfg: cfg, cityPath: cityPath}
}

// Name returns the check identifier.
func (c *BuiltinPackFamilyCheck) Name() string { return "builtin-pack-family" }

// Run validates that bd/dolt overrides are all-or-nothing.
func (c *BuiltinPackFamilyCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	provider := c.cfg.Beads.Provider
	if v := os.Getenv("GC_BEADS"); v != "" {
		provider = v
	}
	if !providerUsesBDDoltStore(provider) {
		r.Status = StatusOK
		r.Message = "builtin bd/dolt pack family not required"
		return r
	}

	overrides := c.userBuiltinPackOverrides()
	switch len(overrides) {
	case 0:
		r.Status = StatusOK
		r.Message = "builtin bd/dolt pack family unmodified"
	case 2:
		r.Status = StatusOK
		r.Message = "user overrides full builtin bd/dolt pack family"
	default:
		r.Status = StatusError
		if overrides["bd"] {
			r.Message = "user overrides builtin pack \"bd\" without also providing \"dolt\""
		} else {
			r.Message = "user overrides builtin pack \"dolt\" without also providing \"bd\""
		}
		r.FixHint = "override both builtin packs together or remove the partial override"
	}
	return r
}

// CanFix returns false.
func (c *BuiltinPackFamilyCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *BuiltinPackFamilyCheck) Fix(_ *CheckContext) error { return nil }

func (c *BuiltinPackFamilyCheck) userBuiltinPackOverrides() map[string]bool {
	systemRoot := filepath.Clean(filepath.Join(c.cityPath, citylayout.SystemPacksRoot))
	seenDirs := make(map[string]bool)
	overrides := make(map[string]bool)

	for _, dir := range packDirsForCheck(c.cfg) {
		dir = filepath.Clean(dir)
		if seenDirs[dir] || isSubpath(systemRoot, dir) {
			continue
		}
		seenDirs[dir] = true
		switch readPackName(dir) {
		case "bd", "dolt":
			overrides[readPackName(dir)] = true
		}
	}

	return overrides
}

func packDirsForCheck(cfg *config.City) []string {
	dirs := append([]string{}, cfg.PackDirs...)
	for _, rigDirs := range cfg.RigPackDirs {
		dirs = append(dirs, rigDirs...)
	}
	return dirs
}

func isSubpath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func readPackName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		return ""
	}
	var pc struct {
		Pack struct {
			Name string `toml:"name"`
		} `toml:"pack"`
	}
	if _, err := toml.Decode(string(data), &pc); err != nil {
		return ""
	}
	return pc.Pack.Name
}

// --- Infrastructure checks ---

// LookPathFunc is the function used to find binaries. Defaults to exec.LookPath.
// Tests can override this.
type LookPathFunc func(file string) (string, error)

// BinaryCheck verifies a binary is on PATH and optionally checks its
// minimum version.
type BinaryCheck struct {
	binary     string
	skipMsg    string // non-empty means skip with OK + this message
	lookPath   LookPathFunc
	minVersion string                             // minimum required version (empty = no version check)
	getVersion func() (version string, err error) // returns installed version
	installURL string                             // install/upgrade hint URL
}

// NewBinaryCheck creates a check for the given binary (no version check).
// If skipMsg is non-empty, the check returns OK with that message (used when
// the binary is not needed due to env config like GC_BEADS=file).
func NewBinaryCheck(binary string, skipMsg string, lp LookPathFunc) *BinaryCheck {
	if lp == nil {
		lp = exec.LookPath
	}
	return &BinaryCheck{binary: binary, skipMsg: skipMsg, lookPath: lp}
}

// NewVersionedBinaryCheck creates a check that also verifies minimum version.
func NewVersionedBinaryCheck(binary, skipMsg string, lp LookPathFunc, minVersion string, getVersion func() (string, error), installURL string) *BinaryCheck {
	if lp == nil {
		lp = exec.LookPath
	}
	return &BinaryCheck{
		binary:     binary,
		skipMsg:    skipMsg,
		lookPath:   lp,
		minVersion: minVersion,
		getVersion: getVersion,
		installURL: installURL,
	}
}

// Name returns the check identifier.
func (c *BinaryCheck) Name() string { return c.binary + "-binary" }

// Run checks if the binary is on PATH and optionally verifies its version.
func (c *BinaryCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skipMsg != "" {
		r.Status = StatusOK
		r.Message = c.skipMsg
		return r
	}
	path, err := c.lookPath(c.binary)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("%s not found in PATH", c.binary)
		r.FixHint = fmt.Sprintf("install %s and ensure it's in PATH", c.binary)
		if c.installURL != "" {
			r.FixHint = fmt.Sprintf("install %s: %s", c.binary, c.installURL)
		}
		return r
	}

	// If no version check configured, just report found.
	if c.minVersion == "" || c.getVersion == nil {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("found %s", path)
		return r
	}

	// Check version.
	version, err := c.getVersion()
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("found %s but could not determine version", path)
		return r
	}

	if deps.CompareVersions(version, c.minVersion) < 0 {
		r.Status = StatusError
		r.Message = fmt.Sprintf("%s v%s is too old (minimum: %s)", c.binary, version, c.minVersion)
		hint := fmt.Sprintf("upgrade %s to %s+", c.binary, c.minVersion)
		if c.installURL != "" {
			hint = fmt.Sprintf("upgrade %s to %s+: %s", c.binary, c.minVersion, c.installURL)
		}
		r.FixHint = hint
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("found %s v%s (minimum: %s)", c.binary, version, c.minVersion)
	return r
}

// CanFix returns false.
func (c *BinaryCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *BinaryCheck) Fix(_ *CheckContext) error { return nil }

// --- Session checks (skipped when controller is running) ---

// AgentSessionsCheck verifies non-suspended agents have running sessions.
type AgentSessionsCheck struct {
	cfg             *config.City
	cityName        string
	sessionTemplate string
	sp              runtime.Provider
}

// NewAgentSessionsCheck creates a check for agent session liveness.
func NewAgentSessionsCheck(cfg *config.City, cityName, sessionTemplate string, sp runtime.Provider) *AgentSessionsCheck {
	return &AgentSessionsCheck{cfg: cfg, cityName: cityName, sessionTemplate: sessionTemplate, sp: sp}
}

// Name returns the check identifier.
func (c *AgentSessionsCheck) Name() string { return "agent-sessions" }

// Run checks that each non-suspended agent has a running session.
func (c *AgentSessionsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	var missing []string
	for _, a := range c.cfg.Agents {
		if a.Suspended {
			continue
		}
		sn := agent.SessionNameFor(c.cityName, a.QualifiedName(), c.sessionTemplate)
		if !c.sp.IsRunning(sn) {
			missing = append(missing, a.QualifiedName())
		}
	}
	if len(missing) == 0 {
		r.Status = StatusOK
		r.Message = "all agent sessions running"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d agent(s) without sessions", len(missing))
	r.Details = missing
	r.FixHint = "run gc start to reconcile sessions"
	return r
}

// CanFix returns false.
func (c *AgentSessionsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *AgentSessionsCheck) Fix(_ *CheckContext) error { return nil }

// ZombieSessionsCheck finds sessions that are alive but the agent process is dead.
type ZombieSessionsCheck struct {
	cfg             *config.City
	cityName        string
	sessionTemplate string
	sp              runtime.Provider
}

// NewZombieSessionsCheck creates a check for zombie sessions.
func NewZombieSessionsCheck(cfg *config.City, cityName, sessionTemplate string, sp runtime.Provider) *ZombieSessionsCheck {
	return &ZombieSessionsCheck{cfg: cfg, cityName: cityName, sessionTemplate: sessionTemplate, sp: sp}
}

// Name returns the check identifier.
func (c *ZombieSessionsCheck) Name() string { return "zombie-sessions" }

// Run checks for sessions where the session exists but the agent process is dead.
func (c *ZombieSessionsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	var zombies []string
	for _, a := range c.cfg.Agents {
		if a.Suspended || len(a.ProcessNames) == 0 {
			continue
		}
		sn := agent.SessionNameFor(c.cityName, a.QualifiedName(), c.sessionTemplate)
		if c.sp.IsRunning(sn) && !c.sp.ProcessAlive(sn, a.ProcessNames) {
			zombies = append(zombies, sn)
		}
	}
	if len(zombies) == 0 {
		r.Status = StatusOK
		r.Message = "no zombie sessions"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d zombie session(s)", len(zombies))
	r.Details = zombies
	return r
}

// CanFix returns true — zombie sessions can be killed.
func (c *ZombieSessionsCheck) CanFix() bool { return true }

// Fix kills all zombie sessions.
func (c *ZombieSessionsCheck) Fix(_ *CheckContext) error {
	for _, a := range c.cfg.Agents {
		if a.Suspended || len(a.ProcessNames) == 0 {
			continue
		}
		sn := agent.SessionNameFor(c.cityName, a.QualifiedName(), c.sessionTemplate)
		if c.sp.IsRunning(sn) && !c.sp.ProcessAlive(sn, a.ProcessNames) {
			if err := c.sp.Stop(sn); err != nil {
				return fmt.Errorf("killing zombie session %q: %w", sn, err)
			}
		}
	}
	return nil
}

// OrphanSessionsCheck finds sessions with the city prefix not in config.
type OrphanSessionsCheck struct {
	cfg             *config.City
	cityName        string
	sessionTemplate string
	sp              runtime.Provider
}

// NewOrphanSessionsCheck creates a check for orphaned sessions.
func NewOrphanSessionsCheck(cfg *config.City, cityName, sessionTemplate string, sp runtime.Provider) *OrphanSessionsCheck {
	return &OrphanSessionsCheck{cfg: cfg, cityName: cityName, sessionTemplate: sessionTemplate, sp: sp}
}

// Name returns the check identifier.
func (c *OrphanSessionsCheck) Name() string { return "orphan-sessions" }

// Run finds sessions with the city prefix that don't match any configured agent.
func (c *OrphanSessionsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	prefix := "" // per-city socket isolation: all sessions belong to this city
	running, err := c.sp.ListRunning(prefix)
	partialList := runtime.IsPartialListError(err)
	if err != nil && !partialList {
		r.Status = StatusError
		r.Message = fmt.Sprintf("listing sessions: %v", err)
		return r
	}

	// Build set of expected session names.
	expected := make(map[string]bool)
	for _, a := range c.cfg.Agents {
		sn := agent.SessionNameFor(c.cityName, a.QualifiedName(), c.sessionTemplate)
		expected[sn] = true
	}

	var orphans []string
	for _, s := range running {
		if !expected[s] {
			orphans = append(orphans, s)
		}
	}

	if len(orphans) == 0 {
		if partialList {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("listing sessions partially failed: %v", err)
			return r
		}
		r.Status = StatusOK
		r.Message = "no orphaned sessions"
		return r
	}
	r.Status = StatusWarning
	if partialList {
		r.Message = fmt.Sprintf("listing sessions partially failed: %v (%d visible orphaned session(s))", err, len(orphans))
	} else {
		r.Message = fmt.Sprintf("%d orphaned session(s)", len(orphans))
	}
	r.Details = orphans
	return r
}

// CanFix returns true — orphan sessions can be killed.
func (c *OrphanSessionsCheck) CanFix() bool { return true }

// Fix kills all orphaned sessions.
func (c *OrphanSessionsCheck) Fix(_ *CheckContext) error {
	prefix := "" // per-city socket isolation: all sessions belong to this city
	running, err := c.sp.ListRunning(prefix)
	if runtime.IsPartialListError(err) {
		return fmt.Errorf("listing sessions partially failed: %w", err)
	}
	if err != nil {
		return err
	}
	expected := make(map[string]bool)
	for _, a := range c.cfg.Agents {
		sn := agent.SessionNameFor(c.cityName, a.QualifiedName(), c.sessionTemplate)
		expected[sn] = true
	}
	for _, s := range running {
		if !expected[s] {
			if err := c.sp.Stop(s); err != nil {
				return fmt.Errorf("killing orphan session %q: %w", s, err)
			}
		}
	}
	return nil
}

// --- Data checks ---

// BeadsStoreCheck verifies the bead store opens and Ping succeeds.
type BeadsStoreCheck struct {
	cityPath string
	newStore func(cityPath string) (beads.Store, error)
}

// NewBeadsStoreCheck creates a check for the bead store.
// newStore is a factory that opens a store from the city path.
func NewBeadsStoreCheck(cityPath string, newStore func(string) (beads.Store, error)) *BeadsStoreCheck {
	return &BeadsStoreCheck{cityPath: cityPath, newStore: newStore}
}

// Name returns the check identifier.
func (c *BeadsStoreCheck) Name() string { return "beads-store" }

// Run opens the store and pings it to verify accessibility.
func (c *BeadsStoreCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	target, fixHint, active, err := validateBDStoreTarget(c.cityPath, c.cityPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("resolve dolt target: %v", err)
		if active {
			r.FixHint = fixHint
		}
		return r
	}
	if active {
		addr := net.JoinHostPort(target.Host, target.Port)
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			r.Status = StatusError
			r.Message = fmt.Sprintf("dolt server not reachable at %s", addr)
			r.FixHint = doltServerFixHint(target)
			return r
		}
		conn.Close() //nolint:errcheck // best-effort close
	}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("store open failed: %v", err)
		return r
	}
	if err := store.Ping(); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("store ping failed: %v", err)
		return r
	}
	r.Status = StatusOK
	r.Message = "store accessible"
	return r
}

// CanFix returns false.
func (c *BeadsStoreCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *BeadsStoreCheck) Fix(_ *CheckContext) error { return nil }

// BDSplitStoreCheck warns when legacy bd embedded/server store directories
// coexist and the inactive store still contains Dolt data.
type BDSplitStoreCheck struct {
	cityPath  string
	name      string
	scopePath string
}

// NewBDSplitStoreCheck creates a city-level split-store check.
func NewBDSplitStoreCheck(scopePath string) *BDSplitStoreCheck {
	return &BDSplitStoreCheck{cityPath: scopePath, name: "bd-split-store", scopePath: scopePath}
}

// NewRigBDSplitStoreCheck creates a rig-level split-store check.
func NewRigBDSplitStoreCheck(cityPath string, rig config.Rig) *BDSplitStoreCheck {
	return &BDSplitStoreCheck{cityPath: cityPath, name: "rig:" + rig.Name + ":bd-split-store", scopePath: rig.Path}
}

// Name returns the check identifier.
func (c *BDSplitStoreCheck) Name() string { return c.name }

// Run detects legacy split bd store directories and reports inactive Dolt repos.
func (c *BDSplitStoreCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	beadsDir := filepath.Join(c.scopePath, ".beads")
	serverDir := filepath.Join(beadsDir, "dolt")
	embeddedDir := filepath.Join(beadsDir, "embeddeddolt")

	serverExists := splitStoreDirExists(serverDir)
	embeddedExists := splitStoreDirExists(embeddedDir)
	if !serverExists || !embeddedExists {
		r.Status = StatusOK
		r.Message = "no legacy split store detected"
		return r
	}

	serverRepos, serverErr := doltReposUnder(serverDir)
	embeddedRepos, embeddedErr := doltReposUnder(embeddedDir)
	if serverErr != nil || embeddedErr != nil {
		r.Status = StatusWarning
		r.Message = "could not inspect legacy bd split store directories"
		r.FixHint = "inspect .beads/dolt and .beads/embeddeddolt manually before deleting either directory"
		if serverErr != nil {
			r.Details = append(r.Details, fmt.Sprintf("scan .beads/dolt: %v", serverErr))
		}
		if embeddedErr != nil {
			r.Details = append(r.Details, fmt.Sprintf("scan .beads/embeddeddolt: %v", embeddedErr))
		}
		return r
	}

	activeSource, activeStore := c.activeBDStore(beadsDir)
	if activeStore == "" {
		if len(serverRepos)+len(embeddedRepos) == 0 {
			r.Status = StatusOK
			r.Message = "legacy split store directories present but no Dolt repos found"
			return r
		}
		r.Status = StatusWarning
		r.Message = "legacy split store detected: both .beads/dolt and .beads/embeddeddolt contain or may contain data, but no active local store was identified"
		r.Details = splitStoreDetails("unknown", activeSource, serverRepos, embeddedRepos)
		r.FixHint = splitStoreFixHint("unknown")
		return r
	}

	inactiveStore := "embeddeddolt"
	inactiveRepos := embeddedRepos
	if activeStore == "embeddeddolt" {
		inactiveStore = "dolt"
		inactiveRepos = serverRepos
	}
	if len(inactiveRepos) == 0 {
		r.Status = StatusOK
		r.Message = "legacy split store directories present but inactive store is empty"
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("legacy split store detected: active .beads/%s (%s), inactive .beads/%s contains %d Dolt repo(s)", activeStore, activeSource, inactiveStore, len(inactiveRepos))
	r.Details = splitStoreDetails(activeStore, activeSource, serverRepos, embeddedRepos)
	r.FixHint = splitStoreFixHint(activeStore)
	return r
}

// CanFix returns false; reconciliation requires explicit user review.
func (c *BDSplitStoreCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *BDSplitStoreCheck) Fix(_ *CheckContext) error { return nil }

func (c *BDSplitStoreCheck) activeBDStore(beadsDir string) (string, string) {
	activeSource, activeStore := activeBDStoreFromMetadata(filepath.Join(beadsDir, "metadata.json"))
	if c.cityPath == "" {
		return activeSource, activeStore
	}
	if !scopeUsesBDDoltStore(c.cityPath, c.scopePath) {
		return activeSource, ""
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, c.cityPath, c.scopePath, "")
	if err != nil {
		if source, ok := c.rawNonLocalEndpointSource(); ok {
			return source, ""
		}
		if source, ok := c.rawCityNonLocalEndpointSource(); ok {
			return source, ""
		}
		return activeSource, activeStore
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative {
		if source, ok := c.rawCityNonLocalEndpointSource(); ok {
			return source, ""
		}
		return activeSource, activeStore
	}
	switch resolved.State.EndpointOrigin {
	case contract.EndpointOriginManagedCity:
		if sameDoctorScope(c.cityPath, c.scopePath) {
			return "canonical endpoint_origin=" + string(resolved.State.EndpointOrigin), "dolt"
		}
		return "canonical endpoint_origin=" + string(resolved.State.EndpointOrigin), ""
	case contract.EndpointOriginCityCanonical, contract.EndpointOriginExplicit, contract.EndpointOriginInheritedCity:
		return "canonical endpoint_origin=" + string(resolved.State.EndpointOrigin), ""
	default:
		return activeSource, activeStore
	}
}

func (c *BDSplitStoreCheck) rawNonLocalEndpointSource() (string, bool) {
	return rawNonLocalEndpointSource(c.scopePath)
}

func (c *BDSplitStoreCheck) rawCityNonLocalEndpointSource() (string, bool) {
	if sameDoctorScope(c.cityPath, c.scopePath) {
		return "", false
	}
	return rawNonLocalEndpointSource(c.cityPath)
}

func rawNonLocalEndpointSource(scopePath string) (string, bool) {
	cfg, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(scopePath, ".beads", "config.yaml"))
	if err != nil || !ok {
		return "", false
	}
	switch cfg.EndpointOrigin {
	case contract.EndpointOriginCityCanonical, contract.EndpointOriginExplicit, contract.EndpointOriginInheritedCity:
		return "canonical endpoint_origin=" + string(cfg.EndpointOrigin), true
	default:
		return "", false
	}
}

func activeBDStoreFromMetadata(path string) (string, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var meta struct {
		Database string `json:"database"`
		Backend  string `json:"backend"`
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", ""
	}
	mode := strings.ToLower(strings.TrimSpace(meta.DoltMode))
	database := strings.ToLower(strings.TrimSpace(meta.Database))
	backend := strings.ToLower(strings.TrimSpace(meta.Backend))
	declaresNonDoltStore := (database != "" && database != "dolt") || (backend != "" && backend != "dolt")
	declaresDoltStore := database == "dolt" || backend == "dolt"
	if mode != "" && declaresNonDoltStore && !declaresDoltStore {
		return mode, ""
	}
	switch mode {
	case "server":
		return "metadata.json dolt_mode=" + mode, "dolt"
	case "embedded", "local":
		return "metadata.json dolt_mode=" + mode, "embeddeddolt"
	default:
		return mode, ""
	}
}

func sameDoctorScope(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && filepath.Clean(resolvedA) == filepath.Clean(resolvedB)
}

func splitStoreDetails(activeStore, activeSource string, serverRepos, embeddedRepos []string) []string {
	activeLine := "active store: unknown"
	recoveryLine := "recovery: export from copies of the legacy stores, review with bd import --dry-run, then import into the current or intended active store"
	if activeStore != "unknown" && activeStore != "" {
		activeLine = fmt.Sprintf("active store: .beads/%s (%s)", activeStore, activeSource)
		recoveryLine = "recovery: export from a copy of the inactive store, review with bd import --dry-run, then import into the active store"
	}
	details := []string{
		activeLine,
		fmt.Sprintf(".beads/dolt repositories: %s", describeRepoList(serverRepos)),
		fmt.Sprintf(".beads/embeddeddolt repositories: %s", describeRepoList(embeddedRepos)),
		recoveryLine,
	}
	return details
}

func splitStoreFixHint(activeStore string) string {
	if activeStore == "" || activeStore == "unknown" {
		return "export from each legacy store into backup JSONL, review with bd import --dry-run, then import into the current or intended active store; keep both directories until reconciled"
	}
	return "export from the inactive store into a backup JSONL, review with bd import --dry-run, then import into the active store; keep both directories until reconciled"
}

func describeRepoList(repos []string) string {
	if len(repos) == 0 {
		return "(none)"
	}
	return strings.Join(repos, ", ")
}

func doltReposUnder(root string) ([]string, error) {
	var repos []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".dolt" {
			return filepath.SkipDir
		}
		if !splitStoreDirExists(filepath.Join(path, ".dolt")) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			rel = filepath.Base(path)
		}
		repos = append(repos, filepath.ToSlash(rel))
		return filepath.SkipDir
	})
	sort.Strings(repos)
	return repos, err
}

func splitStoreDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func validateBDStoreTarget(cityPath, scopeRoot string) (contract.DoltConnectionTarget, string, bool, error) {
	if !scopeUsesBDDoltStore(cityPath, scopeRoot) {
		return contract.DoltConnectionTarget{}, "", false, nil
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return contract.DoltConnectionTarget{}, "reconcile the canonical Dolt endpoint", true, err
	}
	if resolved.Kind == contract.ScopeConfigMissing {
		return contract.DoltConnectionTarget{}, "", false, nil
	}
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
	if err != nil {
		return contract.DoltConnectionTarget{}, fixHintForBDScopeResolution(cityPath, resolved), true, err
	}
	return target, "", true, nil
}

func fixHintForBDScopeResolution(cityPath string, resolved contract.ScopeConfigResolution) string {
	if resolved.Kind == contract.ScopeConfigAuthoritative {
		origin := resolved.State.EndpointOrigin
		if origin == contract.EndpointOriginInheritedCity {
			if cityState, ok, err := contract.ResolveAuthoritativeConfigState(fsys.OSFS{}, cityPath, cityPath, ""); err == nil && ok {
				origin = cityState.EndpointOrigin
			}
		}
		return doltServerFixHint(contract.DoltConnectionTarget{EndpointOrigin: origin})
	}
	return resolveDoltServerFixHint(fsys.OSFS{}, cityPath)
}

func providerUsesBDDoltStore(provider string) bool {
	provider = strings.TrimSpace(provider)
	if provider == "" || provider == "bd" {
		return true
	}
	if strings.HasPrefix(provider, "exec:") && doctorExecProviderBase(provider) == "gc-beads-bd" {
		return true
	}
	return false
}

func doctorExecProviderBase(provider string) string {
	script := strings.TrimSpace(strings.TrimPrefix(provider, "exec:"))
	return strings.TrimSuffix(filepath.Base(script), ".sh")
}

func effectiveDoctorBeadsProvider(cityPath string) string {
	if v := strings.TrimSpace(os.Getenv("GC_BEADS")); v != "" {
		return v
	}
	return configuredDoctorBeadsProvider(cityPath)
}

func configuredDoctorBeadsProvider(cityPath string) string {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return "bd"
	}
	return cfg.Beads.Provider
}

func scopeUsesBDDoltStore(cityPath, scopePath string) bool {
	resolvedScope := resolveDoctorScopePath(cityPath, scopePath)
	if explicit, ok := scopedDoctorBeadsProviderOverride(cityPath, resolvedScope); ok {
		return providerUsesBDDoltStore(explicit)
	}
	provider := effectiveDoctorBeadsProvider(cityPath)
	if strings.TrimSpace(os.Getenv("GC_BEADS_SCOPE_ROOT")) != "" {
		provider = configuredDoctorBeadsProvider(cityPath)
	}
	if sameDoctorScope(resolvedScope, cityPath) {
		return providerUsesBDDoltStore(provider)
	}
	if strings.HasPrefix(strings.TrimSpace(provider), "exec:") && !providerUsesBDDoltStore(provider) {
		return false
	}
	if doctorScopeHasBDMetadata(resolvedScope) {
		return true
	}
	if doctorScopeHasFileStoreMarker(resolvedScope) {
		return false
	}
	return providerUsesBDDoltStore(provider)
}

func scopedDoctorBeadsProviderOverride(cityPath, scopePath string) (string, bool) {
	provider := strings.TrimSpace(os.Getenv("GC_BEADS"))
	if provider == "" {
		return "", false
	}
	scopedRoot := strings.TrimSpace(os.Getenv("GC_BEADS_SCOPE_ROOT"))
	if scopedRoot == "" {
		return provider, true
	}
	if sameDoctorScope(resolveDoctorScopePath(cityPath, scopedRoot), scopePath) {
		return provider, true
	}
	return "", false
}

func resolveDoctorScopePath(cityPath, scopePath string) string {
	scopePath = strings.TrimSpace(scopePath)
	if scopePath == "" {
		scopePath = cityPath
	}
	if !filepath.IsAbs(scopePath) {
		scopePath = filepath.Join(cityPath, scopePath)
	}
	return filepath.Clean(scopePath)
}

func doctorScopeHasBDMetadata(scopePath string) bool {
	info, err := os.Stat(filepath.Join(scopePath, ".beads", "metadata.json"))
	return err == nil && !info.IsDir()
}

func doctorScopeHasFileStoreMarker(scopePath string) bool {
	if doctorScopeHasBDMetadata(scopePath) {
		return false
	}
	info, err := os.Stat(filepath.Join(scopePath, ".gc", "beads.json"))
	return err == nil && !info.IsDir()
}

// DoltServerCheck verifies the dolt server is running and reachable.
type DoltServerCheck struct {
	cityPath string
	skip     bool
}

// NewDoltServerCheck creates a check for the dolt server.
// If skip is true, the check returns OK (dolt not needed).
func NewDoltServerCheck(cityPath string, skip bool) *DoltServerCheck {
	return &DoltServerCheck{cityPath: cityPath, skip: skip}
}

// Name returns the check identifier.
func (c *DoltServerCheck) Name() string { return "dolt-server" }

// Run checks if the dolt server is running and reachable via TCP.
func (c *DoltServerCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip {
		r.Status = StatusOK
		r.Message = "skipped (file backend or GC_DOLT=skip)"
		return r
	}

	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, c.cityPath, c.cityPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("resolve dolt target: %v", err)
		r.FixHint = resolveDoltServerFixHint(fsys.OSFS{}, c.cityPath)
		return r
	}
	addr := net.JoinHostPort(target.Host, target.Port)

	// Check TCP reachability.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("dolt server not reachable at %s", addr)
		r.FixHint = doltServerFixHint(target)
		return r
	}
	conn.Close() //nolint:errcheck // best-effort close

	r.Status = StatusOK
	r.Message = fmt.Sprintf("reachable on %s", addr)
	return r
}

// CanFix returns false.
func (c *DoltServerCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltServerCheck) Fix(_ *CheckContext) error { return nil }

// RigDoltServerCheck verifies a rig-local explicit Dolt endpoint is reachable.
type RigDoltServerCheck struct {
	cityPath string
	rig      config.Rig
	skip     bool
}

// NewRigDoltServerCheck creates a check for an explicit rig Dolt endpoint.
func NewRigDoltServerCheck(cityPath string, rig config.Rig, skip bool) *RigDoltServerCheck {
	return &RigDoltServerCheck{cityPath: cityPath, rig: rig, skip: skip}
}

// Name returns the check identifier.
func (c *RigDoltServerCheck) Name() string { return "rig:" + c.rig.Name + ":dolt-server" }

// Run checks if an explicit rig Dolt endpoint is reachable. Inherited rigs are
// handled by the city-level DoltServerCheck and therefore skip here.
func (c *RigDoltServerCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip {
		r.Status = StatusOK
		r.Message = "skipped (file backend or GC_DOLT=skip)"
		return r
	}
	rigPath := c.rig.Path
	if !filepath.IsAbs(rigPath) {
		rigPath = filepath.Join(c.cityPath, rigPath)
	}
	if err := contract.ValidateInheritedCityEndpointMirror(fsys.OSFS{}, c.cityPath, rigPath); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("inherited city endpoint drift: %v", err)
		r.FixHint = "reconcile the inherited city endpoint mirror"
		return r
	}
	explicit, err := contract.ScopeUsesExplicitEndpoint(fsys.OSFS{}, c.cityPath, rigPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("resolve dolt target: %v", err)
		r.FixHint = "reconcile the canonical external Dolt endpoint"
		return r
	}
	if !explicit {
		r.Status = StatusOK
		r.Message = "inherits city dolt endpoint"
		return r
	}
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, c.cityPath, rigPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("resolve dolt target: %v", err)
		r.FixHint = "reconcile the canonical external Dolt endpoint"
		return r
	}
	addr := net.JoinHostPort(target.Host, target.Port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("dolt server not reachable at %s", addr)
		r.FixHint = doltServerFixHint(target)
		return r
	}
	conn.Close() //nolint:errcheck // best-effort close

	r.Status = StatusOK
	r.Message = fmt.Sprintf("reachable on %s", addr)
	return r
}

// CanFix returns false.
func (c *RigDoltServerCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigDoltServerCheck) Fix(_ *CheckContext) error { return nil }

func resolveDoltServerFixHint(fs fsys.FS, cityPath string) string {
	state, ok, err := contract.ResolveAuthoritativeConfigState(fs, cityPath, cityPath, "")
	if err != nil || !ok {
		return "reconcile the canonical Dolt endpoint"
	}
	return doltServerFixHint(contract.DoltConnectionTarget{EndpointOrigin: state.EndpointOrigin})
}

func doltServerFixHint(target contract.DoltConnectionTarget) string {
	switch target.EndpointOrigin {
	case contract.EndpointOriginManagedCity:
		return "run gc start to start the dolt server"
	case contract.EndpointOriginCityCanonical, contract.EndpointOriginExplicit, contract.EndpointOriginInheritedCity:
		return "reconcile the canonical external Dolt endpoint"
	default:
		return "reconcile the canonical Dolt endpoint"
	}
}

// EventsLogCheck verifies .gc/events.jsonl exists and is writable.
type EventsLogCheck struct{}

// Name returns the check identifier.
func (c *EventsLogCheck) Name() string { return "events-log" }

// Run checks the events log file.
func (c *EventsLogCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	path := filepath.Join(ctx.CityPath, ".gc", "events.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		r.Status = StatusWarning
		r.Message = "events.jsonl not found (events will not be logged)"
		return r
	}
	// Check writable by opening for append.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, fi.Mode())
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("events.jsonl not writable: %v", err)
		return r
	}
	f.Close() //nolint:errcheck // best-effort close
	r.Status = StatusOK
	r.Message = "events.jsonl exists and writable"
	return r
}

// CanFix returns false.
func (c *EventsLogCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *EventsLogCheck) Fix(_ *CheckContext) error { return nil }

// --- Controller check (informational) ---

// ControllerCheck reports whether the controller is running.
type ControllerCheck struct {
	cityPath string
	running  bool // pre-computed by caller
}

// NewControllerCheck creates an informational controller status check.
func NewControllerCheck(cityPath string, running bool) *ControllerCheck {
	return &ControllerCheck{cityPath: cityPath, running: running}
}

// Name returns the check identifier.
func (c *ControllerCheck) Name() string { return "controller" }

// Run reports controller status. Always returns OK — both states are valid.
func (c *ControllerCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Status: StatusOK}
	if c.running {
		r.Message = "controller running (sessions managed)"
	} else {
		r.Message = "controller not running (one-shot mode)"
	}
	return r
}

// CanFix returns false.
func (c *ControllerCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ControllerCheck) Fix(_ *CheckContext) error { return nil }

// --- Per-rig checks ---

// RigPathCheck verifies a rig's path exists and is a directory.
type RigPathCheck struct {
	rig config.Rig
}

// NewRigPathCheck creates a rig path existence check.
func NewRigPathCheck(rig config.Rig) *RigPathCheck {
	return &RigPathCheck{rig: rig}
}

// Name returns the check identifier.
func (c *RigPathCheck) Name() string { return "rig:" + c.rig.Name + ":path" }

// Run checks the rig path exists and is a directory.
func (c *RigPathCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	fi, err := os.Stat(c.rig.Path)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("path %q not found", c.rig.Path)
		return r
	}
	if !fi.IsDir() {
		r.Status = StatusError
		r.Message = fmt.Sprintf("path %q is not a directory", c.rig.Path)
		return r
	}
	r.Status = StatusOK
	r.Message = fmt.Sprintf("path %q exists", c.rig.Path)
	return r
}

// CanFix returns false.
func (c *RigPathCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigPathCheck) Fix(_ *CheckContext) error { return nil }

// RigGitCheck verifies a rig's path is a git repository. Non-git is a warning, not error.
type RigGitCheck struct {
	rig config.Rig
}

// NewRigGitCheck creates a rig git repo check.
func NewRigGitCheck(rig config.Rig) *RigGitCheck {
	return &RigGitCheck{rig: rig}
}

// Name returns the check identifier.
func (c *RigGitCheck) Name() string { return "rig:" + c.rig.Name + ":git" }

// Run checks if the rig path is a git repository.
func (c *RigGitCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	gitDir := filepath.Join(c.rig.Path, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		r.Status = StatusWarning
		r.Message = "not a git repository"
		return r
	}
	r.Status = StatusOK
	r.Message = "git repository"
	return r
}

// CanFix returns false.
func (c *RigGitCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigGitCheck) Fix(_ *CheckContext) error { return nil }

// RigBeadsCheck verifies a rig's beads store is accessible.
type RigBeadsCheck struct {
	cityPath string
	rig      config.Rig
	newStore func(rigPath string) (beads.Store, error)
}

// NewRigBeadsCheck creates a rig beads store accessibility check.
func NewRigBeadsCheck(cityPath string, rig config.Rig, newStore func(string) (beads.Store, error)) *RigBeadsCheck {
	return &RigBeadsCheck{cityPath: cityPath, rig: rig, newStore: newStore}
}

// Name returns the check identifier.
func (c *RigBeadsCheck) Name() string { return "rig:" + c.rig.Name + ":beads" }

// Run opens the rig's bead store and pings it to verify accessibility.
func (c *RigBeadsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	rigPath := c.rig.Path
	if !filepath.IsAbs(rigPath) {
		rigPath = filepath.Join(c.cityPath, rigPath)
	}
	target, fixHint, active, err := validateBDStoreTarget(c.cityPath, rigPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("resolve dolt target: %v", err)
		if active {
			r.FixHint = fixHint
		}
		return r
	}
	if active {
		addr := net.JoinHostPort(target.Host, target.Port)
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			r.Status = StatusError
			r.Message = fmt.Sprintf("dolt server not reachable at %s", addr)
			r.FixHint = doltServerFixHint(target)
			return r
		}
		conn.Close() //nolint:errcheck // best-effort close
	}
	store, err := c.newStore(rigPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("store open failed: %v", err)
		return r
	}
	if err := store.Ping(); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("store ping failed: %v", err)
		return r
	}
	r.Status = StatusOK
	r.Message = "store accessible"
	return r
}

// CanFix returns false.
func (c *RigBeadsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigBeadsCheck) Fix(_ *CheckContext) error { return nil }

// --- Pack cache checks ---

// PackCacheCheck verifies all remote pack caches are present.
type PackCacheCheck struct {
	packs    map[string]config.PackSource
	cityPath string
}

// NewPackCacheCheck creates a check for pack cache completeness.
func NewPackCacheCheck(packs map[string]config.PackSource, cityPath string) *PackCacheCheck {
	return &PackCacheCheck{packs: packs, cityPath: cityPath}
}

// Name returns the check identifier.
func (c *PackCacheCheck) Name() string { return "pack-cache" }

// Run checks that each configured pack has a cached pack.toml.
func (c *PackCacheCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	var missing []string
	for name, src := range c.packs {
		cachePath := config.PackCachePath(c.cityPath, name, src)
		topoFile := filepath.Join(cachePath, "pack.toml")
		if _, err := os.Stat(topoFile); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("all %d pack cache(s) present", len(c.packs))
		return r
	}
	r.Status = StatusError
	r.Message = fmt.Sprintf("%d pack cache(s) missing", len(missing))
	r.Details = missing
	r.FixHint = "run gc pack fetch"
	return r
}

// CanFix returns false — use gc pack fetch to populate caches.
func (c *PackCacheCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *PackCacheCheck) Fix(_ *CheckContext) error { return nil }

// --- Worktree checks ---

// WorktreeCheck verifies that worktree .git file pointers are valid.
// A worktree's .git file contains "gitdir: /path/to/.git/worktrees/name".
// If the target doesn't exist, the worktree is broken.
type WorktreeCheck struct {
	broken []string // populated by Run for Fix to use
}

// Name returns the check identifier.
func (c *WorktreeCheck) Name() string { return "worktrees" }

// Run walks .gc/worktrees/ and verifies each .git pointer.
func (c *WorktreeCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	c.broken = nil

	wtRoot := filepath.Join(ctx.CityPath, ".gc", "worktrees")
	rigEntries, err := os.ReadDir(wtRoot)
	if err != nil {
		if os.IsNotExist(err) {
			r.Status = StatusOK
			r.Message = "no worktrees directory"
			return r
		}
		r.Status = StatusError
		r.Message = fmt.Sprintf("reading worktrees dir: %v", err)
		return r
	}

	var total int
	for _, rigEntry := range rigEntries {
		if !rigEntry.IsDir() {
			continue
		}
		agentEntries, err := os.ReadDir(filepath.Join(wtRoot, rigEntry.Name()))
		if err != nil {
			continue
		}
		for _, agentEntry := range agentEntries {
			if !agentEntry.IsDir() {
				continue
			}
			total++
			wtPath := filepath.Join(wtRoot, rigEntry.Name(), agentEntry.Name())
			if !isWorktreeValid(wtPath) {
				c.broken = append(c.broken, wtPath)
			}
		}
	}

	if len(c.broken) == 0 {
		r.Status = StatusOK
		if total == 0 {
			r.Message = "no worktrees"
		} else {
			r.Message = fmt.Sprintf("all %d worktree(s) valid", total)
		}
		return r
	}

	r.Status = StatusError
	r.Message = fmt.Sprintf("%d broken worktree(s)", len(c.broken))
	r.Details = c.broken
	r.FixHint = "run gc doctor --fix to remove broken worktrees"
	return r
}

// CanFix returns true — broken worktrees can be removed.
func (c *WorktreeCheck) CanFix() bool { return true }

// Fix removes broken worktree directories found by the last Run.
func (c *WorktreeCheck) Fix(_ *CheckContext) error {
	for _, wtPath := range c.broken {
		if err := os.RemoveAll(wtPath); err != nil {
			return fmt.Errorf("removing broken worktree %s: %w", wtPath, err)
		}
	}
	return nil
}

// isWorktreeValid reads a worktree's .git file and checks whether the
// gitdir target exists. Returns true if no .git file exists (not a
// worktree) or if the target is valid.
func isWorktreeValid(wtPath string) bool {
	gitFile := filepath.Join(wtPath, ".git")
	data, err := os.ReadFile(gitFile)
	if err != nil {
		// No .git file — not a git worktree, skip.
		return true
	}
	content := strings.TrimSpace(string(data))
	if !strings.HasPrefix(content, "gitdir: ") {
		// Not a worktree .git file — skip.
		return true
	}
	target := strings.TrimPrefix(content, "gitdir: ")
	_, err = os.Stat(target)
	return err == nil
}

// --- Managed Dolt ops checks (PR 3) ---

// Thresholds for the managed Dolt data directory footprint (bytes).
const (
	doltNomsWarnBytes  = int64(2) * 1024 * 1024 * 1024  // 2 GB
	doltNomsErrorBytes = int64(20) * 1024 * 1024 * 1024 // 20 GB
)

var doltVersionCommandTimeout = 10 * time.Second

const doltDirMeasureTimeout = 60 * time.Second

// resolveManagedDoltDataDir returns the effective Dolt data directory for the
// managed provider. Doctor resolves the inspected city from disk, not ambient
// GC_DOLT_* shell overrides that may point at a different city.
func resolveManagedDoltDataDir(cityPath string) string {
	if dataDir := publishedManagedDoltDataDir(cityPath); dataDir != "" {
		return dataDir
	}
	beadsDataDir := filepath.Join(cityPath, ".beads", "dolt")
	if info, err := os.Stat(beadsDataDir); err == nil && info.IsDir() {
		return beadsDataDir
	}

	packDataDir := filepath.Join(doctorDoltPackStateDir(cityPath), "dolt-data")
	legacyDataDir := filepath.Join(cityPath, ".gc", "dolt-data")
	if info, err := os.Stat(legacyDataDir); err == nil && info.IsDir() {
		return legacyDataDir
	}
	if info, err := os.Stat(doctorDoltPackStateDir(cityPath)); err == nil && info.IsDir() {
		return packDataDir
	}
	return packDataDir
}

// resolveManagedDoltConfigPath returns the effective path to the managed
// dolt-config.yaml for the inspected city, ignoring ambient GC_DOLT_* shell
// overrides that may point at a different city.
func resolveManagedDoltConfigPath(cityPath string) string {
	return filepath.Join(doctorDoltPackStateDir(cityPath), "dolt-config.yaml")
}

type managedDoltDoctorRuntimeState struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	DataDir string `json:"data_dir"`
}

func publishedManagedDoltDataDir(cityPath string) string {
	stateFile := filepath.Join(doctorDoltPackStateDir(cityPath), "dolt-state.json")
	data, err := os.ReadFile(stateFile) //nolint:gosec // path is derived from managed city layout
	if err != nil {
		return ""
	}
	var state managedDoltDoctorRuntimeState
	if json.Unmarshal(data, &state) != nil {
		return ""
	}
	dataDir := strings.TrimSpace(state.DataDir)
	if dataDir == "" {
		return ""
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		return ""
	}
	if state.Running && !validPublishedManagedDoltDoctorState(cityPath, state, dataDir) {
		return ""
	}
	if !state.Running && managedDoltDoctorDefaultDataDirExists(cityPath, dataDir) {
		return ""
	}
	return dataDir
}

func doctorDoltPackStateDir(cityPath string) string {
	return filepath.Join(doctorCityRuntimeDir(cityPath), "packs", "dolt")
}

func doctorCityRuntimeDir(cityPath string) string {
	runtimeDir := strings.TrimSpace(os.Getenv("GC_CITY_RUNTIME_DIR"))
	if runtimeDir != "" {
		for _, key := range []string{"GC_CITY_PATH", "GC_CITY"} {
			if sameDoctorScope(strings.TrimSpace(os.Getenv(key)), cityPath) {
				return filepath.Clean(runtimeDir)
			}
		}
	}
	return citylayout.RuntimeDataDir(cityPath)
}

func validPublishedManagedDoltDoctorState(cityPath string, state managedDoltDoctorRuntimeState, dataDir string) bool {
	if state.PID <= 0 || state.Port <= 0 {
		return false
	}
	if !pidutil.Alive(state.PID) {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(state.Port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	holderPID := managedDoltDoctorPortHolderPID(state.Port)
	if holderPID > 0 {
		return holderPID == state.PID
	}
	return managedDoltDoctorProcessOwnsRuntime(state.PID, dataDir, resolveManagedDoltConfigPath(cityPath))
}

func managedDoltDoctorProcessOwnsRuntime(pid int, dataDir, configPath string) bool {
	cmdline := managedDoltDoctorProcCmdline(pid)
	if cmdline != "" {
		if strings.Contains(cmdline, dataDir) || strings.Contains(cmdline, configPath) {
			return true
		}
	}
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err == nil && sameDoctorScope(cwd, dataDir) {
		return true
	}
	return false
}

func managedDoltDoctorProcCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err == nil {
		return strings.ReplaceAll(string(data), "\x00", " ")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func managedDoltDoctorPortHolderPID(port int) int {
	if port <= 0 {
		return 0
	}
	if pid, checked := managedDoltDoctorPortHolderFromProc(uint16(port)); checked {
		return pid
	}
	return managedDoltDoctorPortHolderFromLsof(port)
}

func managedDoltDoctorPortHolderFromProc(port uint16) (int, bool) {
	inodes := map[string]struct{}{}
	checked := false
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		checked = true
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			gotPort, err := strconv.ParseUint(portHex, 16, 16)
			if err != nil || uint16(gotPort) != port {
				continue
			}
			inodes[fields[9]] = struct{}{}
		}
	}
	if !checked {
		return 0, false
	}
	if len(inodes) == 0 {
		return 0, true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, true
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !pidutil.Alive(pid) {
			continue
		}
		fdDir := filepath.Join("/proc", entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if _, ok := inodes[inode]; ok {
				return pid, true
			}
		}
	}
	return 0, true
}

func managedDoltDoctorPortHolderFromLsof(port int) int {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return 0
	}
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err == nil && pidutil.Alive(pid) {
			return pid
		}
	}
	return 0
}

func managedDoltDoctorDefaultDataDirExists(cityPath, dataDir string) bool {
	for _, candidate := range []string{
		filepath.Join(cityPath, ".beads", "dolt"),
		filepath.Join(cityPath, ".gc", "dolt-data"),
	} {
		if sameDoctorScope(candidate, dataDir) {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

type doltDataScanTarget struct {
	Database string
	ScanRoot string
	Orphan   bool
}

func isManagedDoltSystemDatabase(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "information_schema", "mysql", "dolt_cluster", "__gc_probe":
		return true
	default:
		return false
	}
}

func isManagedDoltUserDatabase(name string) bool {
	name = strings.TrimSpace(name)
	if isManagedDoltSystemDatabase(name) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		valid := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
		if i > 0 {
			valid = valid || c == '-'
		}
		if !valid {
			return false
		}
	}
	return true
}

func appendDoltDataScanTarget(targets *[]doltDataScanTarget, seenRoots map[string]struct{}, db, scanRoot string, orphan bool) {
	if _, seen := seenRoots[scanRoot]; seen {
		return
	}
	seenRoots[scanRoot] = struct{}{}
	*targets = append(*targets, doltDataScanTarget{
		Database: db,
		ScanRoot: scanRoot,
		Orphan:   orphan,
	})
}

func managedDoltScopeRootsFromConfig(cityPath string, cfg *config.City) []string {
	scopeRoots := []string{cityPath}
	if cfg == nil {
		return scopeRoots
	}
	for _, rig := range cfg.Rigs {
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			continue
		}
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		scopeRoots = append(scopeRoots, rigPath)
	}
	return scopeRoots
}

func managedDoltScopeRootsForConfig(cityPath string, cfg *config.City, cfgErr error) []string {
	if cfgErr != nil {
		return managedDoltScopeRootsFromFilesystem(cityPath)
	}
	return managedDoltScopeRootsFromConfig(cityPath, cfg)
}

func managedDoltScopeRootsFromFilesystem(cityPath string) []string {
	scopeRoots := []string{cityPath}
	seen := map[string]struct{}{filepath.Clean(cityPath): {}}
	err := filepath.WalkDir(cityPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", ".gc":
				return fs.SkipDir
			}
			return nil
		}
		if name != "metadata.json" || filepath.Base(filepath.Dir(path)) != ".beads" {
			return nil
		}
		scopeRoot := filepath.Clean(filepath.Dir(filepath.Dir(path)))
		if _, ok := seen[scopeRoot]; ok {
			return nil
		}
		seen[scopeRoot] = struct{}{}
		scopeRoots = append(scopeRoots, scopeRoot)
		return nil
	})
	if err != nil {
		return []string{cityPath}
	}
	return scopeRoots
}

func managedDoltScopeRoots(cityPath string) []string {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return managedDoltScopeRootsFromFilesystem(cityPath)
	}
	return managedDoltScopeRootsFromConfig(cityPath, cfg)
}

// managedLocalDoltScanTargets returns distinct local Dolt databases referenced
// by the workspace plus orphaned on-disk databases under the managed data dir.
// External targets are skipped because their disk footprint is not local state.
func managedLocalDoltScanTargets(cityPath string) ([]doltDataScanTarget, bool) {
	return managedLocalDoltScanTargetsForScopeRoots(cityPath, managedDoltScopeRoots(cityPath))
}

func managedLocalDoltScanTargetsForScopeRoots(cityPath string, scopeRoots []string) ([]doltDataScanTarget, bool) {
	dataDir := resolveManagedDoltDataDir(cityPath)
	seenRoots := map[string]struct{}{}
	targets := make([]doltDataScanTarget, 0, len(scopeRoots))
	unresolved := false
	for _, scopeRoot := range scopeRoots {
		target, _, active, err := validateBDStoreTarget(cityPath, scopeRoot)
		if err == nil {
			if !active || target.External {
				continue
			}
			db := strings.TrimSpace(target.Database)
			if !isManagedDoltUserDatabase(db) {
				continue
			}
			scanRoot := dataDir
			if db != "" {
				scanRoot = filepath.Join(dataDir, db, ".dolt")
			}
			appendDoltDataScanTarget(&targets, seenRoots, db, scanRoot, false)
			continue
		}
		if !active {
			continue
		}

		resolved, resolveErr := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
		if resolveErr != nil || resolved.Kind != contract.ScopeConfigAuthoritative {
			unresolved = true
			continue
		}
		switch resolved.State.EndpointOrigin {
		case contract.EndpointOriginManagedCity, contract.EndpointOriginInheritedCity:
		default:
			continue
		}
		db, ok, dbErr := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
		if dbErr != nil {
			unresolved = true
			continue
		}
		if !ok {
			db = "beads"
		}
		db = strings.TrimSpace(db)
		if !isManagedDoltUserDatabase(db) {
			continue
		}
		scanRoot := dataDir
		if db != "" {
			scanRoot = filepath.Join(dataDir, db, ".dolt")
		}
		appendDoltDataScanTarget(&targets, seenRoots, db, scanRoot, false)
	}

	if entries, err := os.ReadDir(dataDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			db := entry.Name()
			if isManagedDoltSystemDatabase(db) {
				continue
			}
			scanRoot := filepath.Join(dataDir, db, ".dolt")
			if info, statErr := os.Stat(scanRoot); statErr != nil || !info.IsDir() {
				continue
			}
			appendDoltDataScanTarget(&targets, seenRoots, db, scanRoot, true)
		}
	} else if !os.IsNotExist(err) {
		unresolved = true
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Database == targets[j].Database {
			return targets[i].ScanRoot < targets[j].ScanRoot
		}
		return targets[i].Database < targets[j].Database
	})
	return targets, unresolved
}

func workspaceHasLocalManagedDoltTarget(cityPath string) bool {
	targets, _ := managedLocalDoltScanTargets(cityPath)
	return len(targets) > 0
}

// ManagedLocalDoltChecksApplicableForConfig reports whether managed-local Dolt
// doctor checks apply, using an already-loaded city config when available.
func ManagedLocalDoltChecksApplicableForConfig(cityPath string, cfg *config.City, cfgErr error) bool {
	return managedLocalDoltChecksApplicableForScopeRoots(cityPath, managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr), cfgErr != nil)
}

// ManagedLocalDoltChecksApplicable reports whether managed-local Dolt doctor
// checks apply for a city path.
func ManagedLocalDoltChecksApplicable(cityPath string) bool {
	return managedLocalDoltChecksApplicable(cityPath)
}

func managedLocalDoltChecksApplicable(cityPath string) bool {
	if strings.TrimSpace(cityPath) == "" {
		return true
	}

	cityConfigPath := filepath.Join(cityPath, "city.toml")
	cityHasConfig := false
	if info, err := os.Stat(cityConfigPath); err == nil && !info.IsDir() {
		cityHasConfig = true
	}
	if cityHasConfig {
		cfg, err := config.Load(fsys.OSFS{}, cityConfigPath)
		if err != nil {
			return managedLocalDoltChecksApplicableForScopeRoots(cityPath, managedDoltScopeRootsFromFilesystem(cityPath), true)
		}
		return managedLocalDoltChecksApplicableForScopeRoots(cityPath, managedDoltScopeRootsFromConfig(cityPath, cfg), false)
	}

	return managedLocalDoltChecksApplicableForScopeRoots(cityPath, managedDoltScopeRootsFromConfig(cityPath, nil), false)
}

func managedLocalDoltChecksApplicableForScopeRoots(cityPath string, scopeRoots []string, configLoadErr bool) bool {
	if strings.TrimSpace(cityPath) == "" {
		return true
	}
	cityHasConfig := false
	if info, err := os.Stat(filepath.Join(cityPath, "city.toml")); err == nil && !info.IsDir() {
		cityHasConfig = true
	}

	seenScopes := map[string]struct{}{}
	for _, scopeRoot := range scopeRoots {
		scopeRoot = resolveDoctorScopePath(cityPath, scopeRoot)
		if _, seen := seenScopes[scopeRoot]; seen {
			continue
		}
		seenScopes[scopeRoot] = struct{}{}

		if sameDoctorScope(scopeRoot, cityPath) && !cityHasConfig && !doctorScopeHasBDMetadata(scopeRoot) {
			continue
		}
		if configLoadErr && sameDoctorScope(scopeRoot, cityPath) && !doctorScopeHasBDMetadata(scopeRoot) {
			continue
		}
		if !scopeUsesBDDoltStore(cityPath, scopeRoot) {
			continue
		}

		resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
		if err != nil {
			if doctorRawScopeUsesManagedCity(cityPath, scopeRoot) {
				return true
			}
			continue
		}
		switch resolved.Kind {
		case contract.ScopeConfigMissing:
			return true
		case contract.ScopeConfigAuthoritative:
			switch resolved.State.EndpointOrigin {
			case contract.EndpointOriginManagedCity:
				return true
			case contract.EndpointOriginInheritedCity:
				if inheritedDoctorScopeUsesManagedCity(cityPath) {
					return true
				}
			}
		}
	}
	return false
}

func inheritedDoctorScopeUsesManagedCity(cityPath string) bool {
	cityResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
	if err != nil || cityResolved.Kind != contract.ScopeConfigAuthoritative {
		return false
	}
	return cityResolved.State.EndpointOrigin == contract.EndpointOriginManagedCity
}

func doctorRawScopeUsesManagedCity(cityPath, scopeRoot string) bool {
	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil || !ok {
		return false
	}
	switch state.EndpointOrigin {
	case contract.EndpointOriginManagedCity:
		return true
	case contract.EndpointOriginInheritedCity:
		return inheritedDoctorScopeUsesManagedCity(cityPath)
	default:
		return false
	}
}

func managedDoltRuntimeMaterialized(cityPath string) bool {
	for _, path := range []string{
		resolveManagedDoltDataDir(cityPath),
		filepath.Join(cityPath, ".beads", "dolt"),
		filepath.Join(cityPath, ".gc", "dolt-data"),
		doctorDoltPackStateDir(cityPath),
	} {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// sumDirBytes walks root recursively and returns the total size of regular
// files found. Missing roots return (0, false, nil); any other walk error is
// returned.
func sumDirBytes(root string) (int64, bool, error) {
	return sumDirBytesWithContext(context.Background(), root)
}

func sumDirBytesWithContext(ctx context.Context, root string) (int64, bool, error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !info.IsDir() {
		return 0, false, fmt.Errorf("%s is not a directory", root)
	}
	var total int64
	err = filepath.WalkDir(root, func(_ string, d fs.DirEntry, walkErr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		fi, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		if fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	if err != nil {
		return 0, true, err
	}
	return total, true, nil
}

func boundedSumDirBytes(root string) (int64, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), doltDirMeasureTimeout)
	defer cancel()
	return sumDirBytesWithContext(ctx, root)
}

func duDirBytes(root string) (int64, bool, error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !info.IsDir() {
		return 0, false, fmt.Errorf("%s is not a directory", root)
	}

	ctx, cancel := context.WithTimeout(context.Background(), doltDirMeasureTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "du", "-sk", root)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		total, exists, fallbackErr := boundedSumDirBytes(root)
		if fallbackErr != nil {
			return 0, true, fmt.Errorf("measure dolt data dir: du -sk timed out after %s; fallback walk: %w", doltDirMeasureTimeout, fallbackErr)
		}
		return total, exists, nil
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return boundedSumDirBytes(root)
		}
		return 0, true, fmt.Errorf("measure dolt data dir with du -sk: %w", err)
	}

	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, true, fmt.Errorf("measure dolt data dir with du -sk: empty output")
	}
	kb, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, true, fmt.Errorf("measure dolt data dir with du -sk: parse %q: %w", fields[0], err)
	}
	return kb * 1024, true, nil
}

func formatGB(bytes int64) string {
	gb := float64(bytes) / (1024.0 * 1024.0 * 1024.0)
	return fmt.Sprintf("%.2f GB", gb)
}

// DoltNomsSizeCheck warns when the managed Dolt database's on-disk footprint
// is approaching or exceeds operator-set thresholds.
type DoltNomsSizeCheck struct {
	cityPath        string
	skip            bool
	measureDir      func(string) (int64, bool, error)
	applicableKnown bool
	applicable      bool
	scopeRoots      []string
}

// NewDoltNomsSizeCheck creates a Dolt noms/on-disk size check.
func NewDoltNomsSizeCheck(cityPath string, skip bool) *DoltNomsSizeCheck {
	return &DoltNomsSizeCheck{cityPath: cityPath, skip: skip, measureDir: duDirBytes}
}

// NewDoltNomsSizeCheckForConfig creates a Dolt size check using preloaded city config.
func NewDoltNomsSizeCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltNomsSizeCheck {
	return &DoltNomsSizeCheck{
		cityPath:        cityPath,
		skip:            skip,
		measureDir:      duDirBytes,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
		scopeRoots:      managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
	}
}

func (c *DoltNomsSizeCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltNomsSizeCheck) Name() string { return "dolt-noms-size" }

// Run inspects the workspace's managed local Dolt databases and compares the
// largest footprint to warning/error thresholds.
func (c *DoltNomsSizeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.managedApplicable() {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	targets, unresolved := managedLocalDoltScanTargets(c.cityPath)
	if c.applicableKnown {
		targets, unresolved = managedLocalDoltScanTargetsForScopeRoots(c.cityPath, c.scopeRoots)
	}
	if len(targets) == 0 {
		if unresolved {
			// Let the beads-store / dolt-server checks report resolution errors.
			r.Status = StatusOK
			r.Message = "skipped (dolt target unresolved)"
			return r
		}
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	var (
		worstTarget doltDataScanTarget
		worstBytes  int64
		totalBytes  int64
		existsCount int
	)
	measureDir := c.measureDir
	if measureDir == nil {
		measureDir = duDirBytes
	}
	for _, target := range targets {
		total, exists, err := measureDir(target.ScanRoot)
		if err != nil {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("scan dolt data dir: %v", err)
			return r
		}
		if !exists {
			continue
		}
		existsCount++
		totalBytes += total
		if total > worstBytes {
			worstBytes = total
			worstTarget = target
		}
	}
	if existsCount == 0 {
		r.Status = StatusOK
		r.Message = "no dolt data yet"
		return r
	}

	targetLabel := strings.TrimSpace(worstTarget.Database)
	if targetLabel == "" {
		targetLabel = "managed dolt data"
	} else if worstTarget.Orphan {
		targetLabel = "orphan database " + targetLabel
	}
	size := formatGB(worstBytes)
	scopeNote := ""
	if existsCount > 1 {
		scopeNote = fmt.Sprintf(" (largest of %d databases)", existsCount)
	}
	switch {
	case worstBytes >= doltNomsErrorBytes:
		r.Status = StatusError
		r.Message = fmt.Sprintf("dolt noms directory for %s is %s%s — excessive; recovery recommended", targetLabel, size, scopeNote)
		r.FixHint = "see docs/troubleshooting/dolt-bloat-recovery.md"
	case totalBytes >= doltNomsErrorBytes:
		r.Status = StatusError
		r.Message = fmt.Sprintf("aggregate dolt data footprint is %s across %d databases — excessive; recovery recommended", formatGB(totalBytes), existsCount)
		r.FixHint = "see docs/troubleshooting/dolt-bloat-recovery.md"
	case worstBytes >= doltNomsWarnBytes:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("dolt noms directory for %s is %s%s — approaching threshold", targetLabel, size, scopeNote)
		r.FixHint = "see docs/troubleshooting/dolt-bloat-recovery.md"
	case totalBytes >= doltNomsWarnBytes:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("aggregate dolt data footprint is %s across %d databases — approaching threshold", formatGB(totalBytes), existsCount)
		r.FixHint = "see docs/troubleshooting/dolt-bloat-recovery.md"
	default:
		r.Status = StatusOK
		if existsCount > 1 {
			r.Message = fmt.Sprintf("aggregate dolt data footprint is %s across %d databases (largest %s: %s)", formatGB(totalBytes), existsCount, targetLabel, size)
		} else {
			r.Message = fmt.Sprintf("dolt data footprint for %s %s%s", targetLabel, size, scopeNote)
		}
	}
	return r
}

// CanFix returns false — see PR 4 for bloat recovery runbook.
func (c *DoltNomsSizeCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltNomsSizeCheck) Fix(_ *CheckContext) error { return nil }

// DoltConfigExpectedValue is a dotted YAML path and value expected in the
// managed dolt-config.yaml.
type DoltConfigExpectedValue struct {
	Path  string
	Value any
}

// DoltConfigExpectedValues returns the load-bearing managed Dolt config keys
// asserted by DoltConfigCheck.
//
// This is intentionally a contract subset, not a byte-for-byte mirror of
// writeManagedDoltConfigFile in cmd/gc/cmd_dolt_config.go. It covers the keys
// whose drift would change managed runtime behavior materially. Dynamic values
// such as data_dir are checked by DoltConfigCheck because they depend on the
// inspected city path.
func DoltConfigExpectedValues() []DoltConfigExpectedValue {
	return []DoltConfigExpectedValue{
		{"behavior.auto_gc_behavior.enable", true},
		{"behavior.auto_gc_behavior.archive_level", 1},
		{"listener.read_timeout_millis", 300000},
		{"listener.write_timeout_millis", 300000},
		{"listener.max_connections", 1000},
		{"listener.back_log", 50},
		{"listener.max_connections_timeout_millis", 5000},
	}
}

// lookupYAMLPath walks a dotted key path through a decoded YAML map and
// returns the leaf value, whether it was present, and whether the traversal
// hit a non-map node before reaching the leaf.
func lookupYAMLPath(doc map[string]any, dotted string) (any, bool) {
	parts := strings.Split(dotted, ".")
	var cur any = doc
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, present := m[p]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// yamlIntEqual compares a decoded YAML scalar to an expected int value,
// accepting int, int64, uint64, and float64 decodings (gopkg.in/yaml.v3
// normally produces int, but be defensive).
func yamlIntEqual(got any, want int) bool {
	switch v := got.(type) {
	case int:
		return v == want
	case int64:
		return int64(want) == v
	case uint64:
		return uint64(want) == v //nolint:gosec // want is a fixed positive constant
	case float64:
		return v == float64(want)
	}
	return false
}

// DoltConfigCheck verifies the managed dolt-config.yaml exists and contains
// the load-bearing keys/values required by Gas City's managed Dolt contract.
type DoltConfigCheck struct {
	cityPath        string
	skip            bool
	applicableKnown bool
	applicable      bool
}

// NewDoltConfigCheck creates a managed Dolt config drift check.
func NewDoltConfigCheck(cityPath string, skip bool) *DoltConfigCheck {
	return &DoltConfigCheck{cityPath: cityPath, skip: skip}
}

// NewDoltConfigCheckForConfig creates a managed Dolt config drift check using preloaded city config.
func NewDoltConfigCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltConfigCheck {
	return &DoltConfigCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
	}
}

func (c *DoltConfigCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltConfigCheck) Name() string { return "dolt-config" }

// Run parses the managed dolt-config.yaml and verifies required keys/values.
func (c *DoltConfigCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.managedApplicable() {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	path := resolveManagedDoltConfigPath(c.cityPath)
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from city layout
	if err != nil {
		if os.IsNotExist(err) {
			if !managedDoltRuntimeMaterialized(c.cityPath) {
				r.Status = StatusOK
				r.Message = "managed dolt-config.yaml not yet generated (run gc start to materialize)"
				return r
			}
			r.Status = StatusWarning
			r.Message = "managed dolt-config.yaml not found"
			r.FixHint = "run gc start (or gc dolt restart) to regenerate"
			return r
		}
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("read dolt-config.yaml: %v", err)
		r.FixHint = "run gc start (or gc dolt restart) to regenerate"
		return r
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("parse dolt-config.yaml: %v", err)
		r.FixHint = "stop dolt (gc dolt stop) and restart to regenerate managed config"
		return r
	}

	var drifted []string
	for _, exp := range DoltConfigExpectedValues() {
		got, present := lookupYAMLPath(doc, exp.Path)
		if !present {
			drifted = append(drifted, exp.Path+" (missing)")
			continue
		}
		switch want := exp.Value.(type) {
		case bool:
			if gotBool, ok := got.(bool); !ok || gotBool != want {
				drifted = append(drifted, fmt.Sprintf("%s (got %v, want %v)", exp.Path, got, want))
			}
		case int:
			if !yamlIntEqual(got, want) {
				drifted = append(drifted, fmt.Sprintf("%s (got %v, want %d)", exp.Path, got, want))
			}
		default:
			// Strings / other scalars — stringify for compare.
			if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
				drifted = append(drifted, fmt.Sprintf("%s (got %v, want %v)", exp.Path, got, want))
			}
		}
	}
	if got, present := lookupYAMLPath(doc, "data_dir"); !present {
		drifted = append(drifted, "data_dir (missing)")
	} else {
		want := resolveManagedDoltDataDir(c.cityPath)
		if !sameDoctorScope(fmt.Sprintf("%v", got), want) {
			drifted = append(drifted, fmt.Sprintf("data_dir (got %v, want %s)", got, want))
		}
	}

	if len(drifted) > 0 {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("managed dolt-config.yaml drift: %s", strings.Join(drifted, ", "))
		r.FixHint = "stop dolt (gc dolt stop) and restart to regenerate managed config"
		return r
	}

	r.Status = StatusOK
	r.Message = "dolt config OK"
	return r
}

// CanFix returns false. TODO: wire Fix() into the same code path as
// `gc start` uses to rewrite the managed config once that helper is exposed
// from the doctor package.
func (c *DoltConfigCheck) CanFix() bool { return false }

// Fix is a no-op. See TODO on CanFix.
func (c *DoltConfigCheck) Fix(_ *CheckContext) error { return nil }

// doltVersionInfo is the parsed semantic version of the installed `dolt`.
type doltVersionInfo struct {
	Major, Minor, Patch int
	Raw                 string
}

// parseDoltVersion parses the first version-like token from `dolt version`
// output. Accepted formats:
//
//	"dolt version 1.75.2\nWarning: ..."
//	"dolt version 1.75.2"
//	"1.75.2"
//
// Any suffix after patch (e.g. "-rc1") is ignored.
func parseDoltVersion(out string) (doltVersionInfo, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return doltVersionInfo{}, fmt.Errorf("empty version output")
	}
	// Only look at the first line — dolt sometimes emits a "Warning: ..."
	// second line for deprecated flags.
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	out = strings.TrimSpace(out)
	// Strip the "dolt version " prefix if present.
	const prefix = "dolt version "
	if strings.HasPrefix(strings.ToLower(out), prefix) {
		out = out[len(prefix):]
	}
	// Take the first whitespace-delimited token.
	if i := strings.IndexAny(out, " \t"); i >= 0 {
		out = out[:i]
	}
	out = strings.TrimPrefix(out, "v")
	// Strip any pre-release / build suffix after MAJOR.MINOR.PATCH.
	core := out
	for _, sep := range []string{"-", "+"} {
		if i := strings.Index(core, sep); i >= 0 {
			core = core[:i]
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) < 3 {
		return doltVersionInfo{}, fmt.Errorf("unrecognized version %q", out)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return doltVersionInfo{}, fmt.Errorf("unrecognized major in %q: %w", out, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return doltVersionInfo{}, fmt.Errorf("unrecognized minor in %q: %w", out, err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return doltVersionInfo{}, fmt.Errorf("unrecognized patch in %q: %w", out, err)
	}
	return doltVersionInfo{Major: major, Minor: minor, Patch: patch, Raw: fmt.Sprintf("%d.%d.%d", major, minor, patch)}, nil
}

// compareDoltVersion returns -1 if a<b, 0 if a==b, 1 if a>b.
func compareDoltVersion(a, b doltVersionInfo) int {
	switch {
	case a.Major != b.Major:
		if a.Major < b.Major {
			return -1
		}
		return 1
	case a.Minor != b.Minor:
		if a.Minor < b.Minor {
			return -1
		}
		return 1
	case a.Patch != b.Patch:
		if a.Patch < b.Patch {
			return -1
		}
		return 1
	}
	return 0
}

// DoltVersionCheck shells out to `dolt version` and verifies the managed-Dolt
// minimum version requirement.
type DoltVersionCheck struct {
	cityPath string
	// versionOutput is injectable for tests. Nil means exec `dolt version`.
	versionOutput   func() (string, error)
	skip            bool
	applicableKnown bool
	applicable      bool
}

// NewDoltVersionCheck creates a dolt binary version check.
func NewDoltVersionCheck(skip ...bool) *DoltVersionCheck {
	return NewScopedDoltVersionCheck("", skip...)
}

// NewScopedDoltVersionCheck creates a dolt binary version check for a
// specific workspace scope so external-only targets can be skipped cleanly.
func NewScopedDoltVersionCheck(cityPath string, skip ...bool) *DoltVersionCheck {
	c := &DoltVersionCheck{cityPath: cityPath}
	if len(skip) > 0 {
		c.skip = skip[0]
	}
	return c
}

// NewScopedDoltVersionCheckForConfig creates a scoped Dolt version check using preloaded city config.
func NewScopedDoltVersionCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltVersionCheck {
	return &DoltVersionCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
	}
}

func (c *DoltVersionCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltVersionCheck) Name() string { return "dolt-version" }

// Run invokes `dolt version` and compares against the managed-Dolt minimum.
func (c *DoltVersionCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || (c.cityPath != "" && !c.managedApplicable()) {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	getOutput := c.versionOutput
	if getOutput == nil {
		getOutput = func() (string, error) {
			path, lookErr := exec.LookPath("dolt")
			if lookErr != nil {
				return "", lookErr
			}
			ctx, cancel := context.WithTimeout(context.Background(), doltVersionCommandTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, path, "version")
			out, err := cmd.CombinedOutput()
			if ctx.Err() == context.DeadlineExceeded {
				return string(out), fmt.Errorf("dolt version timed out after %s", doltVersionCommandTimeout)
			}
			return string(out), err
		}
	}

	out, err := getOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found") {
			r.Status = StatusWarning
			r.Message = "dolt binary not in PATH"
			return r
		}
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("invoke dolt version: %v", err)
		return r
	}

	info, err := parseDoltVersion(out)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("parse dolt version: %v", err)
		return r
	}

	minVer, _ := parseDoltVersion(doltversion.ManagedMin)

	if compareDoltVersion(info, minVer) < 0 {
		r.Status = StatusError
		r.Message = fmt.Sprintf("dolt version %s is below minimum %s required for managed config", info.Raw, doltversion.ManagedMin)
		r.FixHint = "upgrade dolt: https://docs.dolthub.com/introduction/installation"
		return r
	}
	r.Status = StatusOK
	r.Message = fmt.Sprintf("dolt %s", info.Raw)
	return r
}

// CanFix returns false — upgrade instructions live in the FixHint.
func (c *DoltVersionCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltVersionCheck) Fix(_ *CheckContext) error { return nil }

// IsControllerRunning probes the controller lock file to determine if a
// controller is currently running. It tries to acquire the flock — if it
// fails with EWOULDBLOCK, the controller holds the lock.
func IsControllerRunning(cityPath string) bool {
	path := filepath.Join(cityPath, ".gc", "controller.lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		// Lock file doesn't exist — no controller is running.
		return false
	}
	defer f.Close() //nolint:errcheck // probe only

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		// EWOULDBLOCK means the lock is held — controller is running.
		return true
	}
	// We got the lock, release immediately — no controller running.
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock
	return false
}
