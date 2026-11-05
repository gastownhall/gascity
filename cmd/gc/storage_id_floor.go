package main

// Id floors (S5 §8.2).
//
// bd mints hash ids of adaptive length and checks a new id for collisions
// against live rows only. Once a migration moves a row out of a work store
// and clears it there, nothing in that store remembers the id, so bd can mint
// it again for a different bead: two beads under one id, one in the binding
// and one in the work store (prod-evidence item 2, mc-wisp-jkl). The rig
// import raises each rig's floor (rigImportReserveIDs); this raises the city
// work store's, before its clear, the same way: bd's min_hash_length is set
// one past the longest hash suffix among the carried ids of the store's own
// prefix, so every id it mints afterwards has a longer suffix than any id it
// ever gave up. The floor is never lowered.
//
// A store that mints from a sequence (the file store) never rewinds it, and
// SQLite stores recover their sequence floor from the ids they hold on open
// and on every create, so neither needs a write. A generic exec provider's
// minting is not configurable: it is skipped, and reported.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cityIDFloor is the bd min_hash_length the city work store needs once the
// carried ids leave it: one past the longest hash suffix among the ids of its
// own prefix (another prefix's ids are never minted there), 0 when none
// needs one.
func cityIDFloor(ids []string, prefix string) int {
	own := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.HasPrefix(id, prefix+"-") {
			own = append(own, id)
		}
	}
	return rigImportHashFloor(own)
}

// raiseBdHashFloor raises min_hash_length to floor, never lowering it, and
// max_hash_length with it when the floor is above it (bd sizes ids between
// the two). Neither is raised past bdMaxMintedHashLength. It returns the
// floor in force, 0 when floor is 0.
func raiseBdHashFloor(conf rigHashLengthConfig, floor int) (int, error) {
	if floor == 0 || conf == nil {
		return 0, nil
	}
	floor = min(floor, bdMaxMintedHashLength)
	current, err := conf.ConfigGet(bdMinHashLengthKey)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", bdMinHashLengthKey, err)
	}
	if n, perr := strconv.Atoi(strings.TrimSpace(current)); perr == nil && n >= floor {
		floor = n
	} else if err := conf.ConfigSet(bdMinHashLengthKey, strconv.Itoa(floor)); err != nil {
		return 0, fmt.Errorf("setting %s=%d: %w", bdMinHashLengthKey, floor, err)
	}
	maxText, err := conf.ConfigGet(bdMaxHashLengthKey)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", bdMaxHashLengthKey, err)
	}
	maxLen := bdDefaultMaxHashLength
	if n, perr := strconv.Atoi(strings.TrimSpace(maxText)); perr == nil && n > 0 {
		maxLen = n
	}
	if maxLen < floor {
		if err := conf.ConfigSet(bdMaxHashLengthKey, strconv.Itoa(floor)); err != nil {
			return 0, fmt.Errorf("setting %s=%d: %w", bdMaxHashLengthKey, floor, err)
		}
	}
	return floor, nil
}

// cityHashConfig is the city work store's id-minting surface: the bd config
// it is set through, the prefix it mints under, and the hosted endpoint when
// the store is hosted bd. Conf is nil, with Skipped saying why, for a store
// whose minting needs no floor or cannot take one.
type cityHashConfig struct {
	Conf     rigHashLengthConfig
	Prefix   string
	Hosted   string
	Skipped  string
	Provider string
}

// cityHashConfigOpener opens the city work store's id-minting surface.
// Overridden by tests.
var cityHashConfigOpener = defaultCityHashConfigOpener

// defaultCityHashConfigOpener is the production cityHashConfigOpener.
func defaultCityHashConfigOpener(cityPath string) (cityHashConfig, error) {
	root := resolveStoreScopeRoot(cityPath, cityPath)
	provider := authoritativeBeadsProviderForScope(root, cityPath)
	switch {
	case provider == "file":
		return cityHashConfig{Provider: provider, Skipped: "the file store mints from a sequence, which a clear cannot rewind"}, nil
	case strings.HasPrefix(provider, "exec:") && !providerUsesBdStoreContract(provider):
		return cityHashConfig{Provider: provider, Skipped: fmt.Sprintf("provider %s is a generic exec store, whose id minting this build cannot configure", provider)}, nil
	}
	if _, err := os.Stat(filepath.Join(root, ".beads")); errors.Is(err, os.ErrNotExist) {
		return cityHashConfig{Provider: provider, Skipped: fmt.Sprintf("%s holds no bd store", root)}, nil
	}
	cfg, _ := migrationWindowOpen(cityPath)
	if cfg == nil {
		loaded, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
		if err != nil {
			return cityHashConfig{}, fmt.Errorf("loading the city config to reach its work store: %w", err)
		}
		cfg = loaded
	}
	prefix := issuePrefixForScope(root, cityPath, cfg)
	if prefix == "" {
		return cityHashConfig{}, fmt.Errorf("the city work store at %s names no id prefix, so the carried ids it minted cannot be told from the rest", root)
	}
	return cityHashConfig{Conf: bdStoreForRig(root, cityPath, cfg), Prefix: prefix, Hosted: hostedStorageEndpoint(root), Provider: provider}, nil
}

// hostedStorageEndpoint is the storage_endpoint of a hosted bd scope, or "".
func hostedStorageEndpoint(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".beads", "metadata.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		StorageEndpoint string `json:"storage_endpoint"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.StorageEndpoint)
}

// cityIDFloorResult is what one reservation did.
type cityIDFloorResult struct {
	// Floor is min_hash_length in force; 0 when no carried id needs one.
	Floor int
	// Before is what min_hash_length read before (0 when unset).
	Before int
	// Skipped says why the store took no floor.
	Skipped string
}

func (r cityIDFloorResult) String() string {
	switch {
	case r.Skipped != "":
		return "city id floor skipped: " + r.Skipped
	case r.Floor == 0:
		return "city id floor: no carried id of the work store's prefix needs one"
	case r.Before >= r.Floor:
		return fmt.Sprintf("city work store %s already %d", bdMinHashLengthKey, r.Floor)
	default:
		return fmt.Sprintf("city work store %s %d -> %d", bdMinHashLengthKey, r.Before, r.Floor)
	}
}

// reserveCityIDs raises the city work store's floor past every id the
// manifest records as carried. Idempotent: a run whose floor is in force
// writes nothing.
func reserveCityIDs(cityPath string, carried []string) (cityIDFloorResult, error) {
	surface, err := cityHashConfigOpener(cityPath)
	if err != nil {
		return cityIDFloorResult{}, err
	}
	if surface.Conf == nil {
		return cityIDFloorResult{Skipped: surface.Skipped}, nil
	}
	floor := cityIDFloor(carried, surface.Prefix)
	result := cityIDFloorResult{Floor: floor}
	if current, err := surface.Conf.ConfigGet(bdMinHashLengthKey); err == nil {
		result.Before, _ = strconv.Atoi(strings.TrimSpace(current))
	}
	inForce, err := raiseBdHashFloor(surface.Conf, floor)
	if err != nil {
		if surface.Hosted != "" {
			return result, fmt.Errorf("the city work store is hosted bd at %s, and raising its minimum id length failed (%w). Set %s=%d (and %s at least %d) on the hosted project through the bd serve admin path, out of band (owner decision O10), then run this again; nothing was removed",
				surface.Hosted, err, bdMinHashLengthKey, floor, bdMaxHashLengthKey, floor)
		}
		return result, fmt.Errorf("raising the city work store's minimum id length to %d: %w; nothing was removed", floor, err)
	}
	result.Floor = inForce
	return result, nil
}
