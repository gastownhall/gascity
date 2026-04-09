package main

import (
	"context"
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
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

// slingStdin returns the reader for --stdin input. Extracted for testability.
var slingStdin = func() io.Reader { return os.Stdin }

// BeadQuerier can retrieve a single bead by ID.
type BeadQuerier interface {
	Get(id string) (beads.Bead, error)
}

// BeadChildQuerier extends BeadQuerier with the ability to query child beads.
type BeadChildQuerier interface {
	BeadQuerier
	List(query beads.ListQuery) ([]beads.Bead, error)
}

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
		Short: "Route work to an agent or pool",
		Long: `Route a bead to an agent or pool using the target's sling_query.

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
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&formula, "formula", "f", false, "treat argument as formula name")
	cmd.Flags().BoolVar(&nudge, "nudge", false, "nudge target after routing")
	cmd.Flags().BoolVar(&force, "force", false, "suppress warnings and allow cross-rig routing")
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

// slingOpts captures the user's intent from CLI flags.
type slingOpts struct {
	Target        config.Agent
	BeadOrFormula string
	IsFormula     bool
	OnFormula     string
	NoFormula     bool
	SkipPoke      bool
	Title         string
	Vars          []string
	Merge         string // "", "direct", "mr", "local"
	NoConvoy      bool
	Owned         bool
	Nudge         bool
	Force         bool
	DryRun        bool
	ScopeKind     string
	ScopeRef      string
}

var (
	slingPokeController        = pokeController
	slingPokeControlDispatcher = pokeControlDispatch
)

// slingDeps bundles infrastructure dependencies injected for testability.
type slingDeps struct {
	CityName string
	CityPath string // city directory path; used to poke controller for wake
	Cfg      *config.City
	SP       runtime.Provider
	Runner   SlingRunner
	Store    beads.Store
	StoreRef string
	Stdout   io.Writer
	Stderr   io.Writer
}

// SlingRunner executes a shell command in the given directory with optional
// extra env vars and returns combined output. If dir is empty, the command
// inherits the caller's cwd. The env map entries are added to the process env.
type SlingRunner func(dir, command string, env map[string]string) (string, error)

func slingTracef(format string, args ...any) {
	path := strings.TrimSpace(os.Getenv("GC_SLING_TRACE"))
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()                                                                                    //nolint:errcheck // best-effort trace log
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...)) //nolint:errcheck
}

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
	if len(env) > 0 {
		cmd.Env = mergeRuntimeEnv(os.Environ(), env)
	}
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
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	applyFeatureFlags(cfg)

	var target, beadOrFormula string
	switch {
	case fromStdin:
		target = args[0]
		beadOrFormula = stdinTitle
	case len(args) == 2:
		target = args[0]
		beadOrFormula = args[1]
	default:
		// 1-arg: bead ID only, resolve target from rig's default_sling_target.
		beadOrFormula = args[0]
		if isFormula {
			fmt.Fprintf(stderr, "gc sling: --formula requires explicit target\n") //nolint:errcheck // best-effort stderr
			return 1
		}
		if !looksLikeBeadID(beadOrFormula) {
			fmt.Fprintf(stderr, "gc sling: inline text requires explicit target\n  usage: gc sling <target> %q\n", beadOrFormula) //nolint:errcheck // best-effort stderr
			return 1
		}
		bp := beadPrefix(beadOrFormula)
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
	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}

	// Determine which beads store to use. Priority:
	// 1. Bead's prefix → rig directory (the bead lives in that rig's store)
	// 2. Target agent's rig directory (mol operations create in the agent's store)
	// 3. City path (fallback for city-scoped agents)
	storeDir := cityPath
	if bp := beadPrefix(beadOrFormula); bp != "" {
		if rig, found := findRigByPrefix(cfg, bp); found {
			rigPath := rig.Path
			if !filepath.IsAbs(rigPath) {
				rigPath = filepath.Join(cityPath, rigPath)
			}
			storeDir = rigPath
		}
	}
	if samePath(storeDir, cityPath) {
		if rd := rigDirForAgent(cfg, a); rd != "" {
			storeDir = rd
		}
	}
	storeEnv := bdRuntimeEnv(cityPath)
	if !samePath(storeDir, cityPath) {
		storeEnv = bdRuntimeEnvForRig(cityPath, cfg, storeDir)
	}
	store := beads.NewBdStore(storeDir, beads.ExecCommandRunnerWithEnv(storeEnv))
	storeRef := workflowStoreRefForDir(storeDir, cityPath, cfg.Workspace.Name, cfg)

	// Inline text mode: if the argument doesn't look like a bead ID
	// (and we're not in formula mode), create a task bead from the text.
	// Skip during dry-run to avoid side effects.
	// Also check if the bead exists in the store — hierarchical IDs like
	// "Prefix-abc.1" have dots that looksLikeBeadID doesn't recognize.
	if !isFormula && !dryRun && !looksLikeBeadID(beadOrFormula) && !beadExistsInStore(store, beadOrFormula) {
		created, err := store.Create(beads.Bead{Title: beadOrFormula, Description: stdinDescription, Type: "task"})
		if err != nil {
			fmt.Fprintf(stderr, "gc sling: creating bead: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Created %s — %q\n", created.ID, beadOrFormula) //nolint:errcheck // best-effort stdout
		beadOrFormula = created.ID
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
		Stdout:   stdout,
		Stderr:   stderr,
	}

	return doSlingBatch(opts, deps, store)
}

// findRigByPrefix returns the rig whose effective prefix matches (case-insensitive).
func findRigByPrefix(cfg *config.City, prefix string) (config.Rig, bool) {
	lp := strings.ToLower(prefix)
	for _, r := range cfg.Rigs {
		if strings.ToLower(r.EffectivePrefix()) == lp {
			return r, true
		}
	}
	return config.Rig{}, false
}

// rigDirForBead resolves the rig directory for a bead ID by extracting
// the bead prefix and looking up the rig path. Returns "" if the bead
// has no prefix or no matching rig is found.
func rigDirForBead(cfg *config.City, beadID string) string {
	bp := beadPrefix(beadID)
	if bp == "" {
		return ""
	}
	if rig, ok := findRigByPrefix(cfg, bp); ok {
		return rig.Path
	}
	return ""
}

// rigDirForAgent returns the rig directory for an agent by matching its Dir
// field to a rig Name. Returns "" if the agent has no Dir (city-scoped) or
// no matching rig is found.
func rigDirForAgent(cfg *config.City, a config.Agent) string {
	if a.Dir == "" {
		return ""
	}
	for _, r := range cfg.Rigs {
		if r.Name == a.Dir {
			return r.Path
		}
	}
	return ""
}

func slingDirForBead(cfg *config.City, cityPath, beadID string) string {
	if dir := rigDirForBead(cfg, beadID); dir != "" {
		return dir
	}
	return cityPath
}

// doSling is the pure logic for gc sling. Accepts injected deps, querier,
// and opts struct for testability.
func doSling(opts slingOpts, deps slingDeps, querier BeadQuerier) int {
	a := opts.Target
	// Warn about suspended agents / empty pools (unless --force).
	if a.Suspended && !opts.Force {
		fmt.Fprintf(deps.Stderr, "warning: agent %q is suspended — bead routed but may not be picked up\n", a.QualifiedName()) //nolint:errcheck // best-effort
	}
	if sp := scaleParamsFor(&a); isMultiSessionCfgAgent(&a) && sp.Max == 0 && !opts.Force {
		fmt.Fprintf(deps.Stderr, "warning: pool %q has max=0 — bead routed but no instances to claim it\n", a.QualifiedName()) //nolint:errcheck // best-effort
	}

	// Cross-rig guard — block when a rig-scoped agent receives a bead from
	// a different rig. Only for plain bead routing (formula creates fresh wisps).
	// Dry-run shows an informational section instead of blocking.
	if !opts.IsFormula && !opts.Force && !opts.DryRun {
		if msg := checkCrossRig(opts.BeadOrFormula, a, deps.Cfg); msg != "" {
			fmt.Fprintln(deps.Stderr, msg) //nolint:errcheck // best-effort
			return 1
		}
	}

	// Pre-flight idempotency check — before formula/wisp processing so an
	// idempotent bead skips ALL mutations. Only for plain bead routing
	// (formula mode creates fresh wisps, never idempotent).
	if !opts.IsFormula && !opts.Force {
		result := checkBeadState(querier, opts.BeadOrFormula, a)
		if result.Idempotent {
			if opts.DryRun {
				return dryRunSingle(opts, deps, querier)
			}
			fmt.Fprintf(deps.Stdout, "Bead %s already routed to %s — skipping (idempotent)\n", opts.BeadOrFormula, a.QualifiedName()) //nolint:errcheck // best-effort
			return 0
		}
		for _, w := range result.Warnings {
			fmt.Fprintln(deps.Stderr, w) //nolint:errcheck // best-effort
		}
	}

	// Dry-run: resolve and print preview without executing.
	if opts.DryRun {
		return dryRunSingle(opts, deps, querier)
	}

	beadID := opts.BeadOrFormula
	method := "bead"

	if opts.ScopeKind != "" && !opts.IsFormula && opts.OnFormula == "" && (opts.NoFormula || a.EffectiveDefaultSlingFormula() == "") {
		fmt.Fprintln(deps.Stderr, "gc sling: --scope-kind/--scope-ref require a formula-backed workflow launch") //nolint:errcheck // best-effort
		return 1
	}

	// If --formula, instantiate wisp and use the root bead ID.
	if opts.IsFormula {
		method = "formula"
		formulaVars := buildSlingFormulaVars(opts.BeadOrFormula, "", opts.Vars, a, deps)
		result, err := instantiateSlingFormula(context.Background(), opts.BeadOrFormula, slingFormulaSearchPaths(deps, a), molecule.Options{
			Title: opts.Title,
			Vars:  formulaVars,
		}, "", opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: instantiating formula %q: %v\n", opts.BeadOrFormula, err) //nolint:errcheck // best-effort
			return 1
		}
		if result.GraphWorkflow || isGraphWorkflowAttachment(deps.Store, result.RootID) {
			if code := startGraphWorkflow(result, "", a, method, deps); code != 0 {
				return code
			}
			fmt.Fprintf(deps.Stdout, "Started workflow %s (formula %q) → %s\n", result.RootID, opts.BeadOrFormula, a.QualifiedName()) //nolint:errcheck // best-effort
			return 0
		}
		beadID = result.RootID
	}

	// If --on, attach a wisp to the bead and route the original bead.
	if opts.OnFormula != "" {
		method = "on-formula"
		if err := checkNoMoleculeChildren(querier, beadID, deps.Store, deps.Stderr); err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort
			return 1
		}
		formulaVars := buildSlingFormulaVars(opts.OnFormula, beadID, opts.Vars, a, deps)
		result, err := instantiateSlingFormula(context.Background(), opts.OnFormula, slingFormulaSearchPaths(deps, a), molecule.Options{
			Title:            opts.Title,
			Vars:             formulaVars,
			PriorityOverride: beadPriorityOverride(querier, beadID),
		}, beadID, opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: instantiating formula %q on %s: %v\n", opts.OnFormula, beadID, err) //nolint:errcheck // best-effort
			return 1
		}
		wispRootID := result.RootID
		if result.GraphWorkflow || isGraphWorkflowAttachment(deps.Store, wispRootID) {
			if code := startGraphWorkflow(result, beadID, a, method, deps); code != 0 {
				return code
			}
			fmt.Fprintf(deps.Stdout, "Attached workflow %s (formula %q) to %s\n", wispRootID, opts.OnFormula, beadID) //nolint:errcheck // best-effort
			return 0
		}
		// Record molecule_id on the work bead so agents can discover it
		// without traversing dependencies.
		if err := deps.Store.SetMetadata(beadID, "molecule_id", wispRootID); err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: setting molecule_id on %s: %v\n", beadID, err) //nolint:errcheck // best-effort
			// Non-fatal — wisp was already attached.
		}
		fmt.Fprintf(deps.Stdout, "Attached wisp %s (formula %q) to %s\n", wispRootID, opts.OnFormula, beadID) //nolint:errcheck // best-effort
		// beadID unchanged — route original bead.
	}

	// Apply default formula if target has one and no explicit formula/--no-formula.
	if opts.OnFormula == "" && !opts.IsFormula && !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
		method = "default-on-formula"
		if err := checkNoMoleculeChildren(querier, beadID, deps.Store, deps.Stderr); err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort
			return 1
		}
		defaultVars := buildSlingFormulaVars(a.EffectiveDefaultSlingFormula(), beadID, opts.Vars, a, deps)
		result, err := instantiateSlingFormula(context.Background(), a.EffectiveDefaultSlingFormula(), slingFormulaSearchPaths(deps, a), molecule.Options{
			Title:            opts.Title,
			Vars:             defaultVars,
			PriorityOverride: beadPriorityOverride(querier, beadID),
		}, beadID, opts.ScopeKind, opts.ScopeRef, a, deps)
		if err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: instantiating default formula %q on %s: %v\n", //nolint:errcheck // best-effort
				a.EffectiveDefaultSlingFormula(), beadID, err)
			return 1
		}
		wispRootID := result.RootID
		if result.GraphWorkflow || isGraphWorkflowAttachment(deps.Store, wispRootID) {
			if code := startGraphWorkflow(result, beadID, a, method, deps); code != 0 {
				return code
			}
			fmt.Fprintf(deps.Stdout, "Attached workflow %s (default formula %q) to %s\n", wispRootID, a.EffectiveDefaultSlingFormula(), beadID) //nolint:errcheck // best-effort
			return 0
		}
		// Record molecule_id on the work bead so agents can discover it
		// without traversing dependencies.
		if err := deps.Store.SetMetadata(beadID, "molecule_id", wispRootID); err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: setting molecule_id on %s: %v\n", beadID, err) //nolint:errcheck // best-effort
			// Non-fatal — wisp was already attached.
		}
		fmt.Fprintf(deps.Stdout, "Attached wisp %s (default formula %q) to %s\n", //nolint:errcheck // best-effort
			wispRootID, a.EffectiveDefaultSlingFormula(), beadID)
	}

	// Build and execute sling command.
	// For fixed agents, resolve the target's session name and inject it
	// as GC_SLING_TARGET so the sling query can assign work per-session.
	slingEnv := resolveSlingEnv(a, deps)
	slingCmd := buildSlingCommand(a.EffectiveSlingQuery(), beadID)
	rigDir := slingDirForBead(deps.Cfg, deps.CityPath, beadID)
	if _, err := deps.Runner(rigDir, slingCmd, slingEnv); err != nil {
		fmt.Fprintf(deps.Stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort
		telemetry.RecordSling(context.Background(), a.QualifiedName(), targetType(&a), method, err)
		return 1
	}

	telemetry.RecordSling(context.Background(), a.QualifiedName(), targetType(&a), method, nil)

	// Merge strategy metadata.
	if opts.Merge != "" && deps.Store != nil {
		if err := deps.Store.SetMetadata(beadID, "merge_strategy", opts.Merge); err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: setting merge strategy: %v\n", err) //nolint:errcheck // best-effort
			// Non-fatal — bead was already routed.
		}
	}

	// Auto-convoy: wrap single bead in a tracking convoy (unless suppressed).
	if !opts.NoConvoy && !opts.IsFormula && deps.Store != nil {
		var convoyLabels []string
		if opts.Owned {
			convoyLabels = []string{"owned"}
		}
		convoy, err := deps.Store.Create(beads.Bead{
			Title:  fmt.Sprintf("sling-%s", beadID),
			Type:   "convoy",
			Labels: convoyLabels,
		})
		if err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: creating auto-convoy: %v\n", err) //nolint:errcheck // best-effort
			// Non-fatal — bead was already routed successfully.
		} else {
			parentID := convoy.ID
			if err := deps.Store.Update(beadID, beads.UpdateOpts{ParentID: &parentID}); err != nil {
				fmt.Fprintf(deps.Stderr, "gc sling: linking bead to convoy: %v\n", err) //nolint:errcheck // best-effort
			} else {
				label := ""
				if opts.Owned {
					label = " (owned)"
				}
				fmt.Fprintf(deps.Stdout, "Auto-convoy %s%s\n", convoy.ID, label) //nolint:errcheck // best-effort
			}
		}
	}

	switch {
	case opts.IsFormula:
		fmt.Fprintf(deps.Stdout, "Slung formula %q (wisp root %s) → %s\n", opts.BeadOrFormula, beadID, a.QualifiedName()) //nolint:errcheck // best-effort
	case opts.OnFormula != "":
		fmt.Fprintf(deps.Stdout, "Slung %s (with formula %q) → %s\n", beadID, opts.OnFormula, a.QualifiedName()) //nolint:errcheck // best-effort
	default:
		fmt.Fprintf(deps.Stdout, "Slung %s → %s\n", beadID, a.QualifiedName()) //nolint:errcheck // best-effort
	}

	// Poke controller/supervisor to trigger immediate reconciliation
	// so pool agents wake without waiting for the next patrol tick.
	if !opts.SkipPoke {
		_ = slingPokeController(deps.CityPath)
	}

	// Nudge target if requested.
	if opts.Nudge {
		doSlingNudge(&a, deps.CityName, deps.CityPath, deps.Cfg, deps.SP, deps.Store, deps.Stdout, deps.Stderr)
	}

	return 0
}

// doSlingBatch handles convoy expansion before delegating to doSling.
// If the argument is a convoy, it expands open children and routes each
// individually. Otherwise it falls through to doSling.
func doSlingBatch(opts slingOpts, deps slingDeps, querier BeadChildQuerier) int {
	a := opts.Target
	// Formula mode, nil querier → delegate directly.
	if opts.IsFormula || querier == nil {
		return doSling(opts, deps, querier)
	}

	// Try to look up the bead to check if it's a container.
	b, err := querier.Get(opts.BeadOrFormula)
	if err != nil {
		// Can't query → fall through to doSling (best-effort).
		singleOpts := opts
		singleOpts.IsFormula = false
		return doSling(singleOpts, deps, querier)
	}
	if b.Type == "epic" {
		fmt.Fprintf(deps.Stderr, "gc sling: bead %s is an epic; first-class support is for convoys only\n", b.ID) //nolint:errcheck // best-effort
		return 1
	}

	if !beads.IsContainerType(b.Type) {
		singleOpts := opts
		singleOpts.IsFormula = false
		return doSling(singleOpts, deps, querier)
	}

	// Container expansion keeps closed children in the preview so the skipped
	// section and summary counts still reflect the full container state.
	children, err := querier.List(beads.ListQuery{
		ParentID:      b.ID,
		IncludeClosed: true,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		fmt.Fprintf(deps.Stderr, "gc sling: listing children of %s: %v\n", b.ID, err) //nolint:errcheck // best-effort
		return 1
	}

	// Partition children into open vs skipped.
	var open, skipped []beads.Bead
	for _, c := range children {
		if c.Status == "open" {
			open = append(open, c)
		} else {
			skipped = append(skipped, c)
		}
	}

	if len(open) == 0 {
		fmt.Fprintf(deps.Stderr, "gc sling: %s %s has no open children\n", b.Type, b.ID) //nolint:errcheck // best-effort
		return 1
	}

	// Cross-rig guard — check once on the container bead. Assumes all children
	// share the container's rig prefix. If a convoy contains beads from multiple
	// rigs, the per-child check would need to run inside the loop instead.
	if !opts.Force && !opts.DryRun {
		if msg := checkCrossRig(b.ID, a, deps.Cfg); msg != "" {
			fmt.Fprintln(deps.Stderr, msg) //nolint:errcheck // best-effort
			return 1
		}
	}

	// Pre-check: if --on or default formula, verify NO open child already has an attached molecule.
	useFormula := opts.OnFormula
	if useFormula == "" && !opts.IsFormula && !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
		useFormula = a.EffectiveDefaultSlingFormula()
	}
	if useFormula != "" {
		if err := checkBatchNoMoleculeChildren(querier, open, deps.Store, deps.Stderr); err != nil {
			fmt.Fprintf(deps.Stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort
			return 1
		}
	}

	// Dry-run: print container preview without executing.
	if opts.DryRun {
		return dryRunBatch(opts, deps, b, children, open, querier)
	}

	fmt.Fprintf(deps.Stdout, "Expanding %s %s (%d children, %d open)\n", b.Type, b.ID, len(children), len(open)) //nolint:errcheck // best-effort

	// Telemetry method.
	batchMethod := "batch"
	if opts.OnFormula != "" {
		batchMethod = "batch-on"
	} else if !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
		batchMethod = "batch-default-on"
	}

	// Route each open child.
	routed := 0
	failed := 0
	idempotent := 0
	for _, child := range open {
		// Per-child idempotency / pre-flight check (unless --force).
		if !opts.Force {
			result := checkBeadState(querier, child.ID, a)
			if result.Idempotent {
				fmt.Fprintf(deps.Stdout, "  Skipped %s — already routed to %s\n", child.ID, a.QualifiedName()) //nolint:errcheck // best-effort
				idempotent++
				continue
			}
			for _, w := range result.Warnings {
				fmt.Fprintln(deps.Stderr, w) //nolint:errcheck // best-effort
			}
		}

		// Attach wisp if --on.
		if opts.OnFormula != "" {
			childVars := buildSlingFormulaVars(opts.OnFormula, child.ID, opts.Vars, a, deps)
			cookResult, err := molecule.Cook(context.Background(), deps.Store, opts.OnFormula, slingFormulaSearchPaths(deps, a), molecule.Options{
				Title:            opts.Title,
				Vars:             childVars,
				PriorityOverride: clonePriorityPtr(child.Priority),
			})
			if err != nil {
				fmt.Fprintf(deps.Stderr, "  Failed %s: instantiating formula %q: %v\n", child.ID, opts.OnFormula, err) //nolint:errcheck // best-effort
				telemetry.RecordSling(context.Background(), a.QualifiedName(), targetType(&a), batchMethod, err)
				failed++
				continue
			}
			_ = deps.Store.SetMetadata(child.ID, "molecule_id", cookResult.RootID)             // best-effort
			fmt.Fprintf(deps.Stdout, "  Attached wisp %s → %s\n", cookResult.RootID, child.ID) //nolint:errcheck // best-effort
		} else if !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
			// Apply default formula per-child.
			childVars := buildSlingFormulaVars(a.EffectiveDefaultSlingFormula(), child.ID, opts.Vars, a, deps)
			cookResult, err := molecule.Cook(context.Background(), deps.Store, a.EffectiveDefaultSlingFormula(), slingFormulaSearchPaths(deps, a), molecule.Options{
				Title:            opts.Title,
				Vars:             childVars,
				PriorityOverride: clonePriorityPtr(child.Priority),
			})
			if err != nil {
				fmt.Fprintf(deps.Stderr, "  Failed %s: instantiating default formula %q: %v\n", child.ID, a.EffectiveDefaultSlingFormula(), err) //nolint:errcheck // best-effort
				telemetry.RecordSling(context.Background(), a.QualifiedName(), targetType(&a), batchMethod, err)
				failed++
				continue
			}
			_ = deps.Store.SetMetadata(child.ID, "molecule_id", cookResult.RootID)                               // best-effort
			fmt.Fprintf(deps.Stdout, "  Attached wisp %s (default formula) → %s\n", cookResult.RootID, child.ID) //nolint:errcheck // best-effort
		}

		childEnv := resolveSlingEnv(a, deps)
		slingCmd := buildSlingCommand(a.EffectiveSlingQuery(), child.ID)
		rigDir := slingDirForBead(deps.Cfg, deps.CityPath, child.ID)
		if _, err := deps.Runner(rigDir, slingCmd, childEnv); err != nil {
			fmt.Fprintf(deps.Stderr, "  Failed %s: %v\n", child.ID, err) //nolint:errcheck // best-effort
			telemetry.RecordSling(context.Background(), a.QualifiedName(), targetType(&a), batchMethod, err)
			failed++
			continue
		}

		telemetry.RecordSling(context.Background(), a.QualifiedName(), targetType(&a), batchMethod, nil)
		fmt.Fprintf(deps.Stdout, "  Slung %s → %s\n", child.ID, a.QualifiedName()) //nolint:errcheck // best-effort
		routed++
	}

	// Report skipped children.
	for _, child := range skipped {
		fmt.Fprintf(deps.Stdout, "  Skipped %s (status: %s)\n", child.ID, child.Status) //nolint:errcheck // best-effort
	}

	// Summary line.
	summary := fmt.Sprintf("Slung %d/%d children of %s → %s", routed, len(children), b.ID, a.QualifiedName())
	if idempotent > 0 {
		summary += fmt.Sprintf(" (%d already routed)", idempotent)
	}
	fmt.Fprintln(deps.Stdout, summary) //nolint:errcheck // best-effort

	// Nudge once after all children.
	if opts.Nudge && routed > 0 {
		doSlingNudge(&a, deps.CityName, deps.CityPath, deps.Cfg, deps.SP, deps.Store, deps.Stdout, deps.Stderr)
	}

	if failed > 0 {
		return 1
	}
	return 0
}

// buildSlingFormulaVars merges caller-provided vars with the runtime context
// needed by common work formulas. Explicit --var entries always win.
func buildSlingFormulaVars(formulaName, beadID string, userVars []string, a config.Agent, deps slingDeps) map[string]string {
	vars := make(map[string]string, len(userVars)+3)
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

	if beadID != "" {
		// Attached work formulas conventionally expect issue=<bead-id>.
		addVar("issue", beadID)
	}

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
func slingFormulaSearchPaths(deps slingDeps, a config.Agent) []string {
	if deps.Cfg == nil {
		return nil
	}
	return deps.Cfg.FormulaLayers.SearchPaths(a.Dir)
}

func slingFormulaTargetBranch(beadID string, deps slingDeps, a config.Agent) string {
	if target := beadMetadataTarget(deps.Store, beadID); target != "" {
		return target
	}
	return defaultBranchFor(slingFormulaRepoDir(beadID, deps, a))
}

func beadMetadataTarget(store beads.Store, beadID string) string {
	if store == nil || beadID == "" {
		return ""
	}

	seen := make(map[string]struct{}, 8)
	rootID := beadID
	for beadID != "" {
		if _, ok := seen[beadID]; ok {
			return ""
		}
		seen[beadID] = struct{}{}

		b, err := store.Get(beadID)
		if err != nil {
			return ""
		}
		if target := strings.TrimSpace(b.Metadata["target"]); target != "" {
			if beadID == rootID || b.Type == "convoy" {
				return target
			}
		}
		beadID = strings.TrimSpace(b.ParentID)
	}
	return ""
}

func beadPriorityOverride(store BeadQuerier, beadID string) *int {
	if store == nil || beadID == "" {
		return nil
	}
	bead, err := store.Get(beadID)
	if err != nil {
		return nil
	}
	return clonePriorityPtr(bead.Priority)
}

func clonePriorityPtr(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

func slingFormulaRepoDir(beadID string, deps slingDeps, a config.Agent) string {
	if deps.Cfg != nil {
		if dir := rigDirForBead(deps.Cfg, beadID); dir != "" {
			return dir
		}
		if dir := rigDirForAgent(deps.Cfg, a); dir != "" {
			return dir
		}
	}
	return deps.CityPath
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
func resolveSlingEnv(a config.Agent, deps slingDeps) map[string]string {
	if isMultiSessionCfgAgent(&a) {
		return nil
	}
	sn := lookupSessionNameOrLegacy(deps.Store, deps.CityName, a.QualifiedName(), deps.Cfg.Workspace.SessionTemplate)
	return map[string]string{"GC_SLING_TARGET": sn}
}

// buildSlingCommand replaces {} in the sling query template with the bead ID.
// The bead ID is shell-quoted to prevent command injection.
func buildSlingCommand(template, beadID string) string {
	return strings.ReplaceAll(template, "{}", shellquote.Quote(beadID))
}

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
		bp := beadPrefix(beadID)
		rp := rigPrefixForAgent(a, cfg)
		w("Cross-rig:")
		w(fmt.Sprintf("  Bead %s (prefix %q) targets %s (rig prefix %q).", beadID, bp, a.QualifiedName(), rp))
		w("  Without --force, sling would refuse to route (exit 1).")
		w("")
	}
}

// checkNoMoleculeChildren returns an error if the bead already has an attached
// molecule or wisp child that is still open. Closed molecules are skipped
// (defense-in-depth). Open molecules on unassigned beads are auto-burned
// (closed) to unblock re-dispatch after mid-sling failures.
func checkNoMoleculeChildren(q BeadQuerier, beadID string, store beads.Store, w io.Writer) error {
	parent, ok := beadFromGetters(beadID, q, store)
	if !ok {
		return nil
	}
	parentUnassigned := strings.TrimSpace(parent.Assignee) == ""

	var childQuerier BeadChildQuerier
	if cq, ok := q.(BeadChildQuerier); ok {
		childQuerier = cq
	} else if cq, ok := any(store).(BeadChildQuerier); ok {
		childQuerier = cq
	}
	attachments, err := collectAttachedBeads(parent, store, childQuerier)
	if err != nil && len(attachments) == 0 {
		return nil
	}

	for _, attached := range attachments {
		if attached.Status == "closed" {
			continue
		}
		if parentUnassigned && store != nil {
			if burnErr := store.Close(attached.ID); burnErr == nil {
				fmt.Fprintf(w, "Auto-burned stale %s %s on unassigned bead %s\n", attachmentLabel(attached), attached.ID, beadID) //nolint:errcheck // best-effort
				continue
			}
		}
		return fmt.Errorf("bead %s already has attached %s %s", beadID, attachmentLabel(attached), attached.ID)
	}
	return nil
}

// checkBatchNoMoleculeChildren checks all open children for existing molecule
// attachments before any wisps are created. Closed molecules are skipped.
// Open molecules on unassigned beads are auto-burned to unblock re-dispatch.
// Returns an error listing all problematic beads if any have live molecules.
func checkBatchNoMoleculeChildren(q BeadChildQuerier, open []beads.Bead, store beads.Store, w io.Writer) error {
	var problems []string
	for _, child := range open {
		attachments, err := collectAttachedBeads(child, store, q)
		if err != nil && len(attachments) == 0 {
			continue // best-effort per-child
		}
		childUnassigned := strings.TrimSpace(child.Assignee) == ""
		for _, attached := range attachments {
			if attached.Status == "closed" {
				continue
			}
			if childUnassigned && store != nil {
				if burnErr := store.Close(attached.ID); burnErr == nil {
					fmt.Fprintf(w, "Auto-burned stale %s %s on unassigned bead %s\n", attachmentLabel(attached), attached.ID, child.ID) //nolint:errcheck // best-effort
					continue
				}
			}
			problems = append(problems, fmt.Sprintf("%s (has %s %s)", child.ID, attachmentLabel(attached), attached.ID))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("cannot use --on: beads already have attached molecules: %s",
			strings.Join(problems, ", "))
	}
	return nil
}

func isGraphWorkflowAttachment(store beads.Store, rootID string) bool {
	if store == nil || rootID == "" {
		return false
	}
	b, err := store.Get(rootID)
	if err != nil {
		return false
	}
	return b.Metadata["gc.kind"] == "workflow" && b.Metadata["gc.formula_contract"] == "graph.v2"
}

func instantiateSlingFormula(ctx context.Context, formulaName string, searchPaths []string, opts molecule.Options, sourceBeadID, scopeKind, scopeRef string, a config.Agent, deps slingDeps) (*molecule.Result, error) {
	slingTracef("instantiate start formula=%s source=%s agent=%s parent=%s", formulaName, sourceBeadID, a.QualifiedName(), opts.ParentID)
	if opts.PriorityOverride == nil && sourceBeadID != "" {
		opts.PriorityOverride = beadPriorityOverride(deps.Store, sourceBeadID)
	}
	compileStart := time.Now()
	recipe, err := formula.Compile(ctx, formulaName, searchPaths, opts.Vars)
	if err != nil {
		slingTracef("instantiate compile-error formula=%s dur=%s err=%v", formulaName, time.Since(compileStart), err)
		return nil, err
	}
	slingTracef("instantiate compiled formula=%s dur=%s steps=%d", formulaName, time.Since(compileStart), len(recipe.Steps))
	if err := applyGraphRouting(recipe, &a, a.QualifiedName(), opts.Vars, sourceBeadID, scopeKind, scopeRef, deps.StoreRef, deps.Store, deps.CityName, deps.Cfg); err != nil {
		slingTracef("instantiate decorate-error formula=%s err=%v", formulaName, err)
		return nil, err
	}
	instantiateStart := time.Now()
	result, err := molecule.Instantiate(ctx, deps.Store, recipe, opts)
	if err != nil {
		slingTracef("instantiate molecule-error formula=%s dur=%s err=%v", formulaName, time.Since(instantiateStart), err)
		return nil, err
	}
	slingTracef("instantiate done formula=%s dur=%s root=%s created=%d graph=%t", formulaName, time.Since(instantiateStart), result.RootID, result.Created, result.GraphWorkflow)
	return result, nil
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

func isCompiledGraphWorkflow(recipe *formula.Recipe) bool {
	if recipe == nil || len(recipe.Steps) == 0 {
		return false
	}
	root := recipe.Steps[0]
	return root.Metadata["gc.kind"] == "workflow" && root.Metadata["gc.formula_contract"] == "graph.v2"
}

func decorateGraphWorkflowRecipe(recipe *formula.Recipe, routeVars map[string]string, sourceBeadID, scopeKind, scopeRef, rootStoreRef, routedTo, sessionName string, store beads.Store, cityName string, cfg *config.City) error {
	if recipe == nil {
		return fmt.Errorf("workflow recipe is nil")
	}
	defaultRoute := graphRouteBinding{qualifiedName: routedTo}
	if sessionName != "" {
		defaultRoute.sessionName = sessionName
	} else {
		defaultRoute.metadataOnly = true
	}
	routingRigContext := graphRouteRigContext(defaultRoute.qualifiedName)
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
		if rootStoreRef != "" {
			step.Metadata["gc.root_store_ref"] = rootStoreRef
		}
		if step.IsRoot {
			step.Metadata["gc.run_target"] = routedTo
			if sourceBeadID != "" {
				step.Metadata["gc.source_bead_id"] = sourceBeadID
			}
			if scopeKind != "" {
				step.Metadata["gc.scope_kind"] = scopeKind
			}
			if scopeRef != "" {
				step.Metadata["gc.scope_ref"] = scopeRef
			}
			continue
		}
		switch step.Metadata["gc.kind"] {
		case "workflow", "scope":
			continue
		}
		binding, err := resolveGraphStepBindingWithVars(step.ID, stepByID, stepAlias, depsByStep, bindingCache, resolving, routeVars, defaultRoute, routingRigContext, store, cityName, cfg)
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

type graphRouteBinding struct {
	qualifiedName string
	sessionName   string
	metadataOnly  bool
}

func resolveGraphStepBinding(stepID string, stepByID map[string]*formula.RecipeStep, stepAlias map[string]string, depsByStep map[string][]string, cache map[string]graphRouteBinding, resolving map[string]bool, fallback graphRouteBinding, rigContext string, store beads.Store, cityName string, cfg *config.City) (graphRouteBinding, error) {
	return resolveGraphStepBindingWithVars(stepID, stepByID, stepAlias, depsByStep, cache, resolving, nil, fallback, rigContext, store, cityName, cfg)
}

func resolveGraphStepBindingWithVars(stepID string, stepByID map[string]*formula.RecipeStep, stepAlias map[string]string, depsByStep map[string][]string, cache map[string]graphRouteBinding, resolving map[string]bool, routeVars map[string]string, fallback graphRouteBinding, rigContext string, store beads.Store, cityName string, cfg *config.City) (graphRouteBinding, error) {
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
	if target == "" {
		switch step.Metadata["gc.kind"] {
		case "scope-check":
			target = strings.TrimSpace(step.Metadata["gc.control_for"])
			if target != "" {
				binding, err := resolveGraphStepBindingWithVars(target, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg)
				if err != nil {
					return graphRouteBinding{}, err
				}
				cache[stepID] = binding
				return binding, nil
			}
		case "fanout":
			target = strings.TrimSpace(step.Metadata["gc.control_for"])
			if target != "" {
				binding, err := resolveGraphStepBindingWithVars(target, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg)
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
				binding, err := resolveGraphStepBindingWithVars(subjectID, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg)
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
				binding, err := resolveGraphStepBindingWithVars(depID, stepByID, stepAlias, depsByStep, cache, resolving, routeVars, fallback, rigContext, store, cityName, cfg)
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
	agentCfg, ok := resolveAgentIdentity(cfg, target, rigContext)
	if !ok {
		return graphRouteBinding{}, fmt.Errorf("step %s: unknown graph.v2 target %q", stepID, target)
	}
	binding := graphRouteBinding{qualifiedName: agentCfg.QualifiedName()}
	if isMultiSessionCfgAgent(&agentCfg) {
		binding.metadataOnly = true
		cache[stepID] = binding
		return binding, nil
	}
	sn := lookupSessionNameOrLegacy(store, cityName, agentCfg.QualifiedName(), cfg.Workspace.SessionTemplate)
	if sn == "" {
		return graphRouteBinding{}, fmt.Errorf("step %s: could not resolve session name for %q", stepID, agentCfg.QualifiedName())
	}
	binding.sessionName = sn
	cache[stepID] = binding
	return binding, nil
}

func graphStepRouteTarget(step *formula.RecipeStep, routeVars map[string]string) string {
	if step == nil {
		return ""
	}
	target := strings.TrimSpace(formula.Substitute(step.Assignee, routeVars))
	if target != "" {
		return target
	}
	if step.Metadata == nil {
		return ""
	}
	return strings.TrimSpace(formula.Substitute(step.Metadata["gc.run_target"], routeVars))
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

func shouldPromoteWorkflowLaunchStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "open", "ready", "todo", "triage", "backlog":
		return true
	default:
		return false
	}
}

func promoteWorkflowLaunchBead(store beads.Store, beadID string) error {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return nil
	}
	bead, err := store.Get(beadID)
	if err != nil {
		return err
	}
	if !shouldPromoteWorkflowLaunchStatus(bead.Status) {
		return nil
	}
	status := "in_progress"
	return store.Update(beadID, beads.UpdateOpts{Status: &status})
}

func startGraphWorkflow(result *molecule.Result, sourceBeadID string, a config.Agent, method string, deps slingDeps) int {
	rootID := result.RootID
	slingTracef("workflow-start begin root=%s source=%s agent=%s method=%s", rootID, sourceBeadID, a.QualifiedName(), method)
	statusStart := time.Now()
	if err := promoteWorkflowLaunchBead(deps.Store, rootID); err != nil {
		slingTracef("workflow-start root-status-error root=%s dur=%s err=%v", rootID, time.Since(statusStart), err)
		fmt.Fprintf(deps.Stderr, "gc sling: setting workflow root %s in_progress: %v\n", rootID, err) //nolint:errcheck // best-effort
		return 1
	}
	slingTracef("workflow-start root-status-done root=%s dur=%s", rootID, time.Since(statusStart))
	if sourceBeadID != "" {
		metaStart := time.Now()
		if err := deps.Store.SetMetadata(sourceBeadID, "workflow_id", rootID); err != nil {
			slingTracef("workflow-start metadata-error root=%s source=%s dur=%s err=%v", rootID, sourceBeadID, time.Since(metaStart), err)
			fmt.Fprintf(deps.Stderr, "gc sling: setting workflow_id on %s: %v\n", sourceBeadID, err) //nolint:errcheck // best-effort
			return 1
		}
		slingTracef("workflow-start metadata-done root=%s source=%s dur=%s", rootID, sourceBeadID, time.Since(metaStart))
	}
	pokeStart := time.Now()
	_ = slingPokeControlDispatcher(deps.CityPath)
	slingTracef("workflow-start poke-done root=%s dur=%s", rootID, time.Since(pokeStart))

	telemetry.RecordSling(context.Background(), a.QualifiedName(), targetType(&a), method, nil)
	slingTracef("workflow-start done root=%s", rootID)
	return 0
}

// targetType returns "pool" or "agent" for telemetry attributes.
func targetType(a *config.Agent) string {
	if isMultiSessionCfgAgent(a) {
		return "pool"
	}
	return "agent"
}

// beadCheckResult captures the outcome of a pre-flight bead state check.
type beadCheckResult struct {
	Idempotent bool     // bead already routed to the same target
	Warnings   []string // warnings about existing routing to different targets
}

// checkBeadState checks whether a bead is already routed and returns a
// structured result. Callers decide how to handle idempotency vs warnings.
// Best-effort: nil querier or query failure → empty result (proceed silently).
func checkBeadState(q BeadQuerier, beadID string, a config.Agent) beadCheckResult {
	if q == nil {
		return beadCheckResult{}
	}
	b, err := q.Get(beadID)
	if err != nil {
		return beadCheckResult{} // best-effort: can't query → skip check
	}

	// Custom sling_query: can't determine idempotency — fall through to
	// generic warnings only.
	if isCustomSlingQuery(a) {
		var warnings []string
		if b.Assignee != "" {
			warnings = append(warnings, fmt.Sprintf("warning: bead %s already assigned to %q", beadID, b.Assignee))
		}
		if routedTo := strings.TrimSpace(b.Metadata["gc.routed_to"]); routedTo != "" {
			warnings = append(warnings, fmt.Sprintf("warning: bead %s already routed to %q", beadID, routedTo))
		}
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "pool:") {
				warnings = append(warnings, fmt.Sprintf("warning: bead %s already has pool label %q", beadID, l))
			}
		}
		return beadCheckResult{Warnings: warnings}
	}

	target := a.QualifiedName()
	if strings.TrimSpace(b.Metadata["gc.routed_to"]) == target {
		// Only idempotent if the bead is unassigned or already assigned
		// consistently with the target. Otherwise the bead would be
		// invisible to the target's work_query (which requires --unassigned
		// for pool work in tier 3).
		if b.Assignee == "" || b.Assignee == target {
			return beadCheckResult{Idempotent: true}
		}
		return beadCheckResult{
			Warnings: []string{fmt.Sprintf("warning: bead %s routed to %q but assigned to %q", beadID, target, b.Assignee)},
		}
	}

	// Fixed agent: check legacy assignee routing as a compatibility fallback.
	if !isMultiSessionCfgAgent(&a) {
		if b.Assignee == target {
			return beadCheckResult{Idempotent: true}
		}
		var warnings []string
		if b.Assignee != "" {
			warnings = append(warnings, fmt.Sprintf("warning: bead %s already assigned to %q", beadID, b.Assignee))
		}
		if routedTo := strings.TrimSpace(b.Metadata["gc.routed_to"]); routedTo != "" {
			warnings = append(warnings, fmt.Sprintf("warning: bead %s already routed to %q", beadID, routedTo))
		}
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "pool:") {
				warnings = append(warnings, fmt.Sprintf("warning: bead %s already has pool label %q", beadID, l))
			}
		}
		return beadCheckResult{Warnings: warnings}
	}

	// Multi-session targets: pool labels are a legacy fallback only when
	// gc.routed_to is absent. If gc.routed_to is set (even to a different
	// target), it is authoritative — a stale pool label must not short-circuit.
	if strings.TrimSpace(b.Metadata["gc.routed_to"]) == "" {
		poolLabel := "pool:" + target
		for _, l := range b.Labels {
			if l == poolLabel {
				return beadCheckResult{Idempotent: true}
			}
		}
	}
	var warnings []string
	if b.Assignee != "" {
		warnings = append(warnings, fmt.Sprintf("warning: bead %s already assigned to %q", beadID, b.Assignee))
	}
	if routedTo := strings.TrimSpace(b.Metadata["gc.routed_to"]); routedTo != "" {
		warnings = append(warnings, fmt.Sprintf("warning: bead %s already routed to %q", beadID, routedTo))
	}
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "pool:") {
			warnings = append(warnings, fmt.Sprintf("warning: bead %s already has pool label %q", beadID, l))
		}
	}
	return beadCheckResult{Warnings: warnings}
}

// doSlingNudge sends a nudge to the target agent after routing.
// For pools, nudges the first running instance. If the target is not
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

	if isMultiSessionCfgAgent(a) {
		// Find a running multi-session instance to nudge.
		sp0 := scaleParamsFor(a)
		for _, qn := range discoverPoolInstances(a.Name, a.Dir, sp0, a, cityName, st, sp) {
			sn := lookupSessionNameOrLegacy(store, cityName, qn, st)
			if sp.IsRunning(sn) {
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
		// No running pool member — poke controller for immediate wake.
		if err := pokeController(cityPath); err != nil {
			fmt.Fprintf(stderr, "No running pool members for %q; poke failed: %v\n", a.QualifiedName(), err) //nolint:errcheck // best-effort
		} else {
			fmt.Fprintf(stdout, "No running pool members for %q — poked controller for wake\n", a.QualifiedName()) //nolint:errcheck // best-effort
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
	running := sp.IsRunning(target.sessionName)
	now := time.Now()
	if running && tryDeliverWaitIdleNudge(target, sp, msg) {
		telemetry.RecordNudge(context.Background(), target.agent.QualifiedName(), nil)
		fmt.Fprintf(stdout, "Nudged %s\n", target.agent.QualifiedName()) //nolint:errcheck // best-effort
		return
	}

	if err := enqueueQueuedNudgeWithStore(target.cityPath, store, newQueuedNudge(target.agent.QualifiedName(), msg, "sling", now)); err != nil {
		telemetry.RecordNudge(context.Background(), target.agent.QualifiedName(), err)
		fmt.Fprintf(stderr, "gc sling: nudge failed: %v\n", err) //nolint:errcheck // best-effort
		return
	}
	if running {
		maybeStartCodexNudgePoller(target)
	} else if err := pokeController(cityPath); err != nil {
		fmt.Fprintf(stderr, "Session %q is asleep; poke failed: %v\n", target.agent.QualifiedName(), err) //nolint:errcheck // best-effort
	} else {
		fmt.Fprintf(stdout, "Session %q is asleep — poked controller for wake\n", target.agent.QualifiedName()) //nolint:errcheck // best-effort
	}
	fmt.Fprintf(stdout, "Queued nudge for %s\n", target.agent.QualifiedName()) //nolint:errcheck // best-effort
}

// dryRunSingle prints a step-by-step preview of what gc sling would do for a
// single bead (or formula) without executing any side effects.
func dryRunSingle(opts slingOpts, deps slingDeps, querier BeadQuerier) int {
	a := opts.Target
	w := func(s string) { fmt.Fprintln(deps.Stdout, s) } //nolint:errcheck // best-effort

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
	printTarget(w, a)

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

		routeCmd := buildSlingCommand(a.EffectiveSlingQuery(), "<wisp-root>")
		w("Route command (not executed):")
		w("  " + routeCmd)
		w("  The wisp root bead (not the formula name) is routed to the agent.")
		w("")
	} else {
		// Work section (bead info).
		printBeadInfo(w, querier, opts.BeadOrFormula)

		// Cross-rig section — show when bead prefix doesn't match agent's rig.
		printCrossRigSection(w, opts.BeadOrFormula, a, deps.Cfg)

		// Idempotency section — show when bead is already routed to this target.
		result := checkBeadState(querier, opts.BeadOrFormula, a)
		if result.Idempotent {
			w("Idempotency:")
			w("  Bead " + opts.BeadOrFormula + " is already routed to " + a.QualifiedName() + ".")
			w("  Without --force, sling would skip routing (exit 0).")
			w("")
		}

		// Attach formula section (--on or default).
		if opts.OnFormula != "" {
			if err := checkNoMoleculeChildren(querier, opts.BeadOrFormula, deps.Store, deps.Stderr); err != nil {
				fmt.Fprintf(deps.Stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort
				return 1
			}

			w("Attach formula:")
			w("  Formula: " + opts.OnFormula)
			w("  --on attaches a wisp (structured work instructions) to an existing")
			w("  bead. The agent receives the original bead with the workflow")
			w("  attached, rather than a standalone wisp.")
			w("")
			cookCmd := fmt.Sprintf("bd mol cook --formula=%s --on=%s", opts.OnFormula, opts.BeadOrFormula)
			if opts.Title != "" {
				cookCmd += fmt.Sprintf(" --title=%s", opts.Title)
			}
			w("  Would run: " + cookCmd)
			w("  Pre-check: " + opts.BeadOrFormula + " has no existing molecule/wisp children ✓")
			w("")
		} else if !opts.NoFormula && a.EffectiveDefaultSlingFormula() != "" {
			if err := checkNoMoleculeChildren(querier, opts.BeadOrFormula, deps.Store, deps.Stderr); err != nil {
				fmt.Fprintf(deps.Stderr, "gc sling: %v\n", err) //nolint:errcheck // best-effort
				return 1
			}

			w("Default formula:")
			w("  Formula: " + a.EffectiveDefaultSlingFormula())
			w("  Target " + a.QualifiedName() + " has a default_sling_formula configured.")
			w("  A wisp will be attached automatically (use --no-formula to suppress).")
			w("")
			cookCmd := fmt.Sprintf("bd mol cook --formula=%s --on=%s", a.EffectiveDefaultSlingFormula(), opts.BeadOrFormula)
			if opts.Title != "" {
				cookCmd += fmt.Sprintf(" --title=%s", opts.Title)
			}
			w("  Would run: " + cookCmd)
			w("  Pre-check: " + opts.BeadOrFormula + " has no existing molecule/wisp children ✓")
			w("")
		}

		routeCmd := buildSlingCommand(a.EffectiveSlingQuery(), opts.BeadOrFormula)
		w("Route command (not executed):")
		w("  " + routeCmd)
		if !isCustomSlingQuery(a) {
			if isMultiSessionCfgAgent(&a) {
				w("  This labels the bead for pool \"" + a.QualifiedName() + "\".")
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
func dryRunBatch(opts slingOpts, deps slingDeps,
	b beads.Bead, children, open []beads.Bead, querier BeadQuerier,
) int {
	a := opts.Target
	w := func(s string) { fmt.Fprintln(deps.Stdout, s) } //nolint:errcheck // best-effort

	// Header.
	w("Dry run: gc sling " + a.QualifiedName() + " " + b.ID)
	w("")

	// Target section.
	printTarget(w, a)

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
		clabel := formatBeadLabel(c.ID, c.Title)
		if c.Status == "open" {
			// Check idempotency for open children.
			result := checkBeadState(querier, c.ID, a)
			if result.Idempotent {
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
		routeCmd := buildSlingCommand(a.EffectiveSlingQuery(), c.ID)
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
func printTarget(w func(string), a config.Agent) {
	w("Target:")
	if isMultiSessionCfgAgent(&a) {
		sp := scaleParamsFor(&a)
		maxDisplay := fmt.Sprintf("max=%d", sp.Max)
		if sp.Max < 0 {
			maxDisplay = "max=unlimited"
		}
		w(fmt.Sprintf("  Pool:        %s (min=%d %s)", a.QualifiedName(), sp.Min, maxDisplay))
	} else {
		w("  Agent:       " + a.QualifiedName() + " (fixed agent)")
	}
	sq := a.EffectiveSlingQuery()
	w("  Sling query: " + sq)
	if !isCustomSlingQuery(a) {
		if isMultiSessionCfgAgent(&a) {
			w("               Pool agents share a work queue via labels instead of")
			w("               direct assignment. Any idle pool member can claim work")
			w("               labeled for its pool.")
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

// printNudgePreview prints the Nudge section for dry-run output.
func printNudgePreview(w func(string), a config.Agent, cityName string,
	sp runtime.Provider, store beads.Store, cfg *config.City,
) {
	st := cfg.Workspace.SessionTemplate
	w("Nudge:")
	sn := lookupSessionNameOrLegacy(store, cityName, a.QualifiedName(), st)
	if sp.IsRunning(sn) {
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
	return a.SlingQuery != ""
}

// looksLikeBeadID reports whether s matches the bead ID pattern: an
// alphabetic-led alphanumeric prefix, a dash, and a short alphanumeric
// suffix of 1-8 chars (e.g. "BL-42", "mp-1j1", "gc-56nqn",
// "gc-r5sr6bm"). Short suffixes (1-4 chars) are accepted
// unconditionally. Longer suffixes (5-8 chars) must contain at least
// one digit to distinguish base36 hashes from English words like
// "hello-world". Strings with spaces or multiple dashes (like
// "code-review") are treated as inline text for ad-hoc bead creation.
// beadExistsInStore returns true if the given ID resolves to a bead in the store.
// Used as a fallback when looksLikeBeadID returns false for valid hierarchical
// IDs (e.g., "ProjectWrenUnity-0fze.1").
func beadExistsInStore(store beads.Store, id string) bool {
	_, err := store.Get(id)
	return err == nil
}

func looksLikeBeadID(s string) bool {
	if strings.ContainsAny(s, " \t\n") {
		return false
	}
	i := strings.Index(s, "-")
	if i <= 0 || i == len(s)-1 {
		return false
	}
	// Must have exactly one dash.
	if strings.Count(s, "-") != 1 {
		return false
	}
	prefix := s[:i]
	for idx, c := range prefix {
		if idx == 0 {
			if ('A' > c || c > 'Z') && ('a' > c || c > 'z') {
				return false
			}
			continue
		}
		if ('0' > c || c > '9') && ('a' > c || c > 'z') && ('A' > c || c > 'Z') {
			return false
		}
	}
	suffix := s[i+1:]
	// Strip hierarchical child suffix (e.g., ".1" from "0fze.1") for validation.
	// Dots delimit parent-child hierarchy in bead IDs.
	baseSuffix := suffix
	if dot := strings.IndexByte(suffix, '.'); dot > 0 {
		baseSuffix = suffix[:dot]
	}
	for _, c := range baseSuffix {
		if ('0' > c || c > '9') && ('a' > c || c > 'z') && ('A' > c || c > 'Z') {
			return false
		}
	}
	// Bead ID suffixes from bd are base36 hashes (3-8 chars) or
	// sequential integers. Short suffixes (1-4 chars) are accepted
	// unconditionally — no English words are that short after a dash.
	// Longer suffixes (5-8 chars) must contain at least one digit to
	// distinguish base36 hashes from English words like "world".
	if len(baseSuffix) > 8 {
		return false
	}
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

// beadPrefix extracts the rig prefix from a bead ID by taking the lowercase
// letters before the first dash. "HW-7" → "hw", "FE-123" → "fe".
// Returns "" if the ID has no dash (can't determine prefix).
func beadPrefix(beadID string) string {
	i := strings.Index(beadID, "-")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(beadID[:i])
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
	bp := beadPrefix(beadID)
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
