package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/spf13/cobra"
)

var dispatchControlSessionProvider = newSessionProvider

func sourceWorkflowCommandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// convoyDispatchSubcommands returns the dispatch-related subcommands to add to gc convoy.
func convoyDispatchSubcommands(stdout, stderr io.Writer) []*cobra.Command {
	return []*cobra.Command{
		newConvoyControlCmd(stdout, stderr),
		newConvoyPokeCmd(stdout, stderr),
		newConvoyDeleteCmd(stdout, stderr),
		newConvoyDeleteSourceCmd(stdout, stderr),
		newConvoyReopenSourceCmd(stdout, stderr),
	}
}

// newWorkflowCmd returns a hidden alias for backwards compatibility.
func newWorkflowCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "workflow",
		Short:  "Alias for gc convoy (deprecated)",
		Hidden: true,
	}
	cmd.AddCommand(convoyDispatchSubcommands(stdout, stderr)...)
	return cmd
}

func newConvoyControlCmd(stdout, stderr io.Writer) *cobra.Command {
	var serve bool
	var follow string
	cmd := &cobra.Command{
		Use:   "control [bead-id]",
		Short: "Execute control beads or run the control-dispatcher loop",
		Long: `Process a single control bead, or run the control-dispatcher loop
with --serve to continuously process ready control beads.
Use --follow <agent> to filter the serve loop to a specific agent template.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if serve || follow != "" {
				if follow != "" {
					args = append(args, follow)
				}
				return runConvoyControlServe(args, stdout, stderr)
			}
			if len(args) == 0 {
				return fmt.Errorf("bead-id is required (or use --serve)")
			}
			if err := runControlDispatcher(args[0], stdout, stderr); err != nil {
				if errors.Is(err, dispatch.ErrControlPending) {
					return nil
				}
				_, _ = fmt.Fprintf(stderr, "gc convoy control: %v\n", err)
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&serve, "serve", false, "Run the control-dispatcher loop (continuous)")
	cmd.Flags().StringVar(&follow, "follow", "", "Run serve loop filtered to a specific agent template")
	return cmd
}

func newConvoyPokeCmd(_ io.Writer, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "poke",
		Short:  "Trigger immediate control dispatch reconciliation",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gc convoy poke: %v\n", err)
				return errExit
			}
			if err := pokeControlDispatch(cityPath); err != nil {
				_, _ = fmt.Fprintf(stderr, "gc convoy poke: %v\n", err)
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func pokeControlDispatch(cityPath string) error {
	if _, err := sendControllerCommand(cityPath, "control-dispatcher"); err == nil {
		return nil
	}
	return pokeController(cityPath)
}

func runControlDispatcher(beadID string, stdout, stderr io.Writer) error {
	cityPath, err := resolveCity()
	if err != nil {
		return err
	}

	// Manual control dispatch keeps the operator convenience of resolving a
	// bead ID across city and rig stores.
	store, bead, storePath, err := findBeadAcrossStores(cityPath, beadID, stderr)
	if err != nil {
		return fmt.Errorf("loading bead %s: %w", beadID, err)
	}

	return runControlDispatcherWithStore(cityPath, storePath, store, bead, beadID, stdout, stderr)
}

func runControlDispatcherInStore(cityPath, storePath, beadID string, stdout, stderr io.Writer) error {
	if cityPath == "" {
		var err error
		cityPath, err = resolveCity()
		if err != nil {
			return err
		}
	}
	if storePath == "" {
		storePath = cityPath
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return err
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	store, err := openControlStoreAtForCity(storePath, cityPath, cfg)
	if err != nil {
		return fmt.Errorf("opening scoped control store %q: %w", storePath, err)
	}
	bead, err := store.Get(beadID)
	if err != nil {
		return fmt.Errorf("loading bead %s from scoped control store %q: %w", beadID, storePath, err)
	}

	return runControlDispatcherWithStoreAndConfig(cityPath, storePath, store, bead, beadID, cfg, stdout, stderr)
}

func runControlDispatcherWithStore(cityPath, storePath string, store beads.Store, bead beads.Bead, beadID string, stdout, stderr io.Writer) error {
	return runControlDispatcherWithStoreAndConfig(cityPath, storePath, store, bead, beadID, nil, stdout, stderr)
}

func runControlDispatcherWithStoreAndConfig(cityPath, storePath string, store beads.Store, bead beads.Bead, beadID string, cfg *config.City, stdout, stderr io.Writer) error {
	restoreTraceWarnings := useWorkflowTraceWarnings(stderr)
	defer restoreTraceWarnings()
	var cfgLoadErr error
	if cfg == nil {
		cfg, cfgLoadErr = loadCityConfig(cityPath, stderr)
		if cfg != nil {
			resolveRigPaths(cityPath, cfg.Rigs)
		}
	}
	if cfg != nil {
		warnLegacyWorkflowTracePath(cityPath, cfg.Rigs, stderr)
	} else {
		warnLegacyWorkflowTracePath(cityPath, nil, stderr)
	}

	opts := dispatch.ProcessOptions{CityPath: cityPath, StorePath: storePath}
	opts.Tracef = workflowTracef
	loadCfg := false
	switch bead.Metadata["gc.kind"] {
	case "check", "fanout", "retry-eval", "retry", "ralph":
		loadCfg = true
	case "workflow-finalize":
		// Need cfg to resolve "city:<name>" / "rig:<name>" store refs when
		// closing parent source beads in their native stores.
		loadCfg = true
	}
	if loadCfg {
		if cfg == nil {
			if cfgLoadErr != nil {
				return cfgLoadErr
			}
			return fmt.Errorf("loading city config for %s: unavailable after warning-only load", cityPath)
		}
		opts.ResolveStoreRef = makeStoreRefResolver(cityPath, cfg)
		if bead.Metadata["gc.kind"] == "workflow-finalize" {
			sourceWorkflowCtx, cancelSourceWorkflowCtx := sourceWorkflowCommandContext()
			defer cancelSourceWorkflowCtx()
			opts.SourceWorkflowLock = makeSourceWorkflowLocker(sourceWorkflowCtx, cityPath, cfg, storePath)
			opts.SourceWorkflowStores = makeSourceWorkflowStoresLister(cityPath, cfg)
		}
		switch bead.Metadata["gc.kind"] {
		case "check", "fanout":
			opts.FormulaSearchPaths = workflowFormulaSearchPaths(cfg, bead)
			opts.PrepareFragment = func(fragment *formula.FragmentRecipe, source beads.Bead) error {
				return decorateDynamicFragmentRecipe(fragment, source, store, loadedCityName(cfg, cityPath), cityPath, cfg)
			}
		case "retry-eval":
			sp := dispatchControlSessionProvider()
			opts.RecycleSession = func(subject beads.Bead) error {
				if strings.TrimSpace(subject.Assignee) == "" {
					return fmt.Errorf("subject %s missing assignee for pooled retry recycle", subject.ID)
				}
				return workerKillSessionTargetWithConfig("", store, sp, cfg, subject.Assignee)
			}
		case "retry", "ralph":
			opts.FormulaSearchPaths = workflowFormulaSearchPaths(cfg, bead)
			sp := dispatchControlSessionProvider()
			opts.RecycleSession = func(subject beads.Bead) error {
				if strings.TrimSpace(subject.Assignee) == "" {
					return fmt.Errorf("subject %s missing assignee for pooled retry recycle", subject.ID)
				}
				return workerKillSessionTargetWithConfig("", store, sp, cfg, subject.Assignee)
			}
		}
	}

	result, err := dispatch.ProcessControl(store, bead, opts)
	if err != nil {
		return err
	}
	if result.Processed {
		_, _ = fmt.Fprintf(stdout, "control dispatch: bead=%s action=%s", beadID, result.Action)
		if result.Created > 0 {
			_, _ = fmt.Fprintf(stdout, " created=%d", result.Created)
		}
		if result.Skipped > 0 {
			_, _ = fmt.Fprintf(stdout, " skipped=%d", result.Skipped)
		}
		fmt.Fprintln(stdout) //nolint:errcheck
	}
	return nil
}

// makeStoreRefResolver returns a dispatch.ProcessOptions.ResolveStoreRef
// closure for the given city. The resolver maps "city:<name>" and
// "rig:<name>" gc.source_store_ref values to a beads.Store rooted at the
// matching scope. processWorkflowFinalize uses it to walk the source bead
// chain across store boundaries so a successful rig-scope workflow closes
// the city-scope source bead that spawned it (e.g. PR-review "Adopt PR"
// requests).
func makeStoreRefResolver(cityPath string, cfg *config.City) func(string) (beads.Store, error) {
	cityName := loadedCityName(cfg, cityPath)
	return func(ref string) (beads.Store, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return nil, fmt.Errorf("empty store ref")
		}
		switch {
		case strings.HasPrefix(ref, "city:"):
			name := strings.TrimSpace(strings.TrimPrefix(ref, "city:"))
			// "city:" without a name still resolves to this city's store -
			// older callers stamp ambiguous refs and the only reachable city
			// from a control-dispatcher is the one it was launched in.
			if name != "" && cityName != "" && name != cityName {
				return nil, fmt.Errorf("city ref %q does not match this city %q", ref, cityName)
			}
			return openStoreAtForCity(cityPath, cityPath)
		case strings.HasPrefix(ref, "rig:"):
			name := strings.TrimSpace(strings.TrimPrefix(ref, "rig:"))
			if name == "" {
				return nil, fmt.Errorf("rig ref %q missing rig name", ref)
			}
			if cfg == nil {
				return nil, fmt.Errorf("no city config available to resolve %q", ref)
			}
			for _, rig := range cfg.Rigs {
				if rig.Name != name {
					continue
				}
				return openControlStoreAtForCity(rig.Path, cityPath, cfg)
			}
			return nil, fmt.Errorf("rig %q not found in city config", name)
		default:
			return nil, fmt.Errorf("unsupported store ref scheme: %q", ref)
		}
	}
}

func makeSourceWorkflowLocker(ctx context.Context, cityPath string, cfg *config.City, defaultStorePath string) func(storeRef, sourceBeadID string, fn func() error) error {
	return func(storeRef, sourceBeadID string, fn func() error) error {
		return sourceworkflow.WithLock(ctx, cityPath, sourceWorkflowLockScopeForStoreRef(cityPath, cfg, defaultStorePath, storeRef), sourceBeadID, fn)
	}
}

func makeSourceWorkflowStoresLister(cityPath string, cfg *config.City) func() ([]dispatch.SourceWorkflowStore, error) {
	return makeSourceWorkflowStoresListerWithOpenStore(cityPath, cfg, func(dir string) (beads.Store, error) {
		return openStoreAtForCity(dir, cityPath)
	})
}

func makeSourceWorkflowStoresListerWithOpenStore(cityPath string, cfg *config.City, openStore func(string) (beads.Store, error)) func() ([]dispatch.SourceWorkflowStore, error) {
	var (
		loaded  bool
		stores  []dispatch.SourceWorkflowStore
		loadErr error
	)
	return func() ([]dispatch.SourceWorkflowStore, error) {
		if loaded {
			return stores, loadErr
		}
		loaded = true
		views, skips, err := openSourceWorkflowStoresWith(cfg, cityPath, "", openStore)
		if err != nil {
			loadErr = err
			return nil, err
		}
		if len(skips) > 0 {
			msg := formatSourceWorkflowStoreSkips(skips)
			workflowTracef("source-workflow stores warning=%q", msg)
			loadErr = errors.New(msg)
			return nil, loadErr
		}
		cityName := loadedCityName(cfg, cityPath)
		stores = make([]dispatch.SourceWorkflowStore, 0, len(views))
		for _, view := range views {
			stores = append(stores, dispatch.SourceWorkflowStore{
				Store:    view.store,
				StoreRef: workflowStoreRefForDir(view.path, cityPath, cityName, cfg),
			})
		}
		return stores, nil
	}
}

func sourceWorkflowLockScopeForStoreRef(cityPath string, cfg *config.City, defaultStorePath string, storeRef string) string {
	return sourceworkflow.LockScopeForStoreRef(cityPath, defaultStorePath, storeRef, func(rigName string) (string, bool) {
		if cfg != nil {
			for _, rig := range cfg.Rigs {
				if rig.Name != rigName {
					continue
				}
				return rig.Path, true
			}
		}
		return "", false
	})
}

func openControlStoreAtForCity(storePath, cityPath string, cfg *config.City) (beads.Store, error) {
	scopeRoot := resolveStoreScopeRoot(cityPath, storePath)
	provider := rawBeadsProviderForScope(scopeRoot, cityPath)
	if provider == "file" || strings.HasPrefix(provider, "exec:") {
		return openStoreAtForCity(storePath, cityPath)
	}
	if samePath(scopeRoot, cityPath) {
		return controlBdStoreForCity(scopeRoot, cityPath, cfg), nil
	}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			rigPath := rig.Path
			if !filepath.IsAbs(rigPath) {
				rigPath = filepath.Join(cityPath, rigPath)
			}
			if samePath(rigPath, scopeRoot) {
				return controlBdStoreForRig(scopeRoot, cityPath, cfg), nil
			}
		}
	}
	// A bd-backed scope can outlive its rig entry in city.toml. Control paths
	// still need write-capable bd commands with auto-export suppressed.
	return controlBdStoreForRig(scopeRoot, cityPath, cfg), nil
}

// findBeadAcrossStores tries the city store first, then all rig stores,
// returning the store and bead on first match.
func findBeadAcrossStores(cityPath, beadID string, warningWriter io.Writer) (beads.Store, beads.Bead, string, error) {
	// Try city store first.
	cityStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, beads.Bead{}, "", fmt.Errorf("opening city store: %w", err)
	}
	if b, err := cityStore.Get(beadID); err == nil {
		return cityStore, b, cityPath, nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return nil, beads.Bead{}, "", fmt.Errorf("getting bead %q from %s: %w", beadID, cityPath, err)
	}

	// Try rig stores.
	cfg, err := loadCityConfig(cityPath, warningWriter)
	if err != nil {
		return nil, beads.Bead{}, "", fmt.Errorf("getting bead %q: not in city store, and config unavailable: %w", beadID, err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		store, err := openControlStoreAtForCity(rig.Path, cityPath, cfg)
		if err != nil {
			return nil, beads.Bead{}, "", fmt.Errorf("opening rig store %q: %w", rig.Name, err)
		}
		bead, err := store.Get(beadID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, beads.Bead{}, "", fmt.Errorf("getting bead %q from %s: %w", beadID, rig.Path, err)
		}
		return store, bead, rig.Path, nil
	}
	return nil, beads.Bead{}, "", fmt.Errorf("getting bead %q: %w", beadID, beads.ErrNotFound)
}

func findUniqueBeadAcrossStoresView(cityPath, beadID string) (convoyStoreView, beads.Bead, error) {
	cfg, err := loadCityConfig(cityPath, os.Stderr)
	if err != nil {
		return convoyStoreView{}, beads.Bead{}, fmt.Errorf("loading city config for bead %q: %w", beadID, err)
	}
	stores, skips, err := openSourceWorkflowStores(cfg, cityPath, beadID)
	if err != nil {
		return convoyStoreView{}, beads.Bead{}, err
	}
	if len(skips) > 0 {
		// Surface skipped stores so a not-found isn't silently masking a
		// store we couldn't open.
		fmt.Fprintln(os.Stderr, "warning:", formatSourceWorkflowStoreSkips(skips)) //nolint:errcheck
	}
	var (
		foundView convoyStoreView
		foundBead beads.Bead
		found     bool
	)
	for _, candidate := range stores {
		bead, err := candidate.store.Get(beadID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return convoyStoreView{}, beads.Bead{}, fmt.Errorf("getting bead %q from %s: %w", beadID, candidate.path, err)
		}
		if found {
			return convoyStoreView{}, beads.Bead{}, fmt.Errorf(
				"source bead %s exists in multiple stores (%s and %s); source workflow commands require a uniquely resolvable source bead id",
				beadID,
				foundView.path,
				candidate.path,
			)
		}
		foundView = candidate
		foundBead = bead
		found = true
	}
	if !found {
		return convoyStoreView{}, beads.Bead{}, fmt.Errorf("getting bead %q: %w", beadID, beads.ErrNotFound)
	}
	return foundView, foundBead, nil
}

func workflowFormulaSearchPaths(cfg *config.City, bead beads.Bead) []string {
	if cfg == nil {
		return nil
	}
	routedTo := workflowExecutionRoute(bead)
	if routedTo == "" {
		return cfg.FormulaLayers.City
	}
	rigName, _ := config.ParseQualifiedName(routedTo)
	if paths := cfg.FormulaLayers.SearchPaths(rigName); len(paths) > 0 {
		return paths
	}
	return cfg.FormulaLayers.City
}

func decorateDynamicFragmentRecipe(fragment *formula.FragmentRecipe, source beads.Bead, store beads.Store, cityName, cityPath string, cfg *config.City) error {
	if fragment == nil {
		return fmt.Errorf("fragment recipe is nil")
	}
	defaultRoute, err := graphFallbackBindingForBead(source, store, cityName, cfg)
	if err != nil {
		return err
	}
	routingRigContext := graphRouteRigContext(defaultRoute.QualifiedName)
	controlRoute, err := controlDispatcherBinding(store, cityName, cfg, routingRigContext)
	if err != nil {
		return err
	}

	for i := range fragment.Steps {
		step := &fragment.Steps[i]
		if step.Metadata == nil {
			step.Metadata = make(map[string]string)
		} else {
			step.Metadata = maps.Clone(step.Metadata)
		}
		step.Metadata["gc.dynamic_fragment"] = "true"
		propagateDynamicScopeMetadata(step, source)
	}
	formula.ApplyFragmentRecipeGraphControls(fragment)

	stepByID := make(map[string]*formula.RecipeStep, len(fragment.Steps))
	stepAlias := make(map[string]string, len(fragment.Steps))
	for i := range fragment.Steps {
		stepByID[fragment.Steps[i].ID] = &fragment.Steps[i]
		if short, ok := strings.CutPrefix(fragment.Steps[i].ID, fragment.Name+"."); ok {
			stepAlias[short] = fragment.Steps[i].ID
		}
	}
	depsByStep := make(map[string][]string, len(fragment.Deps))
	for _, dep := range fragment.Deps {
		if dep.Type != "blocks" && dep.Type != "waits-for" && dep.Type != "conditional-blocks" {
			continue
		}
		depsByStep[dep.StepID] = append(depsByStep[dep.StepID], dep.DependsOnID)
	}
	bindingCache := make(map[string]graphRouteBinding, len(fragment.Steps))
	resolving := make(map[string]bool, len(fragment.Steps))
	for i := range fragment.Steps {
		step := &fragment.Steps[i]
		switch step.Metadata["gc.kind"] {
		case "workflow", "scope", "ralph", "retry", "spec":
			continue
		}
		binding, err := resolveGraphStepBinding(step.ID, stepByID, stepAlias, depsByStep, bindingCache, resolving, defaultRoute, routingRigContext, store, cityName, cityPath, cfg)
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

func graphFallbackBindingForBead(source beads.Bead, store beads.Store, cityName string, cfg *config.City) (graphRouteBinding, error) {
	routedTo := workflowExecutionRoute(source)
	if routedTo == "" {
		return graphRouteBinding{SessionName: source.Assignee}, nil
	}
	if cfg == nil {
		return graphRouteBinding{}, fmt.Errorf("graph.v2 routing for %s requires config", source.ID)
	}

	agentCfg, ok := resolveAgentIdentity(cfg, routedTo, graphRouteRigContext(routedTo))
	if !ok {
		return graphRouteBinding{}, fmt.Errorf("unknown graph.v2 fallback target %q on %s", routedTo, source.ID)
	}

	binding := graphRouteBinding{QualifiedName: agentCfg.QualifiedName()}
	if agentCfg.SupportsInstanceExpansion() {
		binding.MetadataOnly = true
		return binding, nil
	}
	if source.Assignee != "" {
		binding.SessionName = source.Assignee
		return binding, nil
	}
	sn := lookupSessionNameOrLegacy(store, cityName, agentCfg.QualifiedName(), cfg.Workspace.SessionTemplate)
	if sn == "" {
		return graphRouteBinding{}, fmt.Errorf("could not resolve session name for %q", agentCfg.QualifiedName())
	}
	binding.SessionName = sn
	return binding, nil
}

func propagateDynamicScopeMetadata(step *formula.RecipeStep, source beads.Bead) {
	if step == nil {
		return
	}
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	if scopeRef := strings.TrimSpace(source.Metadata["gc.scope_ref"]); scopeRef != "" && step.Metadata["gc.scope_ref"] == "" {
		step.Metadata["gc.scope_ref"] = scopeRef
	}
	if onFail := strings.TrimSpace(source.Metadata["gc.on_fail"]); onFail != "" && step.Metadata["gc.on_fail"] == "" {
		step.Metadata["gc.on_fail"] = onFail
	}
	for _, key := range []string{"gc.step_id", "gc.ralph_step_id", "gc.attempt"} {
		if value := strings.TrimSpace(source.Metadata[key]); value != "" && step.Metadata[key] == "" {
			step.Metadata[key] = value
		}
	}
	if step.Metadata["gc.scope_ref"] == "" || step.Metadata["gc.scope_role"] != "" {
		return
	}
	switch step.Metadata["gc.kind"] {
	case "scope":
		return
	case "scope-check", "workflow-finalize", "fanout", "check", "retry-eval", "retry", "ralph":
		step.Metadata["gc.scope_role"] = "control"
		return
	default:
		step.Metadata["gc.scope_role"] = "member"
	}
}

func newConvoyDeleteCmd(stdout, stderr io.Writer) *cobra.Command {
	var force bool
	var deleteBeads bool
	cmd := &cobra.Command{
		Use:   "delete <convoy-id>",
		Short: "Close or delete a convoy and all its beads",
		Long: `Close all open beads in a convoy, or delete them.

Searches all stores (city + rigs) for the convoy root and all beads
with matching gc.root_bead_id. Without --force, shows a preview.

By default, beads are closed with gc.outcome=skipped. Use --delete to
remove them from the store via bd delete --cascade --force.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdWorkflowDelete(args[0], force, deleteBeads, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Actually close/delete (without this, shows preview)")
	cmd.Flags().BoolVar(&deleteBeads, "delete", false, "Delete beads from the store instead of closing")
	return cmd
}

func newConvoyDeleteSourceCmd(stdout, stderr io.Writer) *cobra.Command {
	var apply bool
	var deleteBeads bool
	var rigName string
	var storeRef string
	cmd := &cobra.Command{
		Use:   "delete-source <source-bead-id>",
		Short: "Close workflows sourced from a bead",
		Long: `Find every live workflow root sourced from the given bead and close
its subtree. By default this is a preview. Use --apply to mutate.
Use --delete with --apply to also delete closed beads.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if deleteBeads && !apply {
				fmt.Fprintln(stderr, "gc workflow delete-source: --delete requires --apply") //nolint:errcheck
				return errExit
			}
			selector, err := parseSourceWorkflowStoreSelector(rigName, storeRef)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", err)
				return errExit
			}
			return exitForCode(cmdWorkflowDeleteSource(args[0], selector, apply, deleteBeads, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Actually close/delete matched workflows")
	cmd.Flags().BoolVar(&deleteBeads, "delete", false, "Also delete beads from the store after closing")
	cmd.Flags().StringVar(&rigName, "rig", "", "Select the rig store for the source bead")
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "Select the source bead store (city:<name> or rig:<name>)")
	return cmd
}

func newConvoyReopenSourceCmd(stdout, stderr io.Writer) *cobra.Command {
	var rigName string
	var storeRef string
	cmd := &cobra.Command{
		Use:   "reopen-source <source-bead-id>",
		Short: "Reopen a source bead after workflow cleanup",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			selector, err := parseSourceWorkflowStoreSelector(rigName, storeRef)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", err)
				return errExit
			}
			return exitForCode(cmdWorkflowReopenSource(args[0], selector, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&rigName, "rig", "", "Select the rig store for the source bead")
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "Select the source bead store (city:<name> or rig:<name>)")
	return cmd
}

type workflowStoreMatch struct {
	store  beads.Store
	beads  []beads.Bead
	label  string
	path   string
	runner beads.CommandRunner
}

func cmdWorkflowDelete(workflowID string, force, deleteBeads bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc workflow delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	resolveRigPaths(cityPath, cfg.Rigs)

	var matches []workflowStoreMatch

	stores, err := openConvoyStores(cfg, cityPath, workflowID, func(dir string) (beads.Store, error) {
		return openControlStoreAtForCity(dir, cityPath, cfg)
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc workflow delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, info := range stores {
		found := findWorkflowBeads(info.store, workflowID)
		if len(found) == 0 {
			continue
		}
		matches = append(matches, workflowStoreMatch{
			store:  info.store,
			beads:  found,
			label:  workflowDeleteStoreLabel(cfg, cityPath, info.path),
			path:   info.path,
			runner: workflowDeleteRunnerForPath(cfg, cityPath, info.path),
		})
	}

	total := 0
	openCount := 0
	for _, m := range matches {
		total += len(m.beads)
		for _, b := range m.beads {
			if b.Status != "closed" {
				openCount++
			}
		}
	}
	if total == 0 {
		fmt.Fprintf(stderr, "gc workflow delete: no beads found for workflow %s\n", workflowID) //nolint:errcheck // best-effort stderr
		return 1
	}

	action := "close"
	if deleteBeads {
		action = "delete"
	}
	fmt.Fprintf(stdout, "Workflow %s: %d beads (%d open) — %s\n", workflowID, total, openCount, action) //nolint:errcheck // best-effort stdout
	for _, m := range matches {
		fmt.Fprintf(stdout, "  %s: %d beads\n", m.label, len(m.beads)) //nolint:errcheck // best-effort stdout
	}

	if !force {
		fmt.Fprintln(stdout, "\nDry run. Use --force to proceed.") //nolint:errcheck // best-effort stdout
		return 0
	}

	if deleteBeads {
		deleted, err := deleteWorkflowMatches(matches)
		if err != nil {
			fmt.Fprintf(stderr, "  batch delete: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Deleted %d beads\n", deleted) //nolint:errcheck // best-effort stdout
		return 0
	}

	closed := closeWorkflowMatches(matches)
	fmt.Fprintf(stdout, "Closed %d open beads\n", closed) //nolint:errcheck // best-effort stdout
	return 0
}

func closeWorkflowMatches(matches []workflowStoreMatch) int {
	closed := 0
	for _, m := range matches {
		ids := workflowBeadIDs(m.beads)
		n, _ := m.store.CloseAll(ids, map[string]string{"gc.outcome": "skipped"})
		closed += n
	}
	return closed
}

func workflowDeleteRunnerForPath(cfg *config.City, cityPath, scopePath string) beads.CommandRunner {
	if samePath(scopePath, cityPath) {
		return bdCommandRunnerForCity(cityPath)
	}
	return bdCommandRunnerForRig(cityPath, cfg, scopePath)
}

func deleteWorkflowMatches(matches []workflowStoreMatch) (int, error) {
	deleted := 0
	for _, m := range matches {
		if m.runner == nil {
			return deleted, fmt.Errorf("%s: delete runner missing", m.label)
		}
		ids := workflowBeadIDs(m.beads)
		args := append([]string{"delete"}, ids...)
		args = append(args, "--cascade", "--force")
		if _, err := m.runner(m.path, "bd", args...); err != nil {
			return deleted, fmt.Errorf("%s: %w", m.label, err)
		}
		deleted += len(ids)
	}
	return deleted, nil
}

type sourceWorkflowStoreMatch struct {
	label  string
	store  beads.Store
	roots  []beads.Bead
	beads  []beads.Bead
	path   string
	runner beads.CommandRunner
}

type sourceWorkflowStoreSelector struct {
	storeRef string
}

type resolvedSourceWorkflowTarget struct {
	sourceBeadID string
	storeRef     string
	storeView    convoyStoreView
	sourceBead   beads.Bead
}

func parseSourceWorkflowStoreSelector(rigName, storeRef string) (sourceWorkflowStoreSelector, error) {
	rigName = strings.TrimSpace(rigName)
	storeRef = strings.TrimSpace(storeRef)
	if rigName != "" && storeRef != "" {
		return sourceWorkflowStoreSelector{}, fmt.Errorf("--rig and --store-ref are mutually exclusive")
	}
	if rigName != "" {
		storeRef = "rig:" + rigName
	}
	return sourceWorkflowStoreSelector{storeRef: storeRef}, nil
}

func resolveSourceWorkflowTarget(cfg *config.City, cityPath, sourceBeadID string, selector sourceWorkflowStoreSelector, requireSource bool) (resolvedSourceWorkflowTarget, error) {
	sourceBeadID = sourceworkflow.NormalizeSourceBeadID(sourceBeadID)
	target := resolvedSourceWorkflowTarget{sourceBeadID: sourceBeadID}
	if selector.storeRef != "" {
		view, resolvedStoreRef, err := openSourceWorkflowStoreRef(cfg, cityPath, selector.storeRef)
		if err != nil {
			return resolvedSourceWorkflowTarget{}, err
		}
		target.storeRef = resolvedStoreRef
		target.storeView = view
		bead, err := view.store.Get(sourceBeadID)
		switch {
		case err == nil:
			target.sourceBead = bead
		case errors.Is(err, beads.ErrNotFound):
			if requireSource {
				return resolvedSourceWorkflowTarget{}, fmt.Errorf("getting bead %q: %w", sourceBeadID, beads.ErrNotFound)
			}
		default:
			return resolvedSourceWorkflowTarget{}, fmt.Errorf("getting bead %q from %s: %w", sourceBeadID, workflowDeleteStoreLabel(cfg, cityPath, view.path), err)
		}
		return target, nil
	}
	view, bead, err := findUniqueBeadAcrossStoresView(cityPath, sourceBeadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) && !requireSource {
			return target, nil
		}
		return resolvedSourceWorkflowTarget{}, sourceWorkflowSelectionError(err, sourceBeadID)
	}
	target.storeView = view
	target.sourceBead = bead
	target.storeRef = workflowStoreRefForDir(view.path, cityPath, loadedCityName(cfg, cityPath), cfg)
	return target, nil
}

func sourceWorkflowSelectionError(err error, sourceBeadID string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "exists in multiple stores") {
		return fmt.Errorf("%w; rerun with --rig <name> or --store-ref <city:name|rig:name>", err)
	}
	if errors.Is(err, beads.ErrNotFound) {
		return fmt.Errorf("getting bead %q: %w", sourceBeadID, beads.ErrNotFound)
	}
	return err
}

func openSourceWorkflowStoreRef(cfg *config.City, cityPath, storeRef string) (convoyStoreView, string, error) {
	storeRef = strings.TrimSpace(storeRef)
	switch {
	case storeRef == "", storeRef == "city":
		store, err := openStoreAtForCity(cityPath, cityPath)
		if err != nil {
			return convoyStoreView{}, "", fmt.Errorf("opening city store: %w", err)
		}
		cityName := loadedCityName(cfg, cityPath)
		if cityName == "" {
			cityName = "city"
		}
		return convoyStoreView{path: cityPath, store: store}, "city:" + cityName, nil
	case strings.HasPrefix(storeRef, "city:"):
		store, err := openStoreAtForCity(cityPath, cityPath)
		if err != nil {
			return convoyStoreView{}, "", fmt.Errorf("opening city store: %w", err)
		}
		return convoyStoreView{path: cityPath, store: store}, storeRef, nil
	case strings.HasPrefix(storeRef, "rig:"):
		rigName := strings.TrimPrefix(storeRef, "rig:")
		for _, rig := range cfg.Rigs {
			if rig.Name != rigName {
				continue
			}
			rigPath := resolveStoreScopeRoot(cityPath, rig.Path)
			store, err := openStoreAtForCity(rigPath, cityPath)
			if err != nil {
				return convoyStoreView{}, "", fmt.Errorf("opening rig store %s: %w", rigName, err)
			}
			return convoyStoreView{path: rigPath, store: store}, "rig:" + rigName, nil
		}
		return convoyStoreView{}, "", fmt.Errorf("rig %q not found", rigName)
	default:
		return convoyStoreView{}, "", fmt.Errorf("invalid --store-ref %q (want city:<name> or rig:<name>)", storeRef)
	}
}

func applySourceWorkflowMatchCleanup(match sourceWorkflowStoreMatch, deleteBeads bool, stderr io.Writer) (closed, deleted int, incomplete bool) {
	ids := workflowBeadIDs(match.beads)
	n, closeErr := match.store.CloseAll(ids, map[string]string{"gc.outcome": "skipped"})
	closed += n
	if closeErr != nil {
		incomplete = true
		_, _ = fmt.Fprintf(stderr, "store=%s close_error=%v\n", match.label, closeErr)
		return closed, deleted, incomplete
	}
	if !deleteBeads {
		return closed, deleted, incomplete
	}
	count, errs := deleteSourceWorkflowMatchBeads(match, ids)
	deleted += count
	for _, deleteErr := range errs {
		incomplete = true
		_, _ = fmt.Fprintf(stderr, "store=%s delete_error=%v\n", match.label, deleteErr)
	}
	return closed, deleted, incomplete
}

func deleteSourceWorkflowMatchBeads(match sourceWorkflowStoreMatch, ids []string) (int, []error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return deleteWorkflowBeads(match.store, ids)
}

func cmdWorkflowDeleteSource(sourceBeadID string, selector sourceWorkflowStoreSelector, apply, deleteBeads bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", err)
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", err)
		return 1
	}

	var (
		resultCode int
		runErr     error
	)
	target, err := resolveSourceWorkflowTarget(cfg, cityPath, sourceBeadID, selector, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", err)
		return 1
	}
	lockScope := target.storeView.path
	if strings.TrimSpace(lockScope) == "" {
		lockScope = cityPath
	}
	ctx, cancel := sourceWorkflowCommandContext()
	defer cancel()
	runErr = sourceworkflow.WithLock(ctx, cityPath, lockScope, sourceBeadID, func() error {
		target, err := resolveSourceWorkflowTarget(cfg, cityPath, sourceBeadID, selector, false)
		if err != nil {
			return err
		}
		matches, skips, err := collectSourceWorkflowMatches(cfg, cityPath, sourceBeadID, target.storeRef)
		if err != nil {
			return err
		}
		if len(skips) > 0 {
			// delete-source cannot close live roots it can't see. Warn
			// rather than silently declaring success.
			fmt.Fprintln(stderr, "warning:", formatSourceWorkflowStoreSkips(skips)) //nolint:errcheck
		}
		if target.storeRef == "" && len(matches) > 1 {
			return fmt.Errorf(
				"source workflow %s has live roots in multiple stores (%s); rerun with --rig <name> or --store-ref <city:name|rig:name>",
				sourceBeadID,
				strings.Join(sourceWorkflowMatchLabels(matches), ", "),
			)
		}
		totalRoots, totalBeads, openCount := summarizeSourceWorkflowMatches(matches)
		if totalRoots == 0 {
			cleared := false
			if apply {
				var clearErr error
				cleared, clearErr = clearSourceWorkflowMetadata(cfg, cityPath, target)
				if clearErr != nil {
					return clearErr
				}
			}
			_, _ = fmt.Fprintf(
				stdout,
				"result=already_clean source_bead_id=%s matched_roots=0 matched_beads=0 closed=0 deleted=0 metadata_cleared=%t\n",
				sourceBeadID,
				cleared,
			)
			resultCode = 0
			return nil
		}
		if !apply {
			_, _ = fmt.Fprintf(
				stdout,
				"result=preview source_bead_id=%s matched_roots=%d matched_beads=%d open_beads=%d\n",
				sourceBeadID,
				totalRoots,
				totalBeads,
				openCount,
			)
			for _, match := range matches {
				_, _ = fmt.Fprintf(stdout, "store=%s roots=%s beads=%d\n", match.label, strings.Join(rootIDs(match.roots), ","), len(match.beads))
			}
			resultCode = 0
			return nil
		}

		closed := 0
		deleted := 0
		incomplete := false
		for _, match := range matches {
			matchClosed, matchDeleted, matchIncomplete := applySourceWorkflowMatchCleanup(match, deleteBeads, stderr)
			closed += matchClosed
			deleted += matchDeleted
			if matchIncomplete {
				incomplete = true
			}
		}

		stillOpen, verifyErr := countOpenMatchedBeads(matches)
		if verifyErr != nil {
			return verifyErr
		}
		if stillOpen > 0 {
			incomplete = true
		}
		cleared := false
		if !incomplete {
			var clearErr error
			cleared, clearErr = clearSourceWorkflowMetadata(cfg, cityPath, target)
			if clearErr != nil {
				return clearErr
			}
		}
		if incomplete {
			_, _ = fmt.Fprintf(
				stdout,
				"result=incomplete source_bead_id=%s matched_roots=%d matched_beads=%d closed=%d deleted=%d metadata_cleared=false still_open=%d\n",
				sourceBeadID,
				totalRoots,
				totalBeads,
				closed,
				deleted,
				stillOpen,
			)
			resultCode = 1
			return nil
		}
		_, _ = fmt.Fprintf(
			stdout,
			"result=cleaned source_bead_id=%s matched_roots=%d matched_beads=%d closed=%d deleted=%d metadata_cleared=%t\n",
			sourceBeadID,
			totalRoots,
			totalBeads,
			closed,
			deleted,
			cleared,
		)
		resultCode = 0
		return nil
	})
	if runErr != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow delete-source: %v\n", runErr)
		return 1
	}
	return resultCode
}

func cmdWorkflowReopenSource(sourceBeadID string, selector sourceWorkflowStoreSelector, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", err)
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", err)
		return 1
	}

	resultCode := 0
	target, err := resolveSourceWorkflowTarget(cfg, cityPath, sourceBeadID, selector, true)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", err)
		return 1
	}
	if target.storeView.store == nil || strings.TrimSpace(target.sourceBead.ID) == "" {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: getting bead %q: %v\n", sourceBeadID, beads.ErrNotFound)
		return 1
	}
	ctx, cancel := sourceWorkflowCommandContext()
	defer cancel()
	runErr := sourceworkflow.WithLock(ctx, cityPath, target.storeView.path, sourceBeadID, func() error {
		target, err := resolveSourceWorkflowTarget(cfg, cityPath, sourceBeadID, selector, true)
		if err != nil {
			return err
		}
		if target.storeView.store == nil || strings.TrimSpace(target.sourceBead.ID) == "" {
			return fmt.Errorf("getting bead %q: %w", sourceBeadID, beads.ErrNotFound)
		}
		matches, skips, err := collectSourceWorkflowMatches(cfg, cityPath, sourceBeadID, target.storeRef)
		if err != nil {
			return err
		}
		if len(skips) > 0 {
			// reopen-source risks re-slinging a bead whose true blocking
			// root sits in a store we couldn't scan. Surface the skipped
			// stores so operators know coverage was degraded.
			fmt.Fprintln(stderr, "warning:", formatSourceWorkflowStoreSkips(skips)) //nolint:errcheck
		}
		totalRoots, _, _ := summarizeSourceWorkflowMatches(matches)
		if totalRoots > 0 {
			ids := make([]string, 0, totalRoots)
			for _, match := range matches {
				ids = append(ids, rootIDs(match.roots)...)
			}
			_, _ = fmt.Fprintf(
				stderr,
				"result=conflict source_bead_id=%s blocking_workflow_ids=%s\n",
				sourceBeadID,
				strings.Join(ids, ","),
			)
			resultCode = 3
			return nil
		}
		currentSource, err := target.storeView.store.Get(target.sourceBead.ID)
		if err != nil {
			return err
		}
		open := "open"
		unassigned := ""
		if err := target.storeView.store.SetMetadata(currentSource.ID, "workflow_id", ""); err != nil {
			return err
		}
		// Clear gc.routed_to so a subsequent re-sling is not silently
		// short-circuited by CheckBeadState's idempotency fast-path.
		// CheckBeadState treats a bead with gc.routed_to == target as
		// already routed; a recovered bead must look like a fresh
		// candidate for assignment.
		if err := target.storeView.store.SetMetadata(currentSource.ID, "gc.routed_to", ""); err != nil {
			return err
		}
		if err := target.storeView.store.Update(currentSource.ID, beads.UpdateOpts{
			Status:   &open,
			Assignee: &unassigned,
		}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "result=reopened source_bead_id=%s\n", sourceBeadID)
		return nil
	})
	if runErr != nil {
		_, _ = fmt.Fprintf(stderr, "gc workflow reopen-source: %v\n", runErr)
		return 1
	}
	return resultCode
}

// findWorkflowBeads returns all beads belonging to a workflow resolved by
// either root bead ID or logical gc.workflow_id, plus descendants keyed by the
// resolved root bead IDs.
func workflowDeleteStoreLabel(cfg *config.City, cityPath, scopePath string) string {
	if scopePath == cityPath {
		return "city"
	}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			if resolveStoreScopeRoot(cityPath, rig.Path) == scopePath {
				return "rig:" + rig.Name
			}
		}
	}
	return scopePath
}

func deleteWorkflowBeads(store beads.Store, ids []string) (int, []error) {
	deleted := 0
	var errs []error
	for _, id := range ids {
		if err := deleteWorkflowBead(store, id); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		deleted++
	}
	return deleted, errs
}

func deleteWorkflowBead(store beads.Store, id string) error {
	downDeps, err := store.DepList(id, "down")
	if err != nil {
		return fmt.Errorf("list down deps: %w", err)
	}
	upDeps, err := store.DepList(id, "up")
	if err != nil {
		return fmt.Errorf("list up deps: %w", err)
	}
	removedDown := make([]beads.Dep, 0, len(downDeps))
	for _, dep := range downDeps {
		if err := store.DepRemove(id, dep.DependsOnID); err != nil {
			return withWorkflowDeleteRestoreError(
				fmt.Errorf("remove down dep %s -> %s: %w", id, dep.DependsOnID, err),
				restoreWorkflowDeleteDeps(store, removedDown, nil),
			)
		}
		removedDown = append(removedDown, dep)
	}
	removedUp := make([]beads.Dep, 0, len(upDeps))
	for _, dep := range upDeps {
		if err := store.DepRemove(dep.IssueID, id); err != nil {
			return withWorkflowDeleteRestoreError(
				fmt.Errorf("remove up dep %s -> %s: %w", dep.IssueID, id, err),
				restoreWorkflowDeleteDeps(store, removedDown, removedUp),
			)
		}
		removedUp = append(removedUp, dep)
	}
	if err := store.Delete(id); err != nil {
		return withWorkflowDeleteRestoreError(
			fmt.Errorf("delete bead: %w", err),
			restoreWorkflowDeleteDeps(store, removedDown, removedUp),
		)
	}
	return nil
}

func withWorkflowDeleteRestoreError(primary, restoreErr error) error {
	if restoreErr == nil {
		return primary
	}
	return errors.Join(primary, fmt.Errorf("rollback failed: %w", restoreErr))
}

func restoreWorkflowDeleteDeps(store beads.Store, downDeps, upDeps []beads.Dep) error {
	var restoreErr error
	for _, dep := range downDeps {
		if err := store.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err))
		}
	}
	for _, dep := range upDeps {
		if err := store.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err))
		}
	}
	return restoreErr
}

func collectSourceWorkflowMatches(cfg *config.City, cityPath, sourceBeadID, sourceStoreRef string) ([]sourceWorkflowStoreMatch, []sourceWorkflowStoreSkip, error) {
	stores, skips, err := openSourceWorkflowStores(cfg, cityPath, sourceBeadID)
	if err != nil {
		return nil, skips, err
	}
	matchesByLabel := map[string]sourceWorkflowStoreMatch{}
	visited := map[string]struct{}{}
	cityName := loadedCityName(cfg, cityPath)

	var collect func(string, string) error
	collect = func(currentSourceID, currentSourceStoreRef string) error {
		currentSourceID = strings.TrimSpace(currentSourceID)
		if currentSourceID == "" {
			return nil
		}
		for _, info := range stores {
			rootStoreRef := workflowStoreRefForDir(info.path, cityPath, cityName, cfg)
			// Downward delete-source walks key by root store plus source
			// identity. The upward finalize walk in internal/dispatch only
			// needs source store plus bead ID because each hop has one parent.
			visitKey := rootStoreRef + "\x00" + currentSourceStoreRef + "\x00" + currentSourceID
			if _, ok := visited[visitKey]; ok {
				continue
			}
			visited[visitKey] = struct{}{}
			roots, err := sourceworkflow.ListLiveRoots(info.store, currentSourceID, currentSourceStoreRef, rootStoreRef)
			if err != nil {
				return err
			}
			if len(roots) > 0 {
				beadSet := make([]beads.Bead, 0, len(roots))
				for _, root := range roots {
					beadSet = append(beadSet, findWorkflowBeads(info.store, root.ID)...)
				}
				mergeSourceWorkflowMatch(matchesByLabel, sourceWorkflowStoreMatch{
					label:  workflowDeleteStoreLabel(cfg, cityPath, info.path),
					store:  info.store,
					roots:  roots,
					beads:  uniqueBeads(beadSet),
					path:   info.path,
					runner: workflowDeleteRunnerForPath(cfg, cityPath, info.path),
				})
			}
			children, err := sourceWorkflowChildSources(info.store, currentSourceID, currentSourceStoreRef, rootStoreRef)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := collect(child.ID, rootStoreRef); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := collect(sourceBeadID, sourceStoreRef); err != nil {
		return nil, skips, err
	}
	matches := make([]sourceWorkflowStoreMatch, 0, len(matchesByLabel))
	for _, match := range matchesByLabel {
		match.roots = uniqueBeads(match.roots)
		match.beads = uniqueBeads(match.beads)
		matches = append(matches, match)
	}
	return matches, skips, nil
}

func mergeSourceWorkflowMatch(matches map[string]sourceWorkflowStoreMatch, next sourceWorkflowStoreMatch) {
	if next.label == "" {
		return
	}
	current := matches[next.label]
	if current.label == "" {
		matches[next.label] = next
		return
	}
	current.roots = append(current.roots, next.roots...)
	current.beads = append(current.beads, next.beads...)
	matches[next.label] = current
}

func sourceWorkflowChildSources(store beads.Store, sourceBeadID, sourceStoreRef, rootStoreRef string) ([]beads.Bead, error) {
	sourceBeadID = strings.TrimSpace(sourceBeadID)
	if store == nil || sourceBeadID == "" {
		return nil, nil
	}
	candidates, err := store.List(beads.ListQuery{
		IncludeClosed: true,
		Metadata: map[string]string{
			"gc.source_bead_id": sourceBeadID,
		},
	})
	if err != nil {
		return nil, err
	}
	children := make([]beads.Bead, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID == "" || sourceworkflow.IsWorkflowRoot(candidate) {
			continue
		}
		if !sourceworkflow.WorkflowMatchesSource(candidate, sourceBeadID, sourceStoreRef, rootStoreRef) {
			continue
		}
		children = append(children, candidate)
	}
	return children, nil
}

func sourceWorkflowMatchLabels(matches []sourceWorkflowStoreMatch) []string {
	labels := make([]string, 0, len(matches))
	for _, match := range matches {
		labels = append(labels, match.label)
	}
	return labels
}

func summarizeSourceWorkflowMatches(matches []sourceWorkflowStoreMatch) (roots, beadsTotal, openCount int) {
	for _, match := range matches {
		roots += len(match.roots)
		beadsTotal += len(match.beads)
		for _, bead := range match.beads {
			if bead.Status != "closed" {
				openCount++
			}
		}
	}
	return roots, beadsTotal, openCount
}

func countOpenMatchedBeads(matches []sourceWorkflowStoreMatch) (int, error) {
	open := 0
	for _, match := range matches {
		for _, bead := range match.beads {
			current, err := match.store.Get(bead.ID)
			if err != nil {
				if errors.Is(err, beads.ErrNotFound) {
					continue
				}
				return 0, err
			}
			if current.Status != "closed" {
				open++
			}
		}
	}
	return open, nil
}

// sourceWorkflowStoreSkip records a candidate store that could not be opened
// during a source-workflow singleton scan. Tolerating unopenable stores
// avoids turning a rig-local problem into a city-wide outage, but the
// silent skip creates a correctness hole: a cross-store live root living
// in the broken rig is invisible to the singleton check. Callers MUST
// surface skips (stderr, SlingResult.MetadataErrors, etc.) so operators
// can see when singleton coverage has degraded and decide whether to
// proceed or repair the rig first.
type sourceWorkflowStoreSkip struct {
	path string
	err  error
}

// formatSourceWorkflowStoreSkips renders skipped stores as a single
// human-readable warning line suitable for stderr or MetadataErrors.
func formatSourceWorkflowStoreSkips(skips []sourceWorkflowStoreSkip) string {
	if len(skips) == 0 {
		return ""
	}
	parts := make([]string, 0, len(skips))
	for _, skip := range skips {
		parts = append(parts, fmt.Sprintf("%s (%v)", skip.path, skip.err))
	}
	return fmt.Sprintf(
		"source-workflow singleton scan skipped %d unopenable store(s); cross-store roots in those stores are invisible: %s",
		len(skips),
		strings.Join(parts, "; "),
	)
}

// openSourceWorkflowStores opens every candidate bead store used for
// source-workflow singleton checks. It tolerates broken non-selected stores
// the same way openConvoyStores does: a failure to open one rig's store must
// not block launches or recovery city-wide. Only when *every* candidate is
// unopenable do we surface the first error, because at that point the
// singleton check has no stores to scan and we cannot proceed safely. Stores
// explicitly selected via --rig / --store-ref still go through
// openSourceWorkflowStoreRef, which is strict on purpose.
//
// The second return value lists the stores that were skipped — callers are
// expected to surface these (see formatSourceWorkflowStoreSkips) so operators
// can see when singleton coverage degraded.
func openSourceWorkflowStores(cfg *config.City, cityPath, beadID string) ([]convoyStoreView, []sourceWorkflowStoreSkip, error) {
	return openSourceWorkflowStoresWith(cfg, cityPath, beadID, func(dir string) (beads.Store, error) {
		return openStoreAtForCity(dir, cityPath)
	})
}

// openSourceWorkflowStoresWith is the testable core of openSourceWorkflowStores.
// It takes the store-opening callback explicitly so tests can inject broken
// rig stores without touching the filesystem.
func openSourceWorkflowStoresWith(cfg *config.City, cityPath, beadID string, openStore func(string) (beads.Store, error)) ([]convoyStoreView, []sourceWorkflowStoreSkip, error) {
	candidates := convoyStoreCandidates(cfg, cityPath, beadID)
	var (
		stores   = make([]convoyStoreView, 0, len(candidates))
		skips    []sourceWorkflowStoreSkip
		firstErr error
	)
	for _, dir := range candidates {
		store, err := openStore(dir)
		if err != nil {
			wrapped := fmt.Errorf("opening source workflow store %s: %w", dir, err)
			skips = append(skips, sourceWorkflowStoreSkip{path: dir, err: err})
			if firstErr == nil {
				firstErr = wrapped
			}
			continue
		}
		stores = append(stores, convoyStoreView{path: dir, store: store})
	}
	if len(stores) > 0 {
		return stores, skips, nil
	}
	if firstErr != nil {
		return nil, skips, firstErr
	}
	return nil, skips, fmt.Errorf("no source workflow stores available")
}

func clearSourceWorkflowMetadata(cfg *config.City, cityPath string, target resolvedSourceWorkflowTarget) (bool, error) {
	bead := target.sourceBead
	storeView := target.storeView
	if storeView.store == nil || strings.TrimSpace(storeView.path) == "" {
		if target.storeRef == "" {
			return false, nil
		}
		var err error
		storeView, _, err = openSourceWorkflowStoreRef(cfg, cityPath, target.storeRef)
		if err != nil {
			return false, err
		}
	}
	if strings.TrimSpace(bead.ID) == "" {
		current, err := storeView.store.Get(target.sourceBeadID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		bead = current
	}
	if strings.TrimSpace(bead.Metadata["workflow_id"]) == "" {
		return false, nil
	}
	if err := storeView.store.SetMetadata(bead.ID, "workflow_id", ""); err != nil {
		return false, err
	}
	return true, nil
}

func rootIDs(roots []beads.Bead) []string {
	ids := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.ID == "" {
			continue
		}
		ids = append(ids, root.ID)
	}
	return ids
}

func uniqueBeads(bb []beads.Bead) []beads.Bead {
	out := make([]beads.Bead, 0, len(bb))
	seen := make(map[string]struct{}, len(bb))
	for _, bead := range bb {
		if bead.ID == "" {
			continue
		}
		if _, ok := seen[bead.ID]; ok {
			continue
		}
		seen[bead.ID] = struct{}{}
		out = append(out, bead)
	}
	return out
}

func findWorkflowBeads(store beads.Store, workflowID string) []beads.Bead {
	result := make([]beads.Bead, 0, 4)
	seen := make(map[string]struct{}, 4)
	rootIDs := make([]string, 0, 2)
	rootSeen := make(map[string]struct{}, 2)
	addBead := func(b beads.Bead) {
		if b.ID == "" {
			return
		}
		if _, ok := seen[b.ID]; ok {
			return
		}
		seen[b.ID] = struct{}{}
		result = append(result, b)
	}
	addRoot := func(root beads.Bead) {
		resolvedWorkflowID := strings.TrimSpace(root.Metadata["gc.workflow_id"])
		// Match sourceworkflow.IsWorkflowRoot so graph.v2-only roots (marked
		// via gc.formula_contract=graph.v2 without gc.kind=workflow) are
		// collected here. Without this, delete-source lists the root but
		// fails to close its descendants — a hole in the singleton recovery
		// flow that this PR is trying to enforce.
		if !sourceworkflow.IsWorkflowRoot(root) {
			return
		}
		if root.ID != workflowID && resolvedWorkflowID != workflowID {
			return
		}
		if _, ok := rootSeen[root.ID]; ok {
			return
		}
		rootSeen[root.ID] = struct{}{}
		rootIDs = append(rootIDs, root.ID)
		addBead(root)
	}
	if root, err := store.Get(workflowID); err == nil {
		addRoot(root)
	}
	// Query on gc.workflow_id only; the predicate is applied in-memory via
	// addRoot so we pick up graph.v2-only roots alongside legacy roots.
	if roots, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			"gc.workflow_id": workflowID,
		},
		IncludeClosed: true,
	}); err == nil {
		for _, root := range roots {
			addRoot(root)
		}
	}
	for _, rootID := range rootIDs {
		all, err := store.List(beads.ListQuery{
			Metadata:      map[string]string{"gc.root_bead_id": rootID},
			IncludeClosed: true,
		})
		if err != nil {
			continue
		}
		for _, b := range all {
			addBead(b)
		}
	}
	return result
}

func workflowBeadIDs(bb []beads.Bead) []string {
	ids := make([]string, len(bb))
	for i, b := range bb {
		ids[i] = b.ID
	}
	return ids
}
