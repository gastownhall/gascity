package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func newBdCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bd [bd-args...]",
		Short: "Run bd in the correct rig directory",
		Long: `Run a bd command routed to the correct rig directory.

When beads belong to a rig (not the city root), bd must run from the
rig directory to find the correct .beads database. This command resolves
the rig automatically from the --rig flag or by detecting the bead prefix
in the arguments.

All arguments after "gc bd" are forwarded to bd unchanged.`,
		Example: `  gc bd --rig my-project list
  gc bd --rig my-project create "New task"
  gc bd show my-project-abc          # auto-detects rig from bead prefix
  gc bd list --rig my-project -s open`,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if doBd(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

var bdBeadExists = func(cityPath string, target execStoreTarget, beadID string) bool {
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		return false
	}
	bead, err := store.Get(beadID)
	return err == nil && strings.TrimSpace(bead.ID) != ""
}

func bdCommandEnv(cityPath string, cfg *config.City, target execStoreTarget) []string {
	var overrides map[string]string
	if target.ScopeKind == "rig" {
		overrides = bdRuntimeEnvForRig(cityPath, cfg, target.ScopeRoot)
	} else {
		overrides = bdRuntimeEnv(cityPath)
		overrides["GC_RIG"] = ""
		overrides["GC_RIG_ROOT"] = ""
		overrides["BEADS_DIR"] = filepath.Join(target.ScopeRoot, ".beads")
	}
	overrides["GC_STORE_ROOT"] = target.ScopeRoot
	overrides["GC_STORE_SCOPE"] = target.ScopeKind
	overrides["GC_BEADS_PREFIX"] = target.Prefix
	return mergeRuntimeEnv(os.Environ(), overrides)
}

func warnExternalBdOverrideDrift(stderr io.Writer, cityPath string, target execStoreTarget) {
	resolved, ok, err := canonicalScopeDoltTarget(cityPath, target.ScopeRoot)
	if err != nil || !ok || !resolved.External {
		return
	}
	var drift []string
	if host := strings.TrimSpace(os.Getenv("GC_DOLT_HOST")); host != "" && host != strings.TrimSpace(resolved.Host) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_HOST=%s (canonical %s)", host, strings.TrimSpace(resolved.Host)))
	}
	if port := strings.TrimSpace(os.Getenv("GC_DOLT_PORT")); port != "" && port != strings.TrimSpace(resolved.Port) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_PORT=%s (canonical %s)", port, strings.TrimSpace(resolved.Port)))
	}
	if len(drift) == 0 {
		return
	}
	_, _ = fmt.Fprintf(stderr, "gc bd: warning: ignoring ambient Dolt host/port override for external target: %s\n", strings.Join(drift, ", "))
}

func doBd(args []string, stdout, stderr io.Writer) int {
	cityName, rigName, bdArgs := extractBdScopeFlags(args)

	cityPath, err := resolveBdCity(cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Use the full config load path (includes pack expansion + site
	// binding overlay) so migrated rigs (path only in .gc/site.toml)
	// resolve to their bound path. A raw config.Load here would make
	// every already-migrated rig look unbound and fail the new guard
	// in resolveBdScopeTarget / bdRigScopeTarget.
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !cityUsesBdStoreContract(cityPath) {
		fmt.Fprintln(stderr, "gc bd: only supported for bd-backed beads providers") //nolint:errcheck // best-effort stderr
		return 1
	}

	target, err := resolveBdScopeTarget(cfg, cityPath, rigName, bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	warnExternalBdOverrideDrift(stderr, cityPath, target)

	// Scope guard: prevent mutating operations from accidentally targeting
	// the city-root store when rigs are configured.
	if err := validateBdScope(cfg, cityPath, target.ScopeRoot, rigName, bdArgs); err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	bdPath, err := exec.LookPath("bd")
	if err != nil {
		fmt.Fprintln(stderr, "gc bd: bd not found in PATH") //nolint:errcheck // best-effort stderr
		return 1
	}

	cmd := exec.Command(bdPath, bdArgs...)
	cmd.Dir = target.ScopeRoot
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = bdCommandEnv(cityPath, cfg, target)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func resolveBdCity(cityName string) (string, error) {
	if strings.TrimSpace(cityName) != "" {
		return validateCityPath(cityName)
	}
	return resolveCity()
}

// extractBdScopeFlags extracts gc-owned --city/--rig flags from the raw
// argument list and returns the requested city, rig, and remaining bd args.
// It also falls back to cobra's persistent globals for "gc --city X --rig Y bd".
func extractBdScopeFlags(args []string) (string, string, []string) {
	var cityName string
	var rigName string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--city" && i+1 < len(args):
			cityName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--city="):
			cityName = strings.TrimPrefix(args[i], "--city=")
			continue
		case args[i] == "--rig" && i+1 < len(args):
			rigName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--rig="):
			rigName = strings.TrimPrefix(args[i], "--rig=")
			continue
		}
		rest = append(rest, args[i])
	}
	if cityName == "" && cityFlag != "" {
		cityName = cityFlag
	}
	if rigName == "" && rigFlag != "" {
		rigName = rigFlag
	}
	return cityName, rigName, rest
}

// extractRigFlag extracts --rig <name> from the argument list and returns
// the rig name and remaining args. Also checks the global rigFlag set by
// cobra's persistent flag parsing (for "gc --rig foo bd list" syntax).
func extractRigFlag(args []string) (string, []string) {
	_, rigName, rest := extractBdScopeFlags(args)
	return rigName, rest
}

// resolveBdScopeTarget determines the canonical scope root for a bd command.
// Priority: explicit rig name > bead prefix auto-detection > enclosing rig > city root.
func resolveBdScopeTarget(cfg *config.City, cityPath, rigName string, args []string) (execStoreTarget, error) {
	resolveRigPaths(cityPath, cfg.Rigs)
	if rigName != "" {
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			return execStoreTarget{}, fmt.Errorf("rig %q not found", rigName)
		}
		if strings.TrimSpace(rig.Path) == "" {
			return execStoreTarget{}, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before scoping bd commands", rig.Name, rig.Name)
		}
		return bdRigScopeTarget(cityPath, rig), nil
	}

	// Auto-detect from bead IDs in args, but only accept candidates that
	// actually exist in the resolved rig store.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if rig, ok := bdRigForArg(cfg, arg); ok {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			target := bdRigScopeTarget(cityPath, rig)
			if bdBeadExists(cityPath, target, arg) {
				return target, nil
			}
		}
	}

	if rig, ok, err := bdRigFromCwd(cfg, cityPath); err != nil {
		return execStoreTarget{}, err
	} else if ok {
		return bdRigScopeTarget(cityPath, rig), nil
	}

	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, cityPath),
		ScopeKind: "city",
		Prefix:    config.EffectiveHQPrefix(cfg),
	}, nil
}

// bdMutatingSubcommands lists bd subcommands that write to the bead store.
// Used by the scope guard to prevent accidental writes to the wrong store.
var bdMutatingSubcommands = map[string]bool{
	"assign":      true,
	"close":       true,
	"comment":     true,
	"create":      true,
	"create-form": true,
	"delete":      true,
	"edit":        true,
	"gate":        true,
	"label":       true,
	"link":        true,
	"merge-slot":  true,
	"note":        true,
	"priority":    true,
	"promote":     true,
	"q":           true,
	"reopen":      true,
	"rename":      true,
	"set-state":   true,
	"defer":       true,
	"undefer":     true,
}

// bdSubcommand extracts the first non-flag argument from bd args, which
// is the bd subcommand (e.g., "create", "list", "show").
func bdSubcommand(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

// errRigScopeRequired is returned when a mutating bd operation would fall
// back to the city-root store but rigs are configured, indicating the user
// likely intended a rig-scoped store.
var errRigScopeRequired = fmt.Errorf("rigs are configured but no --rig flag provided; specify --rig <name> to target the correct bead store")

// validateBdScope checks whether a bd command is safe to run at the resolved
// directory. When the resolved dir is the city root (fallback), the subcommand
// is mutating, and rigs are configured, returns an error to prevent accidental
// writes to the wrong store.
//
// The guard does NOT fire when:
//   - An explicit --rig flag was provided (rigName is non-empty)
//   - The resolved dir is a rig directory (not the city root fallback)
//   - The subcommand is read-only (list, show, search, etc.)
//   - No rigs are configured (single-store city)
func validateBdScope(cfg *config.City, cityPath, resolvedDir, rigName string, args []string) error {
	// Explicit rig flag was given — user knows what they're doing.
	if rigName != "" {
		return nil
	}
	// Resolved to a rig directory, not the city root fallback.
	if filepath.Clean(resolvedDir) != filepath.Clean(cityPath) {
		return nil
	}
	// No rigs configured — single-store city, no ambiguity.
	if len(cfg.Rigs) == 0 {
		return nil
	}
	// Read-only subcommand — safe at any scope.
	sub := bdSubcommand(args)
	if !bdMutatingSubcommands[sub] {
		return nil
	}
	return fmt.Errorf("gc bd %s: %w", sub, errRigScopeRequired)
}



func bdRigForArg(cfg *config.City, arg string) (config.Rig, bool) {
	if prefix := beadPrefix(arg); prefix != "" {
		return findRigByPrefix(cfg, prefix)
	}
	return config.Rig{}, false
}

func bdRigFromCwd(cfg *config.City, cityPath string) (config.Rig, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.Rig{}, false, nil
	}
	return resolveRigForDir(cfg, cityPath, cwd)
}

func bdRigScopeTarget(cityPath string, rig config.Rig) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, rig.Path),
		ScopeKind: "rig",
		Prefix:    rig.EffectivePrefix(),
		RigName:   rig.Name,
	}
}
