package bdflags

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// FlagSpec is one bd flag: its long name ("--type"), its shorthand ("-t", or
// ""), and whether it consumes a value. A flag that takes no value is a pflag
// bool (NoOptDefVal "true"): `--flag` sets it, `--flag=false` clears it, and
// it never consumes the next token.
type FlagSpec struct {
	Long       string
	Short      string
	TakesValue bool
}

// CommandPaths returns every bd command path in any pinned inventory
// ("update", "dep add", "mol wisp create"), sorted, the root ("") excluded.
func CommandPaths() []string {
	seen := map[string]bool{}
	var out []string
	for _, inv := range Inventories() {
		for _, p := range inv.Paths() {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// CommandAliases returns the alias-path → command-path map of every pinned
// inventory ("mol fart" → "mol bond", "new" → "create").
func CommandAliases() map[string]string {
	out := map[string]string{}
	for _, inv := range Inventories() {
		for k, v := range inv.Aliases() {
			out[k] = v
		}
	}
	return out
}

// IsCommandPath reports whether path is a bd command path in any pinned
// inventory.
func IsCommandPath(path string) bool {
	for _, inv := range Inventories() {
		if inv.Command(path) != nil {
			return true
		}
	}
	return false
}

// SubcommandsOf returns the direct subcommand names of path ("" for the
// root) across the pinned inventories.
func SubcommandsOf(path string) []string {
	seen := map[string]bool{}
	var out []string
	for _, inv := range Inventories() {
		for _, s := range inv.Subcommands(path) {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// CommandFlagSpecs returns the union, across the pinned inventories, of the
// flags path declares itself (its local and own persistent flags).
func CommandFlagSpecs(path string) []FlagSpec {
	seen := map[string]bool{}
	var out []FlagSpec
	for _, inv := range Inventories() {
		c := inv.Command(path)
		if c == nil {
			continue
		}
		for _, f := range append(append([]InventoryFlag(nil), c.Local...), c.Persistent...) {
			if seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			short := ""
			if f.Shorthand != "" {
				short = "-" + f.Shorthand
			}
			out = append(out, FlagSpec{Long: "--" + f.Name, Short: short, TakesValue: f.TakesValue()})
		}
	}
	return out
}

// GlobalFlagSpecs returns the root's persistent flags (union across the
// pinned inventories).
func GlobalFlagSpecs() []FlagSpec {
	seen := map[string]bool{}
	var out []FlagSpec
	for _, inv := range Inventories() {
		c := inv.Command("")
		if c == nil {
			continue
		}
		for _, f := range c.Persistent {
			if seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			short := ""
			if f.Shorthand != "" {
				short = "-" + f.Shorthand
			}
			out = append(out, FlagSpec{Long: "--" + f.Name, Short: short, TakesValue: f.TakesValue()})
		}
	}
	return out
}

// ParsedFlag is one flag occurrence, named by its long name. Value is the
// stated value; for a bool flag it is "true" or "false".
type ParsedFlag struct {
	Long  string
	Value string
	Bool  bool
}

// ParsedArgs is an argv as pflag parses it: the flag occurrences in order and
// the positional arguments (interspersed ones included, everything after "--"
// too).
type ParsedArgs struct {
	Flags       []ParsedFlag
	Positionals []string
}

// Values returns every value given to the long flag, in order.
func (p ParsedArgs) Values(long string) []string {
	var out []string
	for _, f := range p.Flags {
		if f.Long == long {
			out = append(out, f.Value)
		}
	}
	return out
}

// BoolValue returns a bool flag's final value (the last occurrence wins, as in
// pflag) and whether it was given at all.
func (p ParsedArgs) BoolValue(long string) (value, set bool) {
	for _, f := range p.Flags {
		if f.Long == long && f.Bool {
			value, set = f.Value == "true", true
		}
	}
	return value, set
}

// Has reports whether the long flag occurs.
func (p ParsedArgs) Has(long string) bool {
	for _, f := range p.Flags {
		if f.Long == long {
			return true
		}
	}
	return false
}

// ParseArgs parses args against specs exactly as spf13/pflag parses a cobra
// command's argv (interspersed): "--" ends the flags; `--name=value`,
// `--name value` (the next token is consumed whatever it looks like), a bool
// `--name` or `--name=true|false`; shorthands `-t value`, `-tvalue`, `-t=value`
// and combined bool shorthands `-qv`, ending in a value shorthand that takes the
// rest (`-qtmessage`) or the next token. A lone "-" is positional. An unknown
// flag, a bad bool value, or a value flag with nothing to consume is an error —
// the same argv bd rejects before running anything.
func ParseArgs(specs []FlagSpec, args []string) (ParsedArgs, error) {
	byLong := map[string]FlagSpec{}
	byShort := map[string]FlagSpec{}
	for _, s := range specs {
		byLong[s.Long] = s
		if s.Short != "" {
			byShort[s.Short] = s
		}
	}
	var out ParsedArgs
	set := func(spec FlagSpec, value string) error {
		if !spec.TakesValue {
			b, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid argument %q for %q flag: %w", value, spec.Long, err)
			}
			value = strconv.FormatBool(b)
		}
		out.Flags = append(out.Flags, ParsedFlag{Long: spec.Long, Value: value, Bool: !spec.TakesValue})
		return nil
	}
	for len(args) > 0 {
		s := args[0]
		args = args[1:]
		switch {
		case len(s) < 2 || s[0] != '-':
			out.Positionals = append(out.Positionals, s)
		case s == "--":
			out.Positionals = append(out.Positionals, args...)
			return out, nil
		case strings.HasPrefix(s, "--"):
			name := s[2:]
			if name == "" || name[0] == '-' || name[0] == '=' {
				return ParsedArgs{}, fmt.Errorf("bad flag syntax: %s", s)
			}
			name, value, inline := strings.Cut(name, "=")
			spec, ok := byLong["--"+name]
			if !ok {
				return ParsedArgs{}, fmt.Errorf("unknown flag: --%s", name)
			}
			switch {
			case inline:
			case !spec.TakesValue:
				value = "true"
			case len(args) > 0:
				value, args = args[0], args[1:]
			default:
				return ParsedArgs{}, fmt.Errorf("%w: --%s", errNeedsArgument, name)
			}
			if err := set(spec, value); err != nil {
				return ParsedArgs{}, err
			}
		default:
			shorts := s[1:]
			for len(shorts) > 0 {
				c := "-" + shorts[:1]
				spec, ok := byShort[c]
				if !ok {
					return ParsedArgs{}, fmt.Errorf("unknown shorthand flag: %q in %s", shorts[:1], s)
				}
				var value string
				switch {
				case len(shorts) > 2 && shorts[1] == '=':
					value, shorts = shorts[2:], ""
				case !spec.TakesValue:
					value, shorts = "true", shorts[1:]
				case len(shorts) > 1:
					value, shorts = shorts[1:], ""
				case len(args) > 0:
					value, args, shorts = args[0], args[1:], ""
				default:
					return ParsedArgs{}, fmt.Errorf("%w: %q in %s", errNeedsArgument, shorts[:1], s)
				}
				if err := set(spec, value); err != nil {
					return ParsedArgs{}, err
				}
			}
		}
	}
	return out, nil
}

// errNeedsArgument marks a value flag with nothing left to consume.
var errNeedsArgument = errors.New("flag needs an argument")

// FlagTokenWidth returns how many argv tokens the flag at args[0] consumes
// under pflag's rules (1, or 2 when a value flag takes the next token), so a
// caller can step over a flag without knowing its shape. An unknown flag or a
// malformed value is an error.
func FlagTokenWidth(specs []FlagSpec, args []string) (int, error) {
	if len(args) == 0 {
		return 0, errors.New("no flag")
	}
	_, err := ParseArgs(specs, args[:1])
	switch {
	case err == nil:
		return 1, nil
	case errors.Is(err, errNeedsArgument) && len(args) > 1:
		return 2, nil
	default:
		return 0, err
	}
}

// ClusterTakesNextToken reports whether tok is a run of two or more
// shorthands ("-qC") whose last flag takes a value and has none attached, so
// pflag hands it the NEXT argv token. cobra's subcommand pre-scan (stripFlags)
// does not: it skips the run alone and can read that next token as a
// subcommand name, so the two disagree about what bd runs.
func ClusterTakesNextToken(specs []FlagSpec, tok string) bool {
	if len(tok) < 3 || tok[0] != '-' || tok[1] == '-' {
		return false
	}
	byShort := map[byte]FlagSpec{}
	for _, s := range specs {
		if len(s.Short) == 2 {
			byShort[s.Short[1]] = s
		}
	}
	shorts := tok[1:]
	for i := 0; i < len(shorts); i++ {
		spec, ok := byShort[shorts[i]]
		if !ok {
			return false
		}
		if !spec.TakesValue {
			continue
		}
		// A value flag takes the rest of the run (or "=value") if any is left.
		return i == len(shorts)-1
	}
	return false
}
