//go:build !windows

package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestGcBdSplitRound4Refusals covers the #8333 allowlist round-2 review:
// --graph is refused, a parent's inherited labels are judged, init --prefix
// may not take a reserved namespace, and an external: endpoint is not a
// cross-store edge.
func TestGcBdSplitRound4Refusals(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"create --graph", []string{"create", "--graph", "plan.json"}, beads.Bead{}, bdReadNotFound, true, "--graph", false},
		{"create --graph enterprise", []string{"create", "--graph", "plan.json"}, beads.Bead{}, bdReadNotFound, true, "--graph", true},
		{"init --prefix in a reserved namespace", []string{"init", "--prefix", "gcg"}, beads.Bead{}, bdReadNotFound, true, "reserved namespace", false},
		{"init --prefix with its dash", []string{"init", "--prefix", "gcg-"}, beads.Bead{}, bdReadNotFound, true, "reserved namespace", false},
		{"init --prefix work", []string{"init", "--prefix", "demo"}, beads.Bead{}, bdReadNotFound, false, "", false},
	})
	runEdgeCases(t, []edgeCase{
		{"dep add to an external: ref", []string{"dep", "add", "demo-1", "external:gascity:capability"}, false, ""},
		{"create --deps external:", []string{"create", "--title", "t", "--deps", "blocks:external:other:cap"}, false, ""},
		{"external: does not excuse a binding id beside it", []string{"create", "--title", "t", "--deps", "external:o:c,gcg-x"}, true, "never written"},
	})
}

// TestGcBdSplitCreateParentInheritsLabels: bd create --parent copies the
// parent's labels onto the child, so they are judged with the child's own,
// unless --no-inherit-labels.
func TestGcBdSplitCreateParentInheritsLabels(t *testing.T) {
	parent := beads.Bead{ID: "demo-1", Type: "task", Labels: []string{"gc:session"}}
	deps := splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) {
		if id == "demo-1" {
			return parent, bdReadFound
		}
		return beads.Bead{}, bdReadNotFound
	})
	g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"create", "--title", "child", "--parent", "demo-1"}, deps)
	if !strings.Contains(g.refusal, "sessions-class") {
		t.Fatalf("an inherited gc:session label was not judged: %q", g.refusal)
	}
	g = bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"create", "--title", "child", "--parent", "demo-1", "--no-inherit-labels"}, deps)
	if g.refusal != "" {
		t.Fatalf("--no-inherit-labels was still judged on the parent's labels: %q", g.refusal)
	}

	// The edge rule finds a parent in any of the city's work stores, but bd
	// copies the labels of the parent its own store holds: a parent this
	// store's read does not find exactly is refused, never judged as one
	// with no labels.
	deps = splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) {
		switch id {
		case "demo-2":
			return beads.Bead{}, bdReadFailed
		case "demo-3":
			return beads.Bead{}, bdReadCollision
		}
		return beads.Bead{}, bdReadNotFound
	})
	deps.endpoints = func(ids []string) map[string]bdReadResult {
		out := map[string]bdReadResult{}
		for _, id := range ids {
			out[id] = bdReadFound
		}
		return out
	}
	for parentID, want := range map[string]string{"demo-2": "could not read", "demo-3": "a short or partial id", "demo-4": `no bead "demo-4"`} {
		g = bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"create", "--title", "child", "--parent", parentID}, deps)
		if !strings.Contains(g.refusal, want) {
			t.Errorf("create --parent %s: refusal = %q, want %q", parentID, g.refusal, want)
		}
		g = bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"create", "--title", "child", "--parent", parentID, "--no-inherit-labels"}, deps)
		if g.refusal != "" {
			t.Errorf("create --parent %s --no-inherit-labels inherits nothing but was refused: %q", parentID, g.refusal)
		}
	}
	if g = bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"create", "--title", "child", "--parent", ""}, deps); g.refusal != "" {
		t.Errorf("create --parent \"\" names no parent but was refused: %q", g.refusal)
	}
}

// TestGcBdSplitDeleteRefusesABindingDependency: deleting a work bead a
// class-binding bead depends on is refused (the binding's edge would
// dangle), positionally or through --from-file; --gc-allow-dangling-edges
// overrides, and gc strips it before bd runs.
func TestGcBdSplitDeleteRefusesABindingDependency(t *testing.T) {
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "ids.txt"), []byte("# doomed\ndemo-2\n\ndemo-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) { return beads.Bead{ID: id, Type: "task"}, bdReadFound })
	var asked []string
	deps.bindingDependents = func(ids []string) (map[string][]string, error) {
		asked = append(asked, ids...)
		return map[string][]string{"demo-1": {"gcg-gate1"}}, nil
	}
	for _, argv := range [][]string{
		{"delete", "demo-1"},
		{"delete", "demo-2", "demo-1", "--force"},
		{"delete", "--from-file", "ids.txt", "--force"},
	} {
		g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, deps)
		if !strings.Contains(g.refusal, "gcg-gate1") || !strings.Contains(g.refusal, bdDeleteAllowDanglingFlag) {
			t.Errorf("gc bd %s: refusal = %q, want one naming the binding dependent and the override", strings.Join(argv, " "), g.refusal)
		}
	}
	if !slicesContainsString(asked, "demo-2") {
		t.Errorf("--from-file ids were not checked: %v", asked)
	}
	g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"delete", "demo-1", bdDeleteAllowDanglingFlag, "--force"}, deps)
	if g.refusal != "" {
		t.Fatalf("the override was refused: %q", g.refusal)
	}
	if slicesContainsString(g.args, bdDeleteAllowDanglingFlag) {
		t.Errorf("gc's own flag reached bd: %v", g.args)
	}
	if g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"delete", "demo-3"}, deps); g.refusal != "" {
		t.Errorf("deleting a bead nothing in the binding depends on was refused: %q", g.refusal)
	}

	// Round-3 must-fix 3: every id must name exactly one bead (real bd 1.3.2
	// deleted tt-vrt for `bd delete vrt --force`), the override included,
	// and --cascade is refused.
	exact := splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) {
		switch id {
		case "demo-1":
			return beads.Bead{ID: id, Type: "task"}, bdReadFound
		case "vrt":
			return beads.Bead{}, bdReadNotFound
		case "dem":
			return beads.Bead{}, bdReadCollision
		}
		return beads.Bead{}, bdReadFailed
	})
	for argv, want := range map[string]string{
		"delete vrt --force":                              "no bead \"vrt\"",
		"delete dem --force":                              "a short or partial id",
		"delete demo-1 vrt --force":                       "no bead \"vrt\"",
		"delete vrt --force " + bdDeleteAllowDanglingFlag: "no bead \"vrt\"",
		"delete demo-1 --cascade --force":                 "--cascade",
		"delete demo-x":                                   "could not read",
	} {
		g := bdClassFlipGuard(splitGuardTestCity(), scope, strings.Fields(argv), exact)
		if !strings.Contains(g.refusal, want) {
			t.Errorf("gc bd %s: refusal = %q, want %q", argv, g.refusal, want)
		}
	}
}

// TestBdOpenCrossStoreGatesNaming: an open S3b drain gate whose
// gc.cross_store_blocker names a work bead holds that bead; a closed one, or
// one naming another bead, does not.
func TestBdOpenCrossStoreGatesNaming(t *testing.T) {
	store := beads.NewMemStore()
	mk := func(blocker string, closed bool) string {
		b, err := store.Create(beads.Bead{Title: "gate", Type: "gate", Labels: []string{bdCrossStoreGateLabel}, Metadata: map[string]string{beadmeta.CrossStoreBlockerMetadataKey: blocker}})
		if err != nil {
			t.Fatal(err)
		}
		if closed {
			if err := store.Close(b.ID); err != nil {
				t.Fatal(err)
			}
		}
		return b.ID
	}
	open := mk("demo-1", false)
	// An unlabeled bead carrying the key is not a gate (#8360's query).
	if _, err := store.Create(beads.Bead{Title: "not a gate", Metadata: map[string]string{beadmeta.CrossStoreBlockerMetadataKey: "demo-1"}}); err != nil {
		t.Fatal(err)
	}
	mk("demo-1", true)
	mk("demo-2", false)
	got, err := bdOpenCrossStoreGatesNaming(store, "demo-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != open {
		t.Fatalf("open gates naming demo-1 = %v, want [%s]", got, open)
	}
}

// TestReaperPurgeAgeStaysAboveTheGateSweep pins the age gc holds gc bd purge
// and gc bd prune to on a split city (bdSweepMinAge): it stays far above the
// gate-sweep interval that releases a class-binding gate once its work
// blocker closes, and the core reaper's default ages for its purge
// (PURGE_AGE) and its session prune (SESSION_PURGE_AGE) meet it, so the
// reaper's calls at those ages pass the judge on every pinned build.
func TestReaperPurgeAgeStaysAboveTheGateSweep(t *testing.T) {
	sweep := readBundledPackFileForTest(t, "core", "orders/gate-sweep.toml")
	m := regexp.MustCompile(`(?m)^interval\s*=\s*"([^"]+)"`).FindStringSubmatch(sweep)
	if m == nil {
		t.Fatal("gate-sweep.toml has no interval")
	}
	interval, err := time.ParseDuration(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if bdSweepMinAge < 1000*interval {
		t.Errorf("the purge and prune age floor %s is not well above the gate-sweep interval %s (want >= 1000x)", bdSweepMinAge, interval)
	}
	reaper := readBundledPackFileForTest(t, "core", "assets/scripts/reaper.sh")
	ages := map[string]string{}
	for _, name := range []string{"PURGE_AGE", "SESSION_PURGE_AGE"} {
		m := regexp.MustCompile(`(?m)^` + name + `="\$\{GC_REAPER_` + name + `:-([0-9a-z]+)\}"`).FindStringSubmatch(reaper)
		if m == nil {
			t.Fatalf("reaper.sh has no %s default", name)
		}
		if age, err := bdOlderThanAge(m[1]); err != nil || age < bdSweepMinAge {
			t.Errorf("reaper %s defaults to %s (%v), under the split-city purge and prune floor %s", name, m[1], err, bdSweepMinAge)
		}
		ages[name] = m[1]
	}
	// The reaper's two calls, as reap_scope_purge and the session prune
	// build them, at those ages.
	for _, built := range []string{
		`(purge "$PURGE_PLANE_FLAG" --older-than "$PURGE_AGE" --json)`,
		`(prune --pattern "$SESSION_BEAD_PATTERN" --older-than "$SESSION_PURGE_AGE")`,
	} {
		if !strings.Contains(reaper, built) {
			t.Fatalf("reaper.sh no longer builds %s; update the calls this test judges", built)
		}
	}
	calls := [][]string{
		{"purge", "--wisps-plane", "--older-than", ages["PURGE_AGE"], "--json", "--force", "--limit", "500"},
		{"purge", "--wisps-plane", "--older-than", ages["PURGE_AGE"], "--json", "--dry-run"},
		{"prune", "--pattern", "gm-*", "--older-than", ages["SESSION_PURGE_AGE"], "--force", "--json"},
		{"prune", "--pattern", "gm-*", "--older-than", ages["SESSION_PURGE_AGE"], "--dry-run", "--json"},
	}
	for _, inv := range bdflags.Inventories() {
		deps := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadFailed })
		pinned := inv
		deps.inventory = func() (*bdflags.Inventory, error) { return pinned, nil }
		for _, argv := range calls {
			if g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), argv, deps); g.refusal != "" {
				t.Errorf("%s: the reaper's gc bd %s is refused on a split city: %s", inv.Name, strings.Join(argv, " "), g.refusal)
			}
		}
	}
}

// TestGcBdRefusalsNeverAdviseRawBd is review must-fix 2: no refusal tells
// the operator to run bd itself, which bypasses every guard. It checks the
// tables, every string literal of the files that write refusals (cobra
// Short, Long and Example included), and the documents the refusals point at.
func TestGcBdRefusalsNeverAdviseRawBd(t *testing.T) {
	advice := regexp.MustCompile("(?i)(\\b(run|use|try|call|invoke|via|with|through)\\s+`?(raw\\s+|plain\\s+|the\\s+)?bd(\\s|`|$|\\.|,))|bd directly|raw bd|bd itself|outside gc|bypass")
	for _, bad := range []string{
		"If you need auto-export behavior, invoke bd directly.",
		"stop the city and run bd init in the scope",
		"use `bd backup restore`",
	} {
		if !advice.MatchString(bad) {
			t.Fatalf("the raw-bd advice pattern misses %q", bad)
		}
	}
	check := func(where, s string) {
		if advice.MatchString(s) {
			t.Errorf("%s advises raw bd: %q", where, s)
		}
	}
	for path, r := range bdSplitRefusals {
		check("refusal "+path, r.why+" "+r.alt)
	}
	for alias, why := range bdSplitRefusedAliases {
		check("alias "+alias, why)
	}
	for flag, why := range bdSplitRedirectFlags {
		check("redirect "+flag, why)
	}
	// The documents the refusals point at.
	for _, rel := range []string{"docs/runbooks/split-storage-classes.md", "examples/bd/dolt/docs/journal-corruption-recovery.md"} {
		doc, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(doc)
		if _, section, ok := strings.Cut(text, "## gc bd on split cities"); ok {
			if next := strings.Index(section, "\n## "); next >= 0 {
				section = section[:next]
			}
			text = section
		}
		// The split-city section of the runbook, and every paragraph of the
		// recovery guide that speaks to a split city.
		for i, para := range strings.Split(text, "\n\n") {
			if text == string(doc) && !strings.Contains(para, "split") {
				continue
			}
			check(rel+" paragraph "+strconv.Itoa(i+1), strings.ReplaceAll(para, "\n", " "))
		}
	}
	fset := token.NewFileSet()
	for _, name := range []string{"bd_split_allowlist.go", "bd_split_judges.go", "class_flip_guard.go", "cmd_bd.go", "bd_inventory_trust.go", "doctor_bd_inventory.go", "cmd_bd_by_id.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			check(fset.Position(lit.Pos()).String(), s)
			return true
		})
	}
}

// syntheticBdInventory is a made-up bd tree that reaches the three resolver
// branches no pinned inventory does: an allowlisted path bd does not parse
// flags for, an alias gc has never vetted, and a command with no decision.
func syntheticBdInventory(t *testing.T) *bdflags.Inventory {
	t.Helper()
	help := `{"name": "help", "shorthand": "h", "type": "bool", "default": "false", "no_opt_def_val": "true"}`
	inv, err := bdflags.ParseInventory("bd-synthetic", []byte(`{"version": "9.9.9", "commands": [
		{"path": "", "runnable": true, "local": [`+help+`]},
		{"path": "show", "runnable": true, "disable_flag_parsing": true},
		{"path": "list", "aliases": ["peek"], "runnable": true, "local": [`+help+`]},
		{"path": "brand-new", "runnable": true, "local": [`+help+`]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestGcBdSplitResolverUnreachableBranches(t *testing.T) {
	inv := syntheticBdInventory(t)
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"show", "demo-1"}, "does not parse this command's flags"},
		{[]string{"peek"}, "an alias gc has not vetted"},
		{[]string{"brand-new"}, "it has no decision"},
	} {
		res := resolveBdSplitCommand(inv, c.argv, "/scope")
		if !strings.Contains(res.refusal, c.want) || !strings.Contains(res.refusal, "Nothing was written") {
			t.Errorf("gc bd %s: refusal = %q, want %q", strings.Join(c.argv, " "), res.refusal, c.want)
		}
	}
	if res := resolveBdSplitCommand(inv, []string{"list"}, "/scope"); res.refusal != "" {
		t.Errorf("the synthetic list was refused: %q", res.refusal)
	}
}

// TestBdEndpointScopeRoutesCrossRigEdges: an edge endpoint is read in the
// work store its id prefix names — the city's or any rig's — so a rig↔city
// or rig↔rig work dependency (a shipped order) stays allowed.
func TestBdEndpointScopeRoutesCrossRigEdges(t *testing.T) {
	city := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Prefix: "mc"},
		Rigs: []config.Rig{
			{Name: "gascity", Path: filepath.Join(city, "rigs", "gascity"), Prefix: "gc"},
			{Name: "gascity-docs", Path: filepath.Join(city, "rigs", "docs"), Prefix: "gc-docs"},
		},
	}
	scope := filepath.Join(city, "rigs", "gascity")
	for id, want := range map[string]string{
		"mc-1":      city,
		"gc-1":      scope,
		"gc-docs-1": filepath.Join(city, "rigs", "docs"),
		"zz-1":      scope,
	} {
		if got := bdEndpointScope(city, cfg, id, scope); got != want {
			t.Errorf("bdEndpointScope(%s) = %s, want %s", id, got, want)
		}
	}
	// The guard asks one endpoints call for every id, wherever it lives.
	deps := splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) { return beads.Bead{ID: id}, bdReadFound })
	var asked []string
	deps.endpoints = func(ids []string) map[string]bdReadResult {
		asked = append(asked, ids...)
		out := map[string]bdReadResult{}
		for _, id := range ids {
			out[id] = bdReadFound
		}
		return out
	}
	g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"dep", "add", "gc-1", "mc-1"}, deps)
	if g.refusal != "" {
		t.Fatalf("a rig->city work edge was refused: %q", g.refusal)
	}
	if !slicesContainsString(asked, "mc-1") || !slicesContainsString(asked, "gc-1") {
		t.Errorf("the endpoints were not read across stores: %v", asked)
	}
}

// TestBdByIDUpdateParentStaysInTheBinding: a reparent of a binding resident
// through the by-id door must name a parent the same binding holds.
func TestBdByIDUpdateParentStaysInTheBinding(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	child := mustCreateClassBead(t, classStore, beads.Bead{Title: "the child", Type: "task"})
	parent := mustCreateClassBead(t, classStore, beads.Bead{Title: "the parent", Type: "task"})
	work, err := workStoreFor(t, cityPath).Create(beads.Bead{Title: "a work bead", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"update", child.ID, "--parent", work.ID}, &stdout, &stderr)
	if !handled || code == 0 || !strings.Contains(stderr.String(), "across stores is never written") {
		t.Fatalf("a binding->work reparent = (%d, %t): %s", code, handled, stderr.String())
	}
	if after, _ := classStore.Get(child.ID); after.ParentID != "" {
		t.Errorf("the refused reparent wrote parent %q", after.ParentID)
	}
	stderr.Reset()
	code, handled = maybeRouteBdByID(cityPath, "", []string{"update", child.ID, "--parent", parent.ID}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("an in-binding reparent = (%d, %t): %s", code, handled, stderr.String())
	}
}

// TestBdBindingDependentsListsOpenDrainGates is the delete guard's open-gate
// half end to end: on a city whose classes live in a binding, an open
// cross-store drain gate in the binding naming a work bead makes that bead a
// binding dependent, so gc bd delete refuses it; a released gate does not.
func TestBdBindingDependentsListsOpenDrainGates(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	work, err := workStoreFor(t, cityPath).Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	gate := mustCreateClassBead(t, classStore, beads.Bead{Title: "waits", Type: "gate", Labels: []string{bdCrossStoreGateLabel}, Metadata: map[string]string{beadmeta.CrossStoreBlockerMetadataKey: work.ID}})
	deps, err := bdBindingDependents(cityPath, []string{work.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContainsString(deps[work.ID], gate.ID) {
		t.Fatalf("binding dependents of %s = %v, want the open gate %s", work.ID, deps[work.ID], gate.ID)
	}
	if err := classStore.Close(gate.ID); err != nil {
		t.Fatal(err)
	}
	if deps, err = bdBindingDependents(cityPath, []string{work.ID}); err != nil || len(deps[work.ID]) != 0 {
		t.Fatalf("after the gate was released: %v, %v", deps[work.ID], err)
	}
}
