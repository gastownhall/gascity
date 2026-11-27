package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCanonicalizeLegacyRoutedWorkLeavesForeignBindingOnSharedRigLeg is AC-1 of
// the #5846 design: city A's only omp agent is unbound "qcore/omp" and it mints
// no "pool" binding on rig qcore, so an open unassigned bead on the shared
// "rig:qcore" leg routed to "qcore/pool.omp" is another city's canonical work.
// The re-home must not issue an Update, must leave gc.routed_to alone, and must
// report the refusal instead of silently rewriting somebody else's row.
func TestCanonicalizeLegacyRoutedWorkLeavesForeignBindingOnSharedRigLeg(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("omp", "qcore", intPtr(3), 0)}}
	const foreignCanonical = "qcore/pool.omp"

	wb := workBead("wb-fx", foreignCanonical, "", "open", 2)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
	store := &updateCountingStore{Store: mem}

	report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:qcore"}, io.Discard)

	if store.updates != 0 {
		t.Errorf("refused rewrite still issued %d Update call(s), want 0", store.updates)
	}
	got, err := mem.Get("wb-fx")
	if err != nil {
		t.Fatalf("Get(wb-fx): %v", err)
	}
	if routed := got.Metadata["gc.routed_to"]; routed != foreignCanonical {
		t.Errorf("gc.routed_to = %q, want %q (foreign canonical route left alone)", routed, foreignCanonical)
	}
	if len(report.Rehomed) != 0 {
		t.Errorf("report rehomed %+v, want none", report.Rehomed)
	}
	if len(report.Refused) != 1 {
		t.Fatalf("report refused %+v, want exactly the foreign strip", report.Refused)
	}
	want := legacyRehomeEntry{ID: "wb-fx", Leg: "rig:qcore", Field: "gc.routed_to", From: foreignCanonical, To: "qcore/omp"}
	if report.Refused[0] != want {
		t.Errorf("refusal = %+v, want %+v", report.Refused[0], want)
	}
}

// TestLegacyRehomeLegIsOwn pins the leg classification: the city's own legs are
// the empty ref (the assigned arm's city vocabulary), a scoped city ref, and a
// relocated-class ref; a rig leg and any unrecognized spelling are shared, so
// unknown refs fail closed.
func TestLegacyRehomeLegIsOwn(t *testing.T) {
	cases := map[string]bool{
		"":                 true,  // assigned arm's city vocabulary
		"city:gascity":     true,  // routed arm's city vocabulary
		"class:ga-classes": true,  // relocated class binding, city scope
		"rig:qcore":        false, // a rig other cities attach too
		"qcore":            false, // assigned arm's bare rig vocabulary
		"weird:x":          false, // unrecognized spelling fails closed
	}
	for ref, want := range cases {
		if got := legacyRehomeLegIsOwn(ref); got != want {
			t.Errorf("legacyRehomeLegIsOwn(%q) = %v, want %v", ref, got, want)
		}
	}
}

// TestLegacyRehomeAllowedSharedLeg pins the guard predicate itself: own legs
// keep today's behavior for both directions, and a shared leg refuses the strip
// of any binding this city does not mint on the leg's rig.
func TestLegacyRehomeAllowedSharedLeg(t *testing.T) {
	unbound := poolAgent("omp", "qcore", intPtr(3), 0)
	minted := &config.City{
		Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
		Agents:  []config.Agent{unbound},
	}
	notMinted := &config.City{Agents: []config.Agent{unbound}}

	cases := []struct {
		name  string
		cfg   *config.City
		own   bool
		from  string
		agent *config.Agent
		want  bool
	}{
		{"own leg strips as today", minted, true, "qcore/pool.omp", &unbound, true},
		{"shared strip this city mints", minted, false, "qcore/pool.omp", &unbound, true},
		{"shared strip of a foreign binding", notMinted, false, "qcore/pool.omp", &unbound, false},
		{"shared strip with an empty binding", notMinted, false, "qcore/.omp", &unbound, false},
		{"shared rewrite of a non-bound form", notMinted, false, "qcore/omp-1", &unbound, false},
		{"unresolvable agent refuses", notMinted, false, "qcore/pool.omp", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyRehomeAllowed(tc.cfg, tc.own, tc.from, tc.agent); got != tc.want {
				t.Errorf("legacyRehomeAllowed(own=%v, from=%q) = %v, want %v", tc.own, tc.from, got, tc.want)
			}
		})
	}
}

// TestCanonicalizeLegacyRoutedWorkRefusesBindingThisCityDoesNotMint covers the
// mint-source edges of cityMintsBindingOnRig from the re-home side:
// [defaults.rig.imports] alone binds nothing (gc copies it into each rig's own
// import table when the rig is added), and a binding imported only on another
// rig says nothing about this one.
func TestCanonicalizeLegacyRoutedWorkRefusesBindingThisCityDoesNotMint(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.City
	}{
		{
			name: "default rig imports alone",
			cfg: &config.City{
				DefaultRigImports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
				Agents:            []config.Agent{poolAgent("omp", "qcore", intPtr(3), 0)},
			},
		},
		{
			name: "binding imported only on another rig",
			cfg: &config.City{
				Rigs: []config.Rig{{
					Name:    "other",
					Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
				}},
				Agents: []config.Agent{poolAgent("omp", "qcore", intPtr(3), 0)},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wb := workBead("wb-fx", "qcore/pool.omp", "", "open", 2)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}

			report := canonicalizeLegacyBoundUnassignedRoutedWork(tc.cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:qcore"}, io.Discard)

			if store.updates != 0 {
				t.Errorf("%s: rewrote the route anyway (%d update(s))", tc.name, store.updates)
			}
			if len(report.Refused) != 1 {
				t.Errorf("%s: refused %+v, want the foreign strip", tc.name, report.Refused)
			}
		})
	}
}

// TestCanonicalizeLegacyRoutedRehomeMismatchedRefsWritesNothing pins the refs
// contract for the routed arm: workRefs must be index-aligned with
// workBeads/workStores, and a length mismatch writes nothing, like the
// beads/stores mismatch rule.
func TestCanonicalizeLegacyRoutedRehomeMismatchedRefsWritesNothing(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0)}}
	const legacy = "rig-A/gc.planner"

	wb := workBead("wb-route", legacy, "", "open", 5)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
	store := &updateCountingStore{Store: mem}
	canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, nil, io.Discard)
	if store.updates != 0 {
		t.Errorf("routed arm wrote %d time(s) with mismatched refs", store.updates)
	}
}

// TestLegacyRehomeReportLog pins the audit trail: one line per rewrite naming
// the bead, the leg, and the quoted identity pair, plus at most one refusal
// summary per pass — grouped by from->to pair, exact counts, IDs sampled at
// five — and nothing when nothing was refused. Identities are %q-quoted: they
// are untrusted text read off rows another city wrote.
func TestLegacyRehomeReportLog(t *testing.T) {
	t.Run("empty report logs nothing", func(t *testing.T) {
		var buf bytes.Buffer
		(legacyRehomeReport{}).log(&buf, "canonicalizeLegacyBoundUnassignedRoutedWork")
		if buf.Len() != 0 {
			t.Errorf("empty report logged %q, want nothing", buf.String())
		}
	})

	t.Run("one line per rewrite, one summary per pass", func(t *testing.T) {
		var buf bytes.Buffer
		var rep legacyRehomeReport
		rep.rehomed(legacyRehomeEntry{ID: "ga-1", Leg: "rig:qcore", Field: "assignee", From: "qcore/gc.omp", To: "qcore/omp"})
		rep.refuse(legacyRehomeEntry{ID: "ga-2", Leg: "rig:qcore", Field: "gc.routed_to", From: "qcore/pool.omp", To: "qcore/omp"})
		rep.refuse(legacyRehomeEntry{ID: "ga-3", Leg: "rig:qcore", Field: "gc.routed_to", From: "qcore/pool.omp", To: "qcore/omp"})
		rep.log(&buf, "canonicalizeLegacyBoundAssignedWork")

		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		wantLines := []string{
			`canonicalizeLegacyBoundAssignedWork: rehomed ga-1 on rig:qcore: assignee "qcore/gc.omp" -> "qcore/omp"`,
			`canonicalizeLegacyBoundAssignedWork: refused 2 foreign legacy re-home(s) this pass: "qcore/pool.omp" -> "qcore/omp" (2: ga-2, ga-3)`,
		}
		if !slices.Equal(lines, wantLines) {
			t.Errorf("log lines\n got  %q\n want %q", lines, wantLines)
		}
	})

	t.Run("refusals are grouped, sorted and sampled", func(t *testing.T) {
		var buf bytes.Buffer
		var rep legacyRehomeReport
		// Deliberately out of pair order; the second pair's seven IDs sample at
		// five with "+2 more".
		rep.refuse(legacyRehomeEntry{ID: "ga-b1", Leg: "rig:qcore", Field: "gc.routed_to", From: "qcore/zz.omp", To: "qcore/omp"})
		for i := 0; i < 7; i++ {
			rep.refuse(legacyRehomeEntry{ID: fmt.Sprintf("ga-a%d", i+1), Leg: "rig:qcore", Field: "gc.routed_to", From: "qcore/pool.omp", To: "qcore/omp"})
		}
		rep.log(&buf, "canonicalizeLegacyBoundUnassignedRoutedWork")

		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		if len(lines) != 1 {
			t.Fatalf("got %d lines, want one summary: %q", len(lines), lines)
		}
		want := `canonicalizeLegacyBoundUnassignedRoutedWork: refused 8 foreign legacy re-home(s) this pass: ` +
			`"qcore/pool.omp" -> "qcore/omp" (7: ga-a1, ga-a2, ga-a3, ga-a4, ga-a5 +2 more), ` +
			`"qcore/zz.omp" -> "qcore/omp" (1: ga-b1)`
		if lines[0] != want {
			t.Errorf("summary\n got  %s\n want %s", lines[0], want)
		}
	})

	t.Run("commas and newlines in identities are escaped", func(t *testing.T) {
		var buf bytes.Buffer
		var rep legacyRehomeReport
		rep.refuse(legacyRehomeEntry{ID: "ga-x", Leg: "weird:x", Field: "gc.routed_to", From: "evil,id\nforge", To: "qcore/omp"})
		rep.log(&buf, "canonicalizeLegacyBoundUnassignedRoutedWork")
		if !strings.Contains(buf.String(), `"evil,id\nforge"`) {
			t.Errorf("identity not %q-quoted: %q", buf.String(), buf.String())
		}
	})
}

// TestCanonicalizeLegacyRoutedWorkRefusesAddDirectionOnSharedRigLeg is AC-2 of
// the #5846 design: on a shared rig leg the reverse "add" rewrite — a bare
// legacy identity resolving onto a bound agent — is refused too. A bare
// identity carries no mark of which city minted the binding the rewrite would
// gain, and the rewrite would mint that binding on the row, so no question
// asked of the local config can prove it is this city's legacy spelling.
func TestCanonicalizeLegacyRoutedWorkRefusesAddDirectionOnSharedRigLeg(t *testing.T) {
	bound := poolAgent("omp", "qcore", intPtr(3), 0)
	bound.BindingName = "pool"
	cfg := &config.City{
		Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
		Agents:  []config.Agent{bound},
	}
	if got := bound.QualifiedName(); got != "qcore/pool.omp" {
		t.Fatalf("bound canonical identity = %q, want qcore/pool.omp", got)
	}

	wb := workBead("wb-add", "qcore/omp", "", "open", 2)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
	store := &updateCountingStore{Store: mem}

	report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:qcore"}, io.Discard)

	if store.updates != 0 {
		t.Errorf("add-direction rewrite issued %d Update call(s) on a shared leg, want 0", store.updates)
	}
	got, err := mem.Get("wb-add")
	if err != nil {
		t.Fatalf("Get(wb-add): %v", err)
	}
	if routed := got.Metadata["gc.routed_to"]; routed != "qcore/omp" {
		t.Errorf("gc.routed_to = %q, want %q (bare legacy identity left alone)", routed, "qcore/omp")
	}
	if len(report.Rehomed) != 0 {
		t.Errorf("report rehomed %+v, want none", report.Rehomed)
	}
	if len(report.Refused) != 1 {
		t.Fatalf("report refused %+v, want exactly the add-direction refusal", report.Refused)
	}
	want := legacyRehomeEntry{ID: "wb-add", Leg: "rig:qcore", Field: "gc.routed_to", From: "qcore/omp", To: "qcore/pool.omp"}
	if report.Refused[0] != want {
		t.Errorf("refusal = %+v, want %+v", report.Refused[0], want)
	}
}

// TestCanonicalizeLegacyAssignedWorkLeavesForeignInFlightOnSharedRigLeg is
// AC-3 of the #5846 design: the assigned arm gets the same leg-scoped guard as
// the routed arm. In-flight foreign work — another city's canonical in_progress
// row on a shared rig store — resolves through the permissive fallbacks to this
// city's unbound agent, and re-homing it would pull live work out from under
// the city that owns it. Each changed field is guarded; a refused field skips
// the whole row (never half-migrate it), and the refusal is reported.
func TestCanonicalizeLegacyAssignedWorkLeavesForeignInFlightOnSharedRigLeg(t *testing.T) {
	t.Run("both fields refused", func(t *testing.T) {
		cfg := &config.City{Agents: []config.Agent{poolAgent("omp", "fixture", intPtr(3), 0)}}

		wb := workBead("fx-asg", "fixture/pool.omp", "fixture/pool.omp", "in_progress", 5)
		mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
		store := &updateCountingStore{Store: mem}

		report := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"fixture"}, newSessionBeadSnapshot(nil), io.Discard)

		if store.updates != 0 {
			t.Errorf("refused rewrite still issued %d Update call(s), want 0", store.updates)
		}
		got, err := mem.Get("fx-asg")
		if err != nil {
			t.Fatalf("Get(fx-asg): %v", err)
		}
		if got.Assignee != "fixture/pool.omp" {
			t.Errorf("assignee = %q, want the foreign canonical identity left alone", got.Assignee)
		}
		if routed := got.Metadata["gc.routed_to"]; routed != "fixture/pool.omp" {
			t.Errorf("gc.routed_to = %q, want the foreign canonical route left alone", routed)
		}
		if len(report.Rehomed) != 0 {
			t.Errorf("report rehomed %+v, want none", report.Rehomed)
		}
		if len(report.Refused) != 1 {
			t.Fatalf("report refused %+v, want exactly the assignee refusal (the row is skipped whole)", report.Refused)
		}
		want := legacyRehomeEntry{ID: "fx-asg", Leg: "fixture", Field: "assignee", From: "fixture/pool.omp", To: "fixture/omp"}
		if report.Refused[0] != want {
			t.Errorf("refusal = %+v, want %+v", report.Refused[0], want)
		}
	})

	t.Run("mixed row is never half-migrated", func(t *testing.T) {
		// The city mints "gc" city-wide but not "pool": stripping the gc binding
		// off the assignee is provably this city's own legacy spelling, stripping
		// the pool binding off the route is not. The guard refuses the route, so
		// the assignee stays too — a row half-migrated to a canonical assignee on
		// a foreign route would neither claim nor serve.
		cfg := &config.City{
			Imports: map[string]config.Import{"gc": {Source: "./packs/gc"}},
			Agents:  []config.Agent{poolAgent("omp", "fixture", intPtr(3), 0)},
		}

		wb := workBead("fx-mix", "fixture/pool.omp", "fixture/gc.omp", "in_progress", 5)
		mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
		store := &updateCountingStore{Store: mem}

		report := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"fixture"}, newSessionBeadSnapshot(nil), io.Discard)

		if store.updates != 0 {
			t.Errorf("mixed row written %d time(s), want 0 (never half-migrate)", store.updates)
		}
		got, err := mem.Get("fx-mix")
		if err != nil {
			t.Fatalf("Get(fx-mix): %v", err)
		}
		if got.Assignee != "fixture/gc.omp" || got.Metadata["gc.routed_to"] != "fixture/pool.omp" {
			t.Errorf("row = (assignee %q, route %q), want both fields untouched", got.Assignee, got.Metadata["gc.routed_to"])
		}
		if len(report.Rehomed) != 0 {
			t.Errorf("report rehomed %+v, want none", report.Rehomed)
		}
		if len(report.Refused) != 1 {
			t.Fatalf("report refused %+v, want exactly the route refusal", report.Refused)
		}
		want := legacyRehomeEntry{ID: "fx-mix", Leg: "fixture", Field: "gc.routed_to", From: "fixture/pool.omp", To: "fixture/omp"}
		if report.Refused[0] != want {
			t.Errorf("refusal = %+v, want %+v", report.Refused[0], want)
		}
	})
}

// TestCanonicalizeLegacyAssignedRehomeMismatchedRefsWritesNothing pins the refs
// contract for the assigned arm, mirroring the routed arm's rule: workRefs must
// be index-aligned with workBeads/workStores, and a length mismatch writes
// nothing and reports nothing.
func TestCanonicalizeLegacyAssignedRehomeMismatchedRefsWritesNothing(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0)}}
	const legacy = "rig-A/gc.planner"

	wb := workBead("wb-asg", legacy, legacy, "in_progress", 5)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
	store := &updateCountingStore{Store: mem}
	report := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, nil, newSessionBeadSnapshot(nil), io.Discard)
	if store.updates != 0 {
		t.Errorf("assigned arm wrote %d time(s) with mismatched refs", store.updates)
	}
	if len(report.Rehomed) != 0 || len(report.Refused) != 0 {
		t.Errorf("mismatched refs reported %+v, want an empty report", report)
	}
}

// TestCanonicalizeLegacyAssignedRehomeDegradedSnapshotWritesNothing pins the
// degraded-snapshot rule through the report: a nil or load-errored snapshot
// cannot prove a legacy owner is gone, so the pass skips whole and returns an
// empty report — no rehomes, no refusals, no writes.
func TestCanonicalizeLegacyAssignedRehomeDegradedSnapshotWritesNothing(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0)}}
	const legacy = "rig-A/gc.planner"

	for name, snap := range map[string]*sessionBeadSnapshot{
		"nil snapshot":          nil,
		"load-errored snapshot": newSessionBeadSnapshotWithError(errors.New("dolt down")),
	} {
		t.Run(name, func(t *testing.T) {
			wb := workBead("wb-asg", legacy, legacy, "in_progress", 5)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}
			report := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{""}, snap, io.Discard)
			if store.updates != 0 {
				t.Errorf("degraded snapshot wrote %d time(s)", store.updates)
			}
			if len(report.Rehomed) != 0 || len(report.Refused) != 0 {
				t.Errorf("degraded snapshot reported %+v, want empty", report)
			}
		})
	}
}

// TestCanonicalizeLegacyRoutedWorkRehomesOwnMintedBindingOnSharedRigLeg is
// AC-4: the guard's allowance is not just the city-wide import table. A binding
// minted on the leg's rig through any source the pool orphan sweeper counts —
// the city-wide imports, that rig's own import table, or a configured agent
// bound to it that runs on the rig — is this city's own legacy spelling, so the
// strip rewrite goes through on a shared leg and converges after one write.
func TestCanonicalizeLegacyRoutedWorkRehomesOwnMintedBindingOnSharedRigLeg(t *testing.T) {
	unboundOmp := poolAgent("omp", "qcore", intPtr(3), 0)
	boundOther := poolAgent("other", "qcore", intPtr(3), 0)
	boundOther.BindingName = "pool"
	cases := []struct {
		name string
		cfg  *config.City
	}{
		{
			name: "city-wide import",
			cfg: &config.City{
				Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
				Agents:  []config.Agent{unboundOmp},
			},
		},
		{
			name: "the leg rig's own import",
			cfg: &config.City{
				Rigs: []config.Rig{{
					Name:    "qcore",
					Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
				}},
				Agents: []config.Agent{unboundOmp},
			},
		},
		{
			name: "a bound agent that runs on the rig",
			cfg: &config.City{
				Agents: []config.Agent{unboundOmp, boundOther},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wb := workBead("wb-mint", "qcore/pool.omp", "", "open", 2)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}

			report := canonicalizeLegacyBoundUnassignedRoutedWork(tc.cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:qcore"}, io.Discard)

			if store.updates != 1 {
				t.Fatalf("%s: wrote %d time(s), want the one canonicalizing write", tc.name, store.updates)
			}
			if len(report.Rehomed) != 1 || report.Rehomed[0].To != "qcore/omp" {
				t.Errorf("%s: rehomed %+v, want the strip to qcore/omp", tc.name, report.Rehomed)
			}
			if len(report.Refused) != 0 {
				t.Errorf("%s: refused %+v, want none (this city mints the binding)", tc.name, report.Refused)
			}
			// Second pass: the row is canonical, the pass is silent.
			cur, _ := mem.Get("wb-mint")
			store.updates = 0
			canonicalizeLegacyBoundUnassignedRoutedWork(tc.cfg, []beads.Bead{cur}, []beads.Store{store}, []string{"rig:qcore"}, io.Discard)
			if store.updates != 0 {
				t.Errorf("%s: second pass wrote %d time(s), want a converged no-write pass", tc.name, store.updates)
			}
		})
	}
}

// TestCanonicalizeLegacyRehomeOwnLegsKeepLegacyBehavior is AC-5: on the city's
// own legs — the empty ref, a scoped city ref, and a relocated-class ref — both
// arms keep every pre-guard behavior, in both directions: the #3378 strip of a
// binding segment and the #5293 add of one, including the assigned arm's
// two-fields-one-write shape and the live-legacy-owner skip.
func TestCanonicalizeLegacyRehomeOwnLegsKeepLegacyBehavior(t *testing.T) {
	stripCfg := &config.City{Agents: []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0)}}
	addCfg := &config.City{Agents: []config.Agent{{
		Name: "refinery", Dir: "gascity", BindingName: "gastown", MaxActiveSessions: intPtr(1),
	}}}
	legs := []string{"", "city:test-city", "class:ga-classes"}

	for _, leg := range legs {
		leg := leg

		t.Run("routed strip on "+leg, func(t *testing.T) {
			wb := workBead("wb-strip", "rig-A/gc.planner", "", "open", 5)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}

			report := canonicalizeLegacyBoundUnassignedRoutedWork(stripCfg, []beads.Bead{wb}, []beads.Store{store}, []string{leg}, io.Discard)

			if store.updates != 1 {
				t.Fatalf("wrote %d time(s), want the one canonicalizing write", store.updates)
			}
			if got, _ := mem.Get("wb-strip"); got.Metadata["gc.routed_to"] != "rig-A/planner" {
				t.Errorf("gc.routed_to = %q, want rig-A/planner", got.Metadata["gc.routed_to"])
			}
			if len(report.Rehomed) != 1 || report.Rehomed[0].To != "rig-A/planner" || report.Rehomed[0].Leg != leg {
				t.Errorf("rehomed %+v, want the strip on %q", report.Rehomed, leg)
			}
			if len(report.Refused) != 0 {
				t.Errorf("refused %+v, want none on an own leg", report.Refused)
			}
		})

		t.Run("routed add on "+leg, func(t *testing.T) {
			wb := workBead("wb-add", "gascity/refinery", "", "open", 5)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}

			report := canonicalizeLegacyBoundUnassignedRoutedWork(addCfg, []beads.Bead{wb}, []beads.Store{store}, []string{leg}, io.Discard)

			if store.updates != 1 {
				t.Fatalf("wrote %d time(s), want the one canonicalizing write", store.updates)
			}
			if got, _ := mem.Get("wb-add"); got.Metadata["gc.routed_to"] != "gascity/gastown.refinery" {
				t.Errorf("gc.routed_to = %q, want gascity/gastown.refinery", got.Metadata["gc.routed_to"])
			}
			if len(report.Rehomed) != 1 || report.Rehomed[0].To != "gascity/gastown.refinery" {
				t.Errorf("rehomed %+v, want the add to gascity/gastown.refinery", report.Rehomed)
			}
			if len(report.Refused) != 0 {
				t.Errorf("refused %+v, want none on an own leg", report.Refused)
			}
		})

		t.Run("assigned strip on "+leg, func(t *testing.T) {
			wb := workBead("wb-asg", "rig-A/gc.planner", "rig-A/gc.planner", "in_progress", 5)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}

			report := canonicalizeLegacyBoundAssignedWork(stripCfg, []beads.Bead{wb}, []beads.Store{store}, []string{leg}, newSessionBeadSnapshot(nil), io.Discard)

			if store.updates != 1 {
				t.Fatalf("wrote %d time(s), want one Update carrying both fields", store.updates)
			}
			if got, _ := mem.Get("wb-asg"); got.Assignee != "rig-A/planner" || got.Metadata["gc.routed_to"] != "rig-A/planner" {
				t.Errorf("row = (assignee %q, route %q), want both rig-A/planner", got.Assignee, got.Metadata["gc.routed_to"])
			}
			if len(report.Rehomed) != 2 {
				t.Errorf("rehomed %+v, want one entry per changed field", report.Rehomed)
			}
			if len(report.Refused) != 0 {
				t.Errorf("refused %+v, want none on an own leg", report.Refused)
			}
		})

		t.Run("assigned add on "+leg, func(t *testing.T) {
			wb := workBead("wb-asg", "gascity/refinery", "gascity/refinery", "in_progress", 5)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}

			report := canonicalizeLegacyBoundAssignedWork(addCfg, []beads.Bead{wb}, []beads.Store{store}, []string{leg}, newSessionBeadSnapshot(nil), io.Discard)

			if store.updates != 1 {
				t.Fatalf("wrote %d time(s), want one Update carrying both fields", store.updates)
			}
			if got, _ := mem.Get("wb-asg"); got.Assignee != "gascity/gastown.refinery" || got.Metadata["gc.routed_to"] != "gascity/gastown.refinery" {
				t.Errorf("row = (assignee %q, route %q), want both gascity/gastown.refinery", got.Assignee, got.Metadata["gc.routed_to"])
			}
			if len(report.Rehomed) != 2 || len(report.Refused) != 0 {
				t.Errorf("report = (rehomed %+v, refused %+v), want two rehomes, no refusals", report.Rehomed, report.Refused)
			}
		})
	}

	t.Run("class leg live legacy owner is preserved", func(t *testing.T) {
		const legacy = "gascity/refinery"
		wb := workBead("wb-live", "gascity/gastown.refinery", legacy, "in_progress", 5)
		mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
		store := &updateCountingStore{Store: mem}
		live := beads.Bead{
			ID: "sess-legacy-live", Type: sessionBeadType, Status: "open",
			Metadata: map[string]string{"session_name": legacy, "template": legacy, "state": "active"},
		}

		report := canonicalizeLegacyBoundAssignedWork(addCfg, []beads.Bead{wb}, []beads.Store{store}, []string{"class:ga-classes"}, newSessionBeadSnapshot([]beads.Bead{live}), io.Discard)

		if store.updates != 0 {
			t.Errorf("re-homed work owned by a live legacy session, got %d writes (want 0)", store.updates)
		}
		if len(report.Rehomed) != 0 || len(report.Refused) != 0 {
			t.Errorf("report = %+v, want empty (the row is skipped, not refused)", report)
		}
	})
}
