package bdflags

import (
	"reflect"
	"strings"
	"testing"
)

var parseSpecs = []FlagSpec{
	{"--type", "-t", true},
	{"--assignee", "-a", true},
	{"--dry-run", "", false},
	{"--quiet", "-q", false},
	{"--verbose", "-v", false},
	{"--force", "-f", false},
	{"--persist", "", false},
	{"--prefix", "", true},
}

// TestParseArgsMatchesPflag pins the parser against pflag's rules, including
// the shapes the review reproduced: a value flag consumes the next token even
// when it spells a flag, and a bool takes =true/=false.
func TestParseArgsMatchesPflag(t *testing.T) {
	for _, c := range []struct {
		name  string
		args  []string
		flags []ParsedFlag
		pos   []string
		err   string
	}{
		{"value consumes a flag-shaped token", []string{"--assignee", "--dry-run", "x"}, []ParsedFlag{{"--assignee", "--dry-run", false}}, []string{"x"}, ""},
		{"prefix consumes --dry-run", []string{"f", "--prefix", "--dry-run", "--persist"}, []ParsedFlag{{"--prefix", "--dry-run", false}, {"--persist", "true", true}}, []string{"f"}, ""},
		{"bool =true", []string{"--persist=true"}, []ParsedFlag{{"--persist", "true", true}}, nil, ""},
		{"bool =false", []string{"--force=false", "id"}, []ParsedFlag{{"--force", "false", true}}, []string{"id"}, ""},
		{"bool =1 normalizes", []string{"--force=1"}, []ParsedFlag{{"--force", "true", true}}, nil, ""},
		{"bad bool", []string{"--force=maybe"}, nil, nil, "invalid argument"},
		{"inline value", []string{"--type=message"}, []ParsedFlag{{"--type", "message", false}}, nil, ""},
		{"attached shorthand", []string{"-tmessage"}, []ParsedFlag{{"--type", "message", false}}, nil, ""},
		{"shorthand =", []string{"-t=message"}, []ParsedFlag{{"--type", "message", false}}, nil, ""},
		{"separated shorthand", []string{"-t", "message"}, []ParsedFlag{{"--type", "message", false}}, nil, ""},
		{"combined bools then value", []string{"-qvtmessage"}, []ParsedFlag{{"--quiet", "true", true}, {"--verbose", "true", true}, {"--type", "message", false}}, nil, ""},
		{"combined bools then separated value", []string{"-qt", "message"}, []ParsedFlag{{"--quiet", "true", true}, {"--type", "message", false}}, nil, ""},
		{"short bool =false", []string{"-f=false"}, []ParsedFlag{{"--force", "false", true}}, nil, ""},
		{"interspersed", []string{"a", "-q", "b"}, []ParsedFlag{{"--quiet", "true", true}}, []string{"a", "b"}, ""},
		{"double dash", []string{"a", "--", "--type", "-q"}, nil, []string{"a", "--type", "-q"}, ""},
		{"lone dash is positional", []string{"-"}, nil, []string{"-"}, ""},
		{"unknown long", []string{"--nope"}, nil, nil, "unknown flag"},
		{"unknown short", []string{"-z"}, nil, nil, "unknown shorthand"},
		{"unknown in a cluster", []string{"-qz"}, nil, nil, "unknown shorthand"},
		{"missing value", []string{"--type"}, nil, nil, "needs an argument"},
		{"bad syntax", []string{"---type"}, nil, nil, "bad flag syntax"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseArgs(parseSpecs, c.args)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Flags, c.flags) || !reflect.DeepEqual(got.Positionals, c.pos) {
				t.Fatalf("got flags=%v pos=%v, want flags=%v pos=%v", got.Flags, got.Positionals, c.flags, c.pos)
			}
		})
	}
	p, _ := ParseArgs(parseSpecs, []string{"--persist", "--persist=false"})
	if v, set := p.BoolValue("--persist"); v || !set {
		t.Fatalf("last bool occurrence must win: %v %v", v, set)
	}
}

// TestInventoriesAreWellFormed checks each pinned inventory: a root, every
// path's parent present, aliases naming real commands, flag types pflag
// knows, and lookup by id.
func TestInventoriesAreWellFormed(t *testing.T) {
	known := map[string]bool{"bool": true, "string": true, "int": true, "int64": true, "float64": true, "duration": true, "stringSlice": true, "stringArray": true}
	if len(Inventories()) < 2 {
		t.Fatalf("want the bd 1.3.2 and Enterprise inventories, got %d", len(Inventories()))
	}
	for _, inv := range Inventories() {
		if inv.Command("") == nil {
			t.Errorf("%s has no root command", inv.Name)
		}
		for _, p := range inv.Paths() {
			if p == "" {
				continue
			}
			parent, _ := splitParent(p)
			if inv.Command(parent) == nil {
				t.Errorf("%s: %q has no parent %q", inv.Name, p, parent)
			}
			for _, f := range inv.Flags(p) {
				if !known[f.Type] {
					t.Errorf("%s: %q flag --%s has type %q the mirror does not model", inv.Name, p, f.Name, f.Type)
				}
			}
		}
		for alias, target := range inv.Aliases() {
			if inv.Command(target) == nil {
				t.Errorf("%s: alias %q names unknown %q", inv.Name, alias, target)
			}
		}
	}
	if InventoryByName("bd-1.3.2") == nil || InventoryByName("bd-1.3.0-enterprise-77612097cec0") == nil || InventoryByName("nope") != nil {
		t.Error("InventoryByName does not find the pinned inventories by id")
	}
}
