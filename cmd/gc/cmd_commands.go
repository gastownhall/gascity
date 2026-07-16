package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

const (
	docgenSkipAnnotation           = "gc.docgen.skip"
	productMetricsClassAnnotation  = "gc.productmetrics.class"
	packCommandClassificationValue = "pack-command"
)

type commandClassification string

const (
	unknownCommandClassification commandClassification = "unknown"
	packCommandClassification    commandClassification = packCommandClassificationValue
)

// packCommandOutcome is the privacy-minimized lifecycle result shared by
// eager and lazy pack dispatch. It deliberately cannot carry a binding, pack
// name, command path, or arguments.
type packCommandOutcome struct {
	handled        bool
	classification commandClassification
	exitCode       int
}

// packCommandAction separates private command resolution from execution. The
// lifecycle may inspect outcome before invoking the closure; only the minimized
// outcome is eligible to cross into command classification or recording.
type packCommandAction struct {
	selected bool
	outcome  packCommandOutcome
	invoke   func() int
}

func unresolvedPackCommandAction() packCommandAction {
	return packCommandAction{outcome: packCommandOutcome{
		classification: unknownCommandClassification,
		exitCode:       1,
	}}
}

func resolvedPackCommandAction(invoke func() int) packCommandAction {
	return packCommandAction{
		selected: true,
		outcome: packCommandOutcome{
			handled:        true,
			classification: packCommandClassification,
		},
		invoke: invoke,
	}
}

// selectedUnknownPackCommandAction represents an invocation that selected a
// discovered namespace but did not resolve to one of its children. selected
// stays private to dispatch: the minimized lifecycle outcome remains the same
// unknown outcome used when no pack namespace matched at all.
func selectedUnknownPackCommandAction(invoke func() int) packCommandAction {
	return packCommandAction{
		selected: true,
		outcome: packCommandOutcome{
			classification: unknownCommandClassification,
			exitCode:       1,
		},
		invoke: invoke,
	}
}

func (action packCommandAction) execute() packCommandOutcome {
	return action.executeReporting(nil)
}

func (action packCommandAction) executeReporting(report func(packCommandOutcome)) packCommandOutcome {
	outcome := action.outcome
	if report != nil {
		report(outcome)
	}
	if !action.selected || action.invoke == nil {
		return outcome
	}
	outcome.exitCode = action.invoke()
	return outcome
}

func (outcome packCommandOutcome) err() error {
	return exitForCode(outcome.exitCode)
}

func addDiscoveredCommandsToRoot(root *cobra.Command, entries []config.DiscoveredCommand, cityPath, cityName string, stdout, stderr io.Writer, warnOnCollision bool) {
	core := coreCommandNames(root)
	grouped := make(map[string][]config.DiscoveredCommand)
	for _, entry := range entries {
		if entry.BindingName == "" {
			continue
		}
		grouped[entry.BindingName] = append(grouped[entry.BindingName], entry)
	}

	bindings := make([]string, 0, len(grouped))
	for binding := range grouped {
		bindings = append(bindings, binding)
	}
	slices.Sort(bindings)

	for _, binding := range bindings {
		if core[binding] {
			if warnOnCollision {
				fmt.Fprintf(stderr, "gc: import binding %q: name shadows core command, skipping\n", binding) //nolint:errcheck
			}
			continue
		}
		nsCmd := newDiscoveredNamespaceCmd(binding, grouped[binding], cityPath, cityName, stdout, stderr)
		root.AddCommand(nsCmd)
		configureDiscoveredGroups(nsCmd)
	}
}

func newDiscoveredNamespaceCmd(binding string, entries []config.DiscoveredCommand, cityPath, cityName string, stdout, stderr io.Writer) *cobra.Command {
	ns := &cobra.Command{
		Use:   binding,
		Short: fmt.Sprintf("Commands from the %s import", binding),
		Annotations: map[string]string{
			docgenSkipAnnotation:          "true",
			productMetricsClassAnnotation: packCommandClassificationValue,
		},
		// NoArgs makes an unknown subcommand ("gc <binding> bogus") fail with
		// "unknown command" and a non-zero exit, matching native command groups.
		// A bare invocation ("gc <binding>") passes NoArgs and falls through to
		// RunE, which still prints help and exits 0. See gastownhall/gascity#3966.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	for _, entry := range sortCommandsForTree(entries) {
		addDiscoveredLeaf(ns, entry, cityPath, cityName, stdout, stderr)
	}

	return ns
}

func addDiscoveredLeaf(root *cobra.Command, entry config.DiscoveredCommand, cityPath, cityName string, stdout, stderr io.Writer) {
	if len(entry.Command) == 0 {
		return
	}

	parent := root
	for _, word := range entry.Command[:len(entry.Command)-1] {
		if existing := findSubcommand(parent, word); existing != nil {
			parent = existing
			continue
		}
		next := &cobra.Command{
			Use: word,
			Annotations: map[string]string{
				productMetricsClassAnnotation: packCommandClassificationValue,
			},
			// Intermediate namespace nodes reject unknown subcommands too, so a
			// deep "gc <binding> repo bogus" fails non-zero like a native group
			// rather than printing help and exiting 0. See gastownhall/gascity#3966.
			Args: cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return c.Help()
			},
		}
		parent.AddCommand(next)
		parent = next
	}

	leafWord := entry.Command[len(entry.Command)-1]
	if existing := findSubcommand(parent, leafWord); existing != nil {
		return
	}

	annotations := map[string]string{}
	annotations[productMetricsClassAnnotation] = packCommandClassificationValue
	if strings.TrimSpace(entry.SourceDir) != "" {
		annotations[jsonSchemaDirAnnotation] = filepath.Join(entry.SourceDir, "schemas")
	}

	leaf := &cobra.Command{
		Use:                leafWord,
		Short:              entry.Description,
		Long:               readDiscoveredHelp(entry),
		Annotations:        annotations,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			action := resolveDiscoveredLeafAction(cmd, args, func() int {
				return runDiscoveredCommand(entry, cityPath, cityName, args, stdin(), stdout, stderr)
			})
			return executeProductMetricsPackAction(cmd, action).err()
		},
	}
	parent.AddCommand(leaf)
}

func configureDiscoveredGroups(cmd *cobra.Command) {
	if cmd.DisableFlagParsing {
		return
	}
	configureDiscoveredGroup(cmd)
	for _, child := range cmd.Commands() {
		configureDiscoveredGroups(child)
	}
}

// configureDiscoveredGroup gives namespaces and intermediate nodes the same
// typed lifecycle behavior as leaves without changing Cobra's canonical help
// rendering. The help wrapper only owns this exact node; descendant leaves
// inherit the renderer without creating a second pack action.
func configureDiscoveredGroup(cmd *cobra.Command) {
	renderHelp := cmd.HelpFunc()
	helpAction := func(helpCmd *cobra.Command, args []string) packCommandAction {
		return resolvedPackCommandAction(func() int {
			renderHelp(helpCmd, args)
			return 0
		})
	}
	cmd.SetHelpFunc(func(helpCmd *cobra.Command, args []string) {
		if helpCmd != cmd {
			renderHelp(helpCmd, args)
			return
		}
		_ = executeProductMetricsPackAction(helpCmd, helpAction(helpCmd, args))
	})
	cmd.RunE = func(runCmd *cobra.Command, args []string) error {
		return executeProductMetricsPackAction(runCmd, helpAction(runCmd, args)).err()
	}
}

func resolvedPackCommandUnknownAction(cmd *cobra.Command, arg string, stderr io.Writer) packCommandAction {
	return selectedUnknownPackCommandAction(func() int {
		fmt.Fprintf(stderr, "gc: unknown command %q\n\n", arg) //nolint:errcheck // best-effort stderr
		printCommandUsage(stderr, cmd)
		return 1
	})
}

func resolvedPackCommandHelpAction(cmd *cobra.Command) packCommandAction {
	return resolvedPackCommandAction(func() int {
		return commandExitCode(cmd.Help())
	})
}

func resolveDiscoveredLeafAction(cmd *cobra.Command, args []string, invoke func() int) packCommandAction {
	if discoveredHelpRequested(args) {
		return resolvedPackCommandHelpAction(cmd)
	}
	return resolvedPackCommandAction(invoke)
}

func findSubcommand(cmd *cobra.Command, name string) *cobra.Command {
	for _, existing := range cmd.Commands() {
		if existing.Name() == name {
			return existing
		}
	}
	return nil
}

func readDiscoveredHelp(entry config.DiscoveredCommand) string {
	if entry.HelpFile == "" {
		return ""
	}
	data, err := os.ReadFile(entry.HelpFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func discoveredHelpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func runDiscoveredCommand(entry config.DiscoveredCommand, cityPath, cityName string, args []string, stdinR io.Reader, stdout, stderr io.Writer) int {
	packDir := entry.PackDir
	if packDir == "" {
		packDir = packRootFromEntryDir(entry.SourceDir, "commands")
	}
	scriptPath := expandScriptTemplate(entry.RunScript, cityPath, cityName, packDir)
	if !filepath.IsAbs(scriptPath) {
		scriptPath = filepath.Join(entry.SourceDir, scriptPath)
	}

	cmd := exec.Command(scriptPath, args...)
	cmd.Stdin = stdinR
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), citylayout.PackRuntimeEnv(cityPath, entry.PackName)...)
	cmd.Env = append(cmd.Env,
		"GC_PACK_DIR="+packDir,
		"GC_PACK_NAME="+entry.PackName,
		"GC_CITY_NAME="+cityName,
	)
	cmd.Env = mergeCanonicalScopeDoltEnv(cmd.Env, cityPath)
	disableProductMetricsForChild(cmd)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gc %s %s: %v\n", entry.BindingName, strings.Join(entry.Command, " "), err) //nolint:errcheck
		return 1
	}
	return 0
}

// mergeCanonicalScopeDoltEnv projects the city's canonical Dolt
// connection into a pack command's environment the same way order
// dispatch does (applyOrderExecCanonicalDoltEnv), so a directly invoked
// pack command (e.g. `gc dolt compact`) targets the same server as its
// scheduled order. Without this, a city configured with an external
// Dolt endpoint runs pack scripts against stale ambient GC_DOLT_* values
// or the inactive managed runtime. When the city has no authoritative
// scope config the environment is returned unchanged and pack scripts
// keep resolving the managed runtime themselves.
//
// Pack commands are a city-level surface: the projection intentionally
// targets the city scope even when the invoking shell carries a
// rig-scoped projection, matching what the same command would see when
// dispatched as a city order.
//
// Unlike the order path, whose resolution input is a freshly built env
// map, the input here is the raw ambient environment. Ambient password
// mirrors are parent-shell state — possibly projected for a different
// scope — and doltauth.ResolveScopedFromEnv would trust a map-provided
// BEADS_DOLT_PASSWORD as already-resolved auth ahead of the endpoint's
// credentials-file lookup. They are therefore stripped from the
// resolution input, gated on the same authoritativeness check the
// projection itself applies so the non-authoritative pass-through stays
// a strict no-op. The operator overrides are unaffected: doltauth reads
// GC_DOLT_PASSWORD via os.Getenv, not from the resolution map.
func mergeCanonicalScopeDoltEnv(environ []string, cityPath string) []string {
	resolved := make(map[string]string, len(environ))
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			resolved[key] = value
		}
	}
	before := make(map[string]string, len(resolved))
	for key, value := range resolved {
		before[key] = value
	}
	if canonicalScopeDoltProjectionAuthoritative(cityPath) {
		clearProjectedDoltPasswordEnv(resolved)
	}
	applyOrderExecCanonicalDoltEnv(cityPath, cityPath, resolved)

	out := environ
	removed := make([]string, 0, len(before))
	for key := range before {
		if _, ok := resolved[key]; !ok {
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)
	for _, key := range removed {
		out = removeEnvKey(out, key)
	}
	changed := make([]string, 0, len(resolved))
	for key, value := range resolved {
		if prev, ok := before[key]; !ok || prev != value {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	for _, key := range changed {
		out = removeEnvKey(out, key)
		out = append(out, key+"="+resolved[key])
	}
	return out
}

func resolveDiscoveredCommandFallback(args []string, cfg *config.City, cityPath string, stdout, stderr io.Writer) packCommandAction {
	if len(args) == 0 || cfg == nil {
		return unresolvedPackCommandAction()
	}

	binding := args[0]
	var matching []config.DiscoveredCommand
	for _, entry := range cfg.PackCommands {
		if entry.BindingName == binding {
			matching = append(matching, entry)
		}
	}
	if len(matching) == 0 {
		return unresolvedPackCommandAction()
	}

	if len(args) == 1 {
		return resolvedPackCommandAction(func() int {
			printDiscoveredCommandList(stdout, binding, nil, matching)
			return 0
		})
	}

	cityName := loadedCityName(cfg, cityPath)
	sort.SliceStable(matching, func(i, j int) bool {
		return len(matching[i].Command) > len(matching[j].Command)
	})
	if prefix, ok := discoveredHelpPrefix(args[1:]); ok {
		for _, entry := range matching {
			if slices.Equal(prefix, entry.Command) {
				return resolvedPackCommandAction(func() int {
					printDiscoveredCommandHelp(stdout, entry)
					return 0
				})
			}
		}
		if discoveredCommandPrefixExists(matching, prefix) {
			return resolvedPackCommandAction(func() int {
				printDiscoveredCommandList(stdout, binding, prefix, matching)
				return 0
			})
		}
	}
	for _, entry := range matching {
		if len(args)-1 < len(entry.Command) {
			continue
		}
		if slices.Equal(args[1:1+len(entry.Command)], entry.Command) {
			commandArgs := slices.Clone(args[1+len(entry.Command):])
			if discoveredHelpRequested(commandArgs) {
				return resolvedPackCommandAction(func() int {
					printDiscoveredCommandHelp(stdout, entry)
					return 0
				})
			}
			return resolvedPackCommandAction(func() int {
				return runDiscoveredCommand(entry, cityPath, cityName, commandArgs, stdin(), stdout, stderr)
			})
		}
	}

	knownPrefix := make([]string, 0, len(args)-1)
	for _, word := range args[1:] {
		candidate := append(slices.Clone(knownPrefix), word)
		if !discoveredCommandPrefixExists(matching, candidate) {
			return resolvedDiscoveredCommandUnknownAction(binding, knownPrefix, word, matching, cityPath, cityName, stdout, stderr)
		}
		knownPrefix = candidate
	}
	if len(knownPrefix) > 0 {
		prefix := slices.Clone(knownPrefix)
		return resolvedPackCommandAction(func() int {
			printDiscoveredCommandList(stdout, binding, prefix, matching)
			return 0
		})
	}

	return unresolvedPackCommandAction()
}

func resolvedDiscoveredCommandUnknownAction(binding string, prefix []string, unknown string, entries []config.DiscoveredCommand, cityPath, cityName string, stdout, stderr io.Writer) packCommandAction {
	root := &cobra.Command{Use: "gc"}
	root.SetOut(stdout)
	root.SetErr(stderr)
	namespace := newDiscoveredNamespaceCmd(binding, entries, cityPath, cityName, stdout, stderr)
	root.AddCommand(namespace)
	configureDiscoveredGroups(namespace)

	target := namespace
	for _, word := range prefix {
		next := findSubcommand(target, word)
		if next == nil {
			break
		}
		target = next
	}
	return resolvedPackCommandUnknownAction(target, unknown, stderr)
}

func tryDiscoveredCommandFallback(args []string, cfg *config.City, cityPath string, stdout, stderr io.Writer) packCommandOutcome {
	return resolveDiscoveredCommandFallback(args, cfg, cityPath, stdout, stderr).execute()
}

func discoveredHelpPrefix(args []string) ([]string, bool) {
	for i, arg := range args {
		if arg == "--" {
			return nil, false
		}
		if arg == "--help" || arg == "-h" {
			return args[:i], true
		}
	}
	return nil, false
}

func printDiscoveredCommandHelp(stdout io.Writer, entry config.DiscoveredCommand) {
	if long := readDiscoveredHelp(entry); long != "" {
		fmt.Fprintln(stdout, long) //nolint:errcheck
		return
	}
	if entry.Description != "" {
		fmt.Fprintln(stdout, entry.Description) //nolint:errcheck
		return
	}
	fmt.Fprintf(stdout, "Pack command: %s\n", strings.Join(entry.Command, " ")) //nolint:errcheck
}

func printDiscoveredCommandList(stdout io.Writer, binding string, prefix []string, entries []config.DiscoveredCommand) {
	title := binding
	if len(prefix) > 0 {
		title += " " + strings.Join(prefix, " ")
	}
	fmt.Fprintf(stdout, "Available commands for %s:\n", title) //nolint:errcheck
	for _, entry := range sortCommandsForTree(entries) {
		if !commandHasPrefix(entry.Command, prefix) {
			continue
		}
		name := strings.Join(entry.Command, " ")
		if len(prefix) > 0 {
			name = strings.Join(entry.Command[len(prefix):], " ")
		}
		if name == "" {
			continue
		}
		fmt.Fprintf(stdout, "  %-20s %s\n", name, entry.Description) //nolint:errcheck
	}
}

func discoveredCommandPrefixExists(entries []config.DiscoveredCommand, prefix []string) bool {
	for _, entry := range entries {
		if commandHasPrefix(entry.Command, prefix) {
			return true
		}
	}
	return false
}

func commandHasPrefix(command, prefix []string) bool {
	if len(prefix) > len(command) {
		return false
	}
	return slices.Equal(command[:len(prefix)], prefix)
}

func sortCommandsForTree(entries []config.DiscoveredCommand) []config.DiscoveredCommand {
	sorted := append([]config.DiscoveredCommand(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if len(sorted[i].Command) != len(sorted[j].Command) {
			return len(sorted[i].Command) < len(sorted[j].Command)
		}
		return strings.Join(sorted[i].Command, "\x00") < strings.Join(sorted[j].Command, "\x00")
	})
	return sorted
}

func packRootFromEntryDir(sourceDir, topLevel string) string {
	marker := string(filepath.Separator) + topLevel + string(filepath.Separator)
	if idx := strings.LastIndex(sourceDir, marker); idx >= 0 {
		return sourceDir[:idx]
	}
	return filepath.Dir(sourceDir)
}
