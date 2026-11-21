package beadstest

import (
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// readyBlockingDepTypes are the dependency types a closed blocker must satisfy
// before Ready() offers its dependent.
var readyBlockingDepTypes = []string{"blocks", "waits-for", "conditional-blocks"}

// runStepCloseReadinessTests pins, on every store, when a closed blocker that
// recorded gc.work_outcome=blocked withholds its dependent
// (beads.ReadinessWorkOutcome). A formula step (gc.step_ref) whose close
// dispatch reads as a verdict releases it, because the retry, scope-check and
// finalize controls that consume the close depend on the step and are only
// ever served from Ready(). Every other blocked close withholds it.
func runStepCloseReadinessTests(t *testing.T, newStore func() beads.Store) {
	t.Helper()

	t.Run("ReadyIncludesDependentWhenTerminalStepClosedWithWorkOutcomeBlocked", func(t *testing.T) {
		for _, outcome := range []string{beadmeta.OutcomePass, beadmeta.OutcomeFail, beadmeta.OutcomeSkipped, beadmeta.OutcomeCanceled} {
			for _, depType := range readyBlockingDepTypes {
				t.Run(outcome+"/"+depType, func(t *testing.T) {
					s := newStore()
					stamped := blockedStepClose(map[string]string{beadmeta.OutcomeMetadataKey: outcome})
					blocker, got := readyBehindClosedBlocker(t, s, depType, stamped)
					if len(got) != 1 || got[0].Title != "dependent" {
						t.Fatalf("Ready() = %v, want [dependent]: a formula step closed gc.outcome=%s must satisfy its %s dependent whatever its gc.work_outcome", titlesOf(got), outcome, depType)
					}
					after, err := s.Get(blocker.ID)
					if err != nil {
						t.Fatal(err)
					}
					for k, v := range stamped {
						if after.Metadata[k] != v {
							t.Fatalf("blocker metadata %s = %q after Ready(), want %q unchanged", k, after.Metadata[k], v)
						}
					}
				})
			}
		}
	})

	t.Run("ReadyIncludesDependentWhenDispatchReadsTheStepCloseAsAVerdict", func(t *testing.T) {
		// Dispatch trims gc.outcome and gc.on_fail before it reads them, the
		// retry control owning an attempt root retries a close with a missing
		// or unrecognized gc.outcome, and an abort_scope member's scope-check
		// fails the scope on it, so none of these closes may strand the
		// control that consumes it.
		attempt := map[string]string{
			beadmeta.StepRefMetadataKey:       "review.attempt.1",
			beadmeta.LogicalBeadIDMetadataKey: "gc-logical",
			beadmeta.AttemptMetadataKey:       "1",
			beadmeta.RetryAttemptMetadataKey:  "1",
		}
		member := map[string]string{
			beadmeta.ScopeRefMetadataKey: "body",
			beadmeta.OnFailMetadataKey:   beadmeta.OnFailAbortScope,
		}
		for name, meta := range map[string]map[string]string{
			"padded fail verdict":                        {beadmeta.OutcomeMetadataKey: " fail\t"},
			"padded canceled verdict":                    {beadmeta.OutcomeMetadataKey: "\tcanceled "},
			"retry attempt without an outcome":           attempt,
			"retry attempt with an unrecognized outcome": withMetadataValue(attempt, beadmeta.OutcomeMetadataKey, beadmeta.WorkOutcomeBlocked),
			"retry attempt with an upper-case outcome":   withMetadataValue(attempt, beadmeta.OutcomeMetadataKey, "FAIL"),
			"retry attempt carrying only its retry counter": {
				beadmeta.LogicalBeadIDMetadataKey: "gc-logical",
				beadmeta.RetryAttemptMetadataKey:  "2",
			},
			"v1 retry-run attempt without an outcome": {
				beadmeta.LogicalBeadIDMetadataKey: "gc-logical",
				beadmeta.KindMetadataKey:          beadmeta.KindRetryRun,
			},
			// A store without graph apply creates a compiled attempt before
			// its control: gc.control_for is its only lineage stamp.
			"compiled attempt naming only its control": {
				beadmeta.StepRefMetadataKey:      "review.attempt.1",
				beadmeta.ControlForMetadataKey:   "review",
				beadmeta.AttemptMetadataKey:      "1",
				beadmeta.RetryAttemptMetadataKey: "1",
			},
			"abort_scope member without an outcome":         member,
			"abort_scope member with an unknown outcome":    withMetadataValue(member, beadmeta.OutcomeMetadataKey, "unknown"),
			"padded abort_scope member without an outcome":  withMetadataValue(member, beadmeta.OnFailMetadataKey, " abort_scope "),
			"abort_scope member with an upper-case outcome": withMetadataValue(member, beadmeta.OutcomeMetadataKey, "FAIL"),
		} {
			t.Run(name, func(t *testing.T) {
				_, got := readyBehindClosedBlocker(t, newStore(), "blocks", blockedStepClose(meta))
				if len(got) != 1 || got[0].Title != "dependent" {
					t.Fatalf("Ready() = %v, want [dependent]: dispatch reads the close of a formula step with %v as a verdict, so its gc.work_outcome must not withhold the dependent", titlesOf(got), meta)
				}
			})
		}
	})

	t.Run("ReadyExcludesDependentWhenWorkBeadClosedWithWorkOutcomeBlockedWhateverItsOutcome", func(t *testing.T) {
		// Only a formula step (gc.step_ref) has a dispatch verdict in
		// gc.outcome. A plain work bead closed with gc.work_outcome=blocked
		// withholds its dependent whatever gc.outcome it carries, and
		// whatever retry or scope metadata it carries.
		for name, meta := range map[string]map[string]string{
			"no-outcome":             {},
			beadmeta.OutcomePass:     {beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass},
			beadmeta.OutcomeFail:     {beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail},
			beadmeta.OutcomeSkipped:  {beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped},
			beadmeta.OutcomeCanceled: {beadmeta.OutcomeMetadataKey: beadmeta.OutcomeCanceled},
			"attempt-shaped": {
				beadmeta.LogicalBeadIDMetadataKey: "gc-logical",
				beadmeta.AttemptMetadataKey:       "1",
			},
			"abort_scope-shaped": {beadmeta.OnFailMetadataKey: beadmeta.OnFailAbortScope},
		} {
			t.Run(name, func(t *testing.T) {
				stamped := withMetadataValue(meta, beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkOutcomeBlocked)
				_, got := readyBehindClosedBlocker(t, newStore(), "blocks", stamped)
				if len(got) != 0 {
					t.Fatalf("Ready() = %v, want empty: a work bead without gc.step_ref closed with %v must not satisfy its dependent", titlesOf(got), stamped)
				}
			})
		}
	})

	t.Run("ReadyExcludesDependentWhenStepClosedWithWorkOutcomeBlockedAndNoVerdict", func(t *testing.T) {
		// A formula step that is neither an attempt root nor an abort_scope
		// member, closed with a missing or unrecognized gc.outcome, has no
		// verdict dispatch reads: workflow-finalize, and the scope-check of a
		// member that did not opt into abort_scope, would count the close as a
		// pass. Its blocked work outcome keeps withholding the dependent, as
		// it does for a step whose attempt or scope metadata is incomplete,
		// and for a control, whose gc.control_for names the step it serves
		// rather than a control that owns it.
		cases := map[string]map[string]string{
			"no-outcome":                 {},
			"none":                       {beadmeta.OutcomeMetadataKey: "none"},
			beadmeta.OutcomeMissingRoot:  {beadmeta.OutcomeMetadataKey: beadmeta.OutcomeMissingRoot},
			"unknown":                    {beadmeta.OutcomeMetadataKey: "unknown"},
			"upper-case FAIL":            {beadmeta.OutcomeMetadataKey: "FAIL"},
			"logical bead id alone":      {beadmeta.LogicalBeadIDMetadataKey: "gc-logical"},
			"attempt without a lineage":  {beadmeta.AttemptMetadataKey: "1", beadmeta.RetryAttemptMetadataKey: "1"},
			"blank control_for":          {beadmeta.ControlForMetadataKey: " \t"},
			"member without abort_scope": {beadmeta.ScopeRefMetadataKey: "body", beadmeta.OnFailMetadataKey: "continue"},
		}
		for _, kind := range beadmeta.ControlKinds {
			cases[kind+" control naming the step it serves"] = map[string]string{
				beadmeta.KindMetadataKey:       kind,
				beadmeta.ControlForMetadataKey: "review",
			}
		}
		for name, meta := range cases {
			t.Run(name, func(t *testing.T) {
				_, got := readyBehindClosedBlocker(t, newStore(), "blocks", blockedStepClose(meta))
				if len(got) != 0 {
					t.Fatalf("Ready() = %v, want empty: a formula step closed with %v has no verdict dispatch reads, so its gc.work_outcome=blocked must withhold the dependent", titlesOf(got), meta)
				}
			})
		}
	})
}

// blockedStepClose returns meta as a formula step's close (gc.step_ref, unless
// meta names its own) that recorded gc.work_outcome=blocked.
func blockedStepClose(meta map[string]string) map[string]string {
	stamped := map[string]string{
		beadmeta.StepRefMetadataKey:     "review",
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked,
	}
	maps.Copy(stamped, meta)
	return stamped
}

// withMetadataValue returns a copy of meta with key set to value.
func withMetadataValue(meta map[string]string, key, value string) map[string]string {
	out := maps.Clone(meta)
	if out == nil {
		out = map[string]string{}
	}
	out[key] = value
	return out
}

// readyBehindClosedBlocker creates a blocker and a dependent joined by a
// depType edge, closes the blocker, stamps meta on it, and returns the blocker
// and what Ready() then offers.
func readyBehindClosedBlocker(t *testing.T, s beads.Store, depType string, meta map[string]string) (beads.Bead, []beads.Bead) {
	t.Helper()
	blocker, err := s.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := s.Create(beads.Bead{Title: "dependent", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DepAdd(dependent.ID, blocker.ID, depType); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetadataBatch(blocker.ID, meta); err != nil {
		t.Fatal(err)
	}
	got, err := s.Ready()
	if err != nil {
		t.Fatal(err)
	}
	return blocker, got
}
