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
// ever gave up, and so cannot be one. At bd's cap of 8 characters that
// guarantee becomes a probability: a store that gave up an 8-character
// suffix gets a floor of 8, and a new 8-character id could, very rarely,
// repeat one.
//
// The floor is never lowered, with one exception: a min_hash_length or
// max_hash_length above bd's cap of 8 (an earlier rig import could set both
// to 13) is brought down to 8. It never protected anything: bd's CLI and
// native minting stop at 8 whatever the setting says, and a hosted `bd serve`
// only pads every id it mints to that length with zeros. Capping both at 8
// loses nothing.
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
// the two). Neither is raised past bdMaxMintedHashLength, and either already
// past it is brought down to it: the floor's one documented lowering (file
// comment). It returns the floor in force, 0 when floor is 0, and each
// setting it wrote, in the order written.
func raiseBdHashFloor(conf rigHashLengthConfig, floor int) (int, []bdHashSettingChange, error) {
	if floor == 0 || conf == nil {
		return 0, nil, nil
	}
	floor = min(floor, bdMaxMintedHashLength)
	var wrote []bdHashSettingChange
	set := func(key string, from, to int) error {
		if err := conf.ConfigSet(key, strconv.Itoa(to)); err != nil {
			return fmt.Errorf("setting %s=%d: %w", key, to, err)
		}
		wrote = append(wrote, bdHashSettingChange{Key: key, From: from, To: to})
		return nil
	}
	current, err := conf.ConfigGet(bdMinHashLengthKey)
	if err != nil {
		return 0, nil, fmt.Errorf("reading %s: %w", bdMinHashLengthKey, err)
	}
	n, perr := strconv.Atoi(strings.TrimSpace(current))
	switch {
	case perr == nil && n > bdMaxMintedHashLength:
		// The documented exception (file comment): above bd's cap the min
		// protects nothing, so it comes down to the cap.
		if err := set(bdMinHashLengthKey, n, bdMaxMintedHashLength); err != nil {
			return 0, nil, err
		}
		floor = bdMaxMintedHashLength
	case perr == nil && n >= floor:
		floor = n
	default:
		if err := set(bdMinHashLengthKey, n, floor); err != nil {
			return 0, nil, err
		}
	}
	maxText, err := conf.ConfigGet(bdMaxHashLengthKey)
	if err != nil {
		return 0, nil, fmt.Errorf("reading %s: %w", bdMaxHashLengthKey, err)
	}
	maxLen := bdDefaultMaxHashLength
	if n, perr := strconv.Atoi(strings.TrimSpace(maxText)); perr == nil && n > 0 {
		maxLen = n
	}
	switch {
	case maxLen < floor:
		if err := set(bdMaxHashLengthKey, maxLen, floor); err != nil {
			return 0, nil, err
		}
	case maxLen > bdMaxMintedHashLength:
		// The same exception for the max (file comment); the min is at most
		// the cap by now, so the max never falls below it.
		if err := set(bdMaxHashLengthKey, maxLen, bdMaxMintedHashLength); err != nil {
			return 0, nil, err
		}
	}
	return floor, wrote, nil
}

// bdHashSettingChange is one bd id-length setting raiseBdHashFloor wrote:
// the value it read (0 when unset) and the value it set.
type bdHashSettingChange struct {
	Key      string
	From, To int
}

// String says what changed; a lowering is the floor's one documented
// exception (file comment), and says so.
func (c bdHashSettingChange) String() string {
	if c.To < c.From {
		return fmt.Sprintf("%s %d -> %d (capped at bd's maximum)", c.Key, c.From, c.To)
	}
	return fmt.Sprintf("%s %d -> %d", c.Key, c.From, c.To)
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
	// Changes are the settings the reservation wrote, in the order written;
	// none when the floor was already in force.
	Changes []bdHashSettingChange
	// Skipped says why the store took no floor.
	Skipped string
}

func (r cityIDFloorResult) String() string {
	switch {
	case r.Skipped != "":
		return "city id floor skipped: " + r.Skipped
	case r.Floor == 0:
		return "city id floor: no carried id of the work store's prefix needs one"
	}
	var parts []string
	if len(r.Changes) == 0 || r.Changes[0].Key != bdMinHashLengthKey {
		parts = append(parts, fmt.Sprintf("%s already %d", bdMinHashLengthKey, r.Floor))
	}
	for _, c := range r.Changes {
		parts = append(parts, c.String())
	}
	return "city work store " + strings.Join(parts, ", ")
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
	inForce, changes, err := raiseBdHashFloor(surface.Conf, floor)
	if err != nil {
		if surface.Hosted != "" {
			return result, fmt.Errorf("the city work store is hosted bd at %s, and raising its minimum id length failed (%w). Set %s=%d (and %s at least %d) on the hosted project through the bd serve admin path, out of band (owner decision O10), then run this again; nothing was removed",
				surface.Hosted, err, bdMinHashLengthKey, floor, bdMaxHashLengthKey, floor)
		}
		return result, fmt.Errorf("raising the city work store's minimum id length to %d: %w; nothing was removed", floor, err)
	}
	result.Floor, result.Changes = inForce, changes
	return result, nil
}

// cityIDFloorPlan is the city work store's floor as it stands against the
// carried ids, read without writing (preflight --split, doctor storage-split).
type cityIDFloorPlan struct {
	// Need is the min_hash_length the ids need, capped at bd's
	// (rigImportHashFloor); 0 when none.
	Need int
	// Current is the min_hash_length in force (0 when unset).
	Current int
	// Skipped says why the store takes no floor; Hosted is a hosted bd's
	// endpoint (O10).
	Skipped, Hosted string
}

// below reports a floor under what the carried ids need.
func (p cityIDFloorPlan) below() bool { return p.Need > 0 && p.Current < p.Need }

// planCityIDFloor reads the city work store's floor against ids. It reads
// min_hash_length only when the ids need a floor.
func planCityIDFloor(cityPath string, ids []string) (cityIDFloorPlan, error) {
	surface, err := cityHashConfigOpener(cityPath)
	if err != nil {
		return cityIDFloorPlan{}, err
	}
	if surface.Conf == nil {
		return cityIDFloorPlan{Skipped: surface.Skipped}, nil
	}
	plan := cityIDFloorPlan{Need: cityIDFloor(ids, surface.Prefix), Hosted: surface.Hosted}
	if plan.Need == 0 {
		return plan, nil
	}
	current, err := surface.Conf.ConfigGet(bdMinHashLengthKey)
	if err != nil {
		return plan, fmt.Errorf("reading %s: %w", bdMinHashLengthKey, err)
	}
	plan.Current, _ = strconv.Atoi(strings.TrimSpace(current))
	return plan, nil
}
