package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// TestBuildAttemptRecipeStampsControlFor asserts W4/W5: buildAttemptRecipe
// stamps gc.control_for = control.ID on the attempt root, after the
// step.Metadata copy loop so a formula-authored value cannot shadow it.
func TestBuildAttemptRecipeStampsControlFor(t *testing.T) {
	t.Parallel()

	t.Run("top-level mint uses control bead ID", func(t *testing.T) {
		step := &formula.Step{
			ID:    "review",
			Type:  "task",
			Retry: &formula.RetrySpec{MaxAttempts: 3},
			// A formula-authored control_for must NOT survive — the stamp is
			// written after the copy loop.
			Metadata: map[string]string{"gc.control_for": "formula-authored-junk"},
		}
		control := beads.Bead{
			ID: "gc-control-1",
			Metadata: map[string]string{
				"gc.step_id":  "review",
				"gc.step_ref": "mol-test.review",
			},
		}
		recipe := buildAttemptRecipe(step, control, 2)
		root := recipe.Steps[0]
		if got := root.Metadata["gc.control_for"]; got != "gc-control-1" {
			t.Fatalf("root gc.control_for = %q, want gc-control-1 (formula value must be overridden)", got)
		}
	})

	t.Run("nested seed uses namespaced child ref", func(t *testing.T) {
		child := &formula.Step{
			ID:    "inner",
			Type:  "task",
			Ralph: &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "c.sh"}},
		}
		// buildNestedControlSeed mints via a synthetic control whose .ID is the
		// namespaced child ref; W4's stamp yields that ref.
		synthetic := beads.Bead{
			ID:       "mol.outer.iteration.2.inner",
			Metadata: map[string]string{"gc.step_id": "inner", "gc.step_ref": "mol.outer.iteration.2.inner"},
		}
		recipe := buildAttemptRecipe(child, synthetic, 1)
		root := recipe.Steps[0]
		if got := root.Metadata["gc.control_for"]; got != "mol.outer.iteration.2.inner" {
			t.Fatalf("nested seed gc.control_for = %q, want mol.outer.iteration.2.inner", got)
		}
	})
}

// controlBead is a small helper to build a retry control bead with an identity
// set (store ID, namespaced step_ref, bare step_id).
func controlBead(id, stepRef, stepID string) beads.Bead {
	return beads.Bead{ID: id, Metadata: map[string]string{
		"gc.kind":     "retry",
		"gc.step_ref": stepRef,
		"gc.step_id":  stepID,
	}}
}

// stampedAttempt builds an attempt-root candidate carrying gc.control_for.
func stampedAttempt(id, controlFor string, attempt string, kind string) beads.Bead {
	m := map[string]string{
		"gc.control_for": controlFor,
		"gc.attempt":     attempt,
	}
	if kind != "" {
		m["gc.kind"] = kind
	}
	return beads.Bead{ID: id, Metadata: m}
}

// TestLatestAttemptFromCandidatesPrimary is the T2 read-side table test: the
// primary path matches gc.control_for against the control identity set, skips
// infrastructure and molecule_failed, and selects max(gc.attempt).
func TestLatestAttemptFromCandidatesPrimary(t *testing.T) {
	control := controlBead("gc-ctl", "mol.review", "review")

	tests := []struct {
		name       string
		candidates []beads.Bead
		wantID     string
	}{
		{
			name:       "match by bead ID",
			candidates: []beads.Bead{stampedAttempt("a1", "gc-ctl", "1", "")},
			wantID:     "a1",
		},
		{
			name:       "match by step_ref",
			candidates: []beads.Bead{stampedAttempt("a1", "mol.review", "1", "")},
			wantID:     "a1",
		},
		{
			name:       "match by step_id",
			candidates: []beads.Bead{stampedAttempt("a1", "review", "1", "")},
			wantID:     "a1",
		},
		{
			name: "max attempt wins",
			candidates: []beads.Bead{
				stampedAttempt("a1", "gc-ctl", "1", ""),
				stampedAttempt("a3", "gc-ctl", "3", ""),
				stampedAttempt("a2", "gc-ctl", "2", ""),
			},
			wantID: "a3",
		},
		{
			name: "molecule_failed skipped",
			candidates: []beads.Bead{
				func() beads.Bead {
					b := stampedAttempt("a2", "gc-ctl", "2", "")
					b.Metadata["molecule_failed"] = "true"
					return b
				}(),
				stampedAttempt("a1", "gc-ctl", "1", ""),
			},
			wantID: "a1",
		},
		{
			name: "infrastructure kind carrying same control_for is not selected",
			candidates: []beads.Bead{
				// A scope-check control whose control_for equals the retry step
				// ref must NOT be picked even though it has a higher attempt.
				stampedAttempt("chk", "mol.review", "9", "scope-check"),
				stampedAttempt("a1", "gc-ctl", "1", ""),
			},
			wantID: "a1",
		},
		{
			name:       "non-matching control_for ignored",
			candidates: []beads.Bead{stampedAttempt("a1", "gc-other", "1", "")},
			wantID:     "", // no primary match, no legacy ref → empty
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := latestAttemptFromCandidates(control, tc.candidates)
			if got.ID != tc.wantID {
				t.Fatalf("latestAttemptFromCandidates = %q, want %q", got.ID, tc.wantID)
			}
		})
	}
}

// TestLatestAttemptShadowParity is the I1 deletion-gate check (T3): whenever a
// candidate set carries stamps, the primary path selects exactly what the
// legacy ref-string cascade would have.
func TestLatestAttemptShadowParity(t *testing.T) {
	control := controlBead("gc-ctl", "mol-feature.review", "review")

	// Build candidates that BOTH the legacy ref parser and the stamp resolve:
	// step_ref shaped as the legacy cascade expects AND carrying the stamp.
	mk := func(id, stepRef, attempt string) beads.Bead {
		return beads.Bead{ID: id, Metadata: map[string]string{
			"gc.step_ref":    stepRef,
			"gc.attempt":     attempt,
			"gc.control_for": "gc-ctl",
		}}
	}
	candidates := []beads.Bead{
		mk("a1", "mol-feature.review.attempt.1", "1"),
		mk("a2", "mol-feature.review.attempt.2", "2"),
	}

	primary := latestAttemptFromCandidates(control, candidates)
	legacy := latestAttemptFromCandidatesLegacyRefSurgery(control, candidates)
	if primary.ID != legacy.ID {
		t.Fatalf("shadow parity broken: primary=%q legacy=%q", primary.ID, legacy.ID)
	}
	if primary.ID != "a2" {
		t.Fatalf("primary selected %q, want a2", primary.ID)
	}
}

// TestLatestAttemptLegacyFallbackForUnstamped is T4: candidates minted before
// the stamp (no gc.control_for) still resolve via the guarded legacy cascade,
// and the in-process legacy-hit counter advances so the deletion-gate tests
// can observe legacy usage.
func TestLatestAttemptLegacyFallbackForUnstamped(t *testing.T) {
	control := controlBead("gc-ctl", "mol-feature.review", "review")

	// Pre-stamp shapes: legacy ref surgery only.
	candidates := []beads.Bead{
		{ID: "a1", Metadata: map[string]string{"gc.step_ref": "mol-feature.review.attempt.1", "gc.attempt": "1"}},
		{ID: "a2", Metadata: map[string]string{"gc.step_ref": "mol-feature.review.attempt.2", "gc.attempt": "2"}},
	}

	before := legacyAttemptLineageHitCount()
	got := latestAttemptFromCandidates(control, candidates)
	if got.ID != "a2" {
		t.Fatalf("legacy fallback selected %q, want a2", got.ID)
	}
	if after := legacyAttemptLineageHitCount(); after <= before {
		t.Fatalf("legacy-hit counter did not advance: before=%d after=%d", before, after)
	}
}

// TestLatestAttemptStampedShapesNeverHitLegacyCascade is the executable form of
// the S38 Phase-4 deletion gate: over post-S38 candidate shapes — every
// attempt/iteration root carries a gc.control_for stamp matching its control's
// identity — latestAttemptFromCandidates resolves on the primary equality path
// and never falls through to latestAttemptFromCandidatesLegacyRefSurgery, so
// legacyAttemptLineageHitCount() stays unchanged. This is the "stays at zero
// over post-S38 candidate shapes" guarantee the legacyAttemptLineageHits comment
// relies on; the advancing-counter test (unstamped shapes) and the shadow-parity
// tests (which call the legacy cascade directly) do not prove it. Serial (no
// t.Parallel) so the package-global counter delta is observed without
// interference — dispatch spawns no background goroutine that touches it, and Go
// defers every parallel test until the serial phase completes.
func TestLatestAttemptStampedShapesNeverHitLegacyCascade(t *testing.T) {
	simpleControl := controlBead("gc-ctl", "mol-feature.review", "review")
	iter1Control := controlBead(
		"mol.review-loop.iteration.1.inner",
		"review-loop.iteration.1.inner", "inner")
	iter2Control := controlBead(
		"mol.review-loop.iteration.2.inner",
		"mol.review-loop.iteration.2.inner", "inner")

	// Fully-stamped candidates covering the simple and nested shapes. Each control
	// matches its own attempt roots on the gc.control_for identity, so the primary
	// path returns non-empty and the legacy cascade is never reached. No
	// gc.step_ref is set, so the counter can only move if a primary match is
	// missing — exactly what this gate forbids for stamped shapes.
	stamped := []beads.Bead{
		stampedAttempt("a1", "gc-ctl", "1", ""),
		stampedAttempt("a2", "gc-ctl", "2", ""),
		stampedAttempt("i1a1", "review-loop.iteration.1.inner", "1", ""),
		stampedAttempt("i1a2", "review-loop.iteration.1.inner", "2", ""),
		stampedAttempt("i2a1", "mol.review-loop.iteration.2.inner", "1", ""),
	}

	for _, tc := range []struct {
		name    string
		control beads.Bead
		wantID  string
	}{
		{"simple retry control", simpleControl, "a2"},
		{"nested outer-iteration-1 inner", iter1Control, "i1a2"},
		{"nested outer-iteration-2 inner", iter2Control, "i2a1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := legacyAttemptLineageHitCount()
			got := latestAttemptFromCandidates(tc.control, stamped)
			if got.ID != tc.wantID {
				t.Fatalf("latestAttemptFromCandidates = %q, want %q", got.ID, tc.wantID)
			}
			if after := legacyAttemptLineageHitCount(); after != before {
				t.Fatalf("stamped shape hit the deprecated cascade: legacy-hit counter advanced before=%d after=%d", before, after)
			}
		})
	}
}

// TestRemappedControlForBeadID covers the W6 remap helper: bead-ID pointers at
// re-minted beads map to the new ID; step-ref pointers and pointers outside the
// clone set return "" (left to rewriteRetryControlFor / untouched).
func TestRemappedControlForBeadID(t *testing.T) {
	t.Parallel()
	mapping := map[string]string{"old-ctl": "new-ctl"}

	if got := remappedControlForBeadID(mapping, "old-ctl"); got != "new-ctl" {
		t.Fatalf("bead-ID in mapping = %q, want new-ctl", got)
	}
	if got := remappedControlForBeadID(mapping, "mol.outer.iteration.1.inner"); got != "" {
		t.Fatalf("step-ref value = %q, want empty (not remapped)", got)
	}
	if got := remappedControlForBeadID(mapping, "some-other-bead"); got != "" {
		t.Fatalf("bead-ID outside clone set = %q, want empty", got)
	}
	if got := remappedControlForBeadID(mapping, ""); got != "" {
		t.Fatalf("empty value = %q, want empty", got)
	}
}

// TestBuildRalphRetryGraphNodeControlForRemap covers W7: a bead-ID-valued
// gc.control_for pointing at a bead re-minted in the plan is moved to
// MetadataRefs (so the applier substitutes the new ID); a step-ref-valued
// pointer stays on the string rewrite path in meta.
func TestBuildRalphRetryGraphNodeControlForRemap(t *testing.T) {
	t.Parallel()

	attemptIDs := map[string]bool{"nested-ctl": true, "subject-old": true}

	t.Run("bead-ID pointer moves to MetadataRefs", func(t *testing.T) {
		old := beads.Bead{
			ID:  "attempt-old",
			Ref: "mol.loop.iteration.1.inner.attempt.1",
			Metadata: map[string]string{
				"gc.control_for": "nested-ctl",
				"gc.attempt":     "1",
			},
		}
		node := buildRalphRetryGraphNode(old, "logical", "mol.loop.iteration.1", "mol.loop.iteration.2", 1, 2, attemptIDs, nil)
		if _, ok := node.Metadata["gc.control_for"]; ok {
			t.Fatalf("gc.control_for must be removed from Metadata when remapped, got %q", node.Metadata["gc.control_for"])
		}
		if node.MetadataRefs["gc.control_for"] != "nested-ctl" {
			t.Fatalf("MetadataRefs[gc.control_for] = %q, want nested-ctl", node.MetadataRefs["gc.control_for"])
		}
	})

	t.Run("step-ref pointer stays in Metadata", func(t *testing.T) {
		old := beads.Bead{
			ID:  "check-old",
			Ref: "mol.loop.iteration.1.check",
			Metadata: map[string]string{
				"gc.control_for": "mol.loop.iteration.1.inner",
				"gc.attempt":     "1",
			},
		}
		node := buildRalphRetryGraphNode(old, "logical", "mol.loop.iteration.1", "mol.loop.iteration.2", 1, 2, attemptIDs, nil)
		if node.MetadataRefs["gc.control_for"] != "" {
			t.Fatalf("step-ref pointer must not go to MetadataRefs, got %q", node.MetadataRefs["gc.control_for"])
		}
		if node.Metadata["gc.control_for"] == "" {
			t.Fatalf("step-ref pointer must remain in Metadata")
		}
	})
}

// TestLatestAttemptNestedControlIsolatedAcrossOuterIterations is the S38
// nested-lineage regression guard for the read side: each outer ralph
// iteration's inner control must resolve ONLY its own latest attempt, never a
// sibling outer iteration's, even when the sibling has a higher gc.attempt.
//
// Shapes match what the producers emit after the fix: outer iteration 1 is the
// compile-time seed (non-mol-prefixed inner step_ref, so its attempt roots
// carry the namespaced ref "review-loop.iteration.1.inner"); outer iteration 2
// is the runtime buildNestedControlSeed (mol-prefixed inner ref). Before the
// fix both iterations' attempt roots carried the bare "inner" stamp, so the
// iteration-2 lookup matched iteration-1's attempt.2 through the shared
// gc.step_id identity member and the max(gc.attempt) tiebreak.
func TestLatestAttemptNestedControlIsolatedAcrossOuterIterations(t *testing.T) {
	iter1Control := controlBead(
		"mol.review-loop.iteration.1.inner",
		"review-loop.iteration.1.inner", "inner")
	iter2Control := controlBead(
		"mol.review-loop.iteration.2.inner",
		"mol.review-loop.iteration.2.inner", "inner")

	candidates := []beads.Bead{
		// Outer iteration 1's inner retried once: attempt.1 and attempt.2.
		stampedAttempt("i1a1", "review-loop.iteration.1.inner", "1", ""),
		stampedAttempt("i1a2", "review-loop.iteration.1.inner", "2", ""),
		// Outer iteration 2's inner has only attempt.1 (lower than i1's max).
		stampedAttempt("i2a1", "mol.review-loop.iteration.2.inner", "1", ""),
	}

	if got := latestAttemptFromCandidates(iter1Control, candidates); got.ID != "i1a2" {
		t.Fatalf("iteration-1 inner resolved %q, want i1a2 (its own latest attempt)", got.ID)
	}
	// Decisive assertion: iteration-2 inner must not pick up iteration-1's
	// higher-numbered attempt through a shared bare step id.
	if got := latestAttemptFromCandidates(iter2Control, candidates); got.ID != "i2a1" {
		t.Fatalf("iteration-2 inner resolved %q, want i2a1 (must not match sibling iteration-1 attempt.2)", got.ID)
	}
}

// TestLatestAttemptShadowParityNested extends the I1 deletion-gate check to the
// nested-control shape the reviewers flagged: with namespaced gc.control_for
// stamps and legacy-shaped gc.step_refs present, the primary stamp path and the
// deprecated ref-string cascade must select the SAME per-iteration attempt
// root. This is the shape where a bare stamp made them diverge before S38.
func TestLatestAttemptShadowParityNested(t *testing.T) {
	iter1Control := controlBead(
		"mol.review-loop.iteration.1.inner",
		"review-loop.iteration.1.inner", "inner")
	iter2Control := controlBead(
		"mol.review-loop.iteration.2.inner",
		"mol.review-loop.iteration.2.inner", "inner")

	// Candidates carry BOTH the namespaced stamp (primary) and a legacy-shaped
	// step_ref (ref cascade), so the two paths can be compared directly.
	mk := func(id, controlFor, stepRef, attempt string) beads.Bead {
		return beads.Bead{ID: id, Metadata: map[string]string{
			"gc.control_for": controlFor,
			"gc.step_ref":    stepRef,
			"gc.attempt":     attempt,
		}}
	}
	candidates := []beads.Bead{
		mk("i1a1", "review-loop.iteration.1.inner", "review-loop.iteration.1.inner.attempt.1", "1"),
		mk("i1a2", "review-loop.iteration.1.inner", "review-loop.iteration.1.inner.attempt.2", "2"),
		mk("i2a1", "mol.review-loop.iteration.2.inner", "mol.review-loop.iteration.2.inner.attempt.1", "1"),
	}

	for _, tc := range []struct {
		name    string
		control beads.Bead
		wantID  string
	}{
		{"outer-iteration-1 inner", iter1Control, "i1a2"},
		{"outer-iteration-2 inner", iter2Control, "i2a1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := latestAttemptFromCandidates(tc.control, candidates)
			legacy := latestAttemptFromCandidatesLegacyRefSurgery(tc.control, candidates)
			if primary.ID != legacy.ID {
				t.Fatalf("shadow parity broken: primary=%q legacy=%q", primary.ID, legacy.ID)
			}
			if primary.ID != tc.wantID {
				t.Fatalf("resolved %q, want %q (own iteration's latest attempt)", primary.ID, tc.wantID)
			}
		})
	}
}
