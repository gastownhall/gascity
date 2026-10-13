package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// foreignIdentityTestCity mirrors the config shapes this city actually runs:
// a rig-scoped pool agent with a namepool, addressed in persisted assignees
// both bare ("repo/worker") and in the legacy bound form ("repo/pack.worker")
// that outlived its binding.
//
// The third agent is what makes "pack" a binding THIS city mints (ga-8yi7ne).
// It is not decoration: the legacy bound forms above only resolve because some
// agent from that import is still bound, which is exactly the field condition —
// one agent moved bound→unbound while its siblings did not. Without it the
// fixture would describe a city that had removed the import entirely, and the
// bound forms SHOULD be unresolvable there.
func foreignIdentityTestCity(t *testing.T) *config.City {
	t.Helper()
	return &config.City{
		Rigs: []config.Rig{{Name: "repo", Path: t.TempDir()}},
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			{
				Name:              "worker",
				Dir:               "repo",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(2),
				NamepoolNames:     []string{"furiosa", "nux"},
			},
			{Name: "sibling", Dir: "repo", BindingName: "pack"},
		},
	}
}

func seedForeignIdentityWork(t *testing.T, store beads.Store, title, assignee string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    title,
		Assignee: assignee,
		Metadata: map[string]string{"gc.routed_to": "repo/worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead %q: %v", title, err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status for %q: %v", title, err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead %q: %v", title, err)
	}
	return work
}

func captureSweepLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})
	return &buf
}

// TestReleaseOrphanedPoolAssignments_ProtectsForeignIdentityAndStillReleasesLocal
// is the non-vacuity test for the foreign-identity gate: ONE sweep carries a
// foreign claim and a locally-configured claim whose session is genuinely gone,
// and asserts the foreign one survives while the local one is still reclaimed.
//
// Split across two sweeps these assertions are individually satisfiable by a
// sweeper that has been DISABLED (protect everything) or one that was never
// changed (release everything). Together in one sweep they are not.
func TestReleaseOrphanedPoolAssignments_ProtectsForeignIdentityAndStillReleasesLocal(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	foreign := seedForeignIdentityWork(t, rigStore, "P0 claimed by another city's crew", "repo/dalinar")
	local := seedForeignIdentityWork(t, rigStore, "work held by a dead local pool worker", "repo/worker-1")

	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		foreignIdentityTestCity(t),
		cityPath,
		nil,
		[]beads.Bead{foreign, local},
		[]beads.Store{rigStore, rigStore},
		[]string{"repo", "repo"},
		map[string]beads.Store{"repo": rigStore},
	)

	if len(released) != 1 || released[0].ID != local.ID {
		t.Fatalf("released = %v, want exactly [%s] (the locally-configured dead holder)", released, local.ID)
	}

	gotForeign, err := rigStore.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get foreign work bead: %v", err)
	}
	if gotForeign.Status != "in_progress" || gotForeign.Assignee != "repo/dalinar" {
		t.Fatalf("foreign claim = status %q assignee %q, want in_progress/repo/dalinar untouched", gotForeign.Status, gotForeign.Assignee)
	}

	gotLocal, err := rigStore.Get(local.ID)
	if err != nil {
		t.Fatalf("Get local work bead: %v", err)
	}
	if gotLocal.Status != "open" || gotLocal.Assignee != "" {
		t.Fatalf("local claim = status %q assignee %q, want open/unassigned", gotLocal.Status, gotLocal.Assignee)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "protected 1 foreign/unknown identities this pass (1 claims)") {
		t.Fatalf("missing protected-count summary in log:\n%s", logged)
	}
	if !strings.Contains(logged, `"repo/dalinar" (1: `+foreign.ID+")") {
		t.Fatalf("summary does not name the protected identity and its claim:\n%s", logged)
	}
	if strings.Contains(logged, "repo/worker-1") {
		t.Fatalf("released local holder must not appear as protected:\n%s", logged)
	}
}

// TestReleaseOrphanedPoolAssignments_ForeignIdentityBatchLeavesNoRelease is the
// 2026-08-21 regression: one sweep cleared fifteen foreign claims in 34s. The
// batch must now produce zero releases and exactly one summary line naming
// every identity with its claim count, so an accumulating leak is legible
// without knowing to look for it.
func TestReleaseOrphanedPoolAssignments_ForeignIdentityBatchLeavesNoRelease(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	var work []beads.Bead
	var stores []beads.Store
	var refs []string
	for _, assignee := range []string{"repo/dalinar", "repo/pattern", "repo/szeth", "repo/dalinar", "repo/dalinar"} {
		work = append(work, seedForeignIdentityWork(t, rigStore, "foreign claim "+assignee, assignee))
		stores = append(stores, rigStore)
		refs = append(refs, "repo")
	}

	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore, foreignIdentityTestCity(t), cityPath, nil, work, stores, refs,
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none: every claim in this batch is foreign", released)
	}
	for _, wb := range work {
		got, err := rigStore.Get(wb.ID)
		if err != nil {
			t.Fatalf("Get %s: %v", wb.ID, err)
		}
		if got.Status != "in_progress" || got.Assignee == "" {
			t.Fatalf("%s = status %q assignee %q, want the claim intact", wb.ID, got.Status, got.Assignee)
		}
	}

	logged := logBuf.String()
	if got := strings.Count(logged, "protected "); got != 1 {
		t.Fatalf("want exactly one per-sweep summary line, got %d:\n%s", got, logged)
	}
	if !strings.Contains(logged, "protected 3 foreign/unknown identities this pass (5 claims)") {
		t.Fatalf("summary must count identities and claims separately:\n%s", logged)
	}
	for _, want := range []string{`"repo/dalinar" (3: `, `"repo/pattern" (1: `, `"repo/szeth" (1: `} {
		if !strings.Contains(logged, want) {
			t.Fatalf("summary missing %q:\n%s", want, logged)
		}
	}
}

// TestReleaseOrphanedPoolAssignments_NoForeignClaimsLogsNothing keeps the
// summary a signal rather than per-tick noise: with a 30s patrol, an
// unconditional line would be 2,880 log entries a day saying nothing.
func TestReleaseOrphanedPoolAssignments_NoForeignClaimsLogsNothing(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	local := seedForeignIdentityWork(t, rigStore, "dead local holder", "repo/worker-1")

	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore, foreignIdentityTestCity(t), cityPath, nil,
		[]beads.Bead{local}, []beads.Store{rigStore}, []string{"repo"},
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 1 {
		t.Fatalf("released = %v, want the local dead holder reclaimed", released)
	}
	if strings.Contains(logBuf.String(), "foreign/unknown identities") {
		t.Fatalf("no claim was protected, so no summary should be emitted:\n%s", logBuf.String())
	}
}

// TestPoolAssigneeIsLocallyObservable is the roster matrix. Every "observable"
// row is an assignee shape this city has ACTUALLY reaped (measured from 2,874
// bead.dead_assignee_reopened events in its own event log), so a regression
// here is a sweeper that stopped reclaiming real work.
func TestPoolAssigneeIsLocallyObservable(t *testing.T) {
	cfg := foreignIdentityTestCity(t)
	cfg.NamedSessions = []config.NamedSession{
		{Name: "lana", Template: "worker", Dir: "repo"},
	}
	for _, tc := range []struct {
		name       string
		assignee   string
		observable bool
	}{
		{"configured agent", "repo/worker", true},
		{"legacy bound form of an unbound agent", "repo/pack.worker", true},
		{"numeric pool instance", "repo/worker-1", true},
		{"numeric pool instance past the configured ceiling", "repo/worker-36", true},
		{"legacy bound numeric pool instance", "repo/pack.worker-9", true},
		{"namepool-themed instance", "repo/furiosa", true},
		// The qualified adhoc spelling a pool instance's GC_AGENT / GC_ALIAS may
		// carry (cmd_hook.go's resolveAgentIdentity retry); GenerateAdhocIdentity's
		// own sanitized output ("repo--worker-adhoc-…") is a bare name and never
		// reaches the roster. Unrecognized, a claim under this spelling would be
		// protected forever the moment that session died — a LOCAL leak.
		{"adhoc pool instance", "repo/worker-adhoc-a1b2c3d4e5", true},
		{"sanitized adhoc identity is a bare name", "repo--worker-adhoc-a1b2c3d4e5", true},
		{"legacy bound adhoc pool instance", "repo/pack.worker-adhoc-a1b2c3d4e5", true},
		// Only the digits poolInstanceName actually emits count as a slot;
		// strconv.Atoi would also take these, which no generator produces.
		{"non-minted slot suffix (signed)", "repo/worker-+1", false},
		{"non-minted slot suffix (zero padded)", "repo/worker-007", false},
		{"legacy bound themed instance", "repo/pack.nux", true},
		{"configured named session", "repo/lana", true},
		{"city-level agent", "worker", true},
		{"bare session name", "worker-live", true},
		{"runtime session name", "gastown__dog-ga-up143", true},
		{"session bead id form", "claude-mc-xyz", true},
		{"empty", "", true},
		// ga-8yi7ne: a NEIGHBORING city's canonical "<rig>/<binding>.<name>"
		// whose binding this city does not mint. "pool" is not one of our
		// imports; "worker" is our agent. Before the binding gate, every one of
		// these resolved to our local worker and their live claims were
		// released — measured in the shared store, where the bead oscillated
		// between released and re-claimed twice.
		{"foreign binding, plain", "repo/pool.worker", false},
		{"foreign binding, numeric instance", "repo/pool.worker-1", false},
		{"foreign binding, adhoc instance", "repo/pool.worker-adhoc-a1b2c3d4e5", false},
		{"foreign binding, themed instance", "repo/pool.nux", false},
		{"foreign named crew", "repo/dalinar", false},
		{"foreign named crew in an unknown rig", "westeros/szeth", false},
		{"decommissioned local agent", "repo/retired-hand", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolAssigneeIsLocallyObservable(cfg, "testcity", tc.assignee); got != tc.observable {
				t.Fatalf("poolAssigneeIsLocallyObservable(%q) = %v, want %v", tc.assignee, got, tc.observable)
			}
		})
	}
}

// TestPoolAssigneeIsLocallyObservable_GenericRigScopedTemplate covers the
// template shape that names no rig at all: a Scope="rig", Dir="" agent applies
// to every configured rig, so its pool instances are addressed under a concrete
// rig the template never mentions. Comparing against the unsynthesized agent
// reads every one of those legitimate local holders as foreign.
func TestPoolAssigneeIsLocallyObservable_GenericRigScopedTemplate(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "repo", Path: t.TempDir()}},
		Agents: []config.Agent{{
			Name:              "polecat",
			Scope:             "rig",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			NamepoolNames:     []string{"furiosa"},
		}},
	}
	for _, tc := range []struct {
		assignee   string
		observable bool
	}{
		{"repo/polecat", true},
		{"repo/furiosa", true},
		{"repo/polecat-3", true},
		{"repo/polecat-adhoc-a1b2c3d4e5", true},
		{"repo/dalinar", false},
		{"otherrig/furiosa", false},
	} {
		t.Run(tc.assignee, func(t *testing.T) {
			if got := poolAssigneeIsLocallyObservable(cfg, "testcity", tc.assignee); got != tc.observable {
				t.Fatalf("poolAssigneeIsLocallyObservable(%q) = %v, want %v", tc.assignee, got, tc.observable)
			}
		})
	}
}

// TestProtectedForeignAssigneesSummary pins the rendering: exact counts, sorted
// identities, and a bounded ID sample so one leaking identity cannot make the
// line unreadable.
func TestProtectedForeignAssigneesSummary(t *testing.T) {
	var empty protectedForeignAssignees
	if got := empty.summary(); got != "" {
		t.Fatalf("empty summary = %q, want empty", got)
	}

	var p protectedForeignAssignees
	p.add("repo/zeta", "qc-2")
	p.add("repo/alpha", "qc-1")
	p.add("repo/zeta", "qc-3")
	p.add("", "qc-ignored")
	got := p.summary()
	want := `releaseOrphanedPoolAssignments: protected 2 foreign/unknown identities this pass (3 claims): "repo/alpha" (1: qc-1), "repo/zeta" (2: qc-2, qc-3)`
	if got != want {
		t.Fatalf("summary =\n  %q\nwant\n  %q", got, want)
	}

	var forged protectedForeignAssignees
	forged.add("repo/evil, \"repo/innocent\" (99: qc-x)", "qc-9")
	if got := forged.summary(); strings.Count(got, "protected ") != 1 || !strings.Contains(got, `\"repo/innocent\`) {
		t.Fatalf("an identity containing summary punctuation must be quoted, not spliced: %q", got)
	}

	var many protectedForeignAssignees
	for _, id := range []string{"qc-1", "qc-2", "qc-3", "qc-4", "qc-5", "qc-6", "qc-7"} {
		many.add("repo/leak", id)
	}
	gotMany := many.summary()
	if !strings.Contains(gotMany, `"repo/leak" (7: qc-1, qc-2, qc-3, qc-4, qc-5 +2 more)`) {
		t.Fatalf("bounded sample not rendered: %q", gotMany)
	}
}

// TestCityMintsBinding pins the discriminator that decides whether a
// "<binding>.<name>" identity could have been minted HERE (ga-8yi7ne).
//
// Both agent shapes that run on a rig are asserted, and so is the refusal: an
// unknown binding must be rejected, because that is the only thing standing
// between a neighboring city's canonical identity and our local agent of the
// same leaf name. So are the two near misses that must NOT count: a
// [defaults.rig.imports] entry, which binds nothing until a rig is added, and a
// city-scoped agent, which answers to no rig at all.
func TestCityMintsBinding(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "repo"}},
		Agents: []config.Agent{
			{Name: "worker"}, // unbound: contributes nothing
			{Name: "polecat", Dir: "repo", BindingName: "gastown"},   // bound on repo: contributes "gastown"
			{Name: "dog", Scope: "rig", BindingName: "bd"},           // generic rig template: "bd" on every rig
			{Name: "spaced", Dir: "repo", BindingName: "  padded  "}, // trimmed on both sides
			{Name: "witness", Scope: "city", BindingName: "hoisted"}, // city-scoped: on no rig
		},
		DefaultRigImports: map[string]config.Import{"core": {}},
	}
	for _, tc := range []struct {
		name    string
		binding string
		want    bool
	}{
		{"binding carried by an agent bound to the rig", "gastown", true},
		{"binding carried by a generic rig-scoped template", "bd", true},
		{"binding is trimmed before comparison", "padded", true},
		{"default rig import alone mints nothing", "core", false},
		{"city-scoped agent's binding is on no rig", "hoisted", false},
		{"neighboring city's binding", "pool", false},
		{"neighboring city's other binding", "review", false},
		{"empty binding is never ours", "", false},
		{"whitespace binding is never ours", "   ", false},
		{"agent NAME is not a binding", "worker", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cityMintsBindingOnRig(cfg, "repo", tc.binding); got != tc.want {
				t.Fatalf("cityMintsBindingOnRig(%q) = %v, want %v", tc.binding, got, tc.want)
			}
		})
	}
	if cityMintsBindingOnRig(cfg, "ghost", "bd") {
		t.Fatal(`cityMintsBindingOnRig("ghost", "bd") = true, want false: a generic rig template runs only on configured rigs`)
	}
	if cityMintsBindingOnRig(nil, "repo", "gastown") {
		t.Fatal("cityMintsBindingOnRig(nil, ...) = true, want false — a nil config knows no bindings")
	}
}

// TestCityMintsBindingOnRig_ScopesRigLevelSources pins the rig half of the
// discriminator: a binding imported on ONE rig, or carried by an agent bound to
// one rig, is minted on that rig only. A city-level import applies to every
// rig, and so does a generic rig-scoped template's binding, on every rig this
// city configures. A city-scoped agent hoisted out of alpha's "pool" import
// keeps the binding but adds no rig to it, and a [defaults.rig.imports] entry
// adds none either.
func TestCityMintsBindingOnRig_ScopesRigLevelSources(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "qcore"},
			{Name: "alpha", Imports: map[string]config.Import{"pool": {}}},
		},
		Imports:           map[string]config.Import{"gastown": {}},
		DefaultRigImports: map[string]config.Import{"core": {}},
		Agents: []config.Agent{
			{Name: "omp", Dir: "beta", BindingName: "review"},
			{Name: "dog", Scope: "rig", BindingName: "bd"},
			{Name: "witness", Scope: "city", BindingName: "pool"},
		},
	}
	for _, tc := range []struct {
		rig, binding string
		want         bool
	}{
		{"alpha", "pool", true},
		{"qcore", "pool", false},
		{"beta", "review", true},
		{"qcore", "review", false},
		{"qcore", "gastown", true},
		{"qcore", "core", false},
		{"qcore", "bd", true},
		{"ghost", "bd", false},
		{"", "pool", false},
	} {
		t.Run(tc.rig+"/"+tc.binding, func(t *testing.T) {
			if got := cityMintsBindingOnRig(cfg, tc.rig, tc.binding); got != tc.want {
				t.Fatalf("cityMintsBindingOnRig(%q, %q) = %v, want %v", tc.rig, tc.binding, got, tc.want)
			}
		})
	}
}

// TestForeignBindingOnAnotherRigIsUnobservable is the ga-8yi7ne collision one
// rig over: this city binds "pool" on rig alpha only, and also configures an
// unbound "qcore/omp". A neighbor's canonical "qcore/pool.omp-1" on the shared
// rig must stay foreign — stripping it to "qcore/omp-1" resolves onto our omp
// and releases their live claim — while our own "alpha/pool.omp-1" still
// resolves. Both rig-level sources are covered: the rig's import table, and an
// agent that carries the binding on alpha. So are the two config shapes that
// name "pool" without binding it on qcore: a [defaults.rig.imports] entry, which
// alpha took when it was added and qcore never did, and a city-scoped agent
// hoisted out of alpha's import, which keeps the binding but runs on no rig.
func TestForeignBindingOnAnotherRigIsUnobservable(t *testing.T) {
	ompOn := func(dir, binding string) config.Agent {
		return config.Agent{Name: "omp", Dir: dir, BindingName: binding, MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(4)}
	}
	for _, tc := range []struct {
		name string
		cfg  *config.City
	}{
		{"rig import table", &config.City{
			Rigs: []config.Rig{
				{Name: "qcore", Path: t.TempDir()},
				{Name: "alpha", Path: t.TempDir(), Imports: map[string]config.Import{"pool": {}}},
			},
			Agents: []config.Agent{ompOn("qcore", ""), ompOn("alpha", "")},
		}},
		{"agent bound on the other rig", &config.City{
			Rigs: []config.Rig{
				{Name: "qcore", Path: t.TempDir()},
				{Name: "alpha", Path: t.TempDir()},
			},
			Agents: []config.Agent{ompOn("qcore", ""), ompOn("alpha", "pool")},
		}},
		{"default rig import taken by the other rig only", &config.City{
			Rigs: []config.Rig{
				{Name: "qcore", Path: t.TempDir()},
				{Name: "alpha", Path: t.TempDir(), Imports: map[string]config.Import{"pool": {}}},
			},
			DefaultRigImports: map[string]config.Import{"pool": {}},
			Agents:            []config.Agent{ompOn("qcore", ""), ompOn("alpha", "")},
		}},
		{"city-scoped agent hoisted out of the other rig's import", &config.City{
			Rigs: []config.Rig{
				{Name: "qcore", Path: t.TempDir()},
				{Name: "alpha", Path: t.TempDir(), Imports: map[string]config.Import{"pool": {}}},
			},
			Agents: []config.Agent{
				ompOn("qcore", ""),
				ompOn("alpha", ""),
				{Name: "witness", Scope: "city", BindingName: "pool"},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolAssigneeIsLocallyObservable(tc.cfg, "testcity", "qcore/pool.omp-1"); got {
				t.Fatal(`poolAssigneeIsLocallyObservable("qcore/pool.omp-1") = true, want false: "pool" is minted on alpha, not on the shared rig`)
			}
			if got := poolAssigneeIsLocallyObservable(tc.cfg, "testcity", "alpha/pool.omp-1"); !got {
				t.Fatal(`poolAssigneeIsLocallyObservable("alpha/pool.omp-1") = false, want true: alpha mints "pool"`)
			}
		})
	}
}

// TestForeignBindingIsRejectedAtThePolicyLayer covers the SECOND route into
// the cross-city collision (ga-8yi7ne) — the one that does not go through the
// stripped candidate.
//
// poolIdentityIsInstanceOfLocalAgent reduces "qcore/pool.omp-1" to base
// "qcore/pool.omp", and findAgentByTemplate matches that against an unbound
// "qcore/omp" through legacyBoundTemplateMatchesUnboundAgent. That helper
// accepts ANY binding ON PURPOSE, because it also serves bound->unbound
// migration recovery in cities that removed the binding entirely
// (build_desired_state_legacy_bound_recovery_test.go depends on exactly that).
// So the resolver is deliberately left alone and the discriminator sits in the
// guard, which is asserted here.
func TestForeignBindingIsRejectedAtThePolicyLayer(t *testing.T) {
	cfg := &config.City{
		Rigs:    []config.Rig{{Name: "qcore", Path: t.TempDir()}},
		Imports: map[string]config.Import{"gastown": {}},
		Agents: []config.Agent{
			{Name: "omp", Dir: "qcore", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(4)},
			{Name: "polecat", Dir: "qcore", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(4)},
		},
	}

	// The resolver still mis-resolves, unchanged and intentionally so. If this
	// assertion ever flips, the discriminator moved and the migration-recovery
	// path needs re-checking.
	if findAgentByTemplate(cfg, "qcore/pool.omp") == nil {
		t.Fatal("precondition changed: findAgentByTemplate no longer resolves a foreign binding, so this test is no longer testing the second route")
	}

	for _, tc := range []struct {
		name       string
		assignee   string
		observable bool
	}{
		{"foreign binding, instance form (the field case)", "qcore/pool.omp-1", false},
		{"foreign binding, plain form", "qcore/pool.omp", false},
		{"foreign binding, adhoc form", "qcore/pool.omp-adhoc-a1b2c3d4e5", false},
		// The other direction. A minted binding must still resolve, or this
		// "fix" strands 45 of this city's 46 historically reaped identities.
		{"minted binding still resolves", "qcore/gastown.polecat-1", true},
		{"our own bare instance is untouched", "qcore/omp-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolAssigneeIsLocallyObservable(cfg, "testcity", tc.assignee); got != tc.observable {
				t.Fatalf("poolAssigneeIsLocallyObservable(%q) = %v, want %v", tc.assignee, got, tc.observable)
			}
		})
	}
}

// TestPoolCandidateBindingIsLocal pins the candidate filter directly, including
// the shapes that must pass through untouched.
func TestPoolCandidateBindingIsLocal(t *testing.T) {
	cfg := &config.City{Imports: map[string]config.Import{"gastown": {}}}
	for _, tc := range []struct {
		name      string
		candidate string
		want      bool
	}{
		{"minted binding", "qcore/gastown.polecat", true},
		{"foreign binding", "qcore/pool.omp", false},
		{"no binding at all", "qcore/omp", true},
		{"bare name", "omp", true},
		{"empty", "", true},
		{"trailing dot is not a binding", "qcore/omp.", true},
		{"leading dot is not a binding", "qcore/.omp", true},
		{"session bead id form is untouched", "claude-mc-xyz", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolCandidateBindingIsLocal(cfg, tc.candidate); got != tc.want {
				t.Fatalf("poolCandidateBindingIsLocal(%q) = %v, want %v", tc.candidate, got, tc.want)
			}
		})
	}
}

// TestReleaseOrphanedPoolAssignments_DecommissionedLocalIsProtectedAndReported
// closes the review gap that the decommissioned-agent case existed only in the
// predicate table. A locally-DECOMMISSIONED named identity whose session beads
// have all been purged is indistinguishable from a foreign one — no roster
// entry and no session record is all the gate can see — so its claims are
// knowingly left unreapable. That is the accepted trade, and the summary line
// IS its mitigation: this test proves through a real sweep over a real store
// that the bead survives AND the summary names the identity, so the leak is
// legible rather than silent. While a session record survives, the claim is
// still released (TestReleaseOrphanedPoolAssignments_SessionRecordKeepsTrimmedSeatReapable).
func TestReleaseOrphanedPoolAssignments_DecommissionedLocalIsProtectedAndReported(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// "repo/mad-max" reads exactly like yesterday's crew member whose agent
	// stanza was removed: well-formed <rig>/<name>, rig resolves, name absent
	// from the roster foreignIdentityTestCity declares.
	decommissioned := seedForeignIdentityWork(t, rigStore, "claim held by a decommissioned local agent", "repo/mad-max")

	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		foreignIdentityTestCity(t),
		cityPath,
		nil,
		[]beads.Bead{decommissioned},
		[]beads.Store{rigStore},
		[]string{"repo"},
		map[string]beads.Store{"repo": rigStore},
	)

	if len(released) != 0 {
		t.Fatalf("released = %v, want none (a decommissioned identity is protected, not reaped)", released)
	}
	got, err := rigStore.Get(decommissioned.ID)
	if err != nil {
		t.Fatalf("Get decommissioned claim: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "repo/mad-max" {
		t.Fatalf("claim = status %q assignee %q, want in_progress/repo/mad-max untouched", got.Status, got.Assignee)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "protected 1 foreign/unknown identities this pass (1 claims)") {
		t.Fatalf("the accepted leak's mitigation is the summary line, and it is missing:\n%s", logged)
	}
	if !strings.Contains(logged, `"repo/mad-max" (1: `+decommissioned.ID+")") {
		t.Fatalf("summary must name the decommissioned identity and its claim:\n%s", logged)
	}
}

// seedSeatSessionBead writes the session bead of the "repo/worker" namepool
// seat "worker-gc-nux" into store, closed unless open is set. extra overlays the
// seat's metadata; the alias and alias_history it carries are what the
// provenance check reads.
func seedSeatSessionBead(t *testing.T, store beads.Store, open bool, extra map[string]string) beads.Bead {
	t.Helper()
	metadata := map[string]string{
		"session_name":         "worker-gc-nux",
		"template":             "repo/worker",
		"agent_name":           "repo/worker",
		"pool_slot":            "2",
		poolManagedMetadataKey: boolMetadata(true),
	}
	for k, v := range extra {
		metadata[k] = v
	}
	seat, err := store.Create(beads.Bead{
		Title:    "repo/worker",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:repo/worker"},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create seat session bead: %v", err)
	}
	if !open {
		if err := store.Close(seat.ID); err != nil {
			t.Fatalf("Close seat session bead: %v", err)
		}
	}
	seat, err = store.Get(seat.ID)
	if err != nil {
		t.Fatalf("Reload seat session bead: %v", err)
	}
	return seat
}

// trimmedNamepoolTestCity is foreignIdentityTestCity after "nux" was removed
// from the rig worker's namepool, so "repo/nux" now fails the roster exactly
// like a foreign name.
func trimmedNamepoolTestCity(t *testing.T) *config.City {
	t.Helper()
	cfg := foreignIdentityTestCity(t)
	cfg.Agents[1].NamepoolNames = []string{"furiosa"}
	if poolAssigneeIsLocallyObservable(cfg, "testcity", "repo/nux") {
		t.Fatal(`fixture: "repo/nux" must fail the roster once "nux" is trimmed, or this test proves nothing`)
	}
	return cfg
}

// TestReleaseOrphanedPoolAssignments_SessionRecordKeepsTrimmedSeatReapable is
// the namepool-trim case. Only this city's record of the seat tells "repo/nux"
// apart from a foreign name, and that record survives the seat's close. Every
// row runs ONE sweep carrying the seat's claim and a foreign claim, so a gate
// that protects everything off the roster fails it as surely as one that
// releases everything.
func TestReleaseOrphanedPoolAssignments_SessionRecordKeepsTrimmedSeatReapable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		open          bool
		metadata      map[string]string
		wantReleased  bool
		wantProtected bool
	}{
		{"closed seat under its alias", false, map[string]string{"alias": "repo/nux"}, true, false},
		{"closed seat that moved on to another name", false, map[string]string{"alias": "repo/furiosa", "alias_history": "repo/nux"}, true, false},
		{"closed named session under its configured identity", false, map[string]string{session.NamedSessionIdentityMetadata: "repo/nux"}, true, false},
		// Provenance only reopens the liveness probe; it is not a release
		// verdict. An open seat the sweep's snapshot missed still answers it.
		{"open seat outside the sweep's snapshot", true, map[string]string{"alias": "repo/nux"}, false, false},
		// alias_history is stored comma-joined and looked up by exact value,
		// so a prior name inside a longer history is not found: the claim
		// stays protected and is named, the safe direction.
		{"closed seat whose history holds more than one name", false, map[string]string{"alias": "repo/furiosa", "alias_history": "repo/rictus,repo/nux"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cityStore := beads.NewMemStore()
			rigStore := beads.NewMemStore()
			cfg := trimmedNamepoolTestCity(t)
			seedSeatSessionBead(t, cityStore, tc.open, tc.metadata)
			seat := seedForeignIdentityWork(t, rigStore, "claim left on a trimmed namepool seat", "repo/nux")
			foreign := seedForeignIdentityWork(t, rigStore, "claim held by another city's crew", "repo/dalinar")

			logBuf := captureSweepLog(t)
			released := releaseOrphanedPoolAssignmentsFromBeads(
				cityStore, cfg, cityPath, nil,
				[]beads.Bead{seat, foreign},
				[]beads.Store{rigStore, rigStore},
				[]string{"repo", "repo"},
				map[string]beads.Store{"repo": rigStore},
			)

			gotSeat, err := rigStore.Get(seat.ID)
			if err != nil {
				t.Fatalf("Get seat claim: %v", err)
			}
			if tc.wantReleased {
				if len(released) != 1 || released[0].ID != seat.ID {
					t.Fatalf("released = %v, want exactly [%s] (the closed seat's claim)", released, seat.ID)
				}
				if gotSeat.Status != "open" || gotSeat.Assignee != "" {
					t.Fatalf("seat claim = status %q assignee %q, want open/unassigned", gotSeat.Status, gotSeat.Assignee)
				}
			} else {
				if len(released) != 0 {
					t.Fatalf("released = %v, want none", released)
				}
				if gotSeat.Status != "in_progress" || gotSeat.Assignee != "repo/nux" {
					t.Fatalf("seat claim = status %q assignee %q, want in_progress/repo/nux untouched", gotSeat.Status, gotSeat.Assignee)
				}
			}
			gotForeign, err := rigStore.Get(foreign.ID)
			if err != nil {
				t.Fatalf("Get foreign claim: %v", err)
			}
			if gotForeign.Status != "in_progress" || gotForeign.Assignee != "repo/dalinar" {
				t.Fatalf("foreign claim = status %q assignee %q, want in_progress/repo/dalinar untouched", gotForeign.Status, gotForeign.Assignee)
			}

			logged := logBuf.String()
			if !strings.Contains(logged, `"repo/dalinar" (1: `+foreign.ID+")") {
				t.Fatalf("summary must name the foreign claim:\n%s", logged)
			}
			if tc.wantProtected {
				if !strings.Contains(logged, "protected 2 foreign/unknown identities this pass (2 claims)") ||
					!strings.Contains(logged, `"repo/nux" (1: `+seat.ID+")") {
					t.Fatalf("summary must name the seat's protected claim beside the foreign one:\n%s", logged)
				}
				return
			}
			if !strings.Contains(logged, "protected 1 foreign/unknown identities this pass (1 claims)") {
				t.Fatalf("summary must report exactly the foreign claim:\n%s", logged)
			}
			if strings.Contains(logged, "repo/nux") {
				t.Fatalf("this city's own seat must not be reported as foreign:\n%s", logged)
			}
		})
	}
}

// TestReleaseOrphanedPoolAssignments_NeighborRecordInSharedRigStoreIsNotProvenance
// pins WHERE provenance is read. A shared rig store is exactly where another
// city's session records can sit, so a session bead there answering to
// "repo/dalinar" must not make that claim this city's own: only this city's
// session store counts.
func TestReleaseOrphanedPoolAssignments_NeighborRecordInSharedRigStoreIsNotProvenance(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	seedSeatSessionBead(t, rigStore, false, map[string]string{"session_name": "worker-dalinar", "alias": "repo/dalinar"})
	foreign := seedForeignIdentityWork(t, rigStore, "claim held by another city's crew", "repo/dalinar")

	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore, foreignIdentityTestCity(t), cityPath, nil,
		[]beads.Bead{foreign}, []beads.Store{rigStore}, []string{"repo"},
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none: a record in the shared rig store is not this city's", released)
	}
	got, err := rigStore.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get foreign claim: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "repo/dalinar" {
		t.Fatalf("foreign claim = status %q assignee %q, want in_progress/repo/dalinar untouched", got.Status, got.Assignee)
	}
	if !strings.Contains(logBuf.String(), `"repo/dalinar" (1: `+foreign.ID+")") {
		t.Fatalf("summary must name the protected identity:\n%s", logBuf.String())
	}
}

// provenanceLookupStore records the closed-inclusive reads a sweep issues
// against its session store, the shape only the provenance lookups use, and
// fails them with err when it is set. A partial-result err keeps the rows, as
// the bd store does when it drops rows it could not parse. The lookups are Live
// reads, so a CachingStore wrapped around this double passes its errors
// through unchanged.
type provenanceLookupStore struct {
	beads.Store
	closedQueries []beads.ListQuery
	closedRows    int
	err           error
}

func (s *provenanceLookupStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if !query.IncludeClosed || err != nil {
		return rows, err
	}
	s.closedQueries = append(s.closedQueries, query)
	switch {
	case s.err == nil:
		s.closedRows += len(rows)
		return rows, nil
	case beads.IsPartialResult(s.err):
		s.closedRows += len(rows)
		return rows, s.err
	default:
		return nil, s.err
	}
}

// TestReleaseOrphanedPoolAssignments_ProvenanceLookupFailure covers a session
// store whose provenance lookups fail. A hard failure leaves the trimmed seat's
// claim protected and named, the safe direction, and ends the pass's lookups,
// so an unreachable store costs one failed read per pass however many
// identities miss the roster. That holds behind a primed CachingStore, the
// session store production hands the sweep: the lookups are Live reads, so the
// cache returns the failure as the hard error it is, not as a partial result
// over its cached rows, and records no cache problem for it. A problem recorded
// on every pass would keep postponing the cache's reconcile retry, so it would
// never degrade. A partial result still credits the rows it returned and does
// not end the lookups: the foreign claim is looked up first, so the seat's
// claim behind it is released only if they go on. Either way the failure is
// logged once per pass, not once per candidate.
func TestReleaseOrphanedPoolAssignments_ProvenanceLookupFailure(t *testing.T) {
	unreachable := errors.New("session store unreachable")
	for _, tc := range []struct {
		name string
		err  error
		// cached reads the session store through a primed CachingStore and
		// fails the store under it.
		cached       bool
		wantReleased bool
		wantSummary  string
	}{
		{"hard failure", unreachable, false, false, "protected 2 foreign/unknown identities this pass (2 claims)"},
		{"hard failure under a primed cache", unreachable, true, false, "protected 2 foreign/unknown identities this pass (2 claims)"},
		{"partial result", &beads.PartialResultError{Op: "bd list", Err: errors.New("one row failed to parse")}, false, true, "protected 1 foreign/unknown identities this pass (1 claims)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cityStore := beads.NewMemStore()
			rigStore := beads.NewMemStore()
			cfg := trimmedNamepoolTestCity(t)
			seedSeatSessionBead(t, cityStore, false, map[string]string{"alias": "repo/nux"})
			seat := seedForeignIdentityWork(t, rigStore, "claim left on a trimmed namepool seat", "repo/nux")
			foreign := seedForeignIdentityWork(t, rigStore, "claim held by another city's crew", "repo/dalinar")
			sessions := &provenanceLookupStore{Store: cityStore}
			var sessionStore beads.Store = sessions
			var cache *beads.CachingStore
			if tc.cached {
				cache = beads.NewCachingStoreForTest(sessions, nil)
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatalf("Prime session cache: %v", err)
				}
				sessionStore = cache
			}
			sessions.err = tc.err

			logBuf := captureSweepLog(t)
			released := releaseOrphanedPoolAssignments(cityStore, beads.SessionStore{Store: sessionStore}, cfg, cityPath, nil,
				[]beads.Bead{foreign, seat},
				[]beads.Store{rigStore, rigStore},
				[]string{"repo", "repo"},
				map[string]beads.Store{"repo": rigStore}, nil, nil)

			if gotReleased := len(released) == 1 && released[0].ID == seat.ID; gotReleased != tc.wantReleased || len(released) > 1 {
				t.Fatalf("released = %v, want seat claim %s released = %v", released, seat.ID, tc.wantReleased)
			}
			gotForeign, err := rigStore.Get(foreign.ID)
			if err != nil {
				t.Fatalf("Get foreign claim: %v", err)
			}
			if gotForeign.Status != "in_progress" || gotForeign.Assignee != "repo/dalinar" {
				t.Fatalf("foreign claim = status %q assignee %q, want in_progress/repo/dalinar untouched", gotForeign.Status, gotForeign.Assignee)
			}
			if !beads.IsPartialResult(tc.err) && len(sessions.closedQueries) != 1 {
				t.Fatalf("closed-inclusive session reads = %d, want 1: a hard failure ends the pass's lookups", len(sessions.closedQueries))
			}
			if cache != nil {
				if stats := cache.Stats(); stats.ProblemCount != 0 {
					t.Fatalf("cache recorded %d problems (last %q), want none: the lookup failure is the sweep's to report", stats.ProblemCount, stats.LastProblem)
				}
			}
			logged := logBuf.String()
			if got := strings.Count(logged, "looking up session records for claim provenance"); got != 1 {
				t.Fatalf("want the lookup failure logged once per pass, got %d:\n%s", got, logged)
			}
			if !strings.Contains(logged, tc.err.Error()) {
				t.Fatalf("the logged failure must carry the store's error %q:\n%s", tc.err, logged)
			}
			if !strings.Contains(logged, tc.wantSummary) {
				t.Fatalf("missing %q in log:\n%s", tc.wantSummary, logged)
			}
			if namesSeat := strings.Contains(logged, `"repo/nux" (1: `+seat.ID+")"); namesSeat == tc.wantReleased {
				t.Fatalf("summary names the seat = %v, want %v:\n%s", namesSeat, !tc.wantReleased, logged)
			}
		})
	}
}

// closeNotifications collects the bead.closed notifications a cache emits.
type closeNotifications struct {
	mu  sync.Mutex
	ids []string
}

func (r *closeNotifications) record(eventType, beadID string, _ json.RawMessage) {
	if eventType != events.BeadClosed {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, beadID)
}

// TestReleaseOrphanedPoolAssignments_ProvenanceLookupThroughCache runs the
// namepool-trim case through a primed CachingStore, the session store
// production hands the sweep. The cache holds only open beads, so the seat's
// closed record comes from the store under it, and the Live lookup that finds
// it installs it in the cache. That must not announce a close the cache never
// saw happen, show the record to open-bead readers, or double it in a later
// closed-inclusive read.
func TestReleaseOrphanedPoolAssignments_ProvenanceLookupThroughCache(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	record := seedSeatSessionBead(t, cityStore, false, map[string]string{"alias": "repo/nux"})
	seat := seedForeignIdentityWork(t, rigStore, "claim left on a trimmed namepool seat", "repo/nux")
	foreign := seedForeignIdentityWork(t, rigStore, "claim held by another city's crew", "repo/dalinar")
	notes := &closeNotifications{}
	cache := beads.NewCachingStoreForTest(cityStore, notes.record)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime session cache: %v", err)
	}

	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignments(cityStore, beads.SessionStore{Store: cache}, trimmedNamepoolTestCity(t), t.TempDir(), nil,
		[]beads.Bead{foreign, seat},
		[]beads.Store{rigStore, rigStore},
		[]string{"repo", "repo"},
		map[string]beads.Store{"repo": rigStore}, nil, nil)

	if len(released) != 1 || released[0].ID != seat.ID {
		t.Fatalf("released = %v, want exactly [%s] (the closed seat's claim)", released, seat.ID)
	}
	gotForeign, err := rigStore.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get foreign claim: %v", err)
	}
	if gotForeign.Status != "in_progress" || gotForeign.Assignee != "repo/dalinar" {
		t.Fatalf("foreign claim = status %q assignee %q, want in_progress/repo/dalinar untouched", gotForeign.Status, gotForeign.Assignee)
	}
	if logged := logBuf.String(); !strings.Contains(logged, "protected 1 foreign/unknown identities this pass (1 claims)") {
		t.Fatalf("summary must report exactly the foreign claim:\n%s", logged)
	}

	installed, err := cache.Handles().Cached.Get(record.ID)
	if err != nil {
		t.Fatalf("cached Get of the seat's record: %v; the Live lookup that found it installs it, or this test checks nothing", err)
	}
	if installed.Status != "closed" {
		t.Fatalf("cached seat record status = %q, want closed", installed.Status)
	}
	if len(notes.ids) != 0 {
		t.Fatalf("bead.closed announced for %v, want none: the seat closed before the cache was primed", notes.ids)
	}
	open, err := cache.List(beads.ListQuery{Label: sessionBeadLabel})
	if err != nil {
		t.Fatalf("List open session beads: %v", err)
	}
	for _, b := range open {
		if b.ID == record.ID {
			t.Fatalf("open session listing returned the closed seat record %s", record.ID)
		}
	}
	history, err := cache.List(beads.ListQuery{Metadata: map[string]string{"alias": "repo/nux"}, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List closed-inclusive by alias: %v", err)
	}
	if len(history) != 1 || history[0].ID != record.ID || history[0].Status != "closed" {
		t.Fatalf("closed-inclusive alias listing = %+v, want exactly the closed seat record %s", history, record.ID)
	}
}

// TestReleaseOrphanedPoolAssignments_ProvenanceReadBudget pins what the
// provenance check asks of a 30s patrol. A pass whose candidates all pass the
// roster reads nothing. Otherwise each distinct identity off the roster is
// looked up once per pass by exact metadata, however many claims it carries,
// and the session ledger's size never enters the reads issued or the rows they
// return: the same pass over a store full of unrelated closed session records
// issues the same reads and fetches only the records that answer. What one read
// costs the backend is not counted here: bd/Dolt answers a metadata filter by
// scanning the store (see session.HasRecordAnsweringTo).
func TestReleaseOrphanedPoolAssignments_ProvenanceReadBudget(t *testing.T) {
	// Unit costs, measured through the lookup itself: an identity no record
	// answers to, and one whose closed record answers under its alias.
	unit := &provenanceLookupStore{Store: beads.NewMemStore()}
	seedSeatSessionBead(t, unit.Store, false, map[string]string{"alias": "repo/nux"})
	if found, err := session.HasRecordAnsweringTo(unit, "repo/dalinar"); err != nil || found {
		t.Fatalf("fixture: miss lookup = %v, %v; want no record", found, err)
	}
	missReads := len(unit.closedQueries)
	if found, err := session.HasRecordAnsweringTo(unit, "repo/nux"); err != nil || !found {
		t.Fatalf("fixture: hit lookup = %v, %v; want the seat's record", found, err)
	}
	hitReads := len(unit.closedQueries) - missReads
	if missReads == 0 || hitReads == 0 {
		t.Fatalf("fixture: lookups issued no closed-inclusive reads (miss %d, hit %d), so this test counts nothing", missReads, hitReads)
	}

	for _, tc := range []struct {
		name      string
		assignees []string
		misses    int
		hits      int
	}{
		{"every candidate in the roster", []string{"repo/worker-1", "repo/worker-2"}, 0, 0},
		{"one identity off the roster", []string{"repo/dalinar"}, 1, 0},
		{"four identities, five claims", []string{"repo/dalinar", "repo/pattern", "repo/szeth", "repo/dalinar", "repo/nux"}, 3, 1},
	} {
		for _, ledger := range []int{0, 40} {
			t.Run(fmt.Sprintf("%s/%d unrelated closed records", tc.name, ledger), func(t *testing.T) {
				captureSweepLog(t)
				cityStore := beads.NewMemStore()
				seedSeatSessionBead(t, cityStore, false, map[string]string{"alias": "repo/nux"})
				for i := range ledger {
					seedSeatSessionBead(t, cityStore, false, map[string]string{
						"session_name": fmt.Sprintf("worker-gc-retired-%d", i),
						"alias":        fmt.Sprintf("repo/retired-%d", i),
					})
				}
				rigStore := beads.NewMemStore()
				var work []beads.Bead
				var stores []beads.Store
				var refs []string
				for _, assignee := range tc.assignees {
					work = append(work, seedForeignIdentityWork(t, rigStore, "claim "+assignee, assignee))
					stores = append(stores, rigStore)
					refs = append(refs, "repo")
				}
				sessions := &provenanceLookupStore{Store: cityStore}
				releaseOrphanedPoolAssignments(cityStore, beads.SessionStore{Store: sessions}, trimmedNamepoolTestCity(t), t.TempDir(), nil,
					work, stores, refs, map[string]beads.Store{"repo": rigStore}, nil, nil)

				for _, query := range sessions.closedQueries {
					if len(query.Metadata) == 0 {
						t.Fatalf("closed-inclusive session read with no metadata filter, which reads the whole ledger: %+v", query)
					}
				}
				if want := tc.misses*missReads + tc.hits*hitReads; len(sessions.closedQueries) != want {
					t.Fatalf("closed-inclusive session reads = %d, want %d (%d misses x %d + %d hits x %d)",
						len(sessions.closedQueries), want, tc.misses, missReads, tc.hits, hitReads)
				}
				if sessions.closedRows != tc.hits {
					t.Fatalf("closed-inclusive rows fetched = %d, want %d: only the records that answer may come back", sessions.closedRows, tc.hits)
				}
			})
		}
	}
}

// TestCityRuntimeBeadReconcileTick_ReleasesTrimmedSeatClaimAfterClose is the
// review's reproduction through the real desired-state build and reconcile tick,
// over a city/rig split: "nux" is trimmed from the namepool and its seat is
// closed the way `gc session close` closes it. That lane releases work only
// under the seat's ID and session name, so the claim the seat made under its
// stable alias survives the close, and the dead-assignee sweep is the lane left
// to collect it. The tick must release it there, while a foreign claim in the
// same rig store stays protected and is the only identity the summary names.
func TestCityRuntimeBeadReconcileTick_ReleasesTrimmedSeatClaimAfterClose(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "repo", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "repo",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			NamepoolNames:     []string{"furiosa"},
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"repo": rigStore}
	seat := seedSeatSessionBead(t, cityStore, true, map[string]string{"alias": "repo/nux", "state": "awake", "generation": "1"})
	claim := seedForeignIdentityWork(t, rigStore, "claim made under the seat's stable alias", "repo/nux")
	foreign := seedForeignIdentityWork(t, rigStore, "claim held by another city's crew", "repo/dalinar")

	// The `gc session close` sequence: snapshot the seat, close it, then
	// release its work under the identifiers that lane resolves.
	if err := cityStore.Close(seat.ID); err != nil {
		t.Fatalf("Close seat: %v", err)
	}
	unclaimWorkAssignedToRetiredSessionBead(cityPath, cfg, cityStore, rigStores, seat, "", io.Discard)
	if got, err := rigStore.Get(claim.ID); err != nil || got.Assignee != "repo/nux" {
		t.Fatalf("precondition: claim after close = %+v, %v; the close lane released the alias claim itself, so the sweep is no longer the lane under test", got, err)
	}

	sp := runtime.NewFake()
	snapshot := newSessionBeadSnapshot(nil)
	var buildLog strings.Builder
	result := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, sp,
		cityStore, rigStores, snapshot, nil, &buildLog,
	)
	if len(result.AssignedWorkBeads) != 2 {
		t.Fatalf("AssignedWorkBeads = %d, want both claims; log:\n%s", len(result.AssignedWorkBeads), buildLog.String())
	}

	logBuf := captureSweepLog(t)
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 cfg,
		sp:                  sp,
		standaloneCityStore: cityStore,
		standaloneRigStores: rigStores,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              &stderr,
	}
	cr.beadReconcileTick(context.Background(), result, snapshot, nil, false)

	gotClaim, err := rigStore.Get(claim.ID)
	if err != nil {
		t.Fatalf("Get seat claim: %v", err)
	}
	if gotClaim.Status != "open" || gotClaim.Assignee != "" {
		t.Fatalf("seat claim = status %q assignee %q, want open/unassigned; stderr:\n%s\nlog:\n%s", gotClaim.Status, gotClaim.Assignee, stderr.String(), logBuf.String())
	}
	if !strings.Contains(stderr.String(), "released orphaned pool work: "+claim.ID) {
		t.Fatalf("the tick did not report the release:\n%s", stderr.String())
	}
	gotForeign, err := rigStore.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get foreign claim: %v", err)
	}
	if gotForeign.Status != "in_progress" || gotForeign.Assignee != "repo/dalinar" {
		t.Fatalf("foreign claim = status %q assignee %q, want in_progress/repo/dalinar untouched", gotForeign.Status, gotForeign.Assignee)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "protected 1 foreign/unknown identities this pass (1 claims)") ||
		!strings.Contains(logged, `"repo/dalinar" (1: `+foreign.ID+")") {
		t.Fatalf("summary must report exactly the foreign claim:\n%s", logged)
	}
	if strings.Contains(logged, `"repo/nux"`) {
		t.Fatalf("this city's own seat must not be reported as foreign:\n%s", logged)
	}
}

// TestReleaseConfirmedOrphanSessionWork_ReleasesTrimmedSeatUnderItsStableAlias
// covers the SECOND release site (the ga-jrnou orphan-close tie-break) with the
// one qualified identifier that reaches it: a namepool seat's stable alias.
// Runtime session names are sanitized ("worker-gc-nux"), so a qualified
// identifier arrives only through that alias. "nux" has left the namepool, so
// the alias fails the roster, and that is the ordinary case at this site rather
// than an edge: an orphaned seat is one whose config is gone. The claim must
// still be released, because a roster test here would refuse the seat's own
// work and re-wedge it, while a claim under an identity the seat never held
// stays put.
func TestReleaseConfirmedOrphanSessionWork_ReleasesTrimmedSeatUnderItsStableAlias(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cfg := trimmedNamepoolTestCity(t)
	rigStores := map[string]beads.Store{"repo": rigStore}

	seat := seedForeignIdentityWork(t, rigStore, "claim left on a trimmed namepool seat", "repo/nux")
	foreign := seedForeignIdentityWork(t, rigStore, "claim held by another city's crew", "repo/dalinar")

	released := releaseConfirmedOrphanSessionWork(cfg, cityStore, rigStores,
		[]beads.Bead{seat, foreign},
		[]beads.Store{rigStore, rigStore},
		session.Info{
			ID:                  "gc-seat",
			SessionNameMetadata: "worker-gc-nux",
			Alias:               "repo/nux",
			Template:            "repo/worker",
			PoolSlot:            "2",
			PoolManaged:         true,
		},
	)
	if len(released) != 1 || released[0].ID != seat.ID {
		t.Fatalf("released = %v, want exactly [%s] (the orphaned seat's own alias claim)", released, seat.ID)
	}
	gotSeat, err := rigStore.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get seat claim: %v", err)
	}
	if gotSeat.Status != "open" || gotSeat.Assignee != "" {
		t.Fatalf("seat claim = status %q assignee %q, want open/unassigned", gotSeat.Status, gotSeat.Assignee)
	}
	gotForeign, err := rigStore.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get foreign claim: %v", err)
	}
	if gotForeign.Status != "in_progress" || gotForeign.Assignee != "repo/dalinar" {
		t.Fatalf("foreign claim = status %q assignee %q, want in_progress/repo/dalinar untouched", gotForeign.Status, gotForeign.Assignee)
	}
}

// TestCityRuntimeBeadReconcileTick_OrphanCloseReleasesTrimmedSeatAndClosesIt is
// the ga-jrnou tie-break for a trimmed namepool seat, driven through the
// reconcile tick the way TestSessionReconcilerOrphanCloseReleasesHeldWorkThenCloses
// drives it: an empty desired state and no runtime make the asleep seat a
// confirmed orphan this tick. "nux" left the namepool while the seat still held
// work under its stable alias, so the seat's only qualified identifier fails the
// roster. The orphan close must release that claim and close the seat in the
// same tick: a roster test at that site refuses the alias, the close guard then
// refuses the seat, and the open seat keeps the sweep off the claim, a wedge
// nothing else clears. A claim the seat never held stays put.
func TestCityRuntimeBeadReconcileTick_OrphanCloseReleasesTrimmedSeatAndClosesIt(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "repo", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "repo",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			NamepoolNames:     []string{"furiosa"},
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"repo": rigStore}
	seat := seedSeatSessionBead(t, cityStore, true, map[string]string{
		"alias":              "repo/nux",
		"state":              "asleep",
		"generation":         "1",
		"continuation_epoch": "1",
	})
	claim := seedForeignIdentityWork(t, rigStore, "claim held by the orphaned seat under its stable alias", "repo/nux")
	foreign := seedForeignIdentityWork(t, rigStore, "claim held by another city's crew", "repo/dalinar")

	result := DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{claim, foreign},
		AssignedWorkStores:    []beads.Store{rigStore, rigStore},
		AssignedWorkStoreRefs: []string{"repo", "repo"},
	}

	logBuf := captureSweepLog(t)
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: cityStore,
		standaloneRigStores: rigStores,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              &stderr,
	}
	cr.beadReconcileTick(context.Background(), result, newSessionBeadSnapshot([]beads.Bead{seat}), nil, false)

	gotClaim, err := rigStore.Get(claim.ID)
	if err != nil {
		t.Fatalf("Get seat claim: %v", err)
	}
	if gotClaim.Status != "open" || gotClaim.Assignee != "" {
		t.Fatalf("seat claim = status %q assignee %q, want open/unassigned; stderr:\n%s\nlog:\n%s", gotClaim.Status, gotClaim.Assignee, stderr.String(), logBuf.String())
	}
	gotSeat, err := cityStore.Get(seat.ID)
	if err != nil {
		t.Fatalf("Get seat: %v", err)
	}
	if gotSeat.Status != "closed" {
		t.Fatalf("seat status = %q, want closed in the same tick its claim was released; stderr:\n%s\nlog:\n%s", gotSeat.Status, stderr.String(), logBuf.String())
	}
	gotForeign, err := rigStore.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get foreign claim: %v", err)
	}
	if gotForeign.Status != "in_progress" || gotForeign.Assignee != "repo/dalinar" {
		t.Fatalf("foreign claim = status %q assignee %q, want in_progress/repo/dalinar untouched", gotForeign.Status, gotForeign.Assignee)
	}
}
