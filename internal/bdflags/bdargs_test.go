package bdflags

import (
	"reflect"
	"strings"
	"testing"
)

// TestSplitGlobalFlagsSkipsGlobalFlagValues pins that a global flag's VALUE is
// never mistaken for the subcommand. Taking the first non-dash token reads
// "bob" out of `bd --actor bob update <id> ...`, so anything keyed off the
// subcommand is bypassed by a token the caller never meant as a verb.
func TestSplitGlobalFlagsSkipsGlobalFlagValues(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantVerb string
		wantRest []string
	}{
		{"plain", []string{"update", "bd-1"}, "update", []string{"bd-1"}},
		{"--actor", []string{"--actor", "bob", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"-C dir", []string{"-C", "/some/dir", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"--db", []string{"--db", "/x/y.db", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"--directory", []string{"--directory", "/d", "close", "bd-1"}, "close", []string{"bd-1"}},
		{"inline form consumes nothing", []string{"--actor=bob", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"bool global", []string{"--json", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"stacked", []string{"--actor", "bob", "--json", "-C", "/d", "update", "bd-1"}, "update", []string{"bd-1"}},
		{"no verb", []string{"--actor", "bob"}, "", nil},
		{"empty", nil, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verb, rest := SplitGlobalFlags(tc.args)
			if verb != tc.wantVerb {
				t.Errorf("verb = %q, want %q", verb, tc.wantVerb)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

// TestGlobalValueFlagsIsComplete pins the global value-flag set against bd's
// own persistent-flag list. A flag missing from this table silently reopens the
// bypass below: SplitGlobalFlags would read that flag's value as the verb, and
// every guard keyed off the verb stops firing — with no test failing.
//
// Filing a value flag as a BOOL is the same bypass wearing a different hat: the
// flag is skipped without its value, and the value is read as the verb. That is
// how `--profile` sat in the bool table while bd declared it `--profile string`,
// so `bd --profile <name> create …` resolved the verb to <name> and every
// create-side guard went quiet. Both directions are pinned here and in
// TestGlobalBoolFlagsIsComplete, which is why the two sets are compared exactly
// rather than merged.
//
// Sourced from `bd --help` (bd 1.3.0-rc.2). bd declares exactly seven persistent
// flags that consume the next argument; -C and --directory are the two spellings
// of one of them. --format is a hidden alias for --json and still takes a value,
// so it belongs here, not in the boolean half. Every other persistent flag
// (--cpu-profile, --global, --ignore-schema-skew, --json, --no-color, -q/--quiet,
// --readonly, --sandbox, -v/--verbose, -h/--help, -V/--version) is boolean and
// consumes nothing.
//
// --profile and --server-url are gone from OSS bd as of the RC, but the bd
// 1.3.0 Enterprise build the fleet runs still declares both as `string`
// value flags, so they are carried as value flags: filed as bools (or left
// out) they would read their value as the verb on the fleet's bd. OSS bd
// rejects them, so the superset costs nothing there.
func TestGlobalValueFlagsIsComplete(t *testing.T) {
	want := map[string]bool{
		"--actor": true, "--database": true, "--db": true, "-C": true,
		"--directory": true, "--dolt-auto-commit": true, "--format": true,
		"--mem-profile": true, "--profile": true, "--server-url": true,
	}
	if got := GlobalValueFlags(); !reflect.DeepEqual(got, want) {
		t.Errorf("GlobalValueFlags() = %v, want %v; re-check `bd --help` persistent flags", got, want)
	}
}

// TestGlobalBoolFlagsIsComplete is the same pin for the valueless half. A
// caller that must distinguish a KNOWN valueless flag from an unrecognized one
// — because the unrecognized case means the verb is no longer decidable and the
// guard has to fall back — reads a flag missing from this table as unknown, and
// silently takes the ambiguous branch for an ordinary bd invocation.
//
// Sourced from `bd --help` (bd 1.3.0-rc.2), the same pass as the value-flag
// table. Note --cpu-profile: bd renamed --profile with no alias, so the old
// spelling is now an unknown flag bd rejects.
func TestGlobalBoolFlagsIsComplete(t *testing.T) {
	want := map[string]bool{
		"--cpu-profile": true, "--global": true, "--ignore-schema-skew": true,
		"--json": true, "--no-color": true, "-q": true, "--quiet": true,
		"--readonly": true, "--sandbox": true, "-v": true, "--verbose": true,
		"-h": true, "--help": true, "-V": true, "--version": true,
	}
	if got := GlobalBoolFlags(); !reflect.DeepEqual(got, want) {
		t.Errorf("GlobalBoolFlags() = %v, want %v; re-check `bd --help` persistent flags", got, want)
	}
}

// TestRefusalFiresBehindAGlobalFlag composes the two halves of the guard the way
// the caller does — locate the verb, then judge its args — and pins that a
// global value-flag before the verb does not disarm it.
//
// Testing SplitGlobalFlags and DroppedMetadataRefusal only in isolation leaves
// the composition untested, and the composition is where the bypass lived:
// `bd --actor bob update <id> --set-metadata a=1 b=2` yielded verb "bob", the
// refusal is scoped to "update", so it never fired and bd performed the silent
// 1-of-N write the guard exists to prevent.
func TestRefusalFiresBehindAGlobalFlag(t *testing.T) {
	prefixes := [][]string{
		{"--actor", "bob"},
		{"-C", "/some/dir"},
		{"--db", "/x/y.db"},
		{"--directory", "/d"},
		{"--dolt-auto-commit", "off"},
		{"--actor", "bob", "--json", "-C", "/d"},
	}
	for _, prefix := range prefixes {
		args := append(append([]string{}, prefix...), "update", "bd-1", "--set-metadata", "a=1", "b=2")
		verb, rest := SplitGlobalFlags(args)
		msg, ok := DroppedMetadataRefusal("gc bd", verb, rest)
		if !ok {
			t.Errorf("prefix %v: refusal did not fire (verb=%q); the silent 1-of-N write survives", prefix, verb)
			continue
		}
		if !contains(msg, "b=2") {
			t.Errorf("prefix %v: message %q does not name the dropped pair", prefix, msg)
		}
	}
}

// TestPositionalsKnowsEveryValueTakingFlag is the drift guard: positional
// detection must consume the value of EVERY value-taking flag for the
// subcommand. With a partial set, the value of any omitted flag is read as a
// positional — which is how `update <id> --add-label role=worker` came to look
// like a stray key=value token.
func TestPositionalsKnowsEveryValueTakingFlag(t *testing.T) {
	for flag := range ValueFlags("update") {
		got := Positionals("update", []string{"bd-1", flag, "role=worker"})
		if len(got) != 1 || got[0] != "bd-1" {
			t.Errorf("Positionals(update, bd-1 %s role=worker) = %v; the flag's value was read as an id", flag, got)
		}
	}
}

// TestDroppedMetadataPairs pins detection in both directions.
func TestDroppedMetadataPairs(t *testing.T) {
	dropped := [][]string{
		{"bd-1", "--set-metadata", "a=1", "b=2"},
		{"bd-1", "--set-metadata", "a=1", "b=2", "c=3"},
		{"bd-1", "--set-metadata=a=1", "b=2"},
	}
	for _, args := range dropped {
		if len(DroppedMetadataPairs(args)) == 0 {
			t.Errorf("DroppedMetadataPairs(%v) = none; want the dropped pair caught", args)
		}
	}
	valid := [][]string{
		{"bd-1", "--set-metadata", "a=1"},
		{"bd-1", "--set-metadata", "a=1", "--set-metadata", "b=2"},
		{"bd-1", "--add-label", "role=worker"},
		{"bd-1", "--set-labels", "a=b"},
		{"bd-1", "--external-ref", "https://example.test/i/ABC-1?tab=activity"},
		{"bd-1", "--metadata", `{"url":"https://x?a=b"}`},
		{"bd-1", "bd-2", "--set-metadata", "a=1"},
	}
	for _, args := range valid {
		if got := DroppedMetadataPairs(args); len(got) != 0 {
			t.Errorf("DroppedMetadataPairs(%v) = %v; this is a valid invocation", args, got)
		}
	}
}

// TestDroppedMetadataRefusalOnlyUpdate pins that the refusal is scoped to update.
func TestDroppedMetadataRefusalOnlyUpdate(t *testing.T) {
	if _, ok := DroppedMetadataRefusal("gc bd", "create", []string{"t", "--set-metadata", "a=1", "b=2"}); ok {
		t.Error("refusal fired for create; --set-metadata is an update flag")
	}
	msg, ok := DroppedMetadataRefusal("gc bd", "update", []string{"bd-1", "--set-metadata", "a=1", "b=2"})
	if !ok {
		t.Fatal("refusal did not fire for update")
	}
	for _, want := range []string{"b=2", "--set-metadata", "exits 0"} {
		if !contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

// TestSplitGlobalFlagsStrictReadsTheCommandCobraRuns pins the strict split
// against the way bd's cobra resolves a subcommand: before it only the global
// flags are read, a separate help flag takes the next token (cobra has not
// declared it yet), and anything else makes the subcommand unreadable rather
// than guessed. `--reason show close <id>` is the bypass: SplitGlobalFlags
// reads "show", and bd runs close.
func TestSplitGlobalFlagsStrictReadsTheCommandCobraRuns(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantVerb string
		wantRest []string
		wantOK   bool
	}{
		{"plain", []string{"update", "bd-1"}, "update", []string{"bd-1"}, true},
		{"value global", []string{"--actor", "bob", "update", "bd-1"}, "update", []string{"bd-1"}, true},
		{"inline value global", []string{"--actor=bob", "update", "bd-1"}, "update", []string{"bd-1"}, true},
		{"-C dir", []string{"-C", "/d", "close", "bd-1"}, "close", []string{"bd-1"}, true},
		{"-C=dir", []string{"-C=/d", "close", "bd-1"}, "close", []string{"bd-1"}, true},
		{"value that looks like a flag", []string{"--actor", "--json", "close", "bd-1"}, "close", []string{"bd-1"}, true},
		{"bool global", []string{"--json", "-q", "update", "bd-1"}, "update", []string{"bd-1"}, true},
		{"root's own -V", []string{"-V", "update"}, "update", nil, true},
		{"help takes the next token", []string{"-h", "show", "close", "bd-1"}, "close", []string{"bd-1"}, true},
		{"inline help takes nothing", []string{"--help=false", "close", "bd-1"}, "close", []string{"bd-1"}, true},
		{"unknown long flag", []string{"--reason", "show", "close", "bd-1"}, "", nil, false},
		{"unknown short flag", []string{"-r", "show", "close", "bd-1"}, "", nil, false},
		{"unknown inline flag", []string{"--reason=x", "close", "bd-1"}, "", nil, false},
		{"shorthand run", []string{"-qv", "close", "bd-1"}, "", nil, false},
		{"dash-dash", []string{"--", "close", "bd-1"}, "", nil, false},
		{"empty token", []string{"", "close", "bd-1"}, "", nil, false},
		{"no subcommand", []string{"--actor", "bob"}, "", nil, true},
		{"help alone", []string{"--help"}, "", nil, true},
		{"empty", nil, "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verb, rest, ok := SplitGlobalFlagsStrict(tc.args)
			sameRest := len(rest) == 0 && len(tc.wantRest) == 0 || reflect.DeepEqual(rest, tc.wantRest)
			if verb != tc.wantVerb || ok != tc.wantOK || !sameRest {
				t.Errorf("SplitGlobalFlagsStrict(%q) = %q, %q, %v; want %q, %q, %v", tc.args, verb, rest, ok, tc.wantVerb, tc.wantRest, tc.wantOK)
			}
		})
	}
}

// TestSplitSubcommandStrictReadsTheSubVerb pins the sub-verb half, read from
// the argv without its subcommand: the persistent global flags may come before
// the sub-verb, wherever they stand, the root's own -V may not (`bd -V comments
// add` reads [-V add], and cobra takes "add" as -V's value), and the sub-verb's
// argv keeps every other token in order.
func TestSplitSubcommandStrictReadsTheSubVerb(t *testing.T) {
	cases := []struct {
		name        string
		rest        []string
		wantSub     string
		wantSubArgs []string
		wantOK      bool
	}{
		{"plain", []string{"add", "bd-1", "x"}, "add", []string{"bd-1", "x"}, true},
		{"bool global first", []string{"--json", "add", "bd-1"}, "add", []string{"--json", "bd-1"}, true},
		{"value global first", []string{"--actor", "x", "add", "bd-1"}, "add", []string{"--actor", "x", "bd-1"}, true},
		{"help takes the next token", []string{"-h", "list", "add", "bd-1"}, "add", []string{"-h", "list", "bd-1"}, true},
		{"root's own -V", []string{"-V", "add", "bd-1"}, "", nil, false},
		{"the verb's own flag", []string{"--local-time", "add", "bd-1"}, "", nil, false},
		{"dash-dash", []string{"--", "add", "bd-1"}, "", nil, false},
		{"no sub-verb", []string{"--json"}, "", []string{"--json"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, subArgs, ok := SplitSubcommandStrict(tc.rest)
			if sub != tc.wantSub || ok != tc.wantOK || (ok && !reflect.DeepEqual(subArgs, tc.wantSubArgs)) {
				t.Errorf("SplitSubcommandStrict(%q) = %q, %q, %v; want %q, %q, %v", tc.rest, sub, subArgs, ok, tc.wantSub, tc.wantSubArgs, tc.wantOK)
			}
		})
	}
}

// TestSplitStrictAgreesWithCobra checks the strict split against cobra itself:
// every argv over a small token alphabet that the split reads is resolved by
// the pinned inventories' cobra mirror (the cobra version bd runs) to the
// command the split names, sub-verb included. The help flag is left out of the
// alphabet: the mirror declares it before resolving, which bd does not (its
// own case is pinned above).
func TestSplitStrictAgreesWithCobra(t *testing.T) {
	alphabet := []string{
		"--actor", "x", "--actor=x", "-C", "--json", "-q", "-V", "--reason", "-r",
		"--", "", "show", "close", "comments", "add", "bt-1", "new", "list", "dep",
	}
	var argvs [][]string
	var gen func(prefix []string)
	gen = func(prefix []string) {
		if len(prefix) > 0 {
			argvs = append(argvs, append([]string(nil), prefix...))
		}
		if len(prefix) == 4 {
			return
		}
		for _, tok := range alphabet {
			gen(append(prefix, tok))
		}
	}
	gen(nil)
	for _, inv := range Inventories() {
		root, byCmd := inv.buildCobra()
		aliases := inv.Aliases()
		canonical := func(p string) string {
			if to, ok := aliases[p]; ok {
				return to
			}
			return p
		}
		read := 0
		for _, argv := range argvs {
			verb, rest, ok := SplitGlobalFlagsStrict(argv)
			if !ok {
				continue
			}
			cmd, _, err := root.Find(argv)
			if err != nil {
				continue // bd runs nothing
			}
			read++
			path := strings.Fields(byCmd[cmd])
			if verb == "" {
				if len(path) != 0 {
					t.Errorf("%s: %q: the split reads no command, cobra runs %q", inv.Name, argv, byCmd[cmd])
				}
				continue
			}
			want := canonical(verb)
			if len(path) == 0 || path[0] != want {
				t.Errorf("%s: %q: the split reads %q, cobra runs %q", inv.Name, argv, want, byCmd[cmd])
				continue
			}
			if len(inv.Subcommands(want)) == 0 {
				continue
			}
			at := len(argv) - len(rest) - 1
			sub, _, subOK := SplitSubcommandStrict(append(argv[:at:at], argv[at+1:]...))
			if !subOK {
				continue
			}
			if c := inv.Command(canonical(want + " " + sub)); sub != "" && c != nil {
				if len(path) < 2 || want+" "+path[1] != c.Path {
					t.Errorf("%s: %q: the split reads sub-verb %q, cobra runs %q", inv.Name, argv, sub, byCmd[cmd])
				}
			} else if len(path) != 1 {
				t.Errorf("%s: %q: the split reads no sub-verb, cobra runs %q", inv.Name, argv, byCmd[cmd])
			}
		}
		if read < 1000 {
			t.Fatalf("%s: only %d argvs were read and resolved; the alphabet no longer exercises the split", inv.Name, read)
		}
	}
}
