package main

import (
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// convergeFixture is one row of the shadow harness's golden corpus. Each fixture
// is a fully specified session-tick: the facts the derivation sees, the
// tick-start compared-key snapshot, the predicted executor values, the legacy
// writes recorded this tick, and the realized tick-end snapshot. The corpus is
// derived from the truth table, not intuition.
type convergeFixture struct {
	name       string
	durable    durableFacts
	runtimeCap shadowRuntimeCapture
	start      map[string]string
	end        map[string]string
	pred       convergePredictedValues
	recorded   []legacyCompareWrite
	realCity   bool
	// wantSurviving is the number of divergences that must survive replay (i.e.
	// count against the acceptance bar) for this fixture. The clean corpus is all
	// zeros; the canary flips one to non-zero via a broken derivation.
	wantSurviving int64
}

var fixtureNow = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

// convergeCleanCorpus is the blocking CI corpus: every row must produce zero
// surviving divergences. Rows map truth-table cross-product cases and crash
// windows to named fixtures.
func convergeCleanCorpus() []convergeFixture {
	canonName := "dir/agent-1"
	return []convergeFixture{
		{
			name: "converged_steady_state_noop",
			durable: durableFacts{
				canonicalIdentity: canonName,
				primedAt:          "2026-07-08T11:00:00Z",
				promptConfigured:  true,
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue},
			start:      map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName, sessionpkg.PrimedAtMetadataKey: "2026-07-08T11:00:00Z"},
			end:        map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName, sessionpkg.PrimedAtMetadataKey: "2026-07-08T11:00:00Z"},
			realCity:   true,
		},
		{
			name: "canonical_heal_derived_only_realcity",
			durable: durableFacts{
				canonicalIdentity: "",
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue},
			start:      map[string]string{},
			// Legacy healed the canonical record this tick to the same value the
			// executor would write -> byte-identical, zero surviving divergence.
			end:      map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName},
			pred:     convergePredictedValues{canonicalInstanceName: canonName},
			recorded: []legacyCompareWrite{{key: sessionpkg.CanonicalInstanceNameMetadata, value: canonName, writer: "syncSessionBeads"}},
			realCity: true,
		},
		{
			name: "canonical_heal_with_pool_slot",
			durable: durableFacts{
				canonicalIdentity: "",
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue},
			start:      map[string]string{},
			end:        map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName, sessionpkg.CanonicalPoolSlotMetadata: "3"},
			pred:       convergePredictedValues{canonicalInstanceName: canonName, canonicalPoolSlot: "3"},
			recorded: []legacyCompareWrite{
				{key: sessionpkg.CanonicalInstanceNameMetadata, value: canonName, writer: "poolCreate"},
				{key: sessionpkg.CanonicalPoolSlotMetadata, value: "3", writer: "poolCreate"},
			},
			realCity: true,
		},
		{
			name: "canonical_singleton_heal_clears_stale_slot",
			// The canonical record is absent so the derivation stamps a SINGLETON
			// name (empty predicted slot), while a stale canonical_pool_slot from a
			// prior pooled incarnation sits in the start snapshot. Fixture mode
			// (realCity:false) assumes the heal executed, so end==apply(derived,start)
			// must clear the stale slot; without the clear this fixture flags a
			// divergence, which is the canary for the S19 singleton-heal slot leak.
			durable: durableFacts{
				canonicalIdentity: "",
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue},
			start:      map[string]string{sessionpkg.CanonicalPoolSlotMetadata: "3"},
			end:        map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName},
			pred:       convergePredictedValues{canonicalInstanceName: canonName},
			recorded: []legacyCompareWrite{
				{key: sessionpkg.CanonicalInstanceNameMetadata, value: canonName, writer: "syncSessionBeads"},
				{key: sessionpkg.CanonicalPoolSlotMetadata, value: "", writer: "syncSessionBeads"},
			},
			realCity: false,
		},
		{
			name: "absent_closed_bead_no_heal",
			durable: durableFacts{
				canonicalIdentity: "",
				absent:            true,
				now:               fixtureNow,
			},
			// Unobserved runtime under absent intent -> derivation is empty.
			runtimeCap: shadowRuntimeCapture{probeSite: "orphan", probeTarget: canonName, runtimePresent: convergeTriFalse, processAlive: convergeTriFalse},
			start:      map[string]string{},
			end:        map[string]string{},
			realCity:   true,
		},
		{
			name: "rollback_absent_live_runtime_realcity",
			durable: durableFacts{
				absent: true,
				now:    fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "orphan", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue},
			start:      map[string]string{},
			end:        map[string]string{}, // rollback writes no compared key
			realCity:   true,
		},
		{
			name: "priming_attempt_fixture_only",
			durable: durableFacts{
				canonicalIdentity: canonName,
				promptConfigured:  true,
				primedAt:          "",
				currentPromptHash: "hash-v1",
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue},
			start:      map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName},
			end: map[string]string{
				sessionpkg.CanonicalInstanceNameMetadata: canonName,
				sessionpkg.PrimingAttemptedAtMetadataKey: "2026-07-08T12:00:00Z",
				sessionpkg.PromptHashMetadataKey:         "hash-v1",
			},
			pred: convergePredictedValues{
				primingAttemptedAt: "2026-07-08T12:00:00Z",
				promptHash:         "hash-v1",
			},
			recorded: []legacyCompareWrite{
				{key: sessionpkg.PrimingAttemptedAtMetadataKey, value: "2026-07-08T12:00:00Z", writer: "attemptPrime"},
				{key: sessionpkg.PromptHashMetadataKey, value: "hash-v1", writer: "attemptPrime"},
			},
			realCity: false, // fixtures compare the full owned set incl. priming
		},
		{
			name: "priming_stamp_from_runtime_fixture_only",
			durable: durableFacts{
				canonicalIdentity: canonName,
				promptConfigured:  true,
				primedAt:          "",
				currentPromptHash: "hash-v2",
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue, primedEnv: true},
			start:      map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName},
			end: map[string]string{
				sessionpkg.CanonicalInstanceNameMetadata: canonName,
				sessionpkg.PrimedAtMetadataKey:           "2026-07-08T12:00:00Z",
				sessionpkg.PromptHashMetadataKey:         "hash-v2",
			},
			pred: convergePredictedValues{
				primedAt:   "2026-07-08T12:00:00Z",
				promptHash: "hash-v2",
			},
			recorded: []legacyCompareWrite{
				{key: sessionpkg.PrimedAtMetadataKey, value: "2026-07-08T12:00:00Z", writer: "stampPrimed"},
				{key: sessionpkg.PromptHashMetadataKey, value: "hash-v2", writer: "stampPrimed"},
			},
			realCity: false,
		},
		{
			name: "canonical_absent_derived_only_no_legacy_write_realcity",
			// The canonical record is absent and legacy does NOT heal it this tick
			// (per-tick heal is the derived-only future behavior). The derivation
			// wants to stamp; in shadow it is NOT executed, so end stays absent.
			// This must produce ZERO divergences (no unrealized-prediction flood).
			durable: durableFacts{
				canonicalIdentity: "",
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue},
			start:      map[string]string{},
			end:        map[string]string{}, // legacy did not write; shadow did not execute
			pred:       convergePredictedValues{canonicalInstanceName: canonName},
			realCity:   true,
		},
		{
			name: "priming_excluded_on_realcity_no_divergence",
			// primedEnv is unobservable on real cities (pinned false); the runtime
			// legacy-primed this incarnation but the durable marker is absent. On a
			// real city this must NOT flag — priming is excluded.
			durable: durableFacts{
				canonicalIdentity: canonName,
				promptConfigured:  true,
				primedAt:          "",
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: canonName, runtimePresent: convergeTriTrue, processAlive: convergeTriTrue, primedEnv: false},
			start:      map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName},
			end:        map[string]string{sessionpkg.CanonicalInstanceNameMetadata: canonName},
			realCity:   true,
		},
	}
}

// runFixture evaluates one fixture through a fresh tick collector with the
// harness force-enabled, and returns the resulting counter snapshot.
func runFixture(t *testing.T, f convergeFixture) convergeCounterSnapshot {
	t.Helper()
	t.Setenv("GC_CONVERGE_SHADOW", "1")
	counters := newConvergeShadowCounters()
	tick := newConvergeShadowTick("observer-test", 1, fixtureNow, f.realCity, counters)
	if tick == nil {
		t.Fatal("newConvergeShadowTick returned nil with harness enabled")
	}
	const sid = "sess-1"
	tick.captureDurable(sid, "tok-1", f.runtimeCap.probeTarget, f.durable, f.start, f.pred)
	tick.captureRuntime(sid, f.runtimeCap.probeSite, f.runtimeCap.probeTarget, f.runtimeCap.runtimePresent, f.runtimeCap.processAlive)
	// Preserve primedEnv from the fixture (captureRuntime does not carry it).
	tick.evals[sid].runtimeCap.primedEnv = f.runtimeCap.primedEnv
	// Feed recorded legacy writes.
	for _, w := range f.recorded {
		tick.recorder.record(sid, w.writer, map[string]string{w.key: w.value})
	}
	tick.finish(map[string]map[string]string{sid: f.end})
	return counters.snapshot()
}

// TestConvergeShadowCleanCorpus is the blocking CI gate: every fixture in the
// clean corpus produces zero surviving divergences and a proven denominator.
func TestConvergeShadowCleanCorpus(t *testing.T) {
	for _, f := range convergeCleanCorpus() {
		t.Run(f.name, func(t *testing.T) {
			snap := runFixture(t, f)
			if got := snap.survivingDivergences(); got != f.wantSurviving {
				t.Errorf("%s: surviving divergences = %d, want %d (classes: %v)", f.name, got, f.wantSurviving, snap.DivergenceTotal)
			}
			if snap.SessionsEvaluated == 0 && snap.Incomparable == 0 {
				t.Errorf("%s: nothing evaluated — flatlined denominator is a harness failure, not a pass", f.name)
			}
			if snap.RecordsDropped != 0 {
				t.Errorf("%s: records_dropped = %d, must be 0", f.name, snap.RecordsDropped)
			}
		})
	}
}

// TestConvergeShadowSeededMutationCanary injects a deliberately broken derivation
// and asserts the comparator TRIPS within one tick on the affected fixtures. A
// dead comparator and a perfect derivation both report 0 divergences; this proves
// which one we have. Required pre-soak self-test (3c) — wired to gates.canary.
func TestConvergeShadowSeededMutationCanary(t *testing.T) {
	// Seed 1: drop actionStampCanonicalIdentity. The heal fixture must flag: the
	// legacy path stamped the canonical record (end != start) but the crippled
	// derivation predicts no write -> unpredicted_delta (recorder explains it).
	t.Run("drop_canonical_stamp", func(t *testing.T) {
		// The fixture is the CORRECT converged world (canonical present at tick
		// end), reached under fixture/execution semantics (realCity=false). The
		// broken derivation "forgot to heal": it claims the record is already
		// present (durable.canonicalIdentity set) so it emits no stamp, even though
		// the record was absent at tick start. The full oracle must flag the
		// realized-but-unpredicted canonical delta within this one tick.
		f := convergeFixture{
			name: "canary_drop_canonical",
			durable: durableFacts{
				canonicalIdentity: "dir/agent-1", // broken: derivation emits nothing
				now:               fixtureNow,
			},
			runtimeCap: shadowRuntimeCapture{probeSite: "desired", probeTarget: "dir/agent-1", runtimePresent: convergeTriTrue, processAlive: convergeTriTrue},
			start:      map[string]string{}, // record was absent at tick start
			end:        map[string]string{sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-1"},
			pred:       convergePredictedValues{canonicalInstanceName: "dir/agent-1"},
			recorded:   []legacyCompareWrite{{key: sessionpkg.CanonicalInstanceNameMetadata, value: "dir/agent-1", writer: "syncSessionBeads"}},
			realCity:   false, // fixture/execution semantics -> full oracle
		}
		snap := runFixture(t, f)
		if snap.survivingDivergences() == 0 {
			t.Fatalf("canary did not trip: comparator is dead (divergences: %v)", snap.DivergenceTotal)
		}
	})

	// Seed 2: emit a WRONG canonical slot. Legacy stamped slot 3; a broken
	// executor prediction of slot 9 must surface value_mismatch.
	t.Run("wrong_slot_value_mismatch", func(t *testing.T) {
		snap := runFixtureWrongSlot(t)
		if snap.DivergenceTotal[divergenceValueMismatch] == 0 {
			t.Fatalf("canary did not trip on wrong slot: %v", snap.DivergenceTotal)
		}
	})
}

// runFixtureWrongSlot models a derivation that predicts the wrong canonical slot
// value than legacy actually wrote.
func runFixtureWrongSlot(t *testing.T) convergeCounterSnapshot {
	t.Helper()
	t.Setenv("GC_CONVERGE_SHADOW", "1")
	counters := newConvergeShadowCounters()
	tick := newConvergeShadowTick("observer-test", 1, fixtureNow, true, counters)
	const sid = "sess-1"
	tick.captureDurable(sid, "tok-1", "dir/agent-1",
		durableFacts{canonicalIdentity: "", now: fixtureNow},
		map[string]string{},
		convergePredictedValues{canonicalInstanceName: "dir/agent-1", canonicalPoolSlot: "9"}, // WRONG: predicts 9
	)
	tick.captureRuntime(sid, "desired", "dir/agent-1", convergeTriTrue, convergeTriTrue)
	tick.recorder.record(sid, "poolCreate", map[string]string{
		sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-1",
		sessionpkg.CanonicalPoolSlotMetadata:     "3",
	})
	tick.finish(map[string]map[string]string{sid: {
		sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-1",
		sessionpkg.CanonicalPoolSlotMetadata:     "3", // legacy wrote 3
	}})
	return counters.snapshot()
}

// TestConvergeShadowDisabledIsNoop asserts the harness is byte-identically inert
// when GC_CONVERGE_SHADOW is unset: newConvergeShadowTick returns nil, every
// method is a no-op, and the global recorder is never attached.
func TestConvergeShadowDisabledIsNoop(t *testing.T) {
	t.Setenv("GC_CONVERGE_SHADOW", "")
	tick := newConvergeShadowTick("observer", 1, fixtureNow, true, newConvergeShadowCounters())
	if tick != nil {
		t.Fatal("expected nil tick when harness disabled")
	}
	// Nil-method safety.
	tick.captureDurable("s", "t", "n", durableFacts{}, nil, convergePredictedValues{})
	tick.captureRuntime("s", "site", "n", convergeTriTrue, convergeTriTrue)
	tick.markSkip("s", skipEarlyContinue)
	tick.finish(nil)
	if convergeGlobalRecorder.Load() != nil {
		t.Fatal("global recorder must not be attached when disabled")
	}
	// The write-site wrapper must be a no-op with no recorder attached.
	recordLegacyCompareWrites("s", "writer", map[string]string{sessionpkg.CanonicalInstanceNameMetadata: "x"})
}

// TestConvergeForeignWriteDetected asserts an owned-key delta with no recorder
// entry and no derived prediction is attributed FOREIGN_WRITE (its own lane).
func TestConvergeForeignWriteDetected(t *testing.T) {
	dv := evaluateStateDiffOracle("s", convergeCanonicalOwnedKeys,
		map[string]string{}, // start: absent
		map[string]string{sessionpkg.CanonicalInstanceNameMetadata: "surprise"}, // end: appeared
		nil,                       // derivation predicted nothing
		convergePredictedValues{}, // no predicted values
		nil,                       // no recorder entry
		false,                     // flip/fixture mode
	)
	if len(dv) != 1 || dv[0].class != divergenceForeignWrite {
		t.Fatalf("expected one foreign_write divergence, got %v", dv)
	}
}

// TestConvergeUnpredictedDeltaWhenRecorded asserts an owned-key delta the
// derivation missed but a recorder entry explains is unpredicted_delta, not
// foreign_write.
func TestConvergeUnpredictedDeltaWhenRecorded(t *testing.T) {
	dv := evaluateStateDiffOracle("s", convergeCanonicalOwnedKeys,
		map[string]string{},
		map[string]string{sessionpkg.CanonicalInstanceNameMetadata: "healed"},
		nil,
		convergePredictedValues{},
		[]legacyCompareWrite{{key: sessionpkg.CanonicalInstanceNameMetadata, value: "healed", writer: "legacy"}},
		false, // flip/fixture mode
	)
	if len(dv) != 1 || dv[0].class != divergenceUnpredictedDelta {
		t.Fatalf("expected one unpredicted_delta, got %v", dv)
	}
}

// TestConvergeShadowRecordedWriteMustMaterialize asserts the real-city
// shadowNoExecution oracle does NOT trust a recorder entry blindly: a recorded
// legacy write only explains an owned key when its value actually landed in the
// realized tick-end snapshot. A recorded write that vanished (end absent) or was
// overwritten (end holds a different value) is a foreign_write divergence, not a
// swallowed clean. This is the false-negative the harness exists to catch before
// the 3d flip records canonical writes inside the tick.
func TestConvergeShadowRecordedWriteMustMaterialize(t *testing.T) {
	const worker = "dir/worker-1"
	rec := []legacyCompareWrite{{key: sessionpkg.CanonicalInstanceNameMetadata, value: worker, writer: "syncSessionBeads"}}

	t.Run("recorded_but_end_absent_diverges", func(t *testing.T) {
		dv := evaluateStateDiffOracle("s", convergeCanonicalOwnedKeys,
			map[string]string{}, // start: absent
			map[string]string{}, // end: STILL absent — the recorded write never materialized
			nil,
			convergePredictedValues{},
			rec,  // recorder claims legacy wrote canonical=worker this tick
			true, // real-city shadow / no-execution mode
		)
		if len(dv) != 1 || dv[0].class != divergenceForeignWrite {
			t.Fatalf("expected one foreign_write for a recorded-but-unmaterialized write, got %v", dv)
		}
		if dv[0].actual != "" || dv[0].predicted != worker {
			t.Fatalf("expected predicted=%q actual=\"\", got predicted=%q actual=%q", worker, dv[0].predicted, dv[0].actual)
		}
	})

	t.Run("recorded_but_end_overwritten_diverges", func(t *testing.T) {
		dv := evaluateStateDiffOracle("s", convergeCanonicalOwnedKeys,
			map[string]string{},
			map[string]string{sessionpkg.CanonicalInstanceNameMetadata: "dir/other-9"}, // overwritten
			nil,
			convergePredictedValues{},
			rec,
			true,
		)
		if len(dv) != 1 || dv[0].class != divergenceForeignWrite {
			t.Fatalf("expected one foreign_write for a recorded-then-overwritten write, got %v", dv)
		}
	})

	t.Run("recorded_and_materialized_is_clean", func(t *testing.T) {
		dv := evaluateStateDiffOracle("s", convergeCanonicalOwnedKeys,
			map[string]string{},
			map[string]string{sessionpkg.CanonicalInstanceNameMetadata: worker}, // materialized
			[]sessConvergeAction{actionStampCanonicalIdentity},
			convergePredictedValues{canonicalInstanceName: worker},
			rec,
			true,
		)
		if len(dv) != 0 {
			t.Fatalf("a recorded write that materialized to the predicted value must be clean, got %v", dv)
		}
	})

	t.Run("materialized_but_prediction_value_mismatch_diverges", func(t *testing.T) {
		// Recorder + end agree on worker, but the derivation predicted a different
		// canonical value: C4 value parity must flag the derived-vs-realized breach.
		dv := evaluateStateDiffOracle("s", convergeCanonicalOwnedKeys,
			map[string]string{},
			map[string]string{sessionpkg.CanonicalInstanceNameMetadata: worker},
			[]sessConvergeAction{actionStampCanonicalIdentity},
			convergePredictedValues{canonicalInstanceName: "dir/wrong-2"}, // derived predicts wrong value
			rec,
			true,
		)
		if len(dv) != 1 || dv[0].class != divergenceValueMismatch {
			t.Fatalf("expected one value_mismatch for a wrong derived prediction, got %v", dv)
		}
	})
}

// TestConvergeIdentitySkewSuppressed asserts a probe-target mismatch is
// classified identity-skew (positive evidence) and never a hard divergence.
func TestConvergeIdentitySkewSuppressed(t *testing.T) {
	v := compareReplay(replayInput{
		durable:           durableFacts{canonicalIdentity: "", now: fixtureNow},
		runtime:           runtimeFacts{observed: true, live: true},
		factsProbeTarget:  "dir/agent-A",
		legacyProbeTarget: "dir/agent-B",
	})
	if len(v.divergences) != 0 {
		t.Fatalf("identity-skew must not produce hard divergences, got %v", v.divergences)
	}
	if len(v.suppressed) != 1 || v.suppressed[0] != divergenceIdentitySkew {
		t.Fatalf("expected identity_skew suppression, got %v", v.suppressed)
	}
}

// TestConvergeRollbackSuppression asserts a derived rollback that legacy deferred
// under an active tick-global coupling (budget exhausted / partial store) is
// suppressed (world_moved), not a divergence.
func TestConvergeRollbackSuppression(t *testing.T) {
	base := replayInput{
		durable: durableFacts{absent: true, now: fixtureNow},
		runtime: runtimeFacts{observed: true, live: true},
		// Legacy replay disabled -> legacy set == derived set, so no derived-absent
		// mismatch is possible; force it by making legacy replayable with a
		// non-live legacy read (legacy would NOT roll back).
		legacyReplayable: true,
		legacyValues:     durableFacts{absent: true, now: fixtureNow},
		legacyRuntime:    runtimeFacts{observed: true, live: false},
		suppression:      convergeSuppression{rollbackBudgetExhausted: true},
	}
	v := compareReplay(base)
	if len(v.divergences) != 0 {
		t.Fatalf("expected rollback suppressed, got divergences %v", v.divergences)
	}
	foundWorldMoved := false
	for _, c := range v.suppressed {
		if c == divergenceWorldMoved {
			foundWorldMoved = true
		}
	}
	if !foundWorldMoved {
		t.Fatalf("expected world_moved suppression, got %v", v.suppressed)
	}
}

// TestCompareReplayInertWithoutLegacyFacts pins the Stage-3 production contract:
// with no captured legacy-read facts (legacyReplayable=false) the action-set
// parity comparator emits NOTHING — no compared actions, no divergences, no
// suppressions — even when the facts derive a real action. Comparing the derived
// action set against itself is a tautology, so surfacing per-action compare
// counters or hollow agreement there would be a misleading soak signal. The
// separate identity-skew precondition is covered by
// TestConvergeIdentitySkewSuppressed.
func TestCompareReplayInertWithoutLegacyFacts(t *testing.T) {
	in := replayInput{
		// canonical absent => derives actionStampCanonicalIdentity; promptConfigured
		// defaults false so exactly one action derives.
		durable: durableFacts{canonicalIdentity: "", now: fixtureNow},
		runtime: runtimeFacts{observed: true, live: true},
		// legacyReplayable defaults false: the legacy branch's reads were not captured.
	}
	if len(deriveConvergeActions(in.durable, in.runtime)) == 0 {
		t.Fatal("test setup: facts must derive at least one action to prove the comparator stays inert despite real derived actions")
	}
	v := compareReplay(in)
	if len(v.comparedActions) != 0 {
		t.Errorf("comparedActions = %v, want none (no parity counters without captured legacy facts)", v.comparedActions)
	}
	if len(v.divergences) != 0 {
		t.Errorf("divergences = %v, want none (a self-comparison must not surface a divergence)", v.divergences)
	}
	if len(v.suppressed) != 0 {
		t.Errorf("suppressed = %v, want none (no comparison ran without legacy facts)", v.suppressed)
	}
}

// TestCompareReplayDetectsDerivationGapWhenReplayable proves the comparator is
// suppressed-until-facts, not dead: when the legacy branch's read facts ARE
// supplied (legacyReplayable=true), an action legacy would take that the
// derivation misses surfaces as a surviving unpredicted_delta. This is the
// capability the Stage-4/5 reader cutover will feed in production.
func TestCompareReplayDetectsDerivationGapWhenReplayable(t *testing.T) {
	v := compareReplay(replayInput{
		// Derivation reads canonical already present => derives NO stamp.
		durable: durableFacts{canonicalIdentity: "dir/agent-1", now: fixtureNow},
		runtime: runtimeFacts{observed: true, live: true},
		// Legacy actually read canonical absent => legacy WOULD stamp: a
		// legacy-present/derived-absent gap the comparator must flag.
		legacyReplayable: true,
		legacyValues:     durableFacts{canonicalIdentity: "", now: fixtureNow},
		legacyRuntime:    runtimeFacts{observed: true, live: true},
	})
	found := false
	for _, c := range v.divergences {
		if c == divergenceUnpredictedDelta {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a surviving unpredicted_delta for a legacy stamp the derivation missed, got divergences=%v suppressed=%v", v.divergences, v.suppressed)
	}
}

// TestConvergeShadowRealCityEmitsNoActionSetCompare is the production-path
// regression guard: on a real city evaluateCaptured has no separate legacy-read
// facts, so it must derive and evaluate the session (honest denominator) WITHOUT
// emitting action-set compare counters that would imply a parity check the
// harness cannot yet run. Before the fix the non-replayable comparator counted
// the derived action as a "compared" agreement; this asserts that hollow counter
// is gone while the real derivation and the owned-key oracle still run.
func TestConvergeShadowRealCityEmitsNoActionSetCompare(t *testing.T) {
	t.Setenv("GC_CONVERGE_SHADOW", "1")
	counters := newConvergeShadowCounters()
	tick := newConvergeShadowTick("observer-test", 1, fixtureNow, true /* realCity */, counters)
	if tick == nil {
		t.Fatal("newConvergeShadowTick returned nil with harness enabled")
	}
	const sid = "sess-1"
	// Canonical absent at start; legacy stamps it to the predicted value => an
	// action derives and the owned-key oracle stays clean.
	tick.captureDurable(sid, "tok-1", "dir/agent-1",
		durableFacts{canonicalIdentity: "", now: fixtureNow},
		map[string]string{},
		convergePredictedValues{canonicalInstanceName: "dir/agent-1"},
	)
	tick.captureRuntime(sid, "desired", "dir/agent-1", convergeTriTrue, convergeTriTrue)
	tick.recorder.record(sid, "poolCreate", map[string]string{
		sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-1",
	})
	tick.finish(map[string]map[string]string{sid: {
		sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-1",
	}})
	snap := counters.snapshot()

	if snap.SessionsEvaluated != 1 {
		t.Fatalf("SessionsEvaluated = %d, want 1 (denominator must stay honest)", snap.SessionsEvaluated)
	}
	if len(snap.Derived) == 0 {
		t.Fatalf("Derived = %v, want a derived action (the derivation must still run)", snap.Derived)
	}
	if len(snap.CompareTotal) != 0 {
		t.Errorf("CompareTotal = %v, want empty (no action-set parity counters without captured legacy facts)", snap.CompareTotal)
	}
	if got := snap.survivingDivergences(); got != 0 {
		t.Errorf("survivingDivergences = %d, want 0 (a clean stamp is not a divergence); classes: %v", got, snap.DivergenceTotal)
	}
}

// TestApplyDerivedToOwnedKeysIdempotent asserts applying the empty action list
// leaves the snapshot unchanged (C2 idempotence at the oracle boundary).
func TestApplyDerivedToOwnedKeysIdempotent(t *testing.T) {
	start := map[string]string{sessionpkg.CanonicalInstanceNameMetadata: "x", sessionpkg.CanonicalPoolSlotMetadata: "2"}
	end := applyDerivedToOwnedKeys(start, nil, convergePredictedValues{})
	for k, v := range start {
		if end[k] != v {
			t.Errorf("key %q: got %q want %q", k, end[k], v)
		}
	}
}

// TestApplyDerivedToOwnedKeysClearsStaleSingletonSlot pins that a singleton heal
// (empty predicted pool slot) CLEARS a stale canonical_pool_slot carried in the
// start snapshot, so {canonical_instance_name:"", canonical_pool_slot:"3"} heals
// to a singleton rather than a stray-slot pooled identity. Regression guard for
// the S19 shadow oracle preserving a stale slot on singleton canonical heal.
func TestApplyDerivedToOwnedKeysClearsStaleSingletonSlot(t *testing.T) {
	start := map[string]string{sessionpkg.CanonicalPoolSlotMetadata: "3"}
	end := applyDerivedToOwnedKeys(
		start,
		[]sessConvergeAction{actionStampCanonicalIdentity},
		convergePredictedValues{canonicalInstanceName: "dir/agent-1"}, // singleton: empty slot
	)
	if got := end[sessionpkg.CanonicalInstanceNameMetadata]; got != "dir/agent-1" {
		t.Errorf("%s = %q, want %q", sessionpkg.CanonicalInstanceNameMetadata, got, "dir/agent-1")
	}
	if got := end[sessionpkg.CanonicalPoolSlotMetadata]; got != "" {
		t.Errorf("%s = %q, want cleared (singleton heal must not keep a stale slot)", sessionpkg.CanonicalPoolSlotMetadata, got)
	}
	// The predicted end must read back as a singleton identity, not a pooled one.
	if ci := sessionpkg.CanonicalIdentityFromMetadata(end); ci.PoolSlot != 0 {
		t.Errorf("CanonicalIdentityFromMetadata(end).PoolSlot = %d, want 0 (singleton)", ci.PoolSlot)
	}
}

// TestConvergeShadowMarkSkipLeavesDenominatorOnce proves a session captured at
// loop entry and then skipped (a pre-probe early-continue tick) leaves the
// denominator with exactly ONE typed skip: the skip reason increments, the tick
// never counts as evaluated, and finish does NOT double-count it as capture_loss.
// The last part is the regression guard — markSkip must forget the session in the
// ordered set too, not just the eval map.
func TestConvergeShadowMarkSkipLeavesDenominatorOnce(t *testing.T) {
	t.Setenv("GC_CONVERGE_SHADOW", "1")
	counters := newConvergeShadowCounters()
	tick := newConvergeShadowTick("observer-test", 1, fixtureNow, true, counters)
	if tick == nil {
		t.Fatal("newConvergeShadowTick returned nil with harness enabled")
	}
	const sid = "sess-1"
	// Capture durable facts at loop entry (as the reconciler does), then skip the
	// tick before any runtime probe.
	tick.captureDurable(sid, "tok", "dir/agent-1", durableFacts{now: fixtureNow}, map[string]string{}, convergePredictedValues{})
	tick.markSkip(sid, skipEarlyContinue)
	// finish must not resurrect the skipped session as capture-loss.
	tick.finish(map[string]map[string]string{sid: {}})

	snap := counters.snapshot()
	if snap.SessionsSkipped[skipEarlyContinue] != 1 {
		t.Fatalf("skipEarlyContinue = %d, want 1", snap.SessionsSkipped[skipEarlyContinue])
	}
	if snap.SessionsSkipped[skipCaptureLoss] != 0 {
		t.Fatalf("skipCaptureLoss = %d, want 0 (skipped tick was double-counted)", snap.SessionsSkipped[skipCaptureLoss])
	}
	if snap.SessionsEvaluated != 0 {
		t.Fatalf("SessionsEvaluated = %d, want 0 (a skipped tick is never evaluated)", snap.SessionsEvaluated)
	}
}
