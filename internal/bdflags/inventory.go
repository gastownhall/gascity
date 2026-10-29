package bdflags

import (
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The pinned bd command inventories: bd's own cobra command tree — every
// command, alias and flag, hidden ones included, with each flag's type,
// default and NoOptDefVal — dumped from the bd source of each build gc bd's
// split-city allowlist is pinned to (inventorygen/generate.sh). They are the
// ground truth gc resolves a gc bd argv against: it builds a mirror cobra tree
// from the inventory of the bd it is about to run (InventoryFor) and lets the
// same cobra and pflag versions bd uses (v1.10.2 / v1.0.10) resolve the command
// and parse its flags, so the command gc judges is the command bd runs.

// The binary embeds each inventory gzipped (gzip -n -9 of the reviewed
// inventory/<name>.json, which generate.sh writes beside it;
// TestEmbeddedInventoriesMatchTheirJSON keeps the two equal). Uncompressed
// they would add about half a megabyte to gc.
//
//go:embed inventory/*.json.gz
var inventoryFS embed.FS

// InventoryFlag is one flag as bd's cobra tree declares it.
type InventoryFlag struct {
	Name        string `json:"name"`
	Shorthand   string `json:"shorthand,omitempty"`
	Type        string `json:"type"`
	Default     string `json:"default"`
	NoOptDefVal string `json:"no_opt_def_val,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
	// Usage is the flag's help text, which help renders (a backticked word
	// in it names the value).
	Usage string `json:"usage,omitempty"`
	// ShorthandDeprecated hides the shorthand from help.
	ShorthandDeprecated bool `json:"shorthand_deprecated,omitempty"`
}

// TakesValue reports whether the flag consumes a value when spelled without
// "=" (pflag: no NoOptDefVal).
func (f InventoryFlag) TakesValue() bool { return f.NoOptDefVal == "" }

// InventoryCommand is one command of bd's cobra tree.
type InventoryCommand struct {
	Path               string          `json:"path"`
	Aliases            []string        `json:"aliases,omitempty"`
	Hidden             bool            `json:"hidden,omitempty"`
	Deprecated         bool            `json:"deprecated,omitempty"`
	Runnable           bool            `json:"runnable"`
	DisableFlagParsing bool            `json:"disable_flag_parsing,omitempty"`
	Local              []InventoryFlag `json:"local,omitempty"`
	Persistent         []InventoryFlag `json:"persistent,omitempty"`
}

// InventoryProvenance records where an inventory came from: the bd source
// commit and build tags it was dumped with, and the Go, cobra and pflag
// versions bd was built with. gc's mirror resolves argv with its own cobra and
// pflag, so those must be the same versions (TestInventoryCobraPflagMatchGoMod).
type InventoryProvenance struct {
	Commit       string `json:"commit"`
	BuildTags    string `json:"build_tags"`
	GoVersion    string `json:"go_version"`
	CobraVersion string `json:"cobra_version"`
	PflagVersion string `json:"pflag_version"`
	// CGOEnabled is the dump build's CGO_ENABLED setting.
	CGOEnabled string `json:"cgo_enabled"`
}

// Inventory is one pinned bd build's command tree.
type Inventory struct {
	// Name is the inventory file's base name ("bd-1.3.2"), the id a city
	// vouches for.
	Name       string              `json:"-"`
	Version    string              `json:"version"`
	Build      string              `json:"build"`
	Provenance InventoryProvenance `json:"provenance"`
	Commands   []InventoryCommand  `json:"commands"`

	byPath map[string]*InventoryCommand
}

var (
	inventoriesOnce sync.Once
	inventories     []*Inventory
	inventoriesErr  error
	inventorySetSum string
)

// InventorySetFingerprint is a hash of every embedded inventory (name and
// bytes). A cached trust verdict is valid only under the inventory set it was
// judged against, so a gc upgrade that changes the set re-judges every binary.
func InventorySetFingerprint() string {
	Inventories()
	return inventorySetSum
}

// Inventories returns every pinned inventory, sorted by name.
func Inventories() []*Inventory {
	inventoriesOnce.Do(func() {
		setHash := sha256.New()
		defer func() { inventorySetSum = hex.EncodeToString(setHash.Sum(nil)) }()
		entries, err := inventoryFS.ReadDir("inventory")
		if err != nil {
			inventoriesErr = err
			return
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json.gz") {
				continue
			}
			data, err := readGzipInventory(path.Join("inventory", e.Name()))
			if err != nil {
				inventoriesErr = fmt.Errorf("%s: %w", e.Name(), err)
				return
			}
			// hash.Hash writes never fail.
			_, _ = fmt.Fprintf(setHash, "%s\x00%d\x00", e.Name(), len(data))
			_, _ = setHash.Write(data)
			inv, err := ParseInventory(strings.TrimSuffix(e.Name(), ".json.gz"), data)
			if err != nil {
				inventoriesErr = fmt.Errorf("%s: %w", e.Name(), err)
				return
			}
			inventories = append(inventories, inv)
		}
		sort.Slice(inventories, func(i, j int) bool { return inventories[i].Name < inventories[j].Name })
	})
	if inventoriesErr != nil {
		panic("bdflags: pinned bd inventories: " + inventoriesErr.Error())
	}
	return inventories
}

// readGzipInventory returns the decompressed bytes of an embedded inventory.
func readGzipInventory(name string) ([]byte, error) {
	f, err := inventoryFS.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // embedded read
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close() //nolint:errcheck // embedded read
	return io.ReadAll(zr)
}

// ParseInventory decodes one inventory dump (the JSON generate.sh writes).
// Tests use it to build synthetic inventories.
func ParseInventory(name string, data []byte) (*Inventory, error) {
	inv := &Inventory{}
	if err := json.Unmarshal(data, inv); err != nil {
		return nil, err
	}
	inv.Name = name
	inv.byPath = map[string]*InventoryCommand{}
	for i := range inv.Commands {
		inv.byPath[inv.Commands[i].Path] = &inv.Commands[i]
	}
	if inv.byPath[""] == nil {
		return nil, fmt.Errorf("inventory %s has no root command", name)
	}
	return inv, nil
}

// InventoryByName returns the pinned inventory with that id, or nil.
func InventoryByName(name string) *Inventory {
	for _, inv := range Inventories() {
		if inv.Name == name {
			return inv
		}
	}
	return nil
}

// Command returns the command at path, or nil.
func (inv *Inventory) Command(p string) *InventoryCommand { return inv.byPath[p] }

// Paths returns every command path, sorted ("" is the root).
func (inv *Inventory) Paths() []string {
	out := make([]string, 0, len(inv.Commands))
	for _, c := range inv.Commands {
		out = append(out, c.Path)
	}
	return out
}

// Aliases returns alias path → command path ("mol fart" → "mol bond").
func (inv *Inventory) Aliases() map[string]string {
	out := map[string]string{}
	for _, c := range inv.Commands {
		parent, _ := splitParent(c.Path)
		for _, a := range c.Aliases {
			out[strings.TrimSpace(parent+" "+a)] = c.Path
		}
	}
	return out
}

// Subcommands returns the direct subcommand names of path.
func (inv *Inventory) Subcommands(p string) []string {
	var out []string
	for _, c := range inv.Commands {
		if c.Path == "" {
			continue
		}
		parent, leaf := splitParent(c.Path)
		if parent == p {
			out = append(out, leaf)
		}
	}
	sort.Strings(out)
	return out
}

// Flags returns the flags cobra merges for path: its local flags, its own
// persistent flags, and every ancestor's persistent flags.
func (inv *Inventory) Flags(p string) []InventoryFlag {
	seen := map[string]bool{}
	var out []InventoryFlag
	add := func(fs []InventoryFlag) {
		for _, f := range fs {
			if !seen[f.Name] {
				seen[f.Name] = true
				out = append(out, f)
			}
		}
	}
	for cur := p; ; {
		c := inv.byPath[cur]
		if c != nil {
			if cur == p {
				add(c.Local)
			}
			add(c.Persistent)
		}
		if cur == "" {
			break
		}
		cur, _ = splitParent(cur)
	}
	return out
}

// FlagSpecs returns Flags(path) as FlagSpecs for ParseArgs.
func (inv *Inventory) FlagSpecs(p string) []FlagSpec {
	var out []FlagSpec
	for _, f := range inv.Flags(p) {
		short := ""
		if f.Shorthand != "" {
			short = "-" + f.Shorthand
		}
		out = append(out, FlagSpec{Long: "--" + f.Name, Short: short, TakesValue: f.TakesValue()})
	}
	return out
}

func splitParent(p string) (parent, leaf string) {
	if i := strings.LastIndex(p, " "); i >= 0 {
		return p[:i], p[i+1:]
	}
	if p == "" {
		return "", ""
	}
	return "", p
}

// Resolved is a gc bd argv resolved and parsed exactly as bd resolves and
// parses it.
type Resolved struct {
	// Path is the command bd runs ("" for the root).
	Path string
	// Flags are the flags the argv set, by long name ("--type"); a slice
	// flag carries its elements as pflag split them.
	Flags map[string][]string
	// Positionals are the arguments left after flags.
	Positionals []string
	// Help reports that the argv asks for help, which bd prints instead of
	// running the command.
	Help bool
	// DisableFlagParsing reports a command whose flags bd does not parse.
	DisableFlagParsing bool
}

// Resolve resolves argv against the mirror of this inventory's cobra tree with
// the cobra and pflag versions bd itself uses: cobra's Find (its own stripFlags
// pre-scan, which can disagree with pflag about which token is the
// subcommand), then pflag's parse of the found command's flags.
//
// The mirror is built fresh for each call: cobra and pflag keep parse state
// (Changed, values) on the flag objects, so a shared tree would leak one
// argv's flags into the next.
func (inv *Inventory) Resolve(argv []string) (Resolved, error) {
	root, byCmd := inv.buildCobra()
	cmd, rest, err := root.Find(argv)
	if err != nil {
		return Resolved{}, err
	}
	res := Resolved{Path: byCmd[cmd], DisableFlagParsing: cmd.DisableFlagParsing, Flags: map[string][]string{}}
	if cmd.DisableFlagParsing {
		res.Positionals = rest
		return res, nil
	}
	fs := cmd.Flags()
	// Find has already merged the persistent flags into fs.
	if err := fs.Parse(rest); err != nil {
		return Resolved{}, err
	}
	fs.Visit(func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			res.Flags["--"+f.Name] = append([]string(nil), sv.GetSlice()...)
			return
		}
		res.Flags["--"+f.Name] = []string{f.Value.String()}
	})
	if v, ok := res.Flags["--help"]; ok && len(v) == 1 && v[0] == "true" {
		res.Help = true
	}
	res.Positionals = fs.Args()
	return res, nil
}

// buildCobra builds the mirror tree. Every command is runnable (a no-op) so
// Find never needs bd's Run functions, and Args is left nil as in bd, so
// Find's legacyArgs check behaves as bd's does.
func (inv *Inventory) buildCobra() (*cobra.Command, map[*cobra.Command]string) {
	byCmd := map[*cobra.Command]string{}
	byPath := map[string]*cobra.Command{}
	for _, ic := range inv.Commands {
		_, leaf := splitParent(ic.Path)
		c := &cobra.Command{
			Use:                leaf,
			Aliases:            ic.Aliases,
			Hidden:             ic.Hidden,
			DisableFlagParsing: ic.DisableFlagParsing,
			SilenceErrors:      true,
			SilenceUsage:       true,
		}
		if ic.Deprecated {
			c.Deprecated = "deprecated"
		}
		if ic.Runnable {
			c.Run = func(*cobra.Command, []string) {}
		}
		if ic.Path == "" {
			c.Use = "bd"
		}
		for _, f := range ic.Local {
			addMirrorFlag(c.Flags(), f)
		}
		for _, f := range ic.Persistent {
			addMirrorFlag(c.PersistentFlags(), f)
		}
		byPath[ic.Path] = c
		byCmd[c] = ic.Path
	}
	// Parents before children: paths sort with every prefix first.
	paths := inv.Paths()
	sort.Strings(paths)
	for _, p := range paths {
		if p == "" {
			continue
		}
		parent, _ := splitParent(p)
		byPath[parent].AddCommand(byPath[p])
	}
	return byPath[""], byCmd
}

// addMirrorFlag declares f on fs with pflag's own value type, so Set parses
// exactly as bd's flag does (a stringSlice reads one CSV record, a bool
// rejects anything strconv.ParseBool does, an int rejects non-numbers).
func addMirrorFlag(fs *pflag.FlagSet, f InventoryFlag) {
	switch f.Type {
	case "bool":
		fs.BoolP(f.Name, f.Shorthand, f.Default == "true", "")
	case "int":
		fs.IntP(f.Name, f.Shorthand, 0, "")
	case "int64":
		fs.Int64P(f.Name, f.Shorthand, 0, "")
	case "float64":
		fs.Float64P(f.Name, f.Shorthand, 0, "")
	case "duration":
		fs.DurationP(f.Name, f.Shorthand, 0, "")
	case "stringSlice":
		fs.StringSliceP(f.Name, f.Shorthand, nil, "")
	case "stringArray":
		fs.StringArrayP(f.Name, f.Shorthand, nil, "")
	default:
		fs.StringP(f.Name, f.Shorthand, "", "")
	}
	pf := fs.Lookup(f.Name)
	pf.NoOptDefVal = f.NoOptDefVal
	pf.Hidden = f.Hidden
	pf.Usage = f.Usage
	pf.DefValue = f.Default
	if f.ShorthandDeprecated {
		pf.ShorthandDeprecated = "deprecated"
	}
}
