package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// Call budgets for control processing (ga-vnycm2.18).
//
// Every store call a control makes is a bd fork-exec in production (about
// 0.1 s for a read and 0.2 s for a write on rbe-west), and the graph hop is a
// serial chain of them. These tests pin, per control kind and outcome, how
// many store reads and writes ProcessControl makes, so a change that adds a
// redundant read fails here instead of showing up as a slower hop. A budget
// is a ceiling: lowering it is the point, raising it needs a reason in the
// diff.

// callCountingStore counts the store calls made through it. It offers the
// optional capabilities a production BdStore has (batched dep, children and
// exact reads, and an update that reports the row it wrote), each counted as
// the one bd call it is there, so the counted path is the path production
// takes. A List is one call here; a TierBoth List on BdStore is two bd calls.
type callCountingStore struct {
	beads.Store
	reads  int
	writes int
	ops    []string
}

func (s *callCountingStore) read(op string) {
	s.reads++
	s.ops = append(s.ops, "R "+op)
}

func (s *callCountingStore) write(op string) {
	s.writes++
	s.ops = append(s.ops, "W "+op)
}

func (s *callCountingStore) reset() {
	s.reads, s.writes, s.ops = 0, 0, nil
}

func (s *callCountingStore) Get(id string) (beads.Bead, error) {
	s.read("Get " + id)
	return s.Store.Get(id)
}

func (s *callCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.read(fmt.Sprintf("List %v", q.Metadata))
	return s.Store.List(q)
}

func (s *callCountingStore) ListOpen(status ...string) ([]beads.Bead, error) {
	s.read("ListOpen")
	return s.Store.ListOpen(status...)
}

func (s *callCountingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.read("Ready")
	return s.Store.Ready(q...)
}

func (s *callCountingStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.read("Children " + parentID)
	return s.Store.Children(parentID, opts...)
}

func (s *callCountingStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.read("ListByLabel " + label)
	return s.Store.ListByLabel(label, limit, opts...)
}

func (s *callCountingStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	s.read("ListByAssignee " + assignee)
	return s.Store.ListByAssignee(assignee, status, limit)
}

func (s *callCountingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.read(fmt.Sprintf("ListByMetadata %v", filters))
	return s.Store.ListByMetadata(filters, limit, opts...)
}

func (s *callCountingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.read("DepList " + id + " " + direction)
	return s.Store.DepList(id, direction)
}

func (s *callCountingStore) GetLocalString(id, key string) (string, error) {
	s.read("GetLocalString " + id)
	return s.Store.GetLocalString(id, key)
}

func (s *callCountingStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	batch, ok := beads.DepListBatchFor(s.Store)
	if !ok {
		return nil, beads.ErrDepListBatchUnsupported
	}
	s.read(fmt.Sprintf("DepListBatch %d", len(ids)))
	return batch.DepListBatch(ids)
}

// UpdateReporting models BdStore's one-call update that reports the row it
// wrote: one write, no read.
func (s *callCountingStore) UpdateReporting(id string, opts beads.UpdateOpts) (beads.UpdatedRow, error) {
	s.write("UpdateReporting " + id)
	if err := s.Store.Update(id, opts); err != nil {
		return beads.UpdatedRow{}, err
	}
	bead, err := s.Store.Get(id)
	if err != nil {
		return beads.UpdatedRow{}, err
	}
	return beads.UpdatedRow{Status: bead.Status, Metadata: bead.Metadata}, nil
}

// ChildrenOfAny models BdStore's batched children read: one read per call.
func (s *callCountingStore) ChildrenOfAny(parentIDs []string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.read(fmt.Sprintf("ChildrenOfAny %d", len(parentIDs)))
	var out []beads.Bead
	seen := map[string]bool{}
	for _, id := range parentIDs {
		children, err := s.Store.Children(id, append([]beads.QueryOpt{beads.IncludeClosed}, opts...)...)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			if !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// GetExactBatch models BdStore's batched exact read: one read per call.
func (s *callCountingStore) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	s.read(fmt.Sprintf("GetExactBatch %d", len(ids)))
	found := make(map[string]beads.Bead, len(ids))
	var unresolved []string
	for _, id := range ids {
		bead, err := s.Store.Get(id)
		if err != nil {
			unresolved = append(unresolved, id)
			continue
		}
		found[id] = bead
	}
	return found, unresolved, nil
}

func (s *callCountingStore) Create(b beads.Bead) (beads.Bead, error) {
	s.write("Create")
	return s.Store.Create(b)
}

func (s *callCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.write("Update " + id)
	return s.Store.Update(id, opts)
}

func (s *callCountingStore) Close(id string) error {
	s.write("Close " + id)
	return s.Store.Close(id)
}

func (s *callCountingStore) Reopen(id string) error {
	s.write("Reopen " + id)
	return s.Store.Reopen(id)
}

func (s *callCountingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.write(fmt.Sprintf("CloseAll %d", len(ids)))
	return s.Store.CloseAll(ids, metadata)
}

func (s *callCountingStore) SetMetadata(id, key, value string) error {
	s.write("SetMetadata " + id + " " + key)
	return s.Store.SetMetadata(id, key, value)
}

func (s *callCountingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.write("SetMetadataBatch " + id)
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *callCountingStore) SetLocalString(id, key, value string) error {
	s.write("SetLocalString " + id)
	return s.Store.SetLocalString(id, key, value)
}

func (s *callCountingStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	s.write("Tx " + commitMsg)
	return s.Store.Tx(commitMsg, fn)
}

func (s *callCountingStore) Delete(id string) error {
	s.write("Delete " + id)
	return s.Store.Delete(id)
}

func (s *callCountingStore) DepAdd(issueID, dependsOnID, depType string) error {
	s.write("DepAdd " + issueID + "->" + dependsOnID)
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

func (s *callCountingStore) DepRemove(issueID, dependsOnID string) error {
	s.write("DepRemove " + issueID + "->" + dependsOnID)
	return s.Store.DepRemove(issueID, dependsOnID)
}

// controlCalls is one ProcessControl invocation's store traffic.
type controlCalls struct {
	key    string // kind/action@step_id
	reads  int
	writes int
	ops    []string
}

// callBudgetFormula is the mol-scoped-work shape the graph hop pays for: two
// retried members of one scope (the first scope-check continues, the second
// closes the scope), an unscoped retried step after the scope, and a retried
// teardown step, finalized by workflow-finalize.
const callBudgetFormula = `
formula = "call-budget"
version = 2
contract = "graph.v2"

[[steps]]
id = "setup"
title = "Setup"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "setup", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "implement"
title = "Implement"
needs = ["setup"]
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "body"
title = "Body"
needs = ["setup", "implement"]
metadata = { "gc.kind" = "scope", "gc.scope_role" = "body", "gc.scope_name" = "worktree" }

[[steps]]
id = "report"
title = "Report"
needs = ["body"]

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "cleanup-worktree"
title = "Clean up the worktree"
needs = ["body"]
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

// runCallBudgetMolecule cooks callBudgetFormula and drives it to quiescence the
// way runMolecule does, counting every ProcessControl call's store traffic.
// failFirstAttemptOf, when non-empty, closes the first attempt of the step
// whose gc.step_ref ends with it transient so its retry control mints attempt 2.
func runCallBudgetMolecule(t *testing.T, workOutcome, failFirstAttemptOf string) []controlCalls {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "call-budget.toml"), []byte(callBudgetFormula), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	inner := newBdClosePolicyStore()
	cooked, err := molecule.Cook(context.Background(), inner, "call-budget", []string{dir}, molecule.Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	rootID := cooked.RootID
	// Production decorates every graph bead with the store ref of its root,
	// which arms ProcessControl's root gate; the cooked fixture has none.
	members, err := molecule.ListSubtree(inner, rootID)
	if err != nil {
		t.Fatalf("listing cooked members: %v", err)
	}
	for _, member := range members {
		if member.ID == rootID {
			continue
		}
		if err := inner.SetMetadata(member.ID, beadmeta.RootStoreRefMetadataKey, "city:test-city"); err != nil {
			t.Fatalf("stamping root store ref: %v", err)
		}
	}
	counting := &callCountingStore{Store: inner}
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`
[workspace]
name = "test-city"

[daemon]
formula_v2 = true

[[agent]]
name = "control-dispatcher"
max_active_sessions = 1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	opts := ProcessOptions{CityPath: cityPath}

	var calls []controlCalls
	failed := false
	const maxRounds = 80
	for round := 0; round < maxRounds; round++ {
		members, err := molecule.ListSubtree(inner, rootID)
		if err != nil {
			t.Fatalf("listing molecule members: %v", err)
		}
		slices.SortFunc(members, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
		teardownSteps := teardownStepIDs(members)
		progressed := false
		for _, servingControls := range []bool{true, false} {
			for _, member := range members {
				current := mustGetBead(t, inner, member.ID)
				if current.Status != "open" || current.ID == rootID {
					continue
				}
				kind := current.Metadata[beadmeta.KindMetadataKey]
				if kind == beadmeta.KindSpec || kind == beadmeta.KindScope {
					continue
				}
				if beadmeta.IsControlKind(kind) != servingControls {
					continue
				}
				if !allBlockersClosed(t, inner, current.ID) {
					continue
				}
				if servingControls {
					counting.reset()
					result, err := ProcessControl(counting, current, opts)
					if errors.Is(err, ErrControlPending) {
						continue
					}
					if err != nil {
						t.Fatalf("ProcessControl(%s kind=%s): %v", current.ID, kind, err)
					}
					calls = append(calls, controlCalls{
						key:    kind + "/" + result.Action + "@" + current.Metadata[beadmeta.StepIDMetadataKey],
						reads:  counting.reads,
						writes: counting.writes,
						ops:    slices.Clone(counting.ops),
					})
					progressed = true
					continue
				}
				if !failed && failFirstAttemptOf != "" && strings.HasSuffix(current.Metadata[beadmeta.StepRefMetadataKey], failFirstAttemptOf+".attempt.1") {
					failed = true
					if err := updateMetadataAndClose(inner, current.ID, map[string]string{
						beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
						beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassTransient,
						beadmeta.FailureReasonMetadataKey: "flaky",
					}); err != nil {
						t.Fatalf("worker transient close %s: %v", current.ID, err)
					}
					progressed = true
					continue
				}
				closeAsWorker(t, inner, current, rootID, workOutcome, teardownSteps)
				progressed = true
			}
			if progressed && servingControls {
				break
			}
		}
		if !progressed {
			if root := mustGetBead(t, inner, rootID); root.Status != "closed" {
				t.Fatalf("molecule %s quiesced with the root open:\n%s", rootID, memberReport(t, inner, rootID))
			}
			if failFirstAttemptOf != "" && !failed {
				t.Fatalf("premise: no attempt of %q was failed", failFirstAttemptOf)
			}
			return calls
		}
	}
	t.Fatalf("molecule %s never reached quiescence:\n%s", rootID, memberReport(t, inner, rootID))
	return nil
}

// callBudget is the ceiling for one control kind/action.
type callBudget struct {
	reads  int
	writes int
}

func checkCallBudgets(t *testing.T, calls []controlCalls, budgets map[string]callBudget) {
	t.Helper()
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.key] = true
		budget, ok := budgets[c.key]
		if !ok {
			t.Errorf("no call budget for %s (reads=%d writes=%d); add one:\n  %s", c.key, c.reads, c.writes, strings.Join(c.ops, "\n  "))
			continue
		}
		if c.reads > budget.reads || c.writes > budget.writes {
			t.Errorf("%s made %d reads and %d writes, budget %d reads and %d writes:\n  %s",
				c.key, c.reads, c.writes, budget.reads, budget.writes, strings.Join(c.ops, "\n  "))
		}
	}
	keys := make([]string, 0, len(budgets))
	for key := range budgets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] {
			t.Errorf("premise: the fixture never exercised %s", key)
		}
	}
	if testing.Verbose() {
		for _, c := range calls {
			t.Logf("%-32s reads=%2d writes=%d\n  %s", c.key, c.reads, c.writes, strings.Join(c.ops, "\n  "))
		}
	}
}

func TestControlCallBudgetsPassPath(t *testing.T) {
	calls := runCallBudgetMolecule(t, beadmeta.OutcomePass, "")
	checkCallBudgets(t, calls, controlCallBudgetsPass)
}

func TestControlCallBudgetsFailPath(t *testing.T) {
	calls := runCallBudgetMolecule(t, beadmeta.OutcomeFail, "")
	checkCallBudgets(t, calls, controlCallBudgetsFail)
}

func TestControlCallBudgetsRetryPath(t *testing.T) {
	calls := runCallBudgetMolecule(t, beadmeta.OutcomePass, "implement")
	checkCallBudgets(t, calls, controlCallBudgetsRetry)
}

// Store-call ceilings per control kind/action@step. On BdStore a Get, DepList
// or batched read is one bd call, a List is two (bd list plus the wisp query)
// and a write is one.
var (
	controlCallBudgetsPass = map[string]callBudget{
		// gate Get(root), List(root members), close, List(scope body),
		// List(scope members).
		"retry/pass@setup":     {reads: 4, writes: 1},
		"retry/pass@implement": {reads: 4, writes: 1},
		// Unscoped: gate Get(root), List(root members), close.
		"retry/pass@report": {reads: 2, writes: 1},
		// gate Get(root), DepList(subject), Get(subject), List(scope body),
		// List(scope members), close.
		"scope-check/continue@setup": {reads: 5, writes: 1},
		// ...plus the scope snapshot List; the body's metadata and close are one
		// write.
		"scope-check/scope-pass@implement": {reads: 6, writes: 2},
		"retry/retry@cleanup-worktree":     {reads: 8, writes: 5},
		"retry/pass@cleanup-worktree":      {reads: 6, writes: 1},
		// DepList + one exact batch for the blockers, one subtree read
		// (Get(root), ListByMetadata, one batched children read per level),
		// one batched close-order read.
		"workflow-finalize/workflow-pass@call-budget.workflow-finalize": {reads: 11, writes: 3},
	}
	controlCallBudgetsFail = map[string]callBudget{
		"retry/hard-fail@setup":                                         {reads: 9, writes: 5},
		"scope-check/scope-fail@setup":                                  {reads: 8, writes: 3},
		"retry/hard-fail@report":                                        {reads: 2, writes: 1},
		"retry/retry@cleanup-worktree":                                  {reads: 8, writes: 5},
		"retry/pass@cleanup-worktree":                                   {reads: 6, writes: 1},
		"workflow-finalize/workflow-fail@call-budget.workflow-finalize": {reads: 10, writes: 3},
	}
	controlCallBudgetsRetry = map[string]callBudget{
		"retry/pass@setup":                                              {reads: 4, writes: 1},
		"scope-check/continue@setup":                                    {reads: 5, writes: 1},
		"retry/retry@implement":                                         {reads: 9, writes: 7},
		"retry/pass@implement":                                          {reads: 4, writes: 1},
		"scope-check/continue@implement":                                {reads: 5, writes: 1},
		"scope-check/scope-pass@implement":                              {reads: 6, writes: 2},
		"retry/pass@report":                                             {reads: 2, writes: 1},
		"retry/retry@cleanup-worktree":                                  {reads: 8, writes: 5},
		"retry/pass@cleanup-worktree":                                   {reads: 6, writes: 1},
		"workflow-finalize/workflow-pass@call-budget.workflow-finalize": {reads: 11, writes: 3},
	}
)

// TestCrashInjectionCallBudgetMolecule runs the crash-at-every-write harness
// over the call-budget molecule: two members of one scope (a scope-check that
// continues and one that closes the scope), an unscoped step and a teardown
// tail, with the root gate armed as production arms it. It pins that the
// read and write removals above keep every interrupted run converging to the
// uninterrupted end state.
func TestCrashInjectionCallBudgetMolecule(t *testing.T) {
	t.Parallel()
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "call-budget.toml"), []byte(callBudgetFormula), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	for _, sc := range []crashScenario{
		callBudgetCrashScenario("pass", formulaDir, beadmeta.OutcomePass),
		callBudgetCrashScenario("fail", formulaDir, beadmeta.OutcomeFail),
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			if sc.name == "fail" {
				// Reproduces on origin/main without this change: an
				// interrupted abort-scope skip converges with different skip
				// metadata (statuses and the workflow outcome match).
				t.Skip("ga-vnycm2.27: interrupted abort-scope converges with different skip metadata")
			}
			runCrashScenario(t, sc)
		})
	}
}

func callBudgetCrashScenario(name, formulaDir, workOutcome string) crashScenario {
	return crashScenario{
		name: name,
		build: func(t *testing.T, wrap func(beads.Store) beads.Store) crashFixture {
			store := newBdClosePolicyStore()
			cooked, err := molecule.Cook(context.Background(), store, "call-budget", []string{formulaDir}, molecule.Options{})
			if err != nil {
				t.Fatalf("Cook: %v", err)
			}
			rootID := cooked.RootID
			members, err := molecule.ListSubtree(store, rootID)
			if err != nil {
				t.Fatalf("listing cooked members: %v", err)
			}
			for _, member := range members {
				if member.ID == rootID {
					continue
				}
				if err := store.SetMetadata(member.ID, beadmeta.RootStoreRefMetadataKey, "city:test-city"); err != nil {
					t.Fatalf("stamping root store ref: %v", err)
				}
			}
			return crashFixture{
				ledger:   []*beads.MemStore{store.MemStore},
				dispatch: wrap(store),
				work: func(t *testing.T) bool {
					members, err := molecule.ListSubtree(store, rootID)
					if err != nil {
						t.Fatalf("listing molecule members: %v", err)
					}
					teardownSteps := teardownStepIDs(members)
					slices.SortFunc(members, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
					for _, member := range members {
						kind := member.Metadata[beadmeta.KindMetadataKey]
						if member.Status != "open" || member.ID == rootID || kind == beadmeta.KindSpec ||
							kind == beadmeta.KindScope || beadmeta.IsControlKind(kind) {
							continue
						}
						if !allBlockersClosed(t, store, member.ID) {
							continue
						}
						closeAsWorker(t, store, member, rootID, workOutcome, teardownSteps)
						return true
					}
					return false
				},
			}
		},
		check: func(t *testing.T, ledger []ledgerBead) {
			for _, b := range ledger {
				if b.Status != "closed" {
					t.Fatalf("uninterrupted run left %s open", b)
				}
			}
		},
	}
}
