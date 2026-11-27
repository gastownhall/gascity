package main

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
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

// AC-6

// TestCanonicalizeLegacyRehomeTwoCitiesSharedRigConverges is the #5846
// reproduction: cities A and B alternate both re-home arms over one shared rig
// store, each blind to the other's sessions, and nobody writes.
func TestCanonicalizeLegacyRehomeTwoCitiesSharedRigConverges(t *testing.T) {
	cityA, cityB := sharedRigUnboundOmpCity(), sharedRigBoundOmpCity()
	aRow := workBead("wb-a", sharedRigUnboundOmp, "", "open", 2)
	bRow := workBead("wb-b", sharedRigBoundOmp, sharedRigBoundOmp, "in_progress", 2)
	mem := beads.NewMemStoreFrom(0, []beads.Bead{aRow, bRow}, nil)
	shared := &updateCountingStore{Store: mem}

	for pass := 0; pass < 10; pass++ {
		cfg := cityA
		if pass%2 == 1 {
			cfg = cityB
		}
		rows := []beads.Bead{mustGetBead(t, mem, "wb-a"), mustGetBead(t, mem, "wb-b")}
		stores := []beads.Store{shared, shared}
		canonicalizeLegacyBoundAssignedWork(cfg, rows, stores, []string{"qcore", "qcore"}, newSessionBeadSnapshot(nil), io.Discard)
		rows = []beads.Bead{mustGetBead(t, mem, "wb-a"), mustGetBead(t, mem, "wb-b")}
		canonicalizeLegacyBoundUnassignedRoutedWork(cfg, rows, stores, []string{"rig:qcore", "rig:qcore"}, io.Discard)
	}

	if shared.updates != 0 {
		t.Errorf("shared rig store received %d Update calls over 10 alternating passes, want 0", shared.updates)
	}
	a, b := mustGetBead(t, mem, "wb-a"), mustGetBead(t, mem, "wb-b")
	if a.Assignee != "" || a.Metadata[beadmeta.RoutedToMetadataKey] != sharedRigUnboundOmp {
		t.Errorf("A's row assignee/route = %q/%q, want \"\"/%q", a.Assignee, a.Metadata[beadmeta.RoutedToMetadataKey], sharedRigUnboundOmp)
	}
	if b.Assignee != sharedRigBoundOmp || b.Metadata[beadmeta.RoutedToMetadataKey] != sharedRigBoundOmp {
		t.Errorf("B's row assignee/route = %q/%q, want both %q", b.Assignee, b.Metadata[beadmeta.RoutedToMetadataKey], sharedRigBoundOmp)
	}
}

// AC-7

// sharedRigBackstopEnv is the backstop lane's repair fixture with its stores
// replaced: city holds the city store's rows and the attached rig "fixture"
// holds rig's. The session snapshot is complete and empty.
func sharedRigBackstopEnv(t *testing.T, city, rig []beads.Bead) (externalReadsEnv, *writeLog) {
	t.Helper()
	f := newRepairGoldenFixture(t)
	log := &writeLog{}
	env := f.env
	env.CityStore = writeLogStore{Store: beads.NewMemStoreFrom(0, city, nil), log: log}
	env.RigStores = map[string]beads.Store{
		"fixture": writeLogStore{Store: beads.NewMemStoreFrom(0, rig, nil), log: log},
		"nodisp":  writeLogStore{Store: beads.NewMemStore(), log: log},
	}
	env.Sessions = newSessionBeadSnapshot(nil)
	return env, log
}

// TestBackstopDemandRepairsLeavesForeignRowsOnSharedRigLeg drives the v2
// backstop's repair sequence, which hands both canonicalizers the refs its
// collections return: the city row re-homes, the rig rows (whose "pool"
// binding this city does not mint on rig-A) are refused and summarized once
// per arm.
func TestBackstopDemandRepairsLeavesForeignRowsOnSharedRigLeg(t *testing.T) {
	const foreign = "rig-A/pool.planner"
	env, log := sharedRigBackstopEnv(t,
		[]beads.Bead{workBead("ga-asg", goldenLegacyPlanner, goldenLegacyPlanner, "in_progress", 5)},
		[]beads.Bead{
			workBead("fx-a1", foreign, foreign, "in_progress", 5),
			workBead("fx-a2", foreign, foreign, "in_progress", 5),
			workBead("fx-r1", foreign, "", "open", 5),
		})
	var stderr bytes.Buffer

	runBackstopDemandRepairs(context.Background(), env, &stderr)

	wantWrites := []string{"Update ga-asg assignee=" + goldenCanonicalPlanner + " " + beadmeta.RoutedToMetadataKey + "=" + goldenCanonicalPlanner}
	if !slices.Equal(log.ops, wantWrites) {
		t.Errorf("writes\n got  %q\n want %q", log.ops, wantWrites)
	}
	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	var rehomed, assignedRefused, routedRefused []string
	for _, line := range lines {
		switch {
		case strings.Contains(line, " rehomed "):
			rehomed = append(rehomed, line)
		case strings.HasPrefix(line, "canonicalizeLegacyBoundAssignedWork: refused "):
			assignedRefused = append(assignedRefused, line)
		case strings.HasPrefix(line, "canonicalizeLegacyBoundUnassignedRoutedWork: refused "):
			routedRefused = append(routedRefused, line)
		}
	}
	if len(rehomed) != 1 || !strings.Contains(rehomed[0], "rehomed ga-asg on city: ") {
		t.Errorf("rehomed lines = %q, want exactly one for ga-asg on city", rehomed)
	}
	pair := `"` + foreign + `" -> "` + goldenCanonicalPlanner + `"`
	if len(assignedRefused) != 1 || !strings.Contains(assignedRefused[0], pair+" (2: fx-a1, fx-a2)") {
		t.Errorf("assigned refusal summaries = %q, want one with %s (2: fx-a1, fx-a2)", assignedRefused, pair)
	}
	if len(routedRefused) != 1 || !strings.Contains(routedRefused[0], pair+" (1: fx-r1)") {
		t.Errorf("routed refusal summaries = %q, want one with %s (1: fx-r1)", routedRefused, pair)
	}

	t.Run("a pass with nothing to do logs nothing", func(t *testing.T) {
		env, _ := sharedRigBackstopEnv(t,
			[]beads.Bead{workBead("ga-ok", goldenCanonicalPlanner, goldenCanonicalPlanner, "in_progress", 5)},
			[]beads.Bead{workBead("fx-ok", goldenCanonicalPlanner, "", "open", 5)})
		var quiet bytes.Buffer
		runBackstopDemandRepairs(context.Background(), env, &quiet)
		if quiet.Len() != 0 {
			t.Errorf("stderr = %q, want nothing", quiet.String())
		}
	})
}

// TestLegacyRehomeReportLog pins the log format: one line per rewritten row on
// its normalized leg, one refusal summary with %q-quoted identities, exact
// counts and a five-ID sample, and nothing for an empty report.
func TestLegacyRehomeReportLog(t *testing.T) {
	var empty bytes.Buffer
	legacyRehomeReport{}.log(&empty, "repair")
	if empty.Len() != 0 {
		t.Errorf("empty report logged %q", empty.String())
	}

	forged := "qcore/x,\nfake"
	report := legacyRehomeReport{
		Rehomed: []legacyRehomeRow{{ID: "ga-1", Leg: "qcore", Changes: []legacyRehomeChange{
			{Field: legacyRehomeFieldAssignee, From: "qcore/gc.omp", To: "qcore/omp"},
			{Field: beadmeta.RoutedToMetadataKey, From: "qcore/gc.omp", To: "qcore/omp"},
		}}},
		Refused: []legacyRehomeRow{{ID: "fx-0", Leg: "rig:qcore", Changes: []legacyRehomeChange{{Field: beadmeta.RoutedToMetadataKey, From: forged, To: "qcore/omp"}}}},
	}
	for i := 1; i <= 7; i++ {
		id := "fx-" + string(rune('0'+i))
		report.Refused = append(report.Refused, legacyRehomeRow{ID: id, Leg: "rig:qcore", Changes: []legacyRehomeChange{
			{Field: legacyRehomeFieldAssignee, From: sharedRigBoundOmp, To: sharedRigUnboundOmp},
			{Field: beadmeta.RoutedToMetadataKey, From: sharedRigBoundOmp, To: sharedRigUnboundOmp},
		}})
	}
	var out bytes.Buffer
	report.log(&out, "repair")

	want := []string{
		`repair: rehomed ga-1 on rig:qcore: assignee "qcore/gc.omp" -> "qcore/omp", gc.routed_to "qcore/gc.omp" -> "qcore/omp"`,
		`repair: refused 2 legacy re-home(s) on shared rig legs this pass (8 rows): ` +
			`"qcore/pool.omp" -> "qcore/omp" (7: fx-1, fx-2, fx-3, fx-4, fx-5 +2 more), ` +
			`"qcore/x,\nfake" -> "qcore/omp" (1: fx-0)`,
	}
	if got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n"); !slices.Equal(got, want) {
		t.Errorf("log lines\n got  %q\n want %q", got, want)
	}
}

// AC-8

// TestDoctorV2DemandMigrationsLeavesRefusedSharedRigRows: the doctor sweep runs
// the same guarded canonicalizers, so a refused rig row is neither counted as a
// repair nor written by --fix.
func TestDoctorV2DemandMigrationsLeavesRefusedSharedRigRows(t *testing.T) {
	const foreign = "rig-A/pool.planner"
	f := newRepairGoldenFixture(t)
	cfg := f.env.Cfg
	cfg.Daemon.SessionReconciler = config.SessionReconcilerV2
	city := beads.NewMemStoreFrom(0, []beads.Bead{workBead("ga-rlegacy", goldenLegacyPlanner, "", "open", 5)}, nil)
	rig := beads.NewMemStoreFrom(0, []beads.Bead{workBead("fx-foreign", foreign, "", "open", 5)}, nil)
	c := demandMigrationsCheckFor(t, cfg, f.env.CityPath, city, map[string]beads.Store{"fixture": rig, "nodisp": beads.NewMemStore()})
	ctx := &doctor.CheckContext{CityPath: f.env.CityPath}

	sweep := c.sweep(false, io.Discard)
	wantCounts := map[demandMigrationKey]int{{"canonicalizeLegacyBoundUnassignedRoutedWork", "city"}: 1}
	if !reflect.DeepEqual(sweep.counts, wantCounts) || len(sweep.problems) != 0 {
		t.Errorf("dry-run sweep counts = %v problems = %q, want %v and no problems", sweep.counts, sweep.problems, wantCounts)
	}
	r := c.Run(ctx)
	if strings.Contains(r.Message, "read problem") {
		t.Errorf("dry run message = %q, want no read problem", r.Message)
	}

	var out bytes.Buffer
	ctx.Output = &out
	if err := c.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got := mustGetBead(t, rig, "fx-foreign").Metadata[beadmeta.RoutedToMetadataKey]; got != foreign {
		t.Errorf("refused rig row gc.routed_to = %q, want %q", got, foreign)
	}
	if got := mustGetBead(t, city, "ga-rlegacy").Metadata[beadmeta.RoutedToMetadataKey]; got != goldenCanonicalPlanner {
		t.Errorf("own-leg row gc.routed_to = %q, want %q", got, goldenCanonicalPlanner)
	}
	logged := out.String()
	if !strings.Contains(logged, "canonicalizeLegacyBoundUnassignedRoutedWork: rehomed ga-rlegacy on city: ") ||
		!strings.Contains(logged, `"`+foreign+`" -> "`+goldenCanonicalPlanner+`" (1: fx-foreign)`) {
		t.Errorf("--fix output = %q, want the rehomed line and the refusal summary", logged)
	}
}
