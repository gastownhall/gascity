package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func addPackCommandsToRoot(root *cobra.Command, entries []config.PackCommandInfo, cityPath, cityName string, stdout, stderr io.Writer) {
	discovered := make([]config.DiscoveredCommand, 0, len(entries))
	for _, entry := range entries {
		discovered = append(discovered, discoveredCommandFromPackCommandInfo(entry))
	}
	addDiscoveredCommandsToRoot(root, discovered, cityPath, cityName, stdout, stderr, true)
}

func discoveredCommandFromPackCommandInfo(info config.PackCommandInfo) config.DiscoveredCommand {
	helpFile := strings.TrimSpace(info.Entry.LongDescription)
	if helpFile != "" && !filepath.IsAbs(helpFile) {
		helpFile = filepath.Join(info.PackDir, helpFile)
	}
	return config.DiscoveredCommand{
		Name:        info.Entry.Name,
		Command:     []string{info.Entry.Name},
		Description: info.Entry.Description,
		RunScript:   info.Entry.Script,
		HelpFile:    helpFile,
		SourceDir:   info.PackDir,
		PackDir:     info.PackDir,
		PackName:    info.PackName,
		BindingName: info.PackName,
	}
}

// quietLoadCityConfig loads city config with log output suppressed.
// ExpandCityPacks logs "not found, skipping" for uncached remote packs
// which is confusing during cobra command-tree setup (before gc start
// has fetched them). The expander already skips missing packs gracefully;
// we just silence the log noise.
func quietLoadCityConfig(cityPath string) (*config.City, error) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)
	return loadCityConfig(cityPath, io.Discard)
}

// registerPackCommands attempts to discover the city, load config, and
// register pack-provided CLI commands as top-level subcommands. Fails
// silently if not in a city or config fails to load — core commands
// always work.
func registerPackCommands(root *cobra.Command, args []string, stdout, stderr io.Writer) {
	// git spawns `gc git-credential` mid-clone, while gc may already hold the
	// repo-cache lock for the very import being fetched (a credentialed
	// `gc import install`). Pack-command discovery loads city config, which
	// re-acquires that lock — a self-deadlock that hangs every credentialed
	// import. The helper needs no pack commands, so skip discovery for it.
	if isCredentialHelperInvocation(args) {
		return
	}
	cityPath, err := resolveCity()
	if err != nil {
		return
	}
	cfg, err := quietLoadCityConfig(cityPath)
	if err != nil {
		return
	}

	if len(cfg.PackCommands) == 0 {
		return
	}

	addDiscoveredCommandsToRoot(root, cfg.PackCommands, cityPath, loadedCityName(cfg, cityPath), stdout, stderr, false)
}

// isCredentialHelperInvocation reports whether injected run args invoke the
// hidden `gc git-credential` helper (git runs it as `gc git-credential <op>`). The
// helper is a leaf command on git's clone hot path, so it must skip the
// config-loading pack-command discovery that runs for normal invocations.
func isCredentialHelperInvocation(args []string) bool {
	command, ok := firstRootCommand(args)
	return ok && command == "git-credential"
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

// stdin returns os.Stdin. Extracted for testability (tests can override).
var stdin = func() io.Reader { return os.Stdin }

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

// resolvePackCommandFallback is the lazy fallback resolver for the root
// command's RunE. It classifies the invocation before executing any pack code
// so the central lifecycle never needs a raw binding, command path, or args.
func resolvePackCommandFallback(args []string, stdout, stderr io.Writer) packCommandAction {
	if len(args) == 0 {
		return unresolvedPackCommandAction()
	}

	cityPath, err := resolveCity()
	if err != nil {
		return unresolvedPackCommandAction()
	}
	cfg, err := quietLoadCityConfig(cityPath)
	if err != nil {
		return unresolvedPackCommandAction()
	}

	return resolveDiscoveredCommandFallback(args, cfg, cityPath, stdout, stderr)
}

// materializePackCommandTreeForArgs gives a pack binding missed by eager
// discovery the same Cobra nodes used by the eager path. It runs before
// Execute, because Cobra otherwise consumes group --help at the root before
// the root RunE fallback can resolve the binding.
//
// This is intentionally a narrow injected-argv preparation seam. The central
// invocation lifecycle will eventually own root-construction pre-scanning;
// until then, only the existing persistent scope flags are interpreted here.
func materializePackCommandTreeForArgs(root *cobra.Command, args []string, stdout, stderr io.Writer) {
	request, ok := packCommandTreeRequest(root, args)
	if !ok {
		return
	}

	if existing := findSubcommand(root, request.binding); existing != nil {
		if existing.Annotations[productMetricsClassAnnotation] != packCommandClassificationValue {
			return
		}
		if !request.citySet && !request.rigSet {
			applyPackCommandPreLeafArgs(root, args, request)
			return
		}
		// An explicitly selected scope must never retain a pack node whose
		// closures captured the ambient city during eager root construction.
		// Remove it before resolution so every failure and no-match path stays
		// fail-closed instead of executing stale pack code.
		root.RemoveCommand(existing)
		materializeSelectedPackCommandTree(root, args, request, stdout, stderr)
		return
	} else if coreCommandNames(root)[request.binding] {
		// The binding is a built-in alias or one of Cobra's reserved commands.
		// Packs cannot shadow it, and scope preparation must not remove it.
		return
	}

	// A missing binding makes Cobra look like the selected root command, but
	// that is not enough information to assign every later persistent-looking
	// token to the root. First resolve the unambiguous scope through the binding
	// itself and use that candidate tree to find the real leaf boundary. This
	// is what keeps a lazy tree from stealing --city, --rig, or help-looking
	// arguments that an eager DisableFlagParsing leaf would pass to its child.
	baselineRequest, baselineOK := packCommandRequestThroughBinding(request.binding, args)
	baselineCandidate, candidateOK := resolvePackCommandTreeCandidate(baselineRequest)
	if baselineOK && candidateOK {
		if candidateRequest, selected := baselineCandidate.request(args); selected {
			request = candidateRequest
		}
	}

	materializeSelectedPackCommandTree(root, args, request, stdout, stderr)
}

// materializeSelectedPackCommandTree resolves and validates the scoped tree
// selected by request. Scope ownership is resolved to a bounded fixed point
// because a missing tree cannot know the DisableFlagParsing leaf boundary
// until it probes a real candidate. Cycles, non-convergence, and conflicting
// stable candidates are blocked at the root so neither ambient nor speculative
// pack code can execute.
func materializeSelectedPackCommandTree(root *cobra.Command, args []string, request packCommandTreePreparation, stdout, stderr io.Writer) {
	selectedCandidate, selectedRequest, resolution := resolvePackCommandTreeFromScopeSeeds(args, request, resolvePackCommandTreeCandidate)
	switch resolution {
	case packCommandTreeResolutionUnavailable:
		return
	case packCommandTreeResolutionAmbiguous:
		failClosedPackCommandTree(root, request.binding, stderr)
		return
	case packCommandTreeResolutionStable:
		applyResolvedPackCommandArgs(root, args, request, selectedRequest)
	}
	selectedCandidate.addToRoot(root, stdout, stderr)
	// The root's normal usage-error installation already ran before this lazy
	// tree was materialized. Apply the same wrapper to the newly added namespace
	// so eager and lazy unknown-subcommand failures retain identical output.
	if namespace := findSubcommand(root, request.binding); namespace != nil {
		installArgUsageErrors(namespace, stderr)
	}
}

func failClosedPackCommandTree(root *cobra.Command, binding string, stderr io.Writer) {
	root.DisableFlagParsing = true
	root.SetArgs([]string{binding})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		return executeProductMetricsPackAction(cmd, resolvedPackCommandUnknownAction(cmd, binding, stderr)).err()
	}
}

func applyResolvedPackCommandArgs(root *cobra.Command, args []string, blindRequest, resolvedRequest packCommandTreePreparation) {
	prepared := preparePackCommandArgs(args, resolvedRequest)
	if blindRequest.scopeCount > resolvedRequest.scopeCount &&
		(resolvedRequest.preLeafHelpKind == packCommandPreLeafHelpNone || resolvedRequest.preLeafHelpKind == packCommandPreLeafHelpTrue) {
		// Compatibility: established eager/lazy dispatch passes an ordinary
		// pre-leaf scope through to DisableFlagParsing children. When a blind
		// missing-tree scan over-counted later child-owned scopes, remove only
		// the proven root-owned prefix so the later tokens reach the child
		// exactly once without changing the established single-scope path.
		prepared = packCommandArgsWithoutLeadingScopes(prepared, resolvedRequest.scopeCount)
	}
	root.SetArgs(prepared)
}

type packCommandTreeCandidate struct {
	entries  []config.DiscoveredCommand
	cityPath string
	cityName string
}

type packCommandTreeCandidateResolver func(packCommandTreePreparation) (packCommandTreeCandidate, bool)

type packCommandTreeResolution uint8

const (
	packCommandTreeResolutionUnavailable packCommandTreeResolution = iota
	packCommandTreeResolutionStable
	packCommandTreeResolutionAmbiguous
)

type packCommandTreeScope struct {
	city       string
	rig        string
	citySet    bool
	rigSet     bool
	scopeCount int
}

func (request packCommandTreePreparation) scope() packCommandTreeScope {
	return packCommandTreeScope{
		city:       request.city,
		rig:        request.rig,
		citySet:    request.citySet,
		rigSet:     request.rigSet,
		scopeCount: request.scopeCount,
	}
}

// resolvePackCommandTreeFixedPoint follows the scope selected by each real
// candidate topology until that candidate agrees with its own pre-leaf scope.
// Once a candidate has resolved, a later unavailable scope is an ambiguity,
// not a reason to fall back to an earlier speculative candidate.
func resolvePackCommandTreeFixedPoint(args []string, initial packCommandTreePreparation, resolve packCommandTreeCandidateResolver) (packCommandTreeCandidate, packCommandTreePreparation, packCommandTreeResolution) {
	request := initial
	resolutionLimit := len(packCommandRootScopeCheckpoints(packCommandTreePreparation{}, args)) + 1
	seen := make(map[packCommandTreeScope]bool, resolutionLimit)
	resolvedAny := false
	for range resolutionLimit {
		scope := request.scope()
		if seen[scope] {
			return packCommandTreeCandidate{}, packCommandTreePreparation{}, packCommandTreeResolutionAmbiguous
		}
		seen[scope] = true

		candidate, ok := resolve(request)
		if !ok {
			if resolvedAny {
				return packCommandTreeCandidate{}, packCommandTreePreparation{}, packCommandTreeResolutionAmbiguous
			}
			return packCommandTreeCandidate{}, packCommandTreePreparation{}, packCommandTreeResolutionUnavailable
		}
		resolvedAny = true
		selectedRequest, selected := candidate.request(args)
		if !selected {
			return packCommandTreeCandidate{}, packCommandTreePreparation{}, packCommandTreeResolutionAmbiguous
		}
		if request.sameScope(selectedRequest) {
			return candidate, selectedRequest, packCommandTreeResolutionStable
		}
		request = selectedRequest
	}
	return packCommandTreeCandidate{}, packCommandTreePreparation{}, packCommandTreeResolutionAmbiguous
}

// resolvePackCommandTreeFromScopeSeeds recovers the real root-owned prefix
// when the blind missing-tree scan ended on a post-leaf scope-looking token.
// Every resolvable seed must converge, and every stable result must agree;
// otherwise dispatch remains fail-closed.
func resolvePackCommandTreeFromScopeSeeds(args []string, request packCommandTreePreparation, resolve packCommandTreeCandidateResolver) (packCommandTreeCandidate, packCommandTreePreparation, packCommandTreeResolution) {
	seeds := packCommandTreeScopeSeeds(args, request)

	var stableCandidate packCommandTreeCandidate
	var stableRequest packCommandTreePreparation
	foundStable := false
	for _, seed := range seeds {
		candidate, resolvedRequest, status := resolvePackCommandTreeFixedPoint(args, seed, resolve)
		switch status {
		case packCommandTreeResolutionUnavailable:
			continue
		case packCommandTreeResolutionAmbiguous:
			return packCommandTreeCandidate{}, packCommandTreePreparation{}, packCommandTreeResolutionAmbiguous
		case packCommandTreeResolutionStable:
			if foundStable && (!stableRequest.sameScope(resolvedRequest) || !packCommandTreeCandidatesEqual(stableCandidate, candidate)) {
				return packCommandTreeCandidate{}, packCommandTreePreparation{}, packCommandTreeResolutionAmbiguous
			}
			stableCandidate = candidate
			stableRequest = resolvedRequest
			foundStable = true
		}
	}
	if !foundStable {
		return packCommandTreeCandidate{}, packCommandTreePreparation{}, packCommandTreeResolutionUnavailable
	}
	return stableCandidate, stableRequest, packCommandTreeResolutionStable
}

func packCommandTreeCandidatesEqual(left, right packCommandTreeCandidate) bool {
	return reflect.DeepEqual(left, right)
}

func (candidate packCommandTreeCandidate) valid() bool {
	return len(candidate.entries) > 0
}

func (candidate packCommandTreeCandidate) addToRoot(root *cobra.Command, stdout, stderr io.Writer) {
	addDiscoveredCommandsToRoot(root, candidate.entries, candidate.cityPath, candidate.cityName, stdout, stderr, false)
}

func (candidate packCommandTreeCandidate) request(args []string) (packCommandTreePreparation, bool) {
	if !candidate.valid() {
		return packCommandTreePreparation{}, false
	}
	probe := &cobra.Command{Use: "gc"}
	candidate.addToRoot(probe, io.Discard, io.Discard)
	return packCommandTreeRequest(probe, args)
}

func resolvePackCommandTreeCandidate(request packCommandTreePreparation) (packCommandTreeCandidate, bool) {
	if request.binding == "" || request.hasEmptyExplicitScope() {
		return packCommandTreeCandidate{}, false
	}

	previousCity, previousRig := cityFlag, rigFlag
	if request.citySet {
		cityFlag = request.city
	}
	if request.rigSet {
		rigFlag = request.rig
	}
	defer func() {
		cityFlag, rigFlag = previousCity, previousRig
	}()

	cityPath, err := resolveCity()
	if err != nil {
		return packCommandTreeCandidate{}, false
	}
	cfg, err := quietLoadCityConfig(cityPath)
	if err != nil {
		return packCommandTreeCandidate{}, false
	}

	matching := make([]config.DiscoveredCommand, 0, len(cfg.PackCommands))
	for _, entry := range cfg.PackCommands {
		if entry.BindingName == request.binding {
			matching = append(matching, entry)
		}
	}
	if len(matching) == 0 {
		return packCommandTreeCandidate{}, false
	}
	return packCommandTreeCandidate{
		entries:  matching,
		cityPath: cityPath,
		cityName: loadedCityName(cfg, cityPath),
	}, true
}

// packCommandRequestThroughBinding returns only the scope that is
// unambiguously root-owned before binding. The placeholder leaf stops the
// normal request parser at the binding without teaching it any later command
// topology.
func packCommandRequestThroughBinding(binding string, args []string) (packCommandTreePreparation, bool) {
	if binding == "" {
		return packCommandTreePreparation{}, false
	}
	probe := &cobra.Command{Use: "gc"}
	probe.AddCommand(&cobra.Command{
		Use:                binding,
		DisableFlagParsing: true,
		Annotations: map[string]string{
			productMetricsClassAnnotation: packCommandClassificationValue,
		},
	})
	return packCommandTreeRequest(probe, args)
}

type packCommandPreLeafHelpKind uint8

const (
	packCommandPreLeafHelpNone packCommandPreLeafHelpKind = iota
	packCommandPreLeafHelpTrue
	packCommandPreLeafHelpFalse
	packCommandPreLeafHelpInvalid
)

type packCommandTreePreparation struct {
	binding             string
	city                string
	rig                 string
	citySet             bool
	rigSet              bool
	scopeCount          int
	preLeafHelpKind     packCommandPreLeafHelpKind
	preLeafHelpIndex    int
	preLeafCommandIndex int
}

func (request packCommandTreePreparation) hasExplicitScope() bool {
	return request.citySet || request.rigSet
}

func (request packCommandTreePreparation) hasEmptyExplicitScope() bool {
	return request.citySet && request.city == "" || request.rigSet && request.rig == ""
}

func (request packCommandTreePreparation) sameScope(other packCommandTreePreparation) bool {
	return request.city == other.city &&
		request.rig == other.rig &&
		request.citySet == other.citySet &&
		request.rigSet == other.rigSet &&
		request.scopeCount == other.scopeCount
}

func packCommandTreeRequest(root *cobra.Command, args []string) (packCommandTreePreparation, bool) {
	var request packCommandTreePreparation
	if root == nil {
		return request, false
	}

	current := root
	canDescend := true
	pendingHelpKind := packCommandPreLeafHelpNone
	pendingHelpIndex := 0
	for index := 0; index < len(args); index++ {
		if current != root && current.DisableFlagParsing {
			return completePackCommandTreeRequest(request)
		}

		arg := args[index]
		helpKind, helpArg := packCommandHelpArgKind(arg)
		switch {
		case arg == "--":
			// The terminator belongs to the command Cobra has resolved so far.
			// Never inspect scope-looking tokens after it.
			return completePackCommandTreeRequest(request)
		case arg == "--city" || arg == "--rig":
			request.scopeCount++
			if index+1 >= len(args) {
				if arg == "--city" {
					request.city, request.citySet = "", true
				} else {
					request.rig, request.rigSet = "", true
				}
				return completePackCommandTreeRequest(request)
			}
			index++
			if arg == "--city" {
				request.city, request.citySet = args[index], true
			} else {
				request.rig, request.rigSet = args[index], true
			}
		case strings.HasPrefix(arg, "--city="):
			request.scopeCount++
			request.city, request.citySet = strings.TrimPrefix(arg, "--city="), true
		case strings.HasPrefix(arg, "--rig="):
			request.scopeCount++
			request.rig, request.rigSet = strings.TrimPrefix(arg, "--rig="), true
		case arg == "--json-schema":
			_, index = consumeJSONSchemaRole(args, index)
		case strings.HasPrefix(arg, "--json-schema="):
			continue
		case arg == "-" || isJSONControlArg(arg):
			// Cobra ignores a lone dash while finding command words, and JSON
			// contract controls are removed by the pre-execution JSON resolver.
			// Both remain in argv for a DisableFlagParsing leaf.
			continue
		case helpArg:
			// Help is an early outcome for the selected command, but persistent
			// scope flags remain valid later in a namespace or intermediate
			// command's argv. Malformed valued help is also retained here so a
			// later leaf cannot bypass the group flag error. Keep scanning until
			// a DisableFlagParsing leaf or terminator takes ownership.
			if pendingHelpKind != packCommandPreLeafHelpInvalid {
				pendingHelpKind = helpKind
				pendingHelpIndex = index
			}
			continue
		case strings.HasPrefix(arg, "-"):
			return completePackCommandTreeRequest(request)
		default:
			if request.binding == "" {
				request.binding = arg
				next := findSubcommandForArg(root, arg)
				if next == nil {
					// Root remains Cobra's selected command when the binding is not
					// in the eager tree, so collect every later persistent scope
					// occurrence. Materialization validates those occurrences against
					// the selected scope's real tree before changing dispatch.
					collectPackCommandRootScope(&request, args[index+1:])
					return request, true
				}
				if next.Annotations[productMetricsClassAnnotation] != packCommandClassificationValue {
					return request, true
				}
				current = next
				continue
			}

			if !canDescend {
				continue
			}
			next := findSubcommandForArg(current, arg)
			if next == nil || next.Annotations[productMetricsClassAnnotation] != packCommandClassificationValue {
				// Cobra stops command-path descent at the first unmatched word,
				// but the selected group still parses inherited flags among its
				// remaining arguments.
				canDescend = false
				continue
			}
			current = next
			if current.DisableFlagParsing {
				request.preLeafCommandIndex = index
				if pendingHelpKind != packCommandPreLeafHelpNone {
					request.preLeafHelpKind = pendingHelpKind
					request.preLeafHelpIndex = pendingHelpIndex
				}
				return completePackCommandTreeRequest(request)
			}
		}
	}
	return completePackCommandTreeRequest(request)
}

func packCommandHelpArgKind(arg string) (packCommandPreLeafHelpKind, bool) {
	if arg == "-h" || arg == "--help" {
		return packCommandPreLeafHelpTrue, true
	}
	name, value, hasValue := strings.Cut(arg, "=")
	if !hasValue || name != "-h" && name != "--help" {
		return packCommandPreLeafHelpNone, false
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return packCommandPreLeafHelpInvalid, true
	}
	if parsed {
		return packCommandPreLeafHelpTrue, true
	}
	return packCommandPreLeafHelpFalse, true
}

func applyPackCommandPreLeafArgs(root *cobra.Command, args []string, request packCommandTreePreparation) {
	if root == nil {
		return
	}
	root.SetArgs(preparePackCommandArgs(args, request))
}

func preparePackCommandArgs(args []string, request packCommandTreePreparation) []string {
	prepared, adjusted := movePackCommandPreLeafJSONControls(args, request)
	if adjusted.preLeafHelpKind != packCommandPreLeafHelpNone {
		prepared = packCommandPreLeafArgs(prepared, adjusted)
	}
	return prepared
}

func movePackCommandPreLeafJSONControls(args []string, request packCommandTreePreparation) ([]string, packCommandTreePreparation) {
	commandIndex := request.preLeafCommandIndex
	if commandIndex <= 0 || commandIndex >= len(args) {
		return args, request
	}
	controls := make([]string, 0, 1)
	for index := 0; index < commandIndex; index++ {
		if isJSONControlArg(args[index]) {
			controls = append(controls, args[index])
		}
	}
	if len(controls) == 0 {
		return args, request
	}

	prepared := make([]string, 0, len(args))
	removedBeforeHelp := 0
	for index, arg := range args {
		if index < commandIndex && isJSONControlArg(arg) {
			if index < request.preLeafHelpIndex {
				removedBeforeHelp++
			}
			continue
		}
		prepared = append(prepared, arg)
		if index == commandIndex {
			prepared = append(prepared, controls...)
		}
	}
	request.preLeafCommandIndex -= len(controls)
	if request.preLeafHelpKind != packCommandPreLeafHelpNone {
		request.preLeafHelpIndex -= removedBeforeHelp
	}
	return prepared, request
}

func packCommandPreLeafArgs(args []string, request packCommandTreePreparation) []string {
	helpIndex := request.preLeafHelpIndex
	commandIndex := request.preLeafCommandIndex
	if helpIndex < 0 || helpIndex >= len(args) || commandIndex <= helpIndex || commandIndex >= len(args) {
		return args
	}

	if request.preLeafHelpKind == packCommandPreLeafHelpInvalid {
		return append([]string(nil), args[:helpIndex+1]...)
	}

	out := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if index < commandIndex {
			if _, helpArg := packCommandHelpArgKind(arg); helpArg {
				if request.preLeafHelpKind == packCommandPreLeafHelpTrue && index == helpIndex {
					out = append(out, "--help")
				}
				continue
			}
			if request.preLeafHelpKind == packCommandPreLeafHelpFalse {
				switch {
				case arg == "--city" || arg == "--rig":
					if index+1 < commandIndex {
						index++
					}
					continue
				case strings.HasPrefix(arg, "--city=") || strings.HasPrefix(arg, "--rig="):
					continue
				}
			}
		}
		out = append(out, arg)
	}
	return out
}

func completePackCommandTreeRequest(request packCommandTreePreparation) (packCommandTreePreparation, bool) {
	if request.binding == "" {
		return packCommandTreePreparation{}, false
	}
	return request, true
}

func findSubcommandForArg(cmd *cobra.Command, arg string) *cobra.Command {
	for _, child := range cmd.Commands() {
		if child.Name() == arg || child.HasAlias(arg) {
			return child
		}
	}
	return nil
}

func packCommandTreeScopeSeeds(args []string, request packCommandTreePreparation) []packCommandTreePreparation {
	baseline, baselineOK := packCommandRequestThroughBinding(request.binding, args)
	var checkpoints []packCommandTreePreparation
	if baselineOK {
		if tail, ok := packCommandArgsAfterBinding(request.binding, args); ok {
			checkpoints = packCommandRootScopeCheckpoints(baseline, tail)
		}
	}

	seeds := make([]packCommandTreePreparation, 0, len(checkpoints)+2)
	seen := make(map[packCommandTreeScope]bool, 4)
	addSeed := func(seed packCommandTreePreparation) {
		scope := seed.scope()
		if seen[scope] {
			return
		}
		seen[scope] = true
		seeds = append(seeds, seed)
	}
	addSeed(request)
	for index := len(checkpoints) - 1; index >= 0; index-- {
		addSeed(checkpoints[index])
	}
	if baselineOK && baseline.hasExplicitScope() {
		addSeed(baseline)
	}
	return seeds
}

func packCommandArgsAfterBinding(binding string, args []string) ([]string, bool) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		_, helpArg := packCommandHelpArgKind(arg)
		switch {
		case arg == "--":
			return nil, false
		case arg == "--city" || arg == "--rig":
			if index+1 < len(args) {
				index++
			}
		case strings.HasPrefix(arg, "--city=") || strings.HasPrefix(arg, "--rig="):
			continue
		case arg == "--json-schema":
			_, index = consumeJSONSchemaRole(args, index)
		case strings.HasPrefix(arg, "--json-schema="):
			continue
		case arg == "-" || isJSONControlArg(arg):
			continue
		case helpArg:
			continue
		case strings.HasPrefix(arg, "-"):
			return nil, false
		default:
			if arg != binding {
				return nil, false
			}
			return args[index+1:], true
		}
	}
	return nil, false
}

func packCommandRootScopeCheckpoints(request packCommandTreePreparation, args []string) []packCommandTreePreparation {
	checkpoints := make([]packCommandTreePreparation, 0, 2)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return checkpoints
		case arg == "--city" || arg == "--rig":
			request.scopeCount++
			value := ""
			if index+1 < len(args) {
				index++
				value = args[index]
			}
			if arg == "--city" {
				request.city, request.citySet = value, true
			} else {
				request.rig, request.rigSet = value, true
			}
			checkpoints = append(checkpoints, request)
		case strings.HasPrefix(arg, "--city="):
			request.scopeCount++
			request.city, request.citySet = strings.TrimPrefix(arg, "--city="), true
			checkpoints = append(checkpoints, request)
		case strings.HasPrefix(arg, "--rig="):
			request.scopeCount++
			request.rig, request.rigSet = strings.TrimPrefix(arg, "--rig="), true
			checkpoints = append(checkpoints, request)
		case arg == "--json-schema":
			_, index = consumeJSONSchemaRole(args, index)
		}
	}
	return checkpoints
}

func packCommandArgsWithoutLeadingScopes(args []string, scopeCount int) []string {
	if scopeCount <= 0 {
		return args
	}
	out := make([]string, 0, len(args))
	remaining := scopeCount
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if remaining > 0 {
			switch {
			case arg == "--city" || arg == "--rig":
				remaining--
				if index+1 < len(args) {
					index++
				}
				continue
			case strings.HasPrefix(arg, "--city=") || strings.HasPrefix(arg, "--rig="):
				remaining--
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

func collectPackCommandRootScope(request *packCommandTreePreparation, args []string) {
	checkpoints := packCommandRootScopeCheckpoints(*request, args)
	if len(checkpoints) > 0 {
		*request = checkpoints[len(checkpoints)-1]
	}
}

func packCommandFlagsHaveEmptyExplicitScope(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	for _, name := range []string{"city", "rig"} {
		flag := cmd.Flags().Lookup(name)
		if flag != nil && flag.Changed && flag.Value.String() == "" {
			return true
		}
	}
	return false
}

// tryPackCommandFallback preserves the direct fallback test seam while
// returning the same minimized typed outcome as eager execution. The root uses
// resolvePackCommandFallback so classification is available before execution.
func tryPackCommandFallback(args []string, stdout, stderr io.Writer) packCommandOutcome {
	return resolvePackCommandFallback(args, stdout, stderr).execute()
}
