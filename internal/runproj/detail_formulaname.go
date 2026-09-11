package runproj

import "github.com/gastownhall/gascity/internal/beadmeta"

// resolveRunFormulaIdentityDetailState resolves a run root's formula name,
// provenance, and target for the 'detail'/'state' modes. Port of the
// non-lane path through TS resolveRunFormulaIdentity (formula-name.ts): the
// 'detail' and 'state' modes share every branch (the only mode-specific logic in
// resolveRunFormulaIdentity is the lane handling), so one function serves both.
// The bool mirrors `name: string | null`; target is "" when absent.
func resolveRunFormulaIdentityDetailState(root *runSnapshotBead, formulaDetailName string, hasFormulaDetail bool) (name, source, target string, hasName bool) {
	target = runFormulaTarget(root)

	if metadata := runFormulaMetadataNameRoot(root); metadata != "" {
		return metadata, "metadata", target, true
	}
	if hasFormulaDetail {
		if detailName := nonEmpty(formulaDetailName); detailName != "" {
			return detailName, "formula_detail", target, true
		}
	}
	if title, ok := runFormulaTitleFallbackDetail(root); ok {
		return title, "title_fallback", target, true
	}
	return "", "", target, false
}

// runFormulaMetadataNameRoot resolves the explicit formula name from root
// metadata. Port of the non-lane runFormulaMetadataName.
func runFormulaMetadataNameRoot(root *runSnapshotBead) string {
	if v := rootMetaPtr(root, beadmeta.FormulaMetadataKey); v != "" {
		return v
	}
	return rootMetaPtr(root, beadmeta.FormulaNameMetadataKey)
}

// runFormulaTitleFallbackDetail is the graph.v2 title fallback for the
// detail/state modes (no 'mol-' prefix gate, unlike lane mode). Port of
// runFormulaTitleFallback for non-lane modes.
func runFormulaTitleFallbackDetail(root *runSnapshotBead) (string, bool) {
	if root == nil {
		return "", false
	}
	if rootMetaPtr(root, beadmeta.FormulaContractMetadataKey) != "graph.v2" ||
		rootMetaPtr(root, beadmeta.RunTargetMetadataKey) == "" ||
		isTerminalRunRootStatus(root.status) {
		return "", false
	}
	title := nonEmpty(root.title)
	if title == "" {
		return "", false
	}
	return title, true
}

// runFormulaTarget resolves the run's routing target. Port of TS runFormulaTarget
// ("" mirrors null).
func runFormulaTarget(root *runSnapshotBead) string {
	if v := rootMetaPtr(root, beadmeta.RunTargetMetadataKey); v != "" {
		return v
	}
	if v := rootMetaPtr(root, beadmeta.RoutedToMetadataKey); v != "" {
		return v
	}
	if root != nil {
		return nonEmpty(root.assignee)
	}
	return ""
}

func rootMetaPtr(root *runSnapshotBead, key string) string {
	if root == nil {
		return ""
	}
	return nonEmpty(root.metadata[key])
}

// ── execution-path.ts ───────────────────────────────────────────────────────

// resolveRunExecutionPath resolves the run's execution path from cwd/work_dir/
// rig_root metadata, then the rig root argument. Port of TS resolveRunExecutionPath.
func resolveRunExecutionPath(root *runSnapshotBead, beads []runSnapshotBead, rigRoot string) RunExecutionPath {
	if path, ok := executionWorkDirsPtr(root); ok {
		return RunExecutionPath{Kind: "known", Path: path}
	}
	for i := range beads {
		if path, ok := executionWorkDirs(beads[i]); ok {
			return RunExecutionPath{Kind: "known", Path: path}
		}
	}
	if path, ok := rigRootsPtr(root); ok {
		return RunExecutionPath{Kind: "known", Path: path}
	}
	for i := range beads {
		if path, ok := rigRoots(beads[i]); ok {
			return RunExecutionPath{Kind: "known", Path: path}
		}
	}
	if path := nonEmpty(rigRoot); path != "" {
		return RunExecutionPath{Kind: "known", Path: path}
	}
	return RunExecutionPath{Kind: "unavailable", Reason: "missing_cwd_and_rig_root"}
}

func executionWorkDirs(bead runSnapshotBead) (string, bool) {
	for _, key := range []string{beadmeta.CwdMetadataKey, "cwd", beadmeta.WorkDirMetadataKey, "work_dir"} {
		if v := beadMeta(bead, key); v != "" {
			return v, true
		}
	}
	return "", false
}

func executionWorkDirsPtr(bead *runSnapshotBead) (string, bool) {
	if bead == nil {
		return "", false
	}
	return executionWorkDirs(*bead)
}

func rigRoots(bead runSnapshotBead) (string, bool) {
	for _, key := range []string{beadmeta.RigRootMetadataKey, "rig_root"} {
		if v := beadMeta(bead, key); v != "" {
			return v, true
		}
	}
	return "", false
}

func rigRootsPtr(bead *runSnapshotBead) (string, bool) {
	if bead == nil {
		return "", false
	}
	return rigRoots(*bead)
}
