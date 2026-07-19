package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/pricing"
	"github.com/spf13/cobra"
)

const initPackSchemaVersion = 2

const initMailRetentionExample = `# [mail]
# retention_ttl controls how long read messages are retained before purge.
# 0 disables retention; use "168h" for 7 days.
# "7d" is not a valid Go duration.
# retention_ttl = "0"
`

type initPackMeta struct {
	Name        string                   `toml:"name"`
	Version     string                   `toml:"version,omitempty"`
	Schema      int                      `toml:"schema"`
	Description string                   `toml:"description,omitempty"`
	RequiresGC  string                   `toml:"requires_gc,omitempty"`
	Includes    []string                 `toml:"includes,omitempty"`
	Requires    []config.PackRequirement `toml:"requires,omitempty"`
}

type packDefaults struct {
	Rig packRigDefaults `toml:"rig,omitempty"`
}

type packRigDefaults struct {
	Imports map[string]config.Import `toml:"imports,omitempty"`
}

type initPackConfig struct {
	Pack           initPackMeta                   `toml:"pack"`
	Imports        map[string]config.Import       `toml:"imports,omitempty"`
	AgentDefaults  config.AgentDefaults           `toml:"agent_defaults,omitempty"`
	AgentsDefaults config.AgentDefaults           `toml:"agents,omitempty" jsonschema:"-"`
	Defaults       packDefaults                   `toml:"defaults,omitempty"`
	Agents         []config.Agent                 `toml:"agent"`
	NamedSessions  []config.NamedSession          `toml:"named_session,omitempty"`
	Services       []config.Service               `toml:"service,omitempty"`
	Providers      map[string]config.ProviderSpec `toml:"providers,omitempty"`
	Upstreams      map[string]config.UpstreamSpec `toml:"upstreams,omitempty"`
	Formulas       config.FormulasConfig          `toml:"formulas,omitempty"`
	Patches        config.Patches                 `toml:"patches,omitempty"`
	Doctor         []config.PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands       []config.PackCommandEntry      `toml:"commands,omitempty"`
	Global         config.PackGlobal              `toml:"global,omitempty"`
	Pricing        []pricing.ModelPricing         `toml:"pricing,omitempty"`
}

var initConventionDirs = cityinit.InitConventionDirs()

const defaultInitTemplate = "gascity"

// wizardConfig carries the results of the interactive init wizard (or defaults
// for non-interactive paths). doInit uses it to decide which config to write.
type wizardConfig struct {
	interactive      bool   // true if the wizard ran with user interaction
	configName       string // canonical values: "minimal", "gastown", "gascity", "custom", or "empty"
	defaultProvider  string // selected default provider key
	providers        []string
	provider         string                // compatibility mirror for older internal callers
	startCommand     string                // custom start command (workspace-level)
	bootstrapProfile string                // hosted bootstrap profile, or "" for local defaults
	hostedDolt       hostedDoltInitOptions // external/hosted Dolt ledger endpoint (disabled when zero)
	err              error
}

// defaultWizardConfig returns a non-interactive wizardConfig that produces
// the default init template with no provider.
func defaultWizardConfig() wizardConfig {
	return wizardConfig{configName: defaultInitTemplate}
}

func canBootstrapExistingCity(wiz wizardConfig) bool {
	return !wiz.interactive &&
		(wiz.configName == "minimal" || wiz.configName == defaultInitTemplate) &&
		wizardDefaultProvider(wiz) == "" &&
		len(wiz.providers) == 0 &&
		wiz.startCommand == "" &&
		wiz.bootstrapProfile == "" &&
		wiz.err == nil
}

const (
	bootstrapProfileK8sCell          = cityinit.BootstrapProfileK8sCell
	bootstrapProfileSingleHostCompat = cityinit.BootstrapProfileSingleHostCompat
)

// isTerminal reports whether f is connected to a terminal (not a pipe or file).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

var isTerminalFunc = isTerminal

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

	fmt.Fprintln(stdout, "Welcome to Gas City SDK!")                                         //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "")                                                                 //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "Choose a config template:")                                        //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  1. gascity   — planning & implementation skills pack (default)") //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  2. minimal   — default coding agent")                            //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  3. gastown   — multi-agent orchestration pack")                  //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  4. custom    — empty workspace, configure it yourself")          //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Template [1]: ")                                                    //nolint:errcheck // best-effort stdout

	configChoice := readLine(br)
	configName := defaultInitTemplate

	switch configChoice {
	case "", "1", "gascity":
		configName = "gascity"
	case "2", "minimal", "tutorial":
		configName = "minimal"
	case "3", "gastown":
		configName = "gastown"
	case "4", "custom":
		configName = "custom"
	default:
		fmt.Fprintf(stdout, "Unknown template %q, using gascity.\n", configChoice) //nolint:errcheck // best-effort stdout
	}

	// Custom config → skip agent question, return minimal config.
	if configName == "custom" {
		return wizardConfig{
			interactive: true,
			configName:  "custom",
		}
	}

	fmt.Fprintln(stdout, "") //nolint:errcheck // best-effort stdout
	choices, err := configuredWizardProviderChoices(context.Background())
	if err != nil {
		return wizardConfig{interactive: true, configName: configName, err: err}
	}
	if len(choices) == 0 {
		return wizardConfig{
			interactive: true,
			configName:  configName,
			err:         fmt.Errorf("no configured coding agents found; configure your coding agent and restart the wizard"),
		}
	}

	fmt.Fprintln(stdout, "Choose your coding agent:") //nolint:errcheck // best-effort stdout
	for i, choice := range choices {
		fmt.Fprintf(stdout, "  %d. %s\n", i+1, choice.DisplayName) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintln(stdout, "If you don't see your coding agent, configure it and restart the wizard.") //nolint:errcheck // best-effort stdout

	providers := providerChoiceKeys(choices)
	defaultProvider := choices[0].Name
	if len(choices) > 1 {
		fmt.Fprintf(stdout, "Agent: ") //nolint:errcheck // best-effort stdout
		agentChoice := readLine(br)
		defaultProvider = resolveDefaultProviderChoice(agentChoice, choices)
		if defaultProvider == "" {
			return wizardConfig{
				interactive: true,
				configName:  configName,
				providers:   providers,
				err:         fmt.Errorf("provider selection is required; enter a number or exact provider key"),
			}
		}
	}

	return wizardConfig{
		interactive:     true,
		configName:      configName,
		defaultProvider: defaultProvider,
		providers:       providers,
		provider:        defaultProvider,
	}
}

type wizardProviderChoice struct {
	Name        string
	DisplayName string
}

func configuredWizardProviderChoices(ctx context.Context) ([]wizardProviderChoice, error) {
	names := api.ProviderReadinessNames()
	items, err := initProbeProvidersReadiness(ctx, names, true)
	if err != nil {
		return nil, fmt.Errorf("checking provider readiness: %w", err)
	}
	choices := make([]wizardProviderChoice, 0, len(names))
	builtins := config.BuiltinProviders()
	for _, name := range names {
		item, ok := items[name]
		if !ok || item.Status != api.ProbeStatusConfigured {
			continue
		}
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = builtins[name].DisplayName
		}
		if displayName == "" {
			displayName = name
		}
		choices = append(choices, wizardProviderChoice{Name: name, DisplayName: displayName})
	}
	return choices, nil
}

func providerChoiceKeys(choices []wizardProviderChoice) []string {
	out := make([]string, 0, len(choices))
	for _, choice := range choices {
		out = append(out, choice.Name)
	}
	return out
}

func resolveDefaultProviderChoice(input string, choices []wizardProviderChoice) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	if n, err := strconv.Atoi(input); err == nil && n >= 1 && n <= len(choices) {
		return choices[n-1].Name
	}
	for _, choice := range choices {
		if input == choice.Name {
			return choice.Name
		}
	}
	return ""
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

// initExitAlreadyInitialized is the process exit code for an init request
// that targets an already-initialized city. The supervisor API depends on
// this value to translate gc init conflicts into HTTP 409.
const initExitAlreadyInitialized = 2

func logInitProgress(stdout io.Writer, step int, msg string) {
	if stdout == nil {
		return
	}
	fmt.Fprintf(stdout, "[%d/%d] %s\n", step, initProgressSteps, msg) //nolint:errcheck // best-effort stdout
}

func initAlreadyInitialized(stderr io.Writer) int {
	fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
	return initExitAlreadyInitialized
}

func newInitCmd(stdout, stderr io.Writer) *cobra.Command {
	var fileFlag string
	var fromFlag string
	var nameFlag string
	var templateFlag string
	var providerFlag string
	var providersFlag []string
	var defaultProviderFlag string
	var bootstrapProfileFlag string
	var doltHostFlag string
	var doltPortFlag string
	var doltUserFlag string
	var doltDatabaseFlag string
	var doltProjectIDFlag string
	var skipProviderReadiness bool
	var preserveExisting bool
	var jsonOut bool
	var noStart bool
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Initialize a new city",
		Long: `Create a new Gas City workspace in the given directory (or cwd).

Runs an interactive wizard to choose a config template and coding agent
provider. Creates the .gc/ runtime directory plus pack.toml, city.toml,
the standard top-level directories, and .template.md prompt templates, and
pins the builtin pack imports (resolved from the user-global pack cache).
Use --template with --default-provider to create a city non-interactively,
or --file to initialize from an existing TOML config file.

Pass --preserve-existing to keep any pre-authored pack.toml, city.toml, or
agent prompt files in the target directory (useful when bootstrapping a
committed workspace — e.g. from a bootstrap.sh shipped in the repo).`,
		Example: `  gc init
  gc init ~/my-city
  gc init --default-provider codex ~/my-city
  gc init --template gastown --default-provider codex ~/my-city
  gc init --providers claude,codex --default-provider codex ~/my-city
  gc init --default-provider codex --bootstrap-profile k8s-cell /city
  gc init --name my-city
  gc init --from ~/elan --name elan /city
  gc init --file ./my-city.toml ~/bright-lights
  gc init --file city.toml --preserve-existing .
  gc init --template gascity --default-provider claude \
    --dolt-host db.example.com --dolt-port 4406 \
    --dolt-database bd_prj_x --dolt-project-id prj_x --no-start /city`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(runCmd *cobra.Command, args []string) error {
			out := stdout
			if jsonOut {
				out = io.Discard
			}
			mode := "default"
			if fromFlag != "" {
				mode = "from"
				code := cmdInitFromDirWithOptionsInternal(fromFlag, args, nameFlag, out, stderr, skipProviderReadiness, noStart)
				return writeInitJSONOrExit(code, jsonOut, args, nameFlag, "", "", nil, bootstrapProfileFlag, mode, stdout)
			}
			if fileFlag != "" {
				mode = "file"
				code := cmdInitFromFileWithOptionsInternal(fileFlag, args, nameFlag, out, stderr, skipProviderReadiness, preserveExisting, noStart)
				return writeInitJSONOrExit(code, jsonOut, args, nameFlag, "", "", nil, bootstrapProfileFlag, mode, stdout)
			}
			hosted := resolveHostedDoltInitOptions(hostedDoltInitFlagValues{
				Host:      doltHostFlag,
				Port:      doltPortFlag,
				User:      doltUserFlag,
				Database:  doltDatabaseFlag,
				ProjectID: doltProjectIDFlag,
			}, os.Getenv)
			wiz, flagMode, err := initWizardConfigFromFlags(runCmd, providerFlag, defaultProviderFlag, providersFlag, templateFlag, bootstrapProfileFlag, hosted)
			if err != nil {
				fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
				return err
			}
			if flagMode != "" {
				mode = flagMode
			}
			code := cmdInitWithPreparedWizardInternal(args, wiz, flagMode != "", nameFlag, out, stderr, skipProviderReadiness, preserveExisting, jsonOut, noStart)
			return writeInitJSONOrExit(code, jsonOut, args, nameFlag, wiz.configName, wizardDefaultProvider(wiz), wizardProviders(wiz), bootstrapProfileFlag, mode, stdout)
		},
	}
	cmd.Flags().StringVar(&fileFlag, "file", "", "path to a TOML file to use as city.toml")
	cmd.Flags().StringVar(&fromFlag, "from", "", "path to an example city directory to copy")
	cmd.Flags().StringVar(&nameFlag, "name", "", "workspace name (default: target directory basename)")
	cmd.Flags().StringVar(&providerFlag, "provider", "", "deprecated alias for --default-provider")
	cmd.Flags().StringVar(&defaultProviderFlag, "default-provider", "", "default readiness-aware provider to select from --providers")
	cmd.Flags().StringArrayVar(&providersFlag, "providers", nil, "readiness-aware providers to write to city.toml (repeatable or comma-separated)")
	cmd.Flags().StringVar(&templateFlag, "template", "", "non-interactive template to write: minimal, gastown, gascity, custom, or empty")
	cmd.Flags().StringVar(&bootstrapProfileFlag, "bootstrap-profile", "", "bootstrap profile to apply for hosted/container defaults")
	cmd.Flags().StringVar(&doltHostFlag, "dolt-host", "", "external/hosted Dolt host for the city beads ledger (or "+envDoltHost+"); pins the city to an external endpoint instead of bootstrapping a managed-local Dolt")
	cmd.Flags().StringVar(&doltPortFlag, "dolt-port", "", "external/hosted Dolt port (or "+envDoltPort+"); required with --dolt-host")
	cmd.Flags().StringVar(&doltUserFlag, "dolt-user", "", "external/hosted Dolt user (or "+envDoltUser+"); optional")
	cmd.Flags().StringVar(&doltDatabaseFlag, "dolt-database", "", "hosted beads project database, e.g. bd_prj_… (or "+envDoltDatabase+"); required with --dolt-host")
	cmd.Flags().StringVar(&doltProjectIDFlag, "dolt-project-id", "", "authoritative beads project_id for the identity handshake (or "+envBeadsProjectID+"); derived from a bd_<id> --dolt-database when omitted")
	cmd.Flags().BoolVar(&skipProviderReadiness, "skip-provider-readiness", false, "skip provider login/readiness checks during init and continue startup")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "initialize files and imports without registering or starting the city")
	cmd.Flags().BoolVar(&preserveExisting, "preserve-existing", false, "keep any pre-authored pack.toml, city.toml, or agent prompt files instead of overwriting them")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	cmd.Flags().BoolVar(&assumeYesForSupervisorCycle, "yes", false, "bypass the cross-city supervisor cycle confirmation prompt (warning is still printed for the audit trail)")
	cmd.MarkFlagsMutuallyExclusive("file", "from")
	cmd.MarkFlagsMutuallyExclusive("provider", "file")
	cmd.MarkFlagsMutuallyExclusive("provider", "from")
	cmd.MarkFlagsMutuallyExclusive("default-provider", "file")
	cmd.MarkFlagsMutuallyExclusive("default-provider", "from")
	cmd.MarkFlagsMutuallyExclusive("providers", "file")
	cmd.MarkFlagsMutuallyExclusive("providers", "from")
	cmd.MarkFlagsMutuallyExclusive("template", "file")
	cmd.MarkFlagsMutuallyExclusive("template", "from")
	cmd.MarkFlagsMutuallyExclusive("bootstrap-profile", "file")
	cmd.MarkFlagsMutuallyExclusive("bootstrap-profile", "from")
	for _, doltFlag := range []string{"dolt-host", "dolt-port", "dolt-user", "dolt-database", "dolt-project-id"} {
		cmd.MarkFlagsMutuallyExclusive(doltFlag, "file")
		cmd.MarkFlagsMutuallyExclusive(doltFlag, "from")
	}
	_ = cmd.Flags().MarkHidden("provider")
	return cmd
}

type initJSONResult struct {
	SchemaVersion    string   `json:"schema_version"`
	OK               bool     `json:"ok"`
	CityPath         string   `json:"city_path"`
	CityName         string   `json:"city_name"`
	Mode             string   `json:"mode"`
	Template         string   `json:"template,omitempty"`
	Provider         string   `json:"provider,omitempty"`
	DefaultProvider  string   `json:"default_provider,omitempty"`
	Providers        []string `json:"providers,omitempty"`
	BootstrapProfile string   `json:"bootstrap_profile,omitempty"`
}

func writeInitJSONOrExit(code int, jsonOut bool, args []string, nameOverride, templateName, defaultProvider string, providers []string, bootstrapProfile, mode string, stdout io.Writer) error {
	if code != 0 {
		return exitForCode(code)
	}
	if !jsonOut {
		return nil
	}
	cityPath, err := initTargetPath(args)
	if err != nil {
		return err
	}
	return writeCLIJSONLine(stdout, initJSONResult{
		SchemaVersion:    "1",
		OK:               true,
		CityPath:         cityPath,
		CityName:         resolveCityName(nameOverride, "", cityPath),
		Mode:             mode,
		Template:         strings.TrimSpace(templateName),
		Provider:         strings.TrimSpace(defaultProvider),
		DefaultProvider:  strings.TrimSpace(defaultProvider),
		Providers:        append([]string(nil), providers...),
		BootstrapProfile: strings.TrimSpace(bootstrapProfile),
	})
}

func initTargetPath(args []string) (string, error) {
	if len(args) > 0 {
		return filepath.Abs(args[0])
	}
	return os.Getwd()
}

// cmdInit initializes a new city at the given path (or cwd if no path given).
// Runs the interactive wizard to choose a config template and provider.
// Creates the runtime scaffold and city.toml. If the bead provider is "bd", also
// runs bd init.
func cmdInit(args []string, providerFlag, bootstrapProfileFlag string, stdout, stderr io.Writer) int {
	return cmdInitWithOptions(args, providerFlag, bootstrapProfileFlag, "", stdout, stderr, false, false)
}

func cmdInitWithOptions(args []string, providerFlag, bootstrapProfileFlag, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool) int {
	return cmdInitWithOptionsInternal(args, providerFlag, bootstrapProfileFlag, nameOverride, stdout, stderr, skipProviderReadiness, preserveExisting, false)
}

func cmdInitWithOptionsInternal(args []string, providerFlag, bootstrapProfileFlag, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool, forceDefaultWizard bool) int {
	var prepared wizardConfig
	preparedSet := false
	if providerFlag != "" || bootstrapProfileFlag != "" {
		var err error
		prepared, err = initWizardConfig(providerFlag, bootstrapProfileFlag)
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		preparedSet = true
	}
	return cmdInitWithPreparedWizard(args, prepared, preparedSet, nameOverride, stdout, stderr, skipProviderReadiness, preserveExisting, forceDefaultWizard)
}

func cmdInitWithPreparedWizard(args []string, prepared wizardConfig, preparedSet bool, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool, forceDefaultWizard bool) int {
	return cmdInitWithPreparedWizardInternal(args, prepared, preparedSet, nameOverride, stdout, stderr, skipProviderReadiness, preserveExisting, forceDefaultWizard, false)
}

func cmdInitWithPreparedWizardInternal(args []string, prepared wizardConfig, preparedSet bool, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool, forceDefaultWizard bool, noStart bool) int {
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
	if handled, code := resumeExistingInitIfPossibleInternal(fsys.OSFS{}, cityPath, stdout, stderr, "gc init", true, skipProviderReadiness, noStart); handled {
		return code
	}
	var wiz wizardConfig
	switch {
	case preparedSet:
		wiz = prepared
	case forceDefaultWizard:
		wiz = defaultWizardConfig()
	case isTerminalFunc(os.Stdin):
		wiz = runWizard(os.Stdin, stdout)
		if wiz.err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", wiz.err) //nolint:errcheck // best-effort stderr
			return 1
		}
		maybePrintWizardProviderGuidance(wiz, stdout)
	default:
		wiz = defaultWizardConfig()
	}
	if err := preflightInitSelectedProviders(wiz, skipProviderReadiness); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if code := doInit(fsys.OSFS{}, cityPath, wiz, nameOverride, stdout, stderr, preserveExisting); code != 0 {
		return code
	}
	return finalizeInit(cityPath, stdout, stderr, initFinalizeOptions{
		skipProviderReadiness: skipProviderReadiness,
		showProgress:          true,
		commandName:           "gc init",
		noStart:               noStart,
	})
}

func resumeExistingInitIfPossibleInternal(fs fsys.FS, cityPath string, stdout, stderr io.Writer, commandName string, showProgress bool, skipProviderReadiness bool, noStart bool) (bool, int) {
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
		noStart:               noStart,
	})
}

func initWizardConfig(providerFlag, bootstrapProfileFlag string) (wizardConfig, error) {
	defaultProvider, err := normalizeInitProvider(providerFlag)
	if err != nil {
		return wizardConfig{}, err
	}
	bootstrapProfile, err := normalizeBootstrapProfile(bootstrapProfileFlag)
	if err != nil {
		return wizardConfig{}, err
	}
	providers := []string(nil)
	if defaultProvider != "" {
		providers = []string{defaultProvider}
	}
	return wizardConfig{
		configName:       defaultInitTemplate,
		defaultProvider:  defaultProvider,
		providers:        providers,
		provider:         defaultProvider,
		bootstrapProfile: bootstrapProfile,
	}, nil
}

func initWizardConfigFromFlags(cmd *cobra.Command, providerFlag, defaultProviderFlag string, providersFlag []string, templateFlag, bootstrapProfileFlag string, hosted hostedDoltInitOptions) (wizardConfig, string, error) {
	legacyChanged := cmd.Flags().Changed("provider")
	defaultChanged := cmd.Flags().Changed("default-provider")
	providersChanged := cmd.Flags().Changed("providers")
	templateChanged := cmd.Flags().Changed("template")
	bootstrapChanged := strings.TrimSpace(bootstrapProfileFlag) != ""

	if !legacyChanged && !defaultChanged && !providersChanged && !templateChanged && !bootstrapChanged && !hosted.enabled() {
		return wizardConfig{}, "", nil
	}
	if err := hosted.validate(); err != nil {
		return wizardConfig{}, "", err
	}
	if legacyChanged && defaultChanged {
		return wizardConfig{}, "", fmt.Errorf("--provider is deprecated; use --default-provider, not both")
	}
	if legacyChanged {
		if strings.ContainsAny(providerFlag, ", \t\n") {
			return wizardConfig{}, "", fmt.Errorf("--provider accepts one deprecated default provider; use --providers %s --default-provider <name>", strings.TrimSpace(providerFlag))
		}
		defaultProviderFlag = providerFlag
		defaultChanged = true
	}

	template, err := normalizeInitTemplate(templateFlag, templateChanged)
	if err != nil {
		return wizardConfig{}, "", err
	}
	defaultProvider, err := normalizeInitProvider(defaultProviderFlag)
	if err != nil {
		return wizardConfig{}, "", err
	}
	providers, err := normalizeInitProviders(providersFlag)
	if err != nil {
		return wizardConfig{}, "", err
	}
	if defaultProvider != "" && len(providers) == 0 {
		providers = []string{defaultProvider}
	}
	if len(providers) > 0 && defaultProvider == "" {
		return wizardConfig{}, "", fmt.Errorf("--providers requires --default-provider")
	}
	if defaultProvider != "" && !stringInSlice(defaultProvider, providers) {
		return wizardConfig{}, "", fmt.Errorf("--default-provider %q must be included in --providers", defaultProvider)
	}
	if (template == "custom" || template == "empty") && (legacyChanged || defaultChanged || providersChanged) {
		return wizardConfig{}, "", fmt.Errorf("--template %s cannot be combined with provider flags", template)
	}
	if (template == "minimal" || template == "gastown" || template == "gascity") && defaultProvider == "" {
		return wizardConfig{}, "", fmt.Errorf("--template %s requires --default-provider", template)
	}

	bootstrapProfile, err := normalizeBootstrapProfile(bootstrapProfileFlag)
	if err != nil {
		return wizardConfig{}, "", err
	}
	mode := "provider"
	if templateChanged {
		mode = "template"
	}
	return wizardConfig{
		configName:       template,
		defaultProvider:  defaultProvider,
		providers:        providers,
		provider:         defaultProvider,
		bootstrapProfile: bootstrapProfile,
		hostedDolt:       hosted,
	}, mode, nil
}

func normalizeInitProvider(provider string) (string, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "", nil
	}
	for _, name := range api.ProviderReadinessNames() {
		if provider == name {
			return provider, nil
		}
	}
	return "", fmt.Errorf("unknown provider %q (expected one of: %s)", provider, strings.Join(api.ProviderReadinessNames(), ", "))
}

func normalizeInitProviders(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, value := range values {
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			name, err := normalizeInitProvider(part)
			if err != nil {
				return nil, err
			}
			seen[name] = true
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("--providers requires at least one provider")
	}
	var out []string
	for _, name := range api.ProviderReadinessNames() {
		if seen[name] {
			out = append(out, name)
		}
	}
	return out, nil
}

func normalizeInitTemplate(template string, supplied bool) (string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return defaultInitTemplate, nil
	}
	switch template {
	case "minimal", "gastown", "gascity", "custom", "empty":
		return template, nil
	default:
		if supplied {
			return "", fmt.Errorf("unknown template %q (expected one of: minimal, gastown, gascity, custom, empty)", template)
		}
		return defaultInitTemplate, nil
	}
}

func stringInSlice(value string, items []string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func preflightInitSelectedProviders(wiz wizardConfig, skip bool) error {
	providers := wizardProviders(wiz)
	if skip || len(providers) == 0 {
		return nil
	}
	items, err := initProbeProvidersReadiness(context.Background(), providers, true)
	if err != nil {
		return fmt.Errorf("checking provider readiness: %w", err)
	}
	var blockers []string
	for _, provider := range providers {
		item, ok := items[provider]
		if !ok || item.Status == api.ProbeStatusConfigured {
			continue
		}
		blockers = append(blockers, fmt.Sprintf("%s: %s", item.DisplayName, providerStatusSummary(item.Status)))
	}
	if len(blockers) == 0 {
		return nil
	}
	return fmt.Errorf("provider readiness preflight failed: %s", strings.Join(blockers, "; "))
}

func normalizeBootstrapProfile(profile string) (string, error) {
	return cityinit.NormalizeBootstrapProfile(profile)
}

func initPromptTemplatePath(templatePath string) (string, bool) {
	// Template paths come from embedded config and are always slash-separated,
	// so compare against "/" rather than the OS separator (which is `\` on
	// Windows and silently skipped every scaffold there).
	if !strings.HasPrefix(filepath.ToSlash(templatePath), citylayout.PromptsRoot+"/") {
		return "", false
	}
	base := filepath.Base(templatePath)
	switch {
	case strings.HasSuffix(base, canonicalPromptTemplateSuffix):
		base = strings.TrimSuffix(base, canonicalPromptTemplateSuffix)
	case strings.HasSuffix(base, legacyPromptTemplateSuffix):
		base = strings.TrimSuffix(base, legacyPromptTemplateSuffix)
	case strings.HasSuffix(base, ".md"):
		base = strings.TrimSuffix(base, ".md")
	default:
		return "", false
	}
	if base == "" {
		return "", false
	}
	return filepath.Join("agents", base, "prompt.template.md"), true
}

func rewriteInitPromptTemplates(cfg *config.City) {
	if cfg == nil {
		return
	}
	for i := range cfg.Agents {
		if next, ok := initPromptTemplatePath(cfg.Agents[i].PromptTemplate); ok {
			cfg.Agents[i].PromptTemplate = next
		}
	}
}

func ensureInitConventionDirs(fs fsys.FS, cityPath string) error {
	for _, rel := range initConventionDirs {
		if err := fs.MkdirAll(filepath.Join(cityPath, rel), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// writeInitFile writes data to path. When preserve is true and path already
// exists, the existing file is kept untouched and wrote=false is returned.
// preserve is set by --preserve-existing, which callers (e.g. bootstrap
// scripts operating on a pre-authored workspace) use to avoid clobbering
// committed user files like pack.toml, city.toml, and agent prompts.
func writeInitFile(fs fsys.FS, path string, data []byte, preserve bool) (wrote bool, err error) {
	if preserve {
		if _, err := fs.Stat(path); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	return true, fs.WriteFile(path, data, 0o644)
}

// writeInitPackTomlOpts marshals and writes pack.toml, honoring the
// preserve-existing option. Returns (wrote, err) mirroring writeInitFile.
func writeInitPackTomlOpts(fs fsys.FS, cityPath string, packCfg initPackConfig, preserve bool) (bool, error) {
	content, err := marshalInitPackConfig(packCfg)
	if err != nil {
		return false, err
	}
	return writeInitFile(fs, filepath.Join(cityPath, "pack.toml"), content, preserve)
}

func marshalInitPackConfig(cfg initPackConfig) ([]byte, error) {
	type encodedInitPackMeta struct {
		Name        string                   `toml:"name"`
		Version     string                   `toml:"version,omitempty"`
		Schema      int                      `toml:"schema"`
		Description string                   `toml:"description,omitempty"`
		RequiresGC  string                   `toml:"requires_gc,omitempty"`
		Includes    []string                 `toml:"includes,omitempty"`
		Requires    []config.PackRequirement `toml:"requires,omitempty"`
	}
	type encodedInitPackConfig struct {
		Pack          encodedInitPackMeta            `toml:"pack"`
		Imports       map[string]config.Import       `toml:"imports,omitempty"`
		AgentDefaults *config.AgentDefaults          `toml:"agent_defaults,omitempty"`
		Defaults      *packDefaults                  `toml:"defaults,omitempty"`
		Agents        []config.Agent                 `toml:"agent,omitempty"`
		NamedSessions []config.NamedSession          `toml:"named_session,omitempty"`
		Services      []config.Service               `toml:"service,omitempty"`
		Providers     map[string]config.ProviderSpec `toml:"providers,omitempty"`
		Upstreams     map[string]config.UpstreamSpec `toml:"upstreams,omitempty"`
		Formulas      *config.FormulasConfig         `toml:"formulas,omitempty"`
		Patches       *config.Patches                `toml:"patches,omitempty"`
		Doctor        []config.PackDoctorEntry       `toml:"doctor,omitempty"`
		Commands      []config.PackCommandEntry      `toml:"commands,omitempty"`
		Global        *config.PackGlobal             `toml:"global,omitempty"`
		Pricing       []pricing.ModelPricing         `toml:"pricing,omitempty"`
	}

	encCfg := encodedInitPackConfig{
		Pack: encodedInitPackMeta{
			Name:        cfg.Pack.Name,
			Version:     cfg.Pack.Version,
			Schema:      cfg.Pack.Schema,
			Description: cfg.Pack.Description,
			RequiresGC:  cfg.Pack.RequiresGC,
			Includes:    cfg.Pack.Includes,
			Requires:    cfg.Pack.Requires,
		},
		Imports:       cfg.Imports,
		Agents:        cfg.Agents,
		NamedSessions: cfg.NamedSessions,
		Services:      cfg.Services,
		Providers:     cfg.Providers,
		Upstreams:     cfg.Upstreams,
		Doctor:        cfg.Doctor,
		Commands:      cfg.Commands,
		Pricing:       cfg.Pricing,
	}
	if !isZeroValue(cfg.AgentDefaults) {
		encCfg.AgentDefaults = &cfg.AgentDefaults
	}
	if !isZeroValue(cfg.Defaults) {
		encCfg.Defaults = &cfg.Defaults
	}
	if !isZeroValue(cfg.Formulas) {
		encCfg.Formulas = &cfg.Formulas
	}
	if !isZeroValue(cfg.Patches) {
		encCfg.Patches = &cfg.Patches
	}
	if !isZeroValue(cfg.Global) {
		encCfg.Global = &cfg.Global
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(encCfg); err != nil {
		return nil, fmt.Errorf("marshal pack.toml: %w", err)
	}
	return buf.Bytes(), nil
}

func isZeroValue(v any) bool {
	return reflect.ValueOf(v).IsZero()
}

func withInitMailRetentionExample(content []byte) []byte {
	text := string(content)
	if strings.Contains(text, "retention_ttl") {
		return content
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if !strings.HasSuffix(text, "\n\n") {
		text += "\n"
	}
	text += initMailRetentionExample
	return []byte(text)
}

func newInitPackConfig(cityName string) initPackConfig {
	return initPackConfig{
		Pack: initPackMeta{
			Name:   cityName,
			Schema: initPackSchemaVersion,
		},
	}
}

// splitInitConfig separates a composed init template City into its
// portable pack-first shape and the machine-local city runtime shape:
//
//   - pack.toml owns the portable definition: [pack], explicit [[agent]]
//     entries when the caller keeps them, [[named_session]], [imports.*],
//     [providers.*], services, and agent patches.
//   - city.toml keeps only runtime-local deployment settings (e.g.
//     workspace.provider, workspace.start_command, agent_defaults,
//     default-rig imports, rig/provider patches, api, daemon, beads).
//   - workspace.name and workspace.prefix migrate to .gc/site.toml via
//     persistInitWorkspaceIdentity, so they are cleared here to avoid
//     duplicating the city's machine-local identity in city.toml.
func splitInitConfig(cityName string, cfg *config.City) (initPackConfig, config.City) {
	packCfg := newInitPackConfig(cityName)
	if cfg == nil {
		return packCfg, config.City{}
	}

	cityCfg := *cfg
	cityCfg.Agents = nil
	cityCfg.NamedSessions = nil
	cityCfg.Imports = nil
	cityCfg.Services = nil
	cityCfg.Formulas = config.FormulasConfig{}
	cityCfg.Patches = config.Patches{
		Rigs:      append([]config.RigPatch(nil), cfg.Patches.Rigs...),
		Providers: append([]config.ProviderPatch(nil), cfg.Patches.Providers...),
	}
	cityCfg.AgentsDefaults = config.AgentDefaults{}
	cityCfg.Defaults = config.PackDefaults{}
	cityCfg.Workspace.Name = ""
	cityCfg.Workspace.Prefix = ""

	packCfg.Agents = append([]config.Agent(nil), cfg.Agents...)
	packCfg.NamedSessions = append([]config.NamedSession(nil), cfg.NamedSessions...)
	if len(cfg.Imports) > 0 {
		packCfg.Imports = make(map[string]config.Import, len(cfg.Imports))
		for name, imp := range cfg.Imports {
			packCfg.Imports[name] = imp
		}
	}
	packCfg.Patches = config.Patches{
		Agents: append([]config.AgentPatch(nil), cfg.Patches.Agents...),
	}
	for _, svc := range cfg.Services {
		if svc.PublishMode == "direct" {
			cityCfg.Services = append(cityCfg.Services, svc)
			continue
		}
		packCfg.Services = append(packCfg.Services, svc)
	}

	if len(cfg.Workspace.LegacyIncludes()) > 0 {
		packCfg.Pack.Includes = appendUniqueStrings(
			append([]string(nil), packCfg.Pack.Includes...),
			cfg.Workspace.LegacyIncludes()...,
		)
		cityCfg.Workspace.SetLegacyIncludes(nil)
	}
	defaultRigImports := initDefaultRigImports(cfg)
	if len(defaultRigImports) > 0 {
		defaults := config.PackDefaults{
			Rig: config.PackRigDefaults{
				Imports: make(map[string]config.Import, len(defaultRigImports)),
			},
		}
		for name, imp := range defaultRigImports {
			defaults.Rig.Imports[name] = imp
		}
		cityCfg.Defaults = defaults
		cityCfg.Workspace.SetLegacyDefaultRigIncludes(nil)
	}
	return packCfg, cityCfg
}

func initDefaultRigImports(cfg *config.City) map[string]config.Import {
	if cfg == nil {
		return nil
	}
	if len(cfg.Defaults.Rig.Imports) > 0 {
		return cfg.Defaults.Rig.Imports
	}
	return cfg.DefaultRigImports
}

func decodeInitPackTemplate(data []byte, cityName string) (initPackConfig, error) {
	cfg := newInitPackConfig(cityName)
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return initPackConfig{}, fmt.Errorf("parse pack template: %w", err)
	}
	cfg.Pack.Name = cityName
	cfg.Pack.Schema = initPackSchemaVersion
	return cfg, nil
}

func applyInitPackTemplateExtras(dst *initPackConfig, src initPackConfig) {
	if dst == nil {
		return
	}
	dst.Pack.Version = src.Pack.Version
	dst.Pack.Description = src.Pack.Description
	dst.Pack.RequiresGC = src.Pack.RequiresGC
	dst.Pack.Includes = appendUniqueStrings(
		append([]string(nil), dst.Pack.Includes...),
		src.Pack.Includes...,
	)
	dst.Pack.Requires = append([]config.PackRequirement(nil), src.Pack.Requires...)
	dst.Doctor = append([]config.PackDoctorEntry(nil), src.Doctor...)
	dst.Commands = append([]config.PackCommandEntry(nil), src.Commands...)
	if !isZeroValue(src.Global) {
		dst.Global = src.Global
	}
}

// addBuiltinImportsToInitPack merges the required bundled-pack imports
// into the init pack manifest, preserving any imports the template (or a
// preserved pack.toml) already declares.
func addBuiltinImportsToInitPack(packCfg *initPackConfig, cityProvider string) {
	imports, names := builtinImportsForInit(cityProvider)
	if len(names) == 0 {
		return
	}
	if packCfg.Imports == nil {
		packCfg.Imports = make(map[string]config.Import, len(names))
	}
	for _, name := range names {
		if _, exists := packCfg.Imports[name]; exists {
			continue
		}
		packCfg.Imports[name] = imports[name]
	}
}

func appendUniqueStrings(dst []string, items ...string) []string {
	seen := make(map[string]struct{}, len(dst))
	for _, item := range dst {
		seen[item] = struct{}{}
	}
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		dst = append(dst, item)
	}
	return dst
}

func cmdInitFromFileWithOptions(fileArg string, args []string, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool) int {
	return cmdInitFromFileWithOptionsInternal(fileArg, args, nameOverride, stdout, stderr, skipProviderReadiness, preserveExisting, false)
}

func cmdInitFromFileWithOptionsInternal(fileArg string, args []string, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool, noStart bool) int {
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

	return cmdInitFromTOMLFileWithOptionsInternal(fsys.OSFS{}, fileArg, cityPath, nameOverride, stdout, stderr, skipProviderReadiness, preserveExisting, noStart)
}

// cmdInitFromTOMLFile initializes a city by copying a user-provided TOML
// file as city.toml. Creates the runtime scaffold, visible roots, and runs bead init.
func cmdInitFromTOMLFile(fs fsys.FS, tomlSrc, cityPath string, stdout, stderr io.Writer) int {
	return cmdInitFromTOMLFileWithOptions(fs, tomlSrc, cityPath, "", stdout, stderr, false, false)
}

func cmdInitFromTOMLFileWithOptions(fs fsys.FS, tomlSrc, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool) int {
	return cmdInitFromTOMLFileWithOptionsInternal(fs, tomlSrc, cityPath, nameOverride, stdout, stderr, skipProviderReadiness, preserveExisting, false)
}

func cmdInitFromTOMLFileWithOptionsInternal(fs fsys.FS, tomlSrc, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool, noStart bool) int {
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
	if cfg.Formulas.Dir != "" {
		fmt.Fprintln(stderr, "gc init: [formulas].dir is no longer supported; use the well-known formulas/ directory") //nolint:errcheck // best-effort stderr
		return 1
	}

	// --file creates a new city from a template; default to target dir name.
	cityName := resolveCityName(nameOverride, "", cityPath)
	cityPrefix := strings.TrimSpace(cfg.Workspace.Prefix)
	templatePack, err := decodeInitPackTemplate(data, cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg.Workspace.Name = cityName

	// Create directory structure. With --preserve-existing, only refuse when
	// the runtime scaffold is already in place — a pre-authored city.toml in
	// the target directory (e.g. a committed workspace being bootstrapped)
	// is preserved below rather than blocking init.
	alreadyInitialized := cityAlreadyInitializedFS(fs, cityPath)
	if preserveExisting {
		alreadyInitialized = cityHasScaffoldFS(fs, cityPath)
	}
	if alreadyInitialized {
		return initAlreadyInitialized(stderr)
	}
	if err := ensureCityScaffoldFS(fs, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureInitConventionDirs(fs, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Install Claude Code hooks (settings.json).
	if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Write prompt scaffolds only for the explicit agents declared by the
	// template — preserved individually if --preserve-existing is set.
	if code := writeInitAgentPrompts(fs, cityPath, cfg, stderr, preserveExisting); code != 0 {
		return code
	}

	// Rewrite legacy prompt paths on the composed config before splitting so
	// the pack-owned [[agent]] entries pick up the V2 agents/<name>/
	// prompt.template.md paths we actually scaffold.
	rewriteInitPromptTemplates(cfg)
	packCfg, cityCfg := splitInitConfig(cityName, cfg)
	applyInitPackTemplateExtras(&packCfg, templatePack)
	// Builtin packs compose only through explicit imports: write the
	// canonical bundled-source entries for this city's providers into
	// pack.toml (mirrors doInit; the builtin-pack-imports doctor check
	// repairs them later).
	addBuiltinImportsToInitPack(&packCfg, cityCfg.Beads.Provider)
	var rigSiteBindings []config.Rig
	if hasInitRigSiteBindings(cityCfg.Rigs) {
		rigSiteBindings = append([]config.Rig(nil), cityCfg.Rigs...)
		for i := range cityCfg.Rigs {
			cityCfg.Rigs[i].Path = ""
		}
	}
	wrotePack, err := writeInitPackTomlOpts(fs, cityPath, packCfg, preserveExisting)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !wrotePack {
		fmt.Fprintln(stdout, "Preserved existing pack.toml.") //nolint:errcheck // best-effort stdout
	}

	formulasInitDir := filepath.Join(cityPath, citylayout.FormulasRoot)
	if rfErr := ResolveFormulas(cityPath, []string{formulasInitDir}); rfErr != nil {
		fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort stderr
	}

	// Write city.toml — preserved if --preserve-existing is set and a
	// committed city.toml is already in place. Persist the workspace identity
	// regardless so .gc/site.toml agrees with the preserved or newly-written
	// config.
	cityTomlPath := filepath.Join(cityPath, "city.toml")
	if len(rigSiteBindings) > 0 {
		wroteCity := true
		if preserveExisting {
			if _, err := fs.Stat(cityTomlPath); err == nil {
				wroteCity = false
			} else if !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
		if wroteCity {
			writeCfg := cityCfg
			writeCfg.Rigs = append([]config.Rig(nil), rigSiteBindings...)
			if err := config.WriteCityAndRigSiteBindingsForEdit(fs, cityTomlPath, &writeCfg); err != nil {
				fmt.Fprintf(stderr, "gc init: %v\n", initSiteBindingPersistError(err)) //nolint:errcheck // best-effort stderr
				return 1
			}
		} else {
			fmt.Fprintln(stdout, "Preserved existing city.toml.") //nolint:errcheck // best-effort stdout
		}
	} else {
		// Re-marshal so the name and rewritten prompt paths are updated.
		content, err := cityCfg.Marshal()
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		wroteCity, err := writeInitFile(fs, cityTomlPath, content, preserveExisting)
		if err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !wroteCity {
			fmt.Fprintln(stdout, "Preserved existing city.toml.") //nolint:errcheck // best-effort stdout
		}
	}
	if err := persistInitWorkspaceIdentity(fs, cityPath, filepath.Join(cityPath, "city.toml"), &cityCfg, cityName, cityPrefix); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Write .gitignore entries for city-managed directories.
	if err := ensureGitignoreEntries(fs, cityPath, cityGitignoreEntries); err != nil {
		fmt.Fprintf(stderr, "gc init: writing .gitignore: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if shouldBootstrapScopedFileStore(cfg) {
		if err := bootstrapScopedFileProviderCityFS(fs, cityPath); err != nil {
			fmt.Fprintf(stderr, "gc init: bootstrapping file bead store: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	fmt.Fprintf(stdout, "Welcome to Gas City!\n")                                           //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Initialized city %q from %s.\n", cityName, filepath.Base(tomlSrc)) //nolint:errcheck // best-effort stdout
	return finalizeInit(cityPath, stdout, stderr, initFinalizeOptions{
		skipProviderReadiness: skipProviderReadiness,
		commandName:           "gc init",
		noStart:               noStart,
	})
}

func hasInitRigSiteBindings(rigs []config.Rig) bool {
	for _, rig := range rigs {
		if strings.TrimSpace(rig.Path) != "" {
			return true
		}
	}
	return false
}

// doInit is the pure logic for "gc init". It creates the city directory
// structure and writes city.toml. Minimal configs use WizardCity
// when a provider or start command is supplied; otherwise init writes the
// default mayor-only city. Errors if the runtime scaffold already exists. Accepts an
// injected FS for testability.
// warnEmptyTemplateMissingBootstrapProfile emits a warning when the "empty"
// template is scaffolded without a --bootstrap-profile. The empty template
// ships no [api] block by design; it composes deterministic API config from a
// bootstrap profile (k8s-cell binds 0.0.0.0:9443 with mutations allowed).
// Without one the API binds to localhost — reachable only within this box.
// That is a legitimate default for a LOCAL controller; the warning exists for
// the HOSTED case, where an entrypoint that forgets --bootstrap-profile leaves
// its front door reachable only inside the pod, and that regression should
// surface in logs rather than pass silently.
func warnEmptyTemplateMissingBootstrapProfile(wiz wizardConfig, stderr io.Writer) {
	if wiz.configName != "empty" || strings.TrimSpace(wiz.bootstrapProfile) != "" {
		return
	}
	fmt.Fprintf(stderr, "gc init: WARNING: --template empty ships no [api] block; without --bootstrap-profile the controller API binds to localhost and is not reachable outside this box. That is fine for a local controller, but a hosted controller must pass --bootstrap-profile %s (0.0.0.0:9443) to serve the front door externally.\n", bootstrapProfileK8sCell) //nolint:errcheck // best-effort stderr
}

func doInit(fs fsys.FS, cityPath string, wiz wizardConfig, nameOverride string, stdout, stderr io.Writer, preserveExisting bool) int {
	warnEmptyTemplateMissingBootstrapProfile(wiz, stderr)
	tomlPath := filepath.Join(cityPath, citylayout.CityConfigFile)
	if cityHasScaffoldFS(fs, cityPath) {
		return initAlreadyInitialized(stderr)
	}
	if _, err := fs.Stat(tomlPath); err == nil {
		if !canBootstrapExistingCity(wiz) {
			return initAlreadyInitialized(stderr)
		}
		if err := ensureCityScaffoldFS(fs, cityPath); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
			return code
		}
		if nameOverride != "" {
			if code := overrideCityName(fs, tomlPath, nameOverride, stderr); code != 0 {
				return code
			}
		}
		cityName := resolveCityName(nameOverride, "", cityPath)
		fmt.Fprintln(stdout, "Welcome to Gas City!")                              //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Bootstrapped city %q runtime scaffold.\n", cityName) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Create directory structure.
	logInitProgress(stdout, 1, "Creating runtime scaffold")
	if err := ensureCityScaffoldFS(fs, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureInitConventionDirs(fs, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Install Claude Code hooks (settings.json).
	logInitProgress(stdout, 2, "Installing hooks (Claude Code)")
	if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Build the initial city shape before writing prompt scaffolds so init
	// only creates convention-discoverable prompt files for the agents the
	// chosen city template actually declares.
	cityName := resolveCityName(nameOverride, "", cityPath)
	var cfg config.City
	defaultProvider := wizardDefaultProvider(wiz)
	providers := wizardProviders(wiz)
	switch {
	case wiz.configName == "custom" || wiz.configName == "empty":
		// Both scaffold a bare, providerless city with no bundled agents,
		// roles, or formulas. "custom" is the human affordance ("configure it
		// yourself"); "empty" is the front-door base — a controller boots it
		// with only the core infra pack (control-dispatcher pool + API) and the
		// pack API installs behavior later via POST /v0/city/{name}/packs.
		cfg = config.EmptyCity(cityName)
	case wiz.configName == "gastown":
		cfg = config.GastownCityWithProviders(cityName, defaultProvider, providers)
	case wiz.configName == "gascity":
		cfg = config.GascityCityWithProviders(cityName, defaultProvider, providers)
	case defaultProvider != "" || len(providers) > 0:
		cfg = config.WizardCityWithProviders(cityName, defaultProvider, providers)
	case wiz.startCommand != "":
		cfg = config.WizardCity(cityName, "", wiz.startCommand)
	default:
		cfg = config.DefaultCity(cityName)
	}
	applyBootstrapProfile(&cfg, wiz.bootstrapProfile)
	if wiz.hostedDolt.enabled() {
		if err := wiz.hostedDolt.applyToCityConfig(&cfg); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	cityPrefix := strings.TrimSpace(cfg.Workspace.Prefix)

	// Write prompt files only for the agents declared by the init template.
	logInitProgress(stdout, 3, "Writing default prompts")
	if code := writeInitAgentPrompts(fs, cityPath, &cfg, stderr, preserveExisting); code != 0 {
		return code
	}

	formulasDir := filepath.Join(cityPath, citylayout.FormulasRoot)
	if err := ResolveFormulas(cityPath, []string{formulasDir}); err != nil {
		fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	// Write city.toml — wizard path gets one agent + provider/startCommand;
	// --provider path gets the same city shape non-interactively;
	// custom path gets one mayor + no provider (user configures manually).
	// Rewrite legacy prompt paths on the composed config before splitting so
	// any explicit template agents point at the V2 agents/<name>/
	// prompt.template.md paths we actually scaffold.
	rewriteInitPromptTemplates(&cfg)
	packCfg, cityCfg := splitInitConfig(cityName, &cfg)
	// Fresh built-in init scaffolds its default agents by convention under
	// agents/<name>/ instead of re-emitting inline [[agent]] entries into
	// pack.toml. The built-in templates currently only need the prompt
	// scaffold plus the pack-owned named session.
	packCfg.Agents = nil
	// Builtin packs compose only through explicit imports: write the
	// canonical bundled-source entries for this city's providers into
	// pack.toml. The builtin-pack-imports doctor check repairs them if
	// they go missing.
	addBuiltinImportsToInitPack(&packCfg, cityCfg.Beads.Provider)
	content, err := cityCfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	content = withInitMailRetentionExample(content)
	logInitProgress(stdout, 4, "Writing pack.toml")
	wrotePack, err := writeInitPackTomlOpts(fs, cityPath, packCfg, preserveExisting)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !wrotePack {
		fmt.Fprintln(stdout, "Preserved existing pack.toml.") //nolint:errcheck // best-effort stdout
	}
	logInitProgress(stdout, 5, "Writing city configuration")
	wroteCity, err := writeInitFile(fs, tomlPath, content, preserveExisting)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !wroteCity {
		fmt.Fprintln(stdout, "Preserved existing city.toml.") //nolint:errcheck // best-effort stdout
	}
	if err := persistInitWorkspaceIdentity(fs, cityPath, tomlPath, &cityCfg, cityName, cityPrefix); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// When a hosted/external Dolt endpoint was supplied, write the full
	// canonical external config now (R2/R3/R4/R5) so the unconditional
	// initDirIfReady that follows resolves the city as external and skips the
	// managed-local Dolt bootstrap. Reject incompatible effective backends
	// (file or doltlite) before writing any canonical files so a rejected init
	// leaves no mixed ledger state.
	if wiz.hostedDolt.enabled() {
		if err := hostedDoltBackendError(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := applyInitHostedDoltCanonicalConfig(fs, cityPath, cityPrefix, wiz.hostedDolt); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// Write .gitignore entries for city-managed directories.
	if err := ensureGitignoreEntries(fs, cityPath, cityGitignoreEntries); err != nil {
		fmt.Fprintf(stderr, "gc init: writing .gitignore: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if shouldBootstrapScopedFileStore(&cfg) {
		if err := bootstrapScopedFileProviderCityFS(fs, cityPath); err != nil {
			fmt.Fprintf(stderr, "gc init: bootstrapping file bead store: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	switch {
	case wiz.interactive:
		fmt.Fprintf(stdout, "Created %s config (Level 1) in %q.\n", wiz.configName, cityName) //nolint:errcheck // best-effort stdout
	case wiz.configName == "empty":
		fmt.Fprintln(stdout, "Welcome to Gas City!")                                   //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Initialized bare city %q (no bundled pack).\n", cityName) //nolint:errcheck // best-effort stdout
	case defaultProvider != "":
		fmt.Fprintln(stdout, "Welcome to Gas City!")                                                      //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Initialized city %q with default provider %q.\n", cityName, defaultProvider) //nolint:errcheck // best-effort stdout
	default:
		fmt.Fprintln(stdout, "Welcome to Gas City!")                                     //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Initialized city %q with default mayor agent.\n", cityName) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func wizardDefaultProvider(wiz wizardConfig) string {
	if strings.TrimSpace(wiz.defaultProvider) != "" {
		return strings.TrimSpace(wiz.defaultProvider)
	}
	return strings.TrimSpace(wiz.provider)
}

func wizardProviders(wiz wizardConfig) []string {
	if len(wiz.providers) > 0 {
		return append([]string(nil), wiz.providers...)
	}
	if provider := wizardDefaultProvider(wiz); provider != "" {
		return []string{provider}
	}
	return nil
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
//
// Materializes pack-overlay universal files into cityPath first so the
// Claude override file (.claude/settings.json) is in its final state when
// hooks.Install reads it. Without this, pack overlays are materialized
// asynchronously by tmux/adapter.go:Provider.Start() during the first
// session start. That late write flips .gc/settings.json content on the
// next supervisor reconcile, drifting the CopyFiles fingerprint and
// draining every named session — including ones that never woke. See
// stg-wvpl.
func installClaudeHooks(fs fsys.FS, cityPath string, stderr io.Writer) int {
	materializeCityRootPackOverlays(fs, cityPath, stderr)
	if err := hooks.Install(fs, cityPath, cityPath, []string{"claude"}); err != nil {
		fmt.Fprintf(stderr, "gc init: installing claude hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// materializeCityRootPackOverlays applies pack-overlay universal files into
// cityPath. It mirrors the universal portion of overlay.CopyDirForProviders
// for the cityRoot WorkDir case — agents whose work_dir defaults to cityRoot
// (mayor, deacon, boot in gastown) cause the same materialization during
// session start. Doing it before installClaude makes the supervisor's first
// reconcile observe a stable .gc/settings.json source.
//
// Per-provider overlay files remain agent-scoped (materialized at session
// start when the agent's provider is known); Claude's settings live outside
// per-provider/, so universal-only is sufficient for the drift bug.
//
// Best-effort: if pack expansion fails (e.g. cityPath has no city.toml yet
// in some early-init paths) or an overlay copy fails, we let installClaude
// proceed against the pre-overlay state. Pre-fix behavior is the failure
// mode, so degrading to it is acceptable.
func materializeCityRootPackOverlays(fs fsys.FS, cityPath string, stderr io.Writer) {
	cfg, _, err := config.LoadWithIncludes(fs, filepath.Join(cityPath, citylayout.CityConfigFile))
	if err != nil || cfg == nil {
		return
	}
	for _, od := range cfg.PackOverlayDirs {
		if err := overlay.CopyDirForProviders(od, cityPath, nil, stderr); err != nil {
			fmt.Fprintf(stderr, "gc init: materializing pack overlay %s: %v\n", od, err) //nolint:errcheck // best-effort stderr
		}
	}
}

func shouldBootstrapScopedFileStore(cfg *config.City) bool {
	if v := strings.TrimSpace(os.Getenv("GC_BEADS")); v != "" {
		return v == "file"
	}
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg.Beads.Provider) == "file"
}

func bootstrapScopedFileProviderCityFS(fs fsys.FS, cityPath string) error {
	if err := fs.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		return err
	}
	if err := fs.WriteFile(fileStoreLayoutMarkerPath(cityPath), []byte(fileStoreLayoutScopedV1+"\n"), 0o644); err != nil {
		return err
	}
	beadsPath := filepath.Join(cityPath, ".gc", "beads.json")
	if _, err := fs.Stat(beadsPath); err == nil {
		return nil
	}
	return fs.WriteFile(beadsPath, []byte("{\"seq\":0,\"beads\":[]}\n"), 0o644)
}

// writeInitAgentPrompts creates the agents/ directory and writes only the
// default prompt scaffolds referenced by the init template's explicit agents.
// This keeps a freshly initialized city aligned with the city.toml it writes
// instead of silently creating additional convention-discoverable agents.
func writeInitAgentPrompts(fs fsys.FS, cityPath string, cfg *config.City, stderr io.Writer, preserveExisting bool) int {
	if err := fs.MkdirAll(filepath.Join(cityPath, "agents"), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cfg == nil {
		return 0
	}
	seen := make(map[string]bool, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		dst, ok := initPromptTemplatePath(agent.PromptTemplate)
		if !ok || seen[dst] {
			continue
		}
		seen[dst] = true
		data, err := defaultPrompts.ReadFile(agent.PromptTemplate)
		if err != nil {
			fmt.Fprintf(stderr, "gc init: reading embedded %s: %v\n", agent.PromptTemplate, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		dst = filepath.Join(cityPath, dst)
		if err := fs.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if _, err := writeInitFile(fs, dst, data, preserveExisting); err != nil {
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

// initFromSkipForSource returns the source-aware skip policy for gc init --from.
// PackV2 templates should not carry the deprecated top-level scripts/ shim
// forward into the new city, but real files and foreign symlink trees remain
// user-owned and are copied through unchanged.
func initFromSkipForSource(srcDir string) overlay.SkipFunc {
	return initFromSkipForSourceFS(fsys.OSFS{}, srcDir)
}

func initFromSkipForSourceFS(srcFS fsys.FS, srcDir string) overlay.SkipFunc {
	skipTopLevelScripts := shouldSkipLegacyTopLevelScripts(srcFS, srcDir)
	return func(relPath string, isDir bool) bool {
		if skipTopLevelScripts {
			top, _, _ := strings.Cut(relPath, string(filepath.Separator))
			if top == "scripts" {
				return true
			}
		}
		return initFromSkip(relPath, isDir)
	}
}

func shouldSkipLegacyTopLevelScripts(srcFS fsys.FS, srcDir string) bool {
	if sourceTemplatePackSchemaFS(srcFS, srcDir) < initPackSchemaVersion {
		return false
	}
	_, ok, err := legacyShimLinksFS(srcDir, sourceTemplateLegacyScriptOriginsFS(srcFS, srcDir), srcFS, srcDir)
	return err == nil && ok
}

func sourceTemplateLegacyScriptOriginsFS(srcFS fsys.FS, srcDir string) []string {
	seen := make(map[string]struct{})
	var dirs []string
	add := func(candidates []string) {
		for _, dir := range candidates {
			dir = filepath.Clean(dir)
			if _, ok := seen[dir]; ok {
				continue
			}
			seen[dir] = struct{}{}
			dirs = append(dirs, dir)
		}
	}

	add(legacyLocalScriptOriginsFS(srcFS, srcDir))

	cfg, _, err := config.LoadWithIncludes(srcFS, filepath.Join(srcDir, "city.toml"))
	if err == nil {
		add(legacyScriptSourceDirsFS(srcFS, cfg.PackDirs))
	}

	return dirs
}

func sourceTemplatePackSchemaFS(srcFS fsys.FS, srcDir string) int {
	data, err := srcFS.ReadFile(filepath.Join(srcDir, "pack.toml"))
	if err != nil {
		return 0
	}
	var pc initPackConfig
	if _, err := toml.Decode(string(data), &pc); err != nil {
		return 0
	}
	return pc.Pack.Schema
}

// overrideCityName reads an existing city.toml, updates workspace.name, and writes it back.
func overrideCityName(f fsys.FS, tomlPath, name string, stderr io.Writer) int {
	cfg, err := loadCityConfigForEditFS(f, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg.Workspace.Name = name
	if err := writeCityConfigForEditFS(f, tomlPath, cfg); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// resolveCityName returns the workspace name to use during init.
// Priority: explicit --name flag > name set on the source/template config >
// target directory basename.
func resolveCityName(nameOverride, sourceName, cityPath string) string {
	return cityinit.ResolveCityName(nameOverride, sourceName, cityPath)
}

func cmdInitFromDirWithOptionsInternal(fromDir string, args []string, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool, noStart bool) int {
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

	return doInitFromDirWithOptionsInternal(srcDir, cityPath, nameOverride, stdout, stderr, skipProviderReadiness, noStart)
}

// doInitFromDir copies an example city directory to a new city path,
// writes machine-local workspace identity to .gc/site.toml, and
// installs the standard runtime scaffold.
func doInitFromDir(srcDir, cityPath string, stdout, stderr io.Writer) int {
	return doInitFromDirWithOptions(srcDir, cityPath, "", stdout, stderr, false)
}

func doInitFromDirWithOptionsFS(fs fsys.FS, srcDir, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
	return doInitFromDirWithOptionsFSInternal(fs, srcDir, cityPath, nameOverride, stdout, stderr, skipProviderReadiness, false)
}

func doInitFromDirWithOptionsFSInternal(fs fsys.FS, srcDir, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool, noStart bool) int {
	srcToml := filepath.Join(srcDir, "city.toml")
	if _, err := os.Stat(srcToml); err != nil {
		fmt.Fprintf(stderr, "gc init --from: source %q has no city.toml\n", srcDir) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cityAlreadyInitializedFS(fs, cityPath) {
		return initAlreadyInitialized(stderr)
	}
	if err := fs.MkdirAll(cityPath, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := overlay.CopyDirWithSkip(srcDir, cityPath, initFromSkipForSourceFS(fs, srcDir), stderr); err != nil {
		fmt.Fprintf(stderr, "gc init --from: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	copiedToml := filepath.Join(cityPath, "city.toml")
	cfg, cityName, cityPrefix, persistSiteIdentity, err := rewriteCopiedInitFromIdentity(fs, cityPath, nameOverride)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if persistSiteIdentity {
		if err := persistInitWorkspaceIdentity(fs, cityPath, copiedToml, cfg, cityName, cityPrefix); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// Create runtime scaffold.
	if err := ensureCityScaffoldFS(fs, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureInitConventionDirs(fs, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Install Claude Code hooks.
	if code := installClaudeHooks(fs, cityPath, stderr); code != 0 {
		return code
	}

	// Write .gitignore entries for city-managed directories.
	if err := ensureGitignoreEntries(fs, cityPath, cityGitignoreEntries); err != nil {
		fmt.Fprintf(stderr, "gc init: writing .gitignore: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if shouldBootstrapScopedFileStore(cfg) {
		if err := bootstrapScopedFileProviderCityFS(fs, cityPath); err != nil {
			fmt.Fprintf(stderr, "gc init: bootstrapping file bead store: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// Resolve formulas and scripts from pack layers.
	expandedCfg, _, loadErr := config.LoadWithIncludes(fsys.OSFS{}, copiedToml)
	if loadErr == nil && len(expandedCfg.FormulaLayers.City) > 0 {
		if rfErr := ResolveFormulas(cityPath, expandedCfg.FormulaLayers.City); rfErr != nil {
			fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort stderr
		}
	}
	if loadErr == nil {
		pruneLegacyConfiguredScripts(cityPath, expandedCfg, func(scope string, err error) {
			fmt.Fprintf(stderr, "gc init: pruning legacy %s scripts: %v\n", scope, err) //nolint:errcheck // best-effort stderr
		})
	}

	fmt.Fprintln(stdout, "Welcome to Gas City!")                                           //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Initialized city %q from %s.\n", cityName, filepath.Base(srcDir)) //nolint:errcheck // best-effort stdout
	return finalizeInit(cityPath, stdout, stderr, initFinalizeOptions{
		skipProviderReadiness: skipProviderReadiness,
		commandName:           "gc init",
		noStart:               noStart,
	})
}

func doInitFromDirWithOptions(srcDir, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
	return doInitFromDirWithOptionsFS(fsys.OSFS{}, srcDir, cityPath, nameOverride, stdout, stderr, skipProviderReadiness)
}

func doInitFromDirWithOptionsInternal(srcDir, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool, noStart bool) int {
	return doInitFromDirWithOptionsFSInternal(fsys.OSFS{}, srcDir, cityPath, nameOverride, stdout, stderr, skipProviderReadiness, noStart)
}

func rewriteCopiedInitFromIdentity(fs fsys.FS, cityPath, nameOverride string) (*config.City, string, string, bool, error) {
	copiedToml := filepath.Join(cityPath, "city.toml")
	data, err := fs.ReadFile(copiedToml)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("reading copied city.toml: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, "", "", false, err
	}

	cityName := resolveCityName(nameOverride, "", cityPath)
	cityPrefix := strings.TrimSpace(cfg.Workspace.Prefix)
	packPath := filepath.Join(cityPath, "pack.toml")
	if _, err := fs.Stat(packPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, "", "", false, err
		}
		cfg.Workspace.Name = cityName
		content, err := cfg.Marshal()
		if err != nil {
			return nil, "", "", false, err
		}
		if err := fs.WriteFile(copiedToml, content, 0o644); err != nil {
			return nil, "", "", false, err
		}
		return cfg, cityName, cityPrefix, false, nil
	}
	cfg.Workspace.Name = ""
	cfg.Workspace.Prefix = ""
	var rigSiteBindings []config.Rig
	if hasInitRigSiteBindings(cfg.Rigs) {
		rigSiteBindings = append([]config.Rig(nil), cfg.Rigs...)
		for i := range cfg.Rigs {
			cfg.Rigs[i].Path = ""
		}
	}

	if len(rigSiteBindings) > 0 {
		writeCfg := *cfg
		writeCfg.Rigs = append([]config.Rig(nil), rigSiteBindings...)
		if err := config.WriteCityAndRigSiteBindingsForEdit(fs, copiedToml, &writeCfg); err != nil {
			return nil, "", "", false, initSiteBindingPersistError(err)
		}
	} else {
		content, err := cfg.Marshal()
		if err != nil {
			return nil, "", "", false, err
		}
		if err := fs.WriteFile(copiedToml, content, 0o644); err != nil {
			return nil, "", "", false, err
		}
	}
	if err := rewriteCopiedInitPackName(fs, cityPath, cityName); err != nil {
		return nil, "", "", false, err
	}
	return cfg, cityName, cityPrefix, true, nil
}

func initSiteBindingPersistError(err error) error {
	return fmt.Errorf("writing .gc/site.toml failed while migrating rig paths; city.toml was restored, fix the site binding write error and retry: %w", err)
}

func rewriteCopiedInitPackName(fs fsys.FS, cityPath, cityName string) error {
	packPath := filepath.Join(cityPath, "pack.toml")
	data, err := fs.ReadFile(packPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading copied pack.toml: %w", err)
	}

	updated, err := rewritePackNameInCopiedPackToml(data, cityName)
	if err != nil {
		return fmt.Errorf("updating copied pack.toml: %w", err)
	}
	if err := fsys.WriteFileAtomic(fs, packPath, updated, 0o644); err != nil {
		return fmt.Errorf("writing copied pack.toml: %w", err)
	}
	return nil
}

func rewritePackNameInCopiedPackToml(data []byte, cityName string) ([]byte, error) {
	lines := splitPreserveNewlines(string(data))
	lineEnding := "\n"
	for _, line := range lines {
		if strings.HasSuffix(line, "\n") {
			lineEnding = "\n"
			break
		}
	}

	inPackTable := false
	sawPackTable := false
	multilineDelimiter := ""
	for i, line := range lines {
		if multilineDelimiter != "" {
			if multilineStringEndsOnLine(line, multilineDelimiter) {
				multilineDelimiter = ""
			}
			continue
		}

		trimmed := strings.TrimSpace(line)
		if isTOMLTableHeader(trimmed) {
			if inPackTable {
				lines = insertPackNameLine(lines, i, cityName, lineEnding)
				return []byte(strings.Join(lines, "")), nil
			}
			inPackTable = isPackTableHeader(trimmed)
			if inPackTable {
				sawPackTable = true
			}
			continue
		}
		if !inPackTable {
			continue
		}
		key, _, hasAssignment := strings.Cut(stripTOMLInlineComment(trimmed), "=")
		if hasAssignment && strings.TrimSpace(key) == "name" {
			lines[i] = rewritePackNameLine(line, cityName)
			return []byte(strings.Join(lines, "")), nil
		}
		if delimiter := startsUnterminatedTOMLMultilineString(line); delimiter != "" {
			multilineDelimiter = delimiter
		}
	}

	if !sawPackTable {
		return nil, fmt.Errorf("pack.toml missing [pack] table")
	}
	lines = insertPackNameLine(lines, len(lines), cityName, lineEnding)
	return []byte(strings.Join(lines, "")), nil
}

func splitPreserveNewlines(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	start := 0
	for i, r := range text {
		if r != '\n' {
			continue
		}
		lines = append(lines, text[start:i+1])
		start = i + 1
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func isTOMLTableHeader(line string) bool {
	return strings.HasPrefix(line, "[")
}

func isPackTableHeader(line string) bool {
	if !strings.HasPrefix(line, "[") {
		return false
	}
	end := strings.Index(line, "]")
	if end == -1 {
		return false
	}
	return strings.TrimSpace(line[1:end]) == "pack"
}

func insertPackNameLine(lines []string, idx int, cityName, lineEnding string) []string {
	inserted := "name = " + strconv.Quote(cityName) + lineEnding
	lines = append(lines, "")
	copy(lines[idx+1:], lines[idx:])
	lines[idx] = inserted
	return lines
}

func rewritePackNameLine(line, cityName string) string {
	indentLen := len(line) - len(strings.TrimLeft(line, " \t"))
	indent := line[:indentLen]
	lineEnding := ""
	if strings.HasSuffix(line, "\n") {
		lineEnding = "\n"
	}
	comment := ""
	if suffix := tomlInlineCommentSuffix(strings.TrimRight(line, "\n")); suffix != "" {
		comment = " " + suffix
	}
	return indent + "name = " + strconv.Quote(cityName) + comment + lineEnding
}

func startsUnterminatedTOMLMultilineString(line string) string {
	code := stripTOMLInlineComment(line)
	for _, delimiter := range []string{`"""`, `'''`} {
		if strings.Count(code, delimiter)%2 == 1 {
			return delimiter
		}
	}
	return ""
}

func multilineStringEndsOnLine(line, delimiter string) bool {
	return strings.Count(line, delimiter)%2 == 1
}

func stripTOMLInlineComment(line string) string {
	if suffix := tomlInlineCommentSuffix(line); suffix != "" {
		return strings.TrimSuffix(line, suffix)
	}
	return line
}

func tomlInlineCommentSuffix(line string) string {
	inBasic := false
	inLiteral := false
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case inBasic:
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inBasic = false
			}
		case inLiteral:
			if ch == '\'' {
				inLiteral = false
			}
		default:
			switch ch {
			case '#':
				return line[i:]
			case '"':
				if strings.HasPrefix(line[i:], `"""`) {
					return ""
				}
				inBasic = true
			case '\'':
				if strings.HasPrefix(line[i:], `'''`) {
					return ""
				}
				inLiteral = true
			}
		}
	}
	return ""
}

func persistInitWorkspaceIdentity(fs fsys.FS, cityPath, cityTomlPath string, cfg *config.City, cityName, cityPrefix string) error {
	if err := config.PersistWorkspaceSiteBinding(fs, cityPath, cityName, cityPrefix); err != nil {
		if restoreErr := restoreLegacyWorkspaceIdentity(fs, cityTomlPath, cfg, cityName, cityPrefix); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restoring legacy workspace identity: %w", restoreErr))
		}
		return err
	}
	return nil
}

func restoreLegacyWorkspaceIdentity(fs fsys.FS, cityTomlPath string, cfg *config.City, cityName, cityPrefix string) error {
	if cfg == nil {
		return nil
	}
	restored := *cfg
	restored.Workspace.Name = strings.TrimSpace(cityName)
	restored.Workspace.Prefix = strings.TrimSpace(cityPrefix)
	restored.ResolvedWorkspaceName = ""
	restored.ResolvedWorkspacePrefix = ""
	content, err := restored.Marshal()
	if err != nil {
		return fmt.Errorf("marshal %q: %w", cityTomlPath, err)
	}
	if err := fs.WriteFile(cityTomlPath, content, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", cityTomlPath, err)
	}
	return nil
}
