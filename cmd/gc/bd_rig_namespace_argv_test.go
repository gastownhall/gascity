package main

// Tests of how gc bd reads an argv the way bd's pflag does: help, link and
// --no-auto, the forms core ships, a deny-all scope's reason, and the hook
// legs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// cobra runs the command unless the final parsed help value is true, so a
// help token that does not turn help on is no reason to pass.
func TestBdRigNamespaceHelpOnlyWhenReallyOn(t *testing.T) {
	c := brnCheck(t, "mayor", brnOwned, brnForeign)
	for _, args := range [][]string{
		{"close", "bt-2", "--help=false"},
		{"update", "bt-2", "--assignee", "mayor", "--help=false"},
		{"update", "bt-1", "--claim", "-h=false"},
		{"update", "bt-1", "--claim", "--help=0"},
		{"update", "bt-1", "--claim", "--help", "--help=false"},
		{"sql", "delete from issues", "--help"}, // unknown manifest: never help
		{"update", "bt-2", "--status", "closed", "--append-notes", "--help"},
		{"close", "bt-1", "--continue", "--reason", "--no-auto"},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes; bd runs it", args)
		}
	}
	for _, args := range [][]string{
		{"close", "bt-2", "--help"},
		{"update", "-h"},
		{"update", "bt-2", "--help=true"},
		{"dep", "add", "--help"},
	} {
		if _, msg, refused := c.check(args); refused {
			t.Errorf("gc bd %v (help) refused: %q", args, msg)
		}
	}
}

// `link <a> <b>` is `dep add <a> <b>`.
func TestBdRigNamespaceLinkIsDepAdd(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned, brnForeign)
	for _, args := range [][]string{
		{"link", "bt-2", "bt-1"},
		{"link", "bt-2", "bt-1", "--type", "parent-child"},
		{"link", "bt-2", "bt-1", "-t", "parent-child"},
		{"link", "bt-1"},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes", args)
		}
	}
	for _, args := range [][]string{
		{"link", "bt-1", "bt-2"},
		{"link", "bt-1", "bt-2", "--type=related"},
		{"relate", "bt-2", "bt-1"},
		{"unrelate", "bt-2", "bt-1"},
	} {
		if _, msg, refused := c.check(args); refused {
			t.Errorf("gc bd %v refused: %q", args, msg)
		}
	}
}

func TestBdRigNamespaceNoAutoParsedLikePflag(t *testing.T) {
	c := brnCheck(t, "mayor", brnOwned)
	for _, args := range [][]string{
		{"close", "bt-1", "--continue", "--no-auto=0"},
		{"close", "bt-1", "--continue", "--no-auto=f"},
		{"close", "bt-1", "--continue", "--no-auto=F"},
		{"close", "bt-1", "--continue", "--no-auto", "--no-auto=false"},
		{"close", "bt-1", "--no-auto", "--continue", "--no-auto=0"},
		{"close", "bt-1", "--continue", "--reason", "--no-auto"},
	} {
		if _, _, refused := c.check(args); !refused {
			t.Errorf("gc bd %v passes; bd auto-claims the next step", args)
		}
	}
	if _, msg, refused := c.check([]string{"close", "bt-1", "--continue", "--no-auto=true"}); refused {
		t.Errorf("close --continue --no-auto=true refused: %q", msg)
	}
}

// Forms bd accepts and core prompts, formulas and skills ship must pass for
// an admitted worker on an owned row.
func TestBdRigNamespaceShippedFormsPass(t *testing.T) {
	held := beads.Bead{ID: "bt-4", Status: "in_progress", Assignee: poolIDB}
	c := brnCheck(t, poolIDB, brnOwned, held)
	for _, args := range [][]string{
		// core prompts (pool-worker, graph-worker)
		{"show", "bt-4"},
		{"mol", "current", "bt-4"},
		{"mol", "progress", "bt-4"},
		{"close", "bt-4"},
		{"close", "bt-4", "--reason", "did the thing"},
		{"update", "bt-4", "--set-metadata", "gc.outcome=pass", "--status", "closed"},
		{"update", "bt-4", "--set-metadata", "gc.outcome=fail", "--set-metadata", "gc.failure_class=hard", "--set-metadata", "gc.failure_reason=push_conflict"},
		// core formulas
		{"prime"},
		{"show", "bt-4", "--json"},
		{"update", "bt-4", "--append-notes", "Synthesized: x"},
		{"close", "bt-4", "--reason=did the thing"},
		{"update", "bt-4", "--set-metadata", "gc.outcome=pass", "--status=closed"},
		{"update", "bt-4", "--set-metadata", "work_dir=/tmp/x"},
		{"update", "bt-4", "--unset-metadata", "work_dir"},
		{"update", "bt-1", "--claim"},
		{"heartbeat", "bt-4"},
		{"list", "--assignee=" + poolIDB, "--status=in_progress"},
		{"list", "--title-contains", "K", "--status", "open,in_progress,blocked", "--json"},
		{"list", "--title-contains", "K", "--status", "closed", "--closed-after", "2026-10-15", "--json"},
		{"comment", "bt-1", "Also observed on main while starting bt-4."},
		{"create", "--title", "Pre-existing failure: K", "--type", "bug", "--priority", "1", "--design", "Observed on main."},
		{"create", "--title", "Found: x", "--type", "bug", "--priority", "2"},
		// core skills (gc-work, gc-rigs, gc-dispatch); --rig is gc's own flag
		{"create", "title"},
		{"create", "title", "-t", "bug"},
		{"create", "title", "--label", "priority=high"},
		{"list"},
		{"list", "--ready"},
		{"ready"},
		{"ready", "--label", "role:worker"},
		{"update", "bt-4", "--status", "in_progress"},
		{"update", "bt-4", "--add-label", "k=v"},
		// bd's hidden create aliases and verb flags
		{"create", "title", "--body", "text"},
		{"create", "title", "-m", "text"},
		{"create", "title", "--message", "text"},
		{"create", "title", "--description-file", "d.md"},
		{"state", "bt-1", "phase"},
		{"human", "list"},
		{"formula", "list"},
		{"formula", "show", "f"},
		{"unclaim", "bt-4", "--reason", "done"},
		{"unclaim", "bt-4", "--if-assignee", poolIDB},
		{"defer", "bt-1", "--reason", "later"},
		{"comments", "add", "bt-1", "hi", "-a", "me"},
		{"assign", "bt-1", "beta/crew.lead", "--force"},
	} {
		if _, msg, refused := c.check(args); refused {
			t.Errorf("shipped form refused: gc bd %v -> %s", args, strings.TrimSpace(msg))
		}
	}
}

// A deny-all scope says why.
func TestBdRigNamespaceDenyAllShowsReason(t *testing.T) {
	root := t.TempDir()
	beta := filepath.Join(root, "beta")
	alpha := filepath.Join(root, "alpha")
	for _, d := range []string{filepath.Join(beta, ".beads"), filepath.Join(alpha, ".beads")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(alpha, ".beads", "redirect"), []byte(filepath.Join(beta, ".beads")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := rnsGuardedRig("beta")
	g.Path = beta
	cfg := &config.City{Rigs: []config.Rig{g, {Name: "alpha", Path: alpha}}}
	c := newBdRigNamespaceCheck(cfg, root, beta, func(string) (beads.Bead, error) { return brnOwned, nil })
	_, msg, refused := c.check([]string{"close", "bt-1"})
	if !refused || !strings.Contains(msg, `rig "alpha"`) {
		t.Fatalf("refused=%v msg=%q; want the shared store with rig alpha named", refused, msg)
	}
}

// With the canonical readback failing, the query-row refusal still gates
// adoption: an unadmitted actor never re-stamps.
func TestHookRigGuardUnverifiedAdoptionStillGated(t *testing.T) {
	g := hrgGuard(t)
	row := beads.Bead{ID: "bt-1", Status: "in_progress", Assignee: "beta/crew.lead"}
	var w hrgWrites
	ops := hrgOps(t, &w, g, []beads.Bead{row})
	ops.ReadWorkMeta = func(context.Context, string, []string, string, string) (beads.Bead, error) {
		return beads.Bead{}, errors.New("readback unavailable")
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", hrgDir(g.cfg, "beta"), hookClaimOptions{
		Assignee: shortIDB, RuntimeActor: shortIDB,
		IdentityCandidates: []string{"beta/crew.lead", shortIDB}, JSON: true,
	}, ops, &stdout, &stderr)
	if w.total() != 0 {
		t.Fatalf("an unadmitted actor wrote an unverified adoption: %+v", w)
	}
	if !strings.Contains(stderr.String(), "not claiming bt-1") {
		t.Fatalf("the query-row refusal did not run:\n%s", stderr.String())
	}
}

// Across the production fan-out of a rig agent (the rig leg, its own leg at
// the rig root, the city leg), an unmapped-prefix row of the guarded store
// is never claimed by an unadmitted actor.
func TestHookRigGuardUnmappedPrefixAcrossFanOut(t *testing.T) {
	g := hrgGuard(t)
	beta := hrgDir(g.cfg, "beta")
	city := t.TempDir()
	for _, id := range []string{"or-77", "hq-77", "tr-77"} {
		rows := []beads.Bead{{ID: id, Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}}
		var w hrgWrites
		ops := hrgOps(t, &w, g, rows)
		out, _ := json.Marshal(rows)
		run := func(_ string, dir string, _ []string) (string, error) {
			if dir == city {
				return "[]", nil
			}
			return string(out), nil
		}
		stores := []hookStore{{dir: beta, rig: "beta"}, {dir: beta}, {dir: city}}
		var stdout, stderr bytes.Buffer
		claimHookWorkWithRunner("query", beta, nil, stores, hookClaimOptions{
			Assignee: "mayor", RouteTargets: []string{"beta/crew.worker"}, JSON: true,
		}, ops, run, func(string, error) {}, &stdout, &stderr)
		if len(w.claims) != 0 {
			t.Errorf("%s claimed by an unadmitted actor in the fan-out: %v", id, w.claims)
		}
	}
}

// A leg whose directory names no store but whose BEADS_DIR points bd at the
// guarded store is that store's leg.
func TestHookRigGuardBindsLegByBeadsDir(t *testing.T) {
	g := hrgGuard(t)
	beta := hrgDir(g.cfg, "beta")
	elsewhere := t.TempDir()
	rows := []beads.Bead{{ID: "or-77", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}}
	var w hrgWrites
	ops := hrgOps(t, &w, g, rows)
	out, _ := json.Marshal(rows)
	run := func(string, string, []string) (string, error) { return string(out), nil }
	stores := []hookStore{{dir: elsewhere, env: []string{"BEADS_DIR=" + filepath.Join(beta, ".beads")}}}
	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("query", elsewhere, nil, stores, hookClaimOptions{
		Assignee: "mayor", RouteTargets: []string{"beta/crew.worker"}, JSON: true,
	}, ops, run, func(string, error) {}, &stdout, &stderr)
	if len(w.claims) != 0 {
		t.Fatalf("a leg bound to beta's store by BEADS_DIR was claimed by an unadmitted actor: %v", w.claims)
	}
	// Signals that name different guarded rigs admit nothing.
	gamma := hrgDir(g.cfg, "gamma")
	if leg := g.forLeg("beta", gamma).leg; leg == nil || leg.namespace() != "" {
		t.Fatalf("a leg named beta but run in gamma's store = %+v, want deny-all", leg)
	}
}

// agentScriptBeadEnvForCity drives the full gc bd check: a write against a
// Foreign row of the runner's guarded rig is refused before bd writes, read
// through the rig's bd front door.
func TestAgentScriptBeadEnvRunsGcBdCheck(t *testing.T) {
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
	if _, err := f.WriteString("\n[[rigs]]\nname = \"beta\"\npath = \"" + rigDir + "\"\nprefix = \"bt\"\nfederation = \"rig\"\nnamespace = \"exclusive\"\n\n[[agent]]\nname = \"crew.worker\"\ndir = \"beta\"\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loading the fixture city: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "beta/crew.worker")
	t.Setenv("BEADS_ACTOR", "beta/crew.worker")
	// bd answers every read with a row another city owns.
	foreign, _ := json.Marshal([]beads.Bead{{ID: "bt-2", Status: "in_progress", Assignee: "alpha/crew.worker", Type: "task"}})
	t.Setenv("BD_STUB_STDOUT", string(foreign))
	_, _, ok, err := agentScriptBeadEnvForCity(cityDir, cfg, "bt-2", []string{"update", "bt-2", "--status", "open"})
	if ok || !errors.Is(err, errRigNamespace) || !strings.Contains(err.Error(), `does not own it (owner "alpha/crew.worker")`) {
		t.Fatalf("ok=%v err=%v; want the gc bd row refusal", ok, err)
	}
	data, _ := os.ReadFile(capture)
	if strings.Contains(string(data), "update") {
		t.Fatalf("bd ran the write: %q", data)
	}
	if !strings.Contains(string(data), "show") {
		t.Fatalf("the check did not read the row through bd: %q", data)
	}
}

// bd's cobra finds the command before it parses a flag, and takes the token
// after a flag it does not know yet as that flag's value: `--reason show close
// <id>` runs close. The guard reads the command the same way, and refuses an
// argv it cannot read that way rather than checking the wrong command.
func TestBdRigNamespaceReadsTheCommandCobraRuns(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned, brnForeign)
	for _, args := range [][]string{
		{"--reason", "show", "close", "bt-2"},
		{"-r", "show", "close", "bt-2"},
		{"--append-notes", "list", "update", "bt-2", "--claim"},
		{"--status", "blocked", "update", "bt-1", "--assignee", "alpha/crew.worker"},
		{"--title", "show", "create"},
		{"--reason=x", "close", "bt-2"},
		{"comments", "--local-time", "add", "bt-1", "text"},
		{"comments", "-V", "add", "bt-1", "text"},
		{"-V", "comments", "add", "bt-1", "text"}, // cobra reads "add" as -V's value under comments
		{"--", "close", "bt-2"},
		{"", "close", "bt-2"},
	} {
		if _, msg, refused := c.check(args); !refused || !strings.Contains(msg, "cannot tell which command bd would run") {
			t.Errorf("gc bd %q: refused=%v msg=%q; want the unreadable-command refusal", args, refused, msg)
		}
	}
	// A compound verb's sub-verb is read past the global flags, so these are
	// the writes they are, and the Foreign row refuses them.
	for _, args := range [][]string{
		{"comments", "--json", "add", "bt-2", "text"},
		{"comments", "--actor", "beta/crew.worker", "add", "bt-2", "text"},
		{"--actor", "beta/crew.worker", "comments", "add", "bt-2", "text"},
		{"label", "--json", "add", "bt-2", "gc:hold"},
		{"dep", "--json", "add", "bt-2", "bt-1"},
		{"-h", "show", "close", "bt-2"}, // the help flag takes "show"; bd would print close's help
	} {
		if _, msg, refused := c.check(args); !refused || !strings.Contains(msg, `does not own it (owner "alpha/crew.worker")`) {
			t.Errorf("gc bd %q: refused=%v msg=%q; want the Foreign-row refusal", args, refused, msg)
		}
	}
	for _, args := range [][]string{
		{"comments", "--json", "add", "bt-1", "text"},
		{"comments", "--json", "bt-2"},
		{"dep", "--json", "list", "bt-2"},
		{"--json", "--actor", "beta/crew.worker", "close", "bt-1"},
		{"--help"},
		{"-V"},
		{"--actor", "beta/crew.worker"},
	} {
		if _, msg, refused := c.check(args); refused {
			t.Errorf("gc bd %q refused: %q", args, msg)
		}
	}
}

// The scope that is no guarded rig's cannot tell a guarded rig's bead in an
// argv whose command it cannot read: every positional token is judged.
func TestBdCrossRigCheckJudgesAnUnreadableArgvByEveryToken(t *testing.T) {
	cfg := hrgCity(t)
	c := newBdRigNamespaceCheck(cfg, "/cities/b", hrgDir(cfg, "alpha"), nil)
	if c == nil || c.ns != nil {
		t.Fatalf("fixture: alpha's check = %+v, want the cross-scope check only", c)
	}
	for _, args := range [][]string{
		{"--reason", "show", "close", "bt-2"},
		{"-r", "show", "close", "bt-2"},
		{"--append-notes", "list", "update", "bt-2", "--claim"},
		{"--title", "show", "create", "bt-2"},
		{"comments", "--json", "add", "bt-2", "text"},
		{"comments", "--actor", "x", "add", "bt-2", "text"},
		{"comments", "--local-time", "add", "bt-2", "text"},
	} {
		if _, msg, refused := c.check(args); !refused || !strings.Contains(msg, `on bt-2: it is rig "beta"'s bead`) {
			t.Errorf("gc bd %q from alpha's scope: refused=%v msg=%q; want beta's bead refused", args, refused, msg)
		}
	}
	for _, args := range [][]string{
		{"--reason", "show", "close", "al-1"},
		{"comments", "--json", "bt-2"},
		{"--json", "show", "bt-2"},
	} {
		if _, msg, refused := c.check(args); refused {
			t.Errorf("gc bd %q from alpha's scope refused: %q", args, msg)
		}
	}
}

// A guarded write must land in the scope's own store: every flag that points
// bd at another store is refused, in every spelling and position.
func TestBdRigNamespaceRefusesStoreSelectors(t *testing.T) {
	c := brnCheck(t, "beta/crew.worker", brnOwned)
	for _, sel := range []struct{ flag, value string }{
		{"--db", "/other/beads.db"},
		{"--database", "other_db"},
		{"--server-url", "https://other-service"},
		{"--profile", "other"},
	} {
		for _, args := range [][]string{
			{sel.flag, sel.value, "close", "bt-1"},
			{sel.flag + "=" + sel.value, "close", "bt-1"},
			{"close", "bt-1", sel.flag, sel.value},
			{"update", "bt-1", "--status", "closed", sel.flag + "=" + sel.value},
		} {
			if _, msg, refused := c.check(args); !refused || !strings.Contains(msg, "checks writes against its own store only") {
				t.Errorf("gc bd %q: refused=%v msg=%q; want the store-selector refusal", args, refused, msg)
			}
		}
		if _, msg, refused := c.check([]string{sel.flag, sel.value, "show", "bt-1"}); refused {
			t.Errorf("a read with %s refused: %q", sel.flag, msg)
		}
	}
	for _, args := range [][]string{
		{"--global", "close", "bt-1"},
		{"--global=true", "close", "bt-1"},
		{"close", "bt-1", "--global"},
		{"update", "bt-1", "--status", "closed", "--global=1"},
		{"close", "bt-1", "--global=maybe"},
	} {
		if _, msg, refused := c.check(args); !refused || !strings.Contains(msg, "checks writes against its own store only") {
			t.Errorf("gc bd %q: refused=%v msg=%q; want the store-selector refusal", args, refused, msg)
		}
	}
	if _, msg, refused := c.check([]string{"close", "bt-1", "--global=false"}); refused {
		t.Errorf("--global=false refused: %q", msg)
	}
}

// bd opens the store its -C names (from the directory it runs in, the scope
// root), and gc chose the scope by --rig, --city or the bead prefix first: a
// write whose -C opens another store than the one checked is refused, from a
// guarded scope and into one, in every spelling of -C bd reads. A claim that
// names no bead (ready --claim) is such a write.
func TestBdRigNamespaceDirectoryMustOpenTheCheckedStore(t *testing.T) {
	root := t.TempDir()
	beta := filepath.Join(root, "beta")
	alpha := filepath.Join(root, "alpha")
	for _, d := range []string{filepath.Join(beta, ".beads"), filepath.Join(beta, "sub"), filepath.Join(alpha, ".beads")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g := rnsGuardedRig("beta")
	g.Path, g.Prefix = beta, "bt"
	cfg := &config.City{Rigs: []config.Rig{g, {Name: "alpha", Path: alpha, Prefix: "al"}}}
	t.Setenv("BEADS_ACTOR", "beta/crew.worker")
	rows := map[string]beads.Bead{"bt-1": brnOwned, "al-1": {ID: "al-1", Status: "open"}}
	get := func(id string) (beads.Bead, error) {
		if b, ok := rows[id]; ok {
			return b, nil
		}
		return beads.Bead{}, beads.ErrNotFound
	}

	inBeta := newBdRigNamespaceCheck(cfg, root, beta, get)
	if inBeta == nil || inBeta.ns.rigName() != "beta" {
		t.Fatalf("fixture: beta's check = %+v", inBeta)
	}
	for _, args := range [][]string{
		{"-C", alpha, "close", "bt-1"},
		{"-C=" + alpha, "close", "bt-1"},
		{"close", "bt-1", "--directory", alpha},
		{"close", "bt-1", "--directory=" + alpha},
		{"-C", "../alpha", "close", "bt-1"},
		{"-C", beta, "-C", alpha, "close", "bt-1"},
		{"-C" + alpha, "close", "bt-1"},
		{"close", "bt-1", "-qC", alpha},
		{"close", "bt-1", "-qC=" + alpha},
	} {
		if _, msg, refused := inBeta.check(args); !refused || !strings.Contains(msg, "a store that is no guarded rig's") {
			t.Errorf("gc bd %q in beta's scope: refused=%v msg=%q; want the -C refusal", args, refused, msg)
		}
	}
	for _, args := range [][]string{
		{"-C", beta, "close", "bt-1"},
		{"-C", filepath.Join(beta, "sub"), "close", "bt-1"},
		{"-C", "sub", "close", "bt-1"},
		{"-C", alpha, "show", "bt-1"},
	} {
		if _, msg, refused := inBeta.check(args); refused {
			t.Errorf("gc bd %q in beta's scope refused: %q", args, msg)
		}
	}
	// A claim in the rig's own store is still the rig's guard to refuse.
	for _, args := range [][]string{{"ready", "--claim"}, {"-C", beta, "ready", "--claim"}} {
		if _, msg, refused := inBeta.check(args); !refused || !strings.Contains(msg, "lets gc claim only rows it can check first") {
			t.Errorf("gc bd %q in beta's scope: refused=%v msg=%q; want the unvetted-claim refusal", args, refused, msg)
		}
	}

	// gc picks alpha's scope by the bead prefix, which outranks -C; bd would
	// then write beta's store with no check.
	inAlpha := newBdRigNamespaceCheck(cfg, root, alpha, get)
	if inAlpha == nil || inAlpha.ns != nil {
		t.Fatalf("fixture: alpha's check = %+v", inAlpha)
	}
	for _, args := range [][]string{
		{"-C", beta, "update", "al-1", "--status", "closed"},
		{"-C", filepath.Join(beta, "sub"), "close", "al-1"},
		{"--reason", "x", "-C", beta, "close", "al-1"},
		{"-C", beta, "ready", "--claim"},
		{"-C=" + beta, "ready", "--claim"},
		{"ready", "--claim", "--directory", beta},
		{"-C" + beta, "update", "al-1", "--status", "closed"},
		{"update", "al-1", "-C" + beta, "--status", "closed"},
		{"-qC", beta, "update", "al-1", "--status", "closed"},
		{"update", "al-1", "-qC", beta, "--status", "closed"},
		{"update", "al-1", "-qC" + beta},
		{"update", "al-1", "-qC=" + beta},
		{"--json", "assign", "al-1", "someone", "-C" + beta},
	} {
		if _, msg, refused := inAlpha.check(args); !refused || !strings.Contains(msg, `rig "beta"'s store`) {
			t.Errorf("gc bd %q in alpha's scope: refused=%v msg=%q; want the -C refusal", args, refused, msg)
		}
	}
	for _, args := range [][]string{
		{"-C", alpha, "update", "al-1", "--status", "closed"},
		{"-C", beta, "show", "al-1"},
		{"-C", beta, "ready"},
		{"ready", "--claim"},
		{"-C", alpha, "ready", "--claim"},
		{"-C" + alpha, "update", "al-1", "--status", "closed"},
		{"update", "al-1", "-qC", alpha, "--status", "closed"},
		{"update", "al-1", "-aC" + beta}, // -a takes "C<beta>": an assignee, not a -C
	} {
		if _, msg, refused := inAlpha.check(args); refused {
			t.Errorf("gc bd %q in alpha's scope refused: %q", args, msg)
		}
	}
}

// doBd runs the guard on the command bd would run: an argv that hides a write
// behind a read verb never reaches bd, in the rig's scope or from another,
// and neither does a write whose -C, in any spelling bd reads, opens the rig's
// store from another scope (a claim that names no bead included).
func TestDoBdRigNamespaceRefusesAHiddenWrite(t *testing.T) {
	capture, cityDir := bdSQLRefusalCityDir(t, "")
	var toml strings.Builder
	for _, r := range []struct{ name, prefix, extra string }{
		{"alpha", "al", ""},
		{"beta", "bt", "federation = \"rig\"\n"},
	} {
		dir := filepath.Join(cityDir, r.name)
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: "+r.prefix+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		toml.WriteString("\n[[rigs]]\nname = \"" + r.name + "\"\npath = \"" + dir + "\"\nprefix = \"" + r.prefix + "\"\n" + r.extra)
	}
	f, err := os.OpenFile(filepath.Join(cityDir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(toml.String()); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	t.Setenv("BEADS_ACTOR", "mayor")
	// bd answers every read with a row another city holds.
	foreign, _ := json.Marshal([]beads.Bead{{ID: "bt-2", Status: "in_progress", Assignee: "alpha/crew.worker", Type: "task"}})
	t.Setenv("BD_STUB_STDOUT", string(foreign))
	refused := func(args []string, want string) {
		t.Helper()
		_ = os.Remove(capture)
		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code == 0 {
			t.Errorf("doBd(%q) exited 0; stderr=%q", args, stderr.String())
		}
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("doBd(%q): stderr lacks %q: %q", args, want, stderr.String())
		}
		data, _ := os.ReadFile(capture)
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line != "" && !strings.HasPrefix(line, "show ") {
				t.Errorf("doBd(%q): bd ran %q", args, line)
			}
		}
	}
	for _, args := range [][]string{
		{"--rig", "beta", "--reason", "show", "close", "bt-2"},
		{"--rig", "beta", "--append-notes", "list", "update", "bt-2", "--claim"},
		{"--rig", "beta", "--title", "show", "create"},
		{"--rig", "beta", "comments", "--json", "add", "bt-2", "text"},
		{"--rig", "alpha", "--reason", "show", "close", "bt-2"},
		{"--rig", "alpha", "comments", "--json", "add", "bt-2", "text"},
		{"--rig", "beta", "close", "bt-2", "--server-url", "https://other-service"},
	} {
		refused(args, "gc bd: refusing")
	}
	beta := filepath.Join(cityDir, "beta")
	for _, args := range [][]string{
		{"--rig", "alpha", "-C", beta, "update", "al-1", "--status", "closed"},
		{"--rig", "alpha", "-C", beta, "ready", "--claim"},
		{"--rig", "alpha", "-C=" + beta, "ready", "--claim"},
		{"--rig", "alpha", "-C" + beta, "update", "al-1", "--status", "closed"},
		{"--rig", "alpha", "--json", "update", "al-1", "-C" + beta, "--status", "closed"},
		{"--rig", "alpha", "-qC", beta, "update", "al-1", "--status", "closed"},
		{"--rig", "alpha", "--json", "update", "al-1", "-qC", beta, "--status", "closed"},
		{"--rig", "alpha", "assign", "al-1", "someone", "-C" + beta},
	} {
		refused(args, "gc bd: refusing -C")
	}
}
