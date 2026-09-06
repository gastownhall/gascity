package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storeref"
)

// This file is the two-topology fixture the split-store conformance suite
// (split_topology_conformance_test.go) runs every invariant over.
//
// WHY IT EXISTS: the split-store bug class — code answering "which store owns
// this class of bead?" differently on different paths — is the one this repo
// keeps paying for, and two of its instances reached production. Order-tracking
// beads were created in the work store on a split city (#5127) and accrued
// stranded infrastructure rows until boot refused; `bd sql` kept reading the
// work ledger for relocated graph ids and reported every live molecule root as
// missing (#5125). Both were single-topology fixes: the code was correct on one
// store arrangement and wrong on the other, and nothing ran the same assertion
// on both.
//
// So the fixture builds ONE city twice — single-store and split — and
// forEachTopology runs the invariant body over both. An invariant that encodes
// ownership correctly passes in both; a path that hard-codes one store fails the
// other immediately.
//
// # Fidelity: where each leaf comes from, and what is deliberately NOT wrapped
//
// The leaves are splittest's strict, prefix-disjoint doubles rather than plain
// MemStores, because a MemStore accepts a foreign-prefix create and a dangling
// dep without a word and would make a residence invariant vacuous. Each leaf
// declares the backend it models, which is what production runs:
//
//   - work: BdSemantics, minting "gc-" — bd/Dolt, which hard-fails a mismatched
//     --id and an unresolvable dep endpoint.
//   - class: a REAL beads.SQLiteStore behind SQLiteSemantics, minting the graph
//     class's reserved "gcg-" — the store internal/storebinding/sqlite's
//     OpenEngine opens for the whole split (ONE engine serving all five
//     infrastructure classes, opened with
//     config.ReservedClassPrefix(BeadClassGraph)). It ACCEPTS a residence
//     violation and records it, because SQLite does — and it carries the two
//     capabilities the binding really has and a MemStore leaf does not: the
//     compare-and-swap assignment claim a routed claim acquires through, and
//     the graph applier. See newSplitEnvClassLeaf.
//
// The wrapping is asymmetric on purpose, and the asymmetry is production's, not
// the fixture's: main.go and api_state.go put every WORK store (city and rig)
// behind wrapStoreWithBeadPolicies, while openStorageRoutes keys the class map
// straight to the value the provider's OpenEngine returned. So a relocated class
// store has no bead-policy layer: no create-time storage policy, no read-tier
// expansion. splitEnv.policyWrapped reports which leg a topology's front door is
// on, and the invariants that care assert through it rather than assuming.
//
// That claim is ENFORCED, not just stated:
// TestSplitEnvClassStoreWrappingMatchesOpenStorageRoutes opens routes through
// the real seam and fails if openStorageRoutes ever hands back a policy-wrapped
// class store. Without it the fixture would be free to keep modeling a topology
// production no longer serves, because nothing else in cmd/gc asserts the
// wrapping of the store that function returns.
//
// Accepted fidelity gap: the WORK leaf is in-memory rather than real bd/Dolt
// behind CachingStore (the class leaf is a real SQLite store on disk). The real
// openers are covered by the managed-Dolt and storage-boot integration tests.

// splitEnv is the two-topology store fixture. Every field carries the production
// shape for its topology:
//
//   - split=true: routes relocate all five infrastructure classes to one class
//     store, exactly as storageSplitWhole/openStorageRoutes arrange them, and
//     cfg.Storage says the same thing so config-derived answers
//     (relocatedBeadClasses, storageSplitShapeOf) agree with the routes.
//   - split=false: routes is nil and resolveClassStore's identity branch
//     collapses every class onto the work store, byte-identical to a city that
//     authors no [storage] section at all.
//
// work is the policy-wrapped front door a call site holds as "the city bead
// store". Per-class ownership is DERIVED from it through classStore/graphStore,
// never assumed, so an invariant exercises the same dispatch production makes.
type splitEnv struct {
	cityPath string
	cfg      *config.City
	routes   *storageRoutes
	work     beads.Store
	class    beads.Store
	store    beads.Store
	split    bool

	// Rig leg (only via forEachTopologyWithRig; zero otherwise): one registered
	// rig with a rig-scoped min=0 default-probe pool template — the
	// newNoScaleCheckRigPoolCity shape generalized over both topologies. rig is
	// the policy-wrapped strict rig-prefixed work store, rigStores is the map
	// shape the reconciler paths take, and qualified is the pool template routed
	// work targets through gc.routed_to.
	rig       beads.Store
	rigStores map[string]beads.Store
	rigName   string
	qualified string
}

// Rig-leg identity constants, matching the scale-from-zero fixtures so
// RCA-shaped scenarios read the same across the suite. The bead-id prefix is
// DERIVED from the rig name through config.Rig.EffectivePrefix ("ra"), so the
// cfg the code under test consults and the ids the rig store mints agree by
// construction.
const (
	splitEnvRigName   = "rig-A"
	splitEnvPoolAgent = "executor"
	splitEnvBinding   = "infra"
)

// splitEnvOptions selects the optional legs of the fixture.
type splitEnvOptions struct {
	// rig adds the rig leg: cfg wiring for one rig plus a rig-scoped pool
	// template, and a third strict store minting under the rig's prefix.
	rig bool
}

// newSplitEnv builds one topology of the fixture.
//
// The cfg keeps bd-1.0.5 storage semantics because that is what maps the wisp
// policy onto the ephemeral tier (defaultBeadStorage in bead_policy_store.go);
// under bd-1.0.4 defaults a wisp falls back to the history tier and the
// wisp-tier coverage would be testing nothing.
func newSplitEnv(t *testing.T, split bool) splitEnv {
	t.Helper()
	return newSplitEnvWith(t, split, splitEnvOptions{})
}

// newSplitEnvWithRig is newSplitEnv plus the rig leg, for the RCA-shaped
// scenarios (warm-tick demand, assigned-work capture, wake ownership) that need
// a rig-scoped pool working class-store-resident routed work. The rig leg exists
// on BOTH topologies — rigs are orthogonal to the split, and the single-store
// subtest is what proves a rig-path fix kept legacy behavior.
func newSplitEnvWithRig(t *testing.T, split bool) splitEnv {
	t.Helper()
	return newSplitEnvWith(t, split, splitEnvOptions{rig: true})
}

func newSplitEnvWith(t *testing.T, split bool, opts splitEnvOptions) splitEnv {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := ""
	if opts.rig {
		rigPath = filepath.Join(cityPath, "rigs", splitEnvRigName)
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatalf("mkdir rig path: %v", err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "split-topology-city", Prefix: "gc"},
		Beads: config.BeadsConfig{
			Provider:        "file",
			BDCompatibility: config.BeadsBDCompatibility105,
		},
	}
	work := wrapStoreWithBeadPolicies(splittest.NewWorkStore(t, config.EffectiveHQPrefix(cfg)), cfg)
	e := splitEnv{cityPath: cityPath, cfg: cfg, work: work, store: work, split: split}
	if split {
		// Config and routes state the same relocation. The config half is what
		// relocatedBeadClasses, storageSplitShapeOf and the `gc bd` refusal read;
		// the routes half is what resolveClassStore dispatches on. Keeping them
		// derived from one decision here is what stops the fixture from staging a
		// city no boot could produce.
		cfg.Storage = splitEnvStorageConfig()
		// The class store is NOT policy-wrapped: openStorageRoutes keys the class
		// map straight to the value OpenEngine returned. See the file header.
		e.class = newSplitEnvClassLeaf(t)
		e.routes = splitEnvRoutes(e.class)
		assertSplitEnvStagesAServingSplit(t, e.class, e.routes)
	}
	writeSplitTopologyCityConfig(t, cityPath, rigPath, split)
	// The assigned-work spine resolves a city's bindings BY PATH, because its
	// scans are free functions reached from both planes. Registering the routes
	// this fixture staged is the same thing newCityRuntime does with the ones
	// storageBootGate opened, so both subtests answer residency from the routes
	// the env decided rather than from a second funnel.
	// The work store is registered with them, for the same reason: the census
	// arms are handed the SESSIONS store, and on a split city that is the
	// binding — so without the runtime declaring its work store the census has
	// no way to name the two apart, which is the E2 dual role itself.
	registerResidencyRoutes(cityPath, e.routes, func() beads.Store { return e.work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, e.routes) })
	if opts.rig {
		e.attachRigLeg(t, rigPath)
	}
	return e
}

// assertSplitEnvStagesAServingSplit refuses to hand back a fixture whose split
// is only nominal.
//
// Every invariant in the conformance suite reads "this is the split topology"
// off the fields this constructor sets, and nothing downstream re-checks that
// the binding it names can be READ. Substitute a refusedClassStore for the class
// leg — the store a city configured for a binding it has not converged on
// resolves to, and one line of fixture code away — and the whole suite still
// passes: every row that expects a class-resident bead to be absent from the
// work store gets its expectation from a store that answers the boot refusal to
// everything, and a topology that serves nothing reads as a healthy split.
//
// So both halves are asserted here, once, where the fixture makes the claim: the
// leg answers reads, and the routes derived from it group into one binding with
// no refusal carried. The probes are read-only — a create here would seed a bead
// the residence rows then have to account for.
func assertSplitEnvStagesAServingSplit(t *testing.T, class beads.Store, routes *storageRoutes) {
	t.Helper()
	if err := class.Ping(); err != nil {
		t.Fatalf("the fixture's class leg refuses Ping (%v); this stages a city whose binding serves nothing, and every split-topology invariant below would be asserted against a store that answers the boot refusal to every read", err)
	}
	if _, err := class.List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("the fixture's class leg refuses List (%v); see Ping above — a refusing leg makes 'the bead is not in the work store' true for the wrong reason", err)
	}
	bindings, refused := residencyBindingsFromRoutes(routes)
	if refused != nil {
		t.Fatalf("the routes the fixture staged carry a standing refusal (%v); this is a city that has not converged, not the served whole-split every invariant here describes", refused)
	}
	if len(bindings) != 1 {
		t.Fatalf("the routes the fixture staged group into %d binding(s), want exactly 1; this build serves the whole split or nothing (storageSplitWhole)", len(bindings))
	}
}

// newSplitEnvClassLeaf opens the class leg on the store a split city is really
// served from: a real beads.SQLiteStore under the graph class's reserved prefix,
// which is what internal/storebinding/sqlite's OpenEngine opens, behind the
// kit's SQLiteSemantics strictness so a residence violation is still recorded.
//
// It is the real backend rather than the kit's MemStore leaf because the two
// differ in exactly the ways this suite exists to catch: the SQLite store has
// the compare-and-swap assignment claim a routed claim acquires through, and it
// has the graph applier whose reverse-of-a-parent-child guard is the one thing
// the binding enforces that a MemStore does not. A suite whose class leaf has
// neither cannot see a divergence in either.
//
// (The claim rows were briefly scoped to a per-row helper because materializing
// conformanceGraphRecipe on this leaf failed the graph-apply guard. That was the
// fixture recipe carrying a hand-written parent-child dep no graph.v2 compiler
// emits — see conformanceGraphRecipe — not a property of graph.v2 on a split
// city.)
func newSplitEnvClassLeaf(t *testing.T) beads.Store {
	t.Helper()
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("config.ReservedClassPrefix(graph) = ok:false; the fixture has no reserved namespace to open a class store under")
	}
	namespaces := splitEnvClassNamespaces(t)
	leaf, err := beads.OpenSQLiteStore(t.TempDir(),
		beads.WithSQLiteStoreIDPrefix(prefix),
		beads.WithSQLiteStoreReservedIDPrefixes(namespaces...),
	)
	if err != nil {
		t.Fatalf("opening the SQLite class store the split binding serves from: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := leaf.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	// Both halves carry the same fence, from the one variable above, so they
	// cannot diverge here. Passing it to both is still not redundant, but the
	// reach is one-directional: the wrapper answers first, so an unfenced
	// wrapper over this fenced leaf records the create as an accepted residence
	// violation and only then has the leaf refuse it — a fixture reading a
	// corruption that never happened. The other direction is invisible: a fenced
	// wrapper over an unfenced leaf refuses before the leaf is asked, so the leaf
	// losing its fence would not show up here. That direction is covered where it
	// belongs, on the provider itself — beadstest.RunPinnedIDFenceConformance
	// runs against beads.OpenSQLiteStore directly.
	return splittest.Strict(t, leaf, splittest.SQLiteSemantics, namespaces...)
}

// splitEnvClassNamespaces is the id-namespace fence a whole-split boot puts on
// this binding: every namespace the five infrastructure classes claim, which is
// what internal/storebinding/sqlite's OpenEngine derives from the served class
// set and passes into the store.
//
// It goes through storebinding.EngineReservedPrefixes rather than listing the
// prefixes, because that function is the production derivation — including its
// rule that a set containing work is unfenced. A fixture that spelled the
// prefixes itself would keep passing through a change to either.
func splitEnvClassNamespaces(t *testing.T) []string {
	t.Helper()
	served := make([]coordclass.Class, 0, len(coordclass.Classes()))
	for _, c := range coordclass.Classes() {
		if c.IsInfrastructure() {
			served = append(served, c)
		}
	}
	set, err := storebinding.NewClassSet(served...)
	if err != nil {
		t.Fatalf("building the served class set: %v", err)
	}
	namespaces := storebinding.EngineReservedPrefixes(set)
	if len(namespaces) == 0 {
		t.Fatal("the whole-split binding derived no id namespaces; an unfenced class store accepts another ledger's bead and its namespace claim stops holding")
	}
	return namespaces
}

// splitEnvStorageConfig is the [storage] section of a converged split city: work
// on the reserved binding, all five infrastructure classes on one shared
// binding. It is the only arrangement this build serves (storageSplitWhole).
func splitEnvStorageConfig() *config.StorageConfig {
	return &config.StorageConfig{
		Classes: config.StorageClasses{
			Work:      config.StorageWorkBinding,
			Graph:     splitEnvBinding,
			Sessions:  splitEnvBinding,
			Messaging: splitEnvBinding,
			Orders:    splitEnvBinding,
			Nudges:    splitEnvBinding,
		},
		Bindings: map[string]config.StorageBindingConfig{
			splitEnvBinding: {Provider: config.StorageProviderSQLiteBeads, Path: ".gc/store"},
		},
	}
}

// splitEnvRoutes builds the routes a converged whole-split boot opens: every
// infrastructure class keyed to the one binding store, which is what
// openStorageRoutes produces from the planned assignment.
func splitEnvRoutes(class beads.Store) *storageRoutes {
	routes := &storageRoutes{stores: make(map[coordclass.Class]beads.Store), binding: splitEnvBinding}
	for _, c := range coordclass.Classes() {
		if c.IsInfrastructure() {
			routes.stores[c] = class
		}
	}
	return routes
}

// attachRigLeg wires the rig leg into cfg and the env: one rig with a rig-scoped
// min=0 default-probe pool template (no scale_check — the pool shape of the
// spawn/drain treadmill RCA), and a strict rig-prefixed leaf under the same
// production policy wrap rig stores get from controllerState.buildStores.
func (e *splitEnv) attachRigLeg(t *testing.T, rigPath string) {
	t.Helper()
	maxSess, minSess := 5, 0
	e.cfg.Agents = []config.Agent{{
		Name:              splitEnvPoolAgent,
		MaxActiveSessions: &maxSess,
		MinActiveSessions: &minSess,
		// No ScaleCheck: default-probe pool.
		Dir:      splitEnvRigName,
		Provider: "mock",
	}}
	e.cfg.Rigs = []config.Rig{{Name: splitEnvRigName, Path: rigPath}}
	e.cfg.Providers = map[string]config.ProviderSpec{"mock": {Command: "true"}}
	rigLeaf := splittest.NewWorkStore(t, e.cfg.Rigs[0].EffectivePrefix())
	e.rig = wrapStoreWithBeadPolicies(rigLeaf, e.cfg)
	e.rigStores = map[string]beads.Store{splitEnvRigName: e.rig}
	e.rigName = splitEnvRigName
	e.qualified = e.cfg.Agents[0].QualifiedName()
}

// writeSplitTopologyCityConfig writes the fixture's city.toml so code under test
// that loads config from cityPath sees the same city the in-memory cfg
// describes: file provider (no dolt/bd in the sandbox), bd-1.0.5 storage
// semantics, the [storage] split when the topology has one, and — when the rig
// leg is on — the same rig + pool-template wiring attachRigLeg puts in cfg.
func writeSplitTopologyCityConfig(t *testing.T, cityPath, rigPath string, split bool) {
	t.Helper()
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"split-topology-city\"\nprefix = \"gc\"\n\n")
	b.WriteString("[beads]\nprovider = \"file\"\nbd_compatibility = \"bd-1.0.5\"\n")
	if split {
		b.WriteString("\n[storage.classes]\nwork = \"" + config.StorageWorkBinding + "\"\n")
		for _, class := range []string{"graph", "sessions", "messaging", "orders", "nudges"} {
			fmt.Fprintf(&b, "%s = %q\n", class, splitEnvBinding)
		}
		fmt.Fprintf(&b, "\n[storage.bindings.%s]\nprovider = %q\npath = \".gc/store\"\n", splitEnvBinding, config.StorageProviderSQLiteBeads)
	}
	if rigPath != "" {
		fmt.Fprintf(&b, "\n[providers.mock]\ncommand = \"true\"\n")
		fmt.Fprintf(&b, "\n[[agent]]\nname = %q\ndir = %q\nprovider = \"mock\"\nmax_active_sessions = 5\nmin_active_sessions = 0\n", splitEnvPoolAgent, splitEnvRigName)
		fmt.Fprintf(&b, "\n[[rigs]]\nname = %q\npath = %q\n", splitEnvRigName, rigPath)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write split-topology city.toml: %v", err)
	}
}

// forEachTopology runs the same invariant body over both store topologies. This
// is the core defense against the split-store bug class: an invariant that
// encodes ownership correctly holds in both subtests, while a path that
// hard-codes one store fails the split subtest and a fix that changed legacy
// behavior fails the single-store one.
func forEachTopology(t *testing.T, fn func(t *testing.T, e splitEnv)) {
	t.Helper()
	t.Run("single-store", func(t *testing.T) {
		fn(t, newSplitEnv(t, false))
	})
	t.Run("split", func(t *testing.T) {
		fn(t, newSplitEnv(t, true))
	})
}

// forEachTopologyWithRig is forEachTopology over the rig-legged fixture, for
// invariants about rig-scoped pools working routed orchestration work (the
// treadmill RCA family: warm-tick demand, assigned-work reachability, wake
// ownership). The single-store subtest doubles as the legacy behavior check for
// any rig-path fix under test.
func forEachTopologyWithRig(t *testing.T, fn func(t *testing.T, e splitEnv)) {
	t.Helper()
	t.Run("single-store", func(t *testing.T) {
		fn(t, newSplitEnvWithRig(t, false))
	})
	t.Run("split", func(t *testing.T) {
		fn(t, newSplitEnvWithRig(t, true))
	})
}

// classStore resolves the owning store for a coordination class through the
// production dispatch point. Fixture consumers route through this (or a
// production seam under test) instead of touching e.work/e.class directly, so
// the invariant exercises the ownership decision production makes.
func (e splitEnv) classStore(class string) beads.Store {
	return resolveClassStore(e.routes, e.work, e.cfg, e.cityPath, class, events.Discard)
}

// graphStore is the graph-class front door: the class store on the split
// topology, the work store on the single-store topology. Molecule roots, steps
// and wisps are graph class, so this owns everything mintWisp creates.
func (e splitEnv) graphStore() beads.Store {
	return e.classStore(config.BeadClassGraph)
}

// sessionsStore is the sessions-class front door — and therefore the
// reconciler's LEADING store, because CityRuntime.buildDesiredState passes
// sessionsBeadStore() into buildDesiredStateWithSessionBeads. RCA-shaped
// reconciler scenarios must lead with this store, exactly as production wires
// it.
func (e splitEnv) sessionsStore() beads.Store {
	return e.classStore(config.BeadClassSessions)
}

// owner returns the store that must hold a coordination-class bead on this
// topology, plus the name an assertion should call it.
func (e splitEnv) owner() (beads.Store, string) {
	if e.split {
		return e.class, "class"
	}
	return e.work, "work"
}

// policyWrapped reports whether a store is behind cmd/gc's bead-policy layer —
// create-time storage policy and read-tier expansion. It is true for every work
// store and FALSE for a relocated class store, because openStorageRoutes keys
// the class map to the raw engine value. Invariants about the wisp tier ask this
// instead of assuming, so the suite states main's behavior rather than the
// behavior it would like main to have.
func (e splitEnv) policyWrapped(store beads.Store) bool {
	_, _, ok := unwrapBeadPolicyStore(store)
	return ok
}

// splitWispIDSeq feeds deterministic, process-unique wisp id suffixes.
var splitWispIDSeq atomic.Int64

// splitWispID mints the next wisp bead id in the shape bd's wisp tier mints
// under a reserved class prefix: <prefix>-wisp-<suffix>, e.g. gcg-wisp-0042.
//
// The numeric suffix is deliberate. Production suffixes are short alnum hashes
// ("dv78"), and both shapes make the config-free sling.BeadPrefix heuristic
// report "gcg-wisp" — which is NOT a reserved class prefix — while an ordinary
// class id ("gcg-1a2b3c4d") reports the reserved "gcg". A random suffix would
// flip between the two shapes run to run; the numeric one pins the production
// routing shape deterministically, so any by-id path that routes on the prefix
// heuristic sees wisp ids exactly as production presents them.
func splitWispID() string {
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		prefix = "gcg"
	}
	return fmt.Sprintf("%s-wisp-%04d", prefix, splitWispIDSeq.Add(1))
}

// wispOpts parameterizes mintWispWith so an RCA-shaped state is mintable in one
// call. The zero value plus a title is plain mintWisp.
type wispOpts struct {
	title string
	// routedTo targets the bead at a pool template (gc.routed_to, e.g.
	// e.qualified) — the routed-demand shape of the treadmill RCA.
	routedTo string
	// status is applied AFTER the create ("" keeps the store's create default,
	// "open"): stores mint open beads, so a claimed state is a post-create
	// mutation exactly as a production claim is.
	status string
	// assignee is applied with status (the claiming session's identity) — the
	// claimed-work shape of the assigned-work and wake RCA sites.
	assignee string
	// metadata is merged over the defaults (kind=wisp, routedTo) last, so a
	// scenario can layer extra keys.
	metadata map[string]string
}

// mintWisp creates a wisp root bead through the graph-class front door, the way
// production molecule materialization does: a graph-class create carrying
// gc.kind=wisp. On the single-store topology the policy front door classifies it
// as the wisp policy and lands it on the ephemeral tier; on the split topology
// the relocated class store has no policy layer, so the same create lands a
// durable row — mintWispWith asserts whichever of the two this topology's front
// door actually produces.
//
// Production orchestration work is wisps, not durable rows: invariants seeded
// only with durable beads have already missed a live incident, so invariants
// about orchestration work should prefer this over a plain Create.
func (e splitEnv) mintWisp(t *testing.T, title string) beads.Bead {
	t.Helper()
	return e.mintWispWith(t, wispOpts{title: title})
}

// mintWispWith is mintWisp with the full RCA-state option set.
func (e splitEnv) mintWispWith(t *testing.T, opts wispOpts) beads.Bead {
	t.Helper()
	md := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}
	if opts.routedTo != "" {
		md[beadmeta.RoutedToMetadataKey] = opts.routedTo
	}
	for k, v := range opts.metadata {
		md[k] = v
	}
	b := beads.Bead{
		Title:    opts.title,
		Type:     "task", // graph.v2 wisps materialize as issue_type "task"
		Metadata: md,
	}
	front := e.graphStore()
	if e.split {
		// The class store mints under the reserved prefix and honors a pinned
		// in-prefix id, mirroring bd's wisp minting under the class scope.
		b.ID = splitWispID()
	}
	created, err := front.Create(b)
	if err != nil {
		t.Fatalf("minting wisp %q: %v", opts.title, err)
	}
	// Fixture honesty: the bead must classify as infrastructure, or every
	// ownership invariant built on it is vacuous.
	if !coordclass.Classify(created).IsInfrastructure() {
		t.Fatalf("minted wisp %s classifies as work, want infrastructure (type=%q metadata=%v)",
			created.ID, created.Type, created.Metadata)
	}
	// Tier honesty, stated per leg rather than assumed. A policy-wrapped front
	// door must land the wisp on the ephemeral tier under bd-1.0.5 semantics; an
	// unwrapped relocated class store applies no create policy at all, so the
	// same create is durable. Asserting the wrong one either way would hide a
	// regression in the leg that does apply the policy.
	if wantEphemeral := e.policyWrapped(front); created.Ephemeral != wantEphemeral {
		t.Fatalf("minted wisp %s has Ephemeral=%v, want %v: policy-wrapped=%v (a wrapped front door maps the wisp policy onto the ephemeral tier under bd-1.0.5; a relocated class store carries no policy layer, so its creates are durable)",
			created.ID, created.Ephemeral, wantEphemeral, wantEphemeral)
	}
	if opts.status == "" && opts.assignee == "" {
		return created
	}
	up := beads.UpdateOpts{}
	if opts.status != "" {
		up.Status = &opts.status
	}
	if opts.assignee != "" {
		up.Assignee = &opts.assignee
	}
	if err := front.Update(created.ID, up); err != nil {
		t.Fatalf("staging wisp %s state (status=%q assignee=%q): %v", created.ID, opts.status, opts.assignee, err)
	}
	staged, err := front.Get(created.ID)
	if err != nil {
		t.Fatalf("reloading staged wisp %s: %v", created.ID, err)
	}
	return staged
}

// mintEphemeralGraphBead creates a graph-class bead that lands on the EPHEMERAL
// tier of whichever store owns the class on this topology, and fails the test if
// it did not.
//
// It exists because mintWisp lands a DURABLE row on the split leg: the relocated
// class store carries no bead-policy layer, so the same create that maps onto
// the ephemeral tier through a work front door lands a plain row there. Every
// tier invariant built on mintWisp is therefore vacuous on the leg that matters,
// which is how a whole ephemeral tier went missing from the federated readers
// unnoticed (ga-8lyxc).
//
// The tier is reached the way production reaches it on each leg, not by patching
// the bead afterwards:
//
//   - policy-wrapped front door (the single-store topology's work store): a
//     gc.kind=wisp create, which defaultBeadStorage maps to "ephemeral" under
//     bd-1.0.5 — plain mintWisp.
//   - unwrapped relocated class store (the split topology): the store's own
//     StorageCreateStore capability, which is the exact call
//     createWithStoragePolicy makes once a storage class is decided. It is a
//     real production shape on this leg: internal/mail/beadmail creates its
//     message beads Ephemeral on whatever store owns the messaging class, and
//     that class is relocated to this same binding.
func mintEphemeralGraphBead(t *testing.T, e splitEnv, title string) beads.Bead {
	t.Helper()
	front := e.graphStore()
	if e.policyWrapped(front) {
		return e.mintWispWith(t, wispOpts{title: title})
	}
	store, ok := front.(beads.StorageCreateStore)
	if !ok {
		t.Fatalf("the relocated class store (%T) implements no StorageCreateStore, so this fixture cannot reach its ephemeral tier the way createWithStoragePolicy does", front)
	}
	created, err := store.CreateWithStorage(beads.Bead{
		ID:       splitWispID(),
		Title:    title,
		Type:     "task", // graph.v2 wisps materialize as issue_type "task"
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
	}, beads.StorageEphemeral)
	if err != nil {
		t.Fatalf("minting ephemeral graph bead %q on the relocated class store: %v", title, err)
	}
	if !created.Ephemeral {
		t.Fatalf("minted graph bead %s has Ephemeral=false; the fixture is not exercising the relocated store's wisp tier and every tier assertion on it is vacuous", created.ID)
	}
	if !coordclass.Classify(created).IsInfrastructure() {
		t.Fatalf("minted ephemeral graph bead %s classifies as work, want infrastructure (type=%q metadata=%v)", created.ID, created.Type, created.Metadata)
	}
	return created
}

// mintDurableGraphBead creates a DURABLE graph-class bead through the graph
// front door: the routed control/workflow shape (gc.kind=workflow), which the
// storage policy keeps off the ephemeral tier on either leg. routedTo, if
// non-empty, targets it at a pool template. Fixture honesty mirrors mintWisp:
// the bead must classify as infrastructure and must NOT be ephemeral, or every
// "durable" invariant built on it is vacuous.
func mintDurableGraphBead(t *testing.T, e splitEnv, title, routedTo string) beads.Bead {
	t.Helper()
	md := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}
	if routedTo != "" {
		md[beadmeta.RoutedToMetadataKey] = routedTo
	}
	created, err := e.graphStore().Create(beads.Bead{Title: title, Type: "task", Metadata: md})
	if err != nil {
		t.Fatalf("minting durable graph bead %q: %v", title, err)
	}
	if !coordclass.Classify(created).IsInfrastructure() {
		t.Fatalf("minted durable graph bead %s classifies as work, want infrastructure (type=%q metadata=%v)", created.ID, created.Type, created.Metadata)
	}
	if created.Ephemeral {
		t.Fatalf("minted durable graph bead %s landed on the ephemeral tier, want a durable row", created.ID)
	}
	return created
}

// splitTopologyClassTable is the per-class ownership table: the five
// coordination classes route to the class store on a split city; work stays on
// the work store.
var splitTopologyClassTable = []struct {
	class     string
	wantClass bool
}{
	{config.BeadClassGraph, true},
	{config.BeadClassSessions, true},
	{config.BeadClassMessaging, true},
	{config.BeadClassOrders, true},
	{config.BeadClassNudges, true},
	{config.BeadClassWork, false},
}

// TestSplitEnvTopologies is the fixture self-test: it pins the properties every
// conformance invariant assumes, in both topologies. The plain env has no rig
// leg — reconciler-shaped scenarios opt in via forEachTopologyWithRig.
func TestSplitEnvTopologies(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		assertSplitEnvPins(t, e)
		if e.rig != nil || e.rigStores != nil || e.rigName != "" || e.qualified != "" {
			t.Fatalf("plain splitEnv grew a rig leg (rig=%p rigStores=%v rigName=%q qualified=%q); the rig leg must stay opt-in",
				e.rig, e.rigStores, e.rigName, e.qualified)
		}
	})
}

// TestSplitEnvTopologiesWithRigLeg is the rig-leg self-test: every base pin holds
// unchanged with the rig leg attached, plus the rig leg's own routing and
// disjointness properties.
func TestSplitEnvTopologiesWithRigLeg(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		assertSplitEnvPins(t, e)
		assertRigLeg(t, e)
	})
}

// TestSplitEnvClassStoreWrappingMatchesOpenStorageRoutes anchors the fixture's
// central asymmetry to the production seam that creates it.
//
// newSplitEnvWith assigns the class leaf RAW (e.class = classLeaf) while every
// work store goes through wrapStoreWithBeadPolicies, and the file header claims
// "the asymmetry is production's, not the fixture's". Nothing in the fixture can
// check that claim: splitEnv never calls openStorageRoutes, so a change that
// started wrapping the class map would leave the fixture modeling a topology
// production no longer serves — silently, because every conformance invariant
// asks the fixture rather than the seam.
//
// This test asks the seam. It opens routes the way boot opens them — a real
// resolved plan, the compiled provider's own OpenEngine — and pins that the
// store openStorageRoutes hands back for every relocated class carries NO
// bead-policy layer. Wrap the class map in openStorageRoutes and this is the
// first thing that fails, which is what the fixture's model is entitled to
// assume.
//
// This pins TODAY's behavior, not desired behavior: giving the coordination
// classes a policy layer is a live design question (it would move wisp creates
// on a split city from the durable tier to the ephemeral one). The slice that
// makes that call updates this pin, splitEnv's `e.class = classLeaf`, and
// I11's wrapped/unwrapped branch together.
func TestSplitEnvClassStoreWrappingMatchesOpenStorageRoutes(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))

	plan, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatalf("resolving the storage plan for a converged split city: %v", err)
	}
	target := mustResolveInfraTarget(t, root, cfg)
	routes, err := openStorageRoutes(plan, target)
	if err != nil {
		t.Fatalf("openStorageRoutes: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	relocated := 0
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() {
			continue
		}
		store, ok := routes.storeFor(class)
		if !ok {
			t.Errorf("openStorageRoutes relocated no store for infrastructure class %v; the converged split assigns all five", class)
			continue
		}
		relocated++
		if _, _, wrapped := unwrapBeadPolicyStore(store); wrapped {
			t.Errorf("openStorageRoutes returned a POLICY-WRAPPED store for class %v. splitEnv models the relocated class store as the raw engine value (e.class = classLeaf, split_topology_env_test.go), and I11 branches on that asymmetry. Wrapping the class map changes wisp storage on a split city from a durable row to an ephemeral one — update this pin, splitEnv's class leg, and I11's wrapped/unwrapped branch together.", class)
		}
	}
	if relocated == 0 {
		t.Fatal("openStorageRoutes relocated no classes at all; this pin is evaluating nothing")
	}

	// The work class is NOT relocated, and the work store the caller keeps
	// holding is the policy-wrapped one — the other half of the asymmetry.
	if _, ok := routes.storeFor(coordclass.ClassWork); ok {
		t.Error("openStorageRoutes relocated the WORK class; work stays on the reserved work binding, and splitEnv's work leg would be modeling a store production does not open")
	}
}

// assertSplitEnvPins runs the topology-appropriate base pins shared by the plain
// and rig-legged envs.
func assertSplitEnvPins(t *testing.T, e splitEnv) {
	t.Helper()
	if !sameStorePtr(e.store, e.work) {
		t.Fatalf("splitEnv.store front door = %p, want the work store handle %p", e.store, e.work)
	}
	if !e.policyWrapped(e.work) {
		t.Fatal("the work store is not policy-wrapped; production puts every work store behind wrapStoreWithBeadPolicies")
	}
	// The work leaf must mint under the prefix cfg says the HQ scope uses.
	// Without this the config-aware by-id helpers (sling.BeadPrefixForCity and
	// everything built on it) would be answering about a namespace no store in
	// the fixture owns, and the routing invariants would be vacuous.
	hqBead, err := e.work.Create(beads.Bead{Title: "hq prefix probe", Type: "task"})
	if err != nil {
		t.Fatalf("create hq prefix probe: %v", err)
	}
	if want := config.EffectiveHQPrefix(e.cfg); !strings.HasPrefix(hqBead.ID, want+"-") {
		t.Fatalf("work store minted %q, want the cfg-derived HQ prefix %q-; the config-aware by-id helpers would disagree with the store", hqBead.ID, want)
	}
	if e.split {
		assertSplitTopology(t, e)
	} else {
		assertSingleStoreTopology(t, e)
	}
	assertWispResidence(t, e)
}

// assertSplitTopology pins the split-city half: routes and config agree, the
// handles are distinct, class routing sends every coordination class to the
// class store, and the two id spaces are prefix-disjoint.
func assertSplitTopology(t *testing.T, e splitEnv) {
	t.Helper()
	if e.class == nil || e.routes == nil {
		t.Fatal("split topology has no class store or no routes")
	}
	if sameStorePtr(e.work, e.class) {
		t.Fatal("work and class are the same handle on the split topology, want distinct stores")
	}
	if shape, binding := storageSplitShapeOf(e.cfg.EffectiveStorage()); shape != storageSplitWhole || binding != splitEnvBinding {
		t.Fatalf("cfg describes shape %v binding %q, want the whole split on %q — the fixture must stage a city a boot could actually serve", shape, binding, splitEnvBinding)
	}
	// The config half and the routes half must name the SAME set of relocated
	// classes. A fixture whose config says one thing and whose routes do another
	// stages a city no boot produces, and every invariant on it would be about
	// nothing. This is main's own anti-drift rule
	// (TestRelocatedBeadClassesAgreeWithClassStoreRouting) applied to the fixture.
	named := make(map[string]bool)
	for _, class := range relocatedBeadClasses(e.cfg) {
		named[class.Class] = true
	}
	for _, row := range splitTopologyClassTable {
		want, name := e.work, "work"
		if row.wantClass {
			want, name = e.class, "class"
		}
		if got := e.classStore(row.class); !sameStorePtr(got, want) {
			t.Errorf("resolveClassStore(%q) on the split topology did not route to the %s store", row.class, name)
		}
		if named[row.class] != row.wantClass {
			t.Errorf("class %q: relocatedBeadClasses names it = %v, routing relocates it = %v", row.class, named[row.class], row.wantClass)
		}
	}
	// The named helpers production call sites use must agree with the table —
	// same dispatch point, but a divergence here is exactly the bug class this
	// harness exists for.
	if got := resolveSessionStore(e.routes, e.work, e.cfg, e.cityPath, events.Discard); !sameStorePtr(got, e.class) {
		t.Error("resolveSessionStore routed off the class store on a split city")
	}
	if got := resolveGraphStore(e.routes, e.work, e.cfg, e.cityPath, events.Discard); !sameStorePtr(got, e.class) {
		t.Error("resolveGraphStore routed off the class store on a split city")
	}
	if store, relocated := graphClassBinding(e.routes); !relocated || !sameStorePtr(store, e.class) {
		t.Error("graphClassBinding does not report the graph class as relocated to the class store")
	}
	assertPrefixDisjoint(t, e)
}

// assertPrefixDisjoint pins the ID-prefix half of the boundary invariant: every
// class-store bead carries the graph class's reserved prefix and no work-store
// bead does. Every by-id routing decision in the program rides on this.
func assertPrefixDisjoint(t *testing.T, e splitEnv) {
	t.Helper()
	workBead, err := e.work.Create(beads.Bead{Title: "work-class backlog item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if config.IsReservedClassPrefix(sling.BeadPrefix(workBead.ID)) {
		t.Errorf("work-store bead id %q carries a reserved class prefix; the id spaces must be disjoint", workBead.ID)
	}
	sessionBead, err := e.class.Create(beads.Bead{
		Title:    "worker-1",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_id": "sess-1"},
	})
	if err != nil {
		t.Fatalf("create session bead in class store: %v", err)
	}
	classPrefix, _ := config.ReservedClassPrefix(config.BeadClassGraph)
	if !strings.HasPrefix(sessionBead.ID, classPrefix+"-") {
		t.Errorf("class-store bead id %q does not carry the reserved class prefix %q-", sessionBead.ID, classPrefix)
	}
	if !config.IsReservedClassPrefix(sling.BeadPrefix(sessionBead.ID)) {
		t.Errorf("class-store bead id %q does not resolve to a reserved class prefix; by-id routing would send it to the work store", sessionBead.ID)
	}
	// Strictness is live through the policy stack: an explicitly class-prefixed
	// create against the WORK front door is a residence violation and must fail
	// loud (bd rejects a mismatched --id), not mint a foreign-prefix row the way
	// a plain MemStore would.
	if leaked, err := e.work.Create(beads.Bead{ID: classPrefix + "-leak", Title: "misrouted class bead", Type: "task"}); err == nil {
		t.Errorf("work front door accepted an explicitly class-prefixed create (minted %q); the strict residence guard is not wired", leaked.ID)
	}
}

// assertSingleStoreTopology pins the legacy half: no routes, no class store, and
// resolveClassStore's identity arm collapses EVERY class — known and unknown —
// to the work store, byte-identical to a city that authors no [storage].
func assertSingleStoreTopology(t *testing.T, e splitEnv) {
	t.Helper()
	if e.routes != nil {
		t.Fatalf("single-store topology has non-nil routes %+v", e.routes)
	}
	if e.class != nil {
		t.Fatalf("single-store topology has a non-nil class store %p", e.class)
	}
	if e.cfg.Storage != nil {
		t.Fatalf("single-store topology authored a [storage] section: %+v", e.cfg.Storage)
	}
	if got := relocatedBeadClasses(e.cfg); len(got) != 0 {
		t.Errorf("relocatedBeadClasses on the single-store topology = %v, want none", got)
	}
	classes := make([]string, 0, len(splitTopologyClassTable)+1)
	for _, row := range splitTopologyClassTable {
		classes = append(classes, row.class)
	}
	// The identity arm is class-blind, so even a class string no switch arm
	// names must collapse to the work store.
	classes = append(classes, "not-a-known-class")
	for _, class := range classes {
		if got := e.classStore(class); !sameStorePtr(got, e.work) {
			t.Errorf("resolveClassStore(%q) on the single-store topology did not collapse to the work store", class)
		}
	}
	if got := resolveSessionStore(e.routes, e.work, e.cfg, e.cityPath, events.Discard); !sameStorePtr(got, e.work) {
		t.Error("resolveSessionStore did not collapse to the work store on a single-store city")
	}
	if got := resolveGraphStore(e.routes, e.work, e.cfg, e.cityPath, events.Discard); !sameStorePtr(got, e.work) {
		t.Error("resolveGraphStore did not collapse to the work store on a single-store city")
	}
	if _, relocated := graphClassBinding(e.routes); relocated {
		t.Error("graphClassBinding reports the graph class as relocated on a single-store city")
	}
}

// assertWispResidence pins the residence half of the wisp fixture in both
// topologies: the minted wisp is resident in its owning store ONLY, and is
// readable back through the front door. The tier half is asserted per leg inside
// mintWispWith, because the two legs genuinely differ.
func assertWispResidence(t *testing.T, e splitEnv) {
	t.Helper()
	title := "single-store conformance wisp"
	if e.split {
		title = "split conformance wisp"
	}
	w := e.mintWisp(t, title)

	if e.split {
		classPrefix, _ := config.ReservedClassPrefix(config.BeadClassGraph)
		if !strings.HasPrefix(w.ID, classPrefix+"-wisp-") {
			t.Fatalf("split-topology wisp id = %q, want a %s-wisp- shaped id", w.ID, classPrefix)
		}
		if _, err := e.class.Get(w.ID); err != nil {
			t.Fatalf("minted wisp %s not resident in the class store: %v", w.ID, err)
		}
		if _, err := e.work.Get(w.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("minted wisp %s resolves in the WORK store (err=%v); graph-class beads must be class-resident on a split city", w.ID, err)
		}
	} else if config.IsReservedClassPrefix(sling.BeadPrefix(w.ID)) {
		t.Fatalf("single-store wisp id %q carries a reserved class prefix; a legacy city mints work-store ids", w.ID)
	}

	got, err := e.graphStore().Get(w.ID)
	if err != nil {
		t.Fatalf("minted wisp %s not readable through the graph front door: %v", w.ID, err)
	}
	if got.ID != w.ID {
		t.Fatalf("graph front door returned %q for %q", got.ID, w.ID)
	}
}

// assertRigLeg pins the rig leg: the cfg wiring is the RCA pool shape, the rig
// store is a genuine THIRD handle, class routing never resolves to it, and the
// id spaces stay pairwise disjoint with strict residence in every direction.
func assertRigLeg(t *testing.T, e splitEnv) {
	t.Helper()
	if e.rig == nil {
		t.Fatal("rig-legged env has a nil rig store")
	}
	if !sameStorePtr(e.rigStores[e.rigName], e.rig) {
		t.Fatalf("rigStores[%q] is not the rig store handle", e.rigName)
	}
	if want := splitEnvRigName + "/" + splitEnvPoolAgent; e.qualified != want {
		t.Fatalf("qualified pool template = %q, want %q", e.qualified, want)
	}

	// cfg wiring: one registered rig whose path exists, and one rig-scoped min=0
	// default-probe pool template — the treadmill RCA pool shape.
	if len(e.cfg.Rigs) != 1 || e.cfg.Rigs[0].Name != e.rigName {
		t.Fatalf("cfg.Rigs = %+v, want exactly the %q rig", e.cfg.Rigs, e.rigName)
	}
	if _, err := os.Stat(e.cfg.Rigs[0].Path); err != nil {
		t.Fatalf("registered rig path %q not on disk: %v", e.cfg.Rigs[0].Path, err)
	}
	if len(e.cfg.Agents) != 1 {
		t.Fatalf("cfg.Agents = %+v, want exactly the pool template", e.cfg.Agents)
	}
	agent := e.cfg.Agents[0]
	if agent.Dir != e.rigName || agent.ScaleCheck != "" ||
		agent.MinActiveSessions == nil || *agent.MinActiveSessions != 0 ||
		agent.MaxActiveSessions == nil || *agent.MaxActiveSessions <= 0 {
		t.Fatalf("pool template %+v is not the rig-scoped min=0 default-probe shape", agent)
	}

	// The rig store is a third handle, distinct from both split-boundary stores.
	if sameStorePtr(e.rig, e.work) {
		t.Fatal("rig store aliases the work store")
	}
	if e.class != nil && sameStorePtr(e.rig, e.class) {
		t.Fatal("rig store aliases the class store")
	}

	// Routing identity: resolveClassStore dispatches the split boundary only — no
	// coordination (or work) CLASS may ever resolve to a rig store. Rig stores are
	// addressed by rig NAME (store refs / rigStores), and a class arm quietly
	// returning one would be a new landmine of the audited kind.
	for _, row := range splitTopologyClassTable {
		if sameStorePtr(e.classStore(row.class), e.rig) {
			t.Errorf("resolveClassStore(%q) resolved to the RIG store", row.class)
		}
	}

	assertRigPrefixDisjoint(t, e)
}

// assertRigPrefixDisjoint pins the three-way id-space disjointness the rig leg
// adds: work, class and rig prefixes are pairwise distinct; the rig prefix is a
// work-shaped (non-reserved) prefix agreeing with cfg's EffectivePrefix; and
// residence is strict in every cross-store direction.
func assertRigPrefixDisjoint(t *testing.T, e splitEnv) {
	t.Helper()
	rigPrefix := e.cfg.Rigs[0].EffectivePrefix()
	if config.IsReservedClassPrefix(rigPrefix) {
		t.Fatalf("rig prefix %q is a reserved class prefix; rig beads are WORK beads", rigPrefix)
	}

	rigBead, err := e.rig.Create(beads.Bead{Title: "rig-scoped backlog item", Type: "task"})
	if err != nil {
		t.Fatalf("create rig bead: %v", err)
	}
	if !strings.HasPrefix(rigBead.ID, rigPrefix+"-") {
		t.Errorf("rig-store bead id %q does not carry the cfg-derived rig prefix %q-; by-id routing paths that consult cfg would disagree with the store", rigBead.ID, rigPrefix)
	}
	workBead, err := e.work.Create(beads.Bead{Title: "hq work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if workPrefix := sling.BeadPrefix(workBead.ID); workPrefix == rigPrefix {
		t.Errorf("work and rig stores share the id prefix %q; the trio must stay pairwise disjoint", workPrefix)
	}

	// Residence: the rig bead resolves ONLY in the rig store.
	if _, err := e.work.Get(rigBead.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("rig bead %s resolves in the WORK store (err=%v)", rigBead.ID, err)
	}
	if e.split {
		if _, err := e.class.Get(rigBead.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("rig bead %s resolves in the CLASS store (err=%v)", rigBead.ID, err)
		}
	}

	// Strict cross-prefix creates through the BdSemantics front doors in the
	// directions the base pin does not already cover.
	classPrefix, _ := config.ReservedClassPrefix(config.BeadClassGraph)
	if leaked, err := e.rig.Create(beads.Bead{ID: classPrefix + "-rigleak", Title: "misrouted class bead", Type: "task"}); err == nil {
		t.Errorf("rig front door accepted a class-prefixed create (minted %q)", leaked.ID)
	}
	if leaked, err := e.rig.Create(beads.Bead{ID: sling.BeadPrefix(workBead.ID) + "-999", Title: "misrouted work bead", Type: "task"}); err == nil {
		t.Errorf("rig front door accepted a work-prefixed create (minted %q)", leaked.ID)
	}
	if leaked, err := e.work.Create(beads.Bead{ID: rigPrefix + "-leak", Title: "misrouted rig bead", Type: "task"}); err == nil {
		t.Errorf("work front door accepted a rig-prefixed create (minted %q)", leaked.ID)
	}
	if e.split {
		// SQLite itself keeps a pinned id verbatim — normalizeCreate has no
		// prefix check — but the binding this store serves is FENCED to the
		// namespaces its five infrastructure classes claim, so the write is
		// refused before SQLite can land it. A rig work prefix is in none of
		// them, which is what makes this row the fence's and not the backend's.
		leaked, err := e.class.Create(beads.Bead{ID: rigPrefix + "-leak", Title: "misrouted rig bead", Type: "task"})
		if !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
			t.Errorf("class front door answered (%q, %v) for a rig-prefixed create, want ErrPinnedIDOutsideNamespace; an unfenced binding lands another ledger's bead where no prefix route will ever look for it", leaked.ID, err)
		}
		if _, err := e.class.Get(rigPrefix + "-leak"); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("after the refusal the class store still holds %q (err=%v)", rigPrefix+"-leak", err)
		}
		// The must-be-silent counterpart: the fence admits the namespaces the
		// binding does hold, so this row is not passing on a store that refuses
		// every pinned id.
		held := classPrefix + "-wisp-fixture"
		if created, err := e.class.Create(beads.Bead{ID: held, Title: "held by this binding", Type: "task"}); err != nil {
			t.Errorf("class front door refused %q, a pinned id in a namespace it serves: %v", held, err)
		} else if created.ID != held {
			t.Errorf("class front door rewrote the pinned id to %q", created.ID)
		}
	}
}

// classCandidatesForID answers "which stores could hold this id" through the
// SHARED by-id class resolver, storeref.ClassCandidates.
//
// The routing is keyed on the resolveClassStore identity this fixture already
// derives everything from — graphStore() vs work — so the single-store topology
// gets nil back and its by-id resolution stays byte-identical. The shadows are
// the configured work prefixes (HQ, then each rig), which is what keeps a rig
// whose prefix sits inside the class namespace reachable by id.
func (e splitEnv) classCandidatesForID(id string) []beads.Store {
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		return nil
	}
	shadows := []storeref.PrefixedStore{{Prefix: config.EffectiveHQPrefix(e.cfg), Store: e.work}}
	for _, rig := range e.cfg.Rigs {
		if store := e.rigStores[rig.Name]; store != nil {
			shadows = append(shadows, storeref.PrefixedStore{Prefix: rig.EffectivePrefix(), Store: store})
		}
	}
	return storeref.ClassCandidates(id, storeref.ClassRouting{
		Prefix:  prefix,
		Class:   e.graphStore(),
		Work:    e.work,
		Shadows: shadows,
	})
}

// claimByID runs the by-id claim MUTATION the way a class-aware caller has to:
// take the candidate list from the shared resolver, PROBE it in order, and write
// through the store that answered. An id the resolver does not claim falls back
// to the work fan-out `gc hook --claim` uses today — city store then rig stores,
// all work scopes — which is also the whole of the single-store path.
//
// It returns the store the claim landed in, so the caller can assert residence
// rather than assert that some write succeeded somewhere.
func (e splitEnv) claimByID(t *testing.T, id, assignee string) beads.Store {
	t.Helper()
	candidates := e.classCandidatesForID(id)
	if len(candidates) == 0 {
		candidates = append(candidates, e.work)
		for _, rig := range e.cfg.Rigs {
			if store := e.rigStores[rig.Name]; store != nil {
				candidates = append(candidates, store)
			}
		}
	}
	inProgress := "in_progress"
	for _, store := range candidates {
		if _, err := store.Get(id); err != nil {
			continue
		}
		if err := store.Update(id, beads.UpdateOpts{Assignee: &assignee, Status: &inProgress}); err != nil {
			t.Fatalf("claiming %s through the resolved store: %v", id, err)
		}
		return store
	}
	t.Fatalf("no candidate store held %s: the by-id claim had nowhere to run (candidates %d)", id, len(candidates))
	return nil
}

// assertClaimedIn asserts the claim on id is visible in want and in no other
// leg — the residence half of a routed mutation, which a write that merely
// "succeeded" does not establish.
func (e splitEnv) assertClaimedIn(t *testing.T, id, assignee string, want beads.Store) {
	t.Helper()
	legs := map[string]beads.Store{"work": e.work}
	if e.split {
		legs["class"] = e.class
	}
	for name, store := range legs {
		got, err := store.Get(id)
		if err != nil {
			continue
		}
		holds := strings.TrimSpace(got.Assignee) == assignee
		if sameStorePtr(store, want) && !holds {
			t.Errorf("%s store holds %s with assignee %q, want %q — the claim did not land in the store that owns the bead", name, id, got.Assignee, assignee)
		}
		if !sameStorePtr(store, want) && holds {
			t.Errorf("%s store holds %s claimed for %q; the mutation ran against a store that does not own the bead", name, id, assignee)
		}
	}
}

// reservedClassNamespace reports whether an id sits inside a reserved
// coordination class's id namespace.
//
// It applies the prefix+"-" SEGMENT rule — the same one storeref.PrefixOwner
// routes on — and deliberately NOT the config-free sling.BeadPrefix heuristic,
// which answers "gcg-wisp" for a gcg-wisp-0042 id and would call it unreserved.
// That divergence is real and is pinned as a negative by I5; a residence
// assertion built on the heuristic would report every wisp as a boundary
// violation.
func reservedClassNamespace(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, prefix := range config.ReservedClassPrefixes() {
		if strings.HasPrefix(id, strings.ToLower(prefix)+"-") {
			return true
		}
	}
	return false
}

// beadListHasID reports whether the list contains a bead with the given id.
func beadListHasID(list []beads.Bead, id string) bool {
	for _, b := range list {
		if b.ID == id {
			return true
		}
	}
	return false
}

// beadIndexOf returns the index of the bead with the given id, or -1.
func beadIndexOf(list []beads.Bead, id string) int {
	for i, b := range list {
		if b.ID == id {
			return i
		}
	}
	return -1
}
