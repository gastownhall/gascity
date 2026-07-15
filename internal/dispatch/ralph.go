package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// maxCheckInfraRetries bounds how many times a ralph check gate may be re-run
// because it could not EXECUTE (GateError/GateTimeout) before that infra error
// is treated as a genuine failure. Infra re-runs do NOT burn a gc.attempt, so a
// transport/store outage cannot exhaust a PR's ralph attempts and abort_scope a
// green PR (maintainer-city incident: 3 attempts burned in one outage). The
// bound guarantees a gate that can never run (a missing script, a perpetual
// timeout) still terminates the workflow instead of pending forever. The
// counter is cloned into each next attempt, so this is the ralph loop's total
// infra-retry budget; at a ~15s reconcile cadence it rides a multi-minute
// outage.
const maxCheckInfraRetries = 20

func processRalphCheck(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	if bead.Metadata[beadmeta.TerminalMetadataKey] == "true" {
		return ControlResult{}, nil
	}
	if bead.Metadata[beadmeta.CheckModeMetadataKey] != beadmeta.CheckModeExec {
		return ControlResult{}, fmt.Errorf("%s: unsupported check mode %q", bead.ID, bead.Metadata[beadmeta.CheckModeMetadataKey])
	}

	attempt, err := strconv.Atoi(bead.Metadata[beadmeta.AttemptMetadataKey])
	if err != nil || attempt < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.attempt %q", bead.ID, bead.Metadata[beadmeta.AttemptMetadataKey])
	}
	maxAttempts, err := strconv.Atoi(bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	if err != nil || maxAttempts < 1 {
		return ControlResult{}, fmt.Errorf("%s: invalid gc.max_attempts %q", bead.ID, bead.Metadata[beadmeta.MaxAttemptsMetadataKey])
	}

	logicalID := resolveLogicalBeadID(store, bead)
	if logicalID == "" {
		return ControlResult{}, fmt.Errorf("%s: could not resolve logical bead ID", bead.ID)
	}

	subjectID, err := resolveBlockingSubjectID(store, bead.ID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving subject: %w", bead.ID, err)
	}
	subject, err := store.Get(subjectID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading subject %s: %w", bead.ID, subjectID, err)
	}

	result, err := runRalphCheck(store, bead, subject, attempt, opts)
	if err != nil {
		return ControlResult{}, err
	}
	opts.tracef("ralph check-result bead=%s logical=%s attempt=%d outcome=%s exit=%s dur=%s truncated=%v stderr=%q stdout=%q",
		bead.ID, logicalID, attempt, result.Outcome, formatGateExitCode(result.ExitCode), result.Duration, result.Truncated,
		traceClipString(result.Stderr, traceCheckOutputCap), traceClipString(result.Stdout, traceCheckOutputCap))
	if err := persistCheckResult(store, bead.ID, result); err != nil {
		return ControlResult{}, fmt.Errorf("%s: persisting check result: %w", bead.ID, err)
	}

	// Gate-exec infra errors must not burn a ralph attempt. GateError (the gate
	// could not run) and GateTimeout (the gate did not finish) mean the gate
	// never produced a verdict; only a gate that ran to completion and returned
	// GateFail is a real failure. Re-run the gate via the benign
	// ErrControlPending path (no attempt increment, no close), bounded by
	// maxCheckInfraRetries so an unrunnable gate still terminates.
	if result.Outcome == convergence.GateError || result.Outcome == convergence.GateTimeout {
		infraRetries, _ := strconv.Atoi(bead.Metadata[beadmeta.CheckInfraRetryMetadataKey])
		if infraRetries < maxCheckInfraRetries {
			if err := store.SetMetadata(bead.ID, beadmeta.CheckInfraRetryMetadataKey, strconv.Itoa(infraRetries+1)); err != nil {
				if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
					return ControlResult{}, ErrControlPending
				}
				return ControlResult{}, fmt.Errorf("%s: recording gate infra-retry: %w", bead.ID, err)
			}
			opts.tracef("ralph check-infra-retry bead=%s outcome=%s infra_retry=%d/%d attempt=%d (attempt not burned)",
				bead.ID, result.Outcome, infraRetries+1, maxCheckInfraRetries, attempt)
			return ControlResult{}, ErrControlPending
		}
		// Infra-retry budget spent: fall through to the normal exhaust/retry
		// path so a gate that never becomes runnable still terminates the
		// workflow rather than pending forever.
		opts.tracef("ralph check-infra-exhausted bead=%s outcome=%s infra_retry=%d attempt=%d (falling through)",
			bead.ID, result.Outcome, infraRetries, attempt)
	}

	if result.Outcome == convergence.GatePass {
		if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing passed check: %w", bead.ID, err)
		}
		if outputJSON := subject.Metadata[beadmeta.OutputJSONMetadataKey]; outputJSON != "" {
			if err := store.SetMetadata(logicalID, beadmeta.OutputJSONMetadataKey, outputJSON); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating gc.output_json to logical bead: %w", logicalID, err)
			}
		}
		if err := setOutcomeAndClose(store, logicalID, beadmeta.OutcomePass); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "pass"}, nil
	}

	// A hard-class subject failure is terminal: stop the loop immediately in a
	// single attempt instead of cloning further attempts (the treadmill that
	// abort_scope-killed molecules). This mirrors the retry dispatcher's explicit
	// hard disposition (see processRetryEval in retry.go) but deliberately
	// diverges on the empty class: classifyRetryAttempt maps an empty
	// gc.failure_class to hard (retry.go: `case beadmeta.FailureClassHard, "":`),
	// whereas this loop keeps an empty or transient class repairable and clones up
	// to gc.max_attempts below. Only an explicit "hard" class terminates here.
	if subject.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomeFail &&
		strings.TrimSpace(subject.Metadata[beadmeta.FailureClassMetadataKey]) == beadmeta.FailureClassHard {
		if err := store.SetMetadataBatch(logicalID, map[string]string{
			beadmeta.OutcomeMetadataKey:          beadmeta.OutcomeFail,
			beadmeta.FailedAttemptMetadataKey:    strconv.Itoa(attempt),
			beadmeta.FailureClassMetadataKey:     beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey:    retryFailureReason(subject),
			beadmeta.FinalDispositionMetadataKey: beadmeta.DispositionHardFail,
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking logical hard failure: %w", logicalID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomeFail); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed check: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, logicalID, beadmeta.OutcomeFail); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing hard-failed logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "hard-fail"}, nil
	}

	if attempt >= maxAttempts {
		if err := store.SetMetadataBatch(logicalID, map[string]string{
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailedAttemptMetadataKey: strconv.Itoa(attempt),
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: marking logical failure: %w", logicalID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomeFail); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing failed check: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, logicalID, beadmeta.OutcomeFail); err != nil {
			return ControlResult{}, fmt.Errorf("%s: closing failed logical bead: %w", logicalID, err)
		}
		return ControlResult{Processed: true, Action: "fail"}, nil
	}

	nextAttempt := attempt + 1
	switch bead.Metadata[beadmeta.RetryStateMetadataKey] {
	case "":
		opts.tracef("ralph retry-mark-spawning bead=%s next=%d", bead.ID, nextAttempt)
		if err := store.SetMetadataBatch(bead.ID, map[string]string{
			beadmeta.RetryStateMetadataKey:  beadmeta.SpawnStateSpawning,
			beadmeta.NextAttemptMetadataKey: strconv.Itoa(nextAttempt),
		}); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: recording retry spawn start: %w", bead.ID, err)
		}
	case beadmeta.SpawnStateSpawning:
		// Resume partial append below.
	case beadmeta.SpawnStateSpawned:
		// Resume finalization below without cloning again.
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported gc.retry_state %q", bead.ID, bead.Metadata[beadmeta.RetryStateMetadataKey])
	}
	if bead.Metadata[beadmeta.RetryStateMetadataKey] != beadmeta.SpawnStateSpawned {
		opts.tracef("ralph retry-append-start bead=%s next=%d", bead.ID, nextAttempt)
		if _, err := appendRalphRetry(store, logicalID, subject, bead, nextAttempt, opts); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: appending retry: %w", bead.ID, err)
		}
		opts.tracef("ralph retry-append-done bead=%s next=%d", bead.ID, nextAttempt)
		spawnedMetadata := map[string]string{
			beadmeta.RetryStateMetadataKey:  beadmeta.SpawnStateSpawned,
			beadmeta.NextAttemptMetadataKey: strconv.Itoa(nextAttempt),
		}
		clearControllerSpawnErrorMetadata(spawnedMetadata)
		if err := store.SetMetadataBatch(bead.ID, spawnedMetadata); err != nil {
			if controllerSpawnBoundaryPending(store, bead.ID, err, opts) {
				return ControlResult{}, ErrControlPending
			}
			return ControlResult{}, fmt.Errorf("%s: recording retry spawn complete: %w", bead.ID, err)
		}
	}
	opts.tracef("ralph retry-finalize-start bead=%s next=%d", bead.ID, nextAttempt)
	if err := finalizeRalphRetry(store, logicalID, bead.ID); err != nil {
		return ControlResult{}, fmt.Errorf("%s: finalizing retry: %w", bead.ID, err)
	}
	opts.tracef("ralph retry-finalize-done bead=%s next=%d", bead.ID, nextAttempt)
	return ControlResult{Processed: true, Action: "retry"}, nil
}

func runRalphCheck(store beads.Store, bead, subject beads.Bead, attempt int, opts ProcessOptions) (convergence.GateResult, error) {
	if subject.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomeFail {
		exitCode := 1
		return convergence.GateResult{
			Outcome:   convergence.GateFail,
			ExitCode:  &exitCode,
			Stderr:    fmt.Sprintf("attempt subject %s already failed", subject.ID),
			Truncated: false,
		}, nil
	}

	checkPath := bead.Metadata[beadmeta.CheckPathMetadataKey]
	if checkPath == "" {
		return convergence.GateResult{}, fmt.Errorf("%s: missing gc.check_path", bead.ID)
	}
	cityPath := opts.CityPath
	if cityPath == "" {
		cityPath = resolveInheritedMetadata(store, bead, beadmeta.CityPathMetadataKey)
	}
	if cityPath == "" {
		return convergence.GateResult{}, fmt.Errorf("%s: missing city path for exec check", bead.ID)
	}
	storePath := opts.StorePath
	if storePath == "" {
		storePath = cityPath
	}

	workDir := resolveInheritedMetadata(store, bead, beadmeta.LegacyWorkDirMetadataKey, beadmeta.WorkDirMetadataKey)
	resolvedWorkDir := ""
	if workDir != "" {
		if filepath.IsAbs(workDir) {
			resolvedWorkDir = filepath.Clean(workDir)
		} else {
			resolvedWorkDir = filepath.Clean(filepath.Join(storePath, workDir))
		}
		// work_dir is inherited from bead metadata. For relative check paths
		// it becomes the script resolution base, so it must remain under an
		// operator-controlled tree. Absolute check paths are validated against
		// trusted roots below; for those, work_dir is only the process cwd.
		if !filepath.IsAbs(checkPath) && !pathutil.PathWithin(cityPath, resolvedWorkDir) && !pathutil.PathWithin(storePath, resolvedWorkDir) {
			return convergence.GateResult{}, fmt.Errorf("%s: work_dir %q escapes both city and store roots", bead.ID, workDir)
		}
	}
	scriptBase := storePath
	if resolvedWorkDir != "" {
		scriptBase = resolvedWorkDir
	}
	// Pass cityPath and scriptBase as distinct envelope/base roles: in
	// gastownhall/gascity#2320 storePath (a rig subtree) was passed as both,
	// causing relative gc.check_path values to be looked up under the rig
	// tree even when the script lives in the city tree.
	trustedAbsRoots := ralphCheckTrustedAbsoluteRoots(cityPath, storePath, opts.FormulaSearchPaths)
	if filepath.IsAbs(checkPath) && !pathWithinAny(checkPath, trustedAbsRoots) {
		return convergence.GateResult{}, fmt.Errorf("%s: absolute gc.check_path %q escapes trusted roots", bead.ID, checkPath)
	}
	scriptPath, err := convergence.ResolveConditionPath(cityPath, scriptBase, checkPath)
	if err != nil && scriptBase != storePath && !filepath.IsAbs(checkPath) && errors.Is(err, fs.ErrNotExist) {
		// Pack-shipped check scripts live in the pack/city tree, not the
		// per-task gc.work_dir worktree, so a relative gc.check_path joined
		// against a work_dir worktree that lacks the pack tree resolves to a
		// nonexistent path (gastownhall/gascity#3008). Fall back to the
		// store/city root — exactly the base used when work_dir is empty, so
		// it introduces no new trusted root and stays subject to
		// ResolveConditionPath's containment checks. Only on a not-exist miss,
		// so a check that does exist under the worktree keeps precedence; the
		// original work_dir error is preserved when the fallback also misses.
		if fallbackPath, fallbackErr := convergence.ResolveConditionPath(cityPath, storePath, checkPath); fallbackErr == nil {
			scriptPath, err = fallbackPath, nil
		}
	}
	if err != nil {
		return convergence.GateResult{}, fmt.Errorf("%s: resolving check path: %w", bead.ID, err)
	}
	if filepath.IsAbs(checkPath) && !pathWithinAny(scriptPath, trustedAbsRoots) {
		return convergence.GateResult{}, fmt.Errorf("%s: resolved gc.check_path %q escapes trusted roots", bead.ID, scriptPath)
	}

	timeout := convergence.DefaultGateTimeout
	// Per-step timeout (from formula step.timeout) applies first as a
	// general override. The check-specific gc.check_timeout (from
	// ralph.check.timeout) takes precedence if also set.
	if raw := bead.Metadata[beadmeta.StepTimeoutMetadataKey]; raw != "" {
		parsed, parseErr := parsePositiveRalphTimeout(bead.ID, beadmeta.StepTimeoutMetadataKey, raw)
		if parseErr != nil {
			return convergence.GateResult{}, parseErr
		}
		timeout = parsed
	}
	if raw := bead.Metadata[beadmeta.CheckTimeoutMetadataKey]; raw != "" {
		parsed, parseErr := parsePositiveRalphTimeout(bead.ID, beadmeta.CheckTimeoutMetadataKey, raw)
		if parseErr != nil {
			return convergence.GateResult{}, parseErr
		}
		timeout = parsed
	}

	conditionBeadID := subject.ID
	pathBead := subject
	if conditionBeadID == "" {
		conditionBeadID = bead.ID
		pathBead = bead
	}
	// gastownhall/gascity#2522: ralph.check scripts read $GC_MOLECULE_DIR and
	// $GC_ARTIFACT_DIR to access the molecule-scoped working storage where
	// the per-attempt agent wrote its verdict. Resolve both from the same
	// bead we expose as GC_BEAD_ID (the subject/attempt, falling back to the
	// control bead) so the per-step artifact dir matches where that agent
	// wrote — using the bead's gc.root_bead_id metadata that
	// molecule.Instantiate stamps onto every member. Best-effort: when the
	// bead is not a molecule member (no root stamped) both stay empty and
	// the env vars are omitted, matching the sling-time GC_ARTIFACT_DIR
	// contract that pack scripts already handle.
	moleculeDir, artifactDir := resolveRalphCheckMoleculePaths(pathBead, cityPath)
	opts.tracef("ralph check-start bead=%s script=%s timeout=%s", bead.ID, scriptPath, timeout)
	result := convergence.RunCondition(context.Background(), scriptPath, convergence.ConditionEnv{
		BeadID:      conditionBeadID,
		Iteration:   attempt,
		CityPath:    cityPath,
		StorePath:   storePath,
		WorkDir:     resolvedWorkDir,
		MoleculeDir: moleculeDir,
		ArtifactDir: artifactDir,
	}, timeout, 0)
	opts.tracef("ralph check-done bead=%s outcome=%s dur=%s", bead.ID, result.Outcome, result.Duration)
	return result, nil
}

func ralphCheckTrustedAbsoluteRoots(cityPath, storePath string, formulaSearchPaths []string) []string {
	roots := make([]string, 0, 2+3*len(formulaSearchPaths))
	add := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		normalized := pathutil.NormalizePathForCompare(root)
		for _, existing := range roots {
			if pathutil.SamePath(existing, normalized) {
				return
			}
		}
		roots = append(roots, normalized)
	}
	add(cityPath)
	add(storePath)
	// Pack-authored checks may live beside a formula layer's formulas/ dir.
	for _, formulaPath := range formulaSearchPaths {
		formulaPath = strings.TrimSpace(formulaPath)
		if formulaPath == "" {
			continue
		}
		clean := filepath.Clean(formulaPath)
		add(clean)
		// formula.winningAssetPath resolves a step's "../assets/..." check path
		// to the layer's sibling assets/ tree, regardless of the layer dir's
		// name, so trust that sibling for every layer — a formula layer need not
		// be named "formulas" (e.g. a custom or absolute formulas_dir).
		add(filepath.Join(filepath.Dir(clean), "assets"))
		if filepath.Base(clean) == "formulas" {
			add(filepath.Dir(clean))
		}
	}
	return roots
}

func pathWithinAny(path string, roots []string) bool {
	for _, root := range roots {
		if pathutil.PathWithin(root, path) {
			return true
		}
	}
	return false
}

// resolveRalphCheckMoleculePaths derives the molecule root directory and the
// per-step artifact directory for a ralph bead. Both paths are derived from
// the bead's gc.root_bead_id metadata (stamped by molecule.Instantiate on
// every formula-scaffolded member). Returns empty strings when the bead is
// not a molecule member, when gc.root_bead_id is path-unsafe, or when the
// artifact dir cannot be created; the caller treats empty as "omit the env
// var", which matches the sling-time GC_ARTIFACT_DIR contract.
func resolveRalphCheckMoleculePaths(bead beads.Bead, cityPath string) (string, string) {
	if strings.TrimSpace(cityPath) == "" {
		return "", ""
	}
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		return "", ""
	}
	// Reject a path-traversing/unsafe gc.root_bead_id before joining it so
	// an unsafe root cannot surface a path-escaping GC_MOLECULE_DIR. This
	// mirrors the rejection molecule.EnsureArtifactDir applies to rootID and
	// keeps the omit-on-unsafe contract used by the sling env path.
	if molecule.ValidateMemberID(rootID) != nil {
		return "", ""
	}
	moleculeDir := molecule.Dir(cityPath, rootID)
	artifactDir, err := molecule.EnsureArtifactDir(fsys.OSFS{}, cityPath, rootID, bead.ID)
	if err != nil {
		// rootID is already validated, so EnsureArtifactDir failed either
		// on the per-step bead ID or on mkdir (e.g. permissions). Surface
		// the (safe) molecule root so check scripts that only need
		// GC_MOLECULE_DIR still work; the artifact-dir omission mirrors the
		// sling-time best-effort contract.
		return moleculeDir, ""
	}
	return moleculeDir, artifactDir
}

func parsePositiveRalphTimeout(beadID, key, raw string) (time.Duration, error) {
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: parsing %s %q: %w", beadID, key, raw, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s: %s must be positive, got %v", beadID, key, parsed)
	}
	return parsed, nil
}

func persistCheckResult(store beads.Store, beadID string, result convergence.GateResult) error {
	batch := map[string]string{
		beadmeta.OutcomeMetadataKey:    result.Outcome,
		beadmeta.StdoutMetadataKey:     result.Stdout,
		beadmeta.StderrMetadataKey:     result.Stderr,
		beadmeta.DurationMsMetadataKey: strconv.FormatInt(result.Duration.Milliseconds(), 10),
		beadmeta.TruncatedMetadataKey:  strconv.FormatBool(result.Truncated),
	}
	if result.ExitCode != nil {
		batch[beadmeta.ExitCodeMetadataKey] = strconv.Itoa(*result.ExitCode)
	} else {
		batch[beadmeta.ExitCodeMetadataKey] = ""
	}
	return store.SetMetadataBatch(beadID, batch)
}

func appendRalphRetry(store beads.Store, logicalID string, prevSubject, prevCheck beads.Bead, nextAttempt int, opts ProcessOptions) (map[string]string, error) {
	var rootBeads []beads.Bead
	rootID := prevSubject.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID != "" {
		var err error
		rootBeads, err = listByWorkflowRoot(store, rootID)
		if err != nil {
			return nil, err
		}
	}

	attemptSet, err := collectRalphAttemptBeadsFromBeads(rootBeads, prevSubject)
	if err != nil {
		return nil, err
	}

	oldAttempt, _ := strconv.Atoi(prevSubject.Metadata[beadmeta.AttemptMetadataKey])
	oldScopeRef := prevSubject.Metadata[beadmeta.StepRefMetadataKey]
	if oldScopeRef == "" {
		oldScopeRef = prevSubject.ID
	}
	newScopeRef := rewriteRalphAttemptRef(oldScopeRef, oldAttempt, nextAttempt)
	if newScopeRef == oldScopeRef && prevSubject.Metadata[beadmeta.StepRefMetadataKey] == "" {
		newScopeRef = fmt.Sprintf("%s.retry.%d", prevSubject.ID, nextAttempt)
	}
	if existing, err := resolveExistingRalphRetryFromBeads(store, rootBeads, logicalID, prevSubject, prevCheck, attemptSet, oldAttempt, nextAttempt, oldScopeRef, newScopeRef); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		if newCheckID := existing[prevCheck.ID]; newCheckID != "" {
			if err := store.DepAdd(logicalID, newCheckID, "blocks"); err != nil {
				return nil, fmt.Errorf("restoring logical->check dep: %w", err)
			}
		}
		return existing, nil
	}
	// A routeConfig error is intentionally tolerated here: Ralph retry preserves
	// the prior attempt's already-stamped routes rather than scope-routing, so a
	// nil cfg degrades to metadata-only instead of mis-routing. Spawn/fanout
	// (control.go, fanout.go) fail closed on this error because they scope-route
	// through applyAttemptControlStepRoute.
	cfg, _ := opts.routeConfig()
	if molecule.IsGraphApplyEnabled() {
		if applier, ok := beads.GraphApplyFor(store); ok {
			return appendRalphRetryViaGraphApply(store, applier, logicalID, prevSubject, prevCheck, attemptSet, oldAttempt, nextAttempt, oldScopeRef, newScopeRef, cfg, opts)
		}
	}
	return appendRalphRetryLegacy(store, logicalID, prevSubject, prevCheck, attemptSet, oldAttempt, nextAttempt, oldScopeRef, newScopeRef, cfg)
}

func appendRalphRetryLegacy(store beads.Store, logicalID string, prevSubject, prevCheck beads.Bead, attemptSet map[string]beads.Bead, oldAttempt, nextAttempt int, oldScopeRef, newScopeRef string, cfg *config.City) (map[string]string, error) {
	mapping := make(map[string]string, len(attemptSet)+1)
	pendingAssignees := make(map[string]string, len(attemptSet)+1)

	ordered := make([]beads.Bead, 0, len(attemptSet))
	for _, bead := range attemptSet {
		ordered = append(ordered, bead)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
	})

	// Create the subject first so scope_ref remapping is stable for nested attempts.
	subjectMeta := cloneMetadata(prevSubject.Metadata)
	clearRetryEphemera(subjectMeta)
	subjectMeta[beadmeta.AttemptMetadataKey] = strconv.Itoa(nextAttempt)
	subjectMeta[beadmeta.RetryFromMetadataKey] = prevSubject.ID
	subjectMeta[beadmeta.LogicalBeadIDMetadataKey] = logicalID
	subjectMeta[beadmeta.StepRefMetadataKey] = rewriteRetryStepRef(prevSubject.Metadata, prevSubject.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	if controlFor := strings.TrimSpace(subjectMeta[beadmeta.ControlForMetadataKey]); controlFor != "" {
		subjectMeta[beadmeta.ControlForMetadataKey] = rewriteRetryControlFor(subjectMeta, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	}
	subjectAssignee := retryPreservedAssigneeWithConfig(prevSubject, cfg)
	if subjectAssignee == "" {
		clearSessionAffinityMetadata(subjectMeta)
	}
	newSubject, err := store.Create(beads.Bead{
		Title:       prevSubject.Title,
		Description: prevSubject.Description,
		Type:        prevSubject.Type,
		Ref:         cloneRef(subjectMeta, prevSubject.Ref),
		ParentID:    prevSubject.ParentID,
		Assignee:    "",
		Labels:      removeAttemptPoolLabels(prevSubject.Labels),
		Metadata:    subjectMeta,
	})
	if err != nil {
		return nil, err
	}
	mapping[prevSubject.ID] = newSubject.ID
	if subjectAssignee != "" {
		pendingAssignees[prevSubject.ID] = subjectAssignee
	}

	for _, old := range ordered {
		if old.ID == prevSubject.ID {
			continue
		}
		meta := cloneMetadata(old.Metadata)
		clearRetryEphemera(meta)
		meta[beadmeta.AttemptMetadataKey] = strconv.Itoa(nextAttempt)
		meta[beadmeta.RetryFromMetadataKey] = old.ID
		if currentScopeRef := strings.TrimSpace(meta[beadmeta.ScopeRefMetadataKey]); currentScopeRef != "" {
			meta[beadmeta.ScopeRefMetadataKey] = rewriteRetryScopeRef(currentScopeRef, oldScopeRef, newScopeRef, prevSubject.ID)
		}
		meta[beadmeta.StepRefMetadataKey] = rewriteRetryStepRef(meta, old.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
		if controlFor := strings.TrimSpace(meta[beadmeta.ControlForMetadataKey]); controlFor != "" {
			meta[beadmeta.ControlForMetadataKey] = rewriteRetryControlFor(meta, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
		}
		preservedAssignee := retryPreservedAssigneeWithConfig(old, cfg)
		if preservedAssignee == "" {
			clearSessionAffinityMetadata(meta)
		}
		created, err := store.Create(beads.Bead{
			Title:       old.Title,
			Description: old.Description,
			Type:        old.Type,
			Ref:         cloneRef(meta, old.Ref),
			ParentID:    old.ParentID,
			Assignee:    "",
			Labels:      removeAttemptPoolLabels(old.Labels),
			Metadata:    meta,
		})
		if err != nil {
			return nil, err
		}
		mapping[old.ID] = created.ID
		if preservedAssignee != "" {
			pendingAssignees[old.ID] = preservedAssignee
		}
	}

	checkMeta := cloneMetadata(prevCheck.Metadata)
	clearRetryEphemera(checkMeta)
	checkMeta[beadmeta.AttemptMetadataKey] = strconv.Itoa(nextAttempt)
	checkMeta[beadmeta.RetryFromMetadataKey] = prevCheck.ID
	checkMeta[beadmeta.TerminalMetadataKey] = ""
	checkMeta[beadmeta.LogicalBeadIDMetadataKey] = logicalID
	checkMeta[beadmeta.StepRefMetadataKey] = rewriteRetryStepRef(checkMeta, prevCheck.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	if controlFor := strings.TrimSpace(checkMeta[beadmeta.ControlForMetadataKey]); controlFor != "" {
		checkMeta[beadmeta.ControlForMetadataKey] = rewriteRetryControlFor(checkMeta, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	}
	checkAssignee := retryPreservedAssigneeWithConfig(prevCheck, cfg)
	if checkAssignee == "" {
		clearSessionAffinityMetadata(checkMeta)
	}
	newCheck, err := store.Create(beads.Bead{
		Title:       prevCheck.Title,
		Description: prevCheck.Description,
		Type:        prevCheck.Type,
		Ref:         cloneRef(checkMeta, prevCheck.Ref),
		ParentID:    prevCheck.ParentID,
		Assignee:    "",
		Labels:      removeAttemptPoolLabels(prevCheck.Labels),
		Metadata:    checkMeta,
	})
	if err != nil {
		return nil, err
	}
	mapping[prevCheck.ID] = newCheck.ID
	if checkAssignee != "" {
		pendingAssignees[prevCheck.ID] = checkAssignee
	}

	for _, old := range ordered {
		newID := mapping[old.ID]
		if newID == "" {
			continue
		}
		if remapped := remappedLogicalBeadID(mapping, old.Metadata[beadmeta.LogicalBeadIDMetadataKey]); remapped != "" {
			if err := store.SetMetadata(newID, beadmeta.LogicalBeadIDMetadataKey, remapped); err != nil {
				return nil, fmt.Errorf("remapping logical bead for retry clone %s: %w", newID, err)
			}
		}
		if remapped := remappedControlForBeadID(mapping, old.Metadata[beadmeta.ControlForMetadataKey]); remapped != "" {
			if err := store.SetMetadata(newID, beadmeta.ControlForMetadataKey, remapped); err != nil {
				return nil, fmt.Errorf("remapping control_for for retry clone %s: %w", newID, err)
			}
		}
	}
	if remapped := remappedLogicalBeadID(mapping, prevCheck.Metadata[beadmeta.LogicalBeadIDMetadataKey]); remapped != "" {
		if err := store.SetMetadata(newCheck.ID, beadmeta.LogicalBeadIDMetadataKey, remapped); err != nil {
			return nil, fmt.Errorf("remapping logical bead for retry check %s: %w", newCheck.ID, err)
		}
	}
	if remapped := remappedControlForBeadID(mapping, prevCheck.Metadata[beadmeta.ControlForMetadataKey]); remapped != "" {
		if err := store.SetMetadata(newCheck.ID, beadmeta.ControlForMetadataKey, remapped); err != nil {
			return nil, fmt.Errorf("remapping control_for for retry check %s: %w", newCheck.ID, err)
		}
	}

	for _, old := range ordered {
		if err := copyRetryDeps(store, old.ID, mapping[old.ID], mapping); err != nil {
			return nil, err
		}
	}
	if err := copyRetryDeps(store, prevCheck.ID, newCheck.ID, mapping); err != nil {
		return nil, err
	}
	if err := store.DepAdd(logicalID, newCheck.ID, "blocks"); err != nil {
		return nil, fmt.Errorf("creating logical->check dep: %w", err)
	}
	for _, oldID := range sortedRetryAssigneeIDs(pendingAssignees) {
		assignee := pendingAssignees[oldID]
		newID := mapping[oldID]
		if assignee == "" || newID == "" {
			continue
		}
		if err := store.Update(newID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			return nil, fmt.Errorf("assigning retry bead %s: %w", newID, err)
		}
	}

	return mapping, nil
}

func appendRalphRetryViaGraphApply(store beads.Store, applier beads.GraphApplyStore, logicalID string, prevSubject, prevCheck beads.Bead, attemptSet map[string]beads.Bead, oldAttempt, nextAttempt int, oldScopeRef, newScopeRef string, cfg *config.City, opts ProcessOptions) (map[string]string, error) {
	ordered := make([]beads.Bead, 0, len(attemptSet))
	for _, bead := range attemptSet {
		ordered = append(ordered, bead)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
	})

	attemptIDs := make(map[string]bool, len(attemptSet)+1)
	for _, bead := range ordered {
		attemptIDs[bead.ID] = true
	}
	attemptIDs[prevCheck.ID] = true

	plan := &beads.GraphApplyPlan{
		CommitMessage: fmt.Sprintf("gc: ralph retry %s attempt %d", logicalID, nextAttempt),
		Nodes:         make([]beads.GraphApplyNode, 0, len(attemptSet)+1),
		Edges:         make([]beads.GraphApplyEdge, 0, len(attemptSet)*2),
	}

	plan.Nodes = append(plan.Nodes, buildRalphRetryGraphNode(prevSubject, logicalID, oldScopeRef, newScopeRef, oldAttempt, nextAttempt, attemptIDs, cfg))
	for _, old := range ordered {
		if old.ID == prevSubject.ID {
			continue
		}
		plan.Nodes = append(plan.Nodes, buildRalphRetryGraphNode(old, logicalID, oldScopeRef, newScopeRef, oldAttempt, nextAttempt, attemptIDs, cfg))
	}
	plan.Nodes = append(plan.Nodes, buildRalphRetryGraphNode(prevCheck, logicalID, oldScopeRef, newScopeRef, oldAttempt, nextAttempt, attemptIDs, cfg))

	for _, old := range ordered {
		if err := appendRalphRetryGraphEdges(plan, store, old.ID, attemptIDs); err != nil {
			return nil, err
		}
	}
	if err := appendRalphRetryGraphEdges(plan, store, prevCheck.ID, attemptIDs); err != nil {
		return nil, err
	}
	plan.Edges = append(plan.Edges, beads.GraphApplyEdge{
		FromID: logicalID,
		ToKey:  prevCheck.ID,
		Type:   "blocks",
	})

	opts.tracef("ralph retry-graph-apply-start logical=%s next=%d nodes=%d edges=%d", logicalID, nextAttempt, len(plan.Nodes), len(plan.Edges))
	applied, err := applier.ApplyGraphPlan(context.Background(), plan)
	if err != nil {
		return nil, err
	}
	if err := beads.ValidateGraphApplyResult(plan, applied); err != nil {
		return nil, err
	}
	opts.tracef("ralph retry-graph-apply-done logical=%s next=%d nodes=%d", logicalID, nextAttempt, len(applied.IDs))

	mapping := make(map[string]string, len(applied.IDs))
	for oldID, newID := range applied.IDs {
		mapping[oldID] = newID
	}
	return mapping, nil
}

func buildRalphRetryGraphNode(old beads.Bead, logicalID, oldScopeRef, newScopeRef string, oldAttempt, nextAttempt int, attemptIDs map[string]bool, cfg *config.City) beads.GraphApplyNode {
	meta := cloneMetadata(old.Metadata)
	clearRetryEphemera(meta)
	meta[beadmeta.AttemptMetadataKey] = strconv.Itoa(nextAttempt)
	meta[beadmeta.RetryFromMetadataKey] = old.ID
	if currentScopeRef := strings.TrimSpace(meta[beadmeta.ScopeRefMetadataKey]); currentScopeRef != "" {
		meta[beadmeta.ScopeRefMetadataKey] = rewriteRetryScopeRef(currentScopeRef, oldScopeRef, newScopeRef, old.ID)
	}
	meta[beadmeta.StepRefMetadataKey] = rewriteRetryStepRef(meta, old.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
	metadataRefs := map[string]string(nil)
	// gc.control_for: a bead-ID-valued pointer at a bead re-minted in this plan
	// is remapped to the clone's new ID via MetadataRefs (the applier
	// substitutes the created ID), mirroring gc.logical_bead_id below (S38 W7).
	// Step-ref-valued pointers stay on the string rewrite.
	if controlFor := strings.TrimSpace(meta[beadmeta.ControlForMetadataKey]); controlFor != "" {
		if attemptIDs[controlFor] {
			metadataRefs = make(map[string]string, 1)
			metadataRefs[beadmeta.ControlForMetadataKey] = controlFor
			delete(meta, beadmeta.ControlForMetadataKey)
		} else {
			meta[beadmeta.ControlForMetadataKey] = rewriteRetryControlFor(meta, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
		}
	}
	if oldLogicalID := strings.TrimSpace(old.Metadata[beadmeta.LogicalBeadIDMetadataKey]); oldLogicalID != "" {
		if attemptIDs[oldLogicalID] {
			if metadataRefs == nil {
				metadataRefs = make(map[string]string, 1)
			}
			metadataRefs[beadmeta.LogicalBeadIDMetadataKey] = oldLogicalID
			delete(meta, beadmeta.LogicalBeadIDMetadataKey)
		} else {
			meta[beadmeta.LogicalBeadIDMetadataKey] = oldLogicalID
		}
	} else if kind := meta[beadmeta.KindMetadataKey]; kind == beadmeta.KindScope || kind == beadmeta.KindCheck {
		meta[beadmeta.LogicalBeadIDMetadataKey] = logicalID
	}
	parentKey := ""
	parentID := old.ParentID
	if attemptIDs[old.ParentID] {
		parentKey = old.ParentID
		parentID = ""
	}
	assignee := retryPreservedAssigneeWithConfig(old, cfg)
	if assignee == "" {
		clearSessionAffinityMetadata(meta)
	}
	return beads.GraphApplyNode{
		Key:               old.ID,
		Title:             old.Title,
		Description:       old.Description,
		Type:              old.Type,
		Assignee:          assignee,
		AssignAfterCreate: assignee != "",
		From:              old.From,
		Labels:            removeAttemptPoolLabels(old.Labels),
		Metadata:          meta,
		MetadataRefs:      metadataRefs,
		ParentKey:         parentKey,
		ParentID:          parentID,
	}
}

func retryPreservedAssigneeWithConfig(bead beads.Bead, cfg *config.City) string {
	if bead.Assignee == "" {
		return ""
	}
	if beadUsesMetadataPoolRouteWithConfig(bead, cfg) {
		return ""
	}
	return bead.Assignee
}

func appendRalphRetryGraphEdges(plan *beads.GraphApplyPlan, store beads.Store, oldID string, attemptIDs map[string]bool) error {
	deps, err := store.DepList(oldID, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.Type == "parent-child" {
			continue
		}
		edge := beads.GraphApplyEdge{
			FromKey: oldID,
			Type:    dep.Type,
		}
		if attemptIDs[dep.DependsOnID] {
			edge.ToKey = dep.DependsOnID
		} else {
			edge.ToID = dep.DependsOnID
		}
		plan.Edges = append(plan.Edges, edge)
	}
	return nil
}

func finalizeRalphRetry(store beads.Store, logicalID, checkID string) error {
	if err := store.DepRemove(logicalID, checkID); err != nil {
		return err
	}
	check, err := store.Get(checkID)
	if err != nil {
		return err
	}
	if check.Status == "closed" {
		return nil
	}
	return setOutcomeAndClose(store, checkID, beadmeta.OutcomeFail)
}

func collectRalphAttemptBeads(store beads.Store, subject beads.Bead) (map[string]beads.Bead, error) {
	if subject.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindScope {
		return map[string]beads.Bead{subject.ID: subject}, nil
	}
	rootID := subject.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		return nil, fmt.Errorf("%s: missing gc.root_bead_id", subject.ID)
	}
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return nil, err
	}
	return collectRalphAttemptBeadsFromBeads(all, subject)
}

func collectRalphAttemptBeadsFromBeads(all []beads.Bead, subject beads.Bead) (map[string]beads.Bead, error) {
	out := map[string]beads.Bead{
		subject.ID: subject,
	}
	if subject.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindScope {
		return out, nil
	}
	scopeRef := subject.Metadata[beadmeta.StepRefMetadataKey]
	if scopeRef == "" {
		scopeRef = subject.ID
	}
	for _, bead := range all {
		if bead.Metadata[beadmeta.DynamicFragmentMetadataKey] == "true" {
			continue
		}
		if matchesRalphRetryScope(bead.Metadata[beadmeta.ScopeRefMetadataKey], scopeRef, subject.ID) {
			out[bead.ID] = bead
		}
	}
	return out, nil
}

func matchesRalphRetryScope(beadScopeRef, scopeRef, subjectID string) bool {
	beadScopeRef = strings.TrimSpace(beadScopeRef)
	if beadScopeRef == "" {
		return false
	}
	if beadScopeRef == scopeRef || beadScopeRef == subjectID {
		return true
	}
	return scopeRef != "" && strings.HasSuffix(scopeRef, "."+beadScopeRef)
}

func rewriteRetryScopeRef(beadScopeRef, oldScopeRef, newScopeRef, subjectID string) string {
	if !matchesRalphRetryScope(beadScopeRef, oldScopeRef, subjectID) {
		return beadScopeRef
	}
	return newScopeRef
}

func copyRetryDeps(store beads.Store, oldID, newID string, mapping map[string]string) error {
	deps, err := store.DepList(oldID, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.Type != "blocks" && dep.Type != "waits-for" && dep.Type != "conditional-blocks" {
			continue
		}
		targetID := dep.DependsOnID
		if mapped, ok := mapping[targetID]; ok {
			targetID = mapped
		} else {
			target, err := store.Get(dep.DependsOnID)
			if err != nil {
				return err
			}
			if target.Metadata[beadmeta.DynamicFragmentMetadataKey] == "true" {
				continue
			}
		}
		if err := store.DepAdd(newID, targetID, dep.Type); err != nil {
			return fmt.Errorf("copying dep %s->%s (%s): %w", newID, targetID, dep.Type, err)
		}
	}
	return nil
}

func resolveLogicalBeadID(store beads.Store, bead beads.Bead) string {
	if bead.Metadata[beadmeta.LogicalBeadIDMetadataKey] != "" {
		return bead.Metadata[beadmeta.LogicalBeadIDMetadataKey]
	}

	deps, err := store.DepList(bead.ID, "up")
	if err == nil {
		for _, dep := range deps {
			if dep.Type != "blocks" {
				continue
			}
			candidate, getErr := store.Get(dep.IssueID)
			if getErr != nil {
				continue
			}
			switch candidate.Metadata[beadmeta.KindMetadataKey] {
			case "ralph", "retry":
				return candidate.ID
			}
		}
	}
	if rootID := bead.Metadata[beadmeta.RootBeadIDMetadataKey]; rootID != "" {
		// Build candidate refs: scope-check controlled ref first (most specific),
		// then logicalStepRefForAttemptBead (may trim attempt patterns).
		var candidates []string
		if controlledRef := scopeCheckControlledStepRef(bead); controlledRef != "" {
			candidates = append(candidates, controlledRef)
		}
		if logicalRef := logicalStepRefForAttemptBead(bead); logicalRef != "" {
			alreadyHave := false
			for _, c := range candidates {
				if c == logicalRef {
					alreadyHave = true
					break
				}
			}
			if !alreadyHave {
				candidates = append(candidates, logicalRef)
			}
		}
		if len(candidates) > 0 {
			all, listErr := listByWorkflowRoot(store, rootID)
			if listErr == nil {
				for _, ref := range candidates {
					for _, candidate := range all {
						switch candidate.Metadata[beadmeta.KindMetadataKey] {
						case "ralph", "retry":
						default:
							continue
						}
						candidateRef := strings.TrimSpace(candidate.Metadata[beadmeta.StepRefMetadataKey])
						if candidateRef == "" {
							candidateRef = strings.TrimSpace(candidate.Ref)
						}
						if candidateRef == ref {
							return candidate.ID
						}
					}
				}
			}
		}
	}
	return ""
}

func logicalStepRefForAttemptBead(bead beads.Bead) string {
	stepRef := strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey])
	if stepRef == "" {
		stepRef = strings.TrimSpace(bead.Ref)
	}
	if stepRef == "" {
		return ""
	}
	kind := strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey])
	normalized := stepRef
	if kind == beadmeta.KindScopeCheck && strings.HasSuffix(normalized, "-scope-check") {
		normalized = strings.TrimSuffix(normalized, "-scope-check")
	}
	attempt := strings.TrimSpace(bead.Metadata[beadmeta.AttemptMetadataKey])
	if trimmed, ok := trimAttemptStepRefForKind(normalized, kind, attempt); ok {
		return trimmed
	}
	// For scope-check beads, prefer trimming attempt patterns from the
	// normalized ref (e.g., .eval.1 from a nested retry scope-check) to
	// resolve to the logical retry/ralph step. Fall back to normalized ref
	// for flat scope-checks that don't have attempt patterns.
	if kind == beadmeta.KindScopeCheck && normalized != stepRef {
		if trimmed, ok := trimRightmostAttemptStepRef(normalized); ok {
			return trimmed
		}
		return normalized
	}
	if trimmed, ok := trimRightmostAttemptStepRef(normalized); ok {
		return trimmed
	}
	return ""
}

func scopeCheckControlledStepRef(bead beads.Bead) string {
	if strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]) != beadmeta.KindScopeCheck {
		return ""
	}
	stepRef := strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey])
	if stepRef == "" {
		stepRef = strings.TrimSpace(bead.Ref)
	}
	if stepRef == "" || !strings.HasSuffix(stepRef, "-scope-check") {
		return ""
	}
	return strings.TrimSuffix(stepRef, "-scope-check")
}

func trimAttemptStepRefForKind(stepRef, kind, attempt string) (string, bool) {
	if attempt == "" {
		return "", false
	}
	switch kind {
	case "run", "scope", "retry-run":
		return trimAttemptStepRefSuffix(stepRef, ".run."+attempt)
	case "check":
		return trimAttemptStepRefSuffix(stepRef, ".check."+attempt)
	case "retry-eval":
		return trimAttemptStepRefSuffix(stepRef, ".eval."+attempt)
	default:
		return "", false
	}
}

func trimRightmostAttemptStepRef(stepRef string) (string, bool) {
	best := -1
	for _, prefix := range []string{".run.", ".check.", ".eval.", ".iteration.", ".attempt."} {
		if idx := strings.LastIndex(stepRef, prefix); idx > best {
			best = idx
		}
	}
	if best <= 0 {
		return "", false
	}
	return stepRef[:best], true
}

func trimAttemptStepRefSuffix(stepRef, suffix string) (string, bool) {
	if suffix == "" || !strings.HasSuffix(stepRef, suffix) {
		return "", false
	}
	return strings.TrimSuffix(stepRef, suffix), true
}

func resolveInheritedMetadata(store beads.Store, bead beads.Bead, keys ...string) string {
	current := bead
	visited := map[string]struct{}{}
	for {
		for _, key := range keys {
			if value := current.Metadata[key]; value != "" {
				return value
			}
		}
		if parentID := current.ParentID; parentID != "" {
			if _, seen := visited[parentID]; !seen {
				parent, err := store.Get(parentID)
				if err == nil {
					visited[parentID] = struct{}{}
					current = parent
					continue
				}
			}
		}
		rootID := current.Metadata[beadmeta.RootBeadIDMetadataKey]
		if rootID != "" && current.ID != rootID {
			if _, seen := visited[rootID]; !seen {
				parent, err := store.Get(rootID)
				if err == nil {
					visited[rootID] = struct{}{}
					current = parent
					continue
				}
			}
		}
		return ""
	}
}

func cloneMetadata(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}

func clearRetryEphemera(meta map[string]string) {
	if meta == nil {
		return
	}
	for _, key := range []string{
		beadmeta.OutcomeMetadataKey,
		beadmeta.ExitCodeMetadataKey,
		beadmeta.StdoutMetadataKey,
		beadmeta.StderrMetadataKey,
		beadmeta.OutputJSONMetadataKey,
		beadmeta.DurationMsMetadataKey,
		beadmeta.TruncatedMetadataKey,
		beadmeta.TerminalMetadataKey,
		beadmeta.FailedAttemptMetadataKey,
		beadmeta.FanoutStateMetadataKey,
		beadmeta.SpawnedCountMetadataKey,
		beadmeta.RetryStateMetadataKey,
		beadmeta.NextAttemptMetadataKey,
		beadmeta.PartialRetryMetadataKey,
		beadmeta.FailureClassMetadataKey,
		beadmeta.FailureReasonMetadataKey,
		beadmeta.FinalDispositionMetadataKey,
		beadmeta.ClosedByAttemptMetadataKey,
		beadmeta.LastFailureClassMetadataKey,
		beadmeta.RetrySessionRecycledMetadataKey,
		"review.verdict",
		"design_review.verdict",
		"code_review.verdict",
	} {
		delete(meta, key)
	}
}

func clearSessionAffinityMetadata(meta map[string]string) {
	if meta == nil {
		return
	}
	// Delete (rather than empty-string clear, as cmd/gc does) because this map
	// is handed to store.Create on the cloned attempt, where an absent key is
	// the natural representation of "no affinity".
	for _, key := range beadmeta.SessionAffinityMetadataKeys {
		delete(meta, key)
	}
}

func cloneRef(meta map[string]string, fallback string) string {
	if meta != nil && meta[beadmeta.StepRefMetadataKey] != "" {
		return meta[beadmeta.StepRefMetadataKey]
	}
	return fallback
}

func rewriteRetryStepRef(meta map[string]string, fallbackRef, oldScopeRef, newScopeRef string, oldAttempt, nextAttempt int) string {
	stepRef := fallbackRef
	if meta != nil && meta[beadmeta.StepRefMetadataKey] != "" {
		stepRef = meta[beadmeta.StepRefMetadataKey]
	}
	if stepRef == "" {
		return ""
	}
	if stepRef == oldScopeRef {
		return newScopeRef
	}
	if oldScopeRef != "" && strings.HasPrefix(stepRef, oldScopeRef+".") {
		return newScopeRef + strings.TrimPrefix(stepRef, oldScopeRef)
	}
	return rewriteRalphAttemptRef(stepRef, oldAttempt, nextAttempt)
}

func rewriteRetryControlRef(controlFor, oldScopeRef, newScopeRef string, oldAttempt, nextAttempt int) string {
	return rewriteRetryStepRef(map[string]string{beadmeta.StepRefMetadataKey: controlFor}, controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
}

func rewriteRetryControlFor(meta map[string]string, controlFor, oldScopeRef, newScopeRef string, oldAttempt, nextAttempt int) string {
	if kind := strings.TrimSpace(meta[beadmeta.KindMetadataKey]); kind == beadmeta.KindScopeCheck {
		if stepRef := strings.TrimSpace(meta[beadmeta.StepRefMetadataKey]); strings.HasSuffix(stepRef, "-scope-check") {
			return strings.TrimSuffix(stepRef, "-scope-check")
		}
	}
	return rewriteRetryControlRef(controlFor, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)
}

func remappedLogicalBeadID(mapping map[string]string, raw string) string {
	logicalID := strings.TrimSpace(raw)
	if logicalID == "" {
		return ""
	}
	if mapped := mapping[logicalID]; mapped != "" {
		return mapped
	}
	return logicalID
}

// remappedControlForBeadID returns the new bead ID for a bead-ID-valued
// gc.control_for pointer that referenced a bead re-minted in this retry clone
// (i.e. the old value is a mapping key). It returns "" for step-ref-valued
// pointers and for bead IDs outside the clone set — those keep the value
// produced by rewriteRetryControlFor at clone time. This mirrors the
// gc.logical_bead_id remap so cloned attempt roots point at the cloned
// nested control's NEW bead ID (S38 W6).
func remappedControlForBeadID(mapping map[string]string, raw string) string {
	controlFor := strings.TrimSpace(raw)
	if controlFor == "" {
		return ""
	}
	return mapping[controlFor]
}

func resolveExistingRalphRetryFromBeads(store beads.Store, all []beads.Bead, logicalID string, prevSubject, prevCheck beads.Bead, attemptSet map[string]beads.Bead, oldAttempt, nextAttempt int, oldScopeRef, newScopeRef string) (map[string]string, error) {
	rootID := prevSubject.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		return nil, fmt.Errorf("%s: missing gc.root_bead_id", prevSubject.ID)
	}

	expected := make(map[string]string, len(attemptSet)+1)
	expected[rewriteRetryStepRef(prevSubject.Metadata, prevSubject.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)] = prevSubject.ID
	for _, old := range attemptSet {
		if old.ID == prevSubject.ID {
			continue
		}
		expected[rewriteRetryStepRef(old.Metadata, old.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)] = old.ID
	}
	expected[rewriteRetryStepRef(prevCheck.Metadata, prevCheck.Ref, oldScopeRef, newScopeRef, oldAttempt, nextAttempt)] = prevCheck.ID

	mapping := make(map[string]string, len(expected))
	partial := make(map[string]beads.Bead, len(expected))
	for _, bead := range all {
		if bead.Metadata[beadmeta.PartialRetryMetadataKey] == "true" {
			continue
		}
		stepRef := bead.Metadata[beadmeta.StepRefMetadataKey]
		if stepRef == "" {
			continue
		}
		oldID, ok := expected[stepRef]
		if !ok {
			continue
		}
		if existing := mapping[oldID]; existing != "" && existing != bead.ID {
			return nil, fmt.Errorf("duplicate retry bead for %s (%s, %s)", stepRef, existing, bead.ID)
		}
		mapping[oldID] = bead.ID
		partial[bead.ID] = bead
	}

	switch {
	case len(mapping) == 0:
		return nil, nil
	case len(mapping) != len(expected):
		if err := discardPartialRalphRetry(store, partial); err != nil {
			return nil, fmt.Errorf("recovering partial retry append for %s: %w", prevSubject.ID, err)
		}
		return nil, nil
	default:
		complete, err := ralphRetryAppendComplete(store, logicalID, prevCheck.ID, attemptSet, mapping)
		if err != nil {
			return nil, err
		}
		if !complete {
			if err := discardPartialRalphRetry(store, partial); err != nil {
				return nil, fmt.Errorf("recovering incompletely wired retry append for %s: %w", prevSubject.ID, err)
			}
			return nil, nil
		}
		return mapping, nil
	}
}

func ralphRetryAppendComplete(store beads.Store, logicalID, prevCheckID string, attemptSet map[string]beads.Bead, mapping map[string]string) (bool, error) {
	newCheckID := mapping[prevCheckID]
	if newCheckID == "" {
		return false, nil
	}

	for _, old := range attemptSet {
		newID := mapping[old.ID]
		if newID == "" {
			return false, nil
		}
		if ok, err := copiedDepsPresent(store, old.ID, newID, mapping); err != nil {
			return false, err
		} else if !ok {
			return false, nil
		}
	}
	if ok, err := copiedDepsPresent(store, prevCheckID, newCheckID, mapping); err != nil {
		return false, err
	} else if !ok {
		return false, nil
	}
	for _, old := range attemptSet {
		newID := mapping[old.ID]
		if newID == "" {
			return false, nil
		}
		newBead, err := store.Get(newID)
		if err != nil {
			return false, err
		}
		if newBead.Assignee != old.Assignee {
			return false, nil
		}
	}
	newCheck, err := store.Get(newCheckID)
	if err != nil {
		return false, err
	}
	oldCheck, err := store.Get(prevCheckID)
	if err != nil {
		return false, err
	}
	if newCheck.Assignee != oldCheck.Assignee {
		return false, nil
	}

	deps, err := store.DepList(logicalID, "down")
	if err != nil {
		return false, err
	}
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == newCheckID {
			return true, nil
		}
	}
	return false, nil
}

func copiedDepsPresent(store beads.Store, oldID, newID string, mapping map[string]string) (bool, error) {
	oldDeps, err := store.DepList(oldID, "down")
	if err != nil {
		return false, err
	}
	newDeps, err := store.DepList(newID, "down")
	if err != nil {
		return false, err
	}
	for _, oldDep := range oldDeps {
		if oldDep.Type != "blocks" && oldDep.Type != "waits-for" && oldDep.Type != "conditional-blocks" {
			continue
		}
		targetID := oldDep.DependsOnID
		if mapped, ok := mapping[targetID]; ok {
			targetID = mapped
		} else {
			target, err := store.Get(oldDep.DependsOnID)
			if err != nil {
				return false, err
			}
			if target.Metadata[beadmeta.DynamicFragmentMetadataKey] == "true" {
				continue
			}
		}
		found := false
		for _, newDep := range newDeps {
			if newDep.Type == oldDep.Type && newDep.DependsOnID == targetID {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

func discardPartialRalphRetry(store beads.Store, partial map[string]beads.Bead) error {
	if len(partial) == 0 {
		return nil
	}

	pending := make(map[string]beads.Bead, len(partial))
	for id, bead := range partial {
		pending[id] = bead
	}

	for len(pending) > 0 {
		progress := false
		for _, id := range sortedPendingFragmentIDs(pending) {
			if !canDiscardPartialFragmentBead(store, id, pending) {
				continue
			}
			bead := pending[id]
			if err := detachIncomingDeps(store, id); err != nil {
				return err
			}
			if err := store.SetMetadataBatch(id, map[string]string{
				beadmeta.OutcomeMetadataKey:      beadmeta.OutcomeSkipped,
				beadmeta.PartialRetryMetadataKey: "true",
			}); err != nil {
				return err
			}
			if bead.Status != "closed" {
				if err := store.Close(id); err != nil {
					return fmt.Errorf("closing partial retry bead %s: %w", id, err)
				}
			}
			delete(pending, id)
			progress = true
		}
		if progress {
			continue
		}
		return fmt.Errorf("unable to discard partial retry beads: %v", sortedPendingFragmentIDs(pending))
	}

	return nil
}

func sortedRetryAssigneeIDs(pending map[string]string) []string {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func rewriteRalphAttemptRef(ref string, oldAttempt, nextAttempt int) string {
	if ref == "" || oldAttempt < 1 || nextAttempt < 1 {
		return ref
	}
	if rewritten, ok := rewriteAttemptSegment(ref, "run", oldAttempt, nextAttempt); ok {
		return rewritten
	}
	if rewritten, ok := rewriteAttemptSegment(ref, "check", oldAttempt, nextAttempt); ok {
		return rewritten
	}
	if rewritten, ok := rewriteAttemptSegment(ref, "iteration", oldAttempt, nextAttempt); ok {
		return rewritten
	}
	return ref
}

func rewriteAttemptSegment(ref, kind string, oldAttempt, nextAttempt int) (string, bool) {
	needle := "." + kind + "." + strconv.Itoa(oldAttempt)
	index := strings.LastIndex(ref, needle)
	if index < 0 {
		return "", false
	}
	end := index + len(needle)
	if end < len(ref) && ref[end] != '.' {
		return "", false
	}
	replacement := "." + kind + "." + strconv.Itoa(nextAttempt)
	return ref[:index] + replacement + ref[end:], true
}

// traceCheckOutputCap bounds stderr/stdout in the ralph check-result trace
// line so a noisy script does not produce an unreadable log entry.
// GateResult already truncates each stream to convergence.MaxOutputBytes
// (4 KiB); this further clips for tracing.
const traceCheckOutputCap = 512

// traceClipString returns s truncated to at most limit bytes, appending an
// ellipsis marker when truncation occurred. Used to keep ralph check-result
// trace lines bounded.
func traceClipString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "...[clipped]"
}

// formatGateExitCode renders a GateResult.ExitCode pointer for tracing.
// Avoids leaking the *int address (the prior trace line emitted %v against
// the pointer, producing `exit=0x...` instead of the numeric exit code).
func formatGateExitCode(code *int) string {
	if code == nil {
		return "<nil>"
	}
	return strconv.Itoa(*code)
}
