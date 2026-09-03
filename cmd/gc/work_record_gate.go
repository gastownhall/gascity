package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Work-record close gate (ADR-0009). Closing a work bead through the SDK close
// seam (`gc bd close`) is validated against the typed work-record contract: the
// bead must carry a typed gc.work_outcome, and a "shipped" outcome must point at
// a commit published on a remote-tracking ref. Squash merges count as published
// when every path changed by the work commit has the same blob on a remote ref.
// This turns the recurring "drain-without-published-work" close into a
// machine-checkable violation.
//
// The gate ships warn-only by default — violations are logged but the close
// proceeds — so existing open beads migrate without breakage. Set
// GC_WORK_RECORD_ENFORCE to a truthy value to make violations block the close.
//
// # Two entry points run this gate: the bd fall-through and the class door
//
// doBd runs the by-ID class door (cmd_bd_by_id.go maybeRouteBdByID) BEFORE this
// gate. A close that falls through to the bd subprocess is gated here against
// the PREFIX store (the caller's resolved work scope). A close the door SERVES —
// because the city relocated a coordination class and the bead resides in the
// class binding — never reaches this fall-through, so the door runs the same
// evaluateWorkRecordCloseGate against the class bead it is about to write
// (gateBdByIDClassClose), reusing the row it already resolved. Both spellings
// the door serves — `gc bd close <id>` and `gc bd update <id> --status closed`
// — are therefore gated wherever they land; the two entry points cover disjoint
// stores, and neither trusts the other's read.
//
// The DUAL-RESIDENT case is why the split matters rather than being a redundant
// double-gate. `gc storage migrate` copies every non-work bead with its id
// preserved and keeps the source (readInfraSnapshot / infra_class_migrate.go),
// and coordclass.Classify routes ANY bead carrying gc.root_bead_id to ClassGraph
// (isWorkflowMetadata) — including a plain task-typed molecule work step with no
// gc.kind, which is exactly isWorkRecordGatedBead's population. On a migrated
// city those steps exist in BOTH stores. The gate always validates the row the
// close is about to write: the fall-through path validates the work store's
// retained copy it is closing, and the door validates the class copy it is
// closing. Neither validates the other store's stale row, because neither
// writes it.
//
// Which copy a by-id close writes is the residency question, and both planes
// now answer it the same way: the CLI door resolves a dual resident to the
// CLASS copy through its own residence probe, and internal/api resolves it
// through the residency resolver's ByID plan, which leads with the binding for
// exactly this reason. The retained work copy stays reachable through raw bd
// against the work scope, and it still has to be drained; that reconciliation
// is the sweep's job, not this gate's.

// workRecordEnforceEnvVar gates whether work-record violations block the close
// (enforce) or are logged only (warn-only, the default).
const workRecordEnforceEnvVar = "GC_WORK_RECORD_ENFORCE"

// workRecordEnforceEnabled reports whether the close gate should block closes
// that violate the work-record contract, rather than only warning.
func workRecordEnforceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(workRecordEnforceEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// validWorkOutcome reports whether v is one of the four typed work-record close
// dispositions. The vocabulary is owned here (the consumer), not in beadmeta,
// per that package's data-only convention.
func validWorkOutcome(v string) bool {
	switch v {
	case beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp,
		beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned:
		return true
	default:
		return false
	}
}

// isWorkRecordGatedBead reports whether the work-record close contract applies
// to bead. It applies to worker-claimable work units — plain task beads — and
// deliberately NOT to control/structural beads (anything carrying gc.kind:
// workflow roots, scope/run/check/drain steps, etc.) or non-task beads (convoy,
// message). Those use the disjoint control-plane gc.outcome vocabulary and are
// closed by the dispatch engine, not by a worker reporting a work outcome.
func isWorkRecordGatedBead(bead beads.Bead) bool {
	if t := strings.TrimSpace(bead.Type); t != "" && t != "task" {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]) != "" {
		return false
	}
	return true
}

// validateWorkRecordOnClose checks bead against the typed work-record contract
// and returns a human-readable message for each violation (empty slice ⇒ the
// bead satisfies the contract). commitPublished reports whether a commit is on
// a remote-tracking ref or has squash-equivalent blobs there. The caller is
// responsible for scoping (isWorkRecordGatedBead).
func validateWorkRecordOnClose(bead beads.Bead, commitPublished func(commit string) bool) []string {
	outcome := strings.TrimSpace(bead.Metadata[beadmeta.WorkOutcomeMetadataKey])
	if outcome == "" {
		return []string{fmt.Sprintf("missing %s (want one of shipped|no-op|blocked|abandoned)", beadmeta.WorkOutcomeMetadataKey)}
	}
	if !validWorkOutcome(outcome) {
		return []string{fmt.Sprintf("invalid %s=%q (want one of shipped|no-op|blocked|abandoned)", beadmeta.WorkOutcomeMetadataKey, outcome)}
	}
	if outcome != beadmeta.WorkOutcomeShipped {
		// no-op / blocked / abandoned carry their reason in the close-reason; no
		// commit artifact is required.
		return nil
	}
	commit := strings.TrimSpace(bead.Metadata[beadmeta.WorkCommitMetadataKey])
	branch := strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey])
	var violations []string
	if commit == "" {
		violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the commit that satisfied the bead)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkCommitMetadataKey))
	}
	if branch == "" {
		violations = append(violations, fmt.Sprintf("%s=shipped requires %s (the branch the commit lives on)", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkBranchMetadataKey))
	}
	if commit != "" && branch != "" && !commitPublished(commit) {
		violations = append(violations, fmt.Sprintf("%s %s is not published on any remote-tracking ref", beadmeta.WorkCommitMetadataKey, commit))
	}
	return violations
}

var commitTokenRE = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

const gitBlobPathBatchSize = 256

func closeEvidenceFlag(verb, flag string) (reasonFile, ok bool) {
	switch verb {
	case "close":
		switch flag {
		case "-r", "--reason":
			return false, true
		case "--reason-file":
			return true, true
		}
	case "update":
		switch flag {
		case "--notes", "--append-notes":
			return false, true
		}
	}
	return false, false
}

func closeReasonCommitCandidates(bdArgs []string, scopeRoot string) []string {
	if len(bdArgs) == 0 {
		return nil
	}
	valueFlags := bdflags.ValueFlags(bdArgs[0])
	var candidates []string
	for i := 1; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if arg == "--" {
			break
		}
		flag, value, inline := strings.Cut(arg, "=")
		reasonFile, ok := closeEvidenceFlag(bdArgs[0], flag)
		if !ok {
			if !inline && valueFlags[flag] && i+1 < len(bdArgs) {
				i++
			}
			continue
		}
		if !inline {
			if i+1 >= len(bdArgs) {
				continue
			}
			i++
			value = bdArgs[i]
		}
		if reasonFile {
			if value == "-" {
				continue
			}
			if !filepath.IsAbs(value) {
				value = filepath.Join(scopeRoot, value)
			}
			data, err := os.ReadFile(value)
			if err != nil {
				continue
			}
			value = string(data)
		}
		candidates = append(candidates, commitTokenRE.FindAllString(value, -1)...)
	}
	return candidates
}

func validateCloseReasonCommitsOnPublishedRefs(
	bead beads.Bead,
	bdArgs []string,
	scopeRoot, repoDir string,
) []string {
	if strings.TrimSpace(bead.Metadata[beadmeta.WorkCommitMetadataKey]) != "" {
		return nil
	}
	seen := make(map[string]struct{})
	var violations []string
	for _, commit := range closeReasonCommitCandidates(bdArgs, scopeRoot) {
		if _, ok := seen[commit]; ok {
			continue
		}
		seen[commit] = struct{}{}
		published, valid := gitCommitPublication(repoDir, commit)
		if valid && !published {
			violations = append(violations, fmt.Sprintf(
				"commit %s named in close reason is not published on any remote-tracking ref",
				commit,
			))
		}
	}
	return violations
}

// gitCommitPublication reports whether commit resolves locally and whether its
// work is present on a remote-tracking ref. Direct ancestry covers ordinary
// pushes. Changed-path blob equality covers squash merges without comparing
// unrelated paths added to the destination branch after the work branched.
func gitCommitPublication(repoDir, commit string) (published, valid bool) {
	commit = strings.TrimSpace(commit)
	if strings.TrimSpace(repoDir) == "" || commit == "" || strings.HasPrefix(commit, "-") {
		return false, false
	}
	out, err := exec.Command(
		"git", "-C", repoDir, "rev-parse", "--verify", "--quiet", commit+"^{commit}",
	).Output()
	if err != nil {
		return false, false
	}
	commit = strings.TrimSpace(string(out))
	out, err = exec.Command(
		"git", "-C", repoDir, "for-each-ref", "--format=%(refname)", "refs/remotes/",
	).Output()
	if err != nil {
		return false, true
	}
	for _, ref := range strings.Fields(string(out)) {
		if exec.Command("git", "-C", repoDir, "merge-base", "--is-ancestor", commit, ref).Run() == nil {
			return true, true
		}
		if gitChangedPathBlobsMatchRef(repoDir, commit, ref) {
			return true, true
		}
	}
	return false, true
}

// gitChangedPathBlobsMatchRef compares only paths changed from the work
// commit's merge base through the commit. git diff --quiet compares their tree
// entries and blob OIDs; unrelated destination-ref changes are out of scope.
func gitChangedPathBlobsMatchRef(repoDir, commit, ref string) bool {
	out, err := exec.Command("git", "-C", repoDir, "merge-base", commit, ref).Output()
	if err != nil {
		return false
	}
	base := strings.TrimSpace(string(out))
	out, err = exec.Command(
		"git", "-C", repoDir, "diff", "--name-only", "--no-renames", "-z", base, commit, "--",
	).Output()
	if err != nil {
		return false
	}
	encodedPaths := strings.TrimSuffix(string(out), "\x00")
	if encodedPaths == "" {
		return false
	}
	paths := strings.Split(encodedPaths, "\x00")
	for len(paths) > 0 {
		count := min(len(paths), gitBlobPathBatchSize)
		args := []string{
			"-C", repoDir, "diff", "--quiet", "--no-ext-diff", "--no-textconv", commit, ref, "--",
		}
		args = append(args, paths[:count]...)
		if exec.Command("git", args...).Run() != nil {
			return false
		}
		paths = paths[count:]
	}
	return true
}

// workRecordCloseTargets returns the bead IDs a bd invocation closes, and
// whether the invocation is a close at all. It covers both forms the SDK seam
// sees: the `close` subcommand and `update --status=closed` (the form the
// worker formulas use to stamp metadata and close in one call). Ambiguous or
// ID-less invocations report not-a-close so the gate stays out of the way.
func workRecordCloseTargets(bdArgs []string) ([]string, bool) {
	if len(bdArgs) == 0 {
		return nil, false
	}
	switch bdArgs[0] {
	case "close":
	case "update":
		if !bdUpdateClosesStatus(bdArgs) {
			return nil, false
		}
	default:
		return nil, false
	}
	ids, ok, ambiguous := bdMutationWriteIDs(bdArgs)
	if !ok || ambiguous || len(ids) == 0 {
		return nil, false
	}
	return ids, true
}

// bdUpdateClosesStatus reports whether a `bd update` arg list sets the status to
// "closed" (in any of the --status=closed, --status closed, -s closed forms).
// bd registers status as a scalar flag, so the last occurrence wins. Values of
// other known flags are consumed before looking for status, and `--` terminates
// flag parsing, matching the mutation target scanner and pflag.
func bdUpdateClosesStatus(bdArgs []string) bool {
	valueFlags := bdSubcmdValueFlags("update")
	status := ""
	seen := false
	for i := 1; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if arg == "--" {
			break
		}
		if v, ok := strings.CutPrefix(arg, "--status="); ok {
			status, seen = v, true
			continue
		}
		if v, ok := strings.CutPrefix(arg, "-s="); ok {
			status, seen = v, true
			continue
		}
		if arg == "--status" || arg == "-s" {
			if i+1 >= len(bdArgs) {
				return false
			}
			i++
			status, seen = bdArgs[i], true
			continue
		}
		if !strings.Contains(arg, "=") && valueFlags[arg] && i+1 < len(bdArgs) {
			i++
		}
	}
	return seen && strings.EqualFold(strings.TrimSpace(status), "closed")
}

// runWorkRecordCloseGate validates every bead a `gc bd close` (or
// `gc bd update --status=closed`) invocation closes against the work-record
// contract. Best-effort: it never blocks on its own read failure. Returns
// whether the close should be blocked (only when enforcement is enabled).
//
// preOpened and preFetched let a caller that already opened the store and
// fetched the target beads (e.g. the write-ID collision guard, which reads
// the same beads for the same IDs immediately before this gate runs) hand
// them in instead of paying a second openStoreAtForCity + store.Get round
// trip. Both are optional (nil is fine): preOpened falls back to opening its
// own store, and any ID missing from preFetched falls back to store.Get.
func runWorkRecordCloseGate(bdArgs []string, scopeRoot, cityPath string, cfg *config.City, preOpened beads.Store, preFetched map[string]beads.Bead, stderr io.Writer) bool {
	if _, ok := workRecordCloseTargets(bdArgs); !ok {
		return false
	}
	store := preOpened
	if store == nil {
		var err error
		store, err = openStoreAtForCityWithConfig(scopeRoot, cityPath, cfg)
		if err != nil {
			// Cannot verify — never block a close on our own read failure.
			return false
		}
	}
	return evaluateWorkRecordCloseGate(bdArgs, store, preFetched, scopeRoot, workRecordEnforceEnabled(), stderr)
}

// evaluateWorkRecordCloseGate is the store-driven core of the close gate, split
// from the IO wrapper so it is unit-testable with an in-memory store. It logs
// each violation and reports whether the close should be blocked. preFetched
// (optional) supplies beads already read by an earlier guard in this same
// invocation, avoiding a duplicate store.Get for the same ID.
func evaluateWorkRecordCloseGate(bdArgs []string, store beads.Store, preFetched map[string]beads.Bead, scopeRoot string, enforce bool, stderr io.Writer) (block bool) {
	ids, ok := workRecordCloseTargets(bdArgs)
	if !ok {
		return false
	}
	mode := "warn-only"
	if enforce {
		mode = "enforced"
	}
	for _, id := range ids {
		bead, cached := preFetched[id]
		if !cached {
			var getErr error
			bead, getErr = store.Get(id)
			if getErr != nil {
				continue
			}
		}
		if !isWorkRecordGatedBead(bead) {
			continue
		}
		var projectionErr error
		bead, projectionErr = applyWorkRecordUpdateMetadata(bead, bdArgs)
		repoDir := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey])
		if repoDir == "" {
			repoDir = scopeRoot
		}
		var violations []string
		if projectionErr != nil {
			violations = []string{projectionErr.Error()}
		} else {
			violations = validateWorkRecordOnClose(bead, func(commit string) bool {
				published, _ := gitCommitPublication(repoDir, commit)
				return published
			})
		}
		violations = append(
			violations,
			validateCloseReasonCommitsOnPublishedRefs(bead, bdArgs, scopeRoot, repoDir)...,
		)
		for _, v := range violations {
			fmt.Fprintf(stderr, "gc bd: work-record gate (%s): close of %s: %s\n", mode, id, v) //nolint:errcheck // best-effort stderr
		}
		if enforce && len(violations) > 0 {
			block = true
		}
	}
	return block
}

// workRecordMetadataEdits is the parsed metadata mutation of a `bd update` arg
// list: either a whole-object --metadata merge (hasMetadataJSON) or a set of
// --set-metadata / --unset-metadata edits. The two forms are mutually exclusive
// in bd; applyWorkRecordMetadataEdits enforces that.
type workRecordMetadataEdits struct {
	metadataJSON    string
	hasMetadataJSON bool
	setMetadata     []string
	unsetMetadata   []string
}

// applyWorkRecordUpdateMetadata overlays metadata mutations from an atomic
// `bd update ... --status=closed` invocation onto the stored bead before the
// close gate validates it. The documented worker close form stamps the typed
// work record and closes in one update, so validating only the pre-update bead
// would reject a valid enforced close and warn incorrectly in migration mode.
//
// The parse and apply phases are split so neither carries the whole projection's
// branch density; together they match bd's update flag semantics exactly.
func applyWorkRecordUpdateMetadata(bead beads.Bead, bdArgs []string) (beads.Bead, error) {
	if len(bdArgs) == 0 || bdArgs[0] != "update" {
		return bead, nil
	}
	metadata := make(beads.StringMap, len(bead.Metadata))
	for key, value := range bead.Metadata {
		metadata[key] = value
	}
	bead.Metadata = metadata
	edits, err := parseWorkRecordMetadataEdits(bdArgs)
	if err != nil {
		return bead, err
	}
	if err := applyWorkRecordMetadataEdits(bead.Metadata, edits); err != nil {
		return bead, err
	}
	return bead, nil
}

// parseWorkRecordMetadataEdits extracts the metadata mutations from a `bd update`
// arg list, matching bd's flag semantics: --metadata is a scalar whose last
// occurrence wins, and every known update flag's separate value is consumed so a
// value that itself looks like a metadata flag never mutates the prospective
// record. `--` terminates flag parsing.
func parseWorkRecordMetadataEdits(bdArgs []string) (workRecordMetadataEdits, error) {
	valueFlags := bdSubcmdValueFlags("update")
	var edits workRecordMetadataEdits
	for i := 1; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		switch {
		case arg == "--":
			i = len(bdArgs)
		case arg == "--metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --metadata: missing JSON value")
			}
			i++
			edits.metadataJSON = bdArgs[i]
			edits.hasMetadataJSON = true
		case strings.HasPrefix(arg, "--metadata="):
			edits.metadataJSON = strings.TrimPrefix(arg, "--metadata=")
			edits.hasMetadataJSON = true
		case arg == "--set-metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --set-metadata: missing key=value")
			}
			i++
			edits.setMetadata = append(edits.setMetadata, bdArgs[i])
		case strings.HasPrefix(arg, "--set-metadata="):
			edits.setMetadata = append(edits.setMetadata, strings.TrimPrefix(arg, "--set-metadata="))
		case arg == "--unset-metadata":
			if i+1 >= len(bdArgs) {
				return edits, fmt.Errorf("cannot project --unset-metadata: missing key")
			}
			i++
			edits.unsetMetadata = append(edits.unsetMetadata, bdArgs[i])
		case strings.HasPrefix(arg, "--unset-metadata="):
			edits.unsetMetadata = append(edits.unsetMetadata, strings.TrimPrefix(arg, "--unset-metadata="))
		case !strings.Contains(arg, "=") && valueFlags[arg] && i+1 < len(bdArgs):
			i++
		}
	}
	return edits, nil
}

// applyWorkRecordMetadataEdits overlays parsed edits onto metadata, matching bd:
// --metadata cannot be combined with the edit flags, and bd applies every
// --set-metadata edit before every --unset-metadata edit regardless of their
// order in argv. A more permissive projection could validate prospective
// metadata that bd never persists and allow an invalid close.
func applyWorkRecordMetadataEdits(metadata beads.StringMap, edits workRecordMetadataEdits) error {
	if edits.hasMetadataJSON && (len(edits.setMetadata) > 0 || len(edits.unsetMetadata) > 0) {
		return fmt.Errorf("cannot project metadata: --metadata cannot be combined with --set-metadata or --unset-metadata")
	}
	if edits.hasMetadataJSON {
		if err := mergeWorkRecordMetadataJSON(metadata, edits.metadataJSON); err != nil {
			return fmt.Errorf("cannot project --metadata: %w", err)
		}
		return nil
	}
	for _, edit := range edits.setMetadata {
		key, value, ok := strings.Cut(edit, "=")
		if !ok || key == "" {
			return fmt.Errorf("cannot project --set-metadata %q: expected key=value", edit)
		}
		metadata[key] = value
	}
	for _, key := range edits.unsetMetadata {
		if key == "" {
			return fmt.Errorf("cannot project --unset-metadata: key is empty")
		}
		delete(metadata, key)
	}
	return nil
}

// mergeWorkRecordMetadataJSON applies bd update's --metadata object as an
// additive metadata merge. Decode through beads.StringMap so the prospective
// bead sees the same boolean/number coercion as a bead read back from bd.
// @file inputs deliberately fail closed: resolving a caller-relative file in
// this preflight would introduce a second filesystem interpretation of bd's
// input and could validate bytes different from the mutation bd performs.
func mergeWorkRecordMetadataJSON(metadata beads.StringMap, value string) error {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "@") {
		return fmt.Errorf("@file input is not supported by the close gate")
	}
	var update beads.StringMap
	if err := json.Unmarshal([]byte(value), &update); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	for key, item := range update {
		metadata[key] = item
	}
	return nil
}
