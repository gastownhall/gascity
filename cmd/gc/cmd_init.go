package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/spf13/cobra"
)

// wizardConfig carries the results of the interactive init wizard (or defaults
// for non-interactive paths). doInit uses it to decide which config to write.
type wizardConfig struct {
	interactive  bool   // true if the wizard ran with user interaction
	configName   string // "tutorial" or "custom"
	provider     string // "claude", "codex", "gemini", or "" if startCommand set
	startCommand string // custom start command (workspace-level)
}

// defaultWizardConfig returns a non-interactive wizardConfig that produces
// a single mayor agent with no provider.
func defaultWizardConfig() wizardConfig {
	return wizardConfig{configName: "tutorial"}
}

// isTerminal reports whether f is connected to a terminal (not a pipe or file).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readLine reads a single line from br and returns it trimmed.
// Returns empty string on EOF or error.
func readLine(br *bufio.Reader) string {
	line, err := br.ReadString('\n')
	if err != nil {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(line)
}

// runWizard runs the interactive init wizard, asking the user to choose a
// config template and a coding agent provider. If stdin is nil, returns
// defaultWizardConfig() (non-interactive).
func runWizard(stdin io.Reader, stdout io.Writer) wizardConfig {
	if stdin == nil {
		return defaultWizardConfig()
	}

	br := bufio.NewReader(stdin)

	fmt.Fprintln(stdout, "Welcome to Gas City SDK!")                                //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "")                                                        //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "Choose a config template:")                               //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  1. tutorial  — default coding agent (default)")         //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  2. custom    — empty workspace, configure it yourself") //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Template [1]: ")                                           //nolint:errcheck // best-effort stdout

	configChoice := readLine(br)
	configName := "tutorial"

	switch configChoice {
	case "", "1", "tutorial":
		configName = "tutorial"
	case "2", "custom":
		configName = "custom"
	default:
		fmt.Fprintf(stdout, "Unknown template %q, using tutorial.\n", configChoice) //nolint:errcheck // best-effort stdout
	}

	// Custom config → skip agent question, return minimal config.
	if configName == "custom" {
		return wizardConfig{
			interactive: true,
			configName:  "custom",
		}
	}

	// Build agent menu from built-in provider presets.
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()

	fmt.Fprintln(stdout, "")                          //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "Choose your coding agent:") //nolint:errcheck // best-effort stdout
	for i, name := range order {
		spec := builtins[name]
		suffix := ""
		if i == 0 {
			suffix = "  (default)"
		}
		fmt.Fprintf(stdout, "  %d. %s%s\n", i+1, spec.DisplayName, suffix) //nolint:errcheck // best-effort stdout
	}
	customNum := len(order) + 1
	fmt.Fprintf(stdout, "  %d. Custom command\n", customNum) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Agent [1]: ")                       //nolint:errcheck // best-effort stdout

	agentChoice := readLine(br)
	var provider, startCommand string

	provider = resolveAgentChoice(agentChoice, order, builtins, customNum)
	if provider == "" {
		// Custom command or invalid choice resolved to custom.
		switch {
		case agentChoice == fmt.Sprintf("%d", customNum) || agentChoice == "Custom command":
			fmt.Fprintf(stdout, "Enter start command: ") //nolint:errcheck // best-effort stdout
			startCommand = readLine(br)
		case agentChoice != "":
			fmt.Fprintf(stdout, "Unknown agent %q, using %s.\n", agentChoice, builtins[order[0]].DisplayName) //nolint:errcheck // best-effort stdout
			provider = order[0]
		default:
			provider = order[0]
		}
	}

	return wizardConfig{
		interactive:  true,
		configName:   "tutorial",
		provider:     provider,
		startCommand: startCommand,
	}
}

// resolveAgentChoice maps user input to a provider name. Input can be a
// number (1-based), a display name, or a provider key. Returns "" if the
// input doesn't match any built-in provider.
func resolveAgentChoice(input string, order []string, builtins map[string]config.ProviderSpec, _ int) string {
	if input == "" {
		return order[0]
	}
	// Check by number.
	n, err := strconv.Atoi(input)
	if err == nil && n >= 1 && n <= len(order) {
		return order[n-1]
	}
	// Check by display name or provider key.
	for _, name := range order {
		if input == builtins[name].DisplayName || input == name {
			return name
		}
	}
	return ""
}

func newInitCmd(stdout, stderr io.Writer) *cobra.Command {
	var fileFlag string
	var fromFlag string
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Initialize a new city",
		Long: `Create a new Gas City workspace in the given directory (or cwd).

Runs an interactive wizard to choose a config template and coding agent
provider. Creates the .gc/ runtime directory, default
prompts and formulas, and writes city.toml. Use --file to skip the
wizard and initialize from an existing TOML config file.`,
		Example: `  gc init
  gc init ~/my-city
  gc init --file examples/gastown.toml ~/bright-lights`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if fromFlag != "" {
				if cmdInitFromDir(fromFlag, args, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if fileFlag != "" {
				if cmdInitFromFile(fileFlag, args, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if cmdInit(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&fileFlag, "file", "", "path to a TOML file to use as city.toml")
	cmd.Flags().StringVar(&fromFlag, "from", "", "path to an example city directory to copy")
	cmd.MarkFlagsMutuallyExclusive("file", "from")
	return cmd
}

// cmdInit initializes a new city at the given path (or cwd if no path given).
// Runs the interactive wizard to choose a config template and provider.
// Creates .gc/ and city.toml. If the bead provider is "bd", also
// runs bd init.
func cmdInit(args []string, stdout, stderr io.Writer) int {
	var cityPath string
	if len(args) > 0 {
		var err error
		cityPath, err = filepath.Abs(args[0])
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		var err error
		cityPath, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	var wiz wizardConfig
	if isTerminal(os.Stdin) {
		wiz = runWizard(os.Stdin, stdout)
	} else {
		wiz = defaultWizardConfig()
	}
	cityName := filepath.Base(cityPath)
	if code := doInit(fsys.OSFS{}, cityPath, wiz, stdout, stderr); code != 0 {
		return code
	}
	// Materialize gc-beads-bd before initDirIfReady (may need probe).
	MaterializeBeadsBdScript(cityPath) //nolint:errcheck // best-effort; only needed for bd provider
	MaterializeBuiltinPacks(cityPath)  //nolint:errcheck // best-effort; only needed for bd provider
	prefix := config.DeriveBeadsPrefix(cityName)
	if _, err := initDirIfReady(cityPath, cityPath, prefix); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// cmdInitFromFile initializes a city using the --file flag (non-interactive).
// The flag value is a path to a TOML file that is copied as the city's city.toml.
func cmdInitFromFile(fileArg string, args []string, stdout, stderr io.Writer) int {
	var cityPath string
	if len(args) > 0 {
		var err error
		cityPath, err = filepath.Abs(args[0])
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		var err error
		cityPath, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	return cmdInitFromTOMLFile(fsys.OSFS{}, fileArg, cityPath, stdout, stderr)
}

// cmdInitFromTOMLFile initializes a city by copying a user-provided TOML
// file as city.toml. Creates .gc/, .gc/prompts/, and runs bead init.
func cmdInitFromTOMLFile(fs fsys.FS, tomlSrc, cityPath string, stdout, stderr io.Writer) int {
	// Validate the source file parses as a valid city config.
	data, err := os.ReadFile(tomlSrc)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: reading %q: %v\n", tomlSrc, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := config.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Override workspace name with the directory name.
	cityName := filepath.Base(cityPath)
	cfg.Workspace.Name = cityName

	// Re-marshal so the name is updated.
	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Create directory structure.
	gcDir := filepath.Join(cityPath, ".gc")
	if _, err := fs.Stat(gcDir); err == nil {
		fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := fs.MkdirAll(gcDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Install Claude Code hooks (settings.json).
	if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Write default prompts.
	if code := writeDefaultPrompts(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Write default formulas.
	if code := writeDefaultFormulas(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Materialize system formulas and resolve formula symlinks so bd finds them immediately after init.
	sysDir, _ := MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityPath)
	formulasInitDir := filepath.Join(cityPath, ".gc", "formulas")
	initLayers := []string{}
	if sysDir != "" {
		initLayers = append(initLayers, sysDir)
	}
	initLayers = append(initLayers, formulasInitDir)
	if rfErr := ResolveFormulas(cityPath, initLayers); rfErr != nil {
		fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort stderr
	}

	// Write city.toml.
	if err := fs.WriteFile(filepath.Join(cityPath, "city.toml"), content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Welcome to Gas City!\n")                                           //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Initialized city %q from %s.\n", cityName, filepath.Base(tomlSrc)) //nolint:errcheck // best-effort stdout
	MaterializeBeadsBdScript(cityPath)                                                      //nolint:errcheck // best-effort; only needed for bd provider
	MaterializeBuiltinPacks(cityPath)                                                       //nolint:errcheck // best-effort; only needed for bd provider
	prefix := config.DeriveBeadsPrefix(cityName)
	if _, err := initDirIfReady(cityPath, cityPath, prefix); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// doInit is the pure logic for "gc init". It creates the city directory
// structure (.gc/) and writes city.toml. When wiz.interactive is true,
// uses WizardCity (one agent + provider); otherwise uses DefaultCity (one
// mayor, no provider). Errors if .gc/ already exists. Accepts an injected FS
// for testability.
func doInit(fs fsys.FS, cityPath string, wiz wizardConfig, stdout, stderr io.Writer) int {
	gcDir := filepath.Join(cityPath, ".gc")

	// Check if already initialized.
	if _, err := fs.Stat(gcDir); err == nil {
		fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Create directory structure.
	if err := fs.MkdirAll(gcDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Install Claude Code hooks (settings.json).
	if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Write default prompt files.
	if code := writeDefaultPrompts(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Write default formula files.
	if code := writeDefaultFormulas(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Materialize system formulas and resolve formula symlinks so bd finds them immediately after init.
	sysDir, _ := MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityPath)
	formulasDir := filepath.Join(cityPath, ".gc", "formulas")
	initLayers := []string{}
	if sysDir != "" {
		initLayers = append(initLayers, sysDir)
	}
	initLayers = append(initLayers, formulasDir)
	if err := ResolveFormulas(cityPath, initLayers); err != nil {
		fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	// Write city.toml — wizard path gets one agent + provider/startCommand;
	// non-interactive path gets one mayor + no provider (backwards compat);
	// custom path gets one mayor + no provider (user configures manually).
	cityName := filepath.Base(cityPath)
	var cfg config.City
	switch {
	case !wiz.interactive, wiz.configName == "custom":
		cfg = config.DefaultCity(cityName)
	default:
		cfg = config.WizardCity(cityName, wiz.provider, wiz.startCommand)
	}
	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	tomlPath := filepath.Join(cityPath, "city.toml")
	if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if wiz.interactive {
		fmt.Fprintf(stdout, "Created %s config (Level 1) in %q.\n", wiz.configName, cityName) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintln(stdout, "Welcome to Gas City!")                                     //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Initialized city %q with default mayor agent.\n", cityName) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// installClaudeHooks writes Claude Code hook settings for the city.
// Delegates to hooks.Install which is idempotent (won't overwrite existing files).
func installClaudeHooks(fs fsys.FS, cityPath string, stderr io.Writer) int {
	if err := hooks.Install(fs, cityPath, cityPath, []string{"claude"}); err != nil {
		fmt.Fprintf(stderr, "gc init: installing claude hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// writeDefaultPrompts creates the .gc/prompts/ directory and writes all
// embedded prompt files used across the tutorials.
func writeDefaultPrompts(fs fsys.FS, cityPath string, stderr io.Writer) int {
	promptsDir := filepath.Join(cityPath, ".gc", "prompts")
	if err := fs.MkdirAll(promptsDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, name := range []string{"mayor.md", "worker.md", "one-shot.md", "loop.md", "loop-mail.md", "pool-worker.md"} {
		data, err := defaultPrompts.ReadFile("prompts/" + name)
		if err != nil {
			fmt.Fprintf(stderr, "gc init: reading embedded %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		dst := filepath.Join(promptsDir, name)
		if err := fs.WriteFile(dst, data, 0o644); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	return 0
}

// writeDefaultFormulas creates the .gc/formulas/ directory and writes
// embedded example formula files used across the tutorials.
func writeDefaultFormulas(fs fsys.FS, cityPath string, stderr io.Writer) int {
	formulasDir := filepath.Join(cityPath, ".gc", "formulas")
	if err := fs.MkdirAll(formulasDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, name := range []string{"pancakes.formula.toml", "cooking.formula.toml", "mol-do-work.formula.toml"} {
		data, err := defaultFormulas.ReadFile("formulas/" + name)
		if err != nil {
			fmt.Fprintf(stderr, "gc init: reading embedded %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		dst := filepath.Join(formulasDir, name)
		if err := fs.WriteFile(dst, data, 0o644); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	return 0
}

// initFromSkip returns true for files and directories that should be excluded
// when copying a city template directory via --from. Skips .gc/ runtime state
// (but preserves .gc/prompts/, which is user-editable config) and Go test files.
func initFromSkip(relPath string, isDir bool) bool {
	top, rest, _ := strings.Cut(relPath, string(filepath.Separator))
	if top == ".gc" {
		// Let the walker enter .gc/ so it can reach .gc/prompts/.
		if rest == "" {
			return false
		}
		// Allow .gc/prompts/ through — it's user-editable config, not runtime state.
		sub, _, _ := strings.Cut(rest, string(filepath.Separator))
		if sub == "prompts" {
			return false
		}
		// Skip all other .gc/ contents (runtime state).
		return true
	}
	if !isDir && strings.HasSuffix(filepath.Base(relPath), "_test.go") {
		return true
	}
	return false
}

// cmdInitFromDir initializes a city by copying an example directory.
// Resolves source and target paths, validates, then delegates to doInitFromDir.
func cmdInitFromDir(fromDir string, args []string, stdout, stderr io.Writer) int {
	var cityPath string
	if len(args) > 0 {
		var err error
		cityPath, err = filepath.Abs(args[0])
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		var err error
		cityPath, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	srcDir, err := filepath.Abs(fromDir)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	return doInitFromDir(srcDir, cityPath, stdout, stderr)
}

// doInitFromDir copies an example city directory to a new city path,
// updates workspace.name, creates .gc/, and installs hooks.
func doInitFromDir(srcDir, cityPath string, stdout, stderr io.Writer) int {
	fs := fsys.OSFS{}
	// Validate source has city.toml.
	srcToml := filepath.Join(srcDir, "city.toml")
	if _, err := os.Stat(srcToml); err != nil {
		fmt.Fprintf(stderr, "gc init --from: source %q has no city.toml\n", srcDir) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Check target not already initialized.
	gcDir := filepath.Join(cityPath, ".gc")
	if _, err := fs.Stat(gcDir); err == nil {
		fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Create target directory if needed.
	if err := fs.MkdirAll(cityPath, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Copy directory tree (skip .gc/ and *_test.go).
	if err := overlay.CopyDirWithSkip(srcDir, cityPath, initFromSkip, stderr); err != nil {
		fmt.Fprintf(stderr, "gc init --from: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Parse copied city.toml and override workspace.name.
	cityName := filepath.Base(cityPath)
	copiedToml := filepath.Join(cityPath, "city.toml")
	data, err := os.ReadFile(copiedToml)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: reading copied city.toml: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := config.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg.Workspace.Name = cityName
	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := fs.WriteFile(copiedToml, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Create .gc/ dir.
	if err := fs.MkdirAll(gcDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Install Claude Code hooks.
	if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Resolve formulas from pack layers.
	expandedCfg, _, loadErr := config.LoadWithIncludes(fsys.OSFS{}, copiedToml)
	if loadErr == nil && len(expandedCfg.FormulaLayers.City) > 0 {
		formulasDir := filepath.Join(cityPath, ".gc", "formulas")
		if rfErr := ResolveFormulas(cityPath, expandedCfg.FormulaLayers.City); rfErr != nil {
			fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort stderr
		}
		_ = formulasDir // used indirectly by ResolveFormulas
	}

	fmt.Fprintln(stdout, "Welcome to Gas City!")                                           //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Initialized city %q from %s.\n", cityName, filepath.Base(srcDir)) //nolint:errcheck // best-effort stdout

	MaterializeBeadsBdScript(cityPath) //nolint:errcheck // best-effort; only needed for bd provider
	MaterializeBuiltinPacks(cityPath)  //nolint:errcheck // best-effort; only needed for bd provider
	prefix := config.DeriveBeadsPrefix(cityName)
	if _, err := initDirIfReady(cityPath, cityPath, prefix); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
