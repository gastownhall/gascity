package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/spf13/cobra"
)

var (
	newDoctorDoltServerCheck    = doctor.NewDoltServerCheck
	newDoctorRigDoltServerCheck = doctor.NewRigDoltServerCheck
	newDoctorDoltBackupCheck    = doctor.NewDoltBackupCheck
	newDoctorDoltLocalOnlyCheck = doctor.NewDoltLocalOnlyRemoteCheck
)

func newDoctorCmd(stdout, stderr io.Writer) *cobra.Command {
	var fix, verbose, jsonOut, explainPostgresAuth bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check workspace health",
		Long: `Run diagnostic health checks on the city workspace.

Checks city structure, config validity, binary dependencies (tmux, git,
bd, dolt), controller status, agent sessions, zombie/orphan sessions,
bead stores, Dolt server health, event log integrity, and per-rig
health. Use --fix for the canonical remediation path, including any
safe mechanical PackV1-to-PackV2 rewrites that are available on this
branch.`,
		Example: `  gc doctor
  gc doctor --fix
  gc doctor --verbose
  gc doctor --json
  gc doctor --explain-postgres-auth`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doDoctor(fix, verbose, jsonOut, explainPostgresAuth, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "attempt automatic repairs and safe mechanical migrations")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show extra diagnostic details")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit structured JSON instead of human-readable output")
	cmd.Flags().BoolVar(&explainPostgresAuth, "explain-postgres-auth", false,
		"after running checks, print per-scope Postgres credential resolution table (no values printed)")
	return cmd
}

// doctorWorkspaceHasPostgresScope reports whether at least one scope
// (city or any rig) has MetadataState.Backend == "postgres". Used to
// gate registration of PostgresAuthCheck so pure-Dolt cities never see
// a "skipped postgres-auth" line.
func doctorWorkspaceHasPostgresScope(cityPath string, cfg *config.City) bool {
	if scopeBackendIsPostgres(cityPath, cityPath) {
		return true
	}
	if cfg == nil {
		return false
	}
	for _, rig := range cfg.Rigs {
		if rig.Suspended {
			continue
		}
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			continue
		}
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		if scopeBackendIsPostgres(cityPath, rigPath) {
			return true
		}
	}
	return false
}

// doDoctor runs all health checks and prints results.
func doctorSkipsDoltChecks(cityPath string) bool {
	if gcDoltSkip() {
		return true
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return !cityUsesBdStoreContract(cityPath)
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	return !workspaceUsesManagedBdStoreContract(cityPath, cfg.Rigs)
}

func workspaceNeedsCityDoltCheck(cityPath string, cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	for _, rig := range cfg.Rigs {
		if !rigUsesManagedBdStoreContract(cityPath, rig) {
			continue
		}
		explicit, err := contract.ScopeUsesExplicitEndpoint(fsys.OSFS{}, cityPath, rig.Path)
		if err != nil || !explicit {
			return true
		}
	}
	return false
}

func managedDoltOpsCheckSkip(cityPath string, cfg *config.City, cfgErr error) bool {
	if gcDoltSkip() {
		return true
	}
	return !doctor.ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr)
}

type doltTopologyCheck struct {
	cityPath string
	cfg      *config.City
}

func newDoltTopologyCheck(cityPath string, cfg *config.City) *doltTopologyCheck {
	return &doltTopologyCheck{cityPath: cityPath, cfg: cfg}
}

func (c *doltTopologyCheck) Name() string { return "dolt-topology" }

func (c *doltTopologyCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	if c.cfg == nil || !workspaceUsesManagedBdStoreContract(c.cityPath, c.cfg.Rigs) {
		r.Status = doctor.StatusOK
		r.Message = "not using bd-backed Dolt topology"
		return r
	}
	if err := validateCanonicalCompatDoltDrift(c.cityPath, c.cfg); err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("canonical/compat Dolt drift: %v", err)
		r.FixHint = "reconcile canonical .beads config with deprecated city.toml Dolt settings"
		return r
	}
	r.Status = doctor.StatusOK
	r.Message = "canonical and deprecated Dolt endpoint config agree"
	return r
}

func (c *doltTopologyCheck) CanFix() bool { return false }

func (c *doltTopologyCheck) Fix(_ *doctor.CheckContext) error { return nil }

type buildDoctorChecksOpts struct {
	Stderr               io.Writer
	ControllerRunning    bool
	SkipCityDoltCheck    bool
	SkipManagedDoltCheck bool
}

func doctorOrderFiringCurrentLastRunFunc(cityPath string, cfg *config.City, stderr io.Writer) doctor.OrderFiringCurrentLastRunFunc {
	if stderr == nil {
		stderr = io.Discard
	}
	resolveStores := cachedOrderHistoryStoresResolver(cityPath, cfg, stderr)
	return func(order orders.Order) (time.Time, error) {
		stores, err := resolveStores(order)
		if err != nil {
			return time.Time{}, err
		}
		return orders.LastRunAcrossStores(stores...)(order.ScopedName())
	}
}

func buildDoctorChecks(cityPath string, cfg *config.City, cfgErr error, opts buildDoctorChecksOpts) []doctor.Check {
	var checks []doctor.Check
	register := func(c doctor.Check) {
		checks = append(checks, c)
	}

	managedDoltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	if layout, err := resolveManagedDoltRuntimeLayout(cityPath); err == nil {
		managedDoltDataDir = layout.DataDir
	}

	// Core checks — always run.
	register(&doctor.CityStructureCheck{})
	register(&doctor.CityConfigCheck{})
	for _, c := range v2DeprecationChecks() {
		register(c)
	}
	register(newProviderCatalogDoctorCheck(cityPath))
	register(newProviderCatalogReadinessAdvisoryCheck(cityPath))
	register(expandedConfigLoadCheck{})
	register(&doctor.ImplicitImportCacheCheck{})
	register(&doctor.DeprecatedAttachmentFieldsCheck{})

	// Config-dependent checks run only when city.toml loaded cleanly. If it
	// fails, the core config check above reports the parse error.
	if cfgErr == nil && cfg != nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		if workspaceUsesManagedBdStoreContract(cityPath, cfg.Rigs) {
			register(newDoltTopologyCheck(cityPath, cfg))
			register(newDoltDriftCheck(cityPath, cfg))
		}
		register(doctor.NewConfigValidCheck(cfg))
		register(doctor.NewLegacySuspendedFieldCheck(cfg))
		register(doctor.NewConfigRefsCheck(cfg, cityPath))
		register(doctor.NewStaleLocalPackDirCheck(cfg.Packs, cfg.Imports, cfg.DefaultRigImports, cityPath, cfg.Rigs...))
		register(doctor.NewPreStartScriptsCheck(cfg))
		register(doctor.NewBuiltinPackFamilyCheck(cfg, cityPath))
		register(doctor.NewConfigSemanticsCheck(cfg, filepath.Join(cityPath, "city.toml")))
		register(doctor.NewDurationRangeCheck(cfg))
		register(doctor.NewProviderParityCheck(cfg))
		register(doctor.NewFormulaRequirementsCheck(cfg, cityPath))
		register(doctor.NewNamedAlwaysMinConflictCheck(cfg))
		register(doctor.NewInstructionsFileCheck(cfg, cityPath))
		register(doctor.NewSkillCollisionCheck(cfg, cityPath))
		register(doctor.NewOrderFiringCurrentCheck(cfg, cityPath, doctor.WithOrderFiringCurrentLastRunFunc(doctorOrderFiringCurrentLastRunFunc(cityPath, cfg, opts.Stderr))))
		register(newCodexHooksDriftCheck(codexHookWorkDirs(cityPath, cfg)))
		register(newBeadsProxiedCapabilityCheck(cfg))
		register(doctor.NewRigPackCoverageCheck(cfg, cityPath))
		register(newMCPConfigDoctorCheck(cityPath, cfg, exec.LookPath))
		register(newMCPSharedTargetDoctorCheck(cityPath, cfg, exec.LookPath))
	}
	if _, rawCfgErr := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml")); rawCfgErr == nil {
		register(newImportStateDoctorCheck(cityPath))
		register(newJsonlArchiveDoctorCheck(cityPath))
	}

	// System formulas/orders now ship via the core bootstrap pack; pack
	// materialization and the bootstrap collision checks cover what the
	// legacy SystemFormulasCheck used to verify.

	// Pack cache check (if config has remote packs).
	if cfgErr == nil && cfg != nil && len(cfg.Packs) > 0 {
		register(doctor.NewPackCacheCheck(cfg.Packs, cityPath))
	}

	// Infrastructure checks — universal dependencies.
	// dolt/bd/flock are checked by pack doctor scripts (check-bd.sh,
	// check-dolt.sh) which also verify versions and service health.
	register(doctor.NewBinaryCheck("tmux", "", exec.LookPath))
	register(doctor.NewBinaryCheck("git", "", exec.LookPath))
	register(doctor.NewBinaryCheck("jq", "", exec.LookPath))
	register(doctor.NewBinaryCheck("pgrep", "", exec.LookPath))
	register(doctor.NewBinaryCheck("lsof", "", exec.LookPath))
	// beads.role must be set before any bd command runs; check it here so
	// the missing-role error appears before the downstream data/Dolt checks
	// that will all fail for the same root cause.
	if initNeedsBdTooling(cityPath) {
		register(&doctor.BeadsRoleCheck{})
	}

	// Controller check + session checks (gated by controller state).
	controllerRunning := opts.ControllerRunning
	register(doctor.NewControllerCheck(cityPath, controllerRunning))

	if cfgErr == nil && cfg != nil && !controllerRunning {
		cityName := loadedCityName(cfg, cityPath)
		st := cfg.Workspace.SessionTemplate
		sp := newSessionProvider()

		register(doctor.NewAgentSessionsCheck(cfg, cityName, st, sp))
		register(doctor.NewZombieSessionsCheck(cfg, cityName, st, sp))
		register(doctor.NewOrphanSessionsCheck(cfg, cityName, st, sp))
	}

	storeFactory := openStoreForCity(cityPath)

	// Data checks.
	if cfgErr == nil && cfg != nil {
		register(doctor.NewBDSplitStoreCheck(cityPath))
		register(doctor.NewBeadsStoreCheck(cityPath, storeFactory))
		register(newV2RoutedToNamespaceCheck(cfg, cityPath, storeFactory))
		register(newRunTargetRoutedToBackfillCheck(cfg, cityPath, storeFactory))
		register(newBacklogDepthCheck(cityPath, storeFactory))
		register(newOrderTrackingRetentionCheck(cityPath, storeFactory))
		register(&sessionModelDoctorCheck{cfg: cfg, cityPath: cityPath, newStore: storeFactory})
	}
	register(newDoctorDoltServerCheck(cityPath, opts.SkipCityDoltCheck))
	if cfgErr == nil && doctorWorkspaceHasPostgresScope(cityPath, cfg) {
		register(doctor.NewPostgresAuthCheck(cityPath, cfg))
	}
	// Managed Dolt ops checks (PR 3). Size + config drift are only
	// meaningful when the workspace uses the managed bd/Dolt backend; rigs
	// can inherit the city-managed server even when the city itself is not a
	// managed bd scope. The version check follows the same gate so file-backed
	// and external Dolt workspaces do not get irrelevant local-binary warnings.
	register(doctor.NewDoltNomsSizeCheckForConfig(cityPath, opts.SkipManagedDoltCheck, cfg, cfgErr))
	register(doctor.NewDoltConfigCheckForConfig(cityPath, opts.SkipManagedDoltCheck, cfg, cfgErr))
	register(doctor.NewScopedDoltVersionCheckForConfig(cityPath, opts.SkipManagedDoltCheck, cfg, cfgErr))
	register(&doctor.EventsLogCheck{})
	register(doctor.NewEventLogSizeCheck())
	// bd auto-backup growth canary. bd's auto-backup pipeline (upstream of
	// gascity, gastownhall/beads#2993) writes to .beads/backup/ on every bd
	// invocation without retention. This check warns before the directory
	// fills the disk and cascades into broken dolt writes.
	register(doctor.NewBdBackupSizeCheckForConfig(cityPath, cfg, cfgErr))
	// Worktree checks deliberately run even when cfgErr != nil — they
	// only need the city path, and a broken city.toml is exactly when
	// silent disk-fill is most likely. The zero-value DoctorConfig
	// produces sensible 10/50 GB defaults via its accessor methods.
	var doctorCfg config.DoctorConfig
	if cfg != nil {
		doctorCfg = cfg.Doctor
	}
	register(doctor.NewWorktreeDiskSizeCheck(doctorCfg))
	register(doctor.NewNestedWorktreePruneCheck(doctorCfg))

	// Custom types check — city store.
	register(doctor.NewCustomTypesCheck(cityPath, "city"))

	// Per-rig checks. Skip effectively-suspended rigs — opening their
	// bead store triggers bd auto-start of orphan Dolt servers (ga-wzk).
	if cfgErr == nil && cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
		for _, rig := range cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) {
				continue
			}
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			register(doctor.NewRigPathCheck(rig))
			register(doctor.NewRigGitCheck(rig))
			register(doctor.NewRigRootBranchCheck(rig))
			register(doctor.NewRigBDSplitStoreCheck(cityPath, rig))
			register(doctor.NewRigBeadsCheck(cityPath, rig, storeFactory))
			register(newDoctorRigDoltServerCheck(cityPath, rig, !rigUsesManagedBdStoreContract(cityPath, rig) || gcDoltSkip()))
			// Custom types check — rig store.
			register(doctor.NewCustomTypesCheck(rig.Path, rig.Name))
			// Dolt-backup registration catches the silent gap left by
			// `gc rig add` before the rig is eligible for mol-dog backup
			// automation. Gated to match the sibling dolt-server check:
			// skip non-managed-bdstore rigs and GC_DOLT=skip environments.
			if rigUsesManagedBdStoreContract(cityPath, rig) && !gcDoltSkip() {
				register(newDoctorDoltBackupCheck(cityPath, rig, managedDoltDataDir))
				register(newDoctorDoltLocalOnlyCheck(cityPath, rig, managedDoltDataDir))
			}
		}
	}

	// Worktree integrity check.
	register(&doctor.WorktreeCheck{})

	// Pack doctor checks — scripts shipped with packs.
	if cfgErr == nil && cfg != nil {
		for _, entry := range cfg.PackDoctors {
			register(&doctor.PackScriptCheck{
				CheckName: entry.PackName + ":" + entry.Name,
				Script:    entry.RunScript,
				FixScript: entry.FixScript,
				PackDir:   entry.PackDir,
				PackName:  entry.PackName,
				Warmup:    entry.Warmup,
			})
		}
		registerLocalDoctorChecksTo(register, cityPath, cfg.Doctor.Checks)
	}

	return checks
}

func doDoctor(fix, verbose, jsonOut, explainPostgresAuth bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc doctor: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	d := &doctor.Doctor{}
	ctx := &doctor.CheckContext{CityPath: cityPath, Verbose: verbose, ExplainPostgresAuth: explainPostgresAuth}
	cfg, cfgErr := loadCityConfig(cityPath, stderr)
	if cfgErr == nil {
		resolveRigPaths(cityPath, cfg.Rigs)
	}
	controllerRunning := doctor.IsControllerRunning(cityPath)
	skipCityDoltCheck := gcDoltSkip() || (!scopeUsesManagedBdStoreContract(cityPath, cityPath) && !workspaceNeedsCityDoltCheck(cityPath, cfg))
	skipManagedDoltCheck := managedDoltOpsCheckSkip(cityPath, cfg, cfgErr)
	for _, check := range buildDoctorChecks(cityPath, cfg, cfgErr, buildDoctorChecksOpts{
		Stderr:               stderr,
		ControllerRunning:    controllerRunning,
		SkipCityDoltCheck:    skipCityDoltCheck,
		SkipManagedDoltCheck: skipManagedDoltCheck,
	}) {
		d.Register(check)
	}

	var report *doctor.Report
	if jsonOut {
		report = d.RunCollect(ctx, fix)
		if err := writeDoctorJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "gc doctor: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		report = d.Run(ctx, stdout, fix)
		doctor.PrintSummary(stdout, report)
	}

	if report.BlockingFailed > 0 {
		return 1
	}
	return 0
}

type expandedConfigLoadCheck struct{}

func (expandedConfigLoadCheck) Name() string { return "expanded-config-load" }

func (expandedConfigLoadCheck) CanFix() bool { return false }

func (expandedConfigLoadCheck) WarmupEligible() bool { return false }

func (expandedConfigLoadCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (expandedConfigLoadCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	if _, err := loadCityConfig(ctx.CityPath, io.Discard); err != nil {
		return errorCheck("expanded-config-load",
			fmt.Sprintf("expanded config load error: %v", err),
			expandedConfigLoadFixHint(err),
			nil)
	}
	return okCheck("expanded-config-load", "expanded config loaded")
}

func expandedConfigLoadFixHint(err error) string {
	var providerErr *config.ProviderCatalogError
	if errors.As(err, &providerErr) {
		return "run `gc doctor --fix` to add missing builtin provider aliases; add custom providers manually"
	}
	if config.IsFragmentLegacyV1SurfaceError(err) {
		return "move fragment-authored legacy surfaces by hand; `gc doctor --fix` only rewrites root city.toml/pack.toml surfaces"
	}
	return "fix the reported config, include, import, or pack-layout error and rerun gc doctor"
}

func registerLocalDoctorChecks(d *doctor.Doctor, cityPath string, checks []config.LocalDoctorCheck) {
	registerLocalDoctorChecksTo(d.Register, cityPath, checks)
}

func registerLocalDoctorChecksTo(register func(doctor.Check), cityPath string, checks []config.LocalDoctorCheck) {
	for _, check := range checks {
		checkName := "local:" + check.Name
		script, err := resolveLocalDoctorScript(cityPath, check.Script)
		if err != nil {
			register(doctor.ErrorCheck(checkName, err.Error()))
			continue
		}

		packCheck := &doctor.PackScriptCheck{
			CheckName: checkName,
			Script:    script,
			PackDir:   cityPath,
		}
		if check.Fix != "" {
			fixScript, err := resolveLocalDoctorFixScript(cityPath, check.Fix)
			if err != nil {
				register(doctor.ErrorCheck(checkName, err.Error()))
				continue
			}
			packCheck.FixScript = fixScript
		}
		register(packCheck)
	}
}

func resolveLocalDoctorScript(cityPath, scriptPath string) (string, error) {
	return resolveLocalDoctorPath("script", cityPath, scriptPath)
}

func resolveLocalDoctorFixScript(cityPath, fixPath string) (string, error) {
	return resolveLocalDoctorPath("fix path", cityPath, fixPath)
}

func resolveLocalDoctorPath(kind, cityPath, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("%s must not be empty", kind)
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%s %q must be relative to the city root", kind, relPath)
	}

	candidate := filepath.Clean(filepath.Join(cityPath, relPath))
	absCityPath, err := filepath.Abs(cityPath)
	if err != nil {
		return "", err
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absCityPath, absCandidate)
	if err != nil {
		return "", err
	}
	if pathutil.IsOutsideDir(rel) {
		return "", fmt.Errorf("%s %q escapes the city directory", kind, relPath)
	}
	return candidate, nil
}

// doctorJSONResult mirrors doctor.CheckResult for JSON output. Keeping the
// shape separate from the internal type keeps the wire format stable if the
// internal struct grows new fields that shouldn't leak out.
type doctorJSONResult struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Severity     string   `json:"severity"`
	Message      string   `json:"message"`
	FixHint      string   `json:"fix_hint,omitempty"`
	Details      []string `json:"details,omitempty"`
	FixAttempted bool     `json:"fix_attempted,omitempty"`
	FixError     string   `json:"fix_error,omitempty"`
	Fixed        bool     `json:"fixed,omitempty"`
}

type doctorJSONReport struct {
	Passed         int                `json:"passed"`
	Warned         int                `json:"warned"`
	Failed         int                `json:"failed"`
	BlockingFailed int                `json:"blocking_failed"`
	Fixed          int                `json:"fixed"`
	Results        []doctorJSONResult `json:"results"`
	Error          string             `json:"error,omitempty"`
}

func doctorStatusString(s doctor.CheckStatus) string {
	switch s {
	case doctor.StatusOK:
		return "ok"
	case doctor.StatusWarning:
		return "warning"
	case doctor.StatusError:
		return "error"
	}
	return "unknown"
}

func doctorSeverityString(s doctor.CheckSeverity) string {
	switch s {
	case doctor.SeverityAdvisory:
		return "advisory"
	case doctor.SeverityBlocking:
		return "blocking"
	}
	return "blocking"
}

func writeDoctorJSON(w io.Writer, report *doctor.Report) error {
	out := doctorJSONReport{
		Passed:         report.Passed,
		Warned:         report.Warned,
		Failed:         report.Failed,
		BlockingFailed: report.BlockingFailed,
		Fixed:          report.Fixed,
		Results:        make([]doctorJSONResult, 0, len(report.Results)),
	}
	for _, r := range report.Results {
		out.Results = append(out.Results, doctorJSONResult{
			Name:         r.Name,
			Status:       doctorStatusString(r.Status),
			Severity:     doctorSeverityString(r.Severity),
			Message:      r.Message,
			FixHint:      r.FixHint,
			Details:      r.Details,
			FixAttempted: r.FixAttempted,
			FixError:     r.FixError,
			Fixed:        r.Fixed,
		})
	}
	return writeCLIJSONLine(w, out)
}

// collectPackDirs returns all unique pack directories from the city
// config (both city-level and per-rig). Used to discover pack doctor checks.
func collectPackDirs(cfg *config.City) []string {
	seen := make(map[string]bool)
	var result []string
	for _, dir := range cfg.PackDirs {
		if !seen[dir] {
			seen[dir] = true
			result = append(result, dir)
		}
	}
	for _, dirs := range cfg.RigPackDirs {
		for _, dir := range dirs {
			if !seen[dir] {
				seen[dir] = true
				result = append(result, dir)
			}
		}
	}
	return result
}

// openStoreForCity creates a beads.Store factory rooted in the given city.
// Doctor uses this so rig stores outside the city tree still inherit the
// canonical city topology instead of guessing from the rig path.
func openStoreForCity(cityPath string) func(string) (beads.Store, error) {
	return func(dirPath string) (beads.Store, error) {
		return openStoreAtForCity(dirPath, cityPath)
	}
}
