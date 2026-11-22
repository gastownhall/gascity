package bdflags

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func cloneHelpTree(t HelpTree) HelpTree {
	out := HelpTree{}
	for k, v := range t {
		v.Aliases = append([]string(nil), v.Aliases...)
		v.Subcommands = append([]string(nil), v.Subcommands...)
		out[k] = v
	}
	return out
}

// TestMatchHelpTreeMatchesTreesNotVersions pins the trust policy: a tree equal
// to a pinned projection matches that inventory, and OSS main's kind of tree —
// it reports "1.3.2" like the tag, but adds a root command and rewords one
// flag's usage — matches none.
func TestMatchHelpTreeMatchesTreesNotVersions(t *testing.T) {
	for _, inv := range Inventories() {
		got, diffs := MatchHelpTree(cloneHelpTree(inv.HelpTree()))
		if got != inv {
			t.Fatalf("%s's own projection matched %v (diffs %v)", inv.Name, got, diffs)
		}
	}

	oss := cloneHelpTree(InventoryByName("bd-1.3.2").HelpTree())
	root := oss[""]
	root.Subcommands = append(root.Subcommands, "versions")
	oss[""] = root
	oss["versions"] = HelpView{GlobalFlags: oss["list"].GlobalFlags}
	compact := oss["admin compact"]
	compact.Flags = strings.Replace(compact.Flags, "on .beads/dolt", "on the active database", 1)
	oss["admin compact"] = compact

	got, diffs := MatchHelpTree(oss)
	if got != nil {
		t.Fatalf("OSS main's tree matched %s", got.Name)
	}
	joined := strings.Join(diffs["bd-1.3.2"], "\n")
	for _, want := range []string{`"versions"`, `"admin compact" flags differ`} {
		if !strings.Contains(joined, want) {
			t.Errorf("bd-1.3.2 diffs do not name %s:\n%s", want, joined)
		}
	}
	if InventoryByName("bd-1.3.2").HelpTree().Fingerprint() == oss.Fingerprint() {
		t.Error("the fingerprint does not change with the tree")
	}
}

// TestParseHelpReadsCobraHelp checks ParseHelp against cobra's own help
// rendering of a pinned command.
func TestParseHelpReadsCobraHelp(t *testing.T) {
	inv := InventoryByName("bd-1.3.2")
	_, byCmd := inv.buildCobra()
	for c, p := range byCmd {
		if p != "update" {
			continue
		}
		var b strings.Builder
		c.SetOut(&b)
		if err := c.Help(); err != nil {
			t.Fatal(err)
		}
		got := ParseHelp(b.String(), false)
		want := inv.HelpTree()["update"]
		if got.Flags != want.Flags || got.GlobalFlags != want.GlobalFlags || strings.Join(got.Aliases, ",") != strings.Join(want.Aliases, ",") {
			t.Errorf("ParseHelp(update --help) = %+v\nwant %+v", got, want)
		}
		return
	}
	t.Fatal("no update command in the mirror")
}

// TestInventoryProvenanceMatchesGoMod is the should-fix: every inventory
// records where it came from, and the cobra/pflag bd was built with equal the
// versions gc's mirror resolves with (go.mod), so Resolve parses as bd does.
func TestInventoryProvenanceMatchesGoMod(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var gomod []byte
	for {
		if gomod, err = os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
	version := func(module string) string {
		m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(module) + `\s+(\S+)`).FindSubmatch(gomod)
		if m == nil {
			t.Fatalf("go.mod has no %s", module)
		}
		return string(m[1])
	}
	cobraV, pflagV := version("github.com/spf13/cobra"), version("github.com/spf13/pflag")
	for _, inv := range Inventories() {
		p := inv.Provenance
		if p.Commit == "" || p.GoVersion == "" || p.BuildTags == "" || p.CGOEnabled == "" {
			t.Errorf("%s provenance is incomplete: %+v", inv.Name, p)
		}
		if p.CobraVersion != cobraV || p.PflagVersion != pflagV {
			t.Errorf("%s was built with cobra %s / pflag %s; gc's mirror uses cobra %s / pflag %s", inv.Name, p.CobraVersion, p.PflagVersion, cobraV, pflagV)
		}
	}
}

// TestEmbeddedInventoriesMatchTheirJSON: the binary embeds each inventory
// gzipped; the gzipped copy must decompress to the reviewed JSON beside it,
// and every JSON must have its embedded copy.
func TestEmbeddedInventoriesMatchTheirJSON(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("inventory", "*.json"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no inventory JSON found (%v)", err)
	}
	if len(names) != len(Inventories()) {
		t.Errorf("%d inventory JSON files, %d embedded inventories", len(names), len(Inventories()))
	}
	for _, name := range names {
		want, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readGzipInventory(path.Join("inventory", filepath.Base(name)+".gz"))
		if err != nil {
			t.Errorf("%s has no embedded gzipped copy: %v (run gzip -n -9 -c %s > %s.gz)", name, err, name, name)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s.gz does not decompress to %s; regenerate it with gzip -n -9 -c %s > %s.gz", name, name, name, name)
		}
	}
}

// TestDiffHelpTreesReportsGlobalFlags: a binary whose inherited (global)
// flags differ — a new persistent flag on the root — matches no inventory.
func TestDiffHelpTreesReportsGlobalFlags(t *testing.T) {
	pinned := InventoryByName("bd-1.3.2").HelpTree()
	live := cloneHelpTree(pinned)
	v := live["list"]
	v.GlobalFlags += "\n      --sneaky string   a new persistent flag"
	live["list"] = v
	d := DiffHelpTrees(pinned, live, 8)
	if len(d) != 1 || d[0] != `"list" global flags differ` {
		t.Fatalf("diffs = %v, want the list global-flags difference", d)
	}
	if inv, _ := MatchHelpTree(live); inv != nil {
		t.Fatalf("a tree with an extra global flag matched %s", inv.Name)
	}
}
