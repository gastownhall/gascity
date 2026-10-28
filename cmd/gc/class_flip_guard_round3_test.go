//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// edgeCase is one argv through the split guard with a world where some ids
// exist in the work store and some live in the class binding.
type edgeCase struct {
	name   string
	argv   []string
	refuse bool
	want   string
}

// runEdgeCases runs each case against a work store holding demo-1, demo-2
// and relic-1, where relic-1 is a migrated relic the binding holds; any other
// id is absent.
func runEdgeCases(t *testing.T, cases []edgeCase) {
	t.Helper()
	present := map[string]bool{"demo-1": true, "demo-2": true, "relic-1": true}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scope := t.TempDir()
			deps := splitTestDeps(nil, func(id string) (beads.Bead, bdReadResult) {
				if present[id] {
					return beads.Bead{ID: id, Type: "task"}, bdReadFound
				}
				return beads.Bead{}, bdReadNotFound
			})
			deps.held = func(ids []string) (map[string]bool, error) {
				out := map[string]bool{}
				for _, id := range ids {
					out[id] = id == "relic-1"
				}
				return out, nil
			}
			if err := os.WriteFile(filepath.Join(scope, "ops.txt"), []byte("dep add demo-1 gcg-9\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			g := bdClassFlipGuard(splitGuardTestCity(), scope, c.argv, deps)
			if c.refuse != (g.refusal != "") {
				t.Fatalf("gc bd %s: refused=%v, want %v; refusal=%q", strings.Join(c.argv, " "), g.refusal != "", c.refuse, g.refusal)
			}
			if c.refuse && !strings.Contains(g.refusal, c.want) {
				t.Errorf("refusal does not carry %q: %q", c.want, g.refusal)
			}
		})
	}
}

// TestGcBdSplitNeverWritesACrossStoreEdge is review must-fix 3: every edge
// endpoint must be found in the work store AND not held by the binding.
func TestGcBdSplitNeverWritesACrossStoreEdge(t *testing.T) {
	runEdgeCases(t, []edgeCase{
		{"dep add to an absent (binding) id", []string{"dep", "add", "demo-1", "gcg-x"}, true, "never written"},
		{"dep add --blocked-by", []string{"dep", "add", "demo-1", "--blocked-by", "gcg-x"}, true, "never written"},
		{"dep add --depends-on", []string{"dep", "add", "demo-1", "--depends-on", "gcg-x"}, true, "never written"},
		{"dep add to a relic the binding holds", []string{"dep", "add", "demo-1", "relic-1"}, true, "lives in the class binding"},
		{"dep add from an absent id", []string{"dep", "add", "gcg-x", "demo-1"}, true, "never written"},
		{"dep add --file", []string{"dep", "add", "--file", "deps.txt"}, true, "--file"},
		{"dep relate", []string{"dep", "relate", "demo-1", "gcg-x"}, true, "never written"},
		{"link", []string{"link", "demo-1", "gcg-x"}, true, "never written"},
		{"duplicate --of", []string{"duplicate", "demo-1", "--of", "relic-1"}, true, "class binding"},
		{"supersede --with", []string{"supersede", "demo-1", "--with", "gcg-x"}, true, "never written"},
		{"gate create --blocks", []string{"gate", "create", "--blocks", "gcg-x"}, true, "never written"},
		{"create --deps", []string{"create", "--title", "t", "--deps", "blocks:gcg-x"}, true, "never written"},
		{"create --waits-for", []string{"create", "--title", "t", "--waits-for", "gcg-x"}, true, "never written"},
		{"create --parent", []string{"create", "--title", "t", "--parent", "relic-1"}, true, "class binding"},
		{"q --parent", []string{"q", "t", "--parent", "gcg-x"}, true, "never written"},
		{"update --parent", []string{"update", "demo-1", "--parent", "gcg-x"}, true, "never written"},
		{"batch dep line", []string{"batch", "-f", "ops.txt"}, true, "never written"},
		{"dep add within the work store", []string{"dep", "add", "demo-1", "demo-2"}, false, ""},
		{"create with work deps and parent", []string{"create", "--title", "t", "--deps", "demo-1,discovered-from:demo-2", "--parent", "demo-1"}, false, ""},
		{"update --parent \"\" unparents", []string{"update", "demo-1", "--parent", ""}, false, ""},
		{"dep remove is not an edge write", []string{"dep", "remove", "demo-1", "gcg-x"}, false, ""},
	})
}

// TestGcBdSplitJudgesTheCommandBdRuns is review must-fix 1 in gc: the
// short-flag runs whose last flag takes the next token are refused, and so
// is any argv whose cobra and pflag readings disagree.
func TestGcBdSplitJudgesTheCommandBdRuns(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"-qC swarm create", []string{"-qC", "swarm", "create", ".", "demo-1"}, guardTask, bdReadFound, true, "short-flag run", false},
		{"-qC merge-slot create", []string{"-qC", "merge-slot", "create", "."}, guardTask, bdReadFound, true, "short-flag run", false},
		{"human -qC respond", []string{"human", "-qC", "respond", ".", "demo-1"}, guardTask, bdReadFound, true, "short-flag run", false},
		{"-qC enterprise", []string{"-qC", "swarm", "create", ".", "demo-1"}, guardTask, bdReadFound, true, "short-flag run", true},
		{"update -qt", []string{"update", "demo-1", "-qt", "message"}, guardTask, bdReadFound, true, "short-flag run", false},
		{"attached cluster value is fine", []string{"update", "demo-1", "-qtbug"}, guardTask, bdReadFound, false, "", false},
		{"-- before the verb", []string{"--", "update", "demo-1", "--type", "message"}, guardTask, bdReadFound, true, "not a bd command", false},
		{"help on a refused verb", []string{"swarm", "create", "--help"}, guardTask, bdReadFound, false, "", false},
		{"help on a refused alias", []string{"mol", "fart", "--help"}, guardTask, bdReadFound, false, "", false},
	})
}

// TestGcBdSplitAcceptsHiddenFlags is review must-fix 4: the inventory comes
// from bd's cobra tree, so hidden flags the shipped packs use are known.
func TestGcBdSplitAcceptsHiddenFlags(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"create --label=warrant", []string{"create", "--title", "w", "--label=warrant"}, beads.Bead{}, bdReadNotFound, false, "", false},
		{"create --label=gc:session is judged", []string{"create", "--title", "w", "--label=gc:session"}, beads.Bead{}, bdReadNotFound, true, "sessions-class", false},
		{"close --resolution", []string{"close", "demo-1", "--resolution", "done"}, guardTask, bdReadFound, false, "", false},
		{"close --comment", []string{"close", "demo-1", "--comment", "done"}, guardTask, bdReadFound, false, "", false},
		{"update --acceptance-criteria", []string{"update", "demo-1", "--acceptance-criteria", "x"}, guardTask, bdReadFound, false, "", false},
		{"mol burn --yes", []string{"mol", "burn", "w-1", "--yes"}, guardTask, bdReadFound, false, "", false},
		{"list --state", []string{"list", "--state", "open"}, guardTask, bdReadFound, false, "", false},
		{"list --deps is a string with a default", []string{"list", "--deps", "--json"}, guardTask, bdReadFound, false, "", false},
		{"mol wisp gc", []string{"mol", "wisp", "gc", "--age", "2h"}, guardTask, bdReadFound, false, "", false},
	})
}

// TestGcBdSplitShouldFixes covers the review's should-fixes and nits.
func TestGcBdSplitShouldFixes(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"config set types.custom", []string{"config", "set", "types.custom", "convoy,molecule"}, beads.Bead{}, bdReadFound, false, "", false},
		{"config set routing", []string{"config", "set", "routing.mode", "x"}, beads.Bead{}, bdReadFound, true, "only types.custom, types.infra", false},
		{"config set storage-class", []string{"config", "set", "storage-class.task", "ephemeral"}, beads.Bead{}, bdReadFound, true, "only types.custom", false},
		{"create --id in a reserved namespace", []string{"create", "--title", "x", "--id", "gcg-abc", "--force"}, beads.Bead{}, bdReadNotFound, true, "reserved namespace", false},
		{"unknown bd build", []string{"show", "demo-1"}, guardTask, bdReadFound, false, "", false},
	})
	g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"show", "demo-1"}, func() bdClassGuardDeps {
		d := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return guardTask, bdReadFound })
		d.inventory = func() (*bdflags.Inventory, error) {
			return nil, fmt.Errorf("%w: /opt/bd (sha256 0123456789ab) matches none", errBdUninventoried)
		}
		return d
	}())
	if !strings.Contains(g.refusal, "matches none") || !strings.Contains(g.refusal, "gc doctor") || g.kind != bdRefusalUninventoried {
		t.Fatalf("a bd whose tree matches no inventory: refusal=%q kind=%q", g.refusal, g.kind)
	}
	g = bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"show", "demo-1"}, func() bdClassGuardDeps {
		d := splitTestDeps(nil, nil)
		d.inventory = func() (*bdflags.Inventory, error) { return nil, errors.New("exec: bd: not found") }
		return d
	}())
	if !strings.Contains(g.refusal, "exec: bd: not found") {
		t.Fatalf("an unwalkable bd: refusal=%q", g.refusal)
	}
	if single := bdClassFlipGuard(&config.City{}, t.TempDir(), []string{"show"}, bdClassGuardDeps{}); single.refusal != "" {
		t.Fatalf("a single-store city needs no bd version: %q", single.refusal)
	}
}

// TestGcBdSplitJudgesMetadataFiles: `--metadata @file` is read (relative to
// the scope root, where bd runs) and judged like an inline body.
func TestGcBdSplitJudgesMetadataFiles(t *testing.T) {
	scope := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(scope, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.json", `{"github_url":"https://x","gc.outcome":"pass"}`)
	write("graph.json", `{"gc.root_bead_id":"gcg-1"}`)
	deps := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return guardTask, bdReadFound })
	for argv, refuse := range map[string]bool{
		"create --title t --metadata @ok.json":           false,
		"create --title t --metadata @graph.json":        true,
		"update demo-1 --metadata @ok.json":              false,
		"update demo-1 --metadata @graph.json":           true,
		"update demo-1 --metadata @missing.json":         true,
		`create --title t --metadata {"gc.kind":"wisp"}`: true,
	} {
		g := bdClassFlipGuard(splitGuardTestCity(), scope, strings.Fields(argv), deps)
		if refuse != (g.refusal != "") {
			t.Errorf("gc bd %s: refused=%v, want %v (%q)", argv, g.refusal != "", refuse, g.refusal)
		}
	}
}

// TestGcBdSplitRootOnlyWispJudgesTemplateMatches: bd resolves a mol-* name
// that is not a formula to the first template bead whose title contains it, so
// the guard judges every such template.
func TestGcBdSplitRootOnlyWispJudgesTemplateMatches(t *testing.T) {
	for _, c := range []struct {
		name      string
		templates []beads.Bead
		refuse    bool
	}{
		{"no template", nil, false},
		{"a work template", []beads.Bead{{ID: "demo-t1", Title: "mol-patrol v1", Type: "molecule", Labels: []string{"template"}}}, false},
		{"an infra template", []beads.Bead{{ID: "demo-t2", Title: "mol-patrol v2", Type: "molecule", Labels: []string{"template", "gc:wisp"}}}, true},
		{"an infra template that does not match", []beads.Bead{{ID: "demo-t3", Title: "other", Type: "molecule", Labels: []string{"template", "gc:wisp"}}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			deps := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound })
			deps.templates = func() ([]beads.Bead, error) { return c.templates, nil }
			g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), []string{"mol", "wisp", "mol-patrol", "--root-only"}, deps)
			if c.refuse != (g.refusal != "") {
				t.Fatalf("refused=%v, want %v: %q", g.refusal != "", c.refuse, g.refusal)
			}
		})
	}
}
