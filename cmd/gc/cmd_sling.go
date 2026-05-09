package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

func init() {
	sling.SetTracer(func(format string, args ...any) {
		path := strings.TrimSpace(os.Getenv("GC_SLING_TRACE"))
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer f.Close()                                                                                    //nolint:errcheck
		fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...)) //nolint:errcheck
	})
}

// slingStdin returns the reader for --stdin input. Extracted for testability.
var slingStdin = func() io.Reader { return os.Stdin }

// BeadQuerier is an alias for sling.BeadQuerier.
type BeadQuerier = sling.BeadQuerier

// BeadChildQuerier is an alias for sling.BeadChildQuerier.
type BeadChildQuerier = sling.BeadChildQuerier

func newSlingCmd(stdout, stderr io.Writer) *cobra.Command {
	var formula bool
	var nudge bool
	var force bool
	var title string
	var vars []string
	var merge string
	var noConvoy bool
	var owned bool
	var onFormula string
	var dryRun bool
	var noFormula bool
	var fromStdin bool
	var scopeKind string
	var scopeRef string
	cmd := &cobra.Command{
		Use:   "sling [target] <bead-or-formula-or-text>",
		Short: "Route work to a session config or agent",
		Long: `Route a bead to a session config or agent using the target's sling_query.

The target is an agent qualified name (e.g. "mayor" or "hello-world/polecat").
The second argument is a bead ID, a formula name when --formula is set, or
arbitrary text (which auto-creates a task bead).

When target is omitted, the bead's rig prefix is used to look up the rig's
default_sling_target from config. Requires --formula to have an explicit target.
Inline text also requires an explicit target.

With --formula, a wisp (ephemeral molecule) is instantiated from the formula
and its root bead is routed to the target.

Examples:
  gc sling my-rig/claude BL-42              # route existing bead
  gc sling my-rig/claude "write a README"   # create bead from text, then route
  gc sling mayor code-review --formula      # instantiate formula, route wisp
  echo "fix login" | gc sling mayor --stdin # read bead text from stdin`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if fromStdin {
				if len(args) != 1 {
					fmt.Fprintf(stderr, "gc sling: --stdin requires exactly 1 argument (target)\n") //nolint:errcheck // best-effort stderr
					return errExit
				}
			} else if len(args) < 1 || len(args) > 2 {
				fmt.Fprintf(stderr, "gc sling: requires 1 or 2 arguments: [target] <bead-or-formula>\n") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if owned && noConvoy {
				fmt.Fprintf(stderr, "gc sling: --owned requires a convoy (cannot use with --no-convoy)\n") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if merge != "" && merge != "direct" && merge != "mr" && merge != "local" {
				fmt.Fprintf(stderr, "gc sling: --merge must be direct, mr, or local\n") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if (strings.TrimSpace(scopeKind) == "") != (strings.TrimSpace(scopeRef) == "") {
				fmt.Fprintf(stderr, "gc sling: --scope-kind and --scope-ref must be provided together\n") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if scopeKind != "" && scopeKind != "city" && scopeKind != "rig" {
				fmt.Fprintf(stderr, "gc sling: --scope-kind must be city or rig\n") //nolint:errcheck // best-effort stderr
				return errExit
			}
			code := cmdSling(args, formula, nudge, force, title, vars, merge, noConvoy, owned, onFormula, noFormula, fromStdin, dryRun, scopeKind, scopeRef, stdout, stderr)
			return exitForCode(code)
		},
	}
	cmd.Flags().BoolVarP(&formula, "formula", "f", false, "treat argument as formula name")
	cmd.Flags().BoolVar(&nudge, "nudge", false, "nudge target after routing")
	cmd.Flags().BoolVar(&force, "force", false, "suppress warnings, allow cross-rig routing, allow graph workflow replacement, and for direct bead routes dispatch even if the bead does not resolve in the local store")
	cmd.Flags().StringVarP(&title, "title", "t", "", "wisp root bead title (with --formula or --on)")
	cmd.Flags().StringArrayVar(&vars, "var", nil, "variable substitution for formula (key=value, repeatable)")
	cmd.Flags().StringVar(&merge, "merge", "", "merge strategy: direct, mr, or local")
	cmd.Flags().BoolVar(&noConvoy, "no-convoy", false, "skip auto-convoy creation")
	cmd.Flags().BoolVar(&owned, "owned", false, "mark auto-convoy as owned (skip auto-close)")
	cmd.Flags().StringVar(&onFormula, "on", "", "attach wisp from formula to bead before routing")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "show what would be done without executing")
	cmd.Flags().BoolVar(&noFormula, "no-formula", false, "suppress default formula (route raw bead)")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read bead text from stdin (first line = title, rest = description)")
	cmd.Flags().StringVar(&scopeKind, "scope-kind", "", "logical workflow scope kind for graph.v2 launches")
	cmd.Flags().StringVar(&scopeRef, "scope-ref", "", "logical workflow scope ref for graph.v2 launches")
	cmd.MarkFlagsMutuallyExclusive("formula", "on")
	cmd.MarkFlagsMutuallyExclusive("no-formula", "formula")
	cmd.MarkFlagsMutuallyExclusive("no-formula", "on")
	cmd.MarkFlagsMutuallyExclusive("stdin", "formula")
	_ = cmd.Flags().SetAnnotation("scope-kind", cobra.BashCompOneRequiredFlag, []string{"scope-ref"})
	_ = cmd.Flags().SetAnnotation("scope-ref", cobra.BashCompOneRequiredFlag, []string{"scope-kind"})
	return cmd
}

// slingOpts is an alias for sling.SlingOpts.
type slingOpts = sling.SlingOpts

var (
	slingPokeController        = pokeController
	slingPokeControlDispatcher = pokeControlDispatch
)

// slingDeps is an alias for sling.SlingDeps.
type slingDeps = sling.SlingDeps

// SlingRunner is an alias for sling.SlingRunner.
type SlingRunner = sling.SlingRunner

// shellSlingRunner runs a command via sh -c and returns stdout.
// Times out after 30 seconds. If dir is non-empty, the command runs in
// that directory (needed for rig-scoped beads whose .beads/ lives there).
// Extra env vars are appended to the process environment.
func shellSlingRunner(dir, command string, env map[string]string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = mergeRuntimeEnv(os.Environ(), env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("running %q: %w", command, err)
	}
	return string(out), nil
}

// cmdSling is the CLI entry point for gc sling.
func cmdSling(args []string, isFormula, doNudge, force bool, title string, vars []string, merge string, noConvoy, owned bool, onFormula string, noFormula, fromStdin, dryRun bool, scopeKind, scopeRef string, stdout, stderr io.Writer) int {
	// --stdin: read bead text from stdin early (before city resolution)
	// so errors are reported immediately. First line = title, rest = description.
	var stdinDescription string
	var stdinTitle string
	if fromStdin {
		data, err := io.ReadAll(slingStdin())
		if err != nil {
			fmt.Fprintf(stderr, "gc sling: reading stdin: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		content := strings.TrimRight(string(data), "\n")
		if content == "" {
			fmt.Fprintf(stderr, "gc sling: --stdin: no input received\n") //nolint:errcheck // best-effort stderr
			return 1
		}
		lines := strings.SplitN(content, "\n", 2)
		stdinTitle = lines[0]
		if len(lines) > 1 {
			stdinDescription = strings.TrimSpace(lines[1])
		}
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, prov, err := loadSlingCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	emitLoadCityConfigWarnings(stderr, prov)
	applyFeatureFlags(cfg)
	cityName := loadedCityName(cfg, cityPath)

	var target, beadOrFormula string
	var sourceBead existingSlingSourceBead
	switch {
	case fromStdin:
		target = args[0]
		beadOrFormula = stdinTitle
	case len(args) == 2:
		target = args[0]
		beadOrFormula = args[1]
		if !isFormula {
			sourceBead, err = probeExistingSlingSourceBead(cfg, cityPath, beadOrFormula)
			if err != nil {
				fmt.Fprintf(stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
	default:
		// 1-arg: bead ID only, resolve target from rig's default_sling_target.
		beadOrFormula = args[0]
		if isFormula {
			fmt.Fprintf(stderr, "gc sling: --formula requires explicit target\n") //nolint:errcheck // best-effort stderr
			return 1
		}
		sourceBead, err = probeExistingSlingSourceBead(cfg, cityPath, beadOrFormula)
		if err != nil {
			fmt.Fprintf(stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !canInferSlingDefaultTargetFromBead(cfg, beadOrFormula) && !sourceBead.exists {
			fmt.Fprintf(stderr, "gc sling: inline text requires explicit target\n  usage: gc sling <target> %q\n", beadOrFormula) //nolint:errcheck // best-effort stderr
			return 1
		}
		bp := sling.BeadPrefixForCity(cfg, beadOrFormula)
		if sourceBead.prefix != "" {
			bp = sourceBead.prefix
		}
		if bp == "" {
			fmt.Fprintf(stderr, "gc sling: cannot derive rig from bead %q (no prefix)\n", beadOrFormula) //nolint:errcheck // best-effort stderr
			return 1
		}
		rig, found := findRigByPrefix(cfg, bp)
		if !found {
			fmt.Fprintf(stderr, "gc sling: no rig with prefix %q for bead %s\n", bp, beadOrFormula) //nolint:errcheck // best-effort stderr
			return 1
		}
		if rig.DefaultSlingTarget == "" {
			fmt.Fprintf(stderr, "gc sling: rig %q has no default_sling_target\n", rig.Name) //nolint:errcheck // best-effort stderr
			return 1
		}
		target = rig.DefaultSlingTarget
	}

	// Ensure rig paths are absolute before agent/rig context resolution.
	// Without this, currentRigContext can't match CWD against relative
	// rig paths, so bare agent names (e.g., "claude") don't resolve to
	// rig-scoped implicit agents (e.g., "hello-world/claude").
	resolveRigPaths(cityPath, cfg.Rigs)

	a, ok := resolveAgentIdentity(cfg, target, currentRigContext(cfg))
	if !ok {
		fmt.Fprintln(stderr, agentNotFoundMsg("gc sling", target, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()

	var storeDir string
	var store beads.Store
	if sourceBead.exists {
		storeDir = sourceBead.storeDir
		store, err = openStoreAtForCity(storeDir, cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc sling: opening store %s: %v\n", storeDir, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		storeDir, store, err = openSlingStoreForSource(cfg, cityPath, beadOrFormula, a)
		if err != nil {
			fmt.Fprintf(stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	storeRef := workflowStoreRefForDir(storeDir, cityPath, cityName, cfg)
	storeEnv := slingStoreEnv(cfg, cityPath, storeDir)
	if sourceBead.exists && looksLikeInlineText(cfg, beadOrFormula) {
		fmt.Fprintf(stderr, "gc sling: found existing bead %q in %s; routing it instead of creating inline text\n", beadOrFormula, storeRef) //nolint:errcheck // best-effort stderr
	}

	// Inline text mode: if the argument doesn't look like a bead ID
	// (and we're not in formula mode), create a task bead from the text.
	// During dry-run, mark the text as preview-only instead of creating it.
	inlineText := false
	if !isFormula {
		inlineProbeStore := store
		if !sourceBead.exists && sourceBead.checked && looksLikeInlineText(cfg, beadOrFormula) {
			inlineProbeStore = nil
		}
		createInlineBead, previewInlineText, err := resolveInlineBeadAction(cfg, beadOrFormula, dryRun, inlineProbeStore)
		if err != nil {
			fmt.Fprintf(stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		inlineText = previewInlineText
		if createInlineBead {
			created, err := store.Create(beads.Bead{Title: beadOrFormula, Description: stdinDescription, Type: "task"})
			if err != nil {
				fmt.Fprintf(stderr, "gc sling: creating bead: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			fmt.Fprintf(stdout, "Created %s — %q\n", created.ID, beadOrFormula) //nolint:errcheck // best-effort stdout
			beadOrFormula = created.ID
		}
	}

	opts := slingOpts{
		Target:        a,
		BeadOrFormula: beadOrFormula,
		IsFormula:     isFormula,
		OnFormula:     onFormula,
		NoFormula:     noFormula,
		Title:         title,
		Vars:          vars,
		Merge:         merge,
		NoConvoy:      noConvoy,
		Owned:         owned,
		Nudge:         doNudge,
		Force:         force,
		DryRun:        dryRun,
		InlineText:    inlineText,
		ScopeKind:     scopeKind,
		ScopeRef:      scopeRef,
	}
	runner := SlingRunner(shellSlingRunner)
	if len(storeEnv) > 0 {
		pinnedEnv := maps.Clone(storeEnv)
		runner = func(dir, command string, env map[string]string) (string, error) {
			merged := maps.Clone(pinnedEnv)
			for k, v := range env {
				merged[k] = v
			}
			return shellSlingRunner(dir, command, merged)
		}
	}
	deps := slingDeps{
		CityName: cityName,
		CityPath: cityPath,
		Cfg:      cfg,
		SP:       sp,
		Runner:   runner,
		Store:    store,
		StoreRef: storeRef,
		SourceWorkflowStores: func() ([]sling.SourceWorkflowStore, error) {
			stores, skips, err := openSourceWorkflowStores(cfg, cityPath, "")
			if err != nil {
				return nil, err
			}
			if len(skips) > 0 {
				// The sling callback cannot push into SlingResult from
				// this depth, but stderr is the only channel operators
				// look at; silence here means singleton coverage can
				// degrade without any breadcrumb.
				fmt.Fprintln(stderr, "warning:", formatSourceWorkflowStoreSkips(skips)) //nolint:errcheck
			}
			out := make([]sling.SourceWorkflowStore, 0, len(stores))
			for _, storeView := range stores {
				out = append(out, sling.SourceWorkflowStore{
					Store:    storeView.store,
					StoreRef: workflowStoreRefForDir(storeView.path, cityPath, cityName, cfg),
				})
			}
			return out, nil
		},
	}

	return doSlingBatch(opts, deps, store, stdout, stderr)
}

func loadSlingCityConfig(cityPath string) (*config.City, *config.Provenance, error) {
	return loadCityConfigWithBuiltinPacks(cityPath, extraConfigFiles...)
}

func slingStoreEnv(cfg *config.City, cityPath, storeDir string) map[string]string {
	storeEnv := map[string]string{}
	switch provider := rawBeadsProviderForScope(storeDir, cityPath); {
	case provider == "file":
		// Built-in routing now goes through beads.Store; custom queries own any
		// provider-specific shell environment when they opt out of that path.
	case strings.HasPrefix(provider, "exec:"):
		// Explicit custom sling_query commands own their env for exec providers.
	default:
		storeEnv = bdRuntimeEnv(cityPath)
		if !samePath(storeDir, cityPath) {
			storeEnv = bdRuntimeEnvForRig(cityPath, cfg, storeDir)
		}
	}
	return storeEnv
}

// findRigByPrefix returns the rig whose effective prefix matches (case-insensitive).
func findRigByPrefix(cfg *config.City, prefix string) (config.Rig, bool) {
	return sling.FindRigByPrefix(cfg, prefix)
}

// beadPrefix returns the rig prefix for beadID, preferring the longest
// configured prefix when cfg is non-nil. Pass cfg whenever the caller
// needs hyphenated rig prefixes (e.g. "agent-diagnostics-hnn") to
// resolve correctly; otherwise the underlying sling.BeadPrefix's
// first-dash split is used.
func beadPrefix(cfg *config.City, beadID string) string {
	return sling.BeadPrefixForCity(cfg, beadID)
}

func rigDirForBead(cfg *config.City, beadID string) string {
	return sling.RigDirForBead(cfg, beadID)
}

func rigDirForAgent(cfg *config.City, a config.Agent) string {
	return sling.RigDirForAgent(cfg, a)
}

func slingDirForBead(cfg *config.City, cityPath, beadID string) string {
	if dir := rigDirForBead(cfg, beadID); dir != "" {
		return resolveStoreScopeRoot(cityPath, dir)
	}
	return resolveStoreScopeRoot(cityPath, cityPath)
}

func resolveSlingStoreRoot(cfg *config.City, cityPath, beadOrFormula string, a config.Agent) string {
	storeDir := resolveStoreScopeRoot(cityPath, cityPath)
	if cfg == nil {
		return storeDir
	}
	// Unbound rigs (declared in city.toml but missing a .gc/site.toml
	// binding) have an empty rig.Path; falling through to
	// resolveStoreScopeRoot would silently alias them to the city
	// scope. Skip them so sling falls back to the agent's rig_dir or
	// the city store instead of operating on the wrong store.
	if bp := beadPrefix(cfg, beadOrFormula); bp != "" && !looksLikeInlineText(cfg, beadOrFormula) {
		if sling.IsHQPrefix(cfg, bp) {
			return storeDir
		}
		if rig, found := findRigByPrefix(cfg, bp); found && strings.TrimSpace(rig.Path) != "" {
			return resolveStoreScopeRoot(cityPath, rig.Path)
		}
	}
	if rd := rigDirForAgent(cfg, a); rd != "" {
		return resolveStoreScopeRoot(cityPath, rd)
	}
	return storeDir
}

func openSlingStoreForSource(cfg *config.City, cityPath, beadOrFormula string, a config.Agent) (string, beads.Store, error) {
	storeDir := resolveSlingStoreRoot(cfg, cityPath, beadOrFormula, a)
	store, err := openStoreAtForCity(storeDir, cityPath)
	if err != nil {
		return "", nil, fmt.Errorf("opening store %s: %w", storeDir, err)
	}
	return storeDir, store, nil
}

type existingSlingSourceBead struct {
	exists   bool
	checked  bool
	storeDir string
	prefix   string
}

func probeExistingSlingSourceBead(cfg *config.City, cityPath, beadID string) (existingSlingSourceBead, error) {
	storeDir, prefix, ok := slingSourceStoreRootForCandidate(cfg, cityPath, beadID)
	if !ok {
		return existingSlingSourceBead{}, nil
	}
	store, err := openStoreAtForCity(storeDir, cityPath)
	if err != nil {
		return existingSlingSourceBead{}, fmt.Errorf("opening store %s: %w", storeDir, err)
	}
	exists, err := sling.ProbeBeadInStore(store, beadID)
	if err != nil {
		return existingSlingSourceBead{}, fmt.Errorf("checking bead candidate %q: %w", beadID, err)
	}
	if !exists {
		return existingSlingSourceBead{checked: true, storeDir: storeDir, prefix: prefix}, nil
	}
	return existingSlingSourceBead{exists: true, checked: true, storeDir: storeDir, prefix: prefix}, nil
}

func slingSourceStoreRootForCandidate(cfg *config.City, cityPath, beadID string) (string, string, bool) {
	if cfg == nil || !isBeadIDCandidate(beadID) {
		return "", "", false
	}
	bp := sling.BeadPrefixForCity(cfg, beadID)
	if bp == "" {
		return "", "", false
	}
	if sling.IsHQPrefix(cfg, bp) {
		return resolveStoreScopeRoot(cityPath, cityPath), bp, true
	}
	rig, found := findRigByPrefix(cfg, bp)
	if !found || strings.TrimSpace(rig.Path) == "" {
		return "", "", false
	}
	return resolveStoreScopeRoot(cityPath, rig.Path), bp, true
}

func canInferSlingDefaultTargetFromBead(cfg *config.City, beadOrFormula string) bool {
	return looksLikeBeadID(beadOrFormula) || looksLikeConfiguredBeadID(cfg, beadOrFormula)
}

// populateSlingDepsCallbacks fills in the interface fields on SlingDeps.
func populateSlingDepsCallbacks(deps *slingDeps) {
	deps.Resolver = cliAgentResolver{}
	deps.Branches = cliBranchResolver{}
	deps.Notify = &cliNotifier{}
	deps.DirectSessionResolver = cliDirectSessionResolver
	deps.Router = cliBeadRouter{deps: deps}
}

func cliDirectSessionResolver(store beads.Store, cityName, cityPath string, cfg *config.City, target, rigContext string) (string, bool, error) {
	if cfg == nil {
		return "", false, nil
	}
	if cityName == "" {
		cityName = config.EffectiveCityName(cfg, filepath.Base(cityPath))
	}
	spec, ok, err := resolveNamedSessionSpecForConfigTarget(cfg, cityName, target, rigContext)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, spec.Identity)
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// cliAgentResolver implements sling.AgentResolver using the CLI's
// 3-step resolution with ambient rig context.
type cliAgentResolver struct{}

func (cliAgentResolver) ResolveAgent(cfg *config.City, name, rigContext string) (config.Agent, bool) {
	return resolveAgentIdentity(cfg, name, rigContext)
}

// cliBranchResolver implements sling.BranchResolver using git.
type cliBranchResolver struct{}

func (cliBranchResolver) DefaultBranch(dir string) string {
	return defaultBranchFor(dir)
}

// cliNotifier implements sling.Notifier using IPC.
type cliNotifier struct{}

func (cliNotifier) PokeController(cityPath string) {
	_ = slingPokeController(cityPath)
}

func (cliNotifier) PokeControlDispatch(cityPath string) {
	_ = slingPokeControlDispatcher(cityPath)
}

type cliBeadRouter struct {
	deps *slingDeps
}

func (r cliBeadRouter) Route(_ context.Context, req sling.RouteRequest) error {
	if r.deps == nil {
		return fmt.Errorf("sling router: missing dependencies")
	}
	if r.deps.Cfg != nil {
		if agentCfg, ok := findAgentByQualified(r.deps.Cfg, req.Target); ok && isCustomSlingQuery(agentCfg) {
			if r.deps.Runner == nil {
				return fmt.Errorf("custom sling_query requires a runner")
			}
			slingCmd, _ := sling.BuildSlingCommandForAgent("sling_query", agentCfg.EffectiveSlingQuery(), req.BeadID, r.deps.CityPath, r.deps.CityName, agentCfg, r.deps.Cfg.Rigs)
			_, err := r.deps.Runner(req.WorkDir, slingCmd, req.Env)
			return err
		}
	}
	if r.deps.Store == nil {
		return fmt.Errorf("built-in sling routing requires a store")
	}
	if err := r.deps.Store.SetMetadata(req.BeadID, "gc.routed_to", req.Target); err != nil {
		return fmt.Errorf("setting gc.routed_to on %s: %w", req.BeadID, err)
	}
	return nil
}

// printSlingWarnings prints only warnings from a SlingResult to stderr.
// Called before error handling so warnings are visible even on failure.
func printSlingWarnings(result sling.SlingResult, stderr io.Writer) {
	if result.AgentSuspended {
		fmt.Fprintf(stderr, "warning: agent %q is suspended — bead routed but may not be picked up\n", result.Target) //nolint:errcheck
	}
	if result.PoolEmpty {
		fmt.Fprintf(stderr, "warning: session config %q has max_active_sessions=0 — bead routed but no sessions can claim it\n", result.Target) //nolint:errcheck
	}
	for _, w := range result.BeadWarnings {
		fmt.Fprintln(stderr, w) //nolint:errcheck
	}
	for _, id := range result.AutoBurned {
		fmt.Fprintf(stderr, "Auto-burned stale molecule %s\n", id) //nolint:errcheck
	}
	for _, e := range result.MetadataErrors {
		fmt.Fprintf(stderr, "gc sling: %s\n", e) //nolint:errcheck
	}
}

// printSlingResult formats a SlingResult for CLI display.
// Warnings go to stderr, messages go to stdout -- matching original behavior.
func printSlingResult(result sling.SlingResult, stdout, _ io.Writer) {
	// Skip display messages for idempotent/dry-run (handled separately).
	if result.Idempotent {
		fmt.Fprintf(stdout, "Bead %s already routed to %s — skipping (idempotent)\n", result.BeadID, result.Target) //nolint:errcheck
		return
	}
	if result.DryRun {
		return // dry-run display handled by dryRunSingle/dryRunBatch
	}

	// Messages (stdout).
	if result.ConvoyID != "" {
		fmt.Fprintf(stdout, "Auto-convoy %s\n", result.ConvoyID) //nolint:errcheck
	}
	if result.WispRootID != "" && result.WorkflowID == "" {
		if result.FormulaName != "" {
			fmt.Fprintf(stdout, "Attached wisp %s (formula %q) to %s\n", result.WispRootID, result.FormulaName, result.BeadID) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "Attached wisp %s to %s\n", result.WispRootID, result.BeadID) //nolint:errcheck
		}
	}
	if result.WorkflowID != "" {
		switch result.Method {
		case "on-formula", "default-on-formula":
			fmt.Fprintf(stdout, "Attached workflow %s (formula %q) to %s\n", result.WorkflowID, result.FormulaName, result.BeadID) //nolint:errcheck
		default:
			fmt.Fprintf(stdout, "Started workflow %s (formula %q) → %s\n", result.WorkflowID, result.FormulaName, result.Target) //nolint:errcheck
		}
		return
	}

	// Standard sling confirmation.
	switch result.Method {
	case "formula":
		fmt.Fprintf(stdout, "Slung formula %q (wisp root %s) → %s\n", result.FormulaName, result.BeadID, result.Target) //nolint:errcheck
	case "on-formula":
		fmt.Fprintf(stdout, "Slung %s (with formula %q) → %s\n", result.BeadID, result.FormulaName, result.Target) //nolint:errcheck
	case "default-on-formula":
		fmt.Fprintf(stdout, "Slung %s (with default formula %q) → %s\n", result.BeadID, result.FormulaName, result.Target) //nolint:errcheck
	default:
		fmt.Fprintf(stdout, "Slung %s → %s\n", result.BeadID, result.Target) //nolint:errcheck
	}
}

// printBatchSlingResult formats a batch SlingResult for CLI display.
func printBatchSlingResult(result sling.SlingResult, stdout, stderr io.Writer) {
	// Warnings.
	for _, w := range result.BeadWarnings {
		fmt.Fprintln(stderr, w) //nolint:errcheck
	}
	for _, id := range result.AutoBurned {
		fmt.Fprintf(stderr, "Auto-burned stale molecule %s\n", id) //nolint:errcheck
	}
	for _, e := range result.MetadataErrors {
		fmt.Fprintf(stderr, "gc sling: %s\n", e) //nolint:errcheck
	}

	if result.DryRun {
		return
	}

	// Container expansion header.
	ctype := result.ContainerType
	if ctype == "" {
		ctype = "container"
	}
	fmt.Fprintf(stdout, "Expanding %s %s (%d children, %d open)\n", ctype, result.BeadID, result.Total, result.Total-result.Skipped) //nolint:errcheck

	// Per-child results.
	for _, child := range result.Children {
		switch {
		case child.Skipped:
			if child.Status != "" {
				fmt.Fprintf(stdout, "  Skipped %s (status: %s)\n", child.BeadID, child.Status) //nolint:errcheck
			} else {
				fmt.Fprintf(stdout, "  Skipped %s — already routed to %s\n", child.BeadID, result.Target) //nolint:errcheck
			}
		case child.Failed:
			fmt.Fprintf(stderr, "  Failed %s: %s\n", child.BeadID, child.FailReason) //nolint:errcheck
		case child.Routed:
			switch {
			case child.WorkflowID != "":
				fmt.Fprintf(stdout, "  Attached workflow %s (formula %q) to %s\n", child.WorkflowID, child.FormulaName, child.BeadID) //nolint:errcheck
			case child.WispRootID != "":
				label := "formula"
				if result.Method == "batch-default-on" {
					label = "default formula"
				}
				fmt.Fprintf(stdout, "  Attached wisp %s (%s %q) → %s\n", child.WispRootID, label, child.FormulaName, child.BeadID) //nolint:errcheck
			}
			if child.WorkflowID == "" {
				fmt.Fprintf(stdout, "  Slung %s → %s\n", child.BeadID, result.Target) //nolint:errcheck
			}
		}
	}

	// Summary.
	summary := fmt.Sprintf("Slung %d/%d children of %s → %s", result.Routed, result.Total, result.BeadID, result.Target)
	if result.IdempotentCt > 0 {
		summary += fmt.Sprintf(" (%d already routed)", result.IdempotentCt)
	}
	fmt.Fprintln(stdout, summary) //nolint:errcheck
}

// doSling creates a Sling instance and dispatches to the right intent method.
func doSling(opts slingOpts, deps slingDeps, querier BeadQuerier, stdout, stderr io.Writer) int {
	populateSlingDepsCallbacks(&deps)
	sl, newErr := sling.New(deps)
	if newErr != nil {
		fmt.Fprintln(stderr, newErr) //nolint:errcheck
		return 1
	}
	_ = context.Background() // ctx available for future intent API use

	// Validate scope requires a formula.
	if opts.ScopeKind != "" && !opts.IsFormula && opts.OnFormula == "" &&
		(opts.NoFormula || opts.Target.EffectiveDefaultSlingFormula() == "") {
		fmt.Fprintln(stderr, "--scope-kind/--scope-ref require a formula-backed workflow launch") //nolint:errcheck
		return 1
	}

	// Use the legacy DoSling when a custom querier is provided (tests).
	// The intent API uses deps.Store as the querier; tests may inject
	// a different querier with pre-seeded beads.
	_ = sl // Sling instance available for future direct use
	result, err := sling.DoSling(opts, deps, querier)
	// Always print warnings (suspended, pool-empty, bead warnings)
	// even when the operation fails -- they provide context for the error.
	printSlingWarnings(result, stderr)
	if err != nil {
		var conflictErr *sourceworkflow.ConflictError
		if errors.As(err, &conflictErr) {
			printSourceWorkflowConflict(stderr, conflictErr, deps.StoreRef)
			return 3
		}
		var missingBeadErr *sling.MissingBeadError
		if errors.As(err, &missingBeadErr) {
			printMissingBeadError(stderr, missingBeadErr, missingBeadForceApplies(opts))
			return 1
		}
		fmt.Fprintln(stderr, err) //nolint:errcheck
		return 1
	}
	printSlingResult(result, stdout, stderr)
	// Dry-run: display the CLI preview using the domain result.
	if result.DryRun {
		return dryRunSingle(opts, deps, querier, stdout, stderr)
	}
	if result.NudgeAgent != nil {
		doSlingNudge(result.NudgeAgent, deps.CityName, deps.CityPath, deps.Cfg, deps.SP, deps.Store, stdout, stderr)
	}
	return 0
}

// doSlingBatch creates a Sling instance and dispatches batch or single.
func doSlingBatch(opts slingOpts, deps slingDeps, querier BeadChildQuerier, stdout, stderr io.Writer) int {
	populateSlingDepsCallbacks(&deps)
	sl, newErr := sling.New(deps)
	if newErr != nil {
		fmt.Fprintln(stderr, newErr) //nolint:errcheck
		return 1
	}
	_ = context.Background() // ctx available for future intent API use

	// For formula/on-formula batch, delegate to the old DoSlingBatch
	// which handles per-child formula attachment internally.
	// ExpandConvoy is for plain bead routing of convoy children.
	var result sling.SlingResult
	var err error
	if opts.IsFormula || opts.OnFormula != "" || (!opts.NoFormula && opts.Target.EffectiveDefaultSlingFormula() != "") {
		// Formula paths need per-child wisp attachment -- use legacy API.
		result, err = sling.DoSlingBatch(opts, deps, querier)
	} else {
		result, err = sl.ExpandConvoy(context.Background(), opts.BeadOrFormula, opts.Target, sling.RouteOpts{
			Merge:      opts.Merge,
			NoConvoy:   opts.NoConvoy,
			Owned:      opts.Owned,
			Nudge:      opts.Nudge,
			Force:      opts.Force,
			SkipPoke:   opts.SkipPoke,
			DryRun:     opts.DryRun,
			InlineText: opts.InlineText,
		}, querier)
	}
	// Print warnings before error check so they're visible on failure.
	printSlingWarnings(result, stderr)
	// Always print results when we have children (partial failures
	// should still show per-child status).
	if len(result.Children) > 0 {
		printBatchSlingResult(result, stdout, stderr)
	} else if err == nil {
		printSlingResult(result, stdout, stderr)
	}
	if err != nil {
		// Batch can surface multiple typed conflicts (one per conflicted
		// child) via errors.Join. Walking the tree renders a cleanup
		// hint per affected source bead so a user with N conflicting
		// children sees N cleanup commands instead of a single hint
		// misattributed to the first child.
		if printed := printSourceWorkflowConflicts(stderr, err, deps.StoreRef); printed > 0 {
			return 3
		}
		var missingBeadErr *sling.MissingBeadError
		if errors.As(err, &missingBeadErr) {
			printMissingBeadError(stderr, missingBeadErr, missingBeadForceApplies(opts))
			return 1
		}
		// In batch mode, per-child FailReasons have already been rendered
		// by printBatchSlingResult above. The error returned from
		// DoSlingBatch is an errors.Join of a "N/M children failed"
		// summary plus each child's typed error (kept for errors.As),
		// so printing it verbatim duplicates every child line. Summarize
		// instead when we have per-child detail.
		if len(result.Children) > 0 {
			fmt.Fprintf(stderr, "%d/%d children failed\n", result.Failed, result.Failed+result.Routed+result.IdempotentCt) //nolint:errcheck
		} else {
			fmt.Fprintln(stderr, err) //nolint:errcheck
		}
		return 1
	}
	if result.DryRun {
		// For batch dry-run, look up the container bead for display.
		if querier != nil {
			if b, getErr := querier.Get(opts.BeadOrFormula); getErr == nil {
				children, _ := querier.List(beads.ListQuery{
					ParentID: b.ID, IncludeClosed: true, Sort: beads.SortCreatedAsc,
				})
				var open []beads.Bead
				for _, c := range children {
					if c.Status == "open" {
						open = append(open, c)
					}
				}
				return dryRunBatch(opts, deps, stdout, stderr, b, children, open, querier)
			}
		}
		return dryRunSingle(opts, deps, querier, stdout, stderr)
	}
	if result.NudgeAgent != nil {
		doSlingNudge(result.NudgeAgent, deps.CityName, deps.CityPath, deps.Cfg, deps.SP, deps.Store, stdout, stderr)
	}
	return 0
}

func printMissingBeadError(stderr io.Writer, err *sling.MissingBeadError, allowForce bool) {
	fmt.Fprintln(stderr, err) //nolint:errcheck
	if allowForce {
		fmt.Fprintln(stderr, "  verify the bead ID, or use --force if it exists in a remote view not yet synced locally") //nolint:errcheck
		return
	}
	fmt.Fprintln(stderr, "  verify the bead ID; --force does not bypass missing source validation for formula-backed routes") //nolint:errcheck
}

func missingBeadForceApplies(opts sling.SlingOpts) bool {
	return !opts.IsFormula && opts.OnFormula == "" && (opts.NoFormula || opts.Target.EffectiveDefaultSlingFormula() == "")
}

func sourceWorkflowCleanupCommand(sourceBeadID, storeRef string) string {
	args := []string{"gc", "workflow", "delete-source", sourceBeadID}
	if storeRef = strings.TrimSpace(storeRef); storeRef != "" {
		args = append(args, "--store-ref", storeRef)
	}
	args = append(args, "--apply")
	return shellquote.Join(args)
}

func printSourceWorkflowConflict(stderr io.Writer, conflictErr *sourceworkflow.ConflictError, storeRef string) {
	if conflictErr == nil {
		return
	}
	_, _ = fmt.Fprintf(
		stderr,
		"gc sling: source bead %s already has live workflow(s): %s\n",
		conflictErr.SourceBeadID,
		strings.Join(conflictErr.WorkflowIDs, ","),
	)
	_, _ = fmt.Fprintf(
		stderr,
		"gc sling: use --force to override, or %s to clean up\n",
		sourceWorkflowCleanupCommand(conflictErr.SourceBeadID, storeRef),
	)
}

// printSourceWorkflowConflicts renders every *ConflictError in the error
// chain. Batch preflight emits one ConflictError per conflicted child via
// errors.Join; printing only the first (via errors.As) misattributes the
// later children's blocking roots to the first child and suggests a
// cleanup command that can only fix part of the batch.
func printSourceWorkflowConflicts(stderr io.Writer, err error, storeRef string) (printed int) {
	collectConflictErrors(err, func(c *sourceworkflow.ConflictError) {
		printSourceWorkflowConflict(stderr, c, storeRef)
		printed++
	})
	return printed
}

// collectConflictErrors walks an error tree (including errors.Join trees)
// and invokes visit for each *sourceworkflow.ConflictError encountered
// exactly once. Walks nodes directly via type assertion + Unwrap to avoid
// the errors.As "first match in chain" behavior which would double-visit
// conflicts that are themselves members of a multi-error.
func collectConflictErrors(err error, visit func(*sourceworkflow.ConflictError)) {
	if err == nil {
		return
	}
	// Intentional direct type assertion (not errors.As) so each node in the
	// error tree is visited exactly once — errors.As returns the first match
	// in the chain and we'd lose later ConflictErrors joined via errors.Join.
	if c, ok := err.(*sourceworkflow.ConflictError); ok { //nolint:errorlint
		visit(c)
	}
	type multiUnwrap interface{ Unwrap() []error }
	if mu, ok := err.(multiUnwrap); ok { //nolint:errorlint
		for _, child := range mu.Unwrap() {
			collectConflictErrors(child, visit)
		}
		return
	}
	if inner := errors.Unwrap(err); inner != nil {
		collectConflictErrors(inner, visit)
	}
}

// buildSlingFormulaVars merges caller-provided vars with the runtime context
// needed by common work formulas. Explicit --var entries always win.
func buildSlingFormulaVars(formulaName, beadID string, userVars []string, a config.Agent, deps slingDeps) map[string]string {
	vars := make(map[string]string, len(userVars)+6)
	for _, v := range userVars {
		key, value, ok := strings.Cut(v, "=")
		if ok && key != "" {
			vars[key] = value
		}
	}
	addVar := func(key, value string) {
		if value == "" {
			return
		}
		if _, explicit := vars[key]; explicit {
			return
		}
		vars[key] = value
	}
	addRoutingVar := func(key, value string) {
		if _, explicit := vars[key]; explicit {
			return
		}
		vars[key] = value
	}

	if beadID != "" {
		// Attached work formulas conventionally expect issue=<bead-id>.
		addVar("issue", beadID)
	}
	addRoutingVar("rig_name", a.Dir)
	addRoutingVar("binding_name", a.BindingName)
	addRoutingVar("binding_prefix", a.BindingPrefix())

	autoBranch := slingFormulaTargetBranch(beadID, deps, a)
	if slingFormulaUsesBaseBranch(formulaName) {
		addVar("base_branch", autoBranch)
	}
	if slingFormulaUsesTargetBranch(formulaName) {
		addVar("target_branch", autoBranch)
	}

	return vars
}

// slingFormulaSearchPaths returns the formula search paths for the current
// sling context. Uses the target agent's rig to select rig-specific layers,
// falling back to city-level layers via FormulaLayers.SearchPaths.
//
// slingFormulaTargetBranch resolves the branch used as base_branch /
// target_branch in formula vars. Resolution order:
//  1. metadata.target on the work bead (or convoy ancestor)
//  2. DefaultBranch recorded on the bead's rig in city.toml
//  3. DefaultBranch recorded on the agent's rig in city.toml
//  4. Live probe of the rig repo (git symbolic-ref origin/HEAD)
func slingFormulaTargetBranch(beadID string, deps slingDeps, a config.Agent) string {
	if deps.Branches == nil {
		deps.Branches = cliBranchResolver{}
	}
	return sling.SlingFormulaTargetBranch(beadID, deps, a)
}

func slingFormulaUsesBaseBranch(formula string) bool {
	return strings.HasPrefix(formula, "mol-polecat-") || formula == "mol-scoped-work"
}

func slingFormulaUsesTargetBranch(formula string) bool {
	return formula == "mol-refinery-patrol"
}

// resolveSlingEnv returns extra env vars for the sling command.
// For fixed single-session agents, resolves the target's session name from
// the bead store and returns it as GC_SLING_TARGET. Default routing uses
// gc.routed_to metadata for all agents, but custom sling_query templates may
// still rely on the resolved concrete session target.

// formatBeadLabel formats a bead ID with optional title for display.
func formatBeadLabel(id, title string) string {
	if title != "" {
		return id + " — " + fmt.Sprintf("%q", title)
	}
	return id
}

// printCrossRigSection prints the Cross-rig dry-run section if applicable.
func printCrossRigSection(w func(string), beadID string, a config.Agent, cfg *config.City) {
	if msg := checkCrossRig(beadID, a, cfg); msg != "" {
		bp := sling.BeadPrefixForCity(cfg, beadID)
		rp := rigPrefixForAgent(a, cfg)
		w("Cross-rig:")
		w(fmt.Sprintf("  Bead %s (prefix %q) targets %s (rig prefix %q).", beadID, bp, a.QualifiedName(), rp))
		w("  Without --force, sling would refuse to route (exit 1).")
		w("")
	}
}

func graphWorkflowRouteVars(recipe *formula.Recipe, provided map[string]string) map[string]string {
	routeVars := make(map[string]string, len(provided))
	if recipe != nil {
		for name, def := range recipe.Vars {
			if def != nil && def.Default != nil {
				routeVars[name] = *def.Default
			}
		}
	}
	maps.Copy(routeVars, provided)
	return routeVars
}

func decorateGraphWorkflowRecipe(recipe *formula.Recipe, routeVars map[string]string, routedTo, sessionName string, store beads.Store, cityName, cityPath string, cfg *config.City) error {
	if recipe == nil {
		return fmt.Errorf("workflow recipe is nil")
	}
	defaultRoute := graphRouteBinding{QualifiedName: routedTo}
	if sessionName != "" {
		defaultRoute.SessionName = sessionName
	} else {
		defaultRoute.MetadataOnly = true
	}
	routingRigContext := graphRouteRigContext(defaultRoute.QualifiedName)
	controlRoute, err := controlDispatcherBinding(store, cityName, cfg, routingRigContext)
	if err != nil {
		return err
	}
	stepByID := make(map[string]*formula.RecipeStep, len(recipe.Steps))
	stepAlias := make(map[string]string, len(recipe.Steps))
	for i := range recipe.Steps {
		stepByID[recipe.Steps[i].ID] = &recipe.Steps[i]
		if short, ok := strings.CutPrefix(recipe.Steps[i].ID, recipe.Name+"."); ok {
			stepAlias[short] = recipe.Steps[i].ID
		}
	}
	depsByStep := make(map[string][]string, len(recipe.Deps))
	for _, dep := range recipe.Deps {
		if dep.Type != "blocks" && dep.Type != "waits-for" && dep.Type != "conditional-blocks" {
			continue
		}
		depsByStep[dep.StepID] = append(depsByStep[dep.StepID], dep.DependsOnID)
	}
	bindingCache := make(map[string]graphRouteBinding, len(recipe.Steps))
	resolving := make(map[string]bool, len(recipe.Steps))
	for i := range recipe.Steps {
		step := &recipe.Steps[i]
		if step.Metadata == nil {
			step.Metadata = make(map[string]string)
		} else {
			step.Metadata = maps.Clone(step.Metadata)
		}
		if step.IsRoot {
			step.Metadata["gc.run_target"] = routedTo
			continue
		}
		if sling.IsWorkflowTopologyKind(step.Metadata["gc.kind"]) {
			continue
		}
		binding, err := resolveGraphStepBindingWithVars(step.ID, stepByID, stepAlias, depsByStep, bindingCache, resolving, routeVars, defaultRoute, routingRigContext, store, cityName, cityPath, cfg)
		if err != nil {
			return err
		}
		if isControlDispatcherKind(step.Metadata["gc.kind"]) {
			assignGraphStepRoute(step, binding, &controlRoute)
			continue
		}
		assignGraphStepRoute(step, binding, nil)
	}
	return nil
}

func workflowStoreRefForDir(storeDir, cityPath, cityName string, cfg *config.City) string {
	if strings.TrimSpace(storeDir) == "" || strings.TrimSpace(cityPath) == "" {
		return ""
	}
	storeDir = normalizePathForCompare(storeDir)
	cityPath = normalizePathForCompare(cityPath)
	if storeDir == cityPath {
		cityName = strings.TrimSpace(cityName)
		if cityName == "" {
			cityName = "city"
		}
		return "city:" + cityName
	}
	for _, rig := range cfg.Rigs {
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		if samePath(rigPath, storeDir) {
			return "rig:" + rig.Name
		}
	}
	return ""
}

// graphRouteBinding is an alias for sling.GraphRouteBinding.
type graphRouteBinding = sling.GraphRouteBinding

type graphStepTarget struct {
	value        string
	fromAssignee bool
}

func resolveGraphStepBinding(stepID string, stepByID map[string]*formula.RecipeStep, stepAlias map[string]string, depsByStep map[string][]string, cache map[string]graphRouteBinding, resolving map[string]bool, fallback graphRouteBinding, rigContext string, store beads.Store, cityName, cityPath string, cfg *config.City) (graphRouteBinding, error) {
	return resolveGraphStepBindingWithVars(stepID, stepByID, stepAlias, depsByStep, cache, resolving, nil, fallback, rigContext, store, cityName, cityPath, cfg)
}

func resolveGraphStepBindingWithVars(stepID string, stepByID map[string]*formula.RecipeStep, stepAlias map[string]string, depsByStep map[string][]string, cache map[string]graphRouteBinding, resolving map[string]bool, routeVars map[string]string, fallback graphRouteBinding, rigContext string, store beads.Store, cityName, cityPath string, cfg *config.City) (graphRouteBinding, error) {
	if aliased, ok := stepAlias[stepID]; ok {
		stepID = aliased
	}
	if binding, ok := cache[stepID]; ok {
		return binding, nil
	}
	if resolving[stepID] {
		return graphRouteBinding{}, fmt.Errorf("graph.v2 routing cycle while resolving %s", stepID)
	}
	step := stepByID[stepID]
	if step == nil {
		return fallback, nil
	}
	resolving[stepID] = true
	defer delete(resolving, stepID)

	target := graphStepRouteTarget(step, routeVars)
	if target.value == "" {
		switch step.Metadata["gc.kind"] {
		case "scope-check":
			controlTarget := strings.TrimSpace(step.Metadata["gc.control_for"])
			if controlTarget != "" {
				binding, err := resolveGraphStepBindingWithVars(controlTarget, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cityPath, cfg)
				if err != nil {
					return graphRouteBinding{}, err
				}
				cache[stepID] = binding
				return binding, nil
			}
		case "fanout":
			controlTarget := strings.TrimSpace(step.Metadata["gc.control_for"])
			if controlTarget != "" {
				binding, err := resolveGraphStepBindingWithVars(controlTarget, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cityPath, cfg)
				if err != nil {
					return graphRouteBinding{}, err
				}
				cache[stepID] = binding
				return binding, nil
			}
		case "workflow-finalize":
			cache[stepID] = fallback
			return fallback, nil
		case "retry-eval":
			var subjectID string
			for _, depID := range depsByStep[step.ID] {
				depStep := stepByID[depID]
				if depStep == nil {
					continue
				}
				switch depStep.Metadata["gc.kind"] {
				case "retry-run", "run":
					subjectID = depID
				}
				if subjectID != "" {
					break
				}
			}
			if subjectID == "" && len(depsByStep[step.ID]) > 0 {
				subjectID = depsByStep[step.ID][0]
			}
			if subjectID != "" {
				binding, err := resolveGraphStepBindingWithVars(subjectID, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cityPath, cfg)
				if err != nil {
					return graphRouteBinding{}, err
				}
				cache[stepID] = binding
				return binding, nil
			}
		case "check":
			var resolved graphRouteBinding
			found := false
			for _, depID := range depsByStep[step.ID] {
				if depID == "" {
					continue
				}
				binding, err := resolveGraphStepBindingWithVars(depID, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cityPath, cfg)
				if err != nil {
					return graphRouteBinding{}, err
				}
				if !found {
					resolved = binding
					found = true
					continue
				}
				if binding != resolved {
					return graphRouteBinding{}, fmt.Errorf("step %s: inconsistent control routing between deps (%+v vs %+v)", stepID, resolved, binding)
				}
			}
			if found {
				cache[stepID] = resolved
				return resolved, nil
			}
		}
		cache[stepID] = fallback
		return fallback, nil
	}

	if cfg == nil {
		return graphRouteBinding{}, fmt.Errorf("graph.v2 routing for %s requires config", stepID)
	}
	if target.fromAssignee {
		binding, ok, err := resolveGraphDirectSessionBinding(store, cityName, cityPath, cfg, target.value, rigContext)
		if err != nil {
			return graphRouteBinding{}, fmt.Errorf("step %s: %w", stepID, err)
		}
		if ok {
			cache[stepID] = binding
			return binding, nil
		}
		return graphRouteBinding{}, fmt.Errorf("step %s: assignee target %q did not resolve to a concrete session; use gc.run_target for config routing", stepID, target.value)
	}
	agentCfg, ok := resolveAgentIdentity(cfg, target.value, rigContext)
	if !ok {
		return graphRouteBinding{}, fmt.Errorf("step %s: unknown graph.v2 target %q", stepID, target.value)
	}
	binding := graphRouteBinding{QualifiedName: agentCfg.QualifiedName()}
	if agentCfg.SupportsInstanceExpansion() {
		binding.MetadataOnly = true
		cache[stepID] = binding
		return binding, nil
	}
	sn := lookupSessionNameOrLegacy(store, cityName, agentCfg.QualifiedName(), cfg.Workspace.SessionTemplate)
	if sn == "" {
		return graphRouteBinding{}, fmt.Errorf("step %s: could not resolve session name for %q", stepID, agentCfg.QualifiedName())
	}
	binding.SessionName = sn
	cache[stepID] = binding
	return binding, nil
}

func graphStepRouteTarget(step *formula.RecipeStep, routeVars map[string]string) graphStepTarget {
	if step == nil {
		return graphStepTarget{}
	}
	target := strings.TrimSpace(formula.Substitute(step.Assignee, routeVars))
	if target != "" {
		return graphStepTarget{value: target, fromAssignee: true}
	}
	if step.Metadata == nil {
		return graphStepTarget{}
	}
	return graphStepTarget{value: strings.TrimSpace(formula.Substitute(step.Metadata["gc.run_target"], routeVars))}
}

func resolveGraphDirectSessionBinding(store beads.Store, cityName, cityPath string, cfg *config.City, target, rigContext string) (graphRouteBinding, bool, error) {
	target = strings.TrimSpace(target)
	if store == nil || target == "" {
		return graphRouteBinding{}, false, nil
	}
	if cfg == nil {
		id, err := session.ResolveSessionID(store, target)
		if err != nil {
			return graphRouteBinding{}, false, nil
		}
		if bead, getErr := store.Get(id); getErr == nil && session.IsSessionBeadOrRepairable(bead) && bead.Status != "closed" {
			return graphRouteBinding{DirectSessionID: bead.ID}, true, nil
		}
		return graphRouteBinding{}, false, nil
	}
	if cityName == "" {
		cityName = config.EffectiveCityName(cfg, filepath.Base(cityPath))
	}
	spec, ok, err := resolveNamedSessionSpecForConfigTarget(cfg, cityName, target, rigContext)
	if err != nil {
		return graphRouteBinding{}, false, err
	}
	if !ok {
		// Exact session bead IDs are unambiguous and must win even when they
		// collide with a config target name.
		if id, err := session.ResolveSessionIDByExactID(store, target); err == nil {
			if bead, getErr := store.Get(id); getErr == nil && session.IsSessionBeadOrRepairable(bead) && bead.Status != "closed" {
				return graphRouteBinding{DirectSessionID: bead.ID}, true, nil
			}
		}
		if _, ok := resolveAgentIdentity(cfg, target, rigContext); ok {
			return graphRouteBinding{}, false, nil
		}
		if id, err := session.ResolveSessionID(store, target); err == nil {
			if bead, getErr := store.Get(id); getErr == nil && session.IsSessionBeadOrRepairable(bead) && bead.Status != "closed" {
				return graphRouteBinding{DirectSessionID: bead.ID}, true, nil
			}
		}
		return graphRouteBinding{}, false, nil
	}
	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, spec.Identity)
	if err != nil {
		return graphRouteBinding{}, false, err
	}
	return graphRouteBinding{DirectSessionID: id}, true, nil
}

func graphRouteRigContext(route string) string {
	route = strings.TrimSpace(route)
	if route == "" {
		return ""
	}
	idx := strings.LastIndex(route, "/")
	if idx <= 0 {
		return ""
	}
	return route[:idx]
}

// targetType returns "pool" or "agent" for telemetry attributes.
func targetType(a *config.Agent) string {
	if a.SupportsInstanceExpansion() {
		return "pool"
	}
	return "agent"
}

// beadCheckResult is an alias for sling.BeadCheckResult.
type beadCheckResult = sling.BeadCheckResult

// checkBeadState delegates to sling.CheckBeadState.
//
//nolint:unparam // kept explicit to mirror the production call shape used by tests.
func checkBeadState(q BeadQuerier, beadID string, a config.Agent) beadCheckResult {
	// Build a minimal SlingDeps for the check (only needs IsMultiSession).
	deps := sling.SlingDeps{}
	return sling.CheckBeadState(q, beadID, a, deps)
}

// doSlingNudge sends a nudge to the target agent after routing.
// For multi-session configs, nudges the first running instance. If the target is not
// running, pokes the controller to trigger an immediate reconciler tick
// so WakeWork can wake the session without waiting for the next patrol.
func doSlingNudge(a *config.Agent, cityName, cityPath string, cfg *config.City,
	sp runtime.Provider, store beads.Store, stdout, stderr io.Writer,
) {
	st := cfg.Workspace.SessionTemplate

	if a.Suspended {
		fmt.Fprintf(stderr, "cannot nudge: agent %q is suspended\n", a.QualifiedName()) //nolint:errcheck // best-effort
		return
	}

	if a.SupportsInstanceExpansion() {
		// Find a running multi-session instance to nudge.
		sp0 := scaleParamsFor(a)
		for _, qn := range discoverPoolInstances(a.Name, a.Dir, sp0, a, cityName, st, sp) {
			sn := lookupSessionNameOrLegacy(store, cityName, qn, st)
			running, err := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, sn)
			if err == nil && running {
				member, ok := resolveAgentIdentity(cfg, qn, currentRigContext(cfg))
				if !ok {
					fmt.Fprintf(stderr, "gc sling: agent %q not found in config\n", qn) //nolint:errcheck // best-effort
					return
				}
				target := buildSlingNudgeTarget(member, cityName, cityPath, cfg, store, sn)
				deliverSlingNudge(target, sp, store, cityPath, stdout, stderr)
				return
			}
		}
		// No running config session — poke controller for immediate wake.
		if err := pokeController(cityPath); err != nil {
			fmt.Fprintf(stderr, "No running sessions for %q; poke failed: %v\n", a.QualifiedName(), err) //nolint:errcheck // best-effort
		} else {
			fmt.Fprintf(stdout, "No running sessions for %q — poked controller for wake\n", a.QualifiedName()) //nolint:errcheck // best-effort
		}
		return
	}

	// Fixed agent: nudge directly.
	sn := lookupSessionNameOrLegacy(store, cityName, a.QualifiedName(), st)
	target := buildSlingNudgeTarget(*a, cityName, cityPath, cfg, store, sn)
	deliverSlingNudge(target, sp, store, cityPath, stdout, stderr)
}

// pokeController sends a "poke" command to the controller socket to
// trigger an immediate reconciler tick. If the per-city controller
// socket doesn't exist (supervisor model), falls back to sending
// "reload" to the supervisor socket.
func pokeController(cityPath string) error {
	_, err := sendControllerCommand(cityPath, "poke")
	if err == nil {
		return nil
	}
	// Fall back to supervisor reload.
	return pokeSupervisor()
}

// reloadControllerConfig asks the controller to reload config immediately.
// If the per-city controller socket doesn't exist (supervisor model), falls
// back to sending "reload" to the supervisor socket.
func reloadControllerConfig(cityPath string) error {
	_, err := sendControllerCommand(cityPath, "reload")
	if err == nil {
		return nil
	}
	return pokeSupervisor()
}

// pokeSupervisor sends a best-effort "reload" command to the supervisor
// socket to trigger immediate reconciliation of all managed cities.
//
// Unlike `gc supervisor reload`, this is an opportunistic wake path used by
// commands like `gc sling` after the workflow has already been created. It
// must not wait for the full supervisor reconcile to finish, or the caller can
// block for minutes even though the wake was already queued.
func pokeSupervisor() error {
	sockPath := supervisorSocketPath()
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to supervisor: %w", err)
	}
	defer conn.Close()                                     //nolint:errcheck
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	if _, err := conn.Write([]byte("reload\n")); err != nil {
		return fmt.Errorf("sending reload: %w", err)
	}
	return nil
}

func buildSlingNudgeTarget(agent config.Agent, cityName, cityPath string, cfg *config.City, store beads.Store, sessionName string) nudgeTarget {
	resolved, _ := config.ResolveProvider(&agent, &cfg.Workspace, cfg.Providers, exec.LookPath)
	return withNudgeTargetFence(store, nudgeTarget{
		cityPath:    cityPath,
		cityName:    cityName,
		cfg:         cfg,
		agent:       agent,
		resolved:    resolved,
		sessionName: sessionName,
	})
}

func deliverSlingNudge(target nudgeTarget, sp runtime.Provider, store beads.Store, cityPath string, stdout, stderr io.Writer) {
	const msg = "Work slung. Check your hook."
	obs, err := workerObserveNudgeTarget(target, store, sp)
	running := err == nil && obs.Running
	now := time.Now()
	if running {
		handle, err := workerHandleForNudgeTarget(target, store, sp)
		if err == nil {
			result, nudgeErr := handle.Nudge(context.Background(), worker.NudgeRequest{
				Text:     msg,
				Delivery: worker.NudgeDeliveryWaitIdle,
				Source:   "sling",
				Wake:     worker.NudgeWakeLiveOnly,
			})
			if nudgeErr == nil && result.Delivered {
				telemetry.RecordNudge(context.Background(), target.agent.QualifiedName(), nil)
				fmt.Fprintf(stdout, "Nudged %s\n", target.agent.QualifiedName()) //nolint:errcheck // best-effort
				return
			}
		}
	}

	if err := enqueueQueuedNudgeWithStore(target.cityPath, store, newQueuedNudge(target.agent.QualifiedName(), msg, "sling", now)); err != nil {
		telemetry.RecordNudge(context.Background(), target.agent.QualifiedName(), err)
		fmt.Fprintf(stderr, "gc sling: nudge failed: %v\n", err) //nolint:errcheck // best-effort
		return
	}
	if running {
		maybeStartNudgePoller(target)
	} else if err := pokeController(cityPath); err != nil {
		fmt.Fprintf(stderr, "Session %q is asleep; poke failed: %v\n", target.agent.QualifiedName(), err) //nolint:errcheck // best-effort
	} else {
		fmt.Fprintf(stdout, "Session %q is asleep — poked controller for wake\n", target.agent.QualifiedName()) //nolint:errcheck // best-effort
	}
	fmt.Fprintf(stdout, "Queued nudge for %s\n", target.agent.QualifiedName()) //nolint:errcheck // best-effort
}

// dryRunSingle prints a step-by-step preview of what gc sling would do for a
// single bead (or formula) without executing any side effects.
func dryRunSingle(opts slingOpts, deps slingDeps, querier BeadQuerier, stdout, stderr io.Writer) int {
	a := opts.Target
	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort

	// Header.
	header := "Dry run: gc sling " + a.QualifiedName() + " " + opts.BeadOrFormula
	if opts.IsFormula {
		header += " --formula"
	}
	if opts.OnFormula != "" {
		header += " --on=" + opts.OnFormula
	}
	w(header)
	w("")

	// Target section.
	printTarget(w, a, deps.CityPath, deps.CityName, deps.Cfg.Rigs, io.Discard)

	// Formula mode.
	if opts.IsFormula {
		w("Formula:")
		w("  Name: " + opts.BeadOrFormula)
		w("  A formula is a template for structured work. --formula creates a")
		w("  wisp (ephemeral molecule) — a tree of step beads that guide the")
		w("  agent through the workflow.")
		w("")
		cookCmd := fmt.Sprintf("bd mol cook --formula=%s", opts.BeadOrFormula)
		if opts.Title != "" {
			cookCmd += fmt.Sprintf(" --title=%s", opts.Title)
		}
		w("  Would run: " + cookCmd)
		w("  This creates a wisp and returns its root bead ID.")
		w("")

		routeCmd, _ := sling.BuildSlingCommandForAgent("sling_query", a.EffectiveSlingQuery(), "<wisp-root>", deps.CityPath, deps.CityName, a, deps.Cfg.Rigs)
		w("Route command (not executed):")
		w("  " + routeCmd)
		w("  The wisp root bead (not the formula name) is routed to the agent.")
		w("")
	} else {
		if opts.InlineText {
			w("Work:")
			w("  Would create new task bead with title=" + fmt.Sprintf("%q", opts.BeadOrFormula))
			w("")
		} else {
			printBeadInfo(w, querier, opts.BeadOrFormula)
			printCrossRigSection(w, opts.BeadOrFormula, a, deps.Cfg)

			check := sling.CheckBeadState(querier, opts.BeadOrFormula, a, deps)
			if check.Idempotent {
				w("Idempotency:")
				w("  Bead " + opts.BeadOrFormula + " is already routed to " + a.QualifiedName() + ".")
				w("  Without --force, sling would skip routing (exit 0).")
				w("")
			}
		}

		// Inline-text previews skip the molecule pre-check: the bead
		// does not exist yet, so the "no existing children" claim
		// would be vacuously true and misleading.
		preCheck := !opts.InlineText
		// In inline-text mode the live path creates a fresh bead first
		// and operates on the new ID; reuse a placeholder in preview
		// commands so operators don't read the inline title as the bead
		// ID a real run would attach to or route.
		previewBeadID := opts.BeadOrFormula
		if opts.InlineText {
			previewBeadID = "<new-bead-id>"
		}
		if opts.OnFormula != "" {
			if preCheck {
				if rc := dryRunReportBlockingMolecule(opts, deps, querier, stderr); rc != 0 {
					return rc
				}
			}
			w("Attach formula:")
			w("  Formula: " + opts.OnFormula)
			w("  --on attaches a wisp (structured work instructions) to an existing")
			w("  bead. The agent receives the original bead with the workflow")
			w("  attached, rather than a standalone wisp.")
			w("")
			cookCmd := fmt.Sprintf("bd mol cook --formula=%s --on=%s", opts.OnFormula, previewBeadID)
			if opts.Title != "" {
				cookCmd += fmt.Sprintf(" --title=%s", opts.Title)
			}
			w("  Would run: " + cookCmd)
			if preCheck {
				w("  Pre-check: " + opts.BeadOrFormula + " has no existing molecule/wisp children ✓")
			}
			w("")
		} else if !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
			if preCheck {
				if rc := dryRunReportBlockingMolecule(opts, deps, querier, stderr); rc != 0 {
					return rc
				}
			}
			w("Default formula:")
			w("  Formula: " + a.EffectiveDefaultSlingFormula())
			w("  Target " + a.QualifiedName() + " has a default_sling_formula configured.")
			w("  A wisp will be attached automatically (use --no-formula to suppress).")
			w("")
			cookCmd := fmt.Sprintf("bd mol cook --formula=%s --on=%s", a.EffectiveDefaultSlingFormula(), previewBeadID)
			if opts.Title != "" {
				cookCmd += fmt.Sprintf(" --title=%s", opts.Title)
			}
			w("  Would run: " + cookCmd)
			if preCheck {
				w("  Pre-check: " + opts.BeadOrFormula + " has no existing molecule/wisp children ✓")
			}
			w("")
		}

		routeCmd, _ := sling.BuildSlingCommandForAgent("sling_query", a.EffectiveSlingQuery(), previewBeadID, deps.CityPath, deps.CityName, a, deps.Cfg.Rigs)
		w("Route command (not executed):")
		w("  " + routeCmd)
		if !sling.IsCustomSlingQuery(a) {
			if a.SupportsInstanceExpansion() {
				w("  This routes the bead to session config \"" + a.QualifiedName() + "\".")
			} else {
				w("  This assigns the bead to \"" + a.QualifiedName() + "\".")
			}
		}
		w("")
	}

	// Nudge section.
	if opts.Nudge {
		printNudgePreview(w, a, deps.CityName, deps.SP, deps.Store, deps.Cfg)
	}

	w("No side effects executed (--dry-run).")
	return 0
}

// dryRunBatch prints a step-by-step preview of what gc sling would do for a
// convoy without executing any side effects.
func dryRunBatch(opts slingOpts, deps slingDeps, stdout, _ io.Writer,
	b beads.Bead, children, open []beads.Bead, querier BeadQuerier,
) int {
	a := opts.Target
	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort

	// Header.
	w("Dry run: gc sling " + a.QualifiedName() + " " + b.ID)
	w("")

	// Target section.
	printTarget(w, a, deps.CityPath, deps.CityName, deps.Cfg.Rigs, io.Discard)

	// Work section — container.
	w("Work:")
	label := formatBeadLabel(b.ID, b.Title)
	w("  Bead: " + label)
	w("  Type: " + b.Type)
	w("")
	w("  A " + b.Type + " is a container bead that groups related work. Sling")
	w("  expands it and routes each open child individually.")
	w("")

	// Cross-rig section — show when container bead prefix doesn't match agent's rig.
	printCrossRigSection(w, b.ID, a, deps.Cfg)

	// Children list.
	w(fmt.Sprintf("  Children (%d total, %d open):", len(children), len(open)))
	for _, c := range children {
		clabel := sling.FormatBeadLabel(c.ID, c.Title)
		if c.Status == "open" {
			check := sling.CheckBeadState(querier, c.ID, a, deps)
			if check.Idempotent {
				w("    " + clabel + " (open) → already routed (skip)")
			} else {
				suffix := " → would route"
				if opts.OnFormula != "" || (!opts.NoFormula && a.EffectiveDefaultSlingFormula() != "") {
					suffix = " → would route + attach wisp"
				}
				w("    " + clabel + " (open)" + suffix)
			}
		} else {
			w("    " + clabel + " (" + c.Status + ") → skip")
		}
	}
	w("")

	// Attach formula section (per open child).
	if opts.OnFormula != "" {
		w("Attach formula (per open child):")
		w("  Would run:")
		for _, c := range open {
			w("    bd mol cook --formula=" + opts.OnFormula + " --on=" + c.ID)
		}
		w("")
	} else if !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
		w("Default formula (per open child):")
		w("  Formula: " + a.EffectiveDefaultSlingFormula())
		w("  Would run:")
		for _, c := range open {
			w("    bd mol cook --formula=" + a.EffectiveDefaultSlingFormula() + " --on=" + c.ID)
		}
		w("")
	}

	// Route commands.
	w("Route commands (not executed):")
	for _, c := range open {
		routeCmd, _ := sling.BuildSlingCommandForAgent("sling_query", a.EffectiveSlingQuery(), c.ID, deps.CityPath, deps.CityName, a, deps.Cfg.Rigs)
		w("  " + routeCmd)
	}
	w("")

	// Nudge section.
	if opts.Nudge {
		printNudgePreview(w, a, deps.CityName, deps.SP, deps.Store, deps.Cfg)
	}

	w("No side effects executed (--dry-run).")
	return 0
}

// printTarget prints the Target section for dry-run output.
func printTarget(w func(string), a config.Agent, cityPath, cityName string, rigs []config.Rig, stderr io.Writer) {
	w("Target:")
	if a.SupportsInstanceExpansion() {
		sp := scaleParamsFor(&a)
		maxDisplay := fmt.Sprintf("max=%d", sp.Max)
		if sp.Max < 0 {
			maxDisplay = "max=unlimited"
		}
		w(fmt.Sprintf("  Session config: %s (min=%d %s)", a.QualifiedName(), sp.Min, maxDisplay))
	} else {
		w("  Agent:       " + a.QualifiedName() + " (non-expanding template)")
	}
	sq := expandAgentCommandTemplate(cityPath, cityName, &a, rigs, "sling_query", a.EffectiveSlingQuery(), stderr)
	w("  Sling query: " + sq)
	if !isCustomSlingQuery(a) {
		if a.SupportsInstanceExpansion() {
			w("               Multi-session configs share a routed work queue via gc.routed_to.")
			w("               Any eligible session for that config can claim routed work.")
		} else {
			w("               A sling query is the shell command that routes work.")
			w("               {} is replaced with the bead ID at dispatch time.")
		}
	}
	w("")
}

// printBeadInfo prints the Work section for dry-run output. Gracefully handles
// nil querier or query failure by showing the bead ID only.
func printBeadInfo(w func(string), q BeadQuerier, beadID string) {
	w("Work:")
	if q == nil {
		w("  Bead: " + beadID)
		w("")
		return
	}
	b, err := q.Get(beadID)
	if err != nil {
		w("  Bead: " + beadID)
		w("")
		return
	}
	title := beadID
	if b.Title != "" {
		title = beadID + " — " + fmt.Sprintf("%q", b.Title)
	}
	w("  Bead:   " + title)
	if b.Type != "" {
		w("  Type:   " + b.Type)
	}
	if b.Status != "" {
		w("  Status: " + b.Status)
	}
	w("")
}

// dryRunReportBlockingMolecule returns 1 (and emits a stderr diagnostic)
// when the bead already has an attached molecule that would block
// formula attachment, otherwise 0.
func dryRunReportBlockingMolecule(opts slingOpts, deps slingDeps, querier BeadQuerier, stderr io.Writer) int {
	label, id := sling.FindBlockingMolecule(querier, opts.BeadOrFormula, deps.Store)
	if label == "" {
		return 0
	}
	fmt.Fprintf(stderr, "gc sling: bead %s already has attached %s %s\n", opts.BeadOrFormula, label, id) //nolint:errcheck // best-effort stderr
	return 1
}

// printNudgePreview prints the Nudge section for dry-run output.
func printNudgePreview(w func(string), a config.Agent, cityName string,
	sp runtime.Provider, store beads.Store, cfg *config.City,
) {
	st := cfg.Workspace.SessionTemplate
	w("Nudge:")
	sn := lookupSessionNameOrLegacy(store, cityName, a.QualifiedName(), st)
	running, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, sn)
	if err == nil && running {
		w("  Would nudge " + a.QualifiedName() + " (session " + sn + ").")
		w("  Currently: running ✓")
	} else {
		w("  Would nudge " + a.QualifiedName() + " — but no running session found.")
	}
	w("")
}

// isCustomSlingQuery returns true if the agent has a user-defined sling_query
// (not the auto-generated default).
func isCustomSlingQuery(a config.Agent) bool {
	return sling.IsCustomSlingQuery(a)
}

// looksLikeBeadID reports whether s matches the bead ID pattern: an
// alphabetic-led alphanumeric prefix, a dash, and a short alphanumeric
// suffix of 1-8 chars (e.g. "BL-42", "mp-1j1", "gc-56nqn",
// "gc-r5sr6bm"). Short suffixes (1-4 chars) are accepted
// unconditionally. Longer suffixes (5-8 chars) must contain at least
// one digit to distinguish base36 hashes from English words like
// "hello-world". This is the cfg-free heuristic and rejects bead IDs
// whose rig prefix contains a hyphen ("agent-diagnostics-hnn"); those
// are accepted by looksLikeConfiguredBeadID, which consults cfg.Rigs.
// Multi-dash strings with no matching configured rig prefix are
// treated as inline text for ad-hoc bead creation.
func looksLikeBeadID(s string) bool {
	_, baseSuffix, ok := sling.BeadIDParts(s)
	if !ok || len(baseSuffix) > 8 {
		return false
	}
	return looksLikeBeadIDSuffix(baseSuffix)
}

func looksLikeBeadIDSuffix(baseSuffix string) bool {
	if len(baseSuffix) <= 4 {
		return true
	}
	for _, c := range baseSuffix {
		if '0' <= c && c <= '9' {
			return true
		}
	}
	return false
}

func resolveInlineBeadAction(cfg *config.City, beadOrFormula string, dryRun bool, store beads.Store) (createInlineBead, previewInlineText bool, err error) {
	// Fast path: heuristics already classify this as a bead ID.
	if !looksLikeInlineText(cfg, beadOrFormula) {
		return false, false, nil
	}
	// Store probe: covers IDs that pass the shape pre-check but fail the
	// heuristic (e.g. descriptive multi-dash IDs like "fo-spawn-storm").
	// A store hit means the bead exists and should be routed, not created.
	if store != nil && isBeadIDCandidate(beadOrFormula) {
		exists, err := sling.ProbeBeadInStore(store, beadOrFormula)
		if err != nil {
			return false, false, fmt.Errorf("checking bead candidate %q: %w", beadOrFormula, err)
		}
		if exists {
			return false, false, nil
		}
	}
	if dryRun {
		return false, true, nil
	}
	return true, false, nil
}

// isBeadIDCandidate reports whether s has the shape of a potential bead ID:
// no whitespace, starts with a letter, contains only letters, digits, hyphens,
// underscores, and dots, and has at least one hyphen. Used to gate the store
// probe before falling back to inline-text creation.
func isBeadIDCandidate(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return false
	}
	first := s[0]
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') {
		return false
	}
	hasDash := false
	for _, c := range s {
		switch {
		case c == '-':
			hasDash = true
		case c == '_' || c == '.':
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		default:
			return false
		}
	}
	return hasDash
}

func looksLikeInlineText(cfg *config.City, beadOrFormula string) bool {
	return !looksLikeBeadID(beadOrFormula) && !looksLikeConfiguredBeadID(cfg, beadOrFormula)
}

func looksLikeConfiguredBeadID(cfg *config.City, s string) bool {
	return sling.LooksLikeConfiguredBeadID(cfg, s)
}

// rigPrefixForAgent returns the effective bead prefix for the rig that an
// agent belongs to. City-wide agents (Dir="") return "" (exempt from cross-rig
// checks). Returns "" if no matching rig is found (best-effort skip).
func rigPrefixForAgent(a config.Agent, cfg *config.City) string {
	if a.Dir == "" {
		return ""
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == a.Dir {
			return strings.ToLower(cfg.Rigs[i].EffectivePrefix())
		}
	}
	return ""
}

// checkCrossRig returns a non-empty error message if a bead's rig prefix
// doesn't match the target agent's rig prefix. Returns "" when the check
// passes or can't be performed (missing prefix, city-wide agent, no rig).
func checkCrossRig(beadID string, a config.Agent, cfg *config.City) string {
	bp := sling.BeadPrefixForCity(cfg, beadID)
	if bp == "" {
		return ""
	}
	rp := rigPrefixForAgent(a, cfg)
	if rp == "" {
		return ""
	}
	if bp == rp {
		return ""
	}
	return fmt.Sprintf("gc sling: cross-rig routing blocked — bead %s (prefix %q) targets %s (rig prefix %q); use --force to override",
		beadID, bp, a.QualifiedName(), rp)
}
