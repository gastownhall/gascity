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
	"github.com/gastownhall/gascity/internal/fsys"
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

func doBd(args []string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Extract --rig from args (since DisableFlagParsing prevents cobra from
	// parsing it). The remaining args are forwarded to bd.
	rigName, bdArgs := extractRigFlag(args)

	dir := resolveBdDir(cfg, cityPath, rigName, bdArgs)

	bdPath, err := exec.LookPath("bd")
	if err != nil {
		fmt.Fprintln(stderr, "gc bd: bd not found in PATH") //nolint:errcheck // best-effort stderr
		return 1
	}

	cmd := exec.Command(bdPath, bdArgs...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Build env: strip BEADS_DIR so bd discovers .beads/ from cwd,
	// and inject rig-level Dolt host/port when configured.
	env := removeEnvKey(os.Environ(), "BEADS_DIR")
	if dir != cityPath {
		for _, r := range cfg.Rigs {
			rp := r.Path
			if !filepath.IsAbs(rp) {
				rp = filepath.Join(cityPath, rp)
			}
			if filepath.Clean(rp) == filepath.Clean(dir) {
				if r.DoltHost != "" {
					env = append(env, "BEADS_DOLT_HOST="+r.DoltHost)
				}
				if r.DoltPort != "" {
					env = append(env, "BEADS_DOLT_PORT="+r.DoltPort)
				}
				break
			}
		}
	}
	cmd.Env = env

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

// extractRigFlag extracts --rig <name> from the argument list and returns
// the rig name and remaining args. Also checks the global rigFlag set by
// cobra's persistent flag parsing (for "gc --rig foo bd list" syntax).
func extractRigFlag(args []string) (string, []string) {
	var rigName string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--rig" && i+1 < len(args) {
			rigName = args[i+1]
			i++ // skip value
			continue
		}
		if strings.HasPrefix(args[i], "--rig=") {
			rigName = strings.TrimPrefix(args[i], "--rig=")
			continue
		}
		rest = append(rest, args[i])
	}
	// Fall back to the global persistent flag if set.
	if rigName == "" && rigFlag != "" {
		rigName = rigFlag
	}
	return rigName, rest
}

// resolveBdDir determines the working directory for a bd command.
// Priority: explicit rig name > bead prefix auto-detection > city root.
func resolveBdDir(cfg *config.City, cityPath, rigName string, args []string) string {
	if rigName != "" {
		for _, r := range cfg.Rigs {
			if strings.EqualFold(r.Name, rigName) {
				rp := r.Path
				if !filepath.IsAbs(rp) {
					rp = filepath.Join(cityPath, rp)
				}
				return rp
			}
		}
	}

	// Auto-detect from bead IDs in args.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if dir := rigDirForBead(cfg, arg); dir != "" {
			rp := dir
			if !filepath.IsAbs(rp) {
				rp = filepath.Join(cityPath, rp)
			}
			return rp
		}
	}

	return cityPath
}
