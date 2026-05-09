package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/spf13/cobra"
)

const initPackSchemaVersion = 2

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
	Formulas       config.FormulasConfig          `toml:"formulas,omitempty"`
	Patches        config.Patches                 `toml:"patches,omitempty"`
	Doctor         []config.PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands       []config.PackCommandEntry      `toml:"commands,omitempty"`
	Global         config.PackGlobal              `toml:"global,omitempty"`
}

var initConventionDirs = cityinit.InitConventionDirs()

// wizardConfig carries the results of the interactive init wizard (or defaults
// for non-interactive paths). doInit uses it to decide which config to write.
type wizardConfig struct {
	interactive      bool   // true if the wizard ran with user interaction
	configName       string // canonical values: "minimal", "gastown", or "custom"
	provider         string // built-in provider key, or "" if startCommand set
	startCommand     string // custom start command (workspace-level)
	bootstrapProfile string // hosted bootstrap profile, or "" for local defaults
}

// defaultWizardConfig returns a non-interactive wizardConfig that produces
// a single mayor agent with no provider.
func defaultWizardConfig() wizardConfig {
	return wizardConfig{configName: "minimal"}
}

func canBootstrapExistingCity(wiz wizardConfig) bool {
	return wiz == defaultWizardConfig()
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
	fmt.Fprintln(stdout, "  1. minimal   — default coding agent (default)")         //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  2. gastown   — multi-agent orchestration pack")         //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "  3. custom    — empty workspace, configure it yourself") //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Template [1]: ")                                           //nolint:errcheck // best-effort stdout

	configChoice := readLine(br)
	configName := "minimal"

	switch configChoice {
	case "", "1", "minimal", "tutorial":
		configName = "minimal"
	case "2", "gastown":
		configName = "gastown"
	case "3", "custom":
		configName = "custom"
	default:
		fmt.Fprintf(stdout, "Unknown template %q, using minimal.\n", configChoice) //nolint:errcheck // best-effort stdout
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
	var providerFlag string
	var bootstrapProfileFlag string
	var skipProviderReadiness bool
	var preserveExisting bool
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Initialize a new city",
		Long: `Create a new Gas City workspace in the given directory (or cwd).

Runs an interactive wizard to choose a config template and coding agent
provider. Creates the .gc/ runtime directory plus pack.toml, city.toml,
the standard top-level directories, and .template.md prompt templates, then
materializes builtin packs under .gc/system/packs. Use --provider to create the default minimal city
non-interactively, or --file to initialize from an existing TOML config file.

Pass --preserve-existing to keep any pre-authored pack.toml, city.toml, or
agent prompt files in the target directory (useful when bootstrapping a
committed workspace — e.g. from a bootstrap.sh shipped in the repo).`,
		Example: `  gc init
  gc init ~/my-city
  gc init --provider codex ~/my-city
  gc init --provider codex --bootstrap-profile k8s-cell /city
  gc init --name my-city
  gc init --from ~/elan --name elan /city
  gc init --file examples/gastown.toml ~/bright-lights
  gc init --file city.toml --preserve-existing .`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if fromFlag != "" {
				return exitForCode(cmdInitFromDirWithOptions(fromFlag, args, nameFlag, stdout, stderr, skipProviderReadiness))
			}
			if fileFlag != "" {
				return exitForCode(cmdInitFromFileWithOptions(fileFlag, args, nameFlag, stdout, stderr, skipProviderReadiness, preserveExisting))
			}
			return exitForCode(cmdInitWithOptions(args, providerFlag, bootstrapProfileFlag, nameFlag, stdout, stderr, skipProviderReadiness, preserveExisting))
		},
	}
	cmd.Flags().StringVar(&fileFlag, "file", "", "path to a TOML file to use as city.toml")
	cmd.Flags().StringVar(&fromFlag, "from", "", "path to an example city directory to copy")
	cmd.Flags().StringVar(&nameFlag, "name", "", "workspace name (default: target directory basename)")
	cmd.Flags().StringVar(&providerFlag, "provider", "", "built-in workspace provider to use for the default mayor config")
	cmd.Flags().StringVar(&bootstrapProfileFlag, "bootstrap-profile", "", "bootstrap profile to apply for hosted/container defaults")
	cmd.Flags().BoolVar(&skipProviderReadiness, "skip-provider-readiness", false, "skip provider login/readiness checks during init and continue startup")
	cmd.Flags().BoolVar(&preserveExisting, "preserve-existing", false, "keep any pre-authored pack.toml, city.toml, or agent prompt files instead of overwriting them")
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
	return cmdInitWithOptions(args, providerFlag, bootstrapProfileFlag, "", stdout, stderr, false, false)
}

func cmdInitWithOptions(args []string, providerFlag, bootstrapProfileFlag, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool) int {
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
	if code := doInit(fsys.OSFS{}, cityPath, wiz, nameOverride, stdout, stderr, preserveExisting); code != 0 {
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
		configName:       "minimal",
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
	return cityinit.NormalizeBootstrapProfile(profile)
}

func initPromptTemplatePath(templatePath string) (string, bool) {
	if !strings.HasPrefix(templatePath, citylayout.PromptsRoot+string(filepath.Separator)) {
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
		Formulas      *config.FormulasConfig         `toml:"formulas,omitempty"`
		Patches       *config.Patches                `toml:"patches,omitempty"`
		Doctor        []config.PackDoctorEntry       `toml:"doctor,omitempty"`
		Commands      []config.PackCommandEntry      `toml:"commands,omitempty"`
		Global        *config.PackGlobal             `toml:"global,omitempty"`
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
		Doctor:        cfg.Doctor,
		Commands:      cfg.Commands,
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
//   - pack.toml owns the portable definition: [pack], [[agent]],
//     [[named_session]], [imports.*], [providers.*], agent/service
//     patches, formulas, and agent_defaults.
//   - city.toml keeps only runtime-local deployment settings (e.g.
//     workspace.provider, workspace.start_command, api, daemon, beads).
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
	cityCfg.Providers = nil
	cityCfg.Services = nil
	cityCfg.Formulas = config.FormulasConfig{}
	cityCfg.Patches = config.Patches{}
	cityCfg.AgentDefaults = config.AgentDefaults{}
	cityCfg.AgentsDefaults = config.AgentDefaults{}
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
	if len(cfg.Providers) > 0 {
		packCfg.Providers = make(map[string]config.ProviderSpec, len(cfg.Providers))
		for name, spec := range cfg.Providers {
			packCfg.Providers[name] = spec
		}
	}
	packCfg.AgentDefaults = cfg.AgentDefaults
	if isZeroValue(packCfg.AgentDefaults) && !isZeroValue(cfg.AgentsDefaults) {
		packCfg.AgentDefaults = cfg.AgentsDefaults
	}
	packCfg.Formulas = cfg.Formulas
	packCfg.Patches = cfg.Patches
	for _, svc := range cfg.Services {
		if svc.PublishMode == "direct" {
			cityCfg.Services = append(cityCfg.Services, svc)
			continue
		}
		packCfg.Services = append(packCfg.Services, svc)
	}

	if len(cfg.Workspace.Includes) > 0 {
		packCfg.Pack.Includes = appendUniqueStrings(
			append([]string(nil), packCfg.Pack.Includes...),
			cfg.Workspace.Includes...,
		)
		cityCfg.Workspace.Includes = nil
	}
	if len(cfg.DefaultRigImports) > 0 {
		defaults := packDefaults{
			Rig: packRigDefaults{
				Imports: make(map[string]config.Import, len(cfg.DefaultRigImports)),
			},
		}
		for name, imp := range cfg.DefaultRigImports {
			defaults.Rig.Imports[name] = imp
		}
		packCfg.Defaults = defaults
		cityCfg.Workspace.DefaultRigIncludes = nil
	}
	return packCfg, cityCfg
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

	return cmdInitFromTOMLFileWithOptions(fsys.OSFS{}, fileArg, cityPath, nameOverride, stdout, stderr, skipProviderReadiness, preserveExisting)
}

// cmdInitFromTOMLFile initializes a city by copying a user-provided TOML
// file as city.toml. Creates the runtime scaffold, visible roots, and runs bead init.
func cmdInitFromTOMLFile(fs fsys.FS, tomlSrc, cityPath string, stdout, stderr io.Writer) int {
	return cmdInitFromTOMLFileWithOptions(fs, tomlSrc, cityPath, "", stdout, stderr, false, false)
}

func cmdInitFromTOMLFileWithOptions(fs fsys.FS, tomlSrc, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness, preserveExisting bool) int {
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

	// Re-marshal so the name and rewritten prompt paths are updated.
	content, err := cityCfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Write city.toml — preserved if --preserve-existing is set and a
	// committed city.toml is already in place. Persist the workspace identity
	// regardless so .gc/site.toml agrees with the preserved or newly-written
	// config.
	wroteCity, err := writeInitFile(fs, filepath.Join(cityPath, "city.toml"), content, preserveExisting)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !wroteCity {
		fmt.Fprintln(stdout, "Preserved existing city.toml.") //nolint:errcheck // best-effort stdout
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
	})
}

// doInit is the pure logic for "gc init". It creates the city directory
// structure and writes city.toml. Minimal configs use WizardCity
// when a provider or start command is supplied; otherwise init writes the
// default mayor-only city. Errors if the runtime scaffold already exists. Accepts an
// injected FS for testability.
func doInit(fs fsys.FS, cityPath string, wiz wizardConfig, nameOverride string, stdout, stderr io.Writer, preserveExisting bool) int {
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
	// the pack-owned [[agent]] entries pick up the V2 agents/<name>/
	// prompt.template.md paths we actually scaffold.
	rewriteInitPromptTemplates(&cfg)
	packCfg, cityCfg := splitInitConfig(cityName, &cfg)
	content, err := cityCfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
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
// writes machine-local workspace identity to .gc/site.toml, and
// installs the standard runtime scaffold.
func doInitFromDir(srcDir, cityPath string, stdout, stderr io.Writer) int {
	return doInitFromDirWithOptions(srcDir, cityPath, "", stdout, stderr, false)
}

func doInitFromDirWithOptionsFS(fs fsys.FS, srcDir, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
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
	})
}

func doInitFromDirWithOptions(srcDir, cityPath, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
	return doInitFromDirWithOptionsFS(fsys.OSFS{}, srcDir, cityPath, nameOverride, stdout, stderr, skipProviderReadiness)
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

	content, err := cfg.Marshal()
	if err != nil {
		return nil, "", "", false, err
	}
	if err := fs.WriteFile(copiedToml, content, 0o644); err != nil {
		return nil, "", "", false, err
	}
	if err := rewriteCopiedInitPackName(fs, cityPath, cityName); err != nil {
		return nil, "", "", false, err
	}
	return cfg, cityName, cityPrefix, true, nil
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
