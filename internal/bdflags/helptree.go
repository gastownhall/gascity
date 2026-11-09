package bdflags

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

// The bd trust policy matches COMMAND TREES, not version strings: every
// Enterprise build reports 1.3.0 and OSS main reports 1.3.2 with a tree
// different from the tag's, so a version line cannot say which commands and
// flags a binary has. gc accepts a bd only when the binary's visible command
// tree equals the visible projection of a pinned inventory, and then applies
// that inventory's decisions.
//
// The live tree comes from bd's --help output (WalkHelp), the only structural
// view bd exposes today. It shows every visible command, alias and flag with
// pflag's own rendering of the flag (shorthand, type or backticked value name,
// NoOptDefVal, non-zero default, usage), so the comparison is exact on
// everything visible. Hidden commands and flags are not in --help: those come
// from the pinned inventory, which is dumped from bd's source
// (inventorygen/generate.sh). The proposed `bd __command-tree --json` would let
// gc compare hidden elements too (see the PR body).

// HelpView is one command's visible help structure.
type HelpView struct {
	Aliases     []string `json:"aliases,omitempty"`
	Subcommands []string `json:"subcommands,omitempty"`
	Flags       string   `json:"flags,omitempty"`
	GlobalFlags string   `json:"global_flags,omitempty"`
}

// HelpTree is a bd binary's visible command tree, by command path ("" is
// the root). Commands bd does not parse flags for (DisableFlagParsing, e.g.
// mail) are left out: their --help is not cobra's.
type HelpTree map[string]HelpView

// Fingerprint returns a stable hash of the tree.
func (t HelpTree) Fingerprint() string {
	data, _ := json.Marshal(t) // map keys marshal sorted
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// normalizeHelpBlock trims trailing whitespace from each line and blank lines
// from both ends, as cobra's template does.
func normalizeHelpBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

var helpTreeOnce sync.Map // *Inventory → HelpTree

// HelpTree returns the visible projection of the inventory: what bd's --help
// shows for every visible command, rendered by the same cobra and pflag bd
// uses from the inventory's flags (usage, defaults, NoOptDefVal, hidden).
func (inv *Inventory) HelpTree() HelpTree {
	if cached, ok := helpTreeOnce.Load(inv); ok {
		return cached.(HelpTree)
	}
	root, byCmd := inv.buildCobra()
	tree := HelpTree{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		p := byCmd[c]
		if inv.byPath[p].DisableFlagParsing {
			return
		}
		v := HelpView{Aliases: slices.Sorted(slices.Values(c.Aliases))}
		var subs []*cobra.Command
		for _, sub := range c.Commands() {
			if sub.IsAvailableCommand() {
				v.Subcommands = append(v.Subcommands, sub.Name())
				subs = append(subs, sub)
			}
		}
		sort.Strings(v.Subcommands)
		if c.HasAvailableLocalFlags() {
			v.Flags = normalizeHelpBlock(c.LocalFlags().FlagUsages())
		}
		if c.HasAvailableInheritedFlags() {
			v.GlobalFlags = normalizeHelpBlock(c.InheritedFlags().FlagUsages())
		}
		if len(v.Aliases) == 0 {
			v.Aliases = nil
		}
		tree[p] = v
		for _, sub := range subs {
			walk(sub)
		}
	}
	walk(root)
	helpTreeOnce.Store(inv, tree)
	return tree
}

// helpSectionRE matches a help section header: an unindented line ending in
// ":".
var helpSectionRE = regexp.MustCompile(`^[A-Za-z][^:]*:$`)

// helpCommandLineRE matches one command line of a command listing.
var helpCommandLineRE = regexp.MustCompile(`^  ([A-Za-z0-9_][A-Za-z0-9_-]*)(\s|$)`)

// ParseHelp reads one `bd <path> --help` output into its visible structure.
// At the root, every section other than the known non-command ones lists
// commands (bd groups its root commands under titled sections).
func ParseHelp(out string, root bool) HelpView {
	var v HelpView
	section := ""
	var flags, global []string
	for _, line := range strings.Split(out, "\n") {
		if helpSectionRE.MatchString(line) {
			section = strings.TrimSuffix(line, ":")
			continue
		}
		if strings.TrimSpace(line) == "" {
			if section == "Flags" || section == "Global Flags" || section == "Aliases" {
				section = ""
			}
			continue
		}
		switch section {
		case "Aliases":
			names := strings.Split(strings.TrimSpace(line), ",")
			for _, n := range names[1:] {
				if n = strings.TrimSpace(n); n != "" {
					v.Aliases = append(v.Aliases, n)
				}
			}
			section = ""
		case "Flags":
			flags = append(flags, line)
		case "Global Flags":
			global = append(global, line)
		case "Usage", "Examples", "Additional help topics", "":
		default:
			if section == "Available Commands" || root {
				if m := helpCommandLineRE.FindStringSubmatch(line); m != nil {
					v.Subcommands = append(v.Subcommands, m[1])
				}
			}
		}
	}
	sort.Strings(v.Aliases)
	sort.Strings(v.Subcommands)
	v.Flags = normalizeHelpBlock(strings.Join(flags, "\n"))
	v.GlobalFlags = normalizeHelpBlock(strings.Join(global, "\n"))
	return v
}

// WalkHelp derives a bd binary's visible command tree from its --help output:
// run(path) must return `bd <path...> --help`'s output. It walks the
// subcommands each help lists, up to workers at a time. skip names paths not
// to read (commands bd does not parse flags for).
func WalkHelp(run func(path []string) (string, error), skip func(path string) bool, workers int) (HelpTree, error) {
	tree, _, _, err := WalkHelpUntil(run, skip, workers, nil)
	return tree, err
}

// WalkHelpUntil is WalkHelp, level by level, asking stop after each level
// whether to go on: stop(tree, depth) is called with the tree walked so far,
// whose deepest paths have depth words. It returns the tree, the depth
// walked, and whether stop ended the walk early.
func WalkHelpUntil(run func(path []string) (string, error), skip func(path string) bool, workers int, stop func(tree HelpTree, depth int) bool) (HelpTree, int, bool, error) {
	if workers < 1 {
		workers = 1
	}
	tree := HelpTree{}
	var mu sync.Mutex
	level := [][]string{nil}
	depth := 0
	for len(level) > 0 {
		var next [][]string
		var firstErr error
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for _, path := range level {
			key := strings.Join(path, " ")
			if skip != nil && skip(key) {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(path []string, key string) {
				defer wg.Done()
				defer func() { <-sem }()
				out, err := run(path)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("bd %s --help: %w", key, err)
					}
					return
				}
				v := ParseHelp(out, len(path) == 0)
				tree[key] = v
				for _, sub := range v.Subcommands {
					next = append(next, append(append([]string(nil), path...), sub))
				}
			}(path, key)
		}
		wg.Wait()
		if firstErr != nil {
			return nil, depth, false, firstErr
		}
		if stop != nil && stop(tree, depth) {
			return tree, depth, true, nil
		}
		level = next
		depth++
	}
	return tree, depth - 1, false, nil
}

// SkipUnpinnedOrUnparsed is the trust walk's skip: it reads only command
// paths some pinned inventory has and parses flags for. A command no
// inventory knows is never run, not even with --help (bd may not parse its
// flags, so --help would be an argument); its parent's listing already
// shows the difference.
func SkipUnpinnedOrUnparsed(path string) bool {
	known := false
	for _, inv := range Inventories() {
		if c := inv.Command(path); c != nil {
			if c.DisableFlagParsing {
				return true
			}
			known = true
		}
	}
	return !known
}

// MatchHelpTreeToDepth is MatchHelpTree against each inventory's projection
// cut to paths of at most depth words: the check a walk stopped at depth can
// make.
func MatchHelpTreeToDepth(live HelpTree, depth int) (*Inventory, map[string][]string) {
	diffs := map[string][]string{}
	for _, inv := range Inventories() {
		pinned := HelpTree{}
		for p, v := range inv.HelpTree() {
			if len(strings.Fields(p)) <= depth {
				pinned[p] = v
			}
		}
		d := DiffHelpTrees(pinned, live, 8)
		if len(d) == 0 {
			return inv, nil
		}
		diffs[inv.Name] = d
	}
	return nil, diffs
}

// SkipUnparsedHelp reports the paths whose --help is not cobra's in any pinned
// inventory (DisableFlagParsing): WalkHelp's skip.
func SkipUnparsedHelp(path string) bool {
	for _, inv := range Inventories() {
		if c := inv.Command(path); c != nil && c.DisableFlagParsing {
			return true
		}
	}
	return false
}

// MatchHelpTree returns the pinned inventory whose visible projection equals
// live, or nil and, per inventory, the first differences found.
func MatchHelpTree(live HelpTree) (*Inventory, map[string][]string) {
	diffs := map[string][]string{}
	for _, inv := range Inventories() {
		d := DiffHelpTrees(inv.HelpTree(), live, 8)
		if len(d) == 0 {
			return inv, nil
		}
		diffs[inv.Name] = d
	}
	return nil, diffs
}

// DiffHelpTrees lists up to limit differences between a pinned projection
// and a live tree.
func DiffHelpTrees(pinned, live HelpTree, limit int) []string {
	var out []string
	add := func(format string, a ...any) {
		if len(out) < limit {
			out = append(out, fmt.Sprintf(format, a...))
		}
	}
	for _, p := range slices.Sorted(mapsKeys(pinned)) {
		lv, ok := live[p]
		if !ok {
			add("command %q is missing from the binary", p)
			continue
		}
		pv := pinned[p]
		if !slices.Equal(pv.Aliases, lv.Aliases) {
			add("%q aliases: pinned %v, binary %v", p, pv.Aliases, lv.Aliases)
		}
		if !slices.Equal(pv.Subcommands, lv.Subcommands) {
			added, missing := setDiff(lv.Subcommands, pv.Subcommands), setDiff(pv.Subcommands, lv.Subcommands)
			add("%q subcommands differ: the binary adds %v and lacks %v", p, added, missing)
		}
		if pv.Flags != lv.Flags {
			add("%q flags differ:\n--- pinned\n%s\n--- binary\n%s", p, pv.Flags, lv.Flags)
		}
		if pv.GlobalFlags != lv.GlobalFlags {
			add("%q global flags differ", p)
		}
	}
	for _, p := range slices.Sorted(mapsKeys(live)) {
		if _, ok := pinned[p]; !ok {
			add("the binary has command %q, which the inventory does not", p)
		}
	}
	return out
}

// setDiff returns the elements of a not in b, in a's order.
func setDiff(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func mapsKeys(t HelpTree) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range t {
			if !yield(k) {
				return
			}
		}
	}
}
