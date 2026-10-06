package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workrecord"
)

// The CLI plane's half of the ADR-0009 work-record close gate: the bd-argv
// plumbing around internal/workrecord, which owns the contract itself (which
// beads it covers, what a valid record is, whether enforcement is on). The HTTP
// plane runs the same package against the owner store its residency resolver
// pins, so a close cannot dodge the contract by changing doors.
//
// That sentence covers the reachability clause too, which is the one part of the
// contract a bead cannot answer alone: workrecord.RepoDirFor names the repository
// from the bead's OWNER — its gc.work_dir, else the scope gc.root_store_ref
// records — and both doors run it, so one bead is judged against one repository
// however it is closed. Each door supplies only its own checkout table
// (workRecordRepoDirs here, workRecordScopeDirs in internal/api) and its own
// answer for a bead that names no owner.
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
const workRecordEnforceEnvVar = workrecord.EnforceEnvVar

// workRecordEnforceEnabled reports whether the close gate should block closes
// that violate the work-record contract, rather than only warning.
func workRecordEnforceEnabled() bool { return workrecord.EnforceEnabled() }

// validWorkOutcome reports whether v is one of the four typed work-record close
// dispositions.
func validWorkOutcome(v string) bool { return workrecord.ValidOutcome(v) }

// isWorkRecordGatedBead reports whether the work-record close contract applies
// to bead — the population every plane's gate shares.
func isWorkRecordGatedBead(bead beads.Bead) bool { return workrecord.Gated(bead) }

// validateWorkRecordOnClose checks bead against the typed work-record contract
// and returns a human-readable message for each violation (empty slice ⇒ the
// bead satisfies the contract). commitReachable reports whether a commit SHA is
// an ancestor of a branch; it is injected so the rule is unit-testable without
// a real repo. The caller is responsible for scoping (isWorkRecordGatedBead).
func validateWorkRecordOnClose(bead beads.Bead, commitReachable func(commit, branch string) bool) []string {
	return workrecord.ValidateOnClose(bead, commitReachable)
}

// gitCommitReachableOnBranch reports whether commit is an ancestor of branch in
// the git repository at repoDir. The rule — including which refs can prove
// reachability — lives in internal/workrecord so both close doors ask git the
// same question.
func gitCommitReachableOnBranch(repoDir, commit, branch string) bool {
	return workrecord.CommitReachableOnBranch(repoDir, commit, branch)
}

// workRecordRepoDirs is the CLI plane's checkout table, plus the answer this
// door gave before a bead could name its owner.
//
// workrecord.RepoDirFor decides WHICH repository a bead answers to, from the
// bead's own gc.work_dir or its gc.root_store_ref owner; this type only supplies
// the directories. legacy is the scope the caller READ through — the resolved
// work scope on the bd fall-through, the city on the class door — and it is
// still the answer for a bead that records no owner, so a city that never
// stamped one closes exactly as it did before.
type workRecordRepoDirs struct {
	cityPath string
	legacy   string
	rigs     func() []config.Rig
}

// CityDir returns the city checkout's directory.
func (d workRecordRepoDirs) CityDir() string { return strings.TrimSpace(d.cityPath) }

// RigDir returns the named rig's checkout and whether this city configures that
// rig at all. A configured rig that names no checkout answers ("", true), which
// the rule reads as unknown rather than as the city.
//
// The name is matched case-insensitively, deliberately more forgiving than the
// exact-match lookups this answer feeds (workdir.RigRootForName, sling's
// rigSuspended). Refs are stamped by storeref.RigRef from the configured rig
// name, so only a hand-stamped or historically-buggy ref differs by case;
// matching it judges the close against that rig's checkout instead of degrading
// to unverified. Both doors spell this matcher the same way, so whichever way it
// resolves, one bead is still judged against one repository.
func (d workRecordRepoDirs) RigDir(name string) (string, bool) {
	if d.rigs == nil {
		return "", false
	}
	for _, rig := range d.rigs() {
		if !strings.EqualFold(strings.TrimSpace(rig.Name), name) {
			continue
		}
		if strings.TrimSpace(rig.Path) == "" {
			return "", true
		}
		return resolveStoreScopeRoot(d.cityPath, rig.Path), true
	}
	return "", false
}

// repoDirFor names the repository this close is judged against, or "" when the
// bead's owner names no checkout this city knows.
func (d workRecordRepoDirs) repoDirFor(bead beads.Bead) string {
	dir, kind := workrecord.RepoDirFor(bead, d)
	if kind == workrecord.ScopeUnrooted {
		return strings.TrimSpace(d.legacy)
	}
	return dir
}

// cityRigsLoader returns a rig table that loads the city config at most once, on
// first use.
//
// The class door is entered from doBd's cost gate, before anything in that
// invocation has read city.toml, and every read routed through it must stay
// cheap. The rig table answers one question — where a rig-OWNED bead's commit
// lives — which only a close of a gated bead asks, so the load is deferred to
// that point and never paid by a show, a claim, or a dep walk.
//
// A config this invocation cannot read yields no rigs, which the rule reads as
// an unknown owner and degrades on. Failing the close on a read the door does
// not otherwise need would be a refusal manufactured by the gate.
func cityRigsLoader(cityPath string) func() []config.Rig {
	var (
		once sync.Once
		rigs []config.Rig
	)
	return func() []config.Rig {
		once.Do(func() {
			cfg, err := loadCityConfigAdvisory(cityPath)
			if err != nil || cfg == nil {
				return
			}
			rigs = cfg.Rigs
		})
		return rigs
	}
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
	dirs := workRecordRepoDirs{
		cityPath: cityPath,
		legacy:   scopeRoot,
		rigs: func() []config.Rig {
			if cfg == nil {
				return nil
			}
			return cfg.Rigs
		},
	}
	return evaluateWorkRecordCloseGate(bdArgs, store, preFetched, dirs, workRecordEnforceEnabled(), stderr)
}

// evaluateWorkRecordCloseGate is the store-driven core of the close gate, split
// from the IO wrapper so it is unit-testable with an in-memory store. It logs
// each violation and reports whether the close should be blocked. preFetched
// (optional) supplies beads already read by an earlier guard in this same
// invocation, avoiding a duplicate store.Get for the same ID.
//
// dirs names the checkouts this city knows, and the reachability clause is asked
// about the one the closing bead's owner points at. When that is nothing the
// clause degrades to a warning rather than failing closed: the door cannot pose
// the question, and refusing a close it could not judge on that basis would
// block work on a config gap. The sibling clauses do not degrade — an outcome-
// less bead is refused whether or not a repository was found.
func evaluateWorkRecordCloseGate(bdArgs []string, store beads.Store, preFetched map[string]beads.Bead, dirs workRecordRepoDirs, enforce bool, stderr io.Writer) (block bool) {
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
		var violations []string
		if projectionErr != nil {
			violations = []string{projectionErr.Error()}
		} else {
			// The repository is resolved from the PROJECTED bead: an atomic
			// close may stamp the work directory or the owner in the same
			// invocation that closes.
			repoDir := dirs.repoDirFor(bead)
			unverified := false
			violations = validateWorkRecordOnClose(bead, func(commit, branch string) bool {
				if repoDir == "" {
					unverified = true
					return true
				}
				return gitCommitReachableOnBranch(repoDir, commit, branch)
			})
			if unverified {
				fmt.Fprintf(stderr, "gc bd: work-record gate (%s): close of %s: %s\n", mode, id, workrecord.ReachabilityUnverifiedNote) //nolint:errcheck // best-effort stderr
			}
		}
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
