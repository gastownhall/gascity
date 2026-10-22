package main

// Tests of the guard on the non-store claim paths (hook claims, gc bd and
// agent-script writes), and the wire-level tests that pin each guard to the
// path that runs it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// The adoption tier re-stamps from the CANONICAL holder, so the canonical
// row is what the guard judges: a stale query row naming an admitted holder
// must not hide a canonical holder the rig does not admit.
func TestHookRigGuardAdoptionChecksCanonicalRow(t *testing.T) {
	g := hrgGuard(t)
	cached := beads.Bead{ID: "bt-1", Status: "in_progress", Assignee: "beta/crew.lead"}
	var w hrgWrites
	ops := hrgOps(t, &w, g, []beads.Bead{cached})
	ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
		return beads.Bead{ID: id, Status: "in_progress", Assignee: shortIDB}, nil
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", hrgDir(g.cfg, "beta"), hookClaimOptions{
		Assignee:           "beta/crew.lead",
		RuntimeActor:       "beta/crew.lead",
		SessionID:          shortIDB,
		IdentityCandidates: []string{"beta/crew.lead", shortIDB},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if w.total() != 0 {
		t.Fatalf("writes ran on a canonically Foreign row: %+v (stderr=%s)", w, stderr.String())
	}
	if strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("a canonically Foreign row was adopted: %s", stdout.String())
	}
}

// A row in the guarded rig's store whose id prefix maps to no guarded rig
// (an imported id, an HQ-prefixed row, another city's prefix) is judged by
// the leg it was read from.
func TestHookRigGuardJudgesUnmappedPrefixByLeg(t *testing.T) {
	g := hrgGuard(t)
	for _, id := range []string{"or-77", "hq-77", "al-77"} {
		rows := []beads.Bead{{ID: id, Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}}
		var w hrgWrites
		ops := hrgOps(t, &w, g, rows)
		var stdout, stderr bytes.Buffer
		doHookClaim("query", hrgDir(g.cfg, "beta"), hookClaimOptions{
			Assignee:     "mayor",
			RouteTargets: []string{"beta/crew.worker"},
			JSON:         true,
		}, ops, &stdout, &stderr)
		if len(w.claims) != 0 {
			t.Errorf("%s on the guarded leg claimed by an unadmitted actor: %v", id, w.claims)
		}
		foreign := beads.Bead{ID: id, Status: "in_progress", Assignee: shortIDB}
		if kept := g.forLeg("beta", "").filterCandidates([]beads.Bead{foreign}, &stderr); len(kept) != 0 {
			t.Errorf("Foreign row %s kept on the guarded leg", id)
		}
	}
	// A subdirectory of the rig is the rig's leg too.
	if g.forLeg("", filepath.Join(hrgDir(g.cfg, "beta"), "work", "tree")).leg.rigName() != "beta" {
		t.Error("a directory inside the rig did not bind the rig's leg")
	}
	// A prefix naming a DIFFERENT guarded rig admits nothing on this leg.
	routed := beads.Bead{ID: "zz-1", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}
	if err := g.forLeg("beta", "").claimRefusal(routed, poolIDB); err == nil {
		t.Error("a row whose prefix names another guarded rig was admitted on the beta leg")
	}
	// Off a guarded leg, the prefix still decides.
	if err := g.forLeg("alpha", "").claimRefusal(beads.Bead{ID: "bt-9", Status: "open"}, "mayor"); err == nil {
		t.Error("a beta-prefixed row read from another leg escaped the prefix check")
	}
	if err := g.forLeg("alpha", "").claimRefusal(beads.Bead{ID: "or-9", Status: "open"}, "mayor"); err != nil {
		t.Errorf("an unguarded leg refused an unguarded row: %v", err)
	}
}

// claimHookWorkWithRunner binds the guard to each leg by the leg's rig name,
// whatever its directory.
func TestHookRigGuardBindsLegByStoreRig(t *testing.T) {
	g := hrgGuard(t)
	rows := []beads.Bead{{ID: "or-77", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}}
	var w hrgWrites
	ops := hrgOps(t, &w, g, rows)
	out, _ := json.Marshal(rows)
	run := func(string, string, []string) (string, error) { return string(out), nil }
	stores := []hookStore{{dir: "/elsewhere", rig: "beta"}}
	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("query", "/elsewhere", nil, stores, hookClaimOptions{
		Assignee:     "mayor",
		RouteTargets: []string{"beta/crew.worker"},
		JSON:         true,
	}, ops, run, func(string, error) {}, &stdout, &stderr)
	if len(w.claims) != 0 {
		t.Fatalf("a row on the beta leg was claimed by an unadmitted actor: %v (stderr=%s)", w.claims, stderr.String())
	}
}

// Both Foreign layers hold for an ADMITTED actor: the candidate filter drops
// the row, and claimRefusal refuses it if it ever got through.
func TestHookRigGuardForeignRowRefusedForAdmittedActor(t *testing.T) {
	g := hrgGuard(t)
	foreign := beads.Bead{ID: "bt-1", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "crew.worker")}
	if err := g.claimRefusal(foreign, poolIDB); err == nil || !strings.Contains(err.Error(), "its owner") {
		t.Fatalf("claimRefusal on a Foreign row for an admitted actor = %v", err)
	}
	var w hrgWrites
	ops := hrgOps(t, &w, g, []beads.Bead{foreign})
	var stdout, stderr bytes.Buffer
	doHookClaim("query", hrgDir(g.cfg, "beta"), hookClaimOptions{
		Assignee:     poolIDB,
		RouteTargets: []string{"crew.worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)
	if w.total() != 0 {
		t.Fatalf("writes ran on a Foreign row: %+v", w)
	}
	if !strings.Contains(stderr.String(), "gc hook --claim: skipping bt-1: rig \"beta\"") {
		t.Fatalf("the candidate filter did not drop the row:\n%s", stderr.String())
	}
}

func TestHookCityClaimOpsInstallsRigGuard(t *testing.T) {
	if newHookCity("/cities/b", hrgCity(t)).claimOps().RigGuard == nil {
		t.Fatal("claimOps for a city with a guarded rig installed no rig guard")
	}
}

// gc bd on a guarded scope is an allowlist.
func TestBdRigNamespaceAllowlist(t *testing.T) {
	held := beads.Bead{ID: "bt-4", Status: "in_progress", Assignee: "beta/crew.worker"}
	c := brnCheck(t, "mayor", brnOwned, brnForeign, brnPlain, held)
	for _, args := range [][]string{
		{"assign", "bt-2", "beta/crew.worker"},
		{"assign", "bt-1", "outsider"},
		{"unclaim", "bt-2", "--force"},
		{"unclaim", "bt-4"}, // "mayor" is not the holder
		{"close", "bt-1", "--continue"},
		{"done", "bt-2"},
		{"done"},
		{"duplicate", "bt-2", "--of", "bt-1"},
		{"supersede", "bt-2", "--with", "bt-1"},
		{"dismiss", "bt-2"},
		{"defer", "bt-2"},
		{"undefer", "bt-2"},
		{"set-state", "bt-2", "phase=x"},
		{"comment", "bt-2", "hello"},
		{"note", "bt-2", "hello"},
		{"comments", "add", "bt-2", "hello"},
		{"dep", "add", "bt-2", "bt-1"},
		{"dep", "remove", "bt-2", "bt-1"},
		{"label", "add", "bt-2", "gc:hold"},
		{"tag", "bt-2", "gc:hold"},
		{"reclaim"},
		{"mol", "burn", "bt-2"},
		{"mol", "pour", "f"},
		{"rename", "bt-2", "bt-99"},
		{"rename-prefix", "zz"},
		{"q", "quick capture"},
		{"todo", "x"},
		{"edit", "bt-1"},
		{"gate", "check"},
		{"compact"},
		{"prune"},
		{"purge"},
		{"config", "set", "k", "v"},
		{"sql", "select 1"},
		{"import", "rows.jsonl"},
		{"no-such-verb"},
		{"create", "--title", "x", "--deps", "blocks:bt-2"},
		{"--db", "/other", "close", "bt-1"},
		{"close", "bt-1", "--database=/other"},
		{"create", "--title", "x", "--repo", "/other"},
		{"done", "bt-1", "--mystery", "v"},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes the guard", args)
		}
	}
	c.actor = "beta/crew.worker"
	for _, args := range [][]string{
		{"show", "bt-2"},
		{"list", "--json"},
		{"ready"},
		{"search", "x"},
		{"dep", "list", "bt-2"},
		{"label", "list", "bt-2"},
		{"comments", "bt-2"},
		{"mol", "current"},
		{"link", "bt-1", "bt-2"},
		{"assign", "bt-1", "beta/crew.lead"},
		{"assign", "bt-1", poolIDB},
		{"unclaim", "bt-4"},
		{"done", "bt-1", "--reason=finished"},
		{"close", "bt-1", "--continue", "--no-auto"},
		{"dep", "add", "bt-1", "bt-2"}, // the dependent's side is this city's
		{"label", "add", "bt-1", "x"},
		{"comment", "bt-3", "hello"},
		{"update", "--help"},
	} {
		if _, msg, refused := c.check(args); refused {
			t.Errorf("gc bd %v refused: %q", args, msg)
		}
	}
}

func TestBdRigNamespaceCreateParsesFailClosed(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned)
	for _, args := range [][]string{
		{"create", "-aoutsider", "--title", "x"},
		{"create", "-fplan.md"},
		{"create", "--title", "x", "--metadata", `{"gc.deferred_assignee":"outsider"}`},
		{"create", "--title", "x", "--metadata", `{"gc.stranded_assignee":"gcx-17"}`},
		{"create", "--title", "x", "--metadata", `{}`, "--metadata", `{"k":"v"}`},
		{"create", "--title", "x", "--metadata={}", "--metadata", `{"k":"v"}`},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes the guard", args)
		}
	}
	if _, _, refused := c.check([]string{"update", "bt-1", "--set-metadata", "gc.deferred_assignee=outsider"}); !refused {
		t.Error("update writing an unadmitted gc.deferred_assignee passes the guard")
	}
	if _, msg, refused := c.check([]string{"create", "-t", "task", "--title", "x", "--metadata", `{"gc.deferred_assignee":"beta/crew.worker"}`}); refused {
		t.Errorf("an admitted create refused: %q", msg)
	}
}

// An agent-script write is checked like gc bd checks it: the row's class,
// the assignee, routes and the creation stamp, not only the actor.
func TestAgentScriptRigNamespaceChecksTheWrite(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	rows := map[string]beads.Bead{"bt-1": brnOwned, "bt-2": brnForeign}
	get := func(id string) (beads.Bead, error) {
		if b, ok := rows[id]; ok {
			return b, nil
		}
		return beads.Bead{}, beads.ErrNotFound
	}
	for _, arg := range []map[string]any{
		{"id": "bt-2", "status": "in_progress"},
		{"id": "bt-1", "assignee": "outsider"},
		{"id": "bt-1", "metadata": map[string]any{"gc.owner_namespace": "alpha/"}},
		{"id": "bt-1", "metadata": map[string]any{"gc.routed_to": "crew.worker"}},
		{"id": "bt-404", "status": "in_progress"},
	} {
		args, err := agentScriptBDUpdateArgs(arg, agentScriptContext{})
		if err != nil {
			t.Fatal(err)
		}
		if err := agentScriptRigNamespaceCheck(ns, get, args); !errors.Is(err, errRigNamespace) {
			t.Errorf("bd %v: err = %v, want a refusal", args, err)
		}
	}
	args, _ := agentScriptBDUpdateArgs(map[string]any{"id": "bt-1", "status": "in_progress"}, agentScriptContext{})
	if err := agentScriptRigNamespaceCheck(ns, get, args); err != nil {
		t.Errorf("bd %v on an Owned row refused: %v", args, err)
	}
	claim := []string{"update", "bt-1", "--claim", "--actor", "mayor"}
	if err := agentScriptRigNamespaceCheck(ns, get, claim); !errors.Is(err, errRigNamespace) {
		t.Errorf("claim as mayor: err = %v, want a refusal", err)
	}
}

// An unredirected agent-script write lands in the runner's own store, so
// the runner's rig judges it whatever the bead's prefix.
func TestAgentScriptRigNamespaceUsesRunnersRig(t *testing.T) {
	res, _ := rnsCityBResolver(t)
	cfg := hrgCity(t)
	rigAgent := &config.Agent{Name: "crew.worker", Dir: "beta"}
	if _, _, err := crossStoreClaimDir(cfg, rigAgent, "or-77", "mayor", "beta", res); !errors.Is(err, errRigNamespace) {
		t.Fatalf("an unmapped prefix in the runner's guarded rig: err = %v, want a refusal", err)
	}
	if _, _, err := crossStoreClaimDir(cfg, rigAgent, "or-77", "mayor", "alpha", res); err != nil {
		t.Fatalf("an unmapped prefix in an unguarded rig refused: %v", err)
	}
}

func TestAgentScriptConfigLoadErrorRefuses(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\nname = \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY", cityDir)
	_, _, ok, err := agentScriptCrossStoreBeadEnv("bt-1", []string{"update", "bt-1", "--claim"})
	if err == nil || ok {
		t.Fatalf("ok=%v err=%v, want an error for a city config that does not load", ok, err)
	}
}

// The CLI resolver learns both of the city's session id prefixes: the HQ
// prefix (records migrated from the city store) and the sessions store's own
// prefix, which the bead policy layer hides.
func TestRigNamespaceCLIResolverPrefixes(t *testing.T) {
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY", "")
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "b", Prefix: "hqp"}}
	hqID := "hqp-session-" + tokenB
	engine := rnsNewCountingStore(
		rnsSessionBead(hqID, templateB, ""),
		rnsSessionBead(poolIDB, templateB, poolNameB),
	)
	wrapped := wrapStoreWithBeadPolicies(engine, cfg)
	if _, ok := wrapped.(interface{ IDPrefix() string }); ok {
		t.Fatal("fixture: the policy layer exposes IDPrefix; the test would not reach the unwrap")
	}
	res := newCityCLISessionResolver(cityPath, cfg, func() (beads.SessionStore, error) { return cliStores(cityPath, wrapped).Sessions(), nil })
	for _, id := range []string{hqID, poolIDB} {
		rec, ok, err := res.lookupOwnSession(id)
		if err != nil || !ok || rec.ID != id {
			t.Errorf("lookupOwnSession(%q) = %+v, %v, %v; want the record", id, rec, ok, err)
		}
	}
}

func TestDecorateDynamicFragmentRecipeRigNamespace(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		Rigs:      []config.Rig{{Name: "beta", Prefix: "bt", Federation: config.RigFederationRig}},
		Agents:    []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(3)}},
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "")
	source := beads.Bead{ID: "bt-source", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:     "worker",
		beadmeta.RootStoreRefMetadataKey: "rig:beta",
	}}
	fragment := &formula.FragmentRecipe{Name: "expansion", Steps: []formula.RecipeStep{{ID: "expansion.work", Metadata: map[string]string{}}}}
	err := decorateDynamicFragmentRecipe(fragment, source, store, cfg.Workspace.Name, "", cfg)
	if !errors.Is(err, agentutil.ErrRigNamespaceRoute) {
		t.Fatalf("decorateDynamicFragmentRecipe into a guarded rig with a city route: err = %v, want a refusal", err)
	}
}

// doBd runs the guard before bd: a refused write never reaches bd.
func TestDoBdRigNamespaceRefusesBeforeBd(t *testing.T) {
	capture, cityDir := bdSQLRefusalCityDir(t, "")
	rigDir := filepath.Join(cityDir, "beta")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: bt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(cityDir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[[rigs]]\nname = \"beta\"\npath = \"" + rigDir + "\"\nprefix = \"bt\"\nfederation = \"rig\"\nnamespace = \"exclusive\"\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for _, args := range [][]string{
		{"--rig", "beta", "sql", "delete from issues"},
		{"--rig", "beta", "create", "--title", "x", "--assignee", "mayor"},
	} {
		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code == 0 {
			t.Errorf("doBd(%v) exited 0; stderr=%q", args, stderr.String())
		}
		if !strings.Contains(stderr.String(), `rig "beta" (federation = "rig")`) {
			t.Errorf("doBd(%v): stderr lacks the guard refusal: %q", args, stderr.String())
		}
	}
	if data, err := os.ReadFile(capture); err == nil {
		t.Fatalf("bd ran despite the refusal: %q", data)
	}
}

// The scope decision is by store identity (the shared rigNamespaceForScope):
// a worktree redirecting into a guarded rig's store is that rig's scope, and
// an unguarded rig sharing a guarded rig's store makes both scopes admit
// nothing, never "unguarded".
func TestRigNamespaceScopeFollowsRedirectIntoGuardedStore(t *testing.T) {
	root := t.TempDir()
	beta := filepath.Join(root, "beta")
	alpha := filepath.Join(root, "alpha")
	tree := filepath.Join(root, "worktrees", "w1")
	for _, d := range []string{filepath.Join(beta, ".beads"), filepath.Join(alpha, ".beads"), filepath.Join(tree, ".beads"), filepath.Join(tree, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	redirect := func(d string) {
		if err := os.WriteFile(filepath.Join(d, ".beads", "redirect"), []byte(filepath.Join(beta, ".beads")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	redirect(tree)
	g := rnsGuardedRig("beta")
	g.Path = beta
	cfg := &config.City{Rigs: []config.Rig{g, {Name: "alpha", Path: alpha}}}
	if ns := guardedRigNamespaceForDir(cfg, root, filepath.Join(tree, "sub"), nil); ns.rigName() != "beta" {
		t.Errorf("worktree redirecting into beta's store: ns = %q, want beta", ns.rigName())
	}
	if ns := guardedRigNamespaceForRigName(cfg, root, "alpha", nil); ns != nil {
		t.Errorf("unguarded rig alpha with its own store got namespace %q", ns.rigName())
	}
	if ns := rigNamespaceForScopeOrDeny(cfg, root, root, nil); ns != nil {
		t.Errorf("the city root got namespace %q", ns.rigName())
	}
	if c := newBdRigNamespaceCheck(cfg, root, filepath.Join(tree, "sub"), nil); c == nil || c.ns.rigName() != "beta" {
		t.Fatalf("gc bd in the redirected worktree got no beta check: %+v", c)
	}

	// alpha now shares beta's store: both scopes admit nothing.
	redirect(alpha)
	for _, ns := range []*rigNamespace{
		rigNamespaceForScopeOrDeny(cfg, root, alpha, nil),
		guardedRigNamespaceForRigName(cfg, root, "alpha", nil),
		guardedRigNamespaceForRigName(cfg, root, "beta", nil),
	} {
		if ns == nil || ns.namespace() != "" {
			t.Errorf("a shared guarded store: ns = %+v, want deny-all", ns)
		}
	}
	c := newBdRigNamespaceCheck(cfg, root, alpha, nil)
	if c == nil {
		t.Fatal("gc bd in a rig sharing a guarded store got no check")
	}
	if _, _, refused := c.check([]string{"create", "--title", "x"}); !refused {
		t.Error("a create into a shared guarded store passes")
	}
}
