package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/citylayout"
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

Checks city structure, config validity, binary dependencies (tmux, git,
bd, dolt), controller status, agent sessions, zombie/orphan sessions,
bead stores, Dolt server health, event log integrity, and per-rig
health. Use --fix to attempt automatic repairs.`,
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

	// Load config for deeper checks. If it fails, we still run the core
	// checks above (which will report the parse error).
	cfg, cfgErr := loadCityConfig(cityPath)
	if cfgErr == nil {
		// Register city Dolt config so bdRuntimeEnv can resolve the
		// correct port for bead store checks (mirrors startBeadsLifecycle).
		if cfg.Dolt.Host != "" || cfg.Dolt.Port != 0 {
			cityDoltConfigs.Store(cityPath, cfg.Dolt)
			defer cityDoltConfigs.Delete(cityPath)
		}
		d.Register(doctor.NewConfigValidCheck(cfg))
		d.Register(doctor.NewConfigRefsCheck(cfg, cityPath))
		d.Register(doctor.NewBuiltinPackFamilyCheck(cfg, cityPath))
		d.Register(doctor.NewConfigSemanticsCheck(cfg, filepath.Join(cityPath, "city.toml")))
		d.Register(doctor.NewDurationRangeCheck(cfg))
	}

	// System formulas check.
	expected := ListEmbeddedSystemFormulas(systemFormulasFS, "system_formulas")
	if len(expected) > 0 {
		expectedContent := make(map[string][]byte)
		for _, rel := range expected {
			data, err := fs.ReadFile(systemFormulasFS, "system_formulas/"+rel)
			if err == nil {
				expectedContent[rel] = data
			}
		}
		d.Register(&doctor.SystemFormulasCheck{
			CityPath:        cityPath,
			Expected:        expected,
			ExpectedContent: expectedContent,
			FixFn: func() error {
				_, err := MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityPath)
				return err
			},
		})
	}

	// Pack cache check (if config has remote packs).
	if cfgErr == nil && len(cfg.Packs) > 0 {
		d.Register(doctor.NewPackCacheCheck(cfg.Packs, cityPath))
	}

	// Infrastructure checks — universal dependencies.
	// dolt/bd/flock are checked by pack doctor scripts (check-bd.sh,
	// check-dolt.sh) which also verify versions and service health.
	d.Register(doctor.NewBinaryCheck("tmux", "", exec.LookPath))
	d.Register(doctor.NewBinaryCheck("git", "", exec.LookPath))
	d.Register(doctor.NewBinaryCheck("jq", "", exec.LookPath))
	d.Register(doctor.NewBinaryCheck("pgrep", "", exec.LookPath))
	d.Register(doctor.NewBinaryCheck("lsof", "", exec.LookPath))

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

	// Data checks.
	if cfgErr == nil {
		d.Register(doctor.NewBeadsStoreCheck(cityPath, openStore))
		d.Register(&sessionModelDoctorCheck{cfg: cfg, cityPath: cityPath, newStore: openStore})
	}
	skipDolt := rawBeadsProvider(cityPath) != "bd" || os.Getenv("GC_DOLT") == "skip"
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
			d.Register(doctor.NewRigBeadsCheck(rig, openStore))
			// Custom types check — rig store.
			rigPath := rig.Path
			if !filepath.IsAbs(rigPath) {
				rigPath = filepath.Join(cityPath, rigPath)
			}
			d.Register(doctor.NewCustomTypesCheck(rigPath, rig.Name))
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

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	for _, rig := range cfg.Rigs {
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		rigPath = filepath.Clean(rigPath)

		if err := reg.RegisterRig(rigPath, rig.Name, cityPath); err != nil {
			// Non-fatal — may be a name conflict with another city's rig.
			continue
		}
		// Write GT_ROOT to .beads/.env.
		_ = writeBeadsEnvGTRoot(fsys.OSFS{}, rigPath, cityPath)
	}
	return nil
}

// openStore creates a beads.Store from a directory path. Used as a factory
// for doctor checks that need to verify store accessibility.
func openStore(dirPath string) (beads.Store, error) {
	cityPath := cityForStoreDir(dirPath)
	prov := rawBeadsProvider(cityPath)
	switch {
	case strings.HasPrefix(prov, "exec:"):
		store := beadsexec.NewStore(strings.TrimPrefix(prov, "exec:"))
		store.SetEnv(citylayout.CityRuntimeEnvMap(cityPath))
		return store, nil
	case prov == "file":
		return beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, ".gc", "beads.json"))
	default: // "bd"
		if _, err := exec.LookPath("bd"); err != nil {
			return nil, fmt.Errorf("bd not found in PATH")
		}
		return bdStoreForCity(dirPath, cityPath), nil
	}
}
