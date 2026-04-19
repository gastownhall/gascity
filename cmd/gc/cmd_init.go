package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/spf13/cobra"
)

const initPackSchemaVersion = 2

// initExitAlreadyInitialized is the exit code `gc init` uses when the
// target already contains a city. Callers (notably the supervisor HTTP
// handler behind POST /v0/city) dispatch on this exit code instead of
// string-matching stderr.
const initExitAlreadyInitialized = 2

type initPackMeta struct {
	Name       string                   `toml:"name"`
	Version    string                   `toml:"version,omitempty"`
	Schema     int                      `toml:"schema"`
	RequiresGC string                   `toml:"requires_gc,omitempty"`
	Includes   []string                 `toml:"includes,omitempty"`
	Requires   []config.PackRequirement `toml:"requires,omitempty"`
}

type packDefaults struct {
	Rig packRigDefaults `toml:"rig,omitempty"`
}

type packRigDefaults struct {
	Imports map[string]config.Import `toml:"imports,omitempty"`
}

type initPackConfig struct {
	// Keep this layout in lockstep with internal/config.packConfig so
	// pack.toml write paths in cmd/gc can round-trip the canonical root
	// pack shape without dropping supported fields.
	Pack          initPackMeta                   `toml:"pack"`
	Imports       map[string]config.Import       `toml:"imports,omitempty"`
	AgentDefaults config.AgentDefaults           `toml:"agent_defaults,omitempty"`
	Defaults      packDefaults                   `toml:"defaults,omitempty"`
	Agents        []config.Agent                 `toml:"agent"`
	NamedSessions []config.NamedSession          `toml:"named_session,omitempty"`
	Services      []config.Service               `toml:"service,omitempty"`
	Providers     map[string]config.ProviderSpec `toml:"providers,omitempty"`
	Formulas      config.FormulasConfig          `toml:"formulas,omitempty"`
	Patches       config.Patches                 `toml:"patches,omitempty"`
	Doctor        []config.PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands      []config.PackCommandEntry      `toml:"commands,omitempty"`
	Global        config.PackGlobal              `toml:"global,omitempty"`
}

var initConventionDirs = []string{
	"agents",
	"commands",
	"doctor",
	citylayout.FormulasRoot,
	citylayout.OrdersRoot,
	"template-fragments",
	"overlays",
	"assets",
}

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

const initProgressSteps = 6

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
provider. Creates the .gc/ runtime directory plus pack.toml, city.toml,
the standard top-level directories, and .template.md prompt templates, then
writes the default formulas. Use --provider to create the default mayor city
non-interactively, or --file to initialize from an existing TOML config file.`,
		Example: `  gc init
  gc init ~/my-city
  gc init --provider codex ~/my-city
  gc init --provider codex --bootstrap-profile k8s-cell /city
  gc init --name my-city
  gc init --from ~/elan --name elan /city
  gc init --file examples/gastown.toml ~/bright-lights`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
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
			if cmdInitWithOptions(args, providerFlag, bootstrapProfileFlag, nameFlag, stdout, stderr, skipProviderReadiness) != 0 {
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
	return cmdInitWithOptions(args, providerFlag, bootstrapProfileFlag, "", stdout, stderr, false)
}

func cmdInitWithOptions(args []string, providerFlag, bootstrapProfileFlag, nameOverride string, stdout, stderr io.Writer, skipProviderReadiness bool) int {
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
	if code := doInit(fsys.OSFS{}, cityPath, wiz, nameOverride, stdout, stderr); code != 0 {
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

// rewriteInitPromptTemplates rewrites the mayor agent's legacy prompt_template
// from "prompts/mayor.md" to the V2 "agents/mayor/prompt.template.md" path
// that writeInitAgentPrompts actually scaffolds. Other agents are left alone:
// we only ship a scaffold for mayor, so rewriting e.g. "prompts/worker.md"
// would silently point the config at a file that never gets created.
func rewriteInitPromptTemplates(cfg *config.City) {
	if cfg == nil {
		return
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].Name != "mayor" {
			continue
		}
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

func writeInitPackToml(fs fsys.FS, cityPath string, packCfg initPackConfig) error {
	content, err := marshalInitPackConfig(packCfg)
	if err != nil {
		return err
	}
	return fs.WriteFile(filepath.Join(cityPath, "pack.toml"), content, 0o644)
}

func marshalInitPackConfig(cfg initPackConfig) ([]byte, error) {
	type encodedInitPackConfig struct {
		Pack          initPackMeta                   `toml:"pack"`
		Imports       map[string]config.Import       `toml:"imports,omitempty"`
		AgentDefaults *config.AgentDefaults          `toml:"agent_defaults,omitempty"`
		Defaults      packDefaults                   `toml:"defaults,omitempty"`
		Agents        []config.Agent                 `toml:"agent"`
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
		Pack:          cfg.Pack,
		Imports:       cfg.Imports,
		Defaults:      cfg.Defaults,
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
	if len(cfg.Workspace.GlobalFragments) > 0 {
		packCfg.AgentDefaults.AppendFragments = appendUniqueStrings(
			append([]string(nil), packCfg.AgentDefaults.AppendFragments...),
			cfg.Workspace.GlobalFragments...,
		)
		cityCfg.Workspace.GlobalFragments = nil
	}

	return packCfg, cityCfg
}

func decodeInitPackTemplate(data []byte, cityName string) (initPackConfig, error) {
	cfg := newInitPackConfig(cityName)
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return initPackConfig{}, fmt.Errorf("parse pack template: %w", err)
	}
	// Fresh init always materializes a city-scoped root pack, so the target
	// city name and current schema override any template header identity.
	cfg.Pack.Name = cityName
	cfg.Pack.Schema = initPackSchemaVersion
	return cfg, nil
}

func applyInitPackTemplateExtras(dst *initPackConfig, src initPackConfig) {
	if dst == nil {
		return
	}
	dst.Pack.Version = src.Pack.Version
	dst.Pack.RequiresGC = src.Pack.RequiresGC
	dst.Pack.Includes = appendUniqueStrings(
		append([]string(nil), dst.Pack.Includes...),
		src.Pack.Includes...,
	)
	dst.Pack.Requires = append([]config.PackRequirement(nil), src.Pack.Requires...)
	dst.Doctor = append([]config.PackDoctorEntry(nil), src.Doctor...)
	dst.Commands = append([]config.PackCommandEntry(nil), src.Commands...)
	dst.Global = src.Global
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
	cityName := resolveCityName(nameOverride, cityPath)
	templatePack, err := decodeInitPackTemplate(data, cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg.Workspace.Name = cityName

	// Create directory structure.
	if cityAlreadyInitializedFS(fs, cityPath) {
		fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
		return initExitAlreadyInitialized
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

	// Write prompt scaffolds only for the explicit agents declared by the template.
	if code := writeInitAgentPrompts(fs, cityPath, cfg, stderr); code != 0 {
		return code
	}

	// Rewrite legacy prompt paths only after we've copied the embedded prompt
	// scaffolds that still live under prompts/.
	rewriteInitPromptTemplates(cfg)
	packCfg, cityCfg := splitInitConfig(cityName, cfg)
	applyInitPackTemplateExtras(&packCfg, templatePack)

	// Re-marshal so the name and rewritten prompt paths are updated.
	content, err := cityCfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := writeInitPackToml(fs, cityPath, packCfg); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Write city.toml.
	if err := fs.WriteFile(filepath.Join(cityPath, "city.toml"), content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Resolve formulas symlinks only after pack.toml and city.toml are both
	// on disk so root-pack includes participate in the initial materialization.
	formulasInitDir := filepath.Join(cityPath, citylayout.FormulasRoot)
	if rfErr := ResolveFormulas(cityPath, []string{formulasInitDir}); rfErr != nil {
		fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort stderr
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
// structure and writes city.toml. Tutorial configs use WizardCity
// when a provider or start command is supplied; otherwise init writes the
// default mayor-only city. Errors if the runtime scaffold already exists. Accepts an
// injected FS for testability.
func doInit(fs fsys.FS, cityPath string, wiz wizardConfig, nameOverride string, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, citylayout.CityConfigFile)
	if cityHasScaffoldFS(fs, cityPath) {
		fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
		return initExitAlreadyInitialized
	}
	if _, err := fs.Stat(tomlPath); err == nil {
		if !canBootstrapExistingCity(wiz) {
			fmt.Fprintln(stderr, "gc init: already initialized") //nolint:errcheck // best-effort stderr
			return initExitAlreadyInitialized
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
		cityName := resolveCityName(nameOverride, cityPath)
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
	cityName := resolveCityName(nameOverride, cityPath)
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

	// Write prompt files only for the agents declared by the init template.
	logInitProgress(stdout, 3, "Writing default prompts")
	if code := writeInitAgentPrompts(fs, cityPath, &cfg, stderr); code != 0 {
		return code
	}

	// Write pack.toml + city.toml in the transitional v2 split: pack.toml
	// owns portable definition, while city.toml keeps deployment/runtime
	// state. We intentionally retain current compatibility fields such as
	// workspace.provider in city.toml until the runtime cutover is complete.
	rewriteInitPromptTemplates(&cfg)
	packCfg, cityCfg := splitInitConfig(cityName, &cfg)
	content, err := cityCfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logInitProgress(stdout, 5, "Writing pack.toml")
	if err := writeInitPackToml(fs, cityPath, packCfg); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logInitProgress(stdout, 6, "Writing city configuration")
	if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Default formulas/orders now arrive via the root pack layer, so resolve
	// them after the split config has been written to disk.
	formulasDir := filepath.Join(cityPath, citylayout.FormulasRoot)
	if err := ResolveFormulas(cityPath, []string{formulasDir}); err != nil {
		fmt.Fprintf(stderr, "gc init: resolving formulas: %v\n", err) //nolint:errcheck // best-effort stderr
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

func applyBootstrapProfile(cfg *config.City, profile string) {
	if profile == bootstrapProfileK8sCell {
		cfg.API.Port = config.DefaultAPIPort
		cfg.API.Bind = "0.0.0.0"
		cfg.API.AllowMutations = true
	}
}

// installClaudeHooks writes Claude Code hook settings for the city.
// Delegates to hooks.Install which is idempotent (won't overwrite existing files).
// The stderr prefix is neutral ("claude hooks:") because this function runs
// both at `gc init` time and on every reconciler tick via resolveTemplate.
func installClaudeHooks(fs fsys.FS, cityPath string, stderr io.Writer) int {
	if err := hooks.Install(fs, cityPath, cityPath, []string{"claude"}); err != nil {
		fmt.Fprintf(stderr, "claude hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// writeInitAgentPrompts writes the mayor scaffold prompt into the V2
// agents/mayor/ directory when the init template includes a mayor agent.
// Other implicit agents use prompts shipped by the core bootstrap pack at
// runtime, so they don't need init-time scaffolding.
func writeInitAgentPrompts(fs fsys.FS, cityPath string, cfg *config.City, stderr io.Writer) int {
	if err := fs.MkdirAll(filepath.Join(cityPath, "agents"), 0o755); err != nil {
		fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cfg == nil {
		return 0
	}
	for _, agent := range cfg.Agents {
		if agent.Name != "mayor" {
			continue
		}
		data, err := defaultPrompts.ReadFile("prompts/mayor.md")
		if err != nil {
			fmt.Fprintf(stderr, "gc init: reading embedded mayor prompt: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		dst := filepath.Join(cityPath, "agents", "mayor", "prompt.template.md")
		if err := fs.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := fs.WriteFile(dst, data, 0o644); err != nil {
			fmt.Fprintf(stderr, "gc init: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
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
// Priority: explicit --name flag > target directory basename.
func resolveCityName(nameOverride, cityPath string) string {
	if nameOverride != "" {
		return nameOverride
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
		return initExitAlreadyInitialized
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
	cityName := resolveCityName(nameOverride, cityPath)
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

	// Ensure V2 convention directories (formulas, orders, prompts, etc.)
	// exist even when the copied template doesn't ship them. Compose
	// references these paths unconditionally, so tests that stat the
	// computed formula layers expect them to resolve.
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
