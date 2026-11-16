package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
)

// TestSlingDepsHandlesWrapTheStoresTheyReplace pins the `gc sling` deps
// (typed-stores T6d): the work handle cliStores mints over the scope store the
// sling opened wraps that store, and its graph handle wraps the store
// resolveGraphStore returned for it, by identity, at city and rig scope, on a
// city that relocates nothing and on a split one. On the first the graph store
// is the scope store itself; on the second it is the binding.
func TestSlingDepsHandlesWrapTheStoresTheyReplace(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "single-store"
		bindingRoot := ""
		if split {
			name = "split"
			bindingRoot = filepath.Join(t.TempDir(), "store")
		}
		t.Run(name, func(t *testing.T) {
			cityPath := oneShotCLICity(t, bindingRoot)
			city, err := openStoreAtForCity(cityPath, cityPath)
			if err != nil {
				t.Fatalf("open the city store: %v", err)
			}
			binding, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassGraph)
			if relocated != split {
				t.Fatalf("graph relocated = %t, want %t: the fixture is not the shape it names", relocated, split)
			}
			for scope, store := range map[string]beads.Store{"city": city, "rig": beads.NewMemStore()} {
				cs := cliStores(cityPath, store)
				if got, want := cs.Work().Store, splitStrictWorkView(store); got != want {
					t.Errorf("%s scope: work handle wraps %T(%p), want the scope store %T(%p)", scope, got, got, want, want)
				}
				oldGraph := resolveGraphStore(cliStorageRoutes(cityPath), store, nil, cityPath, nil)
				if got := cs.Graph().Store; got != oldGraph {
					t.Errorf("%s scope: graph handle wraps %T(%p), want resolveGraphStore's %T(%p)", scope, got, got, oldGraph, oldGraph)
				}
				if got, want := beads.SameStore(cs.Graph(), binding), split; got != want {
					t.Errorf("%s scope: graph handle over the binding = %t, want %t", scope, got, want)
				}
			}
		})
	}
}

// TestCmdSlingConvoyRoutesItsTracksOnlyMembers pins R10 at the CLI door: `gc
// sling` hands the batch its work store as the querier, so a convoy's members
// are read through convoy.MembersIn, which follows its tracks edges. A member a
// convoy tracks but does not parent is routed; through a querier's DB children
// alone it would not be seen.
func TestCmdSlingConvoyRoutesItsTracksOnlyMembers(t *testing.T) {
	cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, nil)
	writeTestFileStoreBeads(t, rigDir, []beads.Bead{
		{ID: "fo-cvy", Title: "convoy", Type: "convoy", Status: "open", Metadata: map[string]string{}},
		{ID: "fo-member", Title: "tracked member", Type: "task", Status: "open", Metadata: map[string]string{}},
	})
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open the rig store: %v", err)
	}
	if err := rigStore.DepAdd("fo-cvy", "fo-member", "tracks"); err != nil {
		t.Fatalf("tracking the member: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{"foundations/worker-a", "fo-cvy"},
		false, false, false,
		"", nil, "",
		true, false, false, "",
		false, false, false,
		"", "",
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("gc sling of a convoy = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	rigStore, err = openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("reopen the rig store: %v", err)
	}
	member, err := rigStore.Get("fo-member")
	if err != nil {
		t.Fatalf("Get(fo-member): %v", err)
	}
	if got := member.Metadata[beadmeta.RoutedToMetadataKey]; got != "foundations/worker-a" {
		t.Errorf("tracks-only member gc.routed_to = %q, want foundations/worker-a; stdout=%s", got, stdout.String())
	}
}

// TestCheckBeadStateWithZeroDepsDoesNotPanic pins checkBeadState's zero
// SlingDeps (R11): its work, graph and sessions handles are zero, and a zero
// handle boxed into an interface is non-nil and panics on its first read. A
// routed bead, claimed by a session bead id, reaches the pool-claim and
// convoy-recovery reads, which must skip the absent stores.
func TestCheckBeadStateWithZeroDepsDoesNotPanic(t *testing.T) {
	store := beads.NewMemStore()
	pool := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(3)}
	b, err := store.Create(beads.Bead{
		Title:    "claimed",
		Assignee: "gc-session-1",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "polecat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("checkBeadState with zero deps panicked: %v", r)
		}
	}()
	result := checkBeadState(store, b.ID, pool)
	if result.Idempotent {
		t.Errorf("checkBeadState = %+v: with no sessions store the claimant is not recognized as the pool's own", result)
	}
	// Unclaimed routed work reaches the convoy-recovery read of the work
	// store. A live auto-convoy tracks it in the querier's store, but the zero
	// deps have no work store to read it in, so finalize must re-run.
	unclaimed, err := store.Create(beads.Bead{Title: "routed", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "polecat"}})
	if err != nil {
		t.Fatal(err)
	}
	convoy, err := store.Create(beads.Bead{Title: sling.AutoConvoyRootTitle(unclaimed.ID), Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(convoy.ID, unclaimed.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	if result := checkBeadState(store, unclaimed.ID, pool); result.Idempotent {
		t.Errorf("checkBeadState = %+v: the zero deps read no work store, so the convoy is not seen and finalize must re-run", result)
	}
}

// TestCLIBeadRouterWithNoStoreRefuses pins cliBeadRouter's absent store: with
// no store named by the request and a zero work store it refuses instead of
// writing through a zero handle.
func TestCLIBeadRouterWithNoStoreRefuses(t *testing.T) {
	err := cliBeadRouter{deps: &slingDeps{}}.Route(t.Context(), sling.RouteRequest{BeadID: "gc-1", Target: "worker"})
	if err == nil || !strings.Contains(err.Error(), "requires a store") {
		t.Fatalf("Route with no store = %v, want the requires-a-store refusal", err)
	}
}

// TestCmdSlingRigScopeReslingReadsTheClaimantInTheCityStore pins the `gc
// sling` door's sessions store at rig scope (OD-3c): a rig store holds no
// session beads, so a rig-scope sling reads them in the city store it opens.
// A re-sling of rig work its own pool session claimed under its session bead
// id is then the idempotent no-op it is at city scope, on a city that
// relocates nothing; read through the rig store the claimant was missed and
// the work was routed again beside the session holding it.
func TestCmdSlingRigScopeReslingReadsTheClaimantInTheCityStore(t *testing.T) {
	cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, nil)
	cityToml := "[workspace]\nname = \"demo\"\n\n[[rigs]]\nname = \"foundations\"\npath = \"foundations\"\nprefix = \"fo\"\n\n[[agent]]\nname = \"worker-a\"\ndir = \"foundations\"\nmax_active_sessions = 3\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	writeTestFileStoreBeads(t, cityDir, []beads.Bead{{
		ID:       "gc-sess-1",
		Title:    "pool session",
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"template": "foundations/worker-a", "pool_managed": "true"},
	}})
	writeTestFileStoreBeads(t, rigDir, []beads.Bead{
		{ID: "fo-held", Title: "held work", Type: "task", Status: "in_progress", Assignee: "gc-sess-1", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "foundations/worker-a"}},
		{ID: "fo-cvy", Title: sling.AutoConvoyRootTitle("fo-held"), Type: "convoy", Status: "open", Metadata: map[string]string{}},
	})
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open the rig store: %v", err)
	}
	if err := rigStore.DepAdd("fo-cvy", "fo-held", "tracks"); err != nil {
		t.Fatalf("tracking the held work: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{"foundations/worker-a", "fo-held"},
		false, false, false,
		"", nil, "",
		true, false, false, "",
		false, false, false,
		"", "",
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("gc sling = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "already routed") || strings.Contains(stderr.String(), "assigned to") {
		t.Errorf("re-sling of work pool session gc-sess-1 holds: stdout=%q stderr=%q, want an idempotent skip", stdout.String(), stderr.String())
	}
}

// heldPoolWork writes, in work, nfill filler beads and then a bead pool
// worker template's session sessionID holds, with a live auto-convoy, so a
// re-sling of it is idempotent only when the claimant is recognized.
func heldPoolWork(t *testing.T, work beads.Store, template, sessionID string, nfill int) beads.Bead {
	t.Helper()
	for i := 0; i < nfill; i++ {
		if _, err := work.Create(beads.Bead{Title: "filler", Type: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	held, err := work.Create(beads.Bead{Title: "held work", Type: "task", Status: "in_progress", Assignee: sessionID, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template}})
	if err != nil {
		t.Fatal(err)
	}
	convoy, err := work.Create(beads.Bead{Title: sling.AutoConvoyRootTitle(held.ID), Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(convoy.ID, held.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	return held
}

// runCmdSling runs `gc sling target beadID` and requires it to succeed.
func runCmdSling(t *testing.T, target, beadID string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cmdSling(
		[]string{target, beadID},
		false, false, false,
		"", nil, "",
		true, false, false, "",
		false, false, false,
		"", "",
		&out, &errb,
	)
	if code != 0 {
		t.Fatalf("gc sling %s %s = %d, want 0; stdout=%s stderr=%s", target, beadID, code, out.String(), errb.String())
	}
	return out.String(), errb.String()
}

// TestCmdSlingOnASplitCityReadsTheClaimantInTheSessionsBinding pins the `gc
// sling` door's sessions store on a split city (OD-3c): the scope's sessions
// handle is the binding, where the pool's session bead lives, so a re-sling of
// work its own pool session holds is the idempotent skip. Read through the
// work store the claimant was missed and the work was routed again.
func TestCmdSlingOnASplitCityReadsTheClaimantInTheSessionsBinding(t *testing.T) {
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	writeSplitSlingFormulaCity(t, cityPath)
	sessions, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassSessions)
	if !relocated {
		t.Fatal("the sessions class is not relocated; the fixture is not a split city")
	}
	own, err := sessions.Create(beads.Bead{
		Title:    "pool session",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"template": "worker", "pool_managed": "true"},
	})
	if err != nil {
		t.Fatalf("creating the pool session in the binding: %v", err)
	}
	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("open the work store: %v", err)
	}
	// Fillers keep the work ids clear of the session's, so the claimant id
	// cannot resolve to a work bead by coincidence.
	held := heldPoolWork(t, work, "worker", own.ID, 5)

	stdout, stderr := runCmdSling(t, "worker", held.ID)
	if !strings.Contains(stdout, "already routed") || strings.Contains(stderr, "assigned to") {
		t.Errorf("re-sling of work pool session %s holds: stdout=%q stderr=%q, want an idempotent skip", own.ID, stdout, stderr)
	}
}

// breakCityLedger makes the city's file store unreadable.
func breakCityLedger(t *testing.T, cityDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "beads.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCmdSlingRigScopeDegradesWhenTheCityStoreDoesNotOpen pins the rig-scope
// city-store open's failure: the sling's work is in the rig ledger, so a city
// ledger that does not open leaves the sling routing as it did before the
// sessions store, reading sessions in the rig store, with a warning, instead
// of refusing the sling.
func TestCmdSlingRigScopeDegradesWhenTheCityStoreDoesNotOpen(t *testing.T) {
	_, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, nil)
	writeTestFileStoreBeads(t, rigDir, []beads.Bead{{ID: "fo-work", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{}}})
	breakCityLedger(t, filepath.Dir(rigDir))

	stdout, stderr := runCmdSling(t, "foundations/worker-a", "fo-work")
	if !strings.Contains(stderr, "opening the city store for session lookups") {
		t.Errorf("stderr = %q, want the city-store warning", stderr)
	}
	rigStore, err := openStoreAtForCity(rigDir, filepath.Dir(rigDir))
	if err != nil {
		t.Fatalf("open the rig store: %v", err)
	}
	routed, err := rigStore.Get("fo-work")
	if err != nil {
		t.Fatal(err)
	}
	if got := routed.Metadata[beadmeta.RoutedToMetadataKey]; got != "foundations/worker-a" {
		t.Errorf("gc.routed_to = %q, want foundations/worker-a; stdout=%s", got, stdout)
	}
}

// TestCmdSlingRigScopeOnASplitCityOpensNoCityStore pins that a rig-scope
// sling on a city that relocates the sessions class takes its sessions store
// from the binding and opens no city store for it: with the city ledger
// broken, the sling routes and gives no city-store warning.
func TestCmdSlingRigScopeOnASplitCityOpensNoCityStore(t *testing.T) {
	cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, nil)
	writeOneShotCityTOML(t, cityDir, filepath.Join(t.TempDir(), "store"))
	f, err := os.OpenFile(filepath.Join(cityDir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[[rigs]]\nname = \"foundations\"\npath = \"foundations\"\nprefix = \"fo\"\n\n[[agent]]\nname = \"worker-a\"\ndir = \"foundations\"\n"); err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)
	writeTestFileStoreBeads(t, rigDir, []beads.Bead{{ID: "fo-work", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{}}})
	if _, relocated := cliStorageRoutes(cityDir).storeFor(coordclass.ClassSessions); !relocated {
		t.Fatal("the sessions class is not relocated; the fixture is not a split city")
	}
	breakCityLedger(t, cityDir)

	_, stderr := runCmdSling(t, "foundations/worker-a", "fo-work")
	if strings.Contains(stderr, "opening the city store for session lookups") {
		t.Errorf("stderr = %q: a split city's sessions store is the binding, so no city store is opened for it", stderr)
	}
}

// TestCLISessionStoreEmitsOnlyOverTheRawEngine is why `gc sling` nudges over
// the raw scope store (TestCmdSlingNudgesOverTheRawScopeStore): on a city that
// relocates nothing, cliSessionStore puts the one-shot emitter on a SQLite or
// native-Dolt engine by reading the store's own type (withMainStoreEmission),
// so a session write through it announces itself to the controller's cache.
// Over a class handle of the same store it finds no engine and the write is
// silent. When withMainStoreEmission sees through class layers (T9) the second
// half of this test fails, and the nudge may take the work handle.
func TestCLISessionStoreEmitsOnlyOverTheRawEngine(t *testing.T) {
	if splitStrictSeam != nil {
		t.Skip("the split-strict gate hands cliSessionStore's caller a projection view, which hides the engine from the emitter on either input")
	}
	for name, wrap := range map[string]func(beads.Store) beads.Store{
		"the raw store":   func(s beads.Store) beads.Store { return s },
		"its work handle": func(s beads.Store) beads.Store { return beadstest.Work(s) },
	} {
		cityPath := t.TempDir()
		writeOneShotCityTOML(t, cityPath, "")
		captureCLIStorageStderr(t)
		resetCLIStorageRoutes(t)
		cfg := &config.City{}
		engine, err := beads.OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		sess := cliSessionStore(wrap(wrapStoreWithBeadPolicies(engine, cfg)), cfg, cityPath)
		created, err := sess.Create(beads.Bead{Title: "s", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"template": "worker"}})
		if err != nil {
			t.Fatalf("%s: creating a session bead: %v", name, err)
		}
		announced := 0
		for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
			if evt.Subject == created.ID && evt.Type == events.BeadCreated {
				announced++
			}
		}
		if want := map[bool]int{true: 1, false: 0}[name == "the raw store"]; announced != want {
			t.Errorf("%s: the session create appended %d bead.created row(s), want %d", name, announced, want)
		}
	}
}

// TestCmdSlingNudgesOverTheRawScopeStore pins the `gc sling` nudge's store:
// cmdSlingWithJSON's nudge hands doSlingNudge the scope store it opened, raw,
// not deps.Store, its work handle, whose session writes would stop emitting
// (TestCLISessionStoreEmitsOnlyOverTheRawEngine).
func TestCmdSlingNudgesOverTheRawScopeStore(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "cmd_sling.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "cmdSlingWithJSON" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "doSlingNudge" {
				return true
			}
			calls++
			if arg, ok := call.Args[5].(*ast.Ident); !ok || arg.Name != "store" {
				t.Errorf("cmdSlingWithJSON's nudge hands doSlingNudge %s, want store, the raw scope store openSlingStore opened", types.ExprString(call.Args[5]))
			}
			return true
		})
	}
	if calls != 1 {
		t.Fatalf("cmdSlingWithJSON calls doSlingNudge %d time(s), want 1", calls)
	}
}
