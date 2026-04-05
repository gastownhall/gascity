package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

var (
	initProbeProvidersReadiness = api.ProbeProviders
	errInitProviderPreflight    = errors.New("provider readiness preflight failed")
)

type initFinalizeOptions struct {
	skipProviderReadiness bool
	showProgress          bool
	commandName           string
}

type initProviderTarget struct {
	RefName     string
	ProbeName   string
	DisplayName string
}

func finalizeInit(cityPath string, stdout, stderr io.Writer, opts initFinalizeOptions) int {
	MaterializeBeadsBdScript(cityPath) //nolint:errcheck // best-effort; only needed for bd provider
	MaterializeBuiltinPacks(cityPath)  //nolint:errcheck // best-effort; only needed for bd provider

	// Check hard binary dependencies before handing off to the supervisor.
	// Without this, missing deps (tmux, git, dolt, bd) cause the supervisor
	// to fail-loop silently — the user never sees the error.
	if missing := checkHardDependencies(cityPath); len(missing) > 0 {
		fmt.Fprintf(stderr, "%s: missing required dependencies:\n\n", opts.commandName) //nolint:errcheck // best-effort stderr
		for _, dep := range missing {
			fmt.Fprintf(stderr, "  - %s", dep.name) //nolint:errcheck // best-effort stderr
			if dep.installHint != "" {
				fmt.Fprintf(stderr, "\n    Install: %s", dep.installHint) //nolint:errcheck // best-effort stderr
			}
			fmt.Fprintln(stderr) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(stderr)                                                                                 //nolint:errcheck // best-effort stderr
		fmt.Fprintf(stderr, "%s: install the missing dependencies, then run 'gc start'\n", opts.commandName) //nolint:errcheck // best-effort stderr
		return 1
	}

	if opts.showProgress {
		if opts.skipProviderReadiness {
			logInitProgress(stdout, 6, "Skipping provider readiness checks")
		} else {
			logInitProgress(stdout, 6, "Checking provider readiness")
		}
	}
	if err := fetchCityPacksIfNeeded(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: fetching packs: %v\n", opts.commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !opts.skipProviderReadiness {
		if err := runInitProviderPreflight(cityPath, stdout, stderr, opts.commandName); err != nil {
			return 1
		}
	} else if !opts.showProgress && stdout != nil {
		fmt.Fprintln(stdout, "Skipping provider readiness checks.") //nolint:errcheck // best-effort stdout
	}

	// Load config to resolve explicit HQ prefix (workspace.prefix field).
	// Config must be loadable at this point — using DeriveBeadsPrefix as a
	// silent fallback would create a prefix mismatch between init and runtime.
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: loading config for prefix resolution: %v\n", opts.commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	prefix := config.EffectiveHQPrefix(cfg)
	if _, err := initDirIfReady(cityPath, cityPath, prefix); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", opts.commandName, err)        //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, `hint: run "gc doctor" for diagnostics`) //nolint:errcheck // best-effort stderr
		return 1
	}
	if opts.showProgress {
		logInitProgress(stdout, 7, "Registering city with supervisor")
	}
	return registerCityWithSupervisor(cityPath, stdout, stderr, opts.commandName, opts.showProgress)
}

func maybePrintWizardProviderGuidance(wiz wizardConfig, stdout io.Writer) {
	if !wiz.interactive || wiz.provider == "" || stdout == nil {
		return
	}
	if !api.SupportsProviderReadiness(wiz.provider) {
		return
	}
	items, err := initProbeProvidersReadiness(context.Background(), []string{wiz.provider}, false)
	if err != nil {
		return
	}
	item, ok := items[wiz.provider]
	if !ok {
		return
	}
	msg := wizardProviderGuidanceMessage(item)
	if msg == "" {
		return
	}
	fmt.Fprintln(stdout, "")  //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, msg) //nolint:errcheck // best-effort stdout
}

func wizardProviderGuidanceMessage(item api.ReadinessItem) string {
	switch item.Status {
	case api.ProbeStatusConfigured:
		return ""
	case api.ProbeStatusNeedsAuth:
		return fmt.Sprintf("Note: %s is not signed in yet. gc init will stop before startup until it is configured.", item.DisplayName)
	case api.ProbeStatusNotInstalled:
		return fmt.Sprintf("Note: %s is not installed. gc init will stop before startup until it is available.", item.DisplayName)
	case api.ProbeStatusInvalidConfiguration:
		return fmt.Sprintf("Note: %s is configured in a mode Gas City cannot use. gc init will stop before startup until it is fixed.", item.DisplayName)
	case api.ProbeStatusProbeError:
		return fmt.Sprintf("Note: Gas City could not verify %s yet. gc init will check again before startup.", item.DisplayName)
	default:
		return ""
	}
}

func runInitProviderPreflight(cityPath string, stdout, stderr io.Writer, commandName string) error {
	// Materialize gastown packs before loading config — config.LoadWithIncludes
	// resolves includes = ["packs/gastown"] which requires pack.toml on disk.
	if err := MaterializeGastownPacks(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: materializing gastown packs: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return errInitProviderPreflight
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: city created, but startup is blocked by configuration loading\n", commandName) //nolint:errcheck // best-effort stderr
		fmt.Fprintf(stderr, "%s: loading config for provider readiness: %v\n", commandName, err)                //nolint:errcheck // best-effort stderr
		fmt.Fprintf(stderr, "%s: fix the config issue, then run 'gc start'\n", commandName)                     //nolint:errcheck // best-effort stderr
		return errInitProviderPreflight
	}
	ensureInitArtifacts(cityPath, cfg, stderr, commandName)
	targets, warnings, err := collectInitProviderTargets(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: city created, but startup is blocked by provider resolution\n", commandName) //nolint:errcheck // best-effort stderr
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err)                                                     //nolint:errcheck // best-effort stderr
		fmt.Fprintf(stderr, "%s: fix the provider issue, then run 'gc start'\n", commandName)                 //nolint:errcheck // best-effort stderr
		return errInitProviderPreflight
	}
	for _, warning := range warnings {
		fmt.Fprintln(stdout, warning) //nolint:errcheck // best-effort stdout
	}
	if len(targets) == 0 {
		return nil
	}

	probeNames := uniqueProbeNames(targets)
	items, err := initProbeProvidersReadiness(context.Background(), probeNames, true)
	if err != nil {
		fmt.Fprintf(stderr, "%s: city created, but startup is blocked by provider readiness\n", commandName) //nolint:errcheck // best-effort stderr
		fmt.Fprintf(stderr, "%s: checking provider readiness: %v\n", commandName, err)                       //nolint:errcheck // best-effort stderr
		fmt.Fprintf(stderr, "%s: fix the provider issue, then run 'gc start'\n", commandName)                //nolint:errcheck // best-effort stderr
		return errInitProviderPreflight
	}

	var blockers []initProviderTarget
	for _, target := range targets {
		item, ok := items[target.ProbeName]
		if !ok || item.Status == api.ProbeStatusConfigured {
			continue
		}
		blockers = append(blockers, target)
	}
	if len(blockers) == 0 {
		return nil
	}

	fmt.Fprintf(stderr, "%s: city created, but startup is blocked by provider readiness\n", commandName) //nolint:errcheck // best-effort stderr
	fmt.Fprintln(stderr, "")                                                                             //nolint:errcheck // best-effort stderr
	fmt.Fprintln(stderr, "Referenced providers not ready:")                                              //nolint:errcheck // best-effort stderr
	for _, blocker := range blockers {
		item := items[blocker.ProbeName]
		fmt.Fprintf(stderr, "- %s: %s\n", blocker.DisplayName, providerStatusSummary(item.Status)) //nolint:errcheck // best-effort stderr
		if fix := providerStatusFixHint(blocker.ProbeName, item.Status); fix != "" {
			fmt.Fprintf(stderr, "  Fix: %s\n", fix) //nolint:errcheck // best-effort stderr
		}
	}
	fmt.Fprintln(stderr, "")                                                                          //nolint:errcheck // best-effort stderr
	fmt.Fprintf(stderr, "Next: cd %s && gc start\n", shellQuotePath(cityPath))                        //nolint:errcheck // best-effort stderr
	fmt.Fprintf(stderr, "Override: gc init --skip-provider-readiness %s\n", shellQuotePath(cityPath)) //nolint:errcheck // best-effort stderr
	return errInitProviderPreflight
}

func collectInitProviderTargets(cfg *config.City) ([]initProviderTarget, []string, error) {
	builtins := config.BuiltinProviders()
	providerRefs := explicitProviderRefs(cfg)
	targets := make([]initProviderTarget, 0, len(providerRefs))
	var warnings []string
	seenTargets := make(map[string]struct{}, len(providerRefs))
	seenWarnings := make(map[string]struct{}, len(providerRefs))
	for _, ref := range providerRefs {
		if _, err := config.ResolveProvider(&config.Agent{Provider: ref}, &cfg.Workspace, cfg.Providers, initLookPath); err != nil {
			return nil, nil, fmt.Errorf("provider %q: %w", ref, err)
		}

		displayName := ref
		if spec, ok := cfg.Providers[ref]; ok && strings.TrimSpace(spec.DisplayName) != "" {
			displayName = strings.TrimSpace(spec.DisplayName)
		} else if spec, ok := builtins[ref]; ok && strings.TrimSpace(spec.DisplayName) != "" {
			displayName = strings.TrimSpace(spec.DisplayName)
		}

		probeName := providerReadinessProbeName(ref, cfg)
		if probeName == "" {
			if _, ok := seenWarnings[ref]; ok {
				continue
			}
			seenWarnings[ref] = struct{}{}
			warnings = append(warnings,
				fmt.Sprintf("Note: %s is referenced, but Gas City cannot verify its login state automatically yet.", displayName))
			continue
		}

		key := ref + "\x00" + probeName
		if _, ok := seenTargets[key]; ok {
			continue
		}
		seenTargets[key] = struct{}{}
		targets = append(targets, initProviderTarget{
			RefName:     ref,
			ProbeName:   probeName,
			DisplayName: displayName,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].DisplayName < targets[j].DisplayName })
	sort.Strings(warnings)
	return targets, warnings, nil
}

func explicitProviderRefs(cfg *config.City) []string {
	seen := make(map[string]struct{})
	var refs []string
	if name := strings.TrimSpace(cfg.Workspace.Provider); name != "" {
		seen[name] = struct{}{}
		refs = append(refs, name)
	}
	for _, agent := range cfg.Agents {
		if agent.StartCommand != "" {
			continue
		}
		name := strings.TrimSpace(agent.Provider)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		refs = append(refs, name)
	}
	sort.Strings(refs)
	return refs
}

func providerReadinessProbeName(ref string, cfg *config.City) string {
	if api.SupportsProviderReadiness(ref) {
		return ref
	}
	spec, ok := cfg.Providers[ref]
	if !ok {
		return ""
	}
	candidate := strings.TrimSpace(spec.PathCheck)
	if candidate == "" {
		command := strings.TrimSpace(spec.Command)
		if command != "" && !strings.ContainsAny(command, " \t") {
			candidate = command
		}
	}
	candidate = filepath.Base(candidate)
	if api.SupportsProviderReadiness(candidate) {
		return candidate
	}
	return ""
}

func providerStatusSummary(status string) string {
	switch status {
	case api.ProbeStatusNeedsAuth:
		return "needs authentication"
	case api.ProbeStatusNotInstalled:
		return "is not installed"
	case api.ProbeStatusInvalidConfiguration:
		return "has an unsupported configuration"
	case api.ProbeStatusProbeError:
		return "could not be verified"
	default:
		return status
	}
}

func providerStatusFixHint(probeName, status string) string {
	switch probeName {
	case "claude":
		switch status {
		case api.ProbeStatusNeedsAuth:
			return "run `claude auth login`"
		case api.ProbeStatusNotInstalled:
			return "install Claude Code"
		case api.ProbeStatusInvalidConfiguration:
			return "use first-party Claude Code login (`claude.ai` / `firstParty`)"
		case api.ProbeStatusProbeError:
			return "run `claude auth status --json` and fix the local Claude setup"
		}
	case "codex":
		switch status {
		case api.ProbeStatusNeedsAuth:
			return "sign in to Codex CLI with ChatGPT auth"
		case api.ProbeStatusNotInstalled:
			return "install Codex CLI"
		case api.ProbeStatusInvalidConfiguration:
			return "switch Codex CLI to ChatGPT auth; API-key mode is not supported here"
		case api.ProbeStatusProbeError:
			return "check ~/.codex/auth.json and the local Codex installation"
		}
	case "gemini":
		switch status {
		case api.ProbeStatusNeedsAuth:
			return "sign in to Gemini CLI with personal OAuth"
		case api.ProbeStatusNotInstalled:
			return "install Gemini CLI"
		case api.ProbeStatusInvalidConfiguration:
			return "use Gemini CLI personal OAuth; API-key and ADC modes are not supported here"
		case api.ProbeStatusProbeError:
			return "check ~/.gemini/settings.json and oauth_creds.json"
		}
	}
	return ""
}

func uniqueProbeNames(targets []initProviderTarget) []string {
	seen := make(map[string]struct{}, len(targets))
	var names []string
	for _, target := range targets {
		if _, ok := seen[target.ProbeName]; ok {
			continue
		}
		seen[target.ProbeName] = struct{}{}
		names = append(names, target.ProbeName)
	}
	sort.Strings(names)
	return names
}

func shellQuotePath(path string) string {
	return shellQuotePathForOS(path, runtime.GOOS)
}

func shellQuotePathForOS(path, goos string) string {
	if goos == "windows" {
		return shellQuoteWindowsPath(path)
	}
	return shellQuotePOSIXPath(path)
}

func shellQuotePOSIXPath(path string) string {
	if path == "" {
		return "''"
	}
	if strings.IndexFunc(path, func(r rune) bool {
		return (r < 'a' || r > 'z') &&
			(r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') &&
			r != '/' &&
			r != '.' &&
			r != '_' &&
			r != '-'
	}) == -1 {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func shellQuoteWindowsPath(path string) string {
	if path == "" {
		return `""`
	}
	if strings.IndexFunc(path, func(r rune) bool {
		return (r < 'a' || r > 'z') &&
			(r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') &&
			r != '/' &&
			r != '\\' &&
			r != ':' &&
			r != '.' &&
			r != '_' &&
			r != '-'
	}) == -1 {
		return path
	}
	return `"` + strings.ReplaceAll(path, `"`, `""`) + `"`
}

// missingDep describes a hard dependency that is missing or too old.
type missingDep struct {
	name        string
	installHint string
}

// initLookPath is the exec.LookPath function used by checkHardDependencies.
// Tests can override this to simulate missing binaries.
var initLookPath = exec.LookPath

// initRunVersion runs "<binary> version" and returns the first line.
// Tests can override this.
var initRunVersion = func(binary string) (string, error) {
	out, err := exec.Command(binary, "version").Output()
	if err != nil {
		return "", err
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return line, nil
}

// Minimum versions for beads-provider binaries.
const (
	doltMinVersion = "1.80.0" // sql-server features used by gc-beads-bd
	bdMinVersion   = "0.61.0" // BdStore shell-out interface
)

// checkHardDependencies verifies that all required binaries are available
// (and meet minimum version requirements) before handing off to the supervisor.
// Returns a list of missing or outdated deps. Without this check, missing
// binaries cause the supervisor to fail-loop silently and the user never
// sees the actual error.
func checkHardDependencies(cityPath string) []missingDep {
	type dep struct {
		name        string
		installHint string
		minVersion  string      // empty = no version check
		condition   func() bool // if non-nil, only checked when true
	}

	beadsProvider := rawBeadsProvider(cityPath)
	needsBd := beadsProvider == "bd" || beadsProvider == ""

	deps := []dep{
		{
			name:        "tmux",
			installHint: "https://github.com/tmux/tmux/wiki/Installing",
		},
		{
			name:        "jq",
			installHint: "brew install jq (macOS) or apt install jq (Linux)",
		},
		{
			name:        "git",
			installHint: "https://git-scm.com/downloads",
		},
		{
			name:        "dolt",
			installHint: "https://github.com/dolthub/dolt/releases",
			minVersion:  doltMinVersion,
			condition:   func() bool { return needsBd },
		},
		{
			name:        "bd",
			installHint: "https://github.com/gastownhall/beads/releases",
			minVersion:  bdMinVersion,
			condition:   func() bool { return needsBd },
		},
		{
			name:        "flock",
			installHint: "brew install flock (macOS) or apt install util-linux (Linux)",
			condition:   func() bool { return needsBd },
		},
		{
			name:        "pgrep",
			installHint: "brew install proctools (macOS) or apt install procps (Linux)",
		},
		{
			name:        "lsof",
			installHint: "brew install lsof (macOS) or apt install lsof (Linux)",
		},
	}

	var missing []missingDep
	for _, d := range deps {
		if d.condition != nil && !d.condition() {
			continue
		}
		if _, err := initLookPath(d.name); err != nil {
			missing = append(missing, missingDep{
				name:        d.name,
				installHint: d.installHint,
			})
			continue
		}
		if d.minVersion != "" {
			if ver := parseDepVersion(d.name); ver != "" {
				if compareVersions(ver, d.minVersion) < 0 {
					missing = append(missing, missingDep{
						name:        fmt.Sprintf("%s (found v%s, need v%s+)", d.name, ver, d.minVersion),
						installHint: d.installHint,
					})
				}
			}
		}
	}
	return missing
}

// parseDepVersion runs "<binary> version" and extracts a semver-like version string.
// Returns "" if the version cannot be determined (non-fatal).
func parseDepVersion(binary string) string {
	line, err := initRunVersion(binary)
	if err != nil {
		return ""
	}
	// Patterns: "dolt version 1.83.1", "bd version 0.61.0 (3ac028bf: ...)"
	for _, field := range strings.Fields(line) {
		if len(field) > 0 && field[0] >= '0' && field[0] <= '9' && strings.Contains(field, ".") {
			return field
		}
	}
	return ""
}

// compareVersions compares two dot-separated version strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) || i < len(bParts); i++ {
		var ai, bi int
		if i < len(aParts) {
			_, _ = fmt.Sscanf(aParts[i], "%d", &ai)
		}
		if i < len(bParts) {
			_, _ = fmt.Sscanf(bParts[i], "%d", &bi)
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}
