package bdflags

import (
	"io"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// fuzzTokens builds a token pool from an inventory: command words, every flag
// spelling (long, short, inline values, bool =true/false), short clusters
// ending in value flags, and plain values that also spell command names.
func fuzzTokens(inv *Inventory, paths []string) []string {
	seen := map[string]bool{}
	var pool []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			pool = append(pool, s)
		}
	}
	for _, p := range paths {
		for _, w := range strings.Fields(p) {
			add(w)
		}
		c := inv.Command(p)
		for _, a := range c.Aliases {
			add(a)
		}
		for _, f := range inv.Flags(p) {
			add("--" + f.Name)
			if f.TakesValue() {
				add("--" + f.Name + "=x")
			} else {
				add("--" + f.Name + "=false")
				add("--" + f.Name + "=true")
			}
			if f.Shorthand != "" {
				add("-" + f.Shorthand)
				add("-q" + f.Shorthand)
				add("-" + f.Shorthand + "v")
			}
		}
	}
	for _, s := range []string{"--", "-", ".", "x", "demo-1", "a,b", `"q"`, "-C", "-qC", "--json", "-1"} {
		add(s)
	}
	return pool
}

// TestResolveMatchesCobraExecuteC fuzzes Resolve (cobra Find, then pflag
// Parse on the found command) against cobra's own ExecuteC on the same mirror
// tree: the command run, its flags and its positionals must agree for every
// argv either accepts.
func TestResolveMatchesCobraExecuteC(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, inv := range Inventories() {
		pool := fuzzTokens(inv, []string{"", "update", "create", "swarm create", "merge-slot create", "human respond", "mol wisp", "mol wisp create", "dep add", "label add", "cook", "close"})
		for n := 0; n < 1500; n++ {
			argv := make([]string, 1+rng.Intn(6))
			for i := range argv {
				argv[i] = pool[rng.Intn(len(pool))]
			}
			if slices.ContainsFunc(argv, func(s string) bool {
				return s == "help" || s == "completion" || strings.HasPrefix(s, "--help") || s == "-h" || strings.Contains(s, "h") && strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "--")
			}) {
				continue
			}
			res, rerr := inv.Resolve(argv)
			root, byCmd := inv.buildCobra()
			var ran *cobra.Command
			var ranArgs []string
			var walk func(c *cobra.Command)
			walk = func(c *cobra.Command) {
				if c.Run != nil {
					c.Run = func(cmd *cobra.Command, args []string) { ran, ranArgs = cmd, args }
				}
				for _, s := range c.Commands() {
					walk(s)
				}
			}
			walk(root)
			root.SetArgs(argv)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			_, xerr := root.ExecuteC()
			if xerr != nil || ran == nil {
				if rerr == nil && !res.Help && inv.Command(res.Path).Runnable {
					t.Fatalf("%s: %q: Resolve accepted %q, ExecuteC refused (%v)", inv.Name, argv, res.Path, xerr)
				}
				continue
			}
			if rerr != nil {
				t.Fatalf("%s: %q: ExecuteC ran %q, Resolve refused (%v)", inv.Name, argv, byCmd[ran], rerr)
			}
			if byCmd[ran] != res.Path || !reflect.DeepEqual(append([]string{}, ranArgs...), append([]string{}, res.Positionals...)) {
				t.Fatalf("%s: %q: ExecuteC ran %q %q, Resolve says %q %q", inv.Name, argv, byCmd[ran], ranArgs, res.Path, res.Positionals)
			}
			got := map[string][]string{}
			ran.Flags().Visit(func(f *pflag.Flag) {
				if sv, ok := f.Value.(pflag.SliceValue); ok {
					got["--"+f.Name] = sv.GetSlice()
					return
				}
				got["--"+f.Name] = []string{f.Value.String()}
			})
			if !reflect.DeepEqual(got, res.Flags) {
				t.Fatalf("%s: %q: flags differ: ExecuteC %v, Resolve %v", inv.Name, argv, got, res.Flags)
			}
		}
	}
}

// TestParseArgsMatchesRealPflag fuzzes ParseArgs (gc's pflag reading, used to
// cross-check cobra's command lookup) against pflag v1.0.10 itself.
func TestParseArgsMatchesRealPflag(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for _, inv := range Inventories() {
		for _, path := range []string{"update", "create", "swarm create", "mol wisp", "dep add"} {
			pool := fuzzTokens(inv, []string{path})
			specs := inv.FlagSpecs(path)
			for n := 0; n < 2000; n++ {
				argv := make([]string, 1+rng.Intn(6))
				for i := range argv {
					argv[i] = pool[rng.Intn(len(pool))]
				}
				fs := pflag.NewFlagSet("x", pflag.ContinueOnError)
				fs.SetOutput(io.Discard)
				for _, f := range inv.Flags(path) {
					addMirrorFlag(fs, f)
				}
				perr := fs.Parse(argv)
				parsed, gerr := ParseArgs(specs, argv)
				if perr != nil && gerr == nil && strings.Contains(perr.Error(), "invalid argument") && !strings.Contains(perr.Error(), "ParseBool") {
					// A typed value pflag rejects (an int, a duration):
					// ParseArgs reads token shapes only; the mirror tree
					// does the typed parse.
					continue
				}
				if (perr == nil) != (gerr == nil) {
					t.Fatalf("%s %s: %q: pflag err=%v, ParseArgs err=%v", inv.Name, path, argv, perr, gerr)
				}
				if perr != nil {
					continue
				}
				if !reflect.DeepEqual(append([]string{}, fs.Args()...), append([]string{}, parsed.Positionals...)) {
					t.Fatalf("%s %s: %q: positionals pflag=%q ParseArgs=%q", inv.Name, path, argv, fs.Args(), parsed.Positionals)
				}
				var changed []string
				fs.Visit(func(f *pflag.Flag) { changed = append(changed, "--"+f.Name) })
				var ours []string
				for _, f := range parsed.Flags {
					if !slices.Contains(ours, f.Long) {
						ours = append(ours, f.Long)
					}
				}
				slices.Sort(changed)
				slices.Sort(ours)
				if !slices.Equal(changed, ours) {
					t.Fatalf("%s %s: %q: flags pflag=%v ParseArgs=%v", inv.Name, path, argv, changed, ours)
				}
			}
		}
	}
}

// TestResolveFollowsCobraNotPflag pins the review's repros: for a short-flag
// run ending in a value flag, cobra's lookup reads the next token as the
// subcommand while pflag hands it to the flag. Resolve answers cobra's way —
// the command bd actually runs.
func TestResolveFollowsCobraNotPflag(t *testing.T) {
	for _, inv := range Inventories() {
		for argv, want := range map[string]string{
			"-qC swarm create . demo-1":  "swarm create",
			"-qC merge-slot create .":    "merge-slot create",
			"human -qC respond . demo-1": "human respond",
		} {
			res, err := inv.Resolve(strings.Fields(argv))
			if err != nil || res.Path != want {
				t.Errorf("%s: Resolve(%q) = %q, %v; want %q", inv.Name, argv, res.Path, err, want)
			}
			if !ClusterTakesNextToken(inv.FlagSpecs(want), "-qC") {
				t.Errorf("%s: -qC not flagged as a cluster taking the next token", inv.Name)
			}
		}
	}
}
