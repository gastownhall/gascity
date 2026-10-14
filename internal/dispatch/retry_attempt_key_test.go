package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// The retry counter lives in gc.retry_attempt; gc.attempt keeps its v1.4.2
// meaning (the iteration) on everything inside a ralph body. These tests pin
// both halves of that contract at the points where they used to collide.

// makeBodyRetryControl mints a retry control as a child of ralph iteration
// `iteration`, the shape buildAttemptRecipe produces for a body child with a
// retry spec. attemptKey is what the control carries in gc.attempt, so a test
// can reproduce a bead minted by an older binary.
func makeBodyRetryControl(t *testing.T, store beads.Store, iteration, attemptKey string, maxAttempts int) (root, control beads.Bead, stepRef string) {
	t.Helper()
	spec := &formula.Step{
		ID:    "apply-fixes",
		Title: "Apply fixes",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: maxAttempts},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal step spec: %v", err)
	}
	stepRef = "mol-adopt-pr-v2.review-loop.iteration." + iteration + ".apply-fixes"
	root = mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	meta := map[string]string{
		"gc.kind":             "retry",
		"gc.root_bead_id":     root.ID,
		"gc.step_ref":         stepRef,
		"gc.step_id":          "apply-fixes",
		"gc.max_attempts":     strconv.Itoa(maxAttempts),
		"gc.on_exhausted":     "hard_fail",
		"gc.source_step_spec": string(specJSON),
		"gc.control_epoch":    "1",
		"gc.attempt":          attemptKey,
	}
	if iteration != "" {
		meta["gc.iteration"] = iteration
	}
	control = mustCreate(t, store, beads.Bead{Title: "Apply fixes (retry)", Metadata: meta})
	return root, control, stepRef
}

func failTransient(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.SetMetadataBatch(id, map[string]string{
		"gc.outcome":        "fail",
		"gc.failure_class":  "transient",
		"gc.failure_reason": "flake",
	}); err != nil {
		t.Fatalf("set transient outcome on %s: %v", id, err)
	}
	mustClose(t, store, id)
}

// TestRetryInsideLaterIterationGetsItsFullRetryBudget is ga-v7pu5 at the
// behavioral level: a retry first reached in iteration 3 must still get all
// max_attempts attempts. When the counter was read from gc.attempt it started
// at 3 and exhausted on its first failure.
func TestRetryInsideLaterIterationGetsItsFullRetryBudget(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, control, stepRef := makeBodyRetryControl(t, store, "3", "3", 3)

	first := makeAttemptBead(t, store, root.ID, stepRef+".attempt.1", 3, map[string]string{
		"gc.iteration":     "3",
		"gc.retry_attempt": "1",
		"gc.control_for":   stepRef,
		"gc.outcome":       "fail",
		"gc.failure_class": "transient",
	})
	mustDep(t, store, control.ID, first.ID, "blocks")

	for n := 1; n <= 3; n++ {
		result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
		if err != nil {
			t.Fatalf("round %d: %v", n, err)
		}
		want := "retry"
		if n == 3 {
			want = "fail"
		}
		if result.Action != want {
			t.Fatalf("round %d action = %q, want %q — the retry budget is %d attempts in every iteration", n, result.Action, want, 3)
		}
		if n == 3 {
			break
		}
		next := findAttemptByRef(t, store, root.ID, stepRef+".attempt."+strconv.Itoa(n+1))
		if next.ID == "" {
			t.Fatalf("attempt %d was not spawned", n+1)
		}
		if got := next.Metadata["gc.retry_attempt"]; got != strconv.Itoa(n+1) {
			t.Errorf("attempt %d gc.retry_attempt = %q, want %d", n+1, got, n+1)
		}
		if got := next.Metadata["gc.attempt"]; got != "3" {
			t.Errorf("attempt %d gc.attempt = %q, want 3 (the iteration pack gates join on)", n+1, got)
		}
		if got := next.Metadata["gc.iteration"]; got != "3" {
			t.Errorf("attempt %d gc.iteration = %q, want 3", n+1, got)
		}
		failTransient(t, store, next.ID)
	}

	final := mustGet(t, store, control.ID)
	if final.Metadata["gc.failed_attempt"] != "3" {
		t.Errorf("gc.failed_attempt = %q, want 3 (the retry counter, not the iteration)", final.Metadata["gc.failed_attempt"])
	}
	var log []map[string]string
	if err := json.Unmarshal([]byte(final.Metadata["gc.attempt_log"]), &log); err != nil {
		t.Fatalf("unmarshal attempt_log: %v", err)
	}
	if len(log) != 3 {
		t.Fatalf("attempt_log entries = %d, want 3", len(log))
	}
}

// TestRetryAttemptKeyUpgradeFromV142InFlightMolecule pins the fallback for a
// molecule minted by v1.4.2: no gc.retry_attempt, no gc.iteration, and the
// conflated counter in gc.attempt. The new binary must read that value exactly
// as v1.4.2 did — no lost retries, no wedge — and keep writing the v1.4.2 shape
// for the attempts it spawns into that molecule.
func TestRetryAttemptKeyUpgradeFromV142InFlightMolecule(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, control, stepRef := makeBodyRetryControl(t, store, "", "2", 3)

	// v1.4.2 stamped the iteration (2) as the attempt of iteration 2's attempt.1.
	first := makeAttemptBead(t, store, root.ID, stepRef+".attempt.1", 2, map[string]string{
		"gc.control_for":   stepRef,
		"gc.outcome":       "fail",
		"gc.failure_class": "transient",
	})
	mustDep(t, store, control.ID, first.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if result.Action != "retry" {
		t.Fatalf("round 1 action = %q, want retry (v1.4.2 would retry from 2 to 3)", result.Action)
	}
	next := findAttemptByRef(t, store, root.ID, stepRef+".attempt.3")
	if next.ID == "" {
		t.Fatal("attempt 3 was not spawned — the legacy counter was not honored")
	}
	if next.Metadata["gc.retry_attempt"] != "3" || next.Metadata["gc.attempt"] != "3" {
		t.Errorf("spawned attempt counters = retry_attempt %q attempt %q, want 3 and 3", next.Metadata["gc.retry_attempt"], next.Metadata["gc.attempt"])
	}
	failTransient(t, store, next.ID)
	result, err = processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if result.Action != "fail" {
		t.Fatalf("round 2 action = %q, want fail (exhausted at 3, as on v1.4.2)", result.Action)
	}
}

// TestRetryAttemptKeyUpgradeFromPreKeyV150Molecule pins the fallback for a
// molecule minted by a v1.5.0 build between #5635 and this change. Real
// dispatch froze the body spec retry-expanded, so the body retry control has
// no retry spec of its own and already carried gc.attempt = the iteration;
// only its attempts carried their own retry counter in gc.attempt (and no
// gc.retry_attempt). Retries continue from that counter, and new attempts use
// the new shape.
func TestRetryAttemptKeyUpgradeFromPreKeyV150Molecule(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, control, stepRef := makeBodyRetryControl(t, store, "3", "3", 3)

	first := makeAttemptBead(t, store, root.ID, stepRef+".attempt.1", 1, map[string]string{
		"gc.iteration":     "3",
		"gc.control_for":   stepRef,
		"gc.outcome":       "fail",
		"gc.failure_class": "transient",
	})
	mustDep(t, store, control.ID, first.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if result.Action != "retry" {
		t.Fatalf("round 1 action = %q, want retry", result.Action)
	}
	second := findAttemptByRef(t, store, root.ID, stepRef+".attempt.2")
	if second.ID == "" {
		t.Fatal("attempt 2 was not spawned")
	}
	if second.Metadata["gc.retry_attempt"] != "2" || second.Metadata["gc.attempt"] != "3" {
		t.Errorf("attempt 2 counters = retry_attempt %q attempt %q, want 2 and 3", second.Metadata["gc.retry_attempt"], second.Metadata["gc.attempt"])
	}
	if err := store.SetMetadataBatch(second.ID, map[string]string{"gc.outcome": "pass", "review.verdict": "done"}); err != nil {
		t.Fatalf("set pass: %v", err)
	}
	mustClose(t, store, second.ID)
	result, err = processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("round 2 action = %q, want pass", result.Action)
	}
	final := mustGet(t, store, control.ID)
	if final.Metadata["gc.attempt"] != "3" || final.Metadata["review.verdict"] != "done" {
		t.Errorf("passed control gc.attempt = %q review.verdict = %q, want 3 and done", final.Metadata["gc.attempt"], final.Metadata["review.verdict"])
	}
}

// TestBuildAttemptRecipeAdoptPRReviewLoopJoinsOnIteration is the P0-8
// regression: the mol-adopt-pr-v2 review loop's gate joins apply-fixes beads
// on gc.attempt == iteration, and reviewer prompts and attempt-{attempt}
// artifact paths name the iteration's directory from each bead's own
// gc.attempt. apply-fixes declares its own retry, and #5635 stamped the retry
// counter there on its attempts, so in iteration 2 attempt.1 pointed at
// attempt-1/. Every iteration-2 apply-fixes bead must carry gc.attempt=2 and,
// separately on attempts, gc.retry_attempt=1.
func TestBuildAttemptRecipeAdoptPRReviewLoopJoinsOnIteration(t *testing.T) {
	t.Parallel()

	expanded, err := formula.ApplyRetries([]*formula.Step{
		{ID: "review-pipeline", Title: "Review pipeline", Type: "task"},
		{ID: "apply-fixes", Title: "Apply fixes", Type: "task", Needs: []string{"review-pipeline"}, Retry: &formula.RetrySpec{MaxAttempts: 3}},
	})
	if err != nil {
		t.Fatalf("ApplyRetries: %v", err)
	}
	step := &formula.Step{
		ID:       "review-loop",
		Title:    "Review loop",
		Type:     "task",
		Ralph:    &formula.RalphSpec{MaxAttempts: 5, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "adopt-pr-review-approved.sh"}},
		Children: expanded,
	}
	control := beads.Bead{ID: "gc-loop", Metadata: map[string]string{"gc.step_id": "review-loop", "gc.step_ref": "mol-adopt-pr-v2.review-loop"}}

	for _, iteration := range []int{2, 3} {
		recipe := buildAttemptRecipe(step, control, iteration)
		it := strconv.Itoa(iteration)
		prefix := "mol-adopt-pr-v2.review-loop.iteration." + it
		// The pack's load_verdict: step_id == apply-fixes or starts with
		// "apply-fixes.", and gc.attempt == the iteration.
		joined := map[string]bool{}
		for _, s := range recipe.Steps {
			sid := s.Metadata["gc.step_id"]
			if (sid == "apply-fixes" || strings.HasPrefix(sid, "apply-fixes.")) && s.Metadata["gc.attempt"] == it {
				joined[s.ID] = true
			}
		}
		for _, id := range []string{prefix + ".apply-fixes", prefix + ".apply-fixes.attempt.1"} {
			if !joined[id] {
				t.Errorf("iteration %d: %s does not join on gc.attempt == %d", iteration, id, iteration)
			}
		}
		first := recipe.StepByID(prefix + ".apply-fixes.attempt.1")
		if first == nil {
			t.Fatalf("iteration %d: missing apply-fixes.attempt.1", iteration)
		}
		if got := first.Metadata["gc.retry_attempt"]; got != "1" {
			t.Errorf("iteration %d: apply-fixes.attempt.1 gc.retry_attempt = %q, want 1", iteration, got)
		}
		if got := first.Metadata["gc.iteration"]; got != it {
			t.Errorf("iteration %d: apply-fixes.attempt.1 gc.iteration = %q, want %s", iteration, got, it)
		}
	}
}

// TestSpawnRalphIterationLinksBodyRetryAttemptToItsControl drives the real
// attach path for iteration 2. molecule.Attach derives gc.logical_bead_id from
// the ".attempt.<n>" suffix, and that suffix is the retry counter, not
// gc.attempt (the iteration). Without the link isRetryAttemptSubject stops
// exempting the attempt, and a transient failure with the body's
// gc.on_fail=abort_scope aborts the whole iteration instead of retrying.
func TestSpawnRalphIterationLinksBodyRetryAttemptToItsControl(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`
[workspace]
name = "test-city"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[agent]]
name = "claude"
dir = "gascity"

[[agent]]
name = "control-dispatcher"
dir = "gascity"
max_active_sessions = 1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	store := beads.NewMemStore()

	expanded, err := formula.ApplyRetries([]*formula.Step{
		{ID: "apply-fixes", Title: "Apply fixes", Type: "task", Metadata: map[string]string{"gc.run_target": "gascity/claude"}, Retry: &formula.RetrySpec{MaxAttempts: 3}},
	})
	if err != nil {
		t.Fatalf("ApplyRetries: %v", err)
	}
	spec := &formula.Step{
		ID:       "review-loop",
		Title:    "Review loop",
		Type:     "task",
		Ralph:    &formula.RalphSpec{MaxAttempts: 5, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "check.sh"}},
		Children: expanded,
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	root := mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	loop := mustCreate(t, store, beads.Bead{Title: "Review loop", Metadata: map[string]string{
		"gc.kind":                "ralph",
		"gc.root_bead_id":        root.ID,
		"gc.step_id":             "review-loop",
		"gc.step_ref":            "mol.review-loop",
		"gc.max_attempts":        "5",
		"gc.source_step_spec":    string(specJSON),
		"gc.control_epoch":       "1",
		"gc.execution_routed_to": "gascity/claude",
	}})

	if err := spawnNextAttempt(t.Context(), store, loop, 2, ProcessOptions{CityPath: cityPath}); err != nil {
		t.Fatalf("spawnNextAttempt: %v", err)
	}
	control := findAttemptByRef(t, store, root.ID, "mol.review-loop.iteration.2.apply-fixes")
	first := findAttemptByRef(t, store, root.ID, "mol.review-loop.iteration.2.apply-fixes.attempt.1")
	if control.ID == "" || first.ID == "" {
		t.Fatalf("iteration 2 not attached: control=%q attempt.1=%q", control.ID, first.ID)
	}
	if got := first.Metadata["gc.logical_bead_id"]; got != control.ID {
		t.Errorf("attempt.1 gc.logical_bead_id = %q, want its retry control %q", got, control.ID)
	}
	if first.Metadata["gc.attempt"] != "2" || first.Metadata["gc.retry_attempt"] != "1" {
		t.Errorf("attempt.1 counters = attempt %q retry_attempt %q, want 2 and 1", first.Metadata["gc.attempt"], first.Metadata["gc.retry_attempt"])
	}
	if !isRetryAttemptSubject(first) {
		t.Errorf("attempt.1 is not recognized as a retry attempt subject")
	}
}

// TestRalphRetryMemberRetryAttemptIgnoresNestedAttemptSegments pins the
// dormant clone path: only a bead whose ref ends in ".attempt.<n>" is a retry
// attempt root. A bead nested under one keeps no retry counter, and a v1.4.2
// bead's inflated gc.attempt is never read back as one.
func TestRalphRetryMemberRetryAttemptIgnoresNestedAttemptSegments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		bead beads.Bead
		want string
	}{
		{"key wins", beads.Bead{Metadata: map[string]string{"gc.step_ref": "m.loop.iteration.3.fix.attempt.2", "gc.retry_attempt": "2", "gc.attempt": "3"}}, "2"},
		{"pre-key tail ref", beads.Bead{Metadata: map[string]string{"gc.step_ref": "m.loop.iteration.3.fix.attempt.2", "gc.attempt": "4"}}, "2"},
		{"nested under attempt", beads.Bead{Metadata: map[string]string{"gc.step_ref": "m.loop.iteration.3.fix.attempt.2.check", "gc.attempt": "4"}}, ""},
		{"plain member", beads.Bead{Metadata: map[string]string{"gc.step_ref": "m.loop.iteration.3.review", "gc.attempt": "3"}}, ""},
	} {
		if got := ralphRetryMemberRetryAttempt(tc.bead); got != tc.want {
			t.Errorf("%s: ralphRetryMemberRetryAttempt = %q, want %q", tc.name, got, tc.want)
		}
	}
}
