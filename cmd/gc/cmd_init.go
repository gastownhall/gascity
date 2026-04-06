package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/spf13/cobra"
)

// wizardConfig carries the results of the interactive init wizard (or defaults
// for non-interactive paths). doInit uses it to decide which config to write.
type wizardConfig struct {
	interactive      bool   // true if the wizard ran with user interaction
	configName       string // "tutorial", "gastown", or "custom"
	provider         string // built-in provider key, or "" if startCommand set
	startCommand     string // custom start command (workspace-level)
	bootstrapProfile string // hosted bootstrap profile, or "" for local defaults
}

// defaultWizardConfig returns a non-interactive wizardConfig that produces
// a single mayor agent with no provider.
func defaultWizardConfig() wizardConfig {
	return wizardConfig{configName: "tutorial"}
}

func canBootstrapExistingCity(wiz wizardConfig) bool {
	return wiz == defaultWizardConfig()
}

const (
	bootstrapProfileK8sCell          = "k8s-cell"
	bootstrapProfileSingleHostCompat = "single-host-compat"
)

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
	fmt.Fprintln(stdout, "  2. gastown   — multi-agent orchestration pack")         //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  3. custom    — empty workspace, configure it yourself") //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Template [1]: ")                                           //nolint:errcheck // best-effort stdout

	configChoice := readLine(br)
	configName := "tutorial"

	switch configChoice {
	case "", "1", "tutorial":
		configName = "tutorial"
	case "2", "gastown":
		configName = "gastown"
	case "3", "custom":
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
		configName:   configName,
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

const initProgressSteps = 8

func logInitProgress(stdout io.Writer, step int, msg string) {
	if stdout == nil {
		return
	}
	fmt.Fprintf(stdout, "[%d/%d] %s\n", step, initProgressSteps, msg) //nolint:errcheck // best-effort stdout
}

func newInitCmd(stdout, stderr io.Writer) *cobra.Command {
	var fileFlag string
	var fromFlag string
	var nameFlag string
	var providerFlag string
	var bootstrapProfileFlag string
	var skipProviderReadiness bool
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Initialize a new city",
		Long: `Create a new Gas City workspace in the given directory (or cwd).

Runs an interactive wizard to choose a config template and coding agent
provider. Creates the .gc/ runtime directory, default
prompts and formulas, and writes city.toml. Use --provider to create the
default mayor city non-interactively, or --file to initialize from an
existing TOML config file.`,
		Example: `  gc init
  gc init ~/my-city
  gc init --provider codex ~/my-city
  gc init --provider codex --bootstrap-profile k8s-cell /city
  gc init --from ~/elan --name elan /city
  gc init --file examples/gastown.toml ~/bright-lights`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if nameFlag != "" && fromFlag == "" && fileFlag == "" {
				return fmt.Errorf("--name requires --from or --file")
			}
			if fromFlag != "" {
				if cmdInitFromDirWithOptions(fromFlag, args, nameFlag, stdout, stderr, skipProviderReadiness) != 0 {
					return errExit
				}
				return nil
			}
			if fileFlag != "" {
				if cmdInitFromFileWithOptions(fileFlag, args, nameFlag, stdout, stderr, skipProviderReadiness) != 0 {
					return errExit
				}
				return nil
			}
			if cmdInitWithOptions(args, providerFlag, bootstrapProfileFlag, stdout, stderr, skipProviderReadiness) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&fileFlag, "file", "", "path to a TOML file to use as city.toml")
	cmd.Flags().StringVar(&fromFlag, "from", "", "path to an example city directory to copy")
	cmd.Flags().StringVar(&nameFlag, "name", "", "workspace name (default: target directory basename)")
	cmd.Flags().StringVar(&providerFlag, "provider", "", "built-in workspace provider to use for the default mayor config")
	cmd.Flags().StringVar(&bootstrapProfileFlag, "bootstrap-profile", "", "bootstrap profile to apply for hosted/container defaults")
	cmd.Flags().BoolVar(&skipProviderReadiness, "skip-provider-readiness", false, "skip provider login/readiness checks during init and continue startup")
	cmd.MarkFlagsMutuallyExclusive("file", "from")
	cmd.MarkFlagsMutuallyExclusive("provider", "file")
	cmd.MarkFlagsMutuallyExclusive("provider", "from")
	cmd.MarkFlagsMutuallyExclusive("bootstrap-profile", "file")
	cmd.MarkFlagsMutuallyExclusive("bootstrap-profile", "from")
	return cmd
}

// cmdInit initializes a new city at the given path (or cwd if no path given).
// Runs the interactive wizard to choose a config template and provider.
// Creates the runtime scaffold and city.toml. If the bead provider is "bd", also
// runs bd init.
func cmdInit(args []string, providerFlag, bootstrapProfileFlag string, stdout, stderr io.Writer) int {
	return cmdInitWithOptions(args, providerFlag, bootstrapProfileFlag, stdout, stderr, false)
}

func cmdInitWithOptions(args []string, providerFlag, bootstrapProfileFlag string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
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
	if handled, code := resumeExistingInitIfPossible(fsys.OSFS{}, cityPath, stdout, stderr, "gc init", true, skipProviderReadiness); handled {
		return code
	}
	var wiz wizardConfig
	switch {
	case providerFlag != "" || bootstrapProfileFlag != "":
		var err error
		wiz, err = initWizardConfig(providerFlag, bootstrapProfileFlag)
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	case isTerminal(os.Stdin):
		wiz = runWizard(os.Stdin, stdout)
		maybePrintWizardProviderGuidance(wiz, stdout)
	default:
		wiz = defaultWizardConfig()
	}
	if code := doInit(fsys.OSFS{}, cityPath, wiz, stdout, stderr); code != 0 {
		return code
	}
	return finalizeInit(cityPath, stdout, stderr, initFinalizeOptions{
		skipProviderReadiness: skipProviderReadiness,
		showProgress:          true,
		commandName:           "gc init",
	})
}

func resumeExistingInitIfPossible(fs fsys.FS, cityPath string, stdout, stderr io.Writer, commandName string, showProgress bool, skipProviderReadiness bool) (bool, int) {
	if !cityCanResumeInitFS(fs, cityPath) {
		return false, 0
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "City %q already exists; reusing existing configuration and resuming startup checks.\n", filepath.Base(cityPath)) //nolint:errcheck // best-effort stdout
	}
	return true, finalizeInit(cityPath, stdout, stderr, initFinalizeOptions{
		skipProviderReadiness: skipProviderReadiness,
		showProgress:          showProgress,
		commandName:           commandName,
	})
}

func initWizardConfig(providerFlag, bootstrapProfileFlag string) (wizardConfig, error) {
	provider, err := normalizeInitProvider(providerFlag)
	if err != nil {
		return wizardConfig{}, err
	}
	bootstrapProfile, err := normalizeBootstrapProfile(bootstrapProfileFlag)
	if err != nil {
		return wizardConfig{}, err
	}
	return wizardConfig{
		configName:       "tutorial",
		provider:         provider,
		bootstrapProfile: bootstrapProfile,
	}, nil
}

func normalizeInitProvider(provider string) (string, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "", nil
	}
	if _, ok := config.BuiltinProviders()[provider]; ok {
		return provider, nil
	}
	return "", fmt.Errorf("unknown provider %q (expected one of: %s)", provider, strings.Join(config.BuiltinProviderOrder(), ", "))
}

func normalizeBootstrapProfile(profile string) (string, error) {
	switch strings.TrimSpace(profile) {
	case "":
		return "", nil
	case bootstrapProfileK8sCell, "kubernetes", "kubernetes-cell":
		return bootstrapProfileK8sCell, nil
	case bootstrapProfileSingleHostCompat:
		return bootstrapProfileSingleHostCompat, nil
	default:
		return "", fmt.Errorf("unknown bootstrap profile %q", profile)
	}
}

func cmdInitFromFileWithOptions(fileArg string, args []string, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
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

	return cmdInitFromTOMLFileWithOptions(fsys.OSFS{}, fileArg, cityPath, nameOverride, stdout, stderr, skipProviderReadiness)
}

// cmdInitFromTOMLFile initializes a city by copying a user-provided TOML
// file as city.toml. Creates the runtime scaffold, visible roots, and runs bead init.
func cmdInitFromTOMLFile(fs fsys.FS, tomlSrc, cityPath string, stdout, stderr io.Writer) int {
	return cmdInitFromTOMLFileWithOptions(fs, tomlSrc, cityPath, "", stdout, stderr, false)
}

func cmdInitFromTOMLFileWithOptions(fs fsys.FS, tomlSrc, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
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

	// --file creates a new city from a template; default to target dir name.
	cityName := resolveCityName(nameOverride, "", cityPath)
	cfg.Workspace.Name = cityName

	// Re-marshal so the name is updated.
	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Create directory structure.
	if cityAlreadyInitializedFS(fs, cityPath) {
		fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureCityScaffoldFS(fs, cityPath); err != nil {
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

	// Materialize system formulas into formulas/ and resolve symlinks so bd finds them immediately after init.
	MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityPath) //nolint:errcheck // best-effort
	formulasInitDir := filepath.Join(cityPath, citylayout.FormulasRoot)
	if rfErr := ResolveFormulas(cityPath, []string{formulasInitDir}); rfErr != nil {
		fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort stderr
	}

	// Write city.toml.
	if err := fs.WriteFile(filepath.Join(cityPath, "city.toml"), content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Welcome to Gas City!\n")                                           //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Initialized city %q from %s.\n", cityName, filepath.Base(tomlSrc)) //nolint:errcheck // best-effort stdout
	return finalizeInit(cityPath, stdout, stderr, initFinalizeOptions{
		skipProviderReadiness: skipProviderReadiness,
		commandName:           "gc init",
	})
}

// doInit is the pure logic for "gc init". It creates the city directory
// structure and writes city.toml. Tutorial configs use WizardCity
// when a provider or start command is supplied; otherwise init writes the
// default mayor-only city. Errors if the runtime scaffold already exists. Accepts an
// injected FS for testability.
func doInit(fs fsys.FS, cityPath string, wiz wizardConfig, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, citylayout.CityConfigFile)
	if cityHasScaffoldFS(fs, cityPath) {
		fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
		return 1
	}
	if _, err := fs.Stat(tomlPath); err == nil {
		if !canBootstrapExistingCity(wiz) {
			fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := ensureCityScaffoldFS(fs, cityPath); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
			return code
		}
		fmt.Fprintln(stdout, "Welcome to Gas City!")                                             //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Bootstrapped city %q runtime scaffold.\n", filepath.Base(cityPath)) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Create directory structure.
	logInitProgress(stdout, 1, "Creating runtime scaffold")
	if err := ensureCityScaffoldFS(fs, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Install Claude Code hooks (settings.json).
	logInitProgress(stdout, 2, "Installing hooks (Claude Code)")
	if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Write default prompt files.
	logInitProgress(stdout, 3, "Writing default prompts")
	if code := writeDefaultPrompts(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Write default formula files.
	logInitProgress(stdout, 4, "Writing default formulas")
	if code := writeDefaultFormulas(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Materialize system formulas into formulas/ and resolve symlinks so bd finds them immediately after init.
	MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityPath) //nolint:errcheck // best-effort
	formulasDir := filepath.Join(cityPath, citylayout.FormulasRoot)
	if err := ResolveFormulas(cityPath, []string{formulasDir}); err != nil {
		fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	// Write city.toml — wizard path gets one agent + provider/startCommand;
	// --provider path gets the same city shape non-interactively;
	// custom path gets one mayor + no provider (user configures manually).
	cityName := filepath.Base(cityPath)
	var cfg config.City
	switch {
	case wiz.configName == "custom":
		cfg = config.DefaultCity(cityName)
	case wiz.configName == "gastown":
		cfg = config.GastownCity(cityName, wiz.provider, wiz.startCommand)
	case wiz.provider != "" || wiz.startCommand != "":
		cfg = config.WizardCity(cityName, wiz.provider, wiz.startCommand)
	default:
		cfg = config.DefaultCity(cityName)
	}
	applyBootstrapProfile(&cfg, wiz.bootstrapProfile)
	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logInitProgress(stdout, 5, "Writing city configuration")
	if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	switch {
	case wiz.interactive:
		fmt.Fprintf(stdout, "Created %s config (Level 1) in %q.\n", wiz.configName, cityName) //nolint:errcheck // best-effort stdout
	case wiz.provider != "":
		fmt.Fprintln(stdout, "Welcome to Gas City!")                                                   //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Initialized city %q with default provider %q.\n", cityName, wiz.provider) //nolint:errcheck // best-effort stdout
	default:
		fmt.Fprintln(stdout, "Welcome to Gas City!")                                     //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Initialized city %q with default mayor agent.\n", cityName) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func applyBootstrapProfile(cfg *config.City, profile string) {
	if profile == bootstrapProfileK8sCell {
		cfg.API.Port = config.DefaultAPIPort
		cfg.API.Bind = "0.0.0.0"
		cfg.API.AllowMutations = true
	}
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

// writeDefaultPrompts creates the prompts/ directory and writes all
// embedded prompt files. Walks the embed.FS dynamically — no hardcoded
// filename list. Uses the injected FS for I/O (testability with mock FS).
func writeDefaultPrompts(fs fsys.FS, cityPath string, stderr io.Writer) int {
	promptsDir := filepath.Join(cityPath, citylayout.PromptsRoot)
	if err := fs.MkdirAll(promptsDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	entries, err := defaultPrompts.ReadDir("prompts")
	if err != nil {
		fmt.Fprintf(stderr, "gc init: reading embedded prompts: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := defaultPrompts.ReadFile("prompts/" + e.Name())
		if err != nil {
			fmt.Fprintf(stderr, "gc init: reading embedded %s: %v\n", e.Name(), err) //nolint:errcheck // best-effort stderr
			return 1
		}
		dst := filepath.Join(promptsDir, e.Name())
		if err := fs.WriteFile(dst, data, 0o644); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	return 0
}

// initFromSkip returns true for files and directories that should be excluded
// when copying a city template directory via --from. Skips .gc/ runtime state.
func initFromSkip(relPath string, isDir bool) bool {
	top, _, _ := strings.Cut(relPath, string(filepath.Separator))
	if top == ".gc" {
		return true
	}
	if !isDir && strings.HasSuffix(filepath.Base(relPath), "_test.go") {
		return true
	}
	return false
}

// resolveCityName returns the workspace name to use during init.
// Priority: explicit --name flag > source config name > target directory basename.
func resolveCityName(nameOverride, sourceName, cityPath string) string {
	if nameOverride != "" {
		return nameOverride
	}
	if sourceName != "" {
		return sourceName
	}
	return filepath.Base(cityPath)
}

func cmdInitFromDirWithOptions(fromDir string, args []string, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
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

	return doInitFromDirWithOptions(srcDir, cityPath, nameOverride, stdout, stderr, skipProviderReadiness)
}

// doInitFromDir copies an example city directory to a new city path,
// updates workspace.name, creates .gc/, and installs hooks.
func doInitFromDir(srcDir, cityPath string, stdout, stderr io.Writer) int {
	return doInitFromDirWithOptions(srcDir, cityPath, "", stdout, stderr, false)
}

func doInitFromDirWithOptions(srcDir, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
	fs := fsys.OSFS{}
	// Validate source has city.toml.
	srcToml := filepath.Join(srcDir, "city.toml")
	if _, err := os.Stat(srcToml); err != nil {
		fmt.Fprintf(stderr, "gc init --from: source %q has no city.toml\n", srcDir) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Check target not already initialized.
	if cityAlreadyInitializedFS(fs, cityPath) {
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
	cityName := resolveCityName(nameOverride, cfg.Workspace.Name, cityPath)
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

	// Create runtime scaffold.
	if err := ensureCityScaffold(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Install Claude Code hooks.
	if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Resolve formulas and scripts from pack layers.
	expandedCfg, _, loadErr := config.LoadWithIncludes(fsys.OSFS{}, copiedToml)
	if loadErr == nil && len(expandedCfg.FormulaLayers.City) > 0 {
		if rfErr := ResolveFormulas(cityPath, expandedCfg.FormulaLayers.City); rfErr != nil {
			fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort stderr
		}
	}
	if loadErr == nil && len(expandedCfg.ScriptLayers.City) > 0 {
		if rsErr := ResolveScripts(cityPath, expandedCfg.ScriptLayers.City); rsErr != nil {
			fmt.Fprintf(stderr, "gc init: resolving scripts: %v\n", rsErr) //nolint:errcheck // best-effort stderr
		}
	}

	fmt.Fprintln(stdout, "Welcome to Gas City!")                                           //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Initialized city %q from %s.\n", cityName, filepath.Base(srcDir)) //nolint:errcheck // best-effort stdout
	return finalizeInit(cityPath, stdout, stderr, initFinalizeOptions{
		skipProviderReadiness: skipProviderReadiness,
		commandName:           "gc init",
	})
}
