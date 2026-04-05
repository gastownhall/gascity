package doctor

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/deps"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
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

// CityConfigCheck verifies city.toml parses and workspace.name is set.
type CityConfigCheck struct{}

// Name returns the check identifier.
func (c *CityConfigCheck) Name() string { return "city-config" }

// Run parses city.toml and checks workspace.name.
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
	summary := fmt.Sprintf("city.toml loaded (%d agents, %d rigs)", len(cfg.Agents), len(cfg.Rigs))
	if cfg.Workspace.Name == "" {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("workspace.name not set (using derived name %q); %s", cfg.ResolvedWorkspaceName, summary)
		return r
	}
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
			path := filepath.Join(c.cityPath, a.PromptTemplate)
			if _, err := os.Stat(path); err != nil {
				issues = append(issues, fmt.Sprintf("agent %q: prompt_template %q not found", qn, a.PromptTemplate))
			}
		}
		if a.SessionSetupScript != "" {
			path := filepath.Join(c.cityPath, a.SessionSetupScript)
			if _, err := os.Stat(path); err != nil {
				issues = append(issues, fmt.Sprintf("agent %q: session_setup_script %q not found", qn, a.SessionSetupScript))
			}
		}
		if a.OverlayDir != "" {
			path := filepath.Join(c.cityPath, a.OverlayDir)
			if fi, err := os.Stat(path); err != nil {
				issues = append(issues, fmt.Sprintf("agent %q: overlay_dir %q not found", qn, a.OverlayDir))
			} else if !fi.IsDir() {
				issues = append(issues, fmt.Sprintf("agent %q: overlay_dir %q is not a directory", qn, a.OverlayDir))
			}
		}
		if a.Provider != "" && len(c.cfg.Providers) > 0 {
			if _, ok := c.cfg.Providers[a.Provider]; !ok {
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
	if provider != "" && provider != "bd" {
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
	if err != nil {
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
		r.Status = StatusOK
		r.Message = "no orphaned sessions"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d orphaned session(s)", len(orphans))
	r.Details = orphans
	return r
}

// CanFix returns true — orphan sessions can be killed.
func (c *OrphanSessionsCheck) CanFix() bool { return true }

// Fix kills all orphaned sessions.
func (c *OrphanSessionsCheck) Fix(_ *CheckContext) error {
	prefix := "" // per-city socket isolation: all sessions belong to this city
	running, err := c.sp.ListRunning(prefix)
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

// BeadsStoreCheck verifies the bead store opens and an open-bead query succeeds.
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

// Run opens the store and calls List with an explicit open status filter.
// Using List(Status:"open") avoids an unbounded cross-database query that times
// out on large Dolt-backed stores (same root cause as gc mail inbox timeout).
func (c *BeadsStoreCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("store open failed: %v", err)
		return r
	}
	list, err := store.List(beads.ListQuery{Status: "open"})
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("store list failed: %v", err)
		return r
	}
	r.Status = StatusOK
	r.Message = fmt.Sprintf("store accessible (%d open beads)", len(list))
	return r
}

// CanFix returns false.
func (c *BeadsStoreCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *BeadsStoreCheck) Fix(_ *CheckContext) error { return nil }

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

	// Determine host and port.
	// Priority: GC_DOLT_PORT env → .beads/dolt-server.port file → error.
	// The old default of 3307 was wrong — gc-beads-bd uses hashed ephemeral
	// ports, not the default MySQL port.
	host := os.Getenv("GC_DOLT_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("GC_DOLT_PORT")
	if port == "" {
		portFile := filepath.Join(c.cityPath, ".beads", "dolt-server.port")
		if data, err := os.ReadFile(portFile); err == nil {
			port = strings.TrimSpace(string(data))
		}
	}
	if port == "" {
		r.Status = StatusWarning
		r.Message = "dolt server port unknown (no GC_DOLT_PORT and no .beads/dolt-server.port)"
		r.FixHint = "run gc start to start the dolt server"
		return r
	}
	addr := net.JoinHostPort(host, port)

	// Check TCP reachability.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("dolt server not reachable at %s", addr)
		r.FixHint = "run gc start to start the dolt server"
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
	rig      config.Rig
	newStore func(rigPath string) (beads.Store, error)
}

// NewRigBeadsCheck creates a rig beads store accessibility check.
func NewRigBeadsCheck(rig config.Rig, newStore func(string) (beads.Store, error)) *RigBeadsCheck {
	return &RigBeadsCheck{rig: rig, newStore: newStore}
}

// Name returns the check identifier.
func (c *RigBeadsCheck) Name() string { return "rig:" + c.rig.Name + ":beads" }

// Run opens the rig's bead store and calls List with an explicit open status
// filter.
func (c *RigBeadsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	store, err := c.newStore(c.rig.Path)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("store open failed: %v", err)
		return r
	}
	list, err := store.List(beads.ListQuery{Status: "open"})
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("store list failed: %v", err)
		return r
	}
	r.Status = StatusOK
	r.Message = fmt.Sprintf("store accessible (%d open beads)", len(list))
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

// --- System formulas check ---

// SystemFormulasCheck verifies formulas/ and orders/ contain all expected
// system files with correct content.
type SystemFormulasCheck struct {
	CityPath string
	// Expected is the list of relative paths from ListEmbeddedSystemFormulas.
	// Order files (orders/*) are checked under orders/; formula files under formulas/.
	Expected []string
	// ExpectedContent maps relative path → file content for staleness detection.
	// If nil, only presence is checked (not content).
	ExpectedContent map[string][]byte
	// FixFn re-materializes system formulas. Called by Fix().
	FixFn func() error
}

// Name returns the check identifier.
func (c *SystemFormulasCheck) Name() string { return "system-formulas" }

// systemFileAbsPath returns the absolute path for a system formula/order
// file. Orders (orders/*) resolve under the city orders/ directory;
// formulas resolve under formulas/.
func (c *SystemFormulasCheck) systemFileAbsPath(rel string) string {
	if strings.HasPrefix(rel, "orders/") {
		return filepath.Join(c.CityPath, citylayout.OrdersRoot, strings.TrimPrefix(rel, "orders/"))
	}
	return filepath.Join(c.CityPath, citylayout.FormulasRoot, rel)
}

// Run checks that the formulas/ and orders/ directories have all expected system files.
func (c *SystemFormulasCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	if len(c.Expected) == 0 {
		r.Status = StatusOK
		r.Message = "no system formulas expected"
		return r
	}

	var stale []string
	for _, rel := range c.Expected {
		path := c.systemFileAbsPath(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			stale = append(stale, rel+" (missing)")
			continue
		}
		if c.ExpectedContent != nil {
			if expected, ok := c.ExpectedContent[rel]; ok && string(data) != string(expected) {
				stale = append(stale, rel+" (stale)")
			}
		}
	}

	if len(stale) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("all %d system formula(s) present", len(c.Expected))
		return r
	}

	r.Status = StatusError
	r.Message = fmt.Sprintf("%d system formula(s) missing or stale", len(stale))
	r.Details = stale
	r.FixHint = "run gc doctor --fix to re-materialize"
	return r
}

// CanFix returns true — system formulas can be re-materialized.
func (c *SystemFormulasCheck) CanFix() bool { return c.FixFn != nil }

// Fix re-materializes system formulas from the embedded FS.
func (c *SystemFormulasCheck) Fix(_ *CheckContext) error {
	if c.FixFn == nil {
		return fmt.Errorf("no fix function provided")
	}
	return c.FixFn()
}

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

// --- Global rig index checks ---

// RigIndexCheck verifies that all rigs in the current city's city.toml
// are registered in the global cities.toml rig index, and that each rig's
// .beads/.env has the correct GT_ROOT.
type RigIndexCheck struct {
	// RegistryPath is the path to cities.toml. If empty, uses the default.
	RegistryPath string
	// FixFn is called during Fix to backfill entries. If nil, fix is unavailable.
	FixFn func(cityPath string) error
	// missing is populated by Run for use by Fix.
	missing []rigIndexIssue
}

type rigIndexIssue struct {
	rigName  string
	rigPath  string
	issue    string // "not_registered", "wrong_default", "missing_gt_root", "wrong_gt_root"
	cityPath string
}

// Name returns the check identifier.
func (c *RigIndexCheck) Name() string { return "rig-index" }

// Run checks global rig index consistency.
func (c *RigIndexCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	c.missing = nil

	regPath := c.RegistryPath
	if regPath == "" {
		regPath = supervisor.RegistryPath()
	}

	reg := supervisor.NewRegistry(regPath)
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(ctx.CityPath, "city.toml"))
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("cannot load city config: %v", err)
		return r
	}

	for _, rig := range cfg.Rigs {
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(ctx.CityPath, rigPath)
		}
		rigPath = filepath.Clean(rigPath)

		// Check global registry.
		entry, ok := reg.LookupRigByName(rig.Name)
		if !ok {
			c.missing = append(c.missing, rigIndexIssue{
				rigName: rig.Name, rigPath: rigPath,
				issue: "not_registered", cityPath: ctx.CityPath,
			})
			r.Details = append(r.Details, fmt.Sprintf("rig %q not in global index", rig.Name))
			continue
		}

		// Check default_city.
		if entry.DefaultCity == "" {
			c.missing = append(c.missing, rigIndexIssue{
				rigName: rig.Name, rigPath: rigPath,
				issue: "wrong_default", cityPath: ctx.CityPath,
			})
			r.Details = append(r.Details, fmt.Sprintf("rig %q has no default city", rig.Name))
		}

		// Check .beads/.env GT_ROOT.
		envPath := filepath.Join(rigPath, ".beads", ".env")
		envData, envErr := os.ReadFile(envPath)
		if envErr != nil {
			c.missing = append(c.missing, rigIndexIssue{
				rigName: rig.Name, rigPath: rigPath,
				issue: "missing_gt_root", cityPath: ctx.CityPath,
			})
			r.Details = append(r.Details, fmt.Sprintf("rig %q: .beads/.env missing", rig.Name))
			continue
		}
		gtRoot := parseGTRoot(string(envData))
		expectedCity := entry.DefaultCity
		if expectedCity == "" {
			expectedCity = ctx.CityPath
		}
		if gtRoot != expectedCity {
			c.missing = append(c.missing, rigIndexIssue{
				rigName: rig.Name, rigPath: rigPath,
				issue: "wrong_gt_root", cityPath: ctx.CityPath,
			})
			r.Details = append(r.Details, fmt.Sprintf("rig %q: GT_ROOT=%s, want %s", rig.Name, gtRoot, expectedCity))
		}
	}

	if len(c.missing) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("all %d rigs in global index with correct .beads/.env", len(cfg.Rigs))
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d rig index issues found", len(c.missing))
	r.FixHint = "run \"gc doctor --fix\" to backfill"
	return r
}

// CanFix returns true when a fix function is provided.
func (c *RigIndexCheck) CanFix() bool { return c.FixFn != nil }

// Fix backfills missing rig index entries and .beads/.env.
func (c *RigIndexCheck) Fix(ctx *CheckContext) error {
	if c.FixFn != nil {
		return c.FixFn(ctx.CityPath)
	}
	return fmt.Errorf("no fix function provided")
}

// parseGTRoot extracts the GT_ROOT value from .env file content.
func parseGTRoot(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "GT_ROOT=") {
			return strings.TrimPrefix(line, "GT_ROOT=")
		}
	}
	return ""
}
