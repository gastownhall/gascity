package rig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
)

// Provision runs the full rig-add provisioning against the injected deps.
//
// It is the extracted core of the CLI's doRigAddWithResult: operations are
// ordered so that city.toml is written last — if any earlier step fails,
// config is unchanged. The city config write, the deferred packs.lock commit,
// and the routes regeneration run under a topology snapshot so a failure in
// that window rolls the filesystem back atomically.
//
// Fatal conditions return an error whose Error() is exactly the text the CLI
// prints after the "gc rig add: " prefix. Non-fatal progress and warnings ride
// Deps.OnStep (and ProvisionResult.Steps/Warnings); the caller renders them.
//
// The body reads as the provisioning pipeline; each numbered step delegates to a
// helper below so this function stays a readable orchestration.
func Provision(deps Deps, req ProvisionRequest) (config.Rig, ProvisionResult, error) {
	if err := validateDeps(deps); err != nil {
		return config.Rig{}, ProvisionResult{}, err
	}
	if err := validateRequest(req); err != nil {
		return config.Rig{}, ProvisionResult{}, err
	}

	fs := deps.FS
	cfg := deps.Cfg
	cityPath := deps.CityPath
	rigPath := req.Path
	tomlPath := filepath.Join(cityPath, "city.toml")

	var result ProvisionResult
	emit := func(step ProvisionStep) {
		result.Steps = append(result.Steps, step)
		if step.Warn {
			result.Warnings = append(result.Warnings, step.Detail)
		}
		if deps.OnStep != nil {
			deps.OnStep(step)
		}
	}

	// Step 1: trim and drop empty --include entries.
	includes := trimIncludes(req.Includes)

	// Step 2: stat the rig path (shared with the CLI's StatRigPath preflight).
	rigPathExists, err := StatRigPath(fs, rigPath, req.Adopt)
	if err != nil {
		return config.Rig{}, result, err
	}

	// Step 2.5: clone from --git-url when the caller supports it.
	rigPathExists, err = maybeCloneRig(deps, req, rigPath, rigPathExists)
	if err != nil {
		return config.Rig{}, result, err
	}

	// Step 3: detect git and resolve the default branch.
	hasGit, defaultBranchOverride, resolvedDefaultBranch := resolveGitDefaultBranch(deps, req, rigPath)

	// Step 4: canonicalize --include tokens that name a materialized builtin pack.
	includes = canonicalizeBuiltinPackIncludes(fs, cityPath, includes, cfg.Packs)

	// Steps 5-9: resolve imports, detect re-add, derive the prefix, build the next
	// config, and validate it before any filesystem mutation.
	plan, err := planRigMutation(deps, req, rigPath, resolvedDefaultBranch, includes)
	if err != nil {
		return config.Rig{}, result, err
	}

	// Step 10: create the rig directory when missing.
	if err := createRigDirIfMissing(fs, rigPath, rigPathExists); err != nil {
		return config.Rig{}, result, err
	}

	// Step 11: adopt validation, prefix-mismatch guard, fresh-add store guard.
	if err := validateAdoptAndBeadsStore(deps, req, rigPath, plan); err != nil {
		return config.Rig{}, result, err
	}

	// --- Phase 1: Infrastructure (all fallible, before touching city.toml) ---

	// Step 12: banner + warn lines.
	emitRigBannerAndWarnings(deps, req, plan, includes, hasGit, rigPath, resolvedDefaultBranch, defaultBranchOverride, emit)

	// Step 13: beads-store init.
	deferred, err := initRigBeadsStore(deps, req, rigPath, plan.prefix, emit)
	if err != nil {
		return config.Rig{}, result, err
	}
	result.Deferred = deferred

	// Step 14: snapshot the topology before the first config write.
	snapshots, err := SnapshotTopologyFiles(fs, cityPath, plan.nextCfg)
	if err != nil {
		return config.Rig{}, result, fmt.Errorf("snapshot canonical files: %w", err)
	}

	// Panic-safety for the guarded write window: once the snapshot exists, a
	// panic in an injected write func (or OnStep) must restore the filesystem
	// before it propagates, or the async controller goroutine (C4) would strand
	// half-written topology. After the routes write succeeds the mutations are
	// committed, so a later panic (e.g. in PostProvision) must NOT roll them back.
	committed := false
	defer func() {
		if r := recover(); r != nil {
			if !committed {
				_ = RestoreSnapshots(fs, snapshots)
			}
			panic(r)
		}
	}()

	// Steps 15-16: guarded config write + deferred packs.lock commit.
	if err := writeRigTopology(deps, plan, tomlPath, snapshots); err != nil {
		return config.Rig{}, result, err
	}
	cfg = plan.nextCfg

	if err := deps.WriteRoutes(cityPath, cfg); err != nil {
		return config.Rig{}, result, rollbackError(fs, snapshots, "writing routes", err)
	}
	committed = true
	emit(ProvisionStep{Name: "routes", Detail: "  Generated routes.jsonl for cross-rig routing"})

	// Resolve the returned rig from the post-write config (a fresh add returns
	// the stored, possibly-empty prefix, not the effective one).
	resultRig := resolveResultRig(cfg, req, resolvedDefaultBranch)

	// Step 17: caller-specific side effects (CLI hooks/formulas/.env/reload).
	// Its error does not roll back — the disk writes are committed — but it is
	// captured so an API caller can surface a failed mutateAndPoke.
	result.PostProvisionErr = runRigPostProvision(deps, resultRig, deferred, plan.nextCfg, rigPath)

	emitRigDone(req, plan.reAdd, emit)

	return resultRig, result, nil
}

// rigMutationPlan is the resolved outcome of planRigMutation: what config the
// add will write and the bookkeeping the later steps consume. reAdd and
// reAddNeedsConfigWrite classify the add; existingRig is the pre-existing entry
// on a re-add (nil otherwise); the *RigImports fields and commitRigImports carry
// the resolved bundled imports and the deferred packs.lock commit.
type rigMutationPlan struct {
	nextCfg               *config.City
	prefix                string
	reAdd                 bool
	reAddNeedsConfigWrite bool
	existingRig           *config.Rig
	explicitRigImports    []config.BoundImport
	defaultRigImports     []config.BoundImport
	commitRigImports      func() error
}

// planRigMutation runs steps 5-9: it resolves the explicit bundled imports,
// detects a re-add, derives and collision-checks the prefix, backfills a
// default branch that forces a re-add write, builds the next config, and
// validates the resulting rig set before any filesystem mutation. Its errors
// are the byte-identical fatal texts the CLI prints.
func planRigMutation(deps Deps, req ProvisionRequest, rigPath, resolvedDefaultBranch string, includes []string) (rigMutationPlan, error) {
	cfg := deps.Cfg
	cityPath := deps.CityPath
	name := req.Name

	// Step 5: resolve the explicit bundled rig imports (call #1).
	explicitRigImports, commitRigImports, err := deps.ComposePacks(cityPath, config.BoundImportsFromLegacySources(includes, cfg.Packs))
	if err != nil {
		return rigMutationPlan{}, fmt.Errorf("installing bundled rig imports: %w", err)
	}

	// Step 6: re-add detection.
	reAdd, reAddNeedsConfigWrite, existingRigIdx, existingRig, err := detectRigReAdd(cfg, name, cityPath, rigPath)
	if err != nil {
		return rigMutationPlan{}, err
	}

	// Step 7: prefix resolution + collision checks.
	prefix, err := resolveRigPrefix(cfg, req, name, reAdd, existingRig)
	if err != nil {
		return rigMutationPlan{}, err
	}
	// A resolved default branch that the existing rig lacks forces a config write
	// on an otherwise-plain re-add so the branch is persisted.
	if reAdd && existingRig != nil && existingRig.EffectiveDefaultBranch() == "" && resolvedDefaultBranch != "" {
		reAddNeedsConfigWrite = true
	}

	// Step 8: build nextCfg.
	needsValidation := !reAdd || reAddNeedsConfigWrite
	nextCfg, defaultRigImports, commitRigImports, err := buildNextRigConfig(deps, req, rigPath, resolvedDefaultBranch, reAdd, reAddNeedsConfigWrite, existingRigIdx, explicitRigImports, commitRigImports)
	if err != nil {
		return rigMutationPlan{}, err
	}

	// Step 9: validate rigs before any filesystem mutation.
	if needsValidation {
		if err := config.ValidateRigs(nextCfg.Rigs, config.EffectiveHQPrefix(nextCfg)); err != nil {
			return rigMutationPlan{}, err
		}
	}

	return rigMutationPlan{
		nextCfg:               nextCfg,
		prefix:                prefix,
		reAdd:                 reAdd,
		reAddNeedsConfigWrite: reAddNeedsConfigWrite,
		existingRig:           existingRig,
		explicitRigImports:    explicitRigImports,
		defaultRigImports:     defaultRigImports,
		commitRigImports:      commitRigImports,
	}, nil
}

// detectRigReAdd scans the city for an existing rig with the same name. An empty
// stored path is a re-add that needs a config write to record the path; a
// matching path is a plain re-add; a different path is a fatal collision. A miss
// returns a fresh add (existingRigIdx -1, existingRig nil).
func detectRigReAdd(cfg *config.City, name, cityPath, rigPath string) (reAdd, needsConfigWrite bool, existingRigIdx int, existingRig *config.Rig, err error) {
	existingRigIdx = -1
	for i, r := range cfg.Rigs {
		if r.Name != name {
			continue
		}
		existingRigIdx = i
		existingRig = &cfg.Rigs[i]
		existPath := r.Path
		if strings.TrimSpace(existPath) == "" {
			return true, true, existingRigIdx, existingRig, nil
		}
		if !filepath.IsAbs(existPath) {
			existPath = filepath.Join(cityPath, existPath)
		}
		if filepath.Clean(existPath) != filepath.Clean(rigPath) {
			return false, false, existingRigIdx, existingRig, fmt.Errorf("rig %q already registered at %s (not %s)", name, r.Path, rigPath)
		}
		return true, false, existingRigIdx, existingRig, nil
	}
	return false, false, existingRigIdx, existingRig, nil
}

// resolveRigPrefix derives the rig's bead prefix — the existing rig's on a
// re-add, the lowercased --prefix override, or one derived from the name — and,
// for a fresh add, rejects a prefix that collides with HQ or another rig with
// the byte-identical CLI text.
func resolveRigPrefix(cfg *config.City, req ProvisionRequest, name string, reAdd bool, existingRig *config.Rig) (string, error) {
	var prefix string
	switch {
	case reAdd:
		prefix = existingRig.EffectivePrefix()
	case req.Prefix != "":
		prefix = strings.ToLower(req.Prefix)
	default:
		prefix = config.DeriveBeadsPrefix(name)
	}

	if !reAdd {
		prefixKey := strings.ToLower(prefix)
		if prefixKey == strings.ToLower(config.EffectiveHQPrefix(cfg)) {
			return "", fmt.Errorf("rig %q: prefix %q collides with HQ. Use --prefix to specify a different prefix.", name, prefixKey) //nolint:revive,staticcheck // byte-identical rig-add collision text (trailing period)
		}
		for _, rg := range cfg.Rigs {
			if prefixKey == strings.ToLower(rg.EffectivePrefix()) {
				return "", fmt.Errorf("rig %q: prefix %q collides with %s. Use --prefix to specify a different prefix.", name, prefixKey, rg.Name) //nolint:revive,staticcheck // byte-identical rig-add collision text (trailing period)
			}
		}
	}
	return prefix, nil
}

// buildNextRigConfig materializes the config to write. A re-add that needs a
// write copies the city and backfills the existing rig's path/default branch; a
// fresh add appends a new rig (installing default-rig imports when no explicit
// --include set them, which may reassign commitRigImports). A plain re-add
// returns the caller's config unchanged. It returns the next config, any
// default-rig imports it resolved, and the (possibly updated) packs.lock commit.
func buildNextRigConfig(deps Deps, req ProvisionRequest, rigPath, resolvedDefaultBranch string, reAdd, reAddNeedsConfigWrite bool, existingRigIdx int, explicitRigImports []config.BoundImport, commitRigImports func() error) (*config.City, []config.BoundImport, func() error, error) {
	fs := deps.FS
	cfg := deps.Cfg
	cityPath := deps.CityPath
	name := req.Name

	nextCfg := cfg
	var defaultRigImports []config.BoundImport
	if reAddNeedsConfigWrite {
		next := *cfg
		next.Rigs = append([]config.Rig{}, cfg.Rigs...)
		if strings.TrimSpace(next.Rigs[existingRigIdx].Path) == "" {
			next.Rigs[existingRigIdx].Path = rigPath
		}
		if next.Rigs[existingRigIdx].EffectiveDefaultBranch() == "" && resolvedDefaultBranch != "" {
			next.Rigs[existingRigIdx].DefaultBranch = resolvedDefaultBranch
		}
		nextCfg = &next
	} else if !reAdd {
		storedPrefix := ""
		if req.Prefix != "" {
			storedPrefix = strings.ToLower(req.Prefix)
		}
		addedRig := config.Rig{
			Name:             name,
			Path:             rigPath,
			Prefix:           storedPrefix,
			DefaultBranch:    resolvedDefaultBranch,
			SuspendedOnStart: req.StartSuspended,
		}
		switch {
		case len(explicitRigImports) > 0:
			addedRig.Imports = boundImportsMap(explicitRigImports)
		default:
			rootDefaultRigImports, err := config.LoadRootPackDefaultRigImports(fs, cityPath)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("loading root pack defaults: %w", err)
			}
			// Default-rig imports take the same pin/cache hardening as
			// explicit --include imports: a version-less bundled source
			// arriving from root-pack defaults or legacy
			// default_rig_includes must not persist version-less.
			defaultRigImports, commitRigImports, err = deps.ComposePacks(cityPath, composeDefaultRigImports(rootDefaultRigImports, cfg.Workspace.LegacyDefaultRigIncludes(), cfg.Packs))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("installing bundled rig imports: %w", err)
			}
			if len(defaultRigImports) > 0 {
				addedRig.Imports = boundImportsMap(defaultRigImports)
			}
		}
		next := *cfg
		next.Rigs = append(append([]config.Rig{}, cfg.Rigs...), addedRig)
		nextCfg = &next
	}
	return nextCfg, defaultRigImports, commitRigImports, nil
}

// trimIncludes drops blank --include entries so `--include=` or `--include " "`
// doesn't persist an empty pack path that downstream resolution reads as the
// city root. The result never aliases the input slice's backing array.
func trimIncludes(includes []string) []string {
	out := includes[:0:0]
	for _, inc := range includes {
		if trimmed := strings.TrimSpace(inc); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// maybeCloneRig performs the --git-url clone (C3/G15) when the caller supports
// it, reporting whether the rig path now exists. It is guarded by a non-nil
// Deps.CloneGitURL so the CLI local path (which passes nil) stays byte-identical:
// after a successful clone the rig directory exists (with .git), so it flows
// through the existing git-detect and skips the MkdirAll. The staging-dir→rename
// orchestration and the pre-clone SSRF host fence (internal/ssrf) are the server
// layer's job (C4); on failure that layer removes the partial staging dir. The
// error is already URL-redacted by git.Clone, and req.GitURL is never echoed
// here, so no embedded credential leaks into the returned error.
func maybeCloneRig(deps Deps, req ProvisionRequest, rigPath string, rigPathExists bool) (bool, error) {
	if deps.CloneGitURL == nil || strings.TrimSpace(req.GitURL) == "" {
		return rigPathExists, nil
	}
	opts := git.CloneOptions{RecurseSubmodules: req.RecurseSubmodules}
	if err := deps.CloneGitURL(context.Background(), req.GitURL, rigPath, opts); err != nil {
		return rigPathExists, fmt.Errorf("%w: %w", ErrCloneFailed, err)
	}
	return true, nil
}

// resolveGitDefaultBranch reports whether the rig path is a git repo and resolves
// the default branch: the explicit --default-branch override wins, otherwise a
// probe of the repo (when one is present and a prober is injected). It returns
// hasGit, the trimmed override, and the resolved branch (which equals the
// override when no probe runs).
func resolveGitDefaultBranch(deps Deps, req ProvisionRequest, rigPath string) (hasGit bool, override, resolved string) {
	_, gitErr := deps.FS.Stat(filepath.Join(rigPath, ".git"))
	hasGit = gitErr == nil
	override = strings.TrimSpace(req.DefaultBranch)
	resolved = override
	if resolved == "" && hasGit && deps.ProbeBranch != nil {
		resolved = deps.ProbeBranch(rigPath)
	}
	return hasGit, override, resolved
}

// createRigDirIfMissing creates the rig directory when the earlier stat (or a
// clone) did not already materialize it.
func createRigDirIfMissing(fs fsys.FS, rigPath string, rigPathExists bool) error {
	if rigPathExists {
		return nil
	}
	if err := fs.MkdirAll(rigPath, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", rigPath, err)
	}
	return nil
}

// validateAdoptAndBeadsStore runs step 11's fatal guards against the on-disk
// .beads store: --adopt requires an initialized store with a valid prefix; an
// existing store whose prefix disagrees with the resolved one is rejected with
// role-specific recovery text; and a fresh add refuses to run over a directory
// that already holds a store. All returned texts are byte-identical to the CLI.
func validateAdoptAndBeadsStore(deps Deps, req ProvisionRequest, rigPath string, plan rigMutationPlan) error {
	fs := deps.FS
	name := req.Name
	prefix := plan.prefix

	if req.Adopt {
		metaPath := filepath.Join(rigPath, ".beads", "metadata.json")
		if _, err := fs.Stat(metaPath); err != nil {
			return fmt.Errorf("--adopt requires .beads/metadata.json in %s", rigPath)
		}
		if _, ok := ReadBeadsPrefix(fs, rigPath); !ok {
			return fmt.Errorf("--adopt requires a valid issue_prefix in .beads/config.yaml in %s", rigPath)
		}
	}

	if existingPrefix, ok := ReadBeadsPrefix(fs, rigPath); ok && existingPrefix != prefix {
		switch {
		case plan.reAdd:
			// On re-add, --prefix is ignored (we use the existing rig's
			// configured prefix). Direct the user to edit city.toml.
			return fmt.Errorf("rig %q has bead prefix %q but city.toml has %q; "+
				"edit city.toml to set prefix = %q, or remove %s/.beads to reinitialize",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		case req.Adopt:
			// On --adopt, the user explicitly wants the existing store.
			// "Remove .beads to reinitialize" is the wrong recovery here:
			// nudge them toward matching the existing prefix instead.
			return fmt.Errorf("--adopt: rig %q already has bead prefix %q (requested %q); "+
				"use --prefix %s (or omit --prefix) to match the existing store",
				name, existingPrefix, prefix, existingPrefix)
		default:
			return fmt.Errorf("rig %q already has bead prefix %q (requested %q); "+
				"use --prefix %s to match, or remove %s/.beads to reinitialize",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		}
	}

	// Guard: on a fresh add (not a re-add) without --adopt, refuse to run
	// if .beads/ already holds a beads store. Without this, provisioning
	// falls through to bd init against an existing Dolt store and typically
	// dies with "bd init: signal: killed" after the probe times out.
	//
	// We treat .beads/ as a store only when metadata.json or config.yaml is
	// present. A directory that happens to be named .beads/ but contains
	// only unrelated content (e.g. the beads project's own .beads/formulas/
	// convention for formula source files) is not a store, so the init path
	// decides how to create the missing store files in place.
	if !plan.reAdd && !req.Adopt {
		beadsPath := filepath.Join(rigPath, ".beads")
		fi, err := fs.Stat(beadsPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("checking %s: %w", beadsPath, err)
		}
		if err == nil && fi.IsDir() {
			containsStore, containsErr := beadsDirContainsStore(fs, beadsPath)
			if containsErr != nil {
				return containsErr
			}
			if containsStore {
				return fmt.Errorf("%s/.beads already contains a beads store; "+
					"use --adopt to register it, or remove %s/.beads to reinitialize",
					rigPath, rigPath)
			}
		}
	}
	return nil
}

// emitRigBannerAndWarnings emits step 12's banner and the re-add warning lines:
// on a re-add it warns that --start-suspended, --include, --prefix, and
// --default-branch overrides are ignored in favor of the existing rig; on a
// fresh add it announces the prefix, default branch, and resolved imports. It is
// pure progress emission — every branch ends in an emit, never an error.
func emitRigBannerAndWarnings(deps Deps, req ProvisionRequest, plan rigMutationPlan, includes []string, hasGit bool, rigPath, resolvedDefaultBranch, defaultBranchOverride string, emit func(ProvisionStep)) {
	name := req.Name
	if plan.reAdd {
		emit(ProvisionStep{Name: "banner", Detail: fmt.Sprintf("Re-initializing rig '%s'...", name)})
		if req.StartSuspended && req.StartSuspended != plan.existingRig.EffectiveSuspendedOnStart() {
			emit(ProvisionStep{Name: "start-suspended-ignored", Warn: true, Detail: fmt.Sprintf("warning: --start-suspended ignored (existing: suspended_on_start=%v); edit city.toml to change", plan.existingRig.EffectiveSuspendedOnStart())})
		}
		if len(plan.explicitRigImports) > 0 {
			existingRigImports, err := effectiveRigBoundImports(plan.existingRig, deps.Cfg.Packs)
			if err != nil {
				emit(ProvisionStep{Name: "include-ignored", Warn: true, Detail: fmt.Sprintf("warning: --include flags %v ignored; existing rig imports could not be normalized (%v). Edit city.toml to change", includes, err)})
			} else if !slices.Equal(existingRigImports, plan.explicitRigImports) {
				emit(ProvisionStep{Name: "include-ignored", Warn: true, Detail: fmt.Sprintf("warning: --include flags %v ignored (existing imports: %s); edit city.toml to change", includes, formatBoundImports(existingRigImports))})
			}
		}
		if req.Prefix != "" && strings.ToLower(req.Prefix) != plan.existingRig.EffectivePrefix() {
			emit(ProvisionStep{Name: "prefix-ignored", Warn: true, Detail: fmt.Sprintf("warning: --prefix=%s ignored (existing: %s); edit city.toml to change", req.Prefix, plan.existingRig.EffectivePrefix())})
		}
		if defaultBranchOverride != "" &&
			defaultBranchOverride != plan.existingRig.EffectiveDefaultBranch() &&
			(plan.existingRig.EffectiveDefaultBranch() != "" || resolvedDefaultBranch != defaultBranchOverride) {
			emit(ProvisionStep{Name: "default-branch-ignored", Warn: true, Detail: fmt.Sprintf("warning: --default-branch=%s ignored (existing: %s); edit city.toml to change", defaultBranchOverride, plan.existingRig.EffectiveDefaultBranch())})
		}
	} else {
		emit(ProvisionStep{Name: "banner", Detail: fmt.Sprintf("Adding rig '%s'...", name)})
	}
	if hasGit {
		emit(ProvisionStep{Name: "git-detected", Detail: fmt.Sprintf("  Detected git repo at %s", rigPath)})
	}
	emit(ProvisionStep{Name: "prefix", Detail: fmt.Sprintf("  Prefix: %s", plan.prefix)})
	if !plan.reAdd && resolvedDefaultBranch != "" {
		emit(ProvisionStep{Name: "default-branch", Detail: fmt.Sprintf("  Default branch: %s", resolvedDefaultBranch)})
	}
	if !plan.reAdd {
		switch {
		case len(plan.explicitRigImports) > 0:
			emit(ProvisionStep{Name: "imports", Detail: fmt.Sprintf("  Import: %s", formatBoundImports(plan.explicitRigImports))})
		default:
			if len(plan.defaultRigImports) > 0 {
				emit(ProvisionStep{Name: "imports", Detail: fmt.Sprintf("  Import: %s (default)", formatBoundImports(plan.defaultRigImports))})
			}
		}
	}
}

// initRigBeadsStore runs step 13's bead-store initialization and emits the
// matching progress. --adopt optionally prepares provider state and only inits
// under the store contract; a fresh add inits directly, falling back to the
// deferred "init deferred to controller" path when InitStore punts (and the
// store is GC_DOLT=skip or the deeper InitAndHook fails). It returns whether
// init was deferred.
func initRigBeadsStore(deps Deps, req ProvisionRequest, rigPath, prefix string, emit func(ProvisionStep)) (bool, error) {
	cityPath := deps.CityPath
	storeContract := func() bool { return deps.StoreContract != nil && deps.StoreContract(cityPath) }
	doltSkip := func() bool { return deps.DoltSkip != nil && deps.DoltSkip() }

	var deferred bool
	var err error
	if req.Adopt {
		if deps.PrepareAdopt != nil {
			if err := deps.PrepareAdopt(cityPath, rigPath); err != nil {
				return false, fmt.Errorf("prepare adopted rig store: %w", err)
			}
		}
		if storeContract() {
			deferred, err = deps.InitStore(cityPath, rigPath, prefix)
			if err != nil {
				return false, err
			}
		}
		emit(ProvisionStep{Name: "beads-init", Detail: "  Adopted existing beads database"})
		return deferred, nil
	}

	deferred, err = deps.InitStore(cityPath, rigPath, prefix)
	if err != nil {
		return false, err
	}
	if deferred {
		if storeContract() && doltSkip() {
			emit(ProvisionStep{Name: "beads-init", Detail: "  Beads init deferred to controller"})
		} else if err := deps.InitAndHook(cityPath, rigPath, prefix); err != nil {
			emit(ProvisionStep{Name: "beads-init", Detail: "  Beads init deferred to controller"})
		} else {
			emit(ProvisionStep{Name: "beads-init", Detail: "  Initialized beads database"})
		}
	} else {
		emit(ProvisionStep{Name: "beads-init", Detail: "  Initialized beads database"})
	}
	return deferred, nil
}

// writeRigTopology runs steps 15-16 under the caller's topology snapshot: it
// normalizes the canonical bd scope files and writes city.toml (a surgical
// [[rigs]] append for a fresh add, a full rewrite for a re-add), then commits
// the deferred packs.lock. A failure in any of these rolls the snapshot back and
// returns the rollbackError-wrapped fatal text; success leaves the routes write
// (and the committed flag) to the caller.
func writeRigTopology(deps Deps, plan rigMutationPlan, tomlPath string, snapshots []FileSnapshot) error {
	fs := deps.FS
	cityPath := deps.CityPath

	if !plan.reAdd || plan.reAddNeedsConfigWrite {
		if deps.NormalizeScopes == nil {
			return depErr("NormalizeScopes")
		}
		if err := deps.NormalizeScopes(cityPath, plan.nextCfg); err != nil {
			return rollbackError(fs, snapshots, "canonicalizing rig topology", err)
		}

		var writeErr error
		if !plan.reAdd {
			// Surgical append: preserve existing comments by appending only the
			// new [[rigs]] block instead of re-serializing the whole file.
			newRig := plan.nextCfg.Rigs[len(plan.nextCfg.Rigs)-1]
			writeErr = config.AppendRigAndWriteSiteBindingsForEdit(fs, tomlPath, plan.nextCfg, newRig)
		} else {
			writeErr = config.WriteCityAndRigSiteBindingsForEdit(fs, tomlPath, plan.nextCfg)
		}
		if writeErr != nil {
			return rollbackError(fs, snapshots, "writing config", writeErr)
		}
	}

	// Persist packs.lock and materialize bundled rig imports only after the city
	// config write succeeds, so the lockfile honors the same "city.toml written
	// last" contract: any earlier failure leaves packs.lock untouched, and a
	// failure here rolls back through the snapshot (which now covers packs.lock).
	if plan.commitRigImports != nil {
		if err := plan.commitRigImports(); err != nil {
			return rollbackError(fs, snapshots, "installing bundled rig imports", err)
		}
	}
	return nil
}

// resolveResultRig returns the added/re-added rig as it now stands in the
// post-write config. The constructed fallback (with the stored, possibly-empty
// prefix) mirrors the request for the unreachable-in-practice miss.
func resolveResultRig(cfg *config.City, req ProvisionRequest, resolvedDefaultBranch string) config.Rig {
	for _, rg := range cfg.Rigs {
		if rg.Name == req.Name {
			return rg
		}
	}
	return config.Rig{
		Name:          req.Name,
		Path:          req.Path,
		Prefix:        strings.ToLower(req.Prefix),
		DefaultBranch: resolvedDefaultBranch,
		Suspended:     req.StartSuspended,
	}
}

// runRigPostProvision runs step 17's caller-specific side effects and returns
// their error verbatim (nil when no PostProvision is injected). The error does
// not trigger rollback — the disk writes are already committed — so the caller
// captures it in ProvisionResult.PostProvisionErr.
func runRigPostProvision(deps Deps, resultRig config.Rig, deferred bool, nextCfg *config.City, rigPath string) error {
	if deps.PostProvision == nil {
		return nil
	}
	return deps.PostProvision(ProvisionContext{
		RigPath:  rigPath,
		Rig:      resultRig,
		Deferred: deferred,
		Cfg:      nextCfg,
	})
}

// emitRigDone emits the terminal progress line for the completed add.
func emitRigDone(req ProvisionRequest, reAdd bool, emit func(ProvisionStep)) {
	switch {
	case reAdd:
		emit(ProvisionStep{Name: "done", Detail: "Rig re-initialized."})
	case req.StartSuspended:
		emit(ProvisionStep{Name: "done", Detail: "Rig added (suspended — use 'gc rig resume' to activate)."})
	default:
		emit(ProvisionStep{Name: "done", Detail: "Rig added."})
	}
}

// StatRigPath is the rig-add path preflight. It reports whether rigPath already
// exists as a directory, or returns the fatal error the CLI prints — the
// --adopt-missing, stat-error, and not-a-directory cases. The CLI runs this
// before it loads city.toml so a bad rig path is reported ahead of a
// config-load failure, matching the original doRigAddWithResult ordering;
// Provision calls it as step 2 so the API path enforces the same guard.
func StatRigPath(fs fsys.FS, rigPath string, adopt bool) (exists bool, err error) {
	fi, statErr := fs.Stat(rigPath)
	if statErr != nil {
		if adopt {
			return false, fmt.Errorf("--adopt requires an existing directory: %s", rigPath)
		}
		if !os.IsNotExist(statErr) {
			return false, fmt.Errorf("checking %s: %w", rigPath, statErr)
		}
		return false, nil
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%s is not a directory", rigPath)
	}
	return true, nil
}

// rollbackError restores the topology snapshot and returns the fatal error the
// caller prints. It mirrors the CLI's writeRigAddRollbackError: on a failed
// restore it appends "(rollback failed: ...)" so the operator sees both faults.
func rollbackError(fs fsys.FS, snapshots []FileSnapshot, action string, cause error) error {
	if restoreErr := RestoreSnapshots(fs, snapshots); restoreErr != nil {
		return fmt.Errorf("%s: %w (rollback failed: %w)", action, cause, restoreErr)
	}
	return fmt.Errorf("%s: %w", action, cause)
}
