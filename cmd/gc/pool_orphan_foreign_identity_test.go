package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
		// cmd_hook.go writes session.GenerateAdhocIdentity straight into the
		// claim assignee for an aliasless pool worker ("rig/polecat-adhoc-<hash>").
		// Unrecognised, its work would be protected forever the moment that
		// session died — a silent LOCAL leak, the worst failure this gate has.
		{"adhoc pool instance", "repo/worker-adhoc-a1b2c3d4e5", true},
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
		// ga-8yi7ne: a NEIGHBOURING city's canonical "<rig>/<binding>.<name>"
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
// Both sources are asserted, and so is the refusal: an unknown binding must be
// rejected, because that is the only thing standing between a neighbouring
// city's canonical identity and our local agent of the same leaf name.
func TestCityMintsBinding(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker"},                            // unbound: contributes nothing
			{Name: "polecat", BindingName: "gastown"},   // bound: contributes "gastown"
			{Name: "dog", BindingName: "bd"},            // bound: contributes "bd"
			{Name: "spaced", BindingName: "  padded  "}, // trimmed on both sides
		},
		DefaultRigImports: map[string]config.Import{"core": {}},
	}
	for _, tc := range []struct {
		name    string
		binding string
		want    bool
	}{
		{"binding carried by a configured agent", "gastown", true},
		{"second binding on the same city", "bd", true},
		{"binding is trimmed before comparison", "padded", true},
		{"default rig import with no instantiated agent", "core", true},
		{"neighbouring city's binding", "pool", false},
		{"neighbouring city's other binding", "review", false},
		{"empty binding is never ours", "", false},
		{"whitespace binding is never ours", "   ", false},
		{"agent NAME is not a binding", "worker", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cityMintsBinding(cfg, tc.binding); got != tc.want {
				t.Fatalf("cityMintsBinding(%q) = %v, want %v", tc.binding, got, tc.want)
			}
		})
	}
	if cityMintsBinding(nil, "gastown") {
		t.Fatal("cityMintsBinding(nil, ...) = true, want false — a nil config knows no bindings")
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
// predicate table. A locally-DECOMMISSIONED named identity is indistinguishable
// from a foreign one — "not in the local roster" is all the gate can see — so
// its claims are knowingly left unreapable. That is the accepted trade, and the
// summary line IS its mitigation: this test proves through a real sweep over a
// real store that the bead survives AND the summary names the identity, so the
// leak is legible rather than silent.
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

// TestReleaseConfirmedOrphanSessionWork_GatesUnobservableIdentifier covers the
// SECOND release site (the ga-jrnou orphan-close tie-break). Its identifier set
// is built from raw session-bead metadata (sessionAssignmentIdentifierRawInfo),
// not from the roster, so a stored session_name colliding with another city's
// identity reaches this release with no other guard. One call carries both
// claims: the unobservable identity is protected while the locally-minted pool
// instance is still released — the same non-vacuity shape as the sweep test, so
// a gate that simply disabled this site would fail the second half.
func TestReleaseConfirmedOrphanSessionWork_GatesUnobservableIdentifier(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cfg := foreignIdentityTestCity(t)
	rigStores := map[string]beads.Store{"repo": rigStore}

	foreign := seedForeignIdentityWork(t, rigStore, "claim under a foreign-shaped stored name", "repo/dalinar")
	local := seedForeignIdentityWork(t, rigStore, "claim under a locally minted pool instance", "repo/worker-1")

	logBuf := captureSweepLog(t)
	released := releaseConfirmedOrphanSessionWork(cfg, cityStore, rigStores,
		[]beads.Bead{foreign, local},
		session.Info{SessionNameMetadata: "repo/dalinar"},
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none (the only matching identifier is unobservable)", released)
	}
	gotForeign, err := rigStore.Get(foreign.ID)
	if err != nil {
		t.Fatalf("Get foreign-shaped claim: %v", err)
	}
	if gotForeign.Status != "in_progress" || gotForeign.Assignee != "repo/dalinar" {
		t.Fatalf("claim = status %q assignee %q, want in_progress/repo/dalinar untouched", gotForeign.Status, gotForeign.Assignee)
	}
	if logged := logBuf.String(); !strings.Contains(logged, `protected "repo/dalinar" (`+foreign.ID+`)`) {
		t.Fatalf("gate must refuse loudly, naming the identity and claim:\n%s", logged)
	}

	released = releaseConfirmedOrphanSessionWork(cfg, cityStore, rigStores,
		[]beads.Bead{foreign, local},
		session.Info{SessionNameMetadata: "repo/worker-1"},
	)
	if len(released) != 1 || released[0].ID != local.ID {
		t.Fatalf("released = %v, want exactly [%s] (the locally minted instance still releases)", released, local.ID)
	}
	gotLocal, err := rigStore.Get(local.ID)
	if err != nil {
		t.Fatalf("Get local claim: %v", err)
	}
	if gotLocal.Status != "open" || gotLocal.Assignee != "" {
		t.Fatalf("local claim = status %q assignee %q, want open/unassigned", gotLocal.Status, gotLocal.Assignee)
	}
}
