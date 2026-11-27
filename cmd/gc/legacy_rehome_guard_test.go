package main

import (
	"io"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The #5846 topology: two cities attached to one rig store "qcore". City A
// runs the pool agent unbound ("qcore/omp"); city B imports it under the
// "pool" binding ("qcore/pool.omp"). Each city's permissive legacy resolver
// reads the other's canonical identity as a legacy spelling of its own.
const (
	sharedRigUnboundOmp = "qcore/omp"
	sharedRigBoundOmp   = "qcore/pool.omp"
)

func sharedRigUnboundOmpCity() *config.City {
	return &config.City{Agents: []config.Agent{poolAgent("omp", "qcore", intPtr(3), 0)}}
}

// AC-1

// TestCanonicalizeLegacyRoutedWorkLeavesForeignBindingOnSharedRigLeg: city A
// mints no "pool" binding on qcore, so "qcore/pool.omp" on a rig leg is city
// B's canonical route, not A's legacy spelling.
func TestCanonicalizeLegacyRoutedWorkLeavesForeignBindingOnSharedRigLeg(t *testing.T) {
	cfg := sharedRigUnboundOmpCity()
	wb := workBead("wb-1", sharedRigBoundOmp, "", "open", 2)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
	store := &updateCountingStore{Store: mem}

	report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:qcore"}, io.Discard)

	if store.updates != 0 {
		t.Errorf("Update calls = %d, want 0 (foreign binding on a shared rig leg)", store.updates)
	}
	if got := mustGetBead(t, mem, "wb-1").Metadata[beadmeta.RoutedToMetadataKey]; got != sharedRigBoundOmp {
		t.Errorf("gc.routed_to = %q, want %q", got, sharedRigBoundOmp)
	}
	want := legacyRehomeReport{Refused: []legacyRehomeRow{{
		ID: "wb-1", Leg: "rig:qcore",
		Changes: []legacyRehomeChange{{Field: beadmeta.RoutedToMetadataKey, From: sharedRigBoundOmp, To: sharedRigUnboundOmp}},
	}}}
	if !reflect.DeepEqual(report, want) {
		t.Errorf("report = %+v, want %+v", report, want)
	}
}

// TestLegacyRehomeLegIsOwn: the city leg in both census vocabularies and a
// class binding are own legs; rig legs and unrecognized refs are shared.
func TestLegacyRehomeLegIsOwn(t *testing.T) {
	for ref, want := range map[string]bool{
		"":           true,
		"city:city":  true,
		"class:work": true,
		"class:":     false,
		"qcore":      false,
		"rig:qcore":  false,
		"weird:x":    false,
	} {
		if got := legacyRehomeLegIsOwn(ref); got != want {
			t.Errorf("legacyRehomeLegIsOwn(%q) = %v, want %v", ref, got, want)
		}
	}
}

// TestLegacyRehomeAllowedOnSharedLeg pins which bindings count as minted on
// the row's rig, and that the add direction is never provable on a shared leg.
func TestLegacyRehomeAllowedOnSharedLeg(t *testing.T) {
	unbound := poolAgent("omp", "qcore", intPtr(3), 0)
	bound := unbound
	bound.BindingName = "pool"
	poolImport := map[string]config.Import{"pool": {Source: "./packs/pool"}}
	cases := []struct {
		name  string
		cfg   *config.City
		own   bool
		from  string
		agent *config.Agent
		want  bool
	}{
		{"own leg allows the add direction", &config.City{}, true, "qcore/omp", &bound, true},
		{"own leg allows an unminted strip", &config.City{}, true, "qcore/pool.omp", &unbound, true},
		{"shared leg refuses the add direction", &config.City{Imports: poolImport}, false, "qcore/omp", &bound, false},
		{"shared leg allows a city-minted strip", &config.City{Imports: poolImport}, false, "qcore/pool.omp", &unbound, true},
		{"shared leg refuses an unminted strip", &config.City{}, false, "qcore/pool.omp", &unbound, false},
		{"[defaults.rig.imports] alone does not mint", &config.City{DefaultRigImports: poolImport}, false, "qcore/pool.omp", &unbound, false},
		{"a binding imported only on another rig does not mint", &config.City{Rigs: []config.Rig{{Name: "alpha", Imports: poolImport}}}, false, "qcore/pool.omp", &unbound, false},
		{"a binding imported on the row's rig mints", &config.City{Rigs: []config.Rig{{Name: "qcore", Imports: poolImport}}}, false, "qcore/pool.omp", &unbound, true},
		{"empty binding is refused", &config.City{Imports: poolImport}, false, "qcore/.omp", &unbound, false},
		{"no binding segment is refused", &config.City{Imports: poolImport}, false, "qcore/omp", &unbound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyRehomeAllowed(tc.cfg, tc.own, tc.from, tc.agent); got != tc.want {
				t.Errorf("legacyRehomeAllowed(own=%v, %q) = %v, want %v", tc.own, tc.from, got, tc.want)
			}
		})
	}
}

// TestCanonicalizeLegacyRehomeSharedRigLegEdges: the known cost (a #3378 strip
// of an undeclared binding on a rig leg is refused and reported), the
// misaligned-refs rule, and an empty report on a degraded snapshot.
func TestCanonicalizeLegacyRehomeSharedRigLegEdges(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy, canonical = "rig-A/gc.planner", "rig-A/planner"

	t.Run("#3378 strip on a rig leg is refused and reported", func(t *testing.T) {
		wb := workBead("wb-1", legacy, "", "open", 2)
		store := &updateCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)}
		report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:rig-A"}, io.Discard)
		want := legacyRehomeReport{Refused: []legacyRehomeRow{{
			ID: "wb-1", Leg: "rig:rig-A",
			Changes: []legacyRehomeChange{{Field: beadmeta.RoutedToMetadataKey, From: legacy, To: canonical}},
		}}}
		if store.updates != 0 || !reflect.DeepEqual(report, want) {
			t.Errorf("updates = %d, report = %+v; want 0 and %+v", store.updates, report, want)
		}
	})

	t.Run("misaligned refs write nothing", func(t *testing.T) {
		wb := workBead("wb-1", legacy, legacy, "in_progress", 2)
		store := &updateCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)}
		a := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, nil, newSessionBeadSnapshot(nil), io.Discard)
		open := workBead("wb-1", legacy, "", "open", 2)
		r := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{open}, []beads.Store{store}, []string{"", ""}, io.Discard)
		if store.updates != 0 || !reflect.DeepEqual(a, legacyRehomeReport{}) || !reflect.DeepEqual(r, legacyRehomeReport{}) {
			t.Errorf("updates = %d, reports = %+v / %+v; want 0 and empty", store.updates, a, r)
		}
	})

	t.Run("degraded snapshot yields an empty report", func(t *testing.T) {
		wb := workBead("wb-1", sharedRigBoundOmp, sharedRigBoundOmp, "in_progress", 2)
		store := &updateCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)}
		report := canonicalizeLegacyBoundAssignedWork(sharedRigUnboundOmpCity(), []beads.Bead{wb}, []beads.Store{store}, []string{"qcore"}, nil, io.Discard)
		if store.updates != 0 || !reflect.DeepEqual(report, legacyRehomeReport{}) {
			t.Errorf("updates = %d, report = %+v; want 0 and empty", store.updates, report)
		}
	})
}

// AC-2

func sharedRigBoundOmpCity() *config.City {
	agent := poolAgent("omp", "qcore", intPtr(3), 0)
	agent.BindingName = "pool"
	return &config.City{
		Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
		Agents:  []config.Agent{agent},
	}
}

// TestCanonicalizeLegacyRoutedWorkRefusesAddDirectionOnSharedRigLeg: city B
// cannot prove a bare "qcore/omp" on a rig leg is its own legacy spelling; it
// is city A's canonical route.
func TestCanonicalizeLegacyRoutedWorkRefusesAddDirectionOnSharedRigLeg(t *testing.T) {
	cfg := sharedRigBoundOmpCity()
	wb := workBead("wb-1", sharedRigUnboundOmp, "", "open", 2)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
	store := &updateCountingStore{Store: mem}

	report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:qcore"}, io.Discard)

	if store.updates != 0 {
		t.Errorf("Update calls = %d, want 0 (bare identity on a shared rig leg)", store.updates)
	}
	if got := mustGetBead(t, mem, "wb-1").Metadata[beadmeta.RoutedToMetadataKey]; got != sharedRigUnboundOmp {
		t.Errorf("gc.routed_to = %q, want %q", got, sharedRigUnboundOmp)
	}
	if len(report.Rehomed) != 0 || len(report.Refused) != 1 {
		t.Errorf("report = %+v, want one refused row and none rehomed", report)
	}
}

// AC-3

// TestCanonicalizeLegacyAssignedWorkLeavesForeignInFlightOnSharedRigLeg: city
// B's in-flight row is invisible to city A's session snapshot, so A must not
// move it; and a row whose two fields get different verdicts is never
// half-migrated.
func TestCanonicalizeLegacyAssignedWorkLeavesForeignInFlightOnSharedRigLeg(t *testing.T) {
	t.Run("both fields foreign", func(t *testing.T) {
		cfg := sharedRigUnboundOmpCity()
		wb := workBead("wb-1", sharedRigBoundOmp, sharedRigBoundOmp, "in_progress", 2)
		mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
		store := &updateCountingStore{Store: mem}

		report := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"qcore"}, newSessionBeadSnapshot(nil), io.Discard)

		if store.updates != 0 {
			t.Errorf("Update calls = %d, want 0", store.updates)
		}
		got := mustGetBead(t, mem, "wb-1")
		if got.Assignee != sharedRigBoundOmp || got.Metadata[beadmeta.RoutedToMetadataKey] != sharedRigBoundOmp {
			t.Errorf("assignee/route = %q/%q, want both %q", got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey], sharedRigBoundOmp)
		}
		want := legacyRehomeReport{Refused: []legacyRehomeRow{{
			ID: "wb-1", Leg: "qcore",
			Changes: []legacyRehomeChange{
				{Field: legacyRehomeFieldAssignee, From: sharedRigBoundOmp, To: sharedRigUnboundOmp},
				{Field: beadmeta.RoutedToMetadataKey, From: sharedRigBoundOmp, To: sharedRigUnboundOmp},
			},
		}}}
		if !reflect.DeepEqual(report, want) {
			t.Errorf("report = %+v, want %+v", report, want)
		}
	})

	t.Run("allowed assignee strip with refused route add", func(t *testing.T) {
		worker := poolAgent("worker", "qcore", intPtr(3), 0)
		worker.BindingName = "pool"
		cfg := &config.City{
			// "gc" is minted city-wide, so the assignee strip alone is allowed.
			Imports: map[string]config.Import{"gc": {Source: "./packs/gc"}},
			Agents:  []config.Agent{poolAgent("omp", "qcore", intPtr(3), 0), worker},
		}
		wb := workBead("wb-2", "qcore/worker", "qcore/gc.omp", "in_progress", 2)
		mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
		store := &updateCountingStore{Store: mem}

		report := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"qcore"}, newSessionBeadSnapshot(nil), io.Discard)

		if store.updates != 0 {
			t.Errorf("Update calls = %d, want 0 (a half-refused row is not written at all)", store.updates)
		}
		got := mustGetBead(t, mem, "wb-2")
		if got.Assignee != "qcore/gc.omp" || got.Metadata[beadmeta.RoutedToMetadataKey] != "qcore/worker" {
			t.Errorf("assignee/route = %q/%q, want unchanged", got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey])
		}
		want := legacyRehomeReport{Refused: []legacyRehomeRow{{
			ID: "wb-2", Leg: "qcore",
			Changes: []legacyRehomeChange{{Field: beadmeta.RoutedToMetadataKey, From: "qcore/worker", To: "qcore/pool.worker"}},
		}}}
		if !reflect.DeepEqual(report, want) {
			t.Errorf("report = %+v, want %+v", report, want)
		}
	})
}

// AC-4

// TestCanonicalizeLegacyRoutedWorkRehomesOwnMintedBindingOnSharedRigLeg: a
// binding this city mints on the rig is its own legacy spelling, from each of
// the three sources cityMintsBindingOnRig reads, and re-homes exactly once.
func TestCanonicalizeLegacyRoutedWorkRehomesOwnMintedBindingOnSharedRigLeg(t *testing.T) {
	const legacy, canonical = "R/b.name", "R/name"
	minter := poolAgent("other", "R", intPtr(1), 0)
	minter.BindingName = "b"
	cases := map[string]*config.City{
		"city import": {
			Imports: map[string]config.Import{"b": {Source: "./packs/b"}},
			Agents:  []config.Agent{poolAgent("name", "R", intPtr(3), 0)},
		},
		"rig import on R": {
			Rigs:   []config.Rig{{Name: "R", Imports: map[string]config.Import{"b": {Source: "./packs/b"}}}},
			Agents: []config.Agent{poolAgent("name", "R", intPtr(3), 0)},
		},
		"agent binding on R": {
			Agents: []config.Agent{poolAgent("name", "R", intPtr(3), 0), minter},
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			wb := workBead("wb-1", legacy, "", "open", 2)
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &updateCountingStore{Store: mem}
			refs := []string{"rig:R"}

			report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, refs, io.Discard)

			rehomed := mustGetBead(t, mem, "wb-1")
			if got := rehomed.Metadata[beadmeta.RoutedToMetadataKey]; got != canonical {
				t.Fatalf("gc.routed_to = %q, want %q", got, canonical)
			}
			want := legacyRehomeReport{Rehomed: []legacyRehomeRow{{
				ID: "wb-1", Leg: "rig:R",
				Changes: []legacyRehomeChange{{Field: beadmeta.RoutedToMetadataKey, From: legacy, To: canonical}},
			}}}
			if !reflect.DeepEqual(report, want) {
				t.Errorf("report = %+v, want %+v", report, want)
			}

			again := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{rehomed}, []beads.Store{store}, refs, io.Discard)
			if store.updates != 1 {
				t.Errorf("Update calls = %d, want 1 (second pass writes nothing)", store.updates)
			}
			if len(again.Rehomed)+len(again.Refused) != 0 {
				t.Errorf("second pass report = %+v, want empty", again)
			}
		})
	}
}

// AC-5

// TestCanonicalizeLegacyRehomeOwnLegsKeepMigrationRecovery: the #3378 strip
// and #5293 add recoveries still run on every leg this city owns: the city
// work store under either vocabulary and a relocated class binding.
func TestCanonicalizeLegacyRehomeOwnLegsKeepMigrationRecovery(t *testing.T) {
	fixtures := []struct {
		name              string
		cfg               *config.City
		legacy, canonical string
	}{
		{"#3378 strip", legacyBoundRecoveryConfig(), "rig-A/gc.planner", "rig-A/planner"},
		{"#5293 add", &config.City{Agents: []config.Agent{{
			Name: "refinery", Dir: "gascity", BindingName: "gastown", MaxActiveSessions: intPtr(1),
		}}}, "gascity/refinery", "gascity/gastown.refinery"},
	}
	for _, fx := range fixtures {
		for _, leg := range []string{"", "city:city", "class:work"} {
			t.Run(fx.name+"/assigned/"+leg, func(t *testing.T) {
				wb := workBead("wb-a", fx.legacy, fx.legacy, "in_progress", 2)
				mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)

				report := canonicalizeLegacyBoundAssignedWork(fx.cfg, []beads.Bead{wb}, []beads.Store{mem}, []string{leg}, newSessionBeadSnapshot(nil), io.Discard)

				got := mustGetBead(t, mem, "wb-a")
				if got.Assignee != fx.canonical || got.Metadata[beadmeta.RoutedToMetadataKey] != fx.canonical {
					t.Errorf("assignee/route = %q/%q, want both %q", got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey], fx.canonical)
				}
				if len(report.Rehomed) != 1 || len(report.Refused) != 0 {
					t.Errorf("report = %+v, want one rehomed row", report)
				}
			})
			t.Run(fx.name+"/routed/"+leg, func(t *testing.T) {
				wb := workBead("wb-r", fx.legacy, "", "open", 2)
				mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)

				report := canonicalizeLegacyBoundUnassignedRoutedWork(fx.cfg, []beads.Bead{wb}, []beads.Store{mem}, []string{leg}, io.Discard)

				if got := mustGetBead(t, mem, "wb-r").Metadata[beadmeta.RoutedToMetadataKey]; got != fx.canonical {
					t.Errorf("gc.routed_to = %q, want %q", got, fx.canonical)
				}
				if len(report.Rehomed) != 1 || len(report.Refused) != 0 {
					t.Errorf("report = %+v, want one rehomed row", report)
				}
			})
		}
	}
}
