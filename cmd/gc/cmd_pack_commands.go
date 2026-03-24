package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// remotePacksCached reports whether all named remote packs declared in
// city.toml have been fetched (their cache directories exist). Returns
// true when there are no remote packs or the config cannot be parsed.
func remotePacksCached(cityPath string) bool {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return true // Can't parse → nothing to check.
	}
	for name := range cfg.Packs {
		cacheDir := filepath.Join(cityPath, citylayout.CachePacksRoot, name, ".git")
		if _, err := os.Stat(cacheDir); err != nil {
			return false
		}
	}
	return true
}

// registerPackCommands attempts to discover the city, load config, and
// register pack-provided CLI commands as top-level subcommands. Fails
// silently if not in a city or config fails to load — core commands
// always work.
func registerPackCommands(root *cobra.Command, stdout, stderr io.Writer) {
	cityPath, err := resolveCity()
	if err != nil {
		return
	}
	// Skip if remote packs haven't been fetched yet. LoadWithIncludes
	// would log confusing "not found, skipping" messages for uncached
	// named packs. The actual startup path (gc start) fetches them
	// before its own LoadWithIncludes.
	if !remotePacksCached(cityPath) {
		return
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return
	}

	allPackDirs := collectPackDirs(cfg)
	entries := config.LoadPackCommandEntries(fsys.OSFS{}, allPackDirs)
	if len(entries) == 0 {
		return
	}

	addPackCommandsToRoot(root, entries, cityPath, cfg.Workspace.Name, stdout, stderr)
}

// addPackCommandsToRoot groups command entries by pack name and registers
// them on the root command. Packs whose name shadows a core command are
// skipped with a warning to stderr.
func addPackCommandsToRoot(root *cobra.Command, entries []config.PackCommandInfo, cityPath, cityName string, stdout, stderr io.Writer) {
	core := coreCommandNames(root)

	// Group by pack name.
	grouped := make(map[string][]config.PackCommandInfo)
	for _, e := range entries {
		grouped[e.PackName] = append(grouped[e.PackName], e)
	}

	for packName, cmds := range grouped {
		if core[packName] {
			fmt.Fprintf(stderr, "gc: pack %q: name shadows core command, skipping\n", packName) //nolint:errcheck // best-effort stderr
			continue
		}
		nsCmd := newPackNamespaceCmd(packName, cmds, cityPath, cityName, stdout, stderr)
		root.AddCommand(nsCmd)
	}
}

// coreCommandNames returns the set of built-in command names that packs
// must not shadow.
func coreCommandNames(root *cobra.Command) map[string]bool {
	names := make(map[string]bool)
	for _, c := range root.Commands() {
		names[c.Name()] = true
		for _, alias := range c.Aliases {
			names[alias] = true
		}
	}
	// Also reserve "help" and "completion" which cobra may add.
	names["help"] = true
	names["completion"] = true
	return names
}

// newPackNamespaceCmd creates a cobra command group for a pack's commands.
// gc <pack> --help lists the pack's subcommands.
func newPackNamespaceCmd(packName string, cmds []config.PackCommandInfo, cityPath, cityName string, stdout, stderr io.Writer) *cobra.Command {
	ns := &cobra.Command{
		Use:   packName,
		Short: fmt.Sprintf("Commands from the %s pack", packName),
		// Don't run anything if no subcommand given — show help.
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}

	for _, info := range cmds {
		leaf := newPackLeafCmd(info, cityPath, cityName, stdout, stderr)
		ns.AddCommand(leaf)
	}

	return ns
}

// newPackLeafCmd creates a single pack command leaf. DisableFlagParsing
// is true so all args after the command name pass through to the script.
func newPackLeafCmd(info config.PackCommandInfo, cityPath, cityName string, stdout, stderr io.Writer) *cobra.Command {
	long := readLongDescription(info)
	cmd := &cobra.Command{
		Use:                info.Entry.Name,
		Short:              info.Entry.Description,
		Long:               long,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			code := runPackCommand(info, cityPath, cityName, args, stdin(), stdout, stderr)
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	return cmd
}

// readLongDescription reads the long description file from the pack dir.
// Returns empty string on any error.
func readLongDescription(info config.PackCommandInfo) string {
	if info.Entry.LongDescription == "" {
		return ""
	}
	path := filepath.Join(info.PackDir, info.Entry.LongDescription)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// stdin returns os.Stdin. Extracted for testability (tests can override).
var stdin = func() io.Reader { return os.Stdin }

// runPackCommand executes a pack command script with env vars, template
// expansion, and passthrough I/O. Returns the process exit code.
func runPackCommand(info config.PackCommandInfo, cityPath, cityName string, args []string, stdinR io.Reader, stdout, stderr io.Writer) int {
	scriptRel := expandScriptTemplate(info.Entry.Script, cityPath, cityName, info.PackDir)
	scriptPath := scriptRel
	if !filepath.IsAbs(scriptRel) {
		scriptPath = filepath.Join(info.PackDir, scriptRel)
	}

	cmd := exec.Command(scriptPath, args...)
	cmd.Stdin = stdinR
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), citylayout.PackRuntimeEnv(cityPath, info.PackName)...)
	cmd.Env = append(cmd.Env,
		"GC_PACK_DIR="+info.PackDir,
		"GC_PACK_NAME="+info.PackName,
		"GC_CITY_NAME="+cityName,
	)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gc %s %s: %v\n", info.PackName, info.Entry.Name, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// expandScriptTemplate expands Go text/template variables in the script
// path. On any error, returns the raw script string (graceful fallback).
func expandScriptTemplate(script, cityPath, cityName, packDir string) string {
	if !strings.Contains(script, "{{") {
		return script
	}
	ctx := SessionSetupContext{
		CityRoot:  cityPath,
		CityName:  cityName,
		ConfigDir: packDir,
	}
	tmpl, err := template.New("script").Parse(script)
	if err != nil {
		return script
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return script
	}
	return buf.String()
}

// tryPackCommandFallback is a lazy fallback for the root command's RunE.
// If eager discovery missed a pack command (e.g. config changed), try
// one more time. Returns true if a pack command was found and executed.
func tryPackCommandFallback(args []string, stdout, stderr io.Writer) bool {
	if len(args) == 0 {
		return false
	}

	cityPath, err := resolveCity()
	if err != nil {
		return false
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return false
	}

	allPackDirs := collectPackDirs(cfg)
	entries := config.LoadPackCommandEntries(fsys.OSFS{}, allPackDirs)

	packName := args[0]
	var matching []config.PackCommandInfo
	for _, e := range entries {
		if e.PackName == packName {
			matching = append(matching, e)
		}
	}
	if len(matching) == 0 {
		return false
	}

	// If just the pack name with no subcommand, list available commands.
	if len(args) < 2 {
		fmt.Fprintf(stdout, "Available commands for %s:\n", packName) //nolint:errcheck // best-effort stdout
		for _, m := range matching {
			fmt.Fprintf(stdout, "  %-20s %s\n", m.Entry.Name, m.Entry.Description) //nolint:errcheck // best-effort stdout
		}
		return true
	}

	cmdName := args[1]
	for _, m := range matching {
		if m.Entry.Name == cmdName {
			code := runPackCommand(m, cityPath, cfg.Workspace.Name, args[2:], stdin(), stdout, stderr)
			if code != 0 {
				os.Exit(code)
			}
			return true
		}
	}

	return false
}
