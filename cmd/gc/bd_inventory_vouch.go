package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The vouch: an operator's record that one bd binary (by sha256) was walked
// and matched a pinned gc bd inventory, so gc bd may admit it without
// walking it again. It lives in a gc-owned file, never in city.toml: a key
// there would make every gc that predates it fail to load the city. It is a
// convenience, not a security boundary (anyone who can write .gc can write
// it): gc doctor re-walks vouched binaries, and each admission by a vouch
// records a gc_bd.vouch_used event.

// bdVouch is one vouched binary.
type bdVouch struct {
	BinaryPath string `json:"binary_path"`
	SHA256     string `json:"sha256"`
	Inventory  string `json:"inventory"`
	// InventorySet is the embedded inventory set the walk matched against
	// (bdflags.InventorySetFingerprint); a gc whose set differs ignores the
	// vouch and re-judges the binary.
	InventorySet string    `json:"inventory_set"`
	WrittenAt    time.Time `json:"written_at"`
	WrittenBy    string    `json:"written_by"`
}

type bdVouchFile struct {
	Vouches []bdVouch `json:"vouches"`
}

func bdVouchPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "bd-inventory-vouch.json")
}

// loadBdVouches reads the city's vouches. A missing file is no vouches; an
// unreadable or malformed one is an error the doctor reports, and gc bd
// treats as no vouches.
func loadBdVouches(cityPath string) ([]bdVouch, error) {
	data, err := os.ReadFile(bdVouchPath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f bdVouchFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", bdVouchPath(cityPath), err)
	}
	return f.Vouches, nil
}

// findBdVouch returns the vouch for this binary — its resolved path and its
// bytes (sha256) — written under this gc's inventory set.
func findBdVouch(cityPath, binaryPath, sum, set string) (bdVouch, bool) {
	vouches, _ := loadBdVouches(cityPath)
	for _, v := range vouches {
		if v.BinaryPath == binaryPath && v.SHA256 == sum && v.InventorySet == set && bdflags.InventoryByName(v.Inventory) != nil {
			return v, true
		}
	}
	return bdVouch{}, false
}

// writeBdVouch records v, replacing any vouch for the same binary path,
// under the verdict cache's lock.
func writeBdVouch(cityPath string, v bdVouch) error {
	unlock, err := lockBdTrustCache(cityPath)
	if err != nil {
		return err
	}
	defer unlock()
	vouches, err := loadBdVouches(cityPath)
	if err != nil {
		vouches = nil // a malformed file is replaced, not merged
	}
	kept := vouches[:0]
	for _, old := range vouches {
		if old.BinaryPath != v.BinaryPath {
			kept = append(kept, old)
		}
	}
	kept = append(kept, v)
	sort.Slice(kept, func(i, j int) bool { return kept[i].BinaryPath < kept[j].BinaryPath })
	data, err := json.MarshalIndent(bdVouchFile{Vouches: kept}, "", "  ")
	if err != nil {
		return err
	}
	path := bdVouchPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, path, append(data, '\n'), 0o644)
}

// vouchBdForScope walks the scope's bd for real and, only if its tree
// matches a pinned inventory, records a vouch for that exact binary.
func vouchBdForScope(cityPath string, target execStoreTarget) (bdVouch, error) {
	bdPath, err := resolveBdBinaryForScope(cityPath, target.ScopeRoot)
	if err != nil {
		return bdVouch{}, err
	}
	id, err := statBdBinary(bdPath)
	if err != nil {
		return bdVouch{}, err
	}
	v, err := resolveBdInventory(cityPath, bdPath, bdTrustOpts{forceWalk: true})
	if err != nil {
		return bdVouch{}, fmt.Errorf("refusing to vouch for %s: %w", id.Path, err)
	}
	if v.Inventory == "" {
		return bdVouch{}, fmt.Errorf("refusing to vouch for %s: its command tree matches no pinned gc bd inventory:\n  %s", id.Path, strings.Join(v.Diffs, "\n  "))
	}
	vouch := bdVouch{BinaryPath: id.Path, SHA256: v.SHA256, Inventory: v.Inventory, InventorySet: bdflags.InventorySetFingerprint(), WrittenAt: time.Now().UTC(), WrittenBy: eventActor()}
	if err := writeBdVouch(cityPath, vouch); err != nil {
		return bdVouch{}, err
	}
	return vouch, nil
}

const storageVouchBdVerb = "vouch-bd"

// newStorageVouchBdCmd is `gc storage vouch-bd [--rig <name>]`.
func newStorageVouchBdCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	var rigName string
	cmd := &cobra.Command{
		Use:          storageVouchBdVerb,
		Short:        "Walk a scope's bd and, if it matches a pinned gc bd inventory, vouch for it",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `On a split city, gc bd judges commands only for a bd binary whose command
tree matches a pinned gc bd inventory. It walks each new binary's --help tree
once and caches the verdict. This command walks the city's bd (or a rig's, with
--rig) for real and, only if the tree matches, records a vouch for that binary
by path and bytes (sha256) in .gc/bd-inventory-vouch.json, so gc bd admits it without
walking it again. A binary whose tree matches no inventory is refused and
nothing is written. city.toml is never edited.

The vouch is a convenience, not a security boundary: gc doctor re-walks
binaries and reports a mismatch, and the first admission by a vouch in each
gc process records a gc_bd.vouch_used event. A gc whose pinned inventories
differ from the ones the vouch was written under ignores it and walks the
binary again.`,
		RunE: func(*cobra.Command, []string) error {
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storageVouchBdVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			target, err := vouchBdTarget(request.CityPath, request.Cfg, rigName)
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storageVouchBdVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			vouch, err := vouchBdForScope(request.CityPath, target)
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storageVouchBdVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			fmt.Fprintf(stdout, "vouched for %s (sha256 %s) as %s in %s\n", vouch.BinaryPath, shortSHA(vouch.SHA256), vouch.Inventory, bdVouchPath(request.CityPath)) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	cmd.Flags().StringVar(&rigName, "rig", "", "vouch for this rig's bd instead of the city's")
	return cmd
}

// vouchBdTarget is the city scope, or the named rig's.
func vouchBdTarget(cityPath string, cfg *config.City, rigName string) (execStoreTarget, error) {
	if strings.TrimSpace(rigName) == "" {
		return bdCityScopeTarget(cityPath, cfg), nil
	}
	for _, rig := range cfg.Rigs {
		if rig.Name == rigName && strings.TrimSpace(rig.Path) != "" {
			return bdRigScopeTarget(cityPath, rig), nil
		}
	}
	return execStoreTarget{}, fmt.Errorf("no bound rig %q in this city", rigName)
}
