package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The simulator's coverage census (architecture item C): how often the
// corpus reaches each registered arm of decideRow's table, each effect kind
// and each kind's settle outcomes, so a generator that stops reaching a
// path shows as a zero instead of a silently weaker corpus.

// simCoverage counts across every sim the test binary runs.
var simCoverage = struct {
	sync.Mutex
	arms     map[string]*armHits       // by armKey
	effects  map[string]int            // settled effects, by kind, as posted
	outcomes map[string]map[string]int // by kind, then outcome (and cause)
	reasons  map[string]int            // admitted intents, by a gated reason (simCoverageReasons)
}{arms: map[string]*armHits{}, effects: map[string]int{}, outcomes: map[string]map[string]int{}, reasons: map[string]int{}}

// simCoverageReasons are decide reasons the census counts and the gate
// requires, paths no arm or kind tells apart: A11's floor-ack cancel
// (DRAIN-010, PAR-FLOORACK), one signal-ack among many.
var simCoverageReasons = []string{decideFloorAckCancel}

// countReason counts an intent a planning pass admitted, by its reason,
// when the census gates that reason.
func countReason(it intent) {
	if !slices.Contains(simCoverageReasons, it.Reason) {
		return
	}
	simCoverage.Lock()
	defer simCoverage.Unlock()
	simCoverage.reasons[it.Reason]++
}

// armHits are an arm's decisions: an intent it proposed, or a hold.
type armHits struct{ Acts, Holds int }

// armKey names an arm of the table: its number and its function.
func armKey(a rowArm) string {
	name := runtime.FuncForPC(reflect.ValueOf(a.decide).Pointer()).Name()
	return a.name + "/" + name[strings.LastIndex(name, ".")+1:]
}

// registeredArms are the keys of decideRow's table (production's).
func registeredArms() []string {
	var out []string
	for _, a := range rowArms {
		out = append(out, armKey(a))
	}
	return out
}

// registeredEffects are the effect kinds whose effect runs.
func registeredEffects() []string {
	var out []string
	for kind, spec := range effectSpecs {
		if spec.runs() {
			out = append(out, kind)
		}
	}
	sort.Strings(out)
	return out
}

// decisionCredit is which arm decisions the census credits: a planning
// pass's, the first arm that decides each row. A6's fold probing the later
// arms on the same row, and an effect re-deciding a fresh row (decideRow
// outside the pass), are evaluations, not decisions.
type decisionCredit struct {
	mu       sync.Mutex
	credited map[*rowFacts]bool // the pass's rows decided so far; nil outside a pass
}

// planning runs pass as a planning pass whose decisions are credited.
func (c *decisionCredit) planning(pass func()) {
	c.mu.Lock()
	c.credited = map[*rowFacts]bool{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.credited = nil
		c.mu.Unlock()
	}()
	pass()
}

// take reports whether r's decision is credited: inside a planning pass,
// and r's first.
func (c *decisionCredit) take(r *rowFacts) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credited == nil || c.credited[r] {
		return false
	}
	c.credited[r] = true
	return true
}

// countArms wraps the table's arms so each credited decision is counted
// under the arm's key (a mutant's arm counts under its own name).
func countArms(arms []rowArm, credit *decisionCredit) []rowArm {
	out := make([]rowArm, len(arms))
	for i, a := range arms {
		key, decide := armKey(a), a.decide
		out[i] = rowArm{name: a.name, decide: func(r *rowFacts) (intent, bool) {
			it, ok := decide(r)
			if ok && credit.take(r) {
				simCoverage.Lock()
				h := simCoverage.arms[key]
				if h == nil {
					h = &armHits{}
					simCoverage.arms[key] = h
				}
				if it.Kind != "" {
					h.Acts++
				} else {
					h.Holds++
				}
				simCoverage.Unlock()
			}
			return it, ok
		}}
	}
	return out
}

// countPost counts an effect's settlement as the executor posts it to the
// planner: its kind and its outcome (and cause). A create abandoned at its
// deadline is ambiguous there, whatever it returns later; that late return
// posts its facts alone (Late) and is not counted again.
func countPost(st settlement) {
	if st.Late {
		return
	}
	simCoverage.Lock()
	defer simCoverage.Unlock()
	simCoverage.effects[st.Kind]++
	m := simCoverage.outcomes[st.Kind]
	if m == nil {
		m = map[string]int{}
		simCoverage.outcomes[st.Kind] = m
	}
	m[outcomeName(st.Outcome)]++
	if st.Cause != "" {
		m[outcomeName(st.Outcome)+"/"+st.Cause]++
	}
}

func outcomeName(o settleOutcome) string {
	switch o {
	case settledLanded:
		return "landed"
	case settledFailed:
		return "failed"
	case settledRefused:
		return "refused"
	case settledAmbiguous:
		return "ambiguous"
	case settledNoop:
		return "noop"
	}
	return fmt.Sprintf("outcome-%d", o)
}

// simCoverageReport is the census as written to GC_V2_SIM_COVERAGE.
type simCoverageReport struct {
	Arms     map[string]armHits        `json:"arms"`
	Effects  map[string]int            `json:"effects"`
	Outcomes map[string]map[string]int `json:"outcomes"`
	Reasons  map[string]int            `json:"reasons"`
}

// writeSimCoverage writes the census to the path GC_V2_SIM_COVERAGE names,
// when set, at the end of a corpus test.
func writeSimCoverage(t *testing.T) {
	path := os.Getenv("GC_V2_SIM_COVERAGE")
	if path == "" {
		return
	}
	simCoverage.Lock()
	r := simCoverageReport{Arms: map[string]armHits{}, Effects: simCoverage.effects, Outcomes: simCoverage.outcomes, Reasons: simCoverage.reasons}
	for k, h := range simCoverage.arms {
		r.Arms[k] = *h
	}
	b, err := json.MarshalIndent(r, "", " ")
	simCoverage.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// settleOutcomes are the outcome classes the corpus must reach, across
// kinds: every settleOutcome (one outcomeName does not name is gated as
// outcome-N, which no effect reaches until it is named).
func settleOutcomes() []string {
	var out []string
	for o := settledLanded; o < settleOutcomeEnd; o++ {
		out = append(out, outcomeName(o))
	}
	return out
}

// simCoverageAllowlist are the census keys the corpus is let not reach, each
// for want of a generator (mc-i7efm). The gate fails on a listed key that
// gets hits, so each entry goes when its generator lands.
var simCoverageAllowlist = map[string]string{
	// No generator asks a row to restart (generators frozen for the
	// release; the scripted A13 scenarios cover it).
	"arm:A13/armRestartRequested": "mc-i7efm: restart requested",
	"effect:" + intentMarkerClear: "mc-i7efm: restart requested",
	// No generator wakes an awake row (generators frozen for the release;
	// TestSatisfiedWakeClear and TestSimSatisfiedWakeDoesNotRestartAfterADeath
	// cover it).
	"arm:A6/armSatisfiedWake": "mc-i7efm: wake requests",
	// No agent sleeps after idle (sleep_after_idle, mc-ivrct).
	"arm:A15/armIdleTimeout": "mc-i7efm: idle timeout",
	// No execution stalls past the backstop.
	"arm:A16/armExecutionStalled": "mc-i7efm: execution stalled",
	// No heartbeat goes stale.
	"arm:A17/armHeartbeatRespawn": "mc-i7efm: heartbeat respawn",
	// No row is detached, stranded or carries a stale current bead.
	"arm:A6/armDetachedAt":    "mc-i7efm: A6 subarms",
	"arm:A6/armStrandedClear": "mc-i7efm: A6 subarms",
	"arm:A6/armCurrentBead":   "mc-i7efm: A6 subarms",
	// No row's baseline is behind.
	"arm:A8/armBaseline":       "mc-i7efm: baseline",
	"effect:" + intentBaseline: "mc-i7efm: baseline",
	// Every runtime the sim makes has a row first.
	"arm:A1/armNoRow": "mc-i7efm: no-row runtime",
}

// simCoverageGaps are the registered keys the reports never reach and are
// not allowlisted, then the allowlisted keys they do reach (or that are no
// longer registered).
func simCoverageGaps(reports []simCoverageReport, arms, effects, reasons []string, allow map[string]string) (zero, stale []string) {
	hits := map[string]int{}
	for _, r := range reports {
		for k, h := range r.Arms {
			hits["arm:"+k] += h.Acts + h.Holds
		}
		for k, n := range r.Effects {
			hits["effect:"+k] += n
		}
		for _, m := range r.Outcomes {
			for o, n := range m {
				hits["outcome:"+o] += n
			}
		}
		for k, n := range r.Reasons {
			hits["reason:"+k] += n
		}
	}
	registered := map[string]bool{}
	for _, keys := range [][]string{prefixed("arm:", arms), prefixed("effect:", effects), prefixed("outcome:", settleOutcomes()), prefixed("reason:", reasons)} {
		for _, k := range keys {
			registered[k] = true
			if _, ok := allow[k]; hits[k] == 0 && !ok {
				zero = append(zero, k)
			}
		}
	}
	for k := range allow {
		if hits[k] != 0 || !registered[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(zero)
	sort.Strings(stale)
	return zero, stale
}

func prefixed(p string, keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = p + k
	}
	return out
}

// TestSimCoverageGate fails on a registered arm, effect kind or outcome
// class the corpora never reach, over the censuses GC_V2_SIM_COVERAGE_FILES
// names (one per corpus range, space-separated; relative paths are runfiles,
// BUILD.bazel's gc_sim_coverage_gate_test), which must be the
// GC_V2_SIM_COVERAGE_RANGES it expects; both unset, it skips.
func TestSimCoverageGate(t *testing.T) {
	paths, ranges := strings.Fields(os.Getenv("GC_V2_SIM_COVERAGE_FILES")), os.Getenv("GC_V2_SIM_COVERAGE_RANGES")
	switch want, err := strconv.Atoi(ranges); {
	case len(paths) == 0 && ranges == "":
		t.Skip("GC_V2_SIM_COVERAGE_FILES unset")
	case err != nil || len(paths) != want:
		t.Fatalf("%d censuses in GC_V2_SIM_COVERAGE_FILES, want GC_V2_SIM_COVERAGE_RANGES=%q: a range's census is missing", len(paths), ranges)
	}
	var reports []simCoverageReport
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"), p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			t.Fatalf("%s: empty: its range wrote no census (did the range fail?)", p)
		}
		var r simCoverageReport
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		reports = append(reports, r)
	}
	zero, stale := simCoverageGaps(reports, registeredArms(), registeredEffects(), simCoverageReasons, simCoverageAllowlist)
	for _, k := range zero {
		t.Errorf("%s: no hit over %d censuses (a generator is missing, or list it in simCoverageAllowlist with its bead)", k, len(reports))
	}
	for _, k := range stale {
		t.Errorf("%s: allowlisted (%s) but hit or no longer registered: delete its entry", k, simCoverageAllowlist[k])
	}
}

func TestSimCoverageGaps(t *testing.T) {
	all := simCoverageReport{
		Arms:     map[string]armHits{"A1/a": {Holds: 1}, "A2/b": {Acts: 1}},
		Effects:  map[string]int{"x": 1},
		Outcomes: map[string]map[string]int{"x": {"landed": 1, "failed": 1, "refused": 1, "ambiguous": 1, "noop/why": 1, "noop": 1}},
		Reasons:  map[string]int{"r": 1},
	}
	noAmbiguous := map[string]map[string]int{"x": {"landed": 1, "failed": 1, "refused": 1, "noop": 1, "ambiguous/why": 1}}
	for _, c := range []struct {
		name        string
		reports     []simCoverageReport
		allow       map[string]string
		zero, stale []string
	}{
		{name: "every key hit", reports: []simCoverageReport{all}},
		{name: "hits sum over censuses", reports: []simCoverageReport{
			{Arms: map[string]armHits{"A1/a": {Holds: 1}}, Effects: map[string]int{"x": 1}, Outcomes: all.Outcomes},
			{Arms: map[string]armHits{"A2/b": {Acts: 1}, "A2/b-mutant": {Acts: 1}}, Reasons: map[string]int{"r": 1}},
		}},
		{name: "no census", zero: []string{"arm:A1/a", "arm:A2/b", "effect:x", "outcome:ambiguous", "outcome:failed", "outcome:landed", "outcome:noop", "outcome:refused", "reason:r"}},
		{name: "a zero entry is a hit of none", reports: []simCoverageReport{
			{Arms: map[string]armHits{"A1/a": {}, "A2/b": {Acts: 1}}, Effects: map[string]int{"x": 0}, Outcomes: noAmbiguous, Reasons: map[string]int{"r": 0}},
		}, zero: []string{"arm:A1/a", "effect:x", "outcome:ambiguous", "reason:r"}},
		{name: "allowlisted zeros pass", reports: []simCoverageReport{
			{Arms: map[string]armHits{"A2/b": {Acts: 1}}, Outcomes: noAmbiguous},
		}, allow: map[string]string{"arm:A1/a": "bead", "effect:x": "bead", "outcome:ambiguous": "bead", "reason:r": "bead"}},
		{
			name: "an allowlisted key hit is stale", reports: []simCoverageReport{all},
			allow: map[string]string{"arm:A1/a": "bead", "outcome:noop": "bead"}, stale: []string{"arm:A1/a", "outcome:noop"},
		},
		{
			name: "an allowlisted key not registered is stale", reports: []simCoverageReport{all},
			allow: map[string]string{"arm:A9/gone": "bead", "effect:gone": "bead"}, stale: []string{"arm:A9/gone", "effect:gone"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			zero, stale := simCoverageGaps(c.reports, []string{"A1/a", "A2/b"}, []string{"x"}, []string{"r"}, c.allow)
			if !slices.Equal(zero, c.zero) || !slices.Equal(stale, c.stale) {
				t.Errorf("gaps = %q, %q; want %q, %q", zero, stale, c.zero, c.stale)
			}
		})
	}
}

// The census credits a planning pass's first decision per row only: not a
// fold's probe of a later arm on the same row, nor a decision outside the
// pass (an effect re-deciding its fresh row).
func TestSimCoverageCreditsPassDecisions(t *testing.T) {
	var c decisionCredit
	r1, r2 := &rowFacts{}, &rowFacts{}
	if c.take(r1) {
		t.Error("a decision outside a planning pass was credited")
	}
	var got []bool
	c.planning(func() { got = []bool{c.take(r1), c.take(r1), c.take(r2)} })
	if want := []bool{true, false, true}; !slices.Equal(got, want) {
		t.Errorf("credits in the pass = %v, want %v (the row's second decision is a probe)", got, want)
	}
	if c.take(r2) {
		t.Error("a decision after the pass was credited")
	}
}
