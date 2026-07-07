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
	name := req.Name
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
	storeContract := func() bool { return deps.StoreContract != nil && deps.StoreContract(cityPath) }
	doltSkip := func() bool { return deps.DoltSkip != nil && deps.DoltSkip() }

	// Step 1: trim and drop empty --include entries so `--include=` or
	// `--include " "` doesn't persist a blank pack path that downstream
	// resolution reads as the city root.
	includes := req.Includes[:0:0]
	for _, inc := range req.Includes {
		if trimmed := strings.TrimSpace(inc); trimmed != "" {
			includes = append(includes, trimmed)
		}
	}

	// Step 2: stat the rig path. Shared with the CLI's StatRigPath preflight so
	// a bad rig path is reported before a config-load failure, preserving the
	// original ordering.
	rigPathExists, err := StatRigPath(fs, rigPath, req.Adopt)
	if err != nil {
		return config.Rig{}, result, err
	}

	// Step 2.5: clone from --git-url (C3/G15). Guarded by a non-nil
	// Deps.CloneGitURL so the CLI local path (which passes nil) stays
	// byte-identical. This step only MATERIALIZES the rig directory that the rest
	// of Provision already knows how to consume: after a successful clone rigPath
	// exists (with .git), so it flows through the existing git-detect (step 3) and
	// skips the MkdirAll (step 10). The staging-dir→rename orchestration and the
	// pre-clone SSRF host fence (internal/ssrf) are the server layer's job (C4);
	// on failure that layer removes the partial staging dir. The error is already
	// URL-redacted by git.Clone, and req.GitURL is never echoed here, so no
	// embedded credential leaks into the returned error.
	if deps.CloneGitURL != nil && strings.TrimSpace(req.GitURL) != "" {
		opts := git.CloneOptions{RecurseSubmodules: req.RecurseSubmodules}
		if err := deps.CloneGitURL(context.Background(), req.GitURL, rigPath, opts); err != nil {
			return config.Rig{}, result, fmt.Errorf("%w: %w", ErrCloneFailed, err)
		}
		rigPathExists = true
	}

	// Step 3: detect git and resolve the default branch.
	_, gitErr := fs.Stat(filepath.Join(rigPath, ".git"))
	hasGit := gitErr == nil
	defaultBranchOverride := strings.TrimSpace(req.DefaultBranch)
	resolvedDefaultBranch := defaultBranchOverride
	if resolvedDefaultBranch == "" && hasGit && deps.ProbeBranch != nil {
		resolvedDefaultBranch = deps.ProbeBranch(rigPath)
	}

	// Step 4: canonicalize --include tokens that name a materialized builtin
	// pack so the flag honors its --help promise of "canonical rig imports".
	includes = canonicalizeBuiltinPackIncludes(fs, cityPath, includes, cfg.Packs)

	// Step 5: resolve the explicit bundled rig imports (call #1).
	explicitRigImports, commitRigImports, err := deps.ComposePacks(cityPath, config.BoundImportsFromLegacySources(includes, cfg.Packs))
	if err != nil {
		return config.Rig{}, result, fmt.Errorf("installing bundled rig imports: %w", err)
	}

	// Step 6: re-add detection.
	var reAdd bool
	var reAddNeedsConfigWrite bool
	existingRigIdx := -1
	var existingRig *config.Rig
	for i, r := range cfg.Rigs {
		if r.Name != name {
			continue
		}
		existingRigIdx = i
		existingRig = &cfg.Rigs[i]
		existPath := r.Path
		if strings.TrimSpace(existPath) == "" {
			reAdd = true
			reAddNeedsConfigWrite = true
			break
		}
		if !filepath.IsAbs(existPath) {
			existPath = filepath.Join(cityPath, existPath)
		}
		if filepath.Clean(existPath) != filepath.Clean(rigPath) {
			return config.Rig{}, result, fmt.Errorf("rig %q already registered at %s (not %s)", name, r.Path, rigPath)
		}
		reAdd = true
		break
	}

	// Step 7: prefix resolution, collision checks, default-branch backfill.
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
			return config.Rig{}, result, fmt.Errorf("rig %q: prefix %q collides with HQ. Use --prefix to specify a different prefix.", name, prefixKey) //nolint:revive,staticcheck // byte-identical rig-add collision text (trailing period)
		}
		for _, rg := range cfg.Rigs {
			if prefixKey == strings.ToLower(rg.EffectivePrefix()) {
				return config.Rig{}, result, fmt.Errorf("rig %q: prefix %q collides with %s. Use --prefix to specify a different prefix.", name, prefixKey, rg.Name) //nolint:revive,staticcheck // byte-identical rig-add collision text (trailing period)
			}
		}
	}
	if reAdd && existingRig != nil && existingRig.EffectiveDefaultBranch() == "" && resolvedDefaultBranch != "" {
		reAddNeedsConfigWrite = true
	}

	// Step 8: build nextCfg.
	nextCfg := cfg
	var defaultRigImports []config.BoundImport
	needsValidation := !reAdd || reAddNeedsConfigWrite
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
				return config.Rig{}, result, fmt.Errorf("loading root pack defaults: %w", err)
			}
			// Default-rig imports take the same pin/cache hardening as
			// explicit --include imports: a version-less bundled source
			// arriving from root-pack defaults or legacy
			// default_rig_includes must not persist version-less.
			defaultRigImports, commitRigImports, err = deps.ComposePacks(cityPath, composeDefaultRigImports(rootDefaultRigImports, cfg.Workspace.LegacyDefaultRigIncludes(), cfg.Packs))
			if err != nil {
				return config.Rig{}, result, fmt.Errorf("installing bundled rig imports: %w", err)
			}
			if len(defaultRigImports) > 0 {
				addedRig.Imports = boundImportsMap(defaultRigImports)
			}
		}
		next := *cfg
		next.Rigs = append(append([]config.Rig{}, cfg.Rigs...), addedRig)
		nextCfg = &next
	}

	// Step 9: validate rigs before any filesystem mutation.
	if needsValidation {
		if err := config.ValidateRigs(nextCfg.Rigs, config.EffectiveHQPrefix(nextCfg)); err != nil {
			return config.Rig{}, result, err
		}
	}

	// Step 10: create the rig directory when missing.
	if !rigPathExists {
		if err := fs.MkdirAll(rigPath, 0o755); err != nil {
			return config.Rig{}, result, fmt.Errorf("creating %s: %w", rigPath, err)
		}
	}

	// Step 11: adopt validation, prefix-mismatch guard, fresh-add store guard.
	if req.Adopt {
		metaPath := filepath.Join(rigPath, ".beads", "metadata.json")
		if _, err := fs.Stat(metaPath); err != nil {
			return config.Rig{}, result, fmt.Errorf("--adopt requires .beads/metadata.json in %s", rigPath)
		}
		if _, ok := ReadBeadsPrefix(fs, rigPath); !ok {
			return config.Rig{}, result, fmt.Errorf("--adopt requires a valid issue_prefix in .beads/config.yaml in %s", rigPath)
		}
	}

	if existingPrefix, ok := ReadBeadsPrefix(fs, rigPath); ok && existingPrefix != prefix {
		switch {
		case reAdd:
			// On re-add, --prefix is ignored (we use the existing rig's
			// configured prefix). Direct the user to edit city.toml.
			return config.Rig{}, result, fmt.Errorf("rig %q has bead prefix %q but city.toml has %q; "+
				"edit city.toml to set prefix = %q, or remove %s/.beads to reinitialize",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		case req.Adopt:
			// On --adopt, the user explicitly wants the existing store.
			// "Remove .beads to reinitialize" is the wrong recovery here:
			// nudge them toward matching the existing prefix instead.
			return config.Rig{}, result, fmt.Errorf("--adopt: rig %q already has bead prefix %q (requested %q); "+
				"use --prefix %s (or omit --prefix) to match the existing store",
				name, existingPrefix, prefix, existingPrefix)
		default:
			return config.Rig{}, result, fmt.Errorf("rig %q already has bead prefix %q (requested %q); "+
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
	if !reAdd && !req.Adopt {
		beadsPath := filepath.Join(rigPath, ".beads")
		fi, err := fs.Stat(beadsPath)
		if err != nil && !os.IsNotExist(err) {
			return config.Rig{}, result, fmt.Errorf("checking %s: %w", beadsPath, err)
		}
		if err == nil && fi.IsDir() {
			containsStore, containsErr := beadsDirContainsStore(fs, beadsPath)
			if containsErr != nil {
				return config.Rig{}, result, containsErr
			}
			if containsStore {
				return config.Rig{}, result, fmt.Errorf("%s/.beads already contains a beads store; "+
					"use --adopt to register it, or remove %s/.beads to reinitialize",
					rigPath, rigPath)
			}
		}
	}

	// --- Phase 1: Infrastructure (all fallible, before touching city.toml) ---

	// Step 12: banner + warn lines.
	if reAdd {
		emit(ProvisionStep{Name: "banner", Detail: fmt.Sprintf("Re-initializing rig '%s'...", name)})
		if req.StartSuspended && req.StartSuspended != existingRig.EffectiveSuspendedOnStart() {
			emit(ProvisionStep{Name: "start-suspended-ignored", Warn: true, Detail: fmt.Sprintf("warning: --start-suspended ignored (existing: suspended_on_start=%v); edit city.toml to change", existingRig.EffectiveSuspendedOnStart())})
		}
		if len(explicitRigImports) > 0 {
			existingRigImports, err := effectiveRigBoundImports(existingRig, cfg.Packs)
			if err != nil {
				emit(ProvisionStep{Name: "include-ignored", Warn: true, Detail: fmt.Sprintf("warning: --include flags %v ignored; existing rig imports could not be normalized (%v). Edit city.toml to change", includes, err)})
			} else if !slices.Equal(existingRigImports, explicitRigImports) {
				emit(ProvisionStep{Name: "include-ignored", Warn: true, Detail: fmt.Sprintf("warning: --include flags %v ignored (existing imports: %s); edit city.toml to change", includes, formatBoundImports(existingRigImports))})
			}
		}
		if req.Prefix != "" && strings.ToLower(req.Prefix) != existingRig.EffectivePrefix() {
			emit(ProvisionStep{Name: "prefix-ignored", Warn: true, Detail: fmt.Sprintf("warning: --prefix=%s ignored (existing: %s); edit city.toml to change", req.Prefix, existingRig.EffectivePrefix())})
		}
		if defaultBranchOverride != "" &&
			defaultBranchOverride != existingRig.EffectiveDefaultBranch() &&
			(existingRig.EffectiveDefaultBranch() != "" || resolvedDefaultBranch != defaultBranchOverride) {
			emit(ProvisionStep{Name: "default-branch-ignored", Warn: true, Detail: fmt.Sprintf("warning: --default-branch=%s ignored (existing: %s); edit city.toml to change", defaultBranchOverride, existingRig.EffectiveDefaultBranch())})
		}
	} else {
		emit(ProvisionStep{Name: "banner", Detail: fmt.Sprintf("Adding rig '%s'...", name)})
	}
	if hasGit {
		emit(ProvisionStep{Name: "git-detected", Detail: fmt.Sprintf("  Detected git repo at %s", rigPath)})
	}
	emit(ProvisionStep{Name: "prefix", Detail: fmt.Sprintf("  Prefix: %s", prefix)})
	if !reAdd && resolvedDefaultBranch != "" {
		emit(ProvisionStep{Name: "default-branch", Detail: fmt.Sprintf("  Default branch: %s", resolvedDefaultBranch)})
	}
	if !reAdd {
		switch {
		case len(explicitRigImports) > 0:
			emit(ProvisionStep{Name: "imports", Detail: fmt.Sprintf("  Import: %s", formatBoundImports(explicitRigImports))})
		default:
			if len(defaultRigImports) > 0 {
				emit(ProvisionStep{Name: "imports", Detail: fmt.Sprintf("  Import: %s (default)", formatBoundImports(defaultRigImports))})
			}
		}
	}

	// Step 13: beads-store init.
	deferred := false
	if req.Adopt {
		if deps.PrepareAdopt != nil {
			if err := deps.PrepareAdopt(cityPath, rigPath); err != nil {
				return config.Rig{}, result, fmt.Errorf("prepare adopted rig store: %w", err)
			}
		}
		if storeContract() {
			deferred, err = deps.InitStore(cityPath, rigPath, prefix)
			if err != nil {
				return config.Rig{}, result, err
			}
		}
		emit(ProvisionStep{Name: "beads-init", Detail: "  Adopted existing beads database"})
	} else {
		deferred, err = deps.InitStore(cityPath, rigPath, prefix)
		if err != nil {
			return config.Rig{}, result, err
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
	}
	result.Deferred = deferred

	// Step 14: snapshot the topology before the first config write.
	snapshots, err := SnapshotTopologyFiles(fs, cityPath, nextCfg)
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

	// Step 15: guarded config write.
	if !reAdd || reAddNeedsConfigWrite {
		if deps.NormalizeScopes == nil {
			return config.Rig{}, result, depErr("NormalizeScopes")
		}
		if err := deps.NormalizeScopes(cityPath, nextCfg); err != nil {
			return config.Rig{}, result, rollbackError(fs, snapshots, "canonicalizing rig topology", err)
		}

		var writeErr error
		if !reAdd {
			// Surgical append: preserve existing comments by appending only the
			// new [[rigs]] block instead of re-serializing the whole file.
			newRig := nextCfg.Rigs[len(nextCfg.Rigs)-1]
			writeErr = config.AppendRigAndWriteSiteBindingsForEdit(fs, tomlPath, nextCfg, newRig)
		} else {
			writeErr = config.WriteCityAndRigSiteBindingsForEdit(fs, tomlPath, nextCfg)
		}
		if writeErr != nil {
			return config.Rig{}, result, rollbackError(fs, snapshots, "writing config", writeErr)
		}
	}

	// Step 16: persist packs.lock and materialize bundled rig imports only after
	// the city config write succeeds, so the lockfile honors the same
	// "city.toml written last" contract: any earlier failure leaves packs.lock
	// untouched, and a failure here rolls back through the snapshot (which now
	// covers packs.lock).
	if commitRigImports != nil {
		if err := commitRigImports(); err != nil {
			return config.Rig{}, result, rollbackError(fs, snapshots, "installing bundled rig imports", err)
		}
	}
	cfg = nextCfg

	if err := deps.WriteRoutes(cityPath, cfg); err != nil {
		return config.Rig{}, result, rollbackError(fs, snapshots, "writing routes", err)
	}
	committed = true
	emit(ProvisionStep{Name: "routes", Detail: "  Generated routes.jsonl for cross-rig routing"})

	// Resolve the returned rig from the post-write config (a fresh add returns
	// the stored, possibly-empty prefix, not the effective one). The fallback
	// mirrors the constructed rig for the unreachable-in-practice miss.
	resultRig := config.Rig{
		Name:          name,
		Path:          rigPath,
		Prefix:        strings.ToLower(req.Prefix),
		DefaultBranch: resolvedDefaultBranch,
		Suspended:     req.StartSuspended,
	}
	for _, rg := range cfg.Rigs {
		if rg.Name == name {
			resultRig = rg
			break
		}
	}

	// Step 17: caller-specific side effects (CLI hooks/formulas/.env/reload).
	// Its error does not roll back — the disk writes are committed — but it is
	// captured so an API caller can surface a failed mutateAndPoke.
	if deps.PostProvision != nil {
		result.PostProvisionErr = deps.PostProvision(ProvisionContext{
			RigPath:  rigPath,
			Rig:      resultRig,
			Deferred: deferred,
			Cfg:      nextCfg,
		})
	}

	switch {
	case reAdd:
		emit(ProvisionStep{Name: "done", Detail: "Rig re-initialized."})
	case req.StartSuspended:
		emit(ProvisionStep{Name: "done", Detail: "Rig added (suspended — use 'gc rig resume' to activate)."})
	default:
		emit(ProvisionStep{Name: "done", Detail: "Rig added."})
	}

	return resultRig, result, nil
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
