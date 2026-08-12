package core

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// formulaFile is the subset of a formula TOML these tests inspect. Steps carry
// the agent-facing instructions, so asserting on a step description is how the
// pack pins behavior that lives in prompt text rather than in Go.
type formulaFile struct {
	Formula string `toml:"formula"`
	Steps   []struct {
		ID          string `toml:"id"`
		Title       string `toml:"title"`
		Description string `toml:"description"`
	} `toml:"steps"`
}

// readFormula decodes a formula TOML from the embedded core pack.
//
// file stays parameterized even though every current caller passes
// mol-polecat-base.toml: the pack ships sibling formulas (mol-polecat-commit,
// mol-polecat-report) that inherit these steps, and the next test to pin one of
// them reads it through this same helper.
//
//nolint:unparam // see above
func readFormula(t *testing.T, file string) formulaFile {
	t.Helper()
	data, err := fs.ReadFile(PackFS, "formulas/"+file)
	if err != nil {
		t.Fatalf("reading formulas/%s: %v", file, err)
	}
	var parsed formulaFile
	if _, err := toml.Decode(string(data), &parsed); err != nil {
		t.Fatalf("decoding formulas/%s: %v", file, err)
	}
	return parsed
}

// formulaStep returns the description of the named step, failing the test when
// the step is absent.
func formulaStep(t *testing.T, f formulaFile, id string) string {
	t.Helper()
	for _, step := range f.Steps {
		if step.ID == id {
			return step.Description
		}
	}
	t.Fatalf("formula %s has no step %q", f.Formula, id)
	return ""
}

// TestPolecatPreflightSearchesLedgerBeforeFiling pins the search-before-file
// contract in the polecat preflight step.
//
// Concurrent polecats all run a baseline against the same base branch, so they
// all observe the same pre-existing failure within seconds of each other. With
// no ledger search in front of the create, each one files its own bug: five
// duplicates inside three minutes from three polecats, and one duplicate that
// sat ready in the pool after its original had already merged, one sling away
// from dispatching a polecat to redo merged work.
func TestPolecatPreflightSearchesLedgerBeforeFiling(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	searchAt := strings.Index(step, "gc bd list --title-contains")
	if searchAt < 0 {
		t.Fatal("preflight-tests must search the ledger with `gc bd list --title-contains` before filing a pre-existing-failure bead")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt < 0 {
		t.Fatal("preflight-tests must still describe how to file a pre-existing-failure bead")
	}
	if searchAt > createAt {
		t.Error("preflight-tests searches the ledger after `gc bd create`; the search must gate the create, not follow it")
	}

	// The original report is usually already claimed by the polecat or refinery
	// fixing it, so an open-only filter misses the very bead it should match.
	searchCmd := step[searchAt:createAt]
	if !strings.Contains(searchCmd, "in_progress") {
		t.Error("the dedupe lookup must include --status in_progress; the existing bead is frequently already claimed")
	}

	// A lookup that errors is not an all-clear. The refinery's earlier attempt at
	// this check invoked a flag that does not exist (`gc bd list --search`), so it
	// errored every run and the "no duplicate found" branch filed anyway.
	if !strings.Contains(step, "LOOKUP_RC") || !strings.Contains(step, `"$LOOKUP_RC" -ne 0`) {
		t.Error("the dedupe lookup must capture its exit status in LOOKUP_RC and fail closed on a non-zero result")
	}

	// Round-trip matchability: agents write different prose for one defect, so the
	// filed title has to carry the same stable key the next agent searches for.
	if !strings.Contains(searchCmd, `--title-contains "$SYMPTOM_KEY"`) {
		t.Error("the dedupe lookup must search by the stable $SYMPTOM_KEY, not by free-text description")
	}
	if !strings.Contains(step[createAt:], "$SYMPTOM_KEY") {
		t.Error("the filed title must embed $SYMPTOM_KEY verbatim so the next polecat's lookup matches it")
	}
}

// TestPolecatPreflightKeysOnTestFunctionNotSubtest pins the two widenings that
// an exact-title match alone does not deliver.
//
// Go subtests make the reported name unstable across agents: two concurrent
// polecats often fail different subtests of the same function
// (`TestX/clean_config_with_residual_files_is_a_conflict` versus
// `TestX/peer_successor_cross-device_tree`), search different strings, and both
// file. Stripping at the first `/` keys them together.
//
// Sibling functions in one package are the second gap: three different
// `TestDisableAndPurge*` functions can share a single root cause (for example
// an environment leak), and no exact-name key groups them. Eight
// productmetrics beads landed in ~38h that way.
func TestPolecatPreflightKeysOnTestFunctionNotSubtest(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	if !strings.Contains(step, "%%/*") {
		t.Error("the symptom key must strip the Go subtest suffix (${NAME%%/*}) so sibling subtests of one function share a key")
	}

	familyAt := strings.Index(step, `--title-contains "$SYMPTOM_FAMILY"`)
	if familyAt < 0 {
		t.Error("preflight-tests must widen to a $SYMPTOM_FAMILY lookup; sibling tests in one package usually share one root cause")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt >= 0 && familyAt > createAt {
		t.Error("the family lookup must run before `gc bd create`, not after it")
	}

	// The family branch is a judgement call, so nothing auto-assigns the match —
	// but the step must still tell the agent how to hand a family hit to 3c, or
	// 3c's `gc bd comment "$EXISTING"` runs with an empty id.
	if !strings.Contains(step, `EXISTING="<the bead id you judged to be the same defect>"`) {
		t.Error("3b2 must show how to set $EXISTING for a family match; 3c cannot comment without it")
	}
	if !strings.Contains(step, "FAMILY_RC") {
		t.Error("the family lookup must capture its exit status; a failed lookup is not an all-clear")
	}
}

// TestPolecatPreflightChecksRecentlyClosedBeforeFiling pins the staleness gate.
//
// Re-filing a defect that already merged puts a ready bead in the pool, and the
// next sling dispatches a polecat to redo merged work.
func TestPolecatPreflightChecksRecentlyClosedBeforeFiling(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	closedAt := strings.Index(step, "--closed-after")
	if closedAt < 0 {
		t.Fatal("preflight-tests must check recently-closed beads before filing; a stale baseline otherwise re-files merged work")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt >= 0 && closedAt > createAt {
		t.Error("the recently-closed check must run before `gc bd create`, not after it")
	}
	if !strings.Contains(step, "CLOSED_RC") {
		t.Error("the recently-closed lookup must capture its exit status; a failed lookup is not proof the fix has not landed")
	}
}

// TestPolecatSelfReviewDefersToPreflightDedupeProtocol keeps the second
// pre-existing-failure filing path pointed at the one protocol.
//
// self-review also tells the polecat to file a bead when a failure turns out to
// be pre-existing. Restating the protocol there would let the two copies drift;
// referring to preflight-tests keeps one definition.
func TestPolecatSelfReviewDefersToPreflightDedupeProtocol(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "self-review")

	if !strings.Contains(step, "preflight-tests") {
		t.Error("self-review's pre-existing-failure path must point at the preflight-tests search-first protocol instead of filing directly")
	}
	if strings.Contains(step, "gc bd create") {
		t.Error("self-review must not carry its own `gc bd create` for pre-existing failures; that bypasses the dedupe protocol")
	}
}

// TestCoreShippedAssetsAvoidNonexistentBDListSearchFlag guards the failure mode
// that made the sibling fix a no-op: `gc bd list` has no `--search` flag, so a
// dedupe lookup written against it exits 1 and returns nothing, which reads as
// "no duplicate exists" to the branch that follows. Search by --title-contains.
func TestCoreShippedAssetsAvoidNonexistentBDListSearchFlag(t *testing.T) {
	err := fs.WalkDir(PackFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(PackFS, path)
		if err != nil {
			return err
		}
		body := string(data)
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "bd list") && strings.Contains(line, "--search") {
				t.Errorf("%s: `bd list --search` is not a real flag and exits 1; use --title-contains: %s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded core pack: %v", err)
	}
}
