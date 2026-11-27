package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

	report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:qcore"}, nil, io.Discard)

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

// TestCanonicalizeLegacyRehomeSharedRigLegEdges: a #3378 strip of an
// undeclared binding on a rig leg is refused and reported when nothing proves
// the identity is this city's, the misaligned-refs rule, and an empty report
// on a degraded snapshot.
func TestCanonicalizeLegacyRehomeSharedRigLegEdges(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy, canonical = "rig-A/gc.planner", "rig-A/planner"

	t.Run("#3378 strip on a rig leg without a session record is refused and reported", func(t *testing.T) {
		wb := workBead("wb-1", legacy, "", "open", 2)
		store := &updateCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)}
		report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:rig-A"}, nil, io.Discard)
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
		a := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, nil, nil, newSessionBeadSnapshot(nil), io.Discard)
		open := workBead("wb-1", legacy, "", "open", 2)
		r := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{open}, []beads.Store{store}, []string{"", ""}, nil, io.Discard)
		if store.updates != 0 || !reflect.DeepEqual(a, legacyRehomeReport{}) || !reflect.DeepEqual(r, legacyRehomeReport{}) {
			t.Errorf("updates = %d, reports = %+v / %+v; want 0 and empty", store.updates, a, r)
		}
	})

	t.Run("degraded snapshot yields an empty report", func(t *testing.T) {
		wb := workBead("wb-1", sharedRigBoundOmp, sharedRigBoundOmp, "in_progress", 2)
		store := &updateCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)}
		report := canonicalizeLegacyBoundAssignedWork(sharedRigUnboundOmpCity(), []beads.Bead{wb}, []beads.Store{store}, []string{"qcore"}, nil, nil, io.Discard)
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

	report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"rig:qcore"}, nil, io.Discard)

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

		report := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"qcore"}, nil, newSessionBeadSnapshot(nil), io.Discard)

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

		report := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, []string{"qcore"}, nil, newSessionBeadSnapshot(nil), io.Discard)

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

			report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, []beads.Store{store}, refs, nil, io.Discard)

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

			again := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{rehomed}, []beads.Store{store}, refs, nil, io.Discard)
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

				report := canonicalizeLegacyBoundAssignedWork(fx.cfg, []beads.Bead{wb}, []beads.Store{mem}, []string{leg}, nil, newSessionBeadSnapshot(nil), io.Discard)

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

				report := canonicalizeLegacyBoundUnassignedRoutedWork(fx.cfg, []beads.Bead{wb}, []beads.Store{mem}, []string{leg}, nil, io.Discard)

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
		canonicalizeLegacyBoundAssignedWork(cfg, rows, stores, []string{"qcore", "qcore"}, nil, newSessionBeadSnapshot(nil), io.Discard)
		rows = []beads.Bead{mustGetBead(t, mem, "wb-a"), mustGetBead(t, mem, "wb-b")}
		canonicalizeLegacyBoundUnassignedRoutedWork(cfg, rows, stores, []string{"rig:qcore", "rig:qcore"}, nil, io.Discard)
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
// repair nor written by --fix; the dry run names it in the details, as
// information rather than a problem.
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
	wantDetail := `canonicalizeLegacyBoundUnassignedRoutedWork: refused 1 legacy re-home(s) on shared rig legs this pass (1 rows): "` +
		foreign + `" -> "` + goldenCanonicalPlanner + `" (1: fx-foreign)`
	if !slices.Contains(r.Details, wantDetail) {
		t.Errorf("dry run details = %q, want %q", r.Details, wantDetail)
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

// Session-record provenance

// legacyRehomeEpoch dates the rows and session records the provenance tests
// place by hand. It is in the past, so a row or record a store stamps at its
// create comes after it.
var legacyRehomeEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// legacyRehomeAt is d after legacyRehomeEpoch.
func legacyRehomeAt(d time.Duration) time.Time {
	return legacyRehomeEpoch.Add(d)
}

// rowCreatedAt returns row dated as created at; the zero time leaves it
// undated.
func rowCreatedAt(row beads.Bead, at time.Time) beads.Bead {
	row.CreatedAt = at
	return row
}

// sessionRecordAt is a session record created from template at created and
// last written at lastWrite, closed unless open is set, for a store seeded
// with beads.NewMemStoreFrom: the history a city keeps of a session it ran,
// dated by hand.
func sessionRecordAt(id, template string, open bool, created, lastWrite time.Time) beads.Bead {
	status := "closed"
	if open {
		status = "open"
	}
	return beads.Bead{
		ID: id, Title: "session", Type: sessionBeadType, Status: status, CreatedAt: created, UpdatedAt: lastWrite,
		Metadata: map[string]string{"template": template},
	}
}

// seedSessionRecord writes a closed session record created from template into
// store: the history a city keeps of a session it ran, dated by the store when
// it is written.
func seedSessionRecord(t *testing.T, store beads.Store, template string) {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "session", Type: sessionBeadType, Metadata: map[string]string{"template": template}})
	if err != nil {
		t.Fatalf("Create session record: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close session record: %v", err)
	}
}

// sessionLookupStore counts the listings a session-record test makes and fails
// them with err when it is set; a partial-result err still returns the rows.
type sessionLookupStore struct {
	beads.Store
	lists int
	err   error
}

func (s *sessionLookupStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	rows, err := s.Store.List(query)
	switch {
	case err != nil || s.err == nil:
		return rows, err
	case beads.IsPartialResult(s.err):
		return rows, s.err
	default:
		return nil, s.err
	}
}

// TestCanonicalizeLegacyRehomeSessionRecordProvesOwnMigrationOnSharedRigLeg:
// in a city that migrated an agent itself (the #3378 strip and the #5293 add),
// both arms re-home its rig rows when its own session store shows it running
// the legacy identity at or after the rows were created: the rows are no newer
// than a record's creation, open or closed, rows older than the record
// included (a city's backlog predates the session that runs it). They refuse
// rows created after that (R1: this city's own new rows carry its canonical
// identity), however late the record was last written or closed (the seat a
// city keeps after a migration is written, and closed, while it runs the
// canonical identity), undated rows, and every row when the city holds no
// record of the legacy identity.
func TestCanonicalizeLegacyRehomeSessionRecordProvesOwnMigrationOnSharedRigLeg(t *testing.T) {
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
		rig, _ := config.ParseQualifiedName(fx.legacy)
		// Each record is created an hour after the epoch and last written at
		// hour 3; the rows are created at created.
		records := []struct {
			name     string
			template string
			open     bool
			created  time.Time
			proves   bool
		}{
			{"closed record created after the rows were", fx.legacy, false, legacyRehomeEpoch, true},
			{"closed record created as the rows were", fx.legacy, false, legacyRehomeAt(time.Hour), true},
			{"closed record created before the rows were and closed after", fx.legacy, false, legacyRehomeAt(2 * time.Hour), false},
			{"closed record closed as the rows were created", fx.legacy, false, legacyRehomeAt(3 * time.Hour), false},
			{"closed record created and closed before the rows were", fx.legacy, false, legacyRehomeAt(4 * time.Hour), false},
			{"open record created after the rows were", fx.legacy, true, legacyRehomeEpoch, true},
			{"open record created as the rows were", fx.legacy, true, legacyRehomeAt(time.Hour), true},
			{"open record created before the rows were and last written after", fx.legacy, true, legacyRehomeAt(2 * time.Hour), false},
			{"open record created and last written before the rows were", fx.legacy, true, legacyRehomeAt(4 * time.Hour), false},
			{"closed record and undated rows", fx.legacy, false, time.Time{}, false},
			{"open record and undated rows", fx.legacy, true, time.Time{}, false},
			{"record of the canonical identity only", fx.canonical, false, legacyRehomeEpoch, false},
			{"no record", "", false, legacyRehomeEpoch, false},
		}
		for _, rec := range records {
			t.Run(fx.name+"/"+rec.name, func(t *testing.T) {
				var history []beads.Bead
				if rec.template != "" {
					history = append(history, sessionRecordAt("rec-1", rec.template, rec.open, legacyRehomeAt(time.Hour), legacyRehomeAt(3*time.Hour)))
				}
				sessions := beads.NewMemStoreFrom(0, history, nil)
				assigned := rowCreatedAt(workBead("wb-a", fx.legacy, fx.legacy, "in_progress", 2), rec.created)
				routed := rowCreatedAt(workBead("wb-r", fx.legacy, "", "open", 2), rec.created)
				mem := beads.NewMemStoreFrom(0, []beads.Bead{assigned, routed}, nil)
				provenance := newLegacyRehomeProvenance(sessions)

				a := canonicalizeLegacyBoundAssignedWork(fx.cfg, []beads.Bead{assigned}, []beads.Store{mem}, []string{rig}, provenance, newSessionBeadSnapshot(nil), io.Discard)
				r := canonicalizeLegacyBoundUnassignedRoutedWork(fx.cfg, []beads.Bead{routed}, []beads.Store{mem}, []string{"rig:" + rig}, provenance, io.Discard)

				want, wantRehomed, wantRefused := fx.legacy, 0, 1
				if rec.proves {
					want, wantRehomed, wantRefused = fx.canonical, 1, 0
				}
				gotA, gotR := mustGetBead(t, mem, "wb-a"), mustGetBead(t, mem, "wb-r")
				if gotA.Assignee != want || gotA.Metadata[beadmeta.RoutedToMetadataKey] != want || gotR.Metadata[beadmeta.RoutedToMetadataKey] != want {
					t.Errorf("assigned row %q/%q, routed row %q; want all %q",
						gotA.Assignee, gotA.Metadata[beadmeta.RoutedToMetadataKey], gotR.Metadata[beadmeta.RoutedToMetadataKey], want)
				}
				for arm, report := range map[string]legacyRehomeReport{"assigned": a, "routed": r} {
					if len(report.Rehomed) != wantRehomed || len(report.Refused) != wantRefused || report.LookupErr != nil {
						t.Errorf("%s report = %+v, want %d rehomed and %d refused", arm, report, wantRehomed, wantRefused)
					}
				}
			})
		}
	}
}

// TestCanonicalizeLegacyRehomeRecordInTheSharedStoreProvesNothing: only the
// city's own session store vouches. An open session record of the legacy
// identity that sits in the shared rig store beside the rows proves nothing,
// since a store other cities share is exactly where a neighbor's records can
// sit.
func TestCanonicalizeLegacyRehomeRecordInTheSharedStoreProvesNothing(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"
	assigned := rowCreatedAt(workBead("wb-a", legacy, legacy, "in_progress", 2), legacyRehomeEpoch)
	routed := rowCreatedAt(workBead("wb-r", legacy, "", "open", 2), legacyRehomeEpoch)
	record := sessionRecordAt("rec-1", legacy, true, legacyRehomeEpoch, legacyRehomeAt(time.Hour))
	shared := &updateCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{assigned, routed, record}, nil)}
	provenance := newLegacyRehomeProvenance(beads.NewMemStore())

	a := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{assigned}, []beads.Store{shared}, []string{"rig-A"}, provenance, newSessionBeadSnapshot(nil), io.Discard)
	r := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{routed}, []beads.Store{shared}, []string{"rig:rig-A"}, provenance, io.Discard)

	if shared.updates != 0 || len(a.Refused) != 1 || len(r.Refused) != 1 {
		t.Errorf("updates = %d, refused %d/%d; want no write and both rows refused", shared.updates, len(a.Refused), len(r.Refused))
	}
}

// TestLegacyRehomeProvenanceLooksUpRefusedIdentitiesOncePerPass: the session
// store is read only for a rewrite the config refuses on a shared leg, once
// per identity for the two arms of a pass together.
func TestLegacyRehomeProvenanceLooksUpRefusedIdentitiesOncePerPass(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	cfg.Imports = map[string]config.Import{"pool": {Source: "./packs/pool"}}
	const unproven = "rig-A/gc.planner"
	rows := []beads.Bead{
		workBead("own-leg", unproven, "", "open", 2),
		workBead("minted", "rig-A/pool.planner", "", "open", 2),
		workBead("canonical", "rig-A/planner", "", "open", 2),
		workBead("unproven-1", unproven, "", "open", 2),
		workBead("unproven-2", unproven, "", "open", 2),
	}
	inFlight := workBead("unproven-3", unproven, unproven, "in_progress", 2)
	mem := beads.NewMemStoreFrom(0, append(slices.Clone(rows), inFlight), nil)
	sessions := &sessionLookupStore{Store: beads.NewMemStore()}
	provenance := newLegacyRehomeProvenance(sessions)

	routed := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, rows, []beads.Store{mem, mem, mem, mem, mem},
		[]string{"city:city", "rig:rig-A", "rig:rig-A", "rig:rig-A", "rig:rig-A"}, provenance, io.Discard)
	assigned := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{inFlight}, []beads.Store{mem}, []string{"rig-A"}, provenance, newSessionBeadSnapshot(nil), io.Discard)

	if sessions.lists != 1 {
		t.Errorf("session store listings = %d, want 1 (one per refused identity per pass)", sessions.lists)
	}
	var rehomed, refused []string
	for _, row := range append(routed.Rehomed, assigned.Rehomed...) {
		rehomed = append(rehomed, row.ID)
	}
	for _, row := range append(routed.Refused, assigned.Refused...) {
		refused = append(refused, row.ID)
	}
	if !slices.Equal(rehomed, []string{"own-leg", "minted"}) || !slices.Equal(refused, []string{"unproven-1", "unproven-2", "unproven-3"}) {
		t.Errorf("rehomed %q, refused %q; want [own-leg minted] and [unproven-1 unproven-2 unproven-3]", rehomed, refused)
	}

	t.Run("a pass over canonical rows reads nothing", func(t *testing.T) {
		quiet := &sessionLookupStore{Store: beads.NewMemStore()}
		row := workBead("canonical", "rig-A/planner", "rig-A/planner", "in_progress", 2)
		canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{row}, []beads.Store{mem}, []string{"rig-A"}, newLegacyRehomeProvenance(quiet), newSessionBeadSnapshot(nil), io.Discard)
		if quiet.lists != 0 {
			t.Errorf("session store listings = %d, want 0", quiet.lists)
		}
	})
}

// TestLegacyRehomeProvenanceLookupFailure: a failed provenance lookup proves
// nothing. A hard session-record failure refuses the identity, ends the pass's
// lookups, and is reported once, by the arm that met it; a partial result
// still credits the record it returned; a failed molecule-root read dates the
// row by its own creation and is reported the same way.
func TestLegacyRehomeProvenanceLookupFailure(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0), poolAgent("worker", "rig-A", intPtr(5), 0)}}

	t.Run("hard failure", func(t *testing.T) {
		mem := beads.NewMemStore()
		seedSessionRecord(t, mem, "rig-A/gc.planner")
		sessions := &sessionLookupStore{Store: mem, err: errors.New("session store unreachable")}
		provenance := newLegacyRehomeProvenance(sessions)
		assigned := rowCreatedAt(workBead("wb-a", "rig-A/gc.planner", "rig-A/gc.planner", "in_progress", 2), legacyRehomeEpoch)
		routed := rowCreatedAt(workBead("wb-r", "rig-A/gc.worker", "", "open", 2), legacyRehomeEpoch)
		work := &updateCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{assigned, routed}, nil)}

		a := canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{assigned}, []beads.Store{work}, []string{"rig-A"}, provenance, newSessionBeadSnapshot(nil), io.Discard)
		r := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{routed}, []beads.Store{work}, []string{"rig:rig-A"}, provenance, io.Discard)

		if work.updates != 0 || len(a.Refused) != 1 || len(r.Refused) != 1 {
			t.Errorf("updates = %d, refused %d/%d; want no write and both rows refused", work.updates, len(a.Refused), len(r.Refused))
		}
		if sessions.lists != 1 {
			t.Errorf("session store listings = %d, want 1 (a hard failure ends the pass's lookups)", sessions.lists)
		}
		if !errors.Is(a.LookupErr, sessions.err) || r.LookupErr != nil {
			t.Errorf("LookupErr = %v / %v, want the failure on the assigned arm's report only", a.LookupErr, r.LookupErr)
		}
		var out bytes.Buffer
		a.log(&out, "canonicalizeLegacyBoundAssignedWork")
		if !strings.Contains(out.String(), "canonicalizeLegacyBoundAssignedWork: provenance lookup failed, so this pass refused the shared rig re-homes it could not prove: ") ||
			!strings.Contains(out.String(), "session store unreachable") {
			t.Errorf("log = %q, want the lookup failure line", out.String())
		}
	})

	t.Run("partial result with the record", func(t *testing.T) {
		mem := beads.NewMemStore()
		seedSessionRecord(t, mem, "rig-A/gc.planner")
		partial := &beads.PartialResultError{Op: "bd list", Err: errors.New("one row failed to parse")}
		provenance := newLegacyRehomeProvenance(&sessionLookupStore{Store: mem, err: partial})
		routed := rowCreatedAt(workBead("wb-r", "rig-A/gc.planner", "", "open", 2), legacyRehomeEpoch)
		work := beads.NewMemStoreFrom(0, []beads.Bead{routed}, nil)

		r := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{routed}, []beads.Store{work}, []string{"rig:rig-A"}, provenance, io.Discard)

		if got := mustGetBead(t, work, "wb-r").Metadata[beadmeta.RoutedToMetadataKey]; got != "rig-A/planner" || len(r.Rehomed) != 1 {
			t.Errorf("gc.routed_to = %q, report = %+v; want re-homed to rig-A/planner", got, r)
		}
		if !errors.Is(r.LookupErr, partial) {
			t.Errorf("LookupErr = %v, want the partial result reported", r.LookupErr)
		}
	})

	t.Run("molecule root read failure", func(t *testing.T) {
		sessions := beads.NewMemStoreFrom(0, []beads.Bead{sessionRecordAt("rec-1", "rig-A/gc.planner", false, legacyRehomeEpoch, legacyRehomeAt(time.Hour))}, nil)
		provenance := newLegacyRehomeProvenance(sessions)
		root := rowCreatedAt(beads.Bead{ID: "root-1", Title: "molecule", Type: "task", Status: "in_progress"}, legacyRehomeEpoch)
		routed := rowCreatedAt(moleculeRow(workBead("wb-r", "rig-A/gc.planner", "", "open", 2), "root-1"), legacyRehomeAt(100*time.Hour))
		unreachable := errors.New("rig store unreachable")
		work := &rootReadStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{root, routed}, nil), err: unreachable}

		r := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{routed}, []beads.Store{work}, []string{"rig:rig-A"}, provenance, io.Discard)

		if got := mustGetBead(t, work.Store, "wb-r").Metadata[beadmeta.RoutedToMetadataKey]; got != "rig-A/gc.planner" || len(r.Refused) != 1 {
			t.Errorf("gc.routed_to = %q, report = %+v; want the row refused", got, r)
		}
		if !errors.Is(r.LookupErr, unreachable) || !strings.Contains(r.LookupErr.Error(), `reading molecule root "root-1" of wb-r`) {
			t.Errorf("LookupErr = %v, want the failed root read reported", r.LookupErr)
		}
	})
}

// moleculeRow returns row as a row of the molecule rooted at rootID.
func moleculeRow(row beads.Bead, rootID string) beads.Bead {
	row.Metadata[beadmeta.RootBeadIDMetadataKey] = rootID
	return row
}

// rootReadStore records the beads a re-home pass reads by ID, in order, and
// fails each read with err when it is set.
type rootReadStore struct {
	beads.Store
	gets []string
	err  error
}

func (s *rootReadStore) Get(id string) (beads.Bead, error) {
	s.gets = append(s.gets, id)
	if s.err != nil {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

// TestCanonicalizeLegacyRehomeDatesMoleculeRowsByTheirRoot: the dispatcher
// creates a molecule's retries and ralph iterations under the target frozen
// in the molecule's recipe, so a molecule rooted before a migration gets new
// rows under the legacy spelling after it. In a city that migrated the agent
// itself, both arms date such a row by its molecule root when the row's own
// creation comes after the creation of the city's last session record of the
// legacy identity: it is re-homed when the root was created no later than
// that record (the city's own pre-migration molecule), and refused when the
// root was created after it, before the record closed included (most likely
// a neighbor's molecule), is undated or missing from the row's store, or when
// the row names no root it could read: itself, or an ID a CLI-backed store
// would read as a flag.
func TestCanonicalizeLegacyRehomeDatesMoleculeRowsByTheirRoot(t *testing.T) {
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
	// The city's last session of the legacy identity was created at hour 1
	// and closed at hour 3. The rows are created at hour 100 and name rootID;
	// the store holds the bead "root-1", created at rootCreated, when stored
	// is set.
	cases := []struct {
		name        string
		rootID      string
		rootCreated time.Time
		stored      bool
		proves      bool
		reads       []string
	}{
		{"root created before the city's last legacy session was", "root-1", legacyRehomeEpoch, true, true, []string{"root-1"}},
		{"root created as that session was", "root-1", legacyRehomeAt(time.Hour), true, true, []string{"root-1"}},
		{"root created while that session ran", "root-1", legacyRehomeAt(2 * time.Hour), true, false, []string{"root-1"}},
		{"root created after that session closed", "root-1", legacyRehomeAt(4 * time.Hour), true, false, []string{"root-1"}},
		{"undated root", "root-1", time.Time{}, true, false, []string{"root-1"}},
		{"root missing from the rows' store", "root-1", legacyRehomeEpoch, false, false, []string{"root-1"}},
		{"root ID a CLI would read as a flag", "-root-1", legacyRehomeEpoch, true, false, nil},
		{"root ID with whitespace", "root 1", legacyRehomeEpoch, true, false, nil},
		{"row that is its own root", "self", legacyRehomeEpoch, true, false, nil},
	}
	for _, fx := range fixtures {
		rig, _ := config.ParseQualifiedName(fx.legacy)
		for _, tc := range cases {
			t.Run(fx.name+"/"+tc.name, func(t *testing.T) {
				sessions := beads.NewMemStoreFrom(0, []beads.Bead{sessionRecordAt("rec-1", fx.legacy, false, legacyRehomeAt(time.Hour), legacyRehomeAt(3*time.Hour))}, nil)
				rootOf := func(id string) string {
					if tc.rootID == "self" {
						return id
					}
					return tc.rootID
				}
				created := legacyRehomeAt(100 * time.Hour)
				assigned := rowCreatedAt(moleculeRow(workBead("wb-a", fx.legacy, fx.legacy, "in_progress", 2), rootOf("wb-a")), created)
				routed := rowCreatedAt(moleculeRow(workBead("wb-r", fx.legacy, "", "open", 2), rootOf("wb-r")), created)
				seeded := []beads.Bead{assigned, routed}
				if tc.stored {
					seeded = append(seeded, rowCreatedAt(beads.Bead{ID: "root-1", Title: "molecule", Type: "task", Status: "in_progress"}, tc.rootCreated))
				}
				work := &rootReadStore{Store: beads.NewMemStoreFrom(0, seeded, nil)}
				provenance := newLegacyRehomeProvenance(sessions)

				a := canonicalizeLegacyBoundAssignedWork(fx.cfg, []beads.Bead{assigned}, []beads.Store{work}, []string{rig}, provenance, newSessionBeadSnapshot(nil), io.Discard)
				r := canonicalizeLegacyBoundUnassignedRoutedWork(fx.cfg, []beads.Bead{routed}, []beads.Store{work}, []string{"rig:" + rig}, provenance, io.Discard)

				want, wantRehomed, wantRefused := fx.legacy, 0, 1
				if tc.proves {
					want, wantRehomed, wantRefused = fx.canonical, 1, 0
				}
				gotA, gotR := mustGetBead(t, work.Store, "wb-a"), mustGetBead(t, work.Store, "wb-r")
				if gotA.Assignee != want || gotA.Metadata[beadmeta.RoutedToMetadataKey] != want || gotR.Metadata[beadmeta.RoutedToMetadataKey] != want {
					t.Errorf("assigned row %q/%q, routed row %q; want all %q",
						gotA.Assignee, gotA.Metadata[beadmeta.RoutedToMetadataKey], gotR.Metadata[beadmeta.RoutedToMetadataKey], want)
				}
				for arm, report := range map[string]legacyRehomeReport{"assigned": a, "routed": r} {
					if len(report.Rehomed) != wantRehomed || len(report.Refused) != wantRefused || report.LookupErr != nil {
						t.Errorf("%s report = %+v, want %d rehomed and %d refused", arm, report, wantRehomed, wantRefused)
					}
				}
				if !slices.Equal(work.gets, tc.reads) {
					t.Errorf("reads = %q, want %q (each root once per pass, both arms' spellings of the leg sharing it)", work.gets, tc.reads)
				}
			})
		}
	}
}

// TestLegacyRehomeProvenanceReadsEachRootOncePerPass: a molecule root is read
// only for a row whose own creation this city's dated records of the legacy
// identity leave uncovered, and at most once per leg per pass, a root of the
// same ID on another leg's store being read on its own.
func TestLegacyRehomeProvenanceReadsEachRootOncePerPass(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	const legacy = "rig-A/gc.planner"
	// The city's last session of the legacy identity was created at hour 3.
	sessions := beads.NewMemStoreFrom(0, []beads.Bead{sessionRecordAt("rec-1", legacy, false, legacyRehomeAt(3*time.Hour), legacyRehomeAt(5*time.Hour))}, nil)
	late, early := legacyRehomeAt(100*time.Hour), legacyRehomeAt(2*time.Hour)
	root := func(id string, created time.Time) beads.Bead {
		return rowCreatedAt(beads.Bead{ID: id, Title: "molecule", Type: "task", Status: "in_progress"}, created)
	}
	rowsA := []beads.Bead{
		rowCreatedAt(moleculeRow(workBead("a-1", legacy, "", "open", 2), "root-1"), late),
		rowCreatedAt(moleculeRow(workBead("a-2", legacy, "", "open", 2), "root-1"), late),
		rowCreatedAt(moleculeRow(workBead("a-3", legacy, "", "open", 2), "root-2"), late),
		// Its own creation is covered: its root is never read.
		rowCreatedAt(moleculeRow(workBead("a-4", legacy, "", "open", 2), "root-3"), early),
	}
	rowsB := []beads.Bead{rowCreatedAt(moleculeRow(workBead("b-1", legacy, "", "open", 2), "root-1"), late)}
	storeA := &rootReadStore{Store: beads.NewMemStoreFrom(0, append(slices.Clone(rowsA), root("root-1", early), root("root-2", late), root("root-3", early)), nil)}
	storeB := &rootReadStore{Store: beads.NewMemStoreFrom(0, append(slices.Clone(rowsB), root("root-1", late)), nil)}
	rows := append(slices.Clone(rowsA), rowsB...)
	stores := []beads.Store{storeA, storeA, storeA, storeA, storeB}
	refs := []string{"rig:rig-A", "rig:rig-A", "rig:rig-A", "rig:rig-A", "rig:rig-B"}

	report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, rows, stores, refs, newLegacyRehomeProvenance(sessions), io.Discard)

	if !slices.Equal(storeA.gets, []string{"root-1", "root-2"}) || !slices.Equal(storeB.gets, []string{"root-1"}) {
		t.Errorf("reads on rig-A %q, on rig-B %q; want [root-1 root-2] and [root-1]", storeA.gets, storeB.gets)
	}
	var rehomed, refused []string
	for _, row := range report.Rehomed {
		rehomed = append(rehomed, row.ID)
	}
	for _, row := range report.Refused {
		refused = append(refused, row.ID)
	}
	if !slices.Equal(rehomed, []string{"a-1", "a-2", "a-4"}) || !slices.Equal(refused, []string{"a-3", "b-1"}) {
		t.Errorf("rehomed %q, refused %q; want [a-1 a-2 a-4] and [a-3 b-1]", rehomed, refused)
	}

	t.Run("a city with no record of the identity reads no root", func(t *testing.T) {
		row := rowCreatedAt(moleculeRow(workBead("a-1", legacy, "", "open", 2), "root-1"), late)
		quiet := &rootReadStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{row, root("root-1", early)}, nil)}
		report := canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{row}, []beads.Store{quiet}, []string{"rig:rig-A"}, newLegacyRehomeProvenance(beads.NewMemStore()), io.Discard)
		if len(quiet.gets) != 0 || len(report.Refused) != 1 {
			t.Errorf("reads = %q, report = %+v; want no read and the row refused", quiet.gets, report)
		}
	})
}

// TestCanonicalizeLegacyRehomeTwoCitiesWithSessionHistory: each city's own
// session history vouches only for the identities it ran, and only for rows
// created no later than its last session record of them was. In the #5846
// topology, cities A (unbound qcore/omp) and B (bound qcore/pool.omp)
// alternate both arms over one shared rig store that holds A's queued row and
// B's in-flight row, created at one time. Nobody writes when each city ran
// only its own identity, when B ran A's identity only before the rows were
// created (R1: B migrated), or when each city ran the other's identity only
// before the rows were created (R2: cross-held records). Nor when a city kept
// the session it ran that identity in as a seat of its migrated agent and
// wrote it long after the rows were created, whether it keeps the seat open
// or closed it then, nor for later rows of molecules rooted while such a seat
// ran. The residual is a row created while both cities ran an identity. It
// moves once, to the city whose record covers it, and when each city's record
// covers it under the other's spelling it is rewritten on every pass.
func TestCanonicalizeLegacyRehomeTwoCitiesWithSessionHistory(t *testing.T) {
	// ran is a session record of template that the city created at created
	// and last wrote at lastWrite, hours after the epoch, closing it then
	// unless it is open.
	type ran struct {
		template           string
		created, lastWrite time.Duration
		open               bool
	}
	history := func(records []ran) *beads.MemStore {
		seeded := make([]beads.Bead, 0, len(records))
		for i, r := range records {
			seeded = append(seeded, sessionRecordAt(fmt.Sprintf("rec-%d", i+1), r.template, r.open, legacyRehomeAt(r.created), legacyRehomeAt(r.lastWrite)))
		}
		return beads.NewMemStoreFrom(0, seeded, nil)
	}
	// rowsAt is A's queued row and B's in-flight row, both created at created.
	rowsAt := func(created time.Duration) []beads.Bead {
		return []beads.Bead{
			rowCreatedAt(workBead("wb-a", sharedRigUnboundOmp, "", "open", 2), legacyRehomeAt(created)),
			rowCreatedAt(workBead("wb-b", sharedRigBoundOmp, sharedRigBoundOmp, "in_progress", 2), legacyRehomeAt(created)),
		}
	}
	// laterRowsOf is rowsAt(created) with each row a later row, a retry or a
	// ralph iteration, of a molecule its city rooted at rooted, the root
	// beside it in the shared store.
	laterRowsOf := func(rooted, created time.Duration) []beads.Bead {
		var seeded []beads.Bead
		for _, row := range rowsAt(created) {
			root := rowCreatedAt(beads.Bead{ID: "root-" + row.ID, Title: "molecule", Type: "task", Status: "in_progress"}, legacyRehomeAt(rooted))
			seeded = append(seeded, root, moleculeRow(row, root.ID))
		}
		return seeded
	}
	passes := func(t *testing.T, historyA, historyB []ran, seeded []beads.Bead) (*beads.MemStore, int) {
		t.Helper()
		cityA, cityB := sharedRigUnboundOmpCity(), sharedRigBoundOmpCity()
		sessionsA, sessionsB := history(historyA), history(historyB)
		mem := beads.NewMemStoreFrom(0, seeded, nil)
		shared := &updateCountingStore{Store: mem}
		for pass := 0; pass < 10; pass++ {
			cfg, sessions := cityA, sessionsA
			if pass%2 == 1 {
				cfg, sessions = cityB, sessionsB
			}
			provenance := newLegacyRehomeProvenance(sessions)
			stores := []beads.Store{shared, shared}
			rows := []beads.Bead{mustGetBead(t, mem, "wb-a"), mustGetBead(t, mem, "wb-b")}
			canonicalizeLegacyBoundAssignedWork(cfg, rows, stores, []string{"qcore", "qcore"}, provenance, newSessionBeadSnapshot(nil), io.Discard)
			rows = []beads.Bead{mustGetBead(t, mem, "wb-a"), mustGetBead(t, mem, "wb-b")}
			canonicalizeLegacyBoundUnassignedRoutedWork(cfg, rows, stores, []string{"rig:qcore", "rig:qcore"}, provenance, io.Discard)
		}
		return mem, shared.updates
	}
	expect := func(t *testing.T, mem *beads.MemStore, updates, wantUpdates int, wantA, wantB string) {
		t.Helper()
		if updates != wantUpdates {
			t.Errorf("shared rig store received %d Update calls over 10 alternating passes, want %d", updates, wantUpdates)
		}
		if a := mustGetBead(t, mem, "wb-a"); a.Assignee != "" || a.Metadata[beadmeta.RoutedToMetadataKey] != wantA {
			t.Errorf("A's row assignee/route = %q/%q, want \"\"/%q", a.Assignee, a.Metadata[beadmeta.RoutedToMetadataKey], wantA)
		}
		if b := mustGetBead(t, mem, "wb-b"); b.Assignee != wantB || b.Metadata[beadmeta.RoutedToMetadataKey] != wantB {
			t.Errorf("B's row assignee/route = %q/%q, want both %q", b.Assignee, b.Metadata[beadmeta.RoutedToMetadataKey], wantB)
		}
	}
	// Hours after the epoch. ownA and ownB ran only their current identity.
	// migratedB moved from A's identity to its own at hour 1, creating and
	// closing its last qcore/omp session then. rolledBackA took B's identity
	// in the same fleet-wide change and went back to its own at hour 2,
	// creating and closing its last qcore/pool.omp session then.
	ownA, ownB := []ran{{template: sharedRigUnboundOmp, lastWrite: 5 * time.Hour}}, []ran{{template: sharedRigBoundOmp, lastWrite: 5 * time.Hour}}
	migratedB := []ran{{template: sharedRigUnboundOmp, created: time.Hour, lastWrite: time.Hour}, {template: sharedRigBoundOmp, lastWrite: 5 * time.Hour}}
	rolledBackA := []ran{{template: sharedRigBoundOmp, created: 2 * time.Hour, lastWrite: 2 * time.Hour}, {template: sharedRigUnboundOmp, lastWrite: 5 * time.Hour}}
	// The same two changes, each city keeping the session it ran the other's
	// identity in as a seat of its migrated agent: B's qcore/omp seat was
	// created at the epoch, A's qcore/pool.omp seat at 1h30m, and each city
	// last wrote its seat at hour 101, after rows created at hour 100, running
	// its migrated identity. An open seat still runs; a closed one closed then.
	seat := func(template string, created time.Duration, open bool) ran {
		return ran{template: template, created: created, lastWrite: 101 * time.Hour, open: open}
	}
	seatedB := []ran{seat(sharedRigUnboundOmp, 0, true), {template: sharedRigBoundOmp, lastWrite: 5 * time.Hour}}
	seatedA := []ran{seat(sharedRigBoundOmp, 90*time.Minute, true), {template: sharedRigUnboundOmp, lastWrite: 5 * time.Hour}}
	closedSeatB := []ran{seat(sharedRigUnboundOmp, 0, false), {template: sharedRigBoundOmp, lastWrite: 5 * time.Hour}}
	closedSeatA := []ran{seat(sharedRigBoundOmp, 90*time.Minute, false), {template: sharedRigUnboundOmp, lastWrite: 5 * time.Hour}}

	t.Run("each city's own history", func(t *testing.T) {
		mem, updates := passes(t, ownA, ownB, rowsAt(30*time.Minute))
		expect(t, mem, updates, 0, sharedRigUnboundOmp, sharedRigBoundOmp)
	})

	t.Run("R1: rows created after the neighbor migrated stay put", func(t *testing.T) {
		mem, updates := passes(t, ownA, migratedB, rowsAt(3*time.Hour))
		expect(t, mem, updates, 0, sharedRigUnboundOmp, sharedRigBoundOmp)
	})

	t.Run("R2: cross-held records rewrite nothing created after both changes", func(t *testing.T) {
		mem, updates := passes(t, rolledBackA, migratedB, rowsAt(3*time.Hour))
		expect(t, mem, updates, 0, sharedRigUnboundOmp, sharedRigBoundOmp)
	})

	t.Run("R1: a neighbor's open seat leaves rows created after it migrated", func(t *testing.T) {
		mem, updates := passes(t, ownA, seatedB, rowsAt(100*time.Hour))
		expect(t, mem, updates, 0, sharedRigUnboundOmp, sharedRigBoundOmp)
	})

	t.Run("R2: open seats on both sides rewrite nothing created after both changes", func(t *testing.T) {
		mem, updates := passes(t, seatedA, seatedB, rowsAt(100*time.Hour))
		expect(t, mem, updates, 0, sharedRigUnboundOmp, sharedRigBoundOmp)
	})

	t.Run("R1: a neighbor's closed seat leaves rows created after it migrated", func(t *testing.T) {
		mem, updates := passes(t, ownA, closedSeatB, rowsAt(100*time.Hour))
		expect(t, mem, updates, 0, sharedRigUnboundOmp, sharedRigBoundOmp)
	})

	t.Run("R2: closed seats on both sides rewrite nothing created after both changes", func(t *testing.T) {
		mem, updates := passes(t, closedSeatA, closedSeatB, rowsAt(100*time.Hour))
		expect(t, mem, updates, 0, sharedRigUnboundOmp, sharedRigBoundOmp)
	})

	for _, tc := range []struct {
		state    string
		historyB []ran
	}{{"open", seatedB}, {"closed", closedSeatB}} {
		t.Run("lineage: a neighbor's "+tc.state+" seat leaves later rows of molecules rooted after it migrated", func(t *testing.T) {
			// A rooted its molecules at hour 50, while B's qcore/omp seat ran
			// B's migrated identity, and created their later rows at hour 500.
			mem, updates := passes(t, ownA, tc.historyB, laterRowsOf(50*time.Hour, 500*time.Hour))
			expect(t, mem, updates, 0, sharedRigUnboundOmp, sharedRigBoundOmp)
		})
	}

	t.Run("known residual: a row created before the neighbor migrated moves once", func(t *testing.T) {
		// B's last record of qcore/omp, created at hour 1, covers A's queued
		// row, which B moves; A holds no record of qcore/pool.omp and leaves
		// it there.
		mem, updates := passes(t, ownA, migratedB, rowsAt(30*time.Minute))
		expect(t, mem, updates, 1, sharedRigBoundOmp, sharedRigBoundOmp)
	})

	t.Run("known residual: a row created between the two changes moves once", func(t *testing.T) {
		// A's last record of qcore/pool.omp, created at hour 2, covers B's
		// in-flight row created at 1h30m, which A takes; B's last record of
		// qcore/omp, created at hour 1, leaves it with A.
		mem, updates := passes(t, rolledBackA, migratedB, rowsAt(90*time.Minute))
		expect(t, mem, updates, 1, sharedRigUnboundOmp, sharedRigUnboundOmp)
	})

	t.Run("known residual: a row created before both changes flips every pass", func(t *testing.T) {
		// Each city's record of the other's current identity covers both
		// rows: A's first pass moves B's row, and every pass after it moves
		// both rows back, ending on B's spelling after B's last pass.
		mem, updates := passes(t, rolledBackA, migratedB, rowsAt(30*time.Minute))
		expect(t, mem, updates, 19, sharedRigBoundOmp, sharedRigBoundOmp)
	})
}

// Read side

// TestLegacyRehomeUnproven pins which assignees the pool-demand pass leaves
// out: a legacy spelling of a local pool agent that the config does not let
// this city re-home on the row's leg.
func TestLegacyRehomeUnproven(t *testing.T) {
	cfg := legacyBoundRecoveryConfig()
	cfg.Imports = map[string]config.Import{"pool": {Source: "./packs/pool"}}
	bound := &config.City{Agents: []config.Agent{{Name: "refinery", Dir: "gascity", BindingName: "gastown", MaxActiveSessions: intPtr(1)}}}
	for _, tc := range []struct {
		name     string
		cfg      *config.City
		ref      string
		identity string
		want     bool
	}{
		{"unminted strip on a rig leg", cfg, "rig-A", "rig-A/gc.planner", true},
		{"unminted strip on a scoped rig leg", cfg, "rig:rig-A", "rig-A/gc.planner", true},
		{"add on a rig leg", bound, "gascity", "gascity/refinery", true},
		{"minted strip on a rig leg", cfg, "rig-A", "rig-A/pool.planner", false},
		{"canonical identity", cfg, "rig-A", "rig-A/planner", false},
		{"identity no agent answers to", cfg, "rig-A", "rig-B/gc.planner", false},
		{"city leg", cfg, "", "rig-A/gc.planner", false},
		{"scoped city leg", cfg, "city:city", "rig-A/gc.planner", false},
		{"class binding leg", cfg, "class:work", "rig-A/gc.planner", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyRehomeUnproven(tc.cfg, tc.ref, tc.identity); got != tc.want {
				t.Errorf("legacyRehomeUnproven(%q, %q) = %v, want %v", tc.ref, tc.identity, got, tc.want)
			}
		})
	}
}

// TestFilterAssignedWorkBeadsForPoolDemandLeavesUnprovenLegacyAssignee: work
// on a rig leg assigned under a legacy spelling this city cannot prove is its
// own is not pool demand, so no canonical session is woken for work it could
// not claim; canonical work, a minted strip and work a live session answers to
// stay demand.
func TestFilterAssignedWorkBeadsForPoolDemandLeavesUnprovenLegacyAssignee(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Imports: map[string]config.Import{"pool": {Source: "./packs/pool"}},
		Rigs:    []config.Rig{{Name: "rig-A", Path: filepath.Join(cityPath, "rig-A")}},
		Agents:  []config.Agent{poolAgent("planner", "rig-A", intPtr(5), 0), poolAgent("worker", "rig-A", intPtr(5), 0)},
	}
	// A live session still running under the legacy worker identity.
	live := beads.Bead{
		ID: "sess-legacy", Type: sessionBeadType, Status: "open",
		Metadata: map[string]string{"session_name": "rig-A/gc.worker", "template": "rig-A/gc.worker", "state": "active"},
	}
	work := []beads.Bead{
		workBead("unproven", "rig-A/gc.planner", "rig-A/gc.planner", "in_progress", 2),
		workBead("legacy-route-canonical-assignee", "rig-A/gc.planner", "rig-A/planner", "in_progress", 2),
		workBead("canonical", "rig-A/planner", "rig-A/planner", "in_progress", 2),
		workBead("minted", "rig-A/pool.planner", "rig-A/pool.planner", "in_progress", 2),
		workBead("served", "rig-A/gc.worker", "rig-A/gc.worker", "in_progress", 2),
	}
	refs := []string{"rig-A", "rig-A", "rig-A", "rig-A", "rig-A"}

	got, gotRefs := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, nil, sessionInfosFromBeads([]beads.Bead{live}), work, refs)

	var kept []string
	for _, wb := range got {
		kept = append(kept, wb.ID)
	}
	want := []string{"legacy-route-canonical-assignee", "canonical", "minted", "served"}
	if !slices.Equal(kept, want) || len(gotRefs) != len(got) {
		t.Errorf("kept %q (refs %q), want %q", kept, gotRefs, want)
	}
}

// Tick level

// legacyRehomeTickCity is one city whose rig "rig-A" runs the unbound pool
// agent rig-A/planner; its legacy bound form "rig-A/gc.planner" carries a
// binding the city no longer declares (the #3378 migration). The rig store
// holds one in-flight row assigned and routed to that legacy form, with no
// live session.
func legacyRehomeTickCity(t *testing.T) (cityPath string, cfg *config.City, cityStore, rigStore *beads.MemStore, rowID string) {
	t.Helper()
	cityPath = t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "rig-A")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg = &config.City{
		Agents:    []config.Agent{{Name: "planner", Dir: "rig-A", MaxActiveSessions: intPtr(5), Provider: "mock"}},
		Rigs:      []config.Rig{{Name: "rig-A", Path: rigPath}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
	cityStore, rigStore = beads.NewMemStore(), beads.NewMemStore()
	row := createInFlightRow(t, rigStore, goldenLegacyPlanner, goldenLegacyPlanner)
	return cityPath, cfg, cityStore, rigStore, row.ID
}

// createInFlightRow creates an in_progress work row assigned to assignee and
// routed to routedTo.
func createInFlightRow(t *testing.T, store beads.Store, routedTo, assignee string) beads.Bead {
	t.Helper()
	row, err := store.Create(beads.Bead{Title: "in-flight", Type: "task", Assignee: assignee, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: routedTo}})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(row.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	return mustGetBead(t, store, row.ID)
}

// legacyRehomeTick runs one legacy demand pass over the city and returns its
// result and what it logged.
func legacyRehomeTick(t *testing.T, cityName, cityPath string, cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store) (DesiredStateResult, string) {
	t.Helper()
	sessions, err := loadSessionBeadSnapshot(cityStore)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	var stderr bytes.Buffer
	result := buildDesiredStateWithSessionBeads(cityName, cityPath, time.Now(), cfg, &localMockProvider{}, cityStore, rigStores, sessions, nil, &stderr)
	return result, stderr.String()
}

func desiresTemplate(result DesiredStateResult, template string) bool {
	for _, tp := range result.State {
		if tp.TemplateName == template {
			return true
		}
	}
	return false
}

// TestBuildDesiredStateLegacyRehomeOnSingleCityRigLeg drives the legacy tick
// over a single city's own migrated rig work. With the city's own session
// record of the legacy identity, the row is re-homed and the next tick wakes a
// canonical session that can claim it. Without one, the row is refused and
// reported, and no session is woken for work it could not claim. A retry the
// dispatcher created under the legacy spelling frozen in a molecule rooted
// before the migration is re-homed after the city's last legacy session
// closed, dated by its root when the root is no newer than that session's
// record. A retry of a molecule rooted after the record was created is not,
// whether that session still ran or had closed.
func TestBuildDesiredStateLegacyRehomeOnSingleCityRigLeg(t *testing.T) {
	t.Run("this city's session record proves it", func(t *testing.T) {
		cityPath, cfg, _, rigStore, id := legacyRehomeTickCity(t)
		// The city's last session of the legacy identity was created, and
		// closed, as the row was: the latest a record still covers it.
		created := mustGetBead(t, rigStore, id).CreatedAt
		cityStore := beads.NewMemStoreFrom(0, []beads.Bead{sessionRecordAt("rec-1", goldenLegacyPlanner, false, created, created)}, nil)
		rigStores := map[string]beads.Store{"rig-A": rigStore}

		_, logged := legacyRehomeTick(t, "test-city", cityPath, cfg, cityStore, rigStores)

		got := mustGetBead(t, rigStore, id)
		if got.Assignee != goldenCanonicalPlanner || got.Metadata[beadmeta.RoutedToMetadataKey] != goldenCanonicalPlanner {
			t.Fatalf("assignee/route = %q/%q, want both %q", got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey], goldenCanonicalPlanner)
		}
		if !strings.Contains(logged, "canonicalizeLegacyBoundAssignedWork: rehomed "+id+" on rig:rig-A: ") {
			t.Errorf("first tick logged %q, want the rehomed line", logged)
		}
		result, _ := legacyRehomeTick(t, "test-city", cityPath, cfg, cityStore, rigStores)
		if !desiresTemplate(result, goldenCanonicalPlanner) {
			t.Errorf("second tick desires no %s session for its re-homed in-flight work", goldenCanonicalPlanner)
		}
	})

	for _, tc := range []struct {
		name    string
		root    time.Time
		rehomes bool
	}{
		{"a retry of a molecule rooted before the last legacy session was created", legacyRehomeEpoch, true},
		{"a retry of a molecule rooted while the last legacy session ran", legacyRehomeAt(2 * time.Hour), false},
		{"a retry of a molecule rooted after the last legacy session closed", legacyRehomeAt(4 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg, _, _, _ := legacyRehomeTickCity(t)
			// The city's last session of the legacy identity was created an
			// hour after the epoch and closed at hour 3. The retry is created
			// now, as the dispatcher routes it: the frozen legacy target on
			// both route keys, no assignee, in the molecule root's store.
			cityStore := beads.NewMemStoreFrom(0, []beads.Bead{sessionRecordAt("rec-1", goldenLegacyPlanner, false, legacyRehomeAt(time.Hour), legacyRehomeAt(3*time.Hour))}, nil)
			root := rowCreatedAt(beads.Bead{ID: "rig-root", Title: "molecule", Type: "task", Status: "in_progress"}, tc.root)
			rigStore := beads.NewMemStoreFrom(0, []beads.Bead{root}, nil)
			retry, err := rigStore.Create(beads.Bead{Title: "retry", Type: "task", Metadata: map[string]string{
				beadmeta.RoutedToMetadataKey:          goldenLegacyPlanner,
				beadmeta.ExecutionRoutedToMetadataKey: goldenLegacyPlanner,
				beadmeta.RootBeadIDMetadataKey:        root.ID,
			}})
			if err != nil {
				t.Fatal(err)
			}
			rigStores := map[string]beads.Store{"rig-A": rigStore}

			_, logged := legacyRehomeTick(t, "test-city", cityPath, cfg, cityStore, rigStores)

			want := goldenLegacyPlanner
			if tc.rehomes {
				want = goldenCanonicalPlanner
			}
			if got := mustGetBead(t, rigStore, retry.ID).Metadata[beadmeta.RoutedToMetadataKey]; got != want {
				t.Fatalf("retry gc.routed_to = %q, want %q", got, want)
			}
			wantLog := "canonicalizeLegacyBoundUnassignedRoutedWork: refused 1 legacy re-home(s) on shared rig legs this pass (1 rows): "
			if tc.rehomes {
				wantLog = "canonicalizeLegacyBoundUnassignedRoutedWork: rehomed " + retry.ID + " on rig:rig-A: "
			}
			if !strings.Contains(logged, wantLog) {
				t.Errorf("tick logged %q, want %q", logged, wantLog)
			}
		})
	}

	t.Run("no session record", func(t *testing.T) {
		cityPath, cfg, cityStore, rigStore, id := legacyRehomeTickCity(t)
		rigStores := map[string]beads.Store{"rig-A": rigStore}
		for pass := 1; pass <= 2; pass++ {
			result, logged := legacyRehomeTick(t, "test-city", cityPath, cfg, cityStore, rigStores)
			if desiresTemplate(result, goldenCanonicalPlanner) {
				t.Errorf("pass %d desires a %s session for work it cannot claim", pass, goldenCanonicalPlanner)
			}
			want := `canonicalizeLegacyBoundAssignedWork: refused 1 legacy re-home(s) on shared rig legs this pass (1 rows): "` +
				goldenLegacyPlanner + `" -> "` + goldenCanonicalPlanner + `" (1: ` + id + `)`
			if !strings.Contains(logged, want) {
				t.Errorf("pass %d logged %q, want %q", pass, logged, want)
			}
		}
		got := mustGetBead(t, rigStore, id)
		if got.Assignee != goldenLegacyPlanner || got.Metadata[beadmeta.RoutedToMetadataKey] != goldenLegacyPlanner {
			t.Errorf("assignee/route = %q/%q, want both left %q", got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey], goldenLegacyPlanner)
		}
	})
}

// sharedRigTickCity is one city of the #5846 topology for a legacy demand pass:
// cfg's one agent runs on rig "qcore", and the city's own session store holds
// its history, if any, and the record of a session it ran under its current
// identity.
type sharedRigTickCity struct {
	name     string
	cfg      *config.City
	path     string
	sessions *beads.MemStore
	template string
}

func newSharedRigTickCity(t *testing.T, name string, cfg *config.City, history ...beads.Bead) sharedRigTickCity {
	t.Helper()
	path := t.TempDir()
	rigPath := filepath.Join(path, "rigs", "qcore")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Rigs = []config.Rig{{Name: "qcore", Path: rigPath}}
	cfg.Agents[0].Provider = "mock"
	cfg.Providers = map[string]config.ProviderSpec{"mock": {Command: "true"}}
	c := sharedRigTickCity{name: name, cfg: cfg, path: path, sessions: beads.NewMemStoreFrom(0, history, nil), template: cfg.Agents[0].QualifiedName()}
	seedSessionRecord(t, c.sessions, c.template)
	return c
}

// seedCityWork writes the city's in-flight and queued rows into the shared
// store and returns them by ID.
func (c sharedRigTickCity) seedCityWork(t *testing.T, shared beads.Store) map[string]beads.Bead {
	t.Helper()
	inFlight := createInFlightRow(t, shared, c.template, c.template)
	queued, err := shared.Create(beads.Bead{Title: "queued", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: c.template}})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]beads.Bead{inFlight.ID: inFlight, queued.ID: queued}
}

func (c sharedRigTickCity) tick(t *testing.T, shared beads.Store) (DesiredStateResult, string) {
	t.Helper()
	return legacyRehomeTick(t, c.name, c.path, c.cfg, c.sessions, map[string]beads.Store{"qcore": shared})
}

func assertRowsUnchanged(t *testing.T, store beads.Store, rows map[string]beads.Bead) {
	t.Helper()
	for id, want := range rows {
		got := mustGetBead(t, store, id)
		if got.Assignee != want.Assignee || got.Metadata[beadmeta.RoutedToMetadataKey] != want.Metadata[beadmeta.RoutedToMetadataKey] {
			t.Errorf("row %s assignee/route = %q/%q, want %q/%q", id, got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey], want.Assignee, want.Metadata[beadmeta.RoutedToMetadataKey])
		}
	}
}

// TestBuildDesiredStateLegacyRehomeLeavesNeighborRowsOnSharedRig is AC-6 at
// the tick: cities A (unbound qcore/omp) and B (bound qcore/pool.omp), each
// with its own session history, run legacy demand passes over one shared rig
// store. Alternating over both cities' in-flight and queued rows, nobody
// writes and each pass logs one refusal summary per arm; and a pass over only
// its neighbor's rows wakes no session for them, even in a city whose own
// records show it running its neighbor's identity before the rows were
// created (R1), also from a seat it kept after it migrated, open and revived
// for min_active_sessions or closed after the rows were created.
func TestBuildDesiredStateLegacyRehomeLeavesNeighborRowsOnSharedRig(t *testing.T) {
	t.Run("alternating passes write nothing", func(t *testing.T) {
		cityA := newSharedRigTickCity(t, "city-a", sharedRigUnboundOmpCity())
		cityB := newSharedRigTickCity(t, "city-b", sharedRigBoundOmpCity())
		mem := beads.NewMemStore()
		rows := cityA.seedCityWork(t, mem)
		for id, row := range cityB.seedCityWork(t, mem) {
			rows[id] = row
		}
		shared := &updateCountingStore{Store: mem}

		for pass := 0; pass < 4; pass++ {
			c := cityA
			if pass%2 == 1 {
				c = cityB
			}
			_, logged := c.tick(t, shared)
			for _, arm := range []string{"canonicalizeLegacyBoundAssignedWork", "canonicalizeLegacyBoundUnassignedRoutedWork"} {
				if n := strings.Count(logged, arm+": refused 1 legacy re-home(s)"); n != 1 {
					t.Errorf("pass %d (%s) logged %d refusal summaries for %s, want 1:\n%s", pass, c.name, n, arm, logged)
				}
			}
		}
		if shared.updates != 0 {
			t.Errorf("shared rig store received %d Update calls over 4 alternating passes, want 0", shared.updates)
		}
		assertRowsUnchanged(t, mem, rows)
	})

	t.Run("a pass wakes nothing for its neighbor's rows", func(t *testing.T) {
		cityA := newSharedRigTickCity(t, "city-a", sharedRigUnboundOmpCity())
		cityB := newSharedRigTickCity(t, "city-b", sharedRigBoundOmpCity())
		for _, pair := range [][2]sharedRigTickCity{{cityA, cityB}, {cityB, cityA}} {
			c, neighbor := pair[0], pair[1]
			mem := beads.NewMemStore()
			rows := neighbor.seedCityWork(t, mem)
			shared := &updateCountingStore{Store: mem}

			result, _ := c.tick(t, shared)

			if desiresTemplate(result, c.template) {
				t.Errorf("%s desires a %s session for %s's work", c.name, c.template, neighbor.name)
			}
			if shared.updates != 0 {
				t.Errorf("%s wrote %d times to %s's rows, want 0", c.name, shared.updates, neighbor.name)
			}
			assertRowsUnchanged(t, mem, rows)
		}
	})

	t.Run("a city that ran its neighbor's identity before the rows existed leaves them", func(t *testing.T) {
		// R1: B migrated from A's identity, its last qcore/omp session
		// created at the epoch and closed an hour later, before A created its
		// rows.
		cityA := newSharedRigTickCity(t, "city-a", sharedRigUnboundOmpCity())
		cityB := newSharedRigTickCity(t, "city-b", sharedRigBoundOmpCity(),
			sessionRecordAt("rec-omp", sharedRigUnboundOmp, false, legacyRehomeEpoch, legacyRehomeAt(time.Hour)))
		mem := beads.NewMemStore()
		rows := cityA.seedCityWork(t, mem)
		shared := &updateCountingStore{Store: mem}

		for pass := 1; pass <= 2; pass++ {
			result, logged := cityB.tick(t, shared)
			if desiresTemplate(result, cityB.template) {
				t.Errorf("pass %d desires a %s session for %s's work", pass, cityB.template, cityA.name)
			}
			for _, arm := range []string{"canonicalizeLegacyBoundAssignedWork", "canonicalizeLegacyBoundUnassignedRoutedWork"} {
				if n := strings.Count(logged, arm+": refused 1 legacy re-home(s)"); n != 1 {
					t.Errorf("pass %d logged %d refusal summaries for %s, want 1:\n%s", pass, n, arm, logged)
				}
			}
		}
		if shared.updates != 0 {
			t.Errorf("%s wrote %d times to %s's rows, want 0", cityB.name, shared.updates, cityA.name)
		}
		assertRowsUnchanged(t, mem, rows)
	})
}

// desiredSessionsOf returns the sorted names of the sessions result desires
// under template.
func desiredSessionsOf(result DesiredStateResult, template string) []string {
	var names []string
	for name, tp := range result.State {
		if tp.TemplateName == template {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// TestBuildDesiredStateLegacyRehomeLeavesRowsAfterANeighborsOpenSeat: city B
// migrated from A's identity qcore/omp and kept the session it ran under it
// open, as a seat of its migrated agent that min_active_sessions revives. The
// seat's record keeps template qcore/omp, and B writes it while it runs
// qcore/pool.omp, so it was last written after A created its rows. B's passes
// leave A's rows, which were created after the seat was, and A's rows add no
// session to what B desires without them: the revived seat and what its
// min_active_sessions fills.
func TestBuildDesiredStateLegacyRehomeLeavesRowsAfterANeighborsOpenSeat(t *testing.T) {
	cityB := func(t *testing.T) sharedRigTickCity {
		seat := sessionRecordAt("seat-omp", sharedRigUnboundOmp, true, legacyRehomeEpoch, time.Now().Add(time.Hour))
		seat.Metadata["state"] = "asleep"
		seat.Metadata["sleep_reason"] = "city-stop"
		seat.Metadata["session_name"] = "qcore--omp-legacy"
		cfg := sharedRigBoundOmpCity()
		cfg.Agents[0].MinActiveSessions = intPtr(1)
		return newSharedRigTickCity(t, "city-b", cfg, seat)
	}
	alone := cityB(t)
	baseline, _ := alone.tick(t, beads.NewMemStore())
	want := desiredSessionsOf(baseline, alone.template)
	if !slices.Contains(want, "qcore--omp-legacy") {
		t.Fatalf("B alone desires %s sessions %q, want the revived seat qcore--omp-legacy among them", alone.template, want)
	}

	b := cityB(t)
	cityA := newSharedRigTickCity(t, "city-a", sharedRigUnboundOmpCity())
	mem := beads.NewMemStore()
	rows := cityA.seedCityWork(t, mem)
	shared := &updateCountingStore{Store: mem}
	for pass := 1; pass <= 2; pass++ {
		result, logged := b.tick(t, shared)
		if got := desiredSessionsOf(result, b.template); !slices.Equal(got, want) {
			t.Errorf("pass %d desires %s sessions %q, want %q as without %s's rows", pass, b.template, got, want, cityA.name)
		}
		for _, arm := range []string{"canonicalizeLegacyBoundAssignedWork", "canonicalizeLegacyBoundUnassignedRoutedWork"} {
			if n := strings.Count(logged, arm+": refused 1 legacy re-home(s)"); n != 1 {
				t.Errorf("pass %d logged %d refusal summaries for %s, want 1:\n%s", pass, n, arm, logged)
			}
		}
	}
	if shared.updates != 0 {
		t.Errorf("%s wrote %d times to %s's rows, want 0", b.name, shared.updates, cityA.name)
	}
	assertRowsUnchanged(t, mem, rows)
}

// TestBuildDesiredStateLegacyRehomeLeavesRowsAfterANeighborsClosedSeat: city B
// migrated from A's identity qcore/omp, kept the session it ran under it as a
// seat of its migrated agent, and closed the seat after A created its rows,
// while running qcore/pool.omp. The closed record keeps template qcore/omp and
// was last written at its close. B's passes leave A's in-flight and queued
// rows, which were created after the seat was, and wake no session for them.
func TestBuildDesiredStateLegacyRehomeLeavesRowsAfterANeighborsClosedSeat(t *testing.T) {
	seat := sessionRecordAt("seat-omp", sharedRigUnboundOmp, false, legacyRehomeEpoch, time.Now().Add(time.Hour))
	cityB := newSharedRigTickCity(t, "city-b", sharedRigBoundOmpCity(), seat)
	cityA := newSharedRigTickCity(t, "city-a", sharedRigUnboundOmpCity())
	mem := beads.NewMemStore()
	rows := cityA.seedCityWork(t, mem)
	shared := &updateCountingStore{Store: mem}

	for pass := 1; pass <= 2; pass++ {
		result, logged := cityB.tick(t, shared)
		if got := desiredSessionsOf(result, cityB.template); len(got) != 0 {
			t.Errorf("pass %d desires %s sessions %q for %s's work, want none", pass, cityB.template, got, cityA.name)
		}
		for _, arm := range []string{"canonicalizeLegacyBoundAssignedWork", "canonicalizeLegacyBoundUnassignedRoutedWork"} {
			if n := strings.Count(logged, arm+": refused 1 legacy re-home(s)"); n != 1 {
				t.Errorf("pass %d logged %d refusal summaries for %s, want 1:\n%s", pass, n, arm, logged)
			}
		}
	}
	if shared.updates != 0 {
		t.Errorf("%s wrote %d times to %s's rows, want 0", cityB.name, shared.updates, cityA.name)
	}
	assertRowsUnchanged(t, mem, rows)
}

// TestBackstopDemandRepairsRehomesRigRowsTheCityProvablyRan: the v2 backstop
// reads its session records from the store it leads with, so a session record
// there of the legacy identity re-homes the rig rows the config alone refuses.
func TestBackstopDemandRepairsRehomesRigRowsTheCityProvablyRan(t *testing.T) {
	const legacy = "rig-A/pool.planner"
	env, log := sharedRigBackstopEnv(t, nil, []beads.Bead{
		rowCreatedAt(workBead("fx-a1", legacy, legacy, "in_progress", 5), legacyRehomeEpoch),
		rowCreatedAt(workBead("fx-r1", legacy, "", "open", 5), legacyRehomeEpoch),
	})
	seedSessionRecord(t, env.CityStore, legacy)
	var stderr bytes.Buffer

	runBackstopDemandRepairs(context.Background(), env, &stderr)

	want := []string{
		"Update fx-a1 assignee=" + goldenCanonicalPlanner + " " + beadmeta.RoutedToMetadataKey + "=" + goldenCanonicalPlanner,
		"Update fx-r1 assignee=- " + beadmeta.RoutedToMetadataKey + "=" + goldenCanonicalPlanner,
	}
	var updates []string
	for _, op := range log.ops {
		if strings.HasPrefix(op, "Update fx-") {
			updates = append(updates, op)
		}
	}
	if !slices.Equal(updates, want) {
		t.Errorf("rig writes\n got  %q\n want %q", updates, want)
	}
	if strings.Contains(stderr.String(), "refused") {
		t.Errorf("stderr = %q, want no refusal", stderr.String())
	}
}

// TestDoctorV2DemandMigrationsRehomesRigRowsTheCityProvablyRan: the doctor
// sweep reads its session records from the city's session store, so a rig row
// under an identity this city ran counts as a repair on its rig leg and --fix
// re-homes it.
func TestDoctorV2DemandMigrationsRehomesRigRowsTheCityProvablyRan(t *testing.T) {
	const legacy = "rig-A/pool.planner"
	f := newRepairGoldenFixture(t)
	cfg := f.env.Cfg
	cfg.Daemon.SessionReconciler = config.SessionReconcilerV2
	city := beads.NewMemStore()
	seedSessionRecord(t, city, legacy)
	rig := beads.NewMemStoreFrom(0, []beads.Bead{rowCreatedAt(workBead("fx-ours", legacy, "", "open", 5), legacyRehomeEpoch)}, nil)
	c := demandMigrationsCheckFor(t, cfg, f.env.CityPath, city, map[string]beads.Store{"fixture": rig, "nodisp": beads.NewMemStore()})
	ctx := &doctor.CheckContext{CityPath: f.env.CityPath}

	sweep := c.sweep(false, io.Discard)
	wantCounts := map[demandMigrationKey]int{{"canonicalizeLegacyBoundUnassignedRoutedWork", "rig:fixture"}: 1}
	if !reflect.DeepEqual(sweep.counts, wantCounts) || len(sweep.problems) != 0 || len(sweep.refusals) != 0 {
		t.Errorf("dry-run sweep counts = %v problems = %q refusals = %q, want %v and nothing else", sweep.counts, sweep.problems, sweep.refusals, wantCounts)
	}

	var out bytes.Buffer
	ctx.Output = &out
	if err := c.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got := mustGetBead(t, rig, "fx-ours").Metadata[beadmeta.RoutedToMetadataKey]; got != goldenCanonicalPlanner {
		t.Errorf("rig row gc.routed_to = %q, want %q", got, goldenCanonicalPlanner)
	}
	if !strings.Contains(out.String(), "canonicalizeLegacyBoundUnassignedRoutedWork: rehomed fx-ours on rig:fixture: ") {
		t.Errorf("--fix output = %q, want the rehomed line", out.String())
	}
}

// TestDoctorV2DemandMigrationsDatesAMoleculeRowByItsRoot: a rig row the city's
// own records cover only through its molecule root (a retry created after the
// city's last legacy session closed, of a molecule rooted as that session was
// created) counts as a repair in a dry run, whose stand-in store reads the
// root from the rig store, and --fix re-homes it.
func TestDoctorV2DemandMigrationsDatesAMoleculeRowByItsRoot(t *testing.T) {
	const legacy = "rig-A/pool.planner"
	f := newRepairGoldenFixture(t)
	cfg := f.env.Cfg
	cfg.Daemon.SessionReconciler = config.SessionReconcilerV2
	city := beads.NewMemStoreFrom(0, []beads.Bead{sessionRecordAt("rec-1", legacy, false, legacyRehomeEpoch, legacyRehomeAt(time.Hour))}, nil)
	root := rowCreatedAt(beads.Bead{ID: "fx-root", Title: "molecule", Type: "task", Status: "in_progress"}, legacyRehomeEpoch)
	retry := rowCreatedAt(moleculeRow(workBead("fx-retry", legacy, "", "open", 5), root.ID), legacyRehomeAt(100*time.Hour))
	rig := beads.NewMemStoreFrom(0, []beads.Bead{root, retry}, nil)
	c := demandMigrationsCheckFor(t, cfg, f.env.CityPath, city, map[string]beads.Store{"fixture": rig, "nodisp": beads.NewMemStore()})
	ctx := &doctor.CheckContext{CityPath: f.env.CityPath}

	sweep := c.sweep(false, io.Discard)
	wantCounts := map[demandMigrationKey]int{{"canonicalizeLegacyBoundUnassignedRoutedWork", "rig:fixture"}: 1}
	if !reflect.DeepEqual(sweep.counts, wantCounts) || len(sweep.problems) != 0 || len(sweep.refusals) != 0 {
		t.Errorf("dry-run sweep counts = %v problems = %q refusals = %q, want %v and nothing else", sweep.counts, sweep.problems, sweep.refusals, wantCounts)
	}
	if got := mustGetBead(t, rig, "fx-retry").Metadata[beadmeta.RoutedToMetadataKey]; got != legacy {
		t.Errorf("dry run wrote gc.routed_to = %q, want it left %q", got, legacy)
	}

	var out bytes.Buffer
	ctx.Output = &out
	if err := c.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got := mustGetBead(t, rig, "fx-retry").Metadata[beadmeta.RoutedToMetadataKey]; got != goldenCanonicalPlanner {
		t.Errorf("rig row gc.routed_to = %q, want %q", got, goldenCanonicalPlanner)
	}
	if !strings.Contains(out.String(), "canonicalizeLegacyBoundUnassignedRoutedWork: rehomed fx-retry on rig:fixture: ") {
		t.Errorf("--fix output = %q, want the rehomed line", out.String())
	}
}
