package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// splitGuardTestCity is a whole-split city: every infrastructure class on one
// binding, work on the work stores.
func splitGuardTestCity() *config.City {
	return &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work: config.StorageWorkBinding, Graph: "infra", Sessions: "infra",
			Messaging: "infra", Orders: "infra", Nudges: "infra",
		},
		Bindings: map[string]config.StorageBindingConfig{"infra": {Provider: config.StorageProviderSQLiteBeads}},
	}}
}

// TestGcBdSplitAllowlistDecidesEveryCommand is the allowlist's coverage test
// over every pinned bd inventory (internal/bdflags/inventory, dumped from bd's
// own cobra tree, hidden commands and flags included): every command path is
// either allowlisted with its vetted flag set or refused with a stated
// alternative, and every alias is either mapped onto an allowlisted path or
// refused. A command, alias or flag in a regenerated inventory fails here until
// it has a decision. The decisions are checked by behavior, through
// resolveBdSplitCommand on each inventory, not just by table membership.
func TestGcBdSplitAllowlistDecidesEveryCommand(t *testing.T) {
	for path := range bdSplitAllowlist {
		if !bdflags.IsCommandPath(path) {
			t.Errorf("allowlisted %q is not a command path in any inventory", path)
		}
	}
	for key := range bdSplitRefusals {
		if !bdflags.IsCommandPath(key) {
			t.Errorf("refusal %q is not a command path in any inventory", key)
		}
		if _, allowed := bdSplitAllowlist[key]; allowed {
			t.Errorf("%q is both allowlisted and refused", key)
		}
	}
	for _, inv := range bdflags.Inventories() {
		t.Run(inv.Name, func(t *testing.T) {
			for _, path := range inv.Paths() {
				if path == "" {
					continue
				}
				cmd, allowed := bdSplitAllowlist[path]
				if !allowed {
					r, decided := bdSplitRefusalFor(path)
					if !decided {
						t.Errorf("command %q has no split-city decision: allowlist it (with its flag set) or add a refusal with an alternative", path)
						continue
					}
					if strings.TrimSpace(r.why) == "" || strings.TrimSpace(r.alt) == "" {
						t.Errorf("refusal of %q states no reason or no alternative", path)
					}
					res := resolveBdSplitCommand(inv, strings.Fields(path), "/scope")
					if res.refusal == "" {
						t.Errorf("refused %q resolved without a refusal", path)
					}
					if help := resolveBdSplitCommand(inv, append(strings.Fields(path), "--help"), "/scope"); help.refusal != "" && !inv.Command(path).DisableFlagParsing {
						t.Errorf("--help on refused %q was refused: %s", path, help.refusal)
					}
					continue
				}
				if strings.TrimSpace(cmd.why) == "" {
					t.Errorf("allowlisted %q states no reason", path)
				}
				declared := map[string]bool{}
				for _, f := range inv.Flags(path) {
					declared["--"+f.Name] = true
				}
				for _, f := range append(slices.Clone(cmd.only), cmd.exclude...) {
					if !bdflags.IsCommandPath(path) || !declaredInSomeInventory(path, f) {
						t.Errorf("allowlisted %q vets %s, which no inventory declares for it", path, f)
					}
				}
				res := resolveBdSplitCommand(inv, strings.Fields(path), "/scope")
				if res.refusal != "" || res.path != path {
					t.Errorf("allowlisted %q did not resolve to itself: path=%q refusal=%q", path, res.path, res.refusal)
				}
				// Each vetted flag parses; each unvetted one is refused.
				local := map[string]bool{}
				for _, f := range inv.Command(path).Local {
					local["--"+f.Name] = true
				}
				for _, f := range inv.Flags(path) {
					long := "--" + f.Name
					argv := append(strings.Fields(path), long)
					if f.TakesValue() {
						argv = append(argv, bdFlagSampleValue(f))
					}
					vetted, _ := bdSplitVetted(cmd, local, long)
					got := resolveBdSplitCommand(inv, argv, "/scope")
					if long == "--help" || long == "--version" {
						continue
					}
					if vetted && got.refusal != "" {
						t.Errorf("vetted flag %s of %q was refused: %s", long, path, got.refusal)
					}
					if !vetted && got.refusal == "" {
						t.Errorf("unvetted flag %s of %q was accepted", long, path)
					}
				}
			}
			for alias, target := range inv.Aliases() {
				mapped, allowed := bdSplitAllowedAliases[alias]
				_, refused := bdSplitRefusedAliases[alias]
				switch {
				case allowed && refused:
					t.Errorf("alias %q is both allowed and refused", alias)
				case allowed:
					if mapped != target {
						t.Errorf("alias %q maps to %q, but bd runs it as %q", alias, mapped, target)
					}
					if res := resolveBdSplitCommand(inv, strings.Fields(alias), "/scope"); res.refusal != "" || res.path != target {
						t.Errorf("allowed alias %q resolved to %q (%q)", alias, res.path, res.refusal)
					}
				case refused:
					if res := resolveBdSplitCommand(inv, strings.Fields(alias), "/scope"); res.refusal == "" {
						t.Errorf("refused alias %q resolved to %q", alias, res.path)
					}
				default:
					t.Errorf("alias %q (of %q) has no split-city decision", alias, target)
				}
			}
		})
	}
	for alias := range bdSplitAllowedAliases {
		if _, ok := bdflags.CommandAliases()[alias]; !ok {
			t.Errorf("allowed alias %q is not in any inventory", alias)
		}
	}
	for alias := range bdSplitRefusedAliases {
		if _, ok := bdflags.CommandAliases()[alias]; !ok {
			t.Errorf("refused alias %q is not in any inventory", alias)
		}
	}
}

// declaredInSomeInventory reports whether some inventory declares long on path.
func declaredInSomeInventory(path, long string) bool {
	for _, inv := range bdflags.Inventories() {
		if inv.Command(path) == nil {
			continue
		}
		for _, f := range inv.Flags(path) {
			if "--"+f.Name == long {
				return true
			}
		}
	}
	return false
}

// bdFlagSampleValue is a value pflag accepts for f's type.
func bdFlagSampleValue(f bdflags.InventoryFlag) string {
	switch f.Type {
	case "int", "int64":
		return "1"
	case "float64":
		return "1.5"
	case "duration":
		return "1h"
	default:
		return "v"
	}
}

// TestGcBdSplitRefusesEveryRedirectFlag: every flag in the inventories whose
// name has path/dir/repo/db/server/remote shape is either refused as a store
// redirect or reviewed as one that is not, and every refused redirect is
// refused wherever it appears on an allowlisted command.
func TestGcBdSplitRefusesEveryRedirectFlag(t *testing.T) {
	shape := regexp.MustCompile(`(?i)(path|dir|repo|db|database|server|remote|url|host|port|socket|profile|global|backend|file|output|input|prefix|jsonl|token|export)`)
	for _, inv := range bdflags.Inventories() {
		for _, path := range inv.Paths() {
			for _, f := range inv.Flags(path) {
				long := "--" + f.Name
				if !shape.MatchString(f.Name) {
					continue
				}
				if _, allowed := bdSplitAllowlist[path]; !allowed && path != "" {
					continue // a refused command is refused with every flag it has
				}
				_, refused := bdSplitRedirectFlags[long]
				_, reviewed := bdSplitReviewedPathFlags[long]
				if refused == reviewed && !strings.HasPrefix(f.Name, "proxied-server-") {
					t.Errorf("%s: flag %s of %q has path/store shape and no decision (refuse it in bdSplitRedirectFlags or review it in bdSplitReviewedPathFlags)", inv.Name, long, path)
				}
				if strings.HasPrefix(f.Name, "proxied-server-") && bdSplitAllowlist[path].only == nil && bdSplitAllowlist[path].why != "" {
					t.Errorf("%s: %q vets every flag, including %s", inv.Name, path, long)
				}
			}
		}
		for path := range bdSplitAllowlist {
			if inv.Command(path) == nil {
				continue
			}
			for _, f := range inv.Flags(path) {
				long := "--" + f.Name
				if _, refused := bdSplitRedirectFlags[long]; !refused {
					continue
				}
				argv := append(strings.Fields(path), long)
				if f.TakesValue() {
					argv = append(argv, "v")
				}
				if res := resolveBdSplitCommand(inv, argv, "/scope"); res.refusal == "" {
					t.Errorf("%s: redirect flag %s on %q was accepted", inv.Name, long, path)
				}
			}
		}
	}
	for _, argv := range [][]string{{"-C", "/elsewhere", "update", "a", "--status", "open"}, {"create", "--repo", "x", "--title", "t"}, {"gate", "create", "--repo", "x", "--blocks", "a"}, {"dolt", "push", "--remote", "x"}, {"--db", "x", "list"}} {
		for _, inv := range bdflags.Inventories() {
			if res := resolveBdSplitCommand(inv, argv, "/scope"); res.refusal == "" {
				t.Errorf("%s: gc bd %s was accepted", inv.Name, strings.Join(argv, " "))
			}
		}
	}
}

// TestGcBdSingleStoreForwardsEveryCommandUnchanged pins the single-store
// contract over the whole inventory: no command, alias or flag is refused or
// rewritten (beyond gc's own --skip-infra on import).
func TestGcBdSingleStoreForwardsEveryCommandUnchanged(t *testing.T) {
	cfgs := map[string]*config.City{"no storage": {}, "all on work": {Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding, Sessions: config.StorageWorkBinding,
		Messaging: config.StorageWorkBinding, Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
	}}}}
	argvs := [][]string{{"mol", "fart", "a", "b"}, {"backup", "restore", "/b", "--force=true"}, {"--db", "x", "update", "a", "--type", "message"}, {"sql", "/* */ update issues set issue_type='message'"}}
	for _, p := range bdflags.CommandPaths() {
		argvs = append(argvs, append(strings.Fields(p), "--some-unknown-flag"))
	}
	for alias := range bdflags.CommandAliases() {
		argvs = append(argvs, strings.Fields(alias))
	}
	for name, cfg := range cfgs {
		for _, argv := range argvs {
			g := bdClassFlipGuard(cfg, t.TempDir(), argv, bdClassGuardDeps{read: func(string) (beads.Bead, bdReadResult) {
				t.Fatalf("%s: a single-store city read a bead for %v", name, argv)
				return beads.Bead{}, bdReadFailed
			}})
			if g.refusal != "" || strings.Join(g.args, "\x00") != strings.Join(argv, "\x00") {
				t.Fatalf("%s: gc bd %s was changed: refusal=%q args=%v", name, strings.Join(argv, " "), g.refusal, g.args)
			}
		}
	}
}

// TestGcBdSplitAllowlistIsDocumented keeps the "gc bd on split cities" docs
// section in step with the allowlist.
func TestGcBdSplitAllowlistIsDocumented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "split-storage-classes.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(doc), "## gc bd on split cities")
	if !ok {
		t.Fatal(`docs/runbooks/split-storage-classes.md has no "## gc bd on split cities" section`)
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	for _, path := range bdSplitAllowlistPaths() {
		if !strings.Contains(section, "`"+path+"`") {
			t.Errorf("the docs section does not list allowlisted `%s`", path)
		}
	}
	for alias := range bdSplitAllowedAliases {
		if !strings.Contains(section, "`"+alias+"`") {
			t.Errorf("the docs section does not list allowed alias `%s`", alias)
		}
	}
}

// TestStoreClassHoldsIsNilOnASingleStoreCity pins the single-store contract
// every CLI door relies on: no [storage], or one that keeps every class on the
// work binding, holds nil, and nil disarms coordclass.CheckFlip.
func TestStoreClassHoldsIsNilOnASingleStoreCity(t *testing.T) {
	allWork := &config.City{Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding, Sessions: config.StorageWorkBinding,
		Messaging: config.StorageWorkBinding, Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
	}}}
	for name, cfg := range map[string]*config.City{"nil": nil, "no storage": {}, "all on work": allWork} {
		if got := workStoreClassHolds(cfg); got != nil {
			t.Errorf("%s: work store holds %v, want nil", name, got)
		}
	}
	if got := workStoreClassHolds(splitGuardTestCity()); len(got) != 1 || got[0] != coordclass.ClassWork {
		t.Errorf("split: work store holds %v, want [work]", got)
	}
	if got := storeClassHolds(splitGuardTestCity(), "infra"); len(got) != 5 {
		t.Errorf("split: binding holds %v, want the five infrastructure classes", got)
	}
}

// TestGcBdImportSkipInfraWritesAWorkOnlyCopy checks what --skip-infra hands
// bd, line by line, for a file and for stdin.
func TestGcBdImportSkipInfraWritesAWorkOnlyCopy(t *testing.T) {
	archive := `{"_type":"memory","key":"k","value":"v"}` + "\n" +
		`{"id":"demo-1","title":"w","issue_type":"task","labels":["triage"]}` + "\n" +
		`{"id":"demo-2","title":"m","issue_type":"message"}` + "\n" +
		`{"id":"demo-3","title":"wisp","issue_type":"task","metadata":{"gc.kind":"wisp"}}` + "\n"
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "a.jsonl"), []byte(archive), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound }

	for _, argv := range [][]string{{"import", "a.jsonl", "--skip-infra"}, {"import", "--skip-infra", "-i", "a.jsonl"}, {"import", "--input=a.jsonl", "--skip-infra"}} {
		g := bdClassFlipGuard(splitGuardTestCity(), scope, argv, splitTestDeps(nil, read))
		if g.refusal != "" {
			t.Fatalf("%v refused: %s", argv, g.refusal)
		}
		if slicesContainsString(g.args, "--skip-infra") || slicesContainsString(g.args, "a.jsonl") || slicesContainsString(g.args, "--input=a.jsonl") {
			t.Fatalf("%v forwarded %v; want the work-only copy and no gc flag", argv, g.args)
		}
		var copyPath string
		for _, a := range g.args {
			if i := strings.Index(a, "gc-bd-import-work-"); i >= 0 {
				copyPath = strings.TrimPrefix(a, "--input=")
			}
		}
		data, err := os.ReadFile(copyPath)
		if err != nil {
			t.Fatalf("%v: reading the work-only copy %q: %v", argv, copyPath, err)
		}
		got := string(data)
		if !strings.Contains(got, `"_type":"memory"`) || !strings.Contains(got, `"demo-1"`) || strings.Contains(got, `"demo-2"`) || strings.Contains(got, `"demo-3"`) {
			t.Fatalf("%v: work-only copy = %q", argv, got)
		}
		g.cleanup()
		if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
			t.Errorf("%v: the work-only copy outlived the command", argv)
		}
	}

	g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"import", "-", "--skip-infra"}, splitTestDeps(bytes.NewBufferString(archive), read))
	if g.refusal != "" || g.stdin == nil {
		t.Fatalf("stdin import: refusal=%q stdin=%v", g.refusal, g.stdin)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(g.stdin); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, `"demo-1"`) || strings.Contains(got, `"demo-2"`) || strings.Contains(got, `"demo-3"`) {
		t.Fatalf("stdin import handed bd %q", got)
	}
	if strings.Join(g.args, " ") != "import -" {
		t.Fatalf("stdin import forwarded %v, want [import -]", g.args)
	}
}

func slicesContainsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// splitTestDeps are guard deps for a split-city unit test: the pinned bd 1.3.2,
// no bead held by the binding, no template beads, and a bd that lists no
// molecule members, no dependents and no abandoned wisps.
func splitTestDeps(stdin io.Reader, read func(string) (beads.Bead, bdReadResult)) bdClassGuardDeps {
	return bdClassGuardDeps{
		stdin:             stdin,
		inventory:         func() (*bdflags.Inventory, error) { return bdflags.InventoryByName("bd-1.3.2"), nil },
		read:              read,
		held:              func([]string) (map[string]bool, error) { return map[string]bool{}, nil },
		bindingDependents: func([]string) (map[string][]string, error) { return map[string][]string{}, nil },
		templates:         func() ([]beads.Bead, error) { return nil, nil },
		bdRead:            fakeBdRead(nil, nil, nil),
	}
}

// fakeBdRead is a bd answering the judges' reads as bd's JSON does: a `list
// --parent=<id>` with the beads children[id] names, a `dep list
// --direction=up --json <id>...` with the beads dependents[id] names for
// every id it asks about, and the --dry-run --json of a `mol wisp gc` with
// wisps as the ids it would clean. Any other read is an error.
func fakeBdRead(children, dependents map[string][]string, wisps []string) func(args ...string) ([]byte, error) {
	return func(args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "list" {
			if parent, ok := strings.CutPrefix(args[1], "--parent="); ok {
				rows := []map[string]string{}
				for _, id := range children[parent] {
					rows = append(rows, map[string]string{"id": id})
				}
				return json.Marshal(rows)
			}
		}
		if up := []string{"dep", "list", "--direction=up", "--json"}; len(args) > len(up) && slices.Equal(args[:len(up)], up) {
			rows := []map[string]string{}
			for _, id := range args[len(up):] {
				for _, d := range dependents[id] {
					rows = append(rows, map[string]string{"id": d, "dependency_type": "blocks"})
				}
			}
			return json.Marshal(rows)
		}
		if strings.Contains(strings.Join(args, " "), "mol wisp gc") && len(args) >= 2 && slices.Equal(args[len(args)-2:], []string{"--dry-run", "--json"}) {
			cleaned := append([]string{}, wisps...)
			return json.Marshal(map[string]any{"cleaned_ids": cleaned, "cleaned_count": 0, "dry_run": true})
		}
		return nil, fmt.Errorf("the fake bd has no answer for %v", args)
	}
}

// pinTestBdInventory makes doBd treat the scope's bd (a shell fake with no
// real command tree) as the pinned bd 1.3.2 build, for tests of the
// passthrough on a split city. The trust policy itself is tested on its own
// (bd_inventory_trust_test.go).
func pinTestBdInventory(t testing.TB) {
	t.Helper()
	prev := bdSplitInventoryResolver
	bdSplitInventoryResolver = func(string, *config.City, execStoreTarget) (*bdflags.Inventory, error) {
		return bdflags.InventoryByName("bd-1.3.2"), nil
	}
	t.Cleanup(func() { bdSplitInventoryResolver = prev })
}
