package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

func newDoctorCmd(stdout, stderr io.Writer) *cobra.Command {
	var fix, verbose bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check workspace health",
		Long: `Run diagnostic health checks on the city workspace.

Checks city structure, config validity, session-backend dependencies
(including tmux when required), git, bd, dolt, controller status,
agent sessions, zombie/orphan sessions, bead stores, Dolt server
health, event log integrity, and per-rig health. Use --fix to
attempt automatic repairs.`,
		Example: `  gc doctor
  gc doctor --fix
  gc doctor --verbose`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doDoctor(fix, verbose, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "attempt to fix issues automatically")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show extra diagnostic details")
	return cmd
}

// doDoctor runs all health checks and prints results.
func doctorSkipsDoltChecks(cityPath string) bool {
	return !cityUsesBdStoreContract(cityPath) || os.Getenv("GC_DOLT") == "skip"
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
	if !cityUsesBdStoreContract(c.cityPath) {
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

func doDoctor(fix, verbose bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc doctor: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	d := &doctor.Doctor{}
	ctx := &doctor.CheckContext{CityPath: cityPath, Verbose: verbose}

	// Core checks — always run.
	d.Register(&doctor.CityStructureCheck{})
	d.Register(&doctor.CityConfigCheck{})
	registerV2DeprecationChecks(d)
	d.Register(&doctor.ImplicitImportCacheCheck{})
	d.Register(&doctor.DeprecatedAttachmentFieldsCheck{})

	// Load config for deeper checks. If it fails, we still run the core
	// checks above (which will report the parse error).
	cfg, cfgErr := loadCityConfig(cityPath)
	if cfgErr == nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		if cityUsesBdStoreContract(cityPath) {
			d.Register(newDoltTopologyCheck(cityPath, cfg))
		}
		d.Register(doctor.NewConfigValidCheck(cfg))
		d.Register(doctor.NewConfigRefsCheck(cfg, cityPath))
		d.Register(doctor.NewBuiltinPackFamilyCheck(cfg, cityPath))
		d.Register(doctor.NewConfigSemanticsCheck(cfg, filepath.Join(cityPath, "city.toml")))
		d.Register(doctor.NewDurationRangeCheck(cfg))
		d.Register(doctor.NewSkillCollisionCheck(cfg, cityPath))
	}

	// System formulas/orders now ship via the core bootstrap pack; pack
	// materialization and the bootstrap collision checks cover what the
	// legacy SystemFormulasCheck used to verify.

	// Pack cache check (if config has remote packs).
	if cfgErr == nil && len(cfg.Packs) > 0 {
		d.Register(doctor.NewPackCacheCheck(cfg.Packs, cityPath))
	}

	sessionProvider := effectiveProviderName("")
	if cfgErr == nil {
		sessionProvider = effectiveProviderName(cfg.Session.Provider)
	}
	registerCoreBinaryChecks(d, sessionProvider, exec.LookPath)

	// Controller check + session checks (gated by controller state).
	controllerRunning := doctor.IsControllerRunning(cityPath)
	d.Register(doctor.NewControllerCheck(cityPath, controllerRunning))

	if cfgErr == nil && !controllerRunning {
		cityName := cfg.Workspace.Name
		if cityName == "" {
			cityName = filepath.Base(cityPath)
		}
		st := cfg.Workspace.SessionTemplate
		sp := newSessionProvider()

		d.Register(doctor.NewAgentSessionsCheck(cfg, cityName, st, sp))
		d.Register(doctor.NewZombieSessionsCheck(cfg, cityName, st, sp))
		d.Register(doctor.NewOrphanSessionsCheck(cfg, cityName, st, sp))
	}

	storeFactory := openStoreForCity(cityPath)

	// Data checks.
	if cfgErr == nil {
		d.Register(doctor.NewBeadsStoreCheck(cityPath, storeFactory))
		d.Register(&sessionModelDoctorCheck{cfg: cfg, cityPath: cityPath, newStore: storeFactory})
	}
	skipDolt := doctorSkipsDoltChecks(cityPath)
	d.Register(doctor.NewDoltServerCheck(cityPath, skipDolt))
	d.Register(&doctor.EventsLogCheck{})
	d.Register(doctor.NewEventLogSizeCheck())

	// Custom types check — city store.
	d.Register(doctor.NewCustomTypesCheck(cityPath, "city"))

	// Per-rig checks. Skip suspended rigs — opening their bead store
	// triggers bd auto-start of orphan Dolt servers (ga-wzk).
	if cfgErr == nil {
		for _, rig := range cfg.Rigs {
			if rig.Suspended {
				continue
			}
			d.Register(doctor.NewRigPathCheck(rig))
			d.Register(doctor.NewRigGitCheck(rig))
			d.Register(doctor.NewRigBeadsCheck(cityPath, rig, storeFactory))
			d.Register(doctor.NewRigDoltServerCheck(cityPath, rig, skipDolt))
			// Custom types check — rig store.
			d.Register(doctor.NewCustomTypesCheck(rig.Path, rig.Name))
		}
	}

	// Global rig index check + backfill.
	if cfgErr == nil {
		d.Register(&doctor.RigIndexCheck{
			FixFn: backfillRigIndex,
		})
	}

	// Worktree integrity check.
	d.Register(&doctor.WorktreeCheck{})

	// Pack doctor checks — scripts shipped with packs.
	if cfgErr == nil {
		for _, entry := range cfg.PackDoctors {
			d.Register(&doctor.PackScriptCheck{
				CheckName: entry.PackName + ":" + entry.Name,
				Script:    entry.RunScript,
				PackDir:   entry.PackDir,
				PackName:  entry.PackName,
			})
		}
	}

	report := d.Run(ctx, stdout, fix)
	doctor.PrintSummary(stdout, report)

	if report.Failed > 0 {
		return 1
	}
	return 0
}

func registerCoreBinaryChecks(d *doctor.Doctor, sessionProvider string, lookPath doctor.LookPathFunc) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	// Infrastructure checks — provider-aware core dependencies.
	// dolt/bd/flock are checked by pack doctor scripts (check-bd.sh,
	// check-dolt.sh) which also verify versions and service health.
	for _, dep := range coreBinaryDependencies(sessionProvider, "", coreBinaryDependencyOptions{}) {
		d.Register(newBinaryDependencyCheck(dep, lookPath))
	}
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

// backfillRigIndex registers all rigs from the given city in the global
// rig index and writes GT_ROOT to each rig's .beads/.env.
func backfillRigIndex(cityPath string) error {
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return err
	}

	resolveRigPaths(cityPath, cfg.Rigs)
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	for _, rig := range cfg.Rigs {
		rigPath := rig.Path

		if err := reg.RegisterRig(rigPath, rig.Name, cityPath); err != nil {
			// Non-fatal — may be a name conflict with another city's rig.
			continue
		}
		// Write GT_ROOT to .beads/.env.
		_ = writeBeadsEnvGTRoot(fsys.OSFS{}, rigPath, cityPath)
	}
	return nil
}

// openStoreForCity creates a beads.Store factory rooted in the given city.
// Doctor uses this so rig stores outside the city tree still inherit the
// canonical city topology instead of guessing from the rig path.
func openStoreForCity(cityPath string) func(string) (beads.Store, error) {
	return func(dirPath string) (beads.Store, error) {
		return openStoreAtForCity(dirPath, cityPath)
	}
}
