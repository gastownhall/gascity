package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// S19 Stage 3 shadow-comparison harness (steps 3a–3c, OBSERVATION-ONLY).
//
// This file proves that deriveConvergeActions (Stage 1) reproduces the legacy
// per-path reconciler behavior EXACTLY, without changing any behavior. Nothing
// here executes an action, double-writes, or mutates a session bead. It only:
//
//   3a — builds per-session {durableFacts, runtimeFacts} from ALREADY-observed
//        reconciler state (no new probes, no writes);
//   3b — records legacy writes of the compared metadata keys through an
//        in-process synchronous recorder, snapshots those keys at tick start and
//        tick end (the owned-key state-diff oracle), and judges derived-vs-legacy
//        through a bounded-window replay comparator;
//   3c — increments denominator + divergence counters (no new event type), and
//        is validated by a write-site-completeness guard and a seeded-mutation
//        canary (both in *_test.go).
//
// The flip (3d double-write), the flag/kill-switch substrate (3e), and real-city
// priming coverage are explicitly NOT in this file — they are later, separately
// gated stages. The harness itself sits behind its own enable latch
// (convergeShadowEnabled), fail-closed to OFF, so a controller that never opts in
// is byte-for-byte identical to the pre-Stage-3 controller.

// convergeComparedKeys are the durable metadata keys the shadow harness compares
// between the derived action list and the legacy reconciler writes. Every
// non-test cmd/gc write of any of these keys must be wired into the recorder —
// enforced by TestConvergeCompareKeyWriteSitesWired.
var convergeComparedKeys = []string{
	sessionpkg.CanonicalInstanceNameMetadata,
	sessionpkg.CanonicalPoolSlotMetadata,
	sessionpkg.PrimedAtMetadataKey,
	sessionpkg.PrimingAttemptedAtMetadataKey,
	sessionpkg.PromptHashMetadataKey,
}

// convergeCanonicalOwnedKeys are the keys the derived converge loop will OWN
// under P4 and the ONLY keys compared on real cities in Stage 3. The priming
// keys are excluded from real-city comparison (Q1 / hardening 7:
// GC_STARTUP_PROMPT_DELIVERED is launch-env-only and unobservable in a tick, so
// a real-city priming shadow would be a permanent divergence flood). Priming is
// compared on fixtures only, via convergeFixtureOwnedKeys.
var convergeCanonicalOwnedKeys = []string{
	sessionpkg.CanonicalInstanceNameMetadata,
	sessionpkg.CanonicalPoolSlotMetadata,
}

// convergeFixtureOwnedKeys is the full owned set the fixture corpus compares —
// canonical identity PLUS the priming family. Real cities use
// convergeCanonicalOwnedKeys.
var convergeFixtureOwnedKeys = append(append([]string(nil), convergeCanonicalOwnedKeys...),
	sessionpkg.PrimedAtMetadataKey,
	sessionpkg.PrimingAttemptedAtMetadataKey,
	sessionpkg.PromptHashMetadataKey,
)

// convergeComparedKeySet is a membership set over convergeComparedKeys.
var convergeComparedKeySet = func() map[string]bool {
	m := make(map[string]bool, len(convergeComparedKeys))
	for _, k := range convergeComparedKeys {
		m[k] = true
	}
	return m
}()

// convergeShadowEnabled is the process-wide, fail-closed latch for the shadow
// harness. It is EVALUATED PER CALL — every invocation re-reads
// GC_CONVERGE_SHADOW; the value is not latched or cached (tests toggle it via
// t.Setenv, so do NOT wrap it in sync.OnceValue). An unset, empty, unparseable,
// or false value is hard OFF (legacy-only, byte-identical).
//
// This is the OBSERVER kill-switch (the 138K/day wisp-flood precedent says the
// observer needs one too). It is deliberately NOT the 3e per-city durable
// double-write flag: that substrate belongs to the flip PR (3d/3e), which is out
// of scope for this observation-only harness. An env latch adds no genschema /
// config.Agent surface and is not a liveness status file, so it does not violate
// D7 (see the D7 amendment in the S19 spec).
var convergeShadowEnabled = func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_CONVERGE_SHADOW"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// convergeDivergenceClass is a typed, machine-checkable divergence category.
// Free-text divergence labels are banned (hardening 4): every divergence is one
// of these classes, each with its own counter and triage lane.
type convergeDivergenceClass string

const (
	// divergenceUnrealizedPrediction: the derivation predicted a compared-key
	// write that the legacy path did not make this window.
	divergenceUnrealizedPrediction convergeDivergenceClass = "unrealized_prediction"
	// divergenceValueMismatch: derived and legacy both wrote a key but with
	// different values.
	divergenceValueMismatch convergeDivergenceClass = "value_mismatch"
	// divergenceUnpredictedDelta: an owned-key delta the derivation did not
	// predict but which a legacy recorder entry explains (derivation gap).
	divergenceUnpredictedDelta convergeDivergenceClass = "unpredicted_delta"
	// divergenceForeignWrite: an owned-key delta that no legacy recorder entry
	// explains — either no compared-key write was recorded for it, or a recorded
	// write did not materialize into the realized end snapshot. The start/end
	// snapshots are built from this process's in-memory bead objects, so this
	// detects only IN-PROCESS writers (the wake path, another in-process observer);
	// a truly out-of-process writer (e.g. the separate `gc prime` CLI process)
	// mutates the store, not these objects, and is not observable here. Must be
	// zero for a soak to count.
	divergenceForeignWrite convergeDivergenceClass = "foreign_write"
	// divergenceFixpointNonEmpty: re-running deriveConvergeActions on END-of-tick
	// facts returned a non-empty list (a derivation gap or a mid-tick mutation).
	divergenceFixpointNonEmpty convergeDivergenceClass = "fixpoint_non_empty"
	// divergenceIdentitySkew: the probe-target name used for fact capture and the
	// name the legacy branch probed differ (positive evidence for Stage 5 C4).
	divergenceIdentitySkew convergeDivergenceClass = "identity_skew"
	// divergenceBoundary: a threshold predicate flipped sign within the measured
	// |tickNow - branchNow| window (auto-tolerated timing noise).
	divergenceBoundary convergeDivergenceClass = "boundary"
	// divergenceWorldMoved: deterministic replay on the values legacy actually
	// read reproduced the legacy action — auto-classified and suppressed.
	divergenceWorldMoved convergeDivergenceClass = "world_moved"
)

// convergeSkipReason is a typed reason a session-tick was NOT counted as a clean
// comparison. A skipped session is never "clean" and never "divergent"; it is
// removed from the denominator so "0 divergences" always carries a proven count
// (hardening 2). capture_loss must stay 0 for a soak window to count.
type convergeSkipReason string

const (
	// skipNotComparable: derived facts and the legacy decision used different
	// probe results (e.g. one path probed, the other did not).
	skipNotComparable convergeSkipReason = "not_comparable"
	// skipCaptureLoss: a required capture (durable facts, snapshot) was missing.
	// Must be 0.
	skipCaptureLoss convergeSkipReason = "capture_loss"
	// skipEarlyContinue: the legacy loop took an early-continue path (drain-ack,
	// unknown-state) before the compared region, so there is nothing to compare.
	skipEarlyContinue convergeSkipReason = "early_continue"
	// skipRecorderContended: a concurrent city tick already owned the process-global
	// recorder for this window (the supervisor reconciles each city on its own
	// goroutine), so this tick could not record its own legacy writes. Its sessions
	// are skipped rather than scored against a recorder it does not own — an honest
	// denominator instead of a false-divergence flood.
	skipRecorderContended convergeSkipReason = "recorder_contended"
)

// convergeShadowCounters holds the in-process, monotonic Stage-3 metrics. These
// are the AUTHORITATIVE soak/flip signals (records may sample; counters never).
// No new event type is registered (Q3 / hardening 10). The zero value is ready.
type convergeShadowCounters struct {
	mu sync.Mutex

	sessionsEvaluated int64
	sessionsSkipped   map[convergeSkipReason]int64
	incomparable      int64
	recordsDropped    int64

	compareTotal    map[string]int64                  // by derived action type
	derived         map[string]int64                  // deriveConvergeActions emissions
	divergenceTotal map[convergeDivergenceClass]int64 // by class
}

// newConvergeShadowCounters returns an initialized counter set.
func newConvergeShadowCounters() *convergeShadowCounters {
	return &convergeShadowCounters{
		sessionsSkipped: map[convergeSkipReason]int64{},
		compareTotal:    map[string]int64{},
		derived:         map[string]int64{},
		divergenceTotal: map[convergeDivergenceClass]int64{},
	}
}

// convergeShadowMetrics is the process-global counter set the reconciler feeds.
// Tests use isolated instances so the global stays inert unless the harness runs.
var convergeShadowMetrics = newConvergeShadowCounters()

// convergeShadowTickSeqCounter monotonically numbers reconciler ticks that run
// the shadow harness, so a divergence record can be joined to the tick that
// enqueued its comparison (snapshot vintage).
var convergeShadowTickSeqCounter atomic.Int64

// nextConvergeShadowTickSeq returns the next monotonic shadow tick sequence.
func nextConvergeShadowTickSeq() int64 {
	return convergeShadowTickSeqCounter.Add(1)
}

// triFromBool maps a probed boolean into a resolved tri-state. Unknown is never
// produced here — it is reserved for a bit that a branch did not probe at all.
func triFromBool(b bool) convergeTriState {
	if b {
		return convergeTriTrue
	}
	return convergeTriFalse
}

func (c *convergeShadowCounters) incEvaluated() {
	c.mu.Lock()
	c.sessionsEvaluated++
	c.mu.Unlock()
}

func (c *convergeShadowCounters) incSkipped(r convergeSkipReason) {
	c.mu.Lock()
	if c.sessionsSkipped == nil {
		c.sessionsSkipped = map[convergeSkipReason]int64{}
	}
	c.sessionsSkipped[r]++
	c.mu.Unlock()
}

func (c *convergeShadowCounters) incIncomparable() {
	c.mu.Lock()
	c.incomparable++
	c.mu.Unlock()
}

func (c *convergeShadowCounters) incRecordsDropped() {
	c.mu.Lock()
	c.recordsDropped++
	c.mu.Unlock()
}

func (c *convergeShadowCounters) incDerived(action string) {
	c.mu.Lock()
	if c.derived == nil {
		c.derived = map[string]int64{}
	}
	c.derived[action]++
	c.mu.Unlock()
}

func (c *convergeShadowCounters) incCompare(actionType string) {
	c.mu.Lock()
	if c.compareTotal == nil {
		c.compareTotal = map[string]int64{}
	}
	c.compareTotal[actionType]++
	c.mu.Unlock()
}

func (c *convergeShadowCounters) incDivergence(class convergeDivergenceClass) {
	c.mu.Lock()
	if c.divergenceTotal == nil {
		c.divergenceTotal = map[convergeDivergenceClass]int64{}
	}
	c.divergenceTotal[class]++
	c.mu.Unlock()
}

// convergeCounterSnapshot is an immutable copy of the counters for assertions.
type convergeCounterSnapshot struct {
	SessionsEvaluated int64
	SessionsSkipped   map[convergeSkipReason]int64
	Incomparable      int64
	RecordsDropped    int64
	CompareTotal      map[string]int64
	Derived           map[string]int64
	DivergenceTotal   map[convergeDivergenceClass]int64
}

// snapshot returns a deep copy of the counters, safe to read concurrently.
func (c *convergeShadowCounters) snapshot() convergeCounterSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := convergeCounterSnapshot{
		SessionsEvaluated: c.sessionsEvaluated,
		Incomparable:      c.incomparable,
		RecordsDropped:    c.recordsDropped,
		SessionsSkipped:   map[convergeSkipReason]int64{},
		CompareTotal:      map[string]int64{},
		Derived:           map[string]int64{},
		DivergenceTotal:   map[convergeDivergenceClass]int64{},
	}
	for k, v := range c.sessionsSkipped {
		cp.SessionsSkipped[k] = v
	}
	for k, v := range c.compareTotal {
		cp.CompareTotal[k] = v
	}
	for k, v := range c.derived {
		cp.Derived[k] = v
	}
	for k, v := range c.divergenceTotal {
		cp.DivergenceTotal[k] = v
	}
	return cp
}

// survivingDivergences returns the total divergences that survived replay (i.e.
// the classes that count against the acceptance bar). world_moved, boundary, and
// identity_skew are suppressed / positive-evidence classes: they are counted, but
// excluded here because they do not fail the soak.
func (s convergeCounterSnapshot) survivingDivergences() int64 {
	var total int64
	for class, n := range s.DivergenceTotal {
		switch class {
		case divergenceWorldMoved, divergenceBoundary, divergenceIdentitySkew:
			// Suppressed / positive-evidence classes: counted, but not a failure.
		default:
			total += n
		}
	}
	return total
}

// operatorSummary renders a single bounded, operator-facing line describing the
// shadow soak signal: the proven denominator (evaluated sessions), the typed
// skips that keep it honest, the count of incomparable ticks, the
// surviving-divergence count that gates a soak, and dropped records. It is the
// read path (Q3: no new event type — a behind-latch line on the reconciler's
// existing stderr operator channel) that lets a live GC_CONVERGE_SHADOW soak be
// observed end to end rather than incrementing counters nothing can read.
func (s convergeCounterSnapshot) operatorSummary() string {
	var skipped int64
	for _, n := range s.SessionsSkipped {
		skipped += n
	}
	return fmt.Sprintf(
		"converge-shadow soak: evaluated=%d skipped=%d incomparable=%d surviving_divergences=%d dropped=%d",
		s.SessionsEvaluated, skipped, s.Incomparable, s.survivingDivergences(), s.RecordsDropped,
	)
}

// --- tri-state runtime facts (3a) ---------------------------------------------

// convergeTriState expresses a two-bit runtime observation where "unknown" means
// the reconciler branch that owns this session-tick did not probe that bit, so a
// derived fact built from it must not claim a value the legacy path never saw.
type convergeTriState int

const (
	convergeTriUnknown convergeTriState = iota
	convergeTriFalse
	convergeTriTrue
)

// shadowRuntimeCapture is the two-bit, tri-state runtime observation captured at
// the legacy branch's OWN probe site (never a re-probe). runtimePresent is the
// tmux/provider-present bit; processAlive is the child-process-alive bit
// (unknown on paths that only probe presence). Together they express zombies
// (present && !alive).
type shadowRuntimeCapture struct {
	probeSite      string
	probeTarget    string
	runtimePresent convergeTriState
	processAlive   convergeTriState
	// primedEnv is pinned false on real cities (unobservable in a tick); fixtures
	// set it explicitly.
	primedEnv bool
}

// runtimeFacts projects the tri-state capture into the Stage-1 runtimeFacts the
// derivation consumes. observed is true only when at least the presence bit was
// probed; live is present && alive treated conservatively (unknown alive on a
// present runtime is treated as alive on the desired fast path, matching the
// legacy running/alive semantics only when both bits were probed — otherwise the
// tick is marked NOT-COMPARABLE by the caller).
func (rc shadowRuntimeCapture) runtimeFacts() runtimeFacts {
	if rc.runtimePresent == convergeTriUnknown {
		return runtimeFacts{observed: false}
	}
	present := rc.runtimePresent == convergeTriTrue
	// live requires both bits true; when alive is unknown we conservatively treat
	// a present runtime as not-live so no live-only action is emitted from an
	// under-probed capture (the comparator marks such ticks NOT-COMPARABLE).
	live := present && rc.processAlive == convergeTriTrue
	return runtimeFacts{
		observed:  true,
		live:      live,
		primedEnv: rc.primedEnv,
	}
}

// fullyProbed reports whether both runtime bits were resolved. A capture that is
// present-only (alive unknown) cannot be compared for live-gated actions.
func (rc shadowRuntimeCapture) fullyProbed() bool {
	return rc.runtimePresent != convergeTriUnknown && rc.processAlive != convergeTriUnknown
}

// --- in-process legacy-action recorder (3b layer 1) ---------------------------

// legacyCompareWrite is one recorded legacy write of a compared metadata key,
// captured SYNCHRONOUSLY at the write site (no arming, no budget, no async
// queue, cannot be env-disabled independently of the harness itself).
type legacyCompareWrite struct {
	sessionID string
	key       string
	value     string
	writer    string
	seq       int64
}

// legacyWriteRecorder is the per-tick synchronous capture channel. It is a plain
// slice guarded by a mutex (the reconciler fans sessions out; the recorder must
// be safe under that). It records ONLY compared keys and only when the harness
// is enabled for the current tick.
type legacyWriteRecorder struct {
	mu      sync.Mutex
	seq     int64
	writes  []legacyCompareWrite
	dropped int64
}

// record appends a compared-key write. Non-compared keys are ignored so callers
// can pass a whole batch. A nil recorder is a no-op (the disabled path).
func (r *legacyWriteRecorder) record(sessionID, writer string, batch map[string]string) {
	if r == nil || len(batch) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range batch {
		if !convergeComparedKeySet[k] {
			continue
		}
		r.seq++
		r.writes = append(r.writes, legacyCompareWrite{
			sessionID: sessionID,
			key:       k,
			value:     v,
			writer:    writer,
			seq:       r.seq,
		})
	}
}

// forSession returns the recorded writes for one session, in write order.
func (r *legacyWriteRecorder) forSession(sessionID string) []legacyCompareWrite {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []legacyCompareWrite
	for _, w := range r.writes {
		if w.sessionID == sessionID {
			out = append(out, w)
		}
	}
	return out
}

// convergeGlobalRecorder is the process-global recorder the wired legacy write
// sites feed. It is attached (non-nil) only while a shadow tick is in flight and
// the harness is enabled; otherwise the write-site wrappers see nil and bail.
//
// The supervisor reconciles each city on its own goroutine, so multiple enabled
// ticks can overlap in wall-clock. This is a single-owner slot guarded by an
// ownership token (compare-and-swap): only the tick that installs the recorder
// owns it, and only that tick may clear it (newConvergeShadowTick attaches via
// CAS(nil,rec); convergeShadowTick.detach clears via CAS(rec,nil)). A concurrent
// tick that loses the install CAS is a no-owner — it records nothing of its own
// and skips its sessions at finish — so one city's tick can neither overwrite,
// prematurely clear, nor misattribute another city's compared-key writes.
var convergeGlobalRecorder atomic.Pointer[legacyWriteRecorder]

// recordLegacyCompareWrites is THE recording wrapper every legacy write site of a
// compared key calls. It is a no-op unless a shadow tick attached a recorder, so
// it adds zero behavior and near-zero cost on the disabled path (one atomic
// load). sessionID may be empty at the map-build sites that pre-date bead
// creation; such calls are dropped with a counted drop so the denominator stays
// honest.
func recordLegacyCompareWrites(sessionID, writer string, batch map[string]string) {
	rec := convergeGlobalRecorder.Load()
	if rec == nil {
		return
	}
	if strings.TrimSpace(sessionID) == "" {
		hasCompared := false
		for k := range batch {
			if convergeComparedKeySet[k] {
				hasCompared = true
				break
			}
		}
		if hasCompared {
			rec.mu.Lock()
			rec.dropped++
			rec.mu.Unlock()
		}
		return
	}
	rec.record(sessionID, writer, batch)
}

// --- owned-key state-diff oracle (3b layer 2) ---------------------------------

// snapshotComparedKeys reads the compared keys out of a raw metadata map into a
// dense snapshot (missing keys map to ""). It performs no I/O.
func snapshotComparedKeys(meta map[string]string) map[string]string {
	snap := make(map[string]string, len(convergeComparedKeys))
	for _, k := range convergeComparedKeys {
		snap[k] = meta[k]
	}
	return snap
}

// applyDerivedToOwnedKeys is the pure "apply(derivedActions, start)" over the
// owned keys — the oracle's prediction of the end state. It models ONLY the
// enabled/derivable writes; disabled families still contribute their predicted
// owned-key value so the fixpoint and end-state assertions hold on fixtures.
//
// It never invents timestamps: for priming stamps it copies through the caller-
// supplied predicted values (a real executor reuses facts, never recomputes), so
// on real cities — where priming is excluded — only the canonical keys move.
func applyDerivedToOwnedKeys(start map[string]string, actions []sessConvergeAction, pred convergePredictedValues) map[string]string {
	end := make(map[string]string, len(start))
	for k, v := range start {
		end[k] = v
	}
	for _, a := range actions {
		switch a {
		case actionStampCanonicalIdentity:
			end[sessionpkg.CanonicalInstanceNameMetadata] = pred.canonicalInstanceName
			// Stamp the predicted pool slot for a pooled heal, and CLEAR any stale
			// slot for a singleton heal (empty predicted slot). Without the clear, a
			// start state carrying a stray canonical_pool_slot plus the newly-stamped
			// singleton name would read back through CanonicalIdentityFromMetadata as
			// an authoritative pooled identity.
			if pred.canonicalPoolSlot != "" {
				end[sessionpkg.CanonicalPoolSlotMetadata] = pred.canonicalPoolSlot
			} else {
				end[sessionpkg.CanonicalPoolSlotMetadata] = ""
			}
		case actionStampPrimedFromRuntime:
			end[sessionpkg.PrimedAtMetadataKey] = pred.primedAt
			end[sessionpkg.PromptHashMetadataKey] = pred.promptHash
		case actionAttemptPrime:
			end[sessionpkg.PrimingAttemptedAtMetadataKey] = pred.primingAttemptedAt
			end[sessionpkg.PromptHashMetadataKey] = pred.promptHash
		case actionRollbackRuntimeToAbsent:
			// Runtime teardown writes no compared metadata key (it kills a pane).
		}
	}
	return end
}

// convergePredictedValues carries the exact values a real executor would write,
// so applyDerivedToOwnedKeys reuses them verbatim (byte-identical double-apply).
type convergePredictedValues struct {
	canonicalInstanceName string
	canonicalPoolSlot     string
	primedAt              string
	primingAttemptedAt    string
	promptHash            string
}

// ownedKeyDivergence is a typed owned-key delta the oracle could not reconcile.
type ownedKeyDivergence struct {
	sessionID string
	key       string
	class     convergeDivergenceClass
	predicted string
	actual    string
}

// evaluateStateDiffOracle attributes every owned-key delta between the tick-start
// and tick-end snapshots to a typed class. It has two modes:
//
//   - Flip / fixture mode (shadowNoExecution == false): the derived actions are
//     assumed EXECUTED, so it asserts end == apply(derivedActions, start) over
//     the owned keys. A predicted-but-unrealized write is unrealized_prediction;
//     a realized-but-unpredicted write is unpredicted_delta (recorder-explained)
//     or foreign_write (unexplained); a value disagreement is value_mismatch.
//
//   - Shadow / no-execution mode (shadowNoExecution == true, real cities in
//     Stage 3): the derived heal is NOT executed, so predicted-but-unrealized
//     writes are EXPECTED (the derived-only heal lands at flip, not now) and are
//     not flagged. Realized legacy writes that the per-tick derivation
//     legitimately does not predict (create/adopt stamps) are likewise not
//     flagged. Only two things are divergences in shadow: a foreign write (a
//     realized owned-key delta with no recorder entry) and a C4 value-parity
//     breach (a derived stamp whose value disagrees with the value legacy
//     actually wrote for that key this tick).
func evaluateStateDiffOracle(
	sessionID string,
	ownedKeys []string,
	start, end map[string]string,
	actions []sessConvergeAction,
	pred convergePredictedValues,
	recorded []legacyCompareWrite,
	shadowNoExecution bool,
) []ownedKeyDivergence {
	predEnd := applyDerivedToOwnedKeys(start, actions, pred)
	recordedValue := map[string]string{}
	recordedKeys := map[string]bool{}
	for _, w := range recorded {
		recordedKeys[w.key] = true
		recordedValue[w.key] = w.value
	}
	var out []ownedKeyDivergence
	for _, k := range ownedKeys {
		predictedChange := predEnd[k] != start[k]
		actualChange := end[k] != start[k]

		if shadowNoExecution {
			// Real-city shadow: the derived heal is NOT executed, so predicted-but-
			// unrealized writes are EXPECTED and not flagged. A recorded legacy write
			// only "explains" an owned key when it actually MATERIALIZED (realized end
			// value == recorded value); otherwise it never landed (ApplyPatch failed)
			// or was overwritten, so the realized state is NOT recorder-explained and
			// must not be swallowed as clean — the false-negative this harness most
			// needs to avoid. C4 value parity additionally flags a derived stamp whose
			// value disagrees with the realized end (== recorded value here, so it
			// compares against both). Pulled forward from Stage 5.
			switch {
			case recordedKeys[k] && end[k] != recordedValue[k]:
				out = append(out, ownedKeyDivergence{sessionID, k, divergenceForeignWrite, recordedValue[k], end[k]})
			case recordedKeys[k] && predictedChange && predEnd[k] != end[k]:
				out = append(out, ownedKeyDivergence{sessionID, k, divergenceValueMismatch, predEnd[k], end[k]})
			case !recordedKeys[k] && actualChange:
				out = append(out, ownedKeyDivergence{sessionID, k, divergenceForeignWrite, "", end[k]})
			}
			continue
		}

		// Flip / fixture mode: end must equal apply(derived, start).
		want := predEnd[k]
		got := end[k]
		if want == got {
			continue
		}
		switch {
		case predictedChange && !actualChange:
			out = append(out, ownedKeyDivergence{sessionID, k, divergenceUnrealizedPrediction, want, got})
		case predictedChange && actualChange:
			out = append(out, ownedKeyDivergence{sessionID, k, divergenceValueMismatch, want, got})
		case !predictedChange && actualChange:
			if recordedKeys[k] {
				out = append(out, ownedKeyDivergence{sessionID, k, divergenceUnpredictedDelta, want, got})
			} else {
				out = append(out, ownedKeyDivergence{sessionID, k, divergenceForeignWrite, want, got})
			}
		}
	}
	return out
}

// --- replay comparator (3b layer 3) -------------------------------------------

// convergeSuppression models the tick-global legacy couplings that make a
// derived-present / legacy-absent pairing EXPECTED rather than divergent. The
// core derivation stays pure; these live only in the comparator.
type convergeSuppression struct {
	// rollbackBudgetExhausted: maxRollbacksPerTick reached, so a derived rollback
	// that legacy deferred this tick is expected.
	rollbackBudgetExhausted bool
	// storeQueryPartial: the store returned a partial view, so legacy skipped
	// close/rollback actions this tick.
	storeQueryPartial bool
	// deferSessionClosesOnBoot: boot-time close deferral is active.
	deferSessionClosesOnBoot bool
}

// suppresses reports whether the given derived action is expected to be absent
// from the legacy path under the active tick-global couplings.
func (s convergeSuppression) suppresses(a sessConvergeAction) bool {
	switch a {
	case actionRollbackRuntimeToAbsent:
		return s.rollbackBudgetExhausted || s.storeQueryPartial || s.deferSessionClosesOnBoot
	default:
		return false
	}
}

// replayInput is one derived-vs-legacy comparison for a single session-tick.
type replayInput struct {
	sessionID     string
	instanceToken string
	// durable/runtime are the facts the derivation used.
	durable durableFacts
	runtime runtimeFacts
	// legacyValues are the values the legacy path actually READ at decision time
	// (used for deterministic replay). Empty when the legacy path did not read.
	legacyValues  durableFacts
	legacyRuntime runtimeFacts
	// legacyReplayable is true when legacyValues/legacyRuntime were captured, so
	// a deterministic replay can run.
	legacyReplayable bool
	suppression      convergeSuppression
	// primingExcluded is true on real cities (the priming family is not compared).
	primingExcluded bool
	// factsProbeTarget and legacyProbeTarget are the resolved names; a mismatch is
	// identity-skew.
	factsProbeTarget  string
	legacyProbeTarget string
	// boundaryFlip is true when a threshold predicate flips sign within the
	// measured |tickNow - branchNow| window.
	boundaryFlip bool
}

// replayVerdict is the comparator's classification of one comparison.
type replayVerdict struct {
	// divergences are the surviving, un-suppressed divergence classes.
	divergences []convergeDivergenceClass
	// suppressed are classes recognized and suppressed (counted, not a failure).
	suppressed []convergeDivergenceClass
	// comparedActions is the derived action set that was actually compared (drives
	// the per-action-type compare quotas).
	comparedActions []sessConvergeAction
}

// isPrimingAction reports whether an action is part of the priming family.
func isPrimingAction(a sessConvergeAction) bool {
	return a == actionStampPrimedFromRuntime || a == actionAttemptPrime
}

// actionName returns a stable string name for counter keying.
func actionName(a sessConvergeAction) string {
	switch a {
	case actionRollbackRuntimeToAbsent:
		return "rollback_runtime_to_absent"
	case actionStampCanonicalIdentity:
		return "stamp_canonical_identity"
	case actionStampPrimedFromRuntime:
		return "stamp_primed_from_runtime"
	case actionAttemptPrime:
		return "attempt_prime"
	default:
		return "unknown"
	}
}

// compareReplay is the judge. It applies the identity-skew short-circuit, then —
// only when the legacy branch's read facts were captured (legacyReplayable) —
// derives the action set, filters the priming family on real cities, applies the
// boundary short-circuit, models tick-global suppression, and runs deterministic
// replay on the values legacy actually read: if the replay reproduces the derived
// action, the record is auto-classified world_moved and suppressed. The bar is
// "zero divergences that survive replay".
//
// This comparator judges ACTION-SET agreement, which is meaningful only against
// separately-captured legacy facts. Without them the comparison would degenerate
// to derived-vs-derived, so the parity pass is skipped entirely (no hollow
// compare counters) until the Stage-4/5 reader cutover supplies those facts. The
// owned-key state-diff oracle (evaluateStateDiffOracle) judges realized-value
// agreement and remains the live signal for this stage; the two are
// complementary and both feed the counters once replay is available.
func compareReplay(in replayInput) replayVerdict {
	var v replayVerdict

	// Identity-skew dominates: if the name used for fact capture differs from the
	// name the legacy branch probed, the comparison is not apples-to-apples.
	if strings.TrimSpace(in.factsProbeTarget) != "" &&
		strings.TrimSpace(in.legacyProbeTarget) != "" &&
		in.factsProbeTarget != in.legacyProbeTarget {
		v.suppressed = append(v.suppressed, divergenceIdentitySkew)
		return v
	}

	// Action-set parity requires the values the legacy branch actually READ, so a
	// deterministic replay can reconstruct the legacy action set and a genuine
	// derived-vs-legacy mismatch can surface. Without them (legacyReplayable ==
	// false) the only "legacy" set available is the derived set itself: every
	// derived action trivially agrees with itself, no unrealized-prediction /
	// unpredicted-delta divergence can arise, and emitting per-action compare
	// counters would imply a parity check that never ran. The production
	// reconciler cannot supply separate legacy facts until the Stage-4/5 reader
	// cutover, so this comparator stays inert there instead of reporting hollow
	// agreement; the owned-key state-diff oracle carries the realized-value signal
	// for this stage.
	if !in.legacyReplayable {
		return v
	}

	derived := deriveConvergeActions(in.durable, in.runtime)
	// The legacy action set is what deriveConvergeActions produces on the values
	// legacy actually read (deterministic replay ground truth).
	legacy := deriveConvergeActions(in.legacyValues, in.legacyRuntime)

	derivedSet := actionSet(derived)
	legacySet := actionSet(legacy)

	for _, a := range derived {
		if in.primingExcluded && isPrimingAction(a) {
			continue // excluded from real-city comparison (Q1)
		}
		v.comparedActions = append(v.comparedActions, a)
		if legacySet[a] {
			continue // agreement
		}
		// Derived-present, legacy-absent. Classify.
		if in.suppression.suppresses(a) {
			v.suppressed = append(v.suppressed, divergenceWorldMoved)
			continue
		}
		if in.boundaryFlip {
			v.suppressed = append(v.suppressed, divergenceBoundary)
			continue
		}
		// Deterministic replay already produced `legacy`; if it lacks this action
		// the world genuinely moved between derivation and legacy read.
		v.suppressed = append(v.suppressed, divergenceWorldMoved)
	}

	// Legacy-present, derived-absent: a derivation gap (the derivation would miss
	// an action legacy takes). Priming excluded on real cities.
	for _, a := range legacy {
		if in.primingExcluded && isPrimingAction(a) {
			continue
		}
		if derivedSet[a] {
			continue
		}
		if in.boundaryFlip {
			v.suppressed = append(v.suppressed, divergenceBoundary)
			continue
		}
		v.divergences = append(v.divergences, divergenceUnpredictedDelta)
	}

	return v
}

// actionSet builds a membership set over an action list.
func actionSet(actions []sessConvergeAction) map[sessConvergeAction]bool {
	m := make(map[sessConvergeAction]bool, len(actions))
	for _, a := range actions {
		m[a] = true
	}
	return m
}

// --- per-tick collector (3a assembly + 3b/3c evaluation) ----------------------

// shadowSessionEval bundles everything the harness captured for one session in
// one tick: the assembled facts (3a), the tick-start compared-key snapshot, the
// runtime capture with its probe provenance, and the replay context. The
// reconciler fills it incrementally (durable at loop entry, runtime at the probe
// site) and the collector evaluates it at tick end against the tick-end
// snapshot.
type shadowSessionEval struct {
	sessionID     string
	instanceToken string
	durable       durableFacts
	runtimeCap    shadowRuntimeCapture
	startSnap     map[string]string
	pred          convergePredictedValues
	factsTarget   string
	legacyTarget  string
	suppression   convergeSuppression
	// captured records whether durable facts were ever set (guards capture-loss).
	captured bool
}

// convergeShadowTick is the per-tick collector. It is created only when the
// harness is enabled; a nil *convergeShadowTick makes every method a no-op, so
// the reconciler wiring is byte-identical when the harness is off.
type convergeShadowTick struct {
	observerID string
	tickSeq    int64
	tickNow    time.Time
	// realCity is true for live-city ticks (priming family excluded); fixtures
	// set it false to compare the full owned set.
	realCity  bool
	recorder  *legacyWriteRecorder
	counters  *convergeShadowCounters
	evals     map[string]*shadowSessionEval
	orderedID []string
	// owned reports whether this tick won the ownership CAS for the process-global
	// recorder. A tick that did not (a concurrent city tick owns it this window)
	// records nothing and skips its sessions at finish.
	owned bool
	// detached guards detach() so it runs exactly once even though both finish and
	// the reconciler's safety-net defer call it.
	detached bool
}

// newConvergeShadowTick returns a live collector when the harness is enabled and
// nil otherwise. Callers guard every use with `if tick != nil`, so the disabled
// path costs one comparison.
func newConvergeShadowTick(observerID string, tickSeq int64, tickNow time.Time, realCity bool, counters *convergeShadowCounters) *convergeShadowTick {
	if !convergeShadowEnabled() {
		return nil
	}
	rec := &legacyWriteRecorder{}
	t := &convergeShadowTick{
		observerID: observerID,
		tickSeq:    tickSeq,
		tickNow:    tickNow,
		realCity:   realCity,
		recorder:   rec,
		counters:   counters,
		evals:      map[string]*shadowSessionEval{},
	}
	// Ownership token: install the recorder only if no concurrent city tick already
	// holds the slot. The loser stays a no-owner (owned=false) and its write sites
	// will observe the winner's recorder but under this tick's globally-unique
	// session ids, so they can never cross into the winner's own read-back.
	t.owned = convergeGlobalRecorder.CompareAndSwap(nil, rec)
	return t
}

// detach releases this tick's claim on the process-global recorder. It is
// idempotent (finish and the reconciler's safety-net defer both call it) and
// clears the slot only when this tick owns it, via CAS(rec,nil) — so a concurrent
// owner's live recorder is never torn out from under it. A no-owner tick has
// nothing to release.
func (t *convergeShadowTick) detach() {
	if t == nil || t.detached {
		return
	}
	t.detached = true
	if t.owned {
		convergeGlobalRecorder.CompareAndSwap(t.recorder, nil)
	}
}

// captureDurable records the durable facts + tick-start compared-key snapshot for
// a session at Phase-1 loop entry, from ALREADY-observed reconciler state (the
// coherent Info snapshot). No new probes, no writes.
func (t *convergeShadowTick) captureDurable(sessionID, instanceToken, factsTarget string, d durableFacts, startSnap map[string]string, pred convergePredictedValues) {
	if t == nil {
		return
	}
	e := t.evals[sessionID]
	if e == nil {
		e = &shadowSessionEval{sessionID: sessionID}
		t.evals[sessionID] = e
		t.orderedID = append(t.orderedID, sessionID)
	}
	e.instanceToken = instanceToken
	e.durable = d
	e.startSnap = startSnap
	e.pred = pred
	e.factsTarget = factsTarget
	e.captured = true
}

// captureRuntime records the two-bit runtime observation at the legacy branch's
// OWN probe site (never a re-probe), with its probe provenance.
func (t *convergeShadowTick) captureRuntime(sessionID, probeSite, probeTarget string, present, alive convergeTriState) {
	if t == nil {
		return
	}
	e := t.evals[sessionID]
	if e == nil {
		e = &shadowSessionEval{sessionID: sessionID}
		t.evals[sessionID] = e
		t.orderedID = append(t.orderedID, sessionID)
	}
	e.runtimeCap = shadowRuntimeCapture{
		probeSite:      probeSite,
		probeTarget:    probeTarget,
		runtimePresent: present,
		processAlive:   alive,
	}
	e.legacyTarget = probeTarget
}

// markSkip records a typed skip reason for a session-tick that cannot be
// compared, keeping the denominator honest. It drops the session from BOTH the
// eval map and the ordered set so finish never re-counts a skipped tick as
// capture-loss — a skipped session leaves the denominator exactly once.
func (t *convergeShadowTick) markSkip(sessionID string, r convergeSkipReason) { //nolint:unparam // typed skip-and-remove primitive over the full skip vocabulary; only skipEarlyContinue needs mid-loop removal today
	if t == nil {
		return
	}
	if t.counters != nil {
		t.counters.incSkipped(r)
	}
	if _, ok := t.evals[sessionID]; !ok {
		// Never captured (skipped before captureDurable): count once, nothing to drop.
		return
	}
	delete(t.evals, sessionID)
	kept := t.orderedID[:0]
	for _, id := range t.orderedID {
		if id != sessionID {
			kept = append(kept, id)
		}
	}
	t.orderedID = kept
}

// finish evaluates every captured session against its tick-end snapshot (read
// from the coherent post-Phase-1 Info snapshot by the caller, passed via
// endSnaps), runs the oracle + replay comparator, updates counters, and detaches
// the global recorder. It is safe to call on a nil tick.
func (t *convergeShadowTick) finish(endSnaps map[string]map[string]string) {
	if t == nil {
		return
	}
	defer t.detach()

	// A concurrent city tick owns the process-global recorder this window, so this
	// tick recorded none of its own legacy writes. Scoring against a recorder it
	// does not own would flag every owned-key delta as a phantom foreign_write, so
	// every captured session is a typed recorder_contended skip instead — the
	// denominator stays honest and no false divergence is manufactured.
	if !t.owned {
		for range t.orderedID {
			t.counters.incSkipped(skipRecorderContended)
		}
		return
	}

	ownedKeys := convergeCanonicalOwnedKeys
	if !t.realCity {
		ownedKeys = convergeFixtureOwnedKeys
	}

	for _, id := range t.orderedID {
		e := t.evals[id]
		if e == nil || !e.captured {
			t.counters.incSkipped(skipCaptureLoss)
			continue
		}
		end := endSnaps[id]
		if end == nil {
			t.counters.incSkipped(skipCaptureLoss)
			continue
		}
		t.evaluateCaptured(e, end, ownedKeys)
	}

	t.tallyDroppedRecords()
}

// evaluateCaptured runs the owned-key oracle, the fixpoint invariant (fixtures
// only), and the replay comparator for one fully captured session against its
// tick-end snapshot, updating the counters. A present-only runtime capture (alive
// unknown) is NOT-COMPARABLE for live-gated actions and leaves the denominator
// instead of being scored.
func (t *convergeShadowTick) evaluateCaptured(e *shadowSessionEval, end map[string]string, ownedKeys []string) {
	rf := e.runtimeCap.runtimeFacts()
	if e.runtimeCap.runtimePresent == convergeTriTrue && !e.runtimeCap.fullyProbed() {
		t.counters.incIncomparable()
		t.counters.incSkipped(skipNotComparable)
		return
	}

	t.counters.incEvaluated()

	actions := deriveConvergeActions(e.durable, rf)
	for _, a := range actions {
		t.counters.incDerived(actionName(a))
	}

	// Oracle: attribute owned-key deltas. On real cities the derived heal is not
	// executed, so shadowNoExecution (== realCity) suppresses the flip-stage
	// end==apply(derived,start) assertion and keeps only foreign-write + C4
	// value-parity (no unrealized-prediction flood).
	recorded := t.recorder.forSession(e.sessionID)
	for _, dv := range evaluateStateDiffOracle(e.sessionID, ownedKeys, e.startSnap, end, actions, e.pred, recorded, t.realCity) {
		t.counters.incDivergence(dv.class)
	}

	// Fixpoint (fixtures only): re-derive on END-of-tick facts must be empty. In
	// real-city shadow the derived heal is unexecuted, so a canonical re-derive
	// would be expected-non-empty; running it there would be a permanent false
	// positive.
	if !t.realCity {
		residual := fixpointResidual(e.durable, end, rf)
		for i := 0; i < residual; i++ {
			t.counters.incDivergence(divergenceFixpointNonEmpty)
		}
	}

	// Replay comparator. The Stage-3 harness captures a single fact set (the
	// coherent Info snapshot plus the legacy branch's own probe), so it cannot yet
	// supply the SEPARATE legacy-read facts action-set parity needs — that arrives
	// with the Stage-4/5 reader cutover. legacyReplayable is therefore false and
	// this runs the identity-skew precondition check only; the action-set parity
	// pass stays inert instead of comparing the derived action set against itself.
	// The owned-key state-diff oracle above is the realized-value signal here.
	verdict := compareReplay(replayInput{
		sessionID:         e.sessionID,
		instanceToken:     e.instanceToken,
		durable:           e.durable,
		runtime:           rf,
		legacyReplayable:  false,
		suppression:       e.suppression,
		primingExcluded:   t.realCity,
		factsProbeTarget:  e.factsTarget,
		legacyProbeTarget: e.legacyTarget,
	})
	for _, a := range verdict.comparedActions {
		t.counters.incCompare(actionName(a))
	}
	for _, class := range verdict.divergences {
		t.counters.incDivergence(class)
	}
	for _, class := range verdict.suppressed {
		t.counters.incDivergence(class)
	}
}

// fixpointResidual re-derives the converge actions on the END-of-tick durable
// facts. A non-empty result is a flip-stage invariant breach (a derivation gap or
// a mid-tick mutation); the residual action count is returned so each is counted.
func fixpointResidual(durable durableFacts, end map[string]string, rf runtimeFacts) int {
	endDurable := durable
	endDurable.canonicalIdentity = strings.TrimSpace(end[sessionpkg.CanonicalInstanceNameMetadata])
	endDurable.primedAt = end[sessionpkg.PrimedAtMetadataKey]
	if v := strings.TrimSpace(end[sessionpkg.PrimingAttemptedAtMetadataKey]); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			endDurable.primingAttemptedAt = ts.UTC()
		}
	}
	endDurable.primedPromptHash = end[sessionpkg.PromptHashMetadataKey]
	return len(deriveConvergeActions(endDurable, rf))
}

// tallyDroppedRecords folds the recorder's dropped-write count (compared-key
// writes seen before a bead ID existed) into the counters.
func (t *convergeShadowTick) tallyDroppedRecords() {
	if t.recorder == nil {
		return
	}
	for i := int64(0); i < t.recorder.dropped; i++ {
		t.counters.incRecordsDropped()
	}
}

// --- fact builders (3a) -------------------------------------------------------

// buildDurableFactsFromInfo assembles durableFacts from the reconciler's ALREADY
// coherent typed Info snapshot. Every compared key — canonical identity AND the
// priming markers — is a verbatim raw Info mirror (the typed tree projects all
// five compared keys onto Info via the codec table), so the harness reads the
// priming facts straight off Info instead of a raw side-channel: the reconciler
// loop carries no raw session beads. No probes, no writes. On real cities the
// priming inputs are still captured so the fixpoint stays honest, but the priming
// action FAMILY is excluded from real-city comparison downstream.
func buildDurableFactsFromInfo(info sessionpkg.Info, tickNow time.Time) durableFacts {
	d := durableFacts{
		primedAt:          info.PrimedAtMetadata,
		primedPromptHash:  info.PromptHashMetadata,
		canonicalIdentity: info.CanonicalInstanceNameMetadata,
		absent:            info.Closed,
		now:               tickNow,
	}
	if v := strings.TrimSpace(info.PrimingAttemptedAtMetadata); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			d.primingAttemptedAt = ts.UTC()
		}
	}
	return d
}

// comparedKeyMetadataFromInfo projects the five compared keys off a typed Info
// into a dense metadata map (canonical identity + priming markers, all verbatim
// raw Info mirrors). It is the typed-tree bridge for snapshotComparedKeys: the
// reconciler no longer carries raw session beads through the tick loop, so the
// shadow harness snapshots the compared keys from Info's mirrors — kept in
// lockstep with the store by the same codec that drives every other projected
// key — instead of a raw metadata map.
func comparedKeyMetadataFromInfo(info sessionpkg.Info) map[string]string {
	return map[string]string{
		sessionpkg.CanonicalInstanceNameMetadata: info.CanonicalInstanceNameMetadata,
		sessionpkg.CanonicalPoolSlotMetadata:     info.CanonicalPoolSlotMetadata,
		sessionpkg.PrimedAtMetadataKey:           info.PrimedAtMetadata,
		sessionpkg.PrimingAttemptedAtMetadataKey: info.PrimingAttemptedAtMetadata,
		sessionpkg.PromptHashMetadataKey:         info.PromptHashMetadata,
	}
}

// snapshotComparedKeysFromInfo is the Info-mirror counterpart of
// snapshotComparedKeys: it reads the compared keys off a typed Info into a dense
// snapshot (missing keys map to ""). The reconciler uses it for the tick-start
// and tick-end compared-key snapshots, since the typed loop carries Info, not raw
// beads.
func snapshotComparedKeysFromInfo(info sessionpkg.Info) map[string]string {
	return snapshotComparedKeys(comparedKeyMetadataFromInfo(info))
}
