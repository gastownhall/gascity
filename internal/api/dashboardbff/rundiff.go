package dashboardbff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// runDiffResponse is the wire shape the SPA fetches via
// POST /api/city/{cityName}/runs/{runId}/diff. It must match
// shared/src/run-detail.ts RunDiffResponse exactly — the frontend's
// decodeRunDiff validates kind (string), rootPath (object), comparison
// (object), status (array), changedFiles (array), patch (string), and
// truncated (boolean). The optional error field is present only when
// kind == "error".
type runDiffResponse struct {
	Kind         string           `json:"kind"`
	RootPath     runDiffRootPath  `json:"rootPath"`
	Comparison   runDiffCompare   `json:"comparison"`
	Status       []string         `json:"status"`
	ChangedFiles []runChangedFile `json:"changedFiles"`
	Patch        string           `json:"patch"`
	Truncated    bool             `json:"truncated"`
	// Error is the human-readable failure reason. omitempty keeps it absent
	// from the ok / not_git / path_unknown shapes, matching the discriminated
	// union in run-detail.ts where only the error variant carries it.
	Error string `json:"error,omitempty"`
}

// runDiffRootPath mirrors RunDiffRootPath: { kind: "known", path } or
// { kind: "unavailable", reason }. Both fields use omitempty so each variant
// serializes with only its own keys.
type runDiffRootPath struct {
	Kind   string `json:"kind"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// runDiffCompare mirrors RunDiffComparison's three variants:
//   - { kind: "upstream", ref, mergeBase }
//   - { kind: "head", reason: "no_upstream" | "upstream_lookup_failed" }
//   - { kind: "unavailable", reason: "path_unknown" | "not_git" | "error" }
type runDiffCompare struct {
	Kind      string `json:"kind"`
	Ref       string `json:"ref,omitempty"`
	MergeBase string `json:"mergeBase,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// runChangedFile mirrors RunChangedFile: { path, status, kind }. kind is one
// of code|test|docs|config|other.
type runChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Kind   string `json:"kind"`
}

// runDiffRequest decodes the POST body. It mirrors RunDiffRequest:
// { executionPath: RunExecutionPath }. The execution path (the git cwd) is
// supplied by the browser from the run-detail it already loaded — there is NO
// supervisor round-trip in this endpoint. The scope_kind/scope_ref query
// params are validated for shape (matching the BFF route guard) but the cwd
// comes solely from this body.
type runDiffRequest struct {
	ExecutionPath runExecutionPath `json:"executionPath"`
}

// runExecutionPath mirrors RunExecutionPath: { kind: "known", path } or
// { kind: "unavailable", reason: "missing_cwd_and_rig_root" }.
type runExecutionPath struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Validation patterns shared with the BFF route + shared package.
var (
	// beadIDRE mirrors shared/src/bead-id.ts BEAD_ID_RE.
	beadIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	// scopeRefRE mirrors shared/src/run-detail.ts SCOPE_REF_RE.
	scopeRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
	// mergeBaseHashRE matches the 40-64 hex commit hash a merge-base must be.
	mergeBaseHashRE = regexp.MustCompile(`(?i)^[0-9a-f]{40,64}$`)
)

// registerRunDiff wires POST /api/city/{cityName}/runs/{runId}/diff. The plane
// is mounted at /api/, so the full path is registered here. plane.go's
// registerRoutes calls this; it is intentionally not edited by this file.
func (p *Plane) registerRunDiff() {
	p.mux.HandleFunc("POST /api/city/{cityName}/runs/{runId}/diff", p.handleRunDiff)
}

func (p *Plane) handleRunDiff(w http.ResponseWriter, r *http.Request) {
	cityName := r.PathValue("cityName")
	cityPath, ok := p.resolveCityPath(cityName)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown city")
		return
	}

	runID := r.PathValue("runId")
	if !beadIDRE.MatchString(runID) {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	if err := validateRunScopeQuery(r); err != "" {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	var body runDiffRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "request body must be a JSON object")
		return
	}
	cwd, ferr := body.ExecutionPath.resolve()
	if ferr != "" {
		writeError(w, http.StatusBadRequest, ferr)
		return
	}

	// The effective allowed-roots for this request are the configured roots plus
	// the resolved city's own directory. Always passing the city path means runs
	// under the city work by default, while the empty-allowlist case stays
	// fail-closed (isValidRunCwd rejects everything when the effective list is
	// empty), so arbitrary host paths are still refused.
	allowedRoots := append(append([]string{}, p.deps.RunCwdAllowedRoots...), cityPath)

	resp, err := p.readRunGitDiff(r.Context(), cwd, allowedRoots)
	if err != nil {
		// The exec methods return a validation error when the cwd fails the
		// shape/allowlist gate; surface that as a 400 rather than a 500 so the
		// browser sees a client error for a bad execution path.
		var ee *execError
		if errors.As(err, &ee) {
			writeError(w, http.StatusBadRequest, "invalid execution path")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch run diff")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// validateRunScopeQuery mirrors the BFF's fromRequestScope shape checks. The
// values are not used to resolve the cwd (the body carries that); they are
// validated only so a malformed deep-link is rejected the same way both sides
// of the wire reject it. Returns "" when valid, else the error message.
func validateRunScopeQuery(r *http.Request) string {
	q := r.URL.Query()
	hasKind := q.Has("scope_kind")
	hasRef := q.Has("scope_ref")
	if hasKind {
		k := q.Get("scope_kind")
		if k != "city" && k != "rig" {
			return "invalid scope kind"
		}
	}
	if hasKind != hasRef {
		return "scope kind and scope ref are required together"
	}
	if hasRef && !scopeRefRE.MatchString(q.Get("scope_ref")) {
		return "invalid scope ref"
	}
	return ""
}

// resolve extracts the validated git cwd from the execution path, or returns a
// 400 message. Mirrors parseRunDiffBody in runs.ts: an "unavailable" path is a
// well-formed request that yields a path_unknown diff (empty cwd, no error); a
// "known" path must carry a non-empty path; any other kind/reason is rejected.
func (e runExecutionPath) resolve() (cwd string, errMsg string) {
	switch e.Kind {
	case "known":
		if strings.TrimSpace(e.Path) == "" {
			return "", "executionPath.path must be a non-empty string"
		}
		return e.Path, ""
	case "unavailable":
		if e.Reason != "missing_cwd_and_rig_root" {
			return "", "executionPath.reason is invalid"
		}
		return "", ""
	default:
		return "", "executionPath.kind is invalid"
	}
}

// readRunGitDiff ports backend/src/runs/diff.ts readRunGitDiff. An empty cwd
// (from an "unavailable" execution path) yields a path_unknown diff. Otherwise
// it resolves the repo root, the comparison base (upstream merge-base vs HEAD),
// and assembles the reviewable status, changedFiles, and unified patch.
// allowedRoots is the effective run-cwd allowlist (configured roots plus the
// resolved city dir); every git read is gated against it.
//
// Simplification vs the BFF: the BFF additionally walks UNTRACKED files via the
// `untracked` git view (git ls-files --others) and synthesizes per-file patches
// via execRunGitNewFileDiff. Neither helper is exposed on this plane's exec
// runner, so the untracked-patch and "??"-status changedFiles sub-path is not
// ported here. Untracked files still surface in the `status` array because the
// `status` view passes --untracked-files=all; only their synthesized patch
// bodies and changedFiles entries are omitted. The tracked-diff path — the core
// of the endpoint — is ported faithfully.
func (p *Plane) readRunGitDiff(ctx context.Context, cwd string, allowedRoots []string) (runDiffResponse, error) {
	if cwd == "" {
		return emptyDiff("path_unknown"), nil
	}

	rootRes, err := p.exec.execRunGit(ctx, cwd, "root", allowedRoots)
	if err != nil {
		return runDiffResponse{}, err
	}
	rootPath := strings.TrimSpace(rootRes.stdout)
	if rootRes.exitCode != 0 || rootPath == "" {
		// Not a git repo (or rev-parse failed): mirror the BFF's not_git path.
		return emptyDiff("not_git"), nil
	}

	comparison, cmpErr := p.resolveComparison(ctx, cwd, allowedRoots)
	if cmpErr != nil {
		return runDiffResponse{}, cmpErr
	}

	statusRes, err := p.exec.execRunGit(ctx, cwd, "status", allowedRoots)
	if err != nil {
		return runDiffResponse{}, err
	}
	if statusRes.exitCode != 0 {
		return errorDiff(rootPath), nil
	}
	status := parseStatusLines(statusRes.stdout)

	trackedPatch, patchTrunc, perr := p.readTrackedPatch(ctx, cwd, comparison, allowedRoots)
	if perr != nil {
		return runDiffResponse{}, perr
	}
	changedFiles, cerr := p.readTrackedChangedFiles(ctx, cwd, comparison, allowedRoots)
	if cerr != nil {
		return runDiffResponse{}, cerr
	}

	patch := filterReviewablePatch(trackedPatch)

	return runDiffResponse{
		Kind:         "ok",
		RootPath:     runDiffRootPath{Kind: "known", Path: rootPath},
		Comparison:   comparison,
		Status:       status,
		ChangedFiles: mergeChangedFiles(changedFiles),
		Patch:        sanitizeTerminalOutput(patch),
		Truncated:    statusRes.truncated || patchTrunc,
	}, nil
}

// emptyDiff builds the not_git / path_unknown response: an unavailable rootPath
// and comparison whose reason matches the diff kind, and empty data.
func emptyDiff(kind string) runDiffResponse {
	return runDiffResponse{
		Kind:         kind,
		RootPath:     runDiffRootPath{Kind: "unavailable", Reason: kind},
		Comparison:   runDiffCompare{Kind: "unavailable", Reason: kind},
		Status:       []string{},
		ChangedFiles: []runChangedFile{},
		Patch:        "",
		Truncated:    false,
	}
}

// errorDiff builds the error response after the repo root resolved but a later
// read failed. It keeps the known rootPath and carries an "error" comparison.
func errorDiff(rootPath string) runDiffResponse {
	return runDiffResponse{
		Kind:         "error",
		RootPath:     runDiffRootPath{Kind: "known", Path: rootPath},
		Comparison:   runDiffCompare{Kind: "unavailable", Reason: "error"},
		Status:       []string{},
		ChangedFiles: []runChangedFile{},
		Patch:        "",
		Truncated:    false,
		Error:        "git diff failed",
	}
}

// resolveComparison ports diff.ts resolveComparison. It prefers an upstream
// merge-base comparison; if no upstream is configured or its merge-base can't
// be resolved to a valid hash, it falls back to comparing against HEAD.
func (p *Plane) resolveComparison(ctx context.Context, cwd string, allowedRoots []string) (runDiffCompare, error) {
	upstream, err := p.exec.execRunGit(ctx, cwd, "upstream", allowedRoots)
	if err != nil {
		return runDiffCompare{}, err
	}
	upstreamRef := strings.TrimSpace(upstream.stdout)
	if upstream.exitCode != 0 || upstreamRef == "" {
		return runDiffCompare{Kind: "head", Reason: "no_upstream"}, nil
	}
	mergeBase, err := p.exec.execRunGit(ctx, cwd, "merge-base-upstream", allowedRoots)
	if err != nil {
		return runDiffCompare{}, err
	}
	mergeBaseHash := strings.TrimSpace(mergeBase.stdout)
	if mergeBase.exitCode != 0 || !mergeBaseHashRE.MatchString(mergeBaseHash) {
		return runDiffCompare{Kind: "head", Reason: "upstream_lookup_failed"}, nil
	}
	return runDiffCompare{Kind: "upstream", Ref: upstreamRef, MergeBase: mergeBaseHash}, nil
}

// readTrackedPatch ports diff.ts readTrackedPatch. For an upstream comparison
// it diffs from the merge-base; for a head comparison it diffs HEAD. A capped
// diff (truncated) is treated as success even with a non-zero exit. A HEAD diff
// that fails outright degrades to an empty patch (matching the BFF's logWarn +
// empty-return), while an upstream diff failure is a hard error.
func (p *Plane) readTrackedPatch(ctx context.Context, cwd string, comparison runDiffCompare, allowedRoots []string) (string, bool, error) {
	switch comparison.Kind {
	case "upstream":
		res, err := p.exec.execRunGitDiffFrom(ctx, cwd, comparison.MergeBase, allowedRoots)
		if err != nil {
			return "", false, err
		}
		if res.exitCode != 0 && !res.truncated {
			return "", false, errRunDiff
		}
		return res.stdout, res.truncated, nil
	case "head":
		res, err := p.exec.execRunGit(ctx, cwd, "diff-head", allowedRoots)
		if err != nil {
			return "", false, err
		}
		if res.exitCode != 0 && !res.truncated {
			return "", false, nil
		}
		return res.stdout, res.truncated, nil
	default:
		return "", false, nil
	}
}

// readTrackedChangedFiles ports diff.ts readTrackedChangedFiles. It reads the
// name-status output for the chosen comparison, keeps only reviewable lines,
// parses each into a RunChangedFile, and drops any whose path is not reviewable.
// A non-zero exit degrades to an empty list (the BFF logs and returns []).
func (p *Plane) readTrackedChangedFiles(ctx context.Context, cwd string, comparison runDiffCompare, allowedRoots []string) ([]runChangedFile, error) {
	var res *execResult
	var err error
	switch comparison.Kind {
	case "upstream":
		res, err = p.exec.execRunGitNameStatusFrom(ctx, cwd, comparison.MergeBase, allowedRoots)
	case "head":
		res, err = p.exec.execRunGit(ctx, cwd, "name-status-head", allowedRoots)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if res.exitCode != 0 {
		return nil, nil
	}
	return parseNameStatus(res.stdout), nil
}

// errRunDiff signals a hard upstream-diff failure (mapped to a 500 by the
// handler). It is a plain error, distinct from *execError validation failures.
var errRunDiff = &runDiffError{"git diff from upstream merge base failed"}

type runDiffError struct{ msg string }

func (e *runDiffError) Error() string { return e.msg }

// parseStatusLines ports the status parse in diff.ts: split on newlines, trim
// trailing whitespace, drop empties, and keep only reviewable porcelain-v1
// lines. The returned slice is never nil so it serializes as [] not null.
func parseStatusLines(stdout string) []string {
	out := []string{}
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimRight(raw, " \t\r\n")
		if line == "" {
			continue
		}
		if isReviewableStatusLine(line) {
			out = append(out, line)
		}
	}
	return out
}

// isReviewableStatusLine ports diff.ts isReviewableStatusLine. The porcelain-v1
// XY status occupies the first two columns; the payload starts at column 3. A
// rename ("old -> new") is reviewable only if both sides are reviewable.
func isReviewableStatusLine(line string) bool {
	if len(line) < 3 {
		return true
	}
	payload := strings.TrimSpace(line[3:])
	if payload == "" {
		return true
	}
	for _, fp := range strings.Split(payload, " -> ") {
		if !isReviewableRunDiffPath(fp) {
			return false
		}
	}
	return true
}

// parseNameStatus ports the name-status → changedFiles pipeline in diff.ts:
// keep reviewable lines, parse each, drop nils, and drop non-reviewable paths.
// The returned slice is never nil.
func parseNameStatus(stdout string) []runChangedFile {
	out := []runChangedFile{}
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimRight(raw, " \t\r\n")
		if line == "" {
			continue
		}
		if !isReviewableNameStatusLine(line) {
			continue
		}
		f, ok := parseNameStatusLine(line)
		if !ok {
			continue
		}
		if !isReviewableRunDiffPath(f.Path) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// isReviewableNameStatusLine ports diff.ts isReviewableNameStatusLine: a line
// is reviewable when every path column (everything after the status column) is
// reviewable. Lines with fewer than two columns are passed through.
func isReviewableNameStatusLine(line string) bool {
	parts := splitNonEmpty(line, "\t")
	if len(parts) < 2 {
		return true
	}
	for _, fp := range parts[1:] {
		if !isReviewableRunDiffPath(fp) {
			return false
		}
	}
	return true
}

// parseNameStatusLine ports diff.ts parseNameStatusLine. The status is the
// first tab-column; renames (R) and copies (C) collapse to a single letter and
// the destination path (the last column) is used. classifyRunDiffFile sets the
// file kind. Returns ok=false when the line lacks a status or path.
func parseNameStatusLine(line string) (runChangedFile, bool) {
	parts := splitNonEmpty(line, "\t")
	if len(parts) < 2 {
		return runChangedFile{}, false
	}
	rawStatus := parts[0]
	filePath := parts[len(parts)-1]
	var status string
	switch {
	case strings.HasPrefix(rawStatus, "R"):
		status = "R"
	case strings.HasPrefix(rawStatus, "C"):
		status = "C"
	default:
		status = rawStatus[:1]
	}
	if status == "" || filePath == "" {
		return runChangedFile{}, false
	}
	return runChangedFile{Path: filePath, Status: status, Kind: classifyRunDiffFile(filePath)}, true
}

// mergeChangedFiles ports diff.ts mergeChangedFiles: dedupe by path (last entry
// wins) and sort by path. The returned slice is never nil so it serializes as
// [] not null.
func mergeChangedFiles(files []runChangedFile) []runChangedFile {
	byPath := make(map[string]runChangedFile, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	out := make([]runChangedFile, 0, len(byPath))
	for _, f := range byPath {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// filterReviewablePatch ports diff.ts filterReviewablePatch: split the unified
// diff into "diff --git" blocks and drop any block whose a/ or b/ path is not
// reviewable. Blocks are rejoined with blank-line separators (joinPatch).
func filterReviewablePatch(patch string) string {
	if strings.TrimSpace(patch) == "" {
		return ""
	}
	var blocks []string
	var current []string
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") && len(current) > 0 {
			blocks = append(blocks, strings.Join(current, "\n"))
			current = nil
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	kept := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if isReviewablePatchBlock(b) {
			kept = append(kept, b)
		}
	}
	return joinPatch(kept)
}

var diffGitHeaderRE = regexp.MustCompile(`^diff --git a/(.+) b/(.+)$`)

// isReviewablePatchBlock ports diff.ts isReviewablePatchBlock. A block whose
// header doesn't match the "diff --git a/X b/Y" shape is kept; otherwise both
// captured paths must be reviewable.
func isReviewablePatchBlock(block string) bool {
	header := block
	if i := strings.IndexByte(block, '\n'); i >= 0 {
		header = block[:i]
	}
	m := diffGitHeaderRE.FindStringSubmatch(header)
	if m == nil {
		return true
	}
	return isReviewableRunDiffPath(m[1]) && isReviewableRunDiffPath(m[2])
}

// joinPatch ports diff.ts joinPatch: trim trailing whitespace off each part,
// drop empties, and join with a blank line between parts.
func joinPatch(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimRight(p, " \t\r\n")
		if t != "" {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, "\n\n")
}

// splitNonEmpty splits s on sep and drops empty fields, matching the BFF's
// `.split(sep).filter(part => part.length > 0)`.
func splitNonEmpty(s, sep string) []string {
	out := []string{}
	for _, part := range strings.Split(s, sep) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// controlPlanePathPrefixes mirrors run-diff-policy.ts CONTROL_PLANE_PATH_PREFIXES:
// diff reads must never expose the .beads / .gc control-plane dirs.
var controlPlanePathPrefixes = []string{".beads", ".gc"}

var gitDiffPathPrefixRE = regexp.MustCompile(`^"?[ab]/`)

// normalizeGitDiffPath ports run-diff-policy.ts normalizeGitDiffPath: strip a
// leading a/ or b/ (optionally quoted) and a trailing quote so the path can be
// matched against the control-plane prefixes.
func normalizeGitDiffPath(filePath string) string {
	out := gitDiffPathPrefixRE.ReplaceAllString(filePath, "")
	return strings.TrimSuffix(out, `"`)
}

// isReviewableRunDiffPath ports run-diff-policy.ts isReviewableRunDiffPath: the
// normalized path must not be, or be nested under, any control-plane dir.
func isReviewableRunDiffPath(filePath string) bool {
	normalized := normalizeGitDiffPath(filePath)
	for _, prefix := range controlPlanePathPrefixes {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"/") {
			return false
		}
	}
	return true
}

var codeExtRE = regexp.MustCompile(`\.(ts|tsx|js|jsx|go|rs|py|rb|java|kt|swift|c|cc|cpp|h|hpp|css|scss|html)$`)

// classifyRunDiffFile ports run-diff-policy.ts classifyRunDiffFile. The order
// of checks is load-bearing: a "/test/" path classifies as test even when its
// extension would otherwise read as code, and config extensions win over the
// generic code-extension match.
func classifyRunDiffFile(filePath string) string {
	lower := strings.ToLower(normalizeGitDiffPath(filePath))
	switch {
	case strings.HasSuffix(lower, ".test.ts"),
		strings.HasSuffix(lower, ".test.tsx"),
		strings.HasSuffix(lower, ".spec.ts"),
		strings.HasSuffix(lower, ".spec.tsx"),
		strings.Contains(lower, "/test/"),
		strings.Contains(lower, "/tests/"):
		return "test"
	case strings.HasSuffix(lower, ".md"),
		strings.HasSuffix(lower, ".mdx"),
		strings.Contains(lower, "/docs/"):
		return "docs"
	case strings.HasSuffix(lower, ".json"),
		strings.HasSuffix(lower, ".toml"),
		strings.HasSuffix(lower, ".yaml"),
		strings.HasSuffix(lower, ".yml"),
		strings.HasSuffix(lower, ".config.ts"),
		strings.HasSuffix(lower, ".config.js"),
		lower == "package.json",
		strings.HasSuffix(lower, "/package.json"):
		return "config"
	case codeExtRE.MatchString(lower):
		return "code"
	default:
		return "other"
	}
}
