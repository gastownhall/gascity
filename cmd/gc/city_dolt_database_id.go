package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// legacySharedDoltDatabase is the database name every city used before cities
// minted a per-city id, and the one a city without an id still uses. Other
// cities on the same Dolt server may own it, so rig teardown never drops it.
const legacySharedDoltDatabase = "hq"

// cityDoltDatabaseIDRE is the only shape mintCityDoltDatabaseID writes and
// the only shape readCityDoltDatabaseID accepts.
var cityDoltDatabaseIDRE = regexp.MustCompile(`^[0-9a-f]{8}$`)

// cityDoltDatabaseIDPath is where a city's minted Dolt database id lives. It
// sits under .gc/ (machine-local runtime state), not in city.toml.
func cityDoltDatabaseIDPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "dolt-database-id")
}

// mintCityDoltDatabaseID gives a fresh city a stable random id that
// defaultScopeDoltDatabase turns into a city-unique database name. It is
// write-once and does nothing for a city that already has a persisted beads
// identity: such a city keeps whatever database it is stamped with (or "hq"
// if its metadata is lost), and is never renamed. Concurrent mints converge on
// one id: the file is published with a hard link, which fails if another mint
// got there first.
func mintCityDoltDatabaseID(cityPath string) error {
	initialized, err := scopeHasPersistedBeadsIdentity(cityPath)
	if err != nil {
		return err
	}
	if initialized {
		return nil
	}
	path := cityDoltDatabaseIDPath(cityPath)
	if _, err := os.Stat(path); err == nil {
		// An existing file is kept even when malformed; readCityDoltDatabaseID
		// warns about it.
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("minting city dolt database id: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".dolt-database-id-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	_, werr := tmp.WriteString(hex.EncodeToString(raw[:]) + "\n")
	cerr := tmp.Close()
	if werr != nil {
		return fmt.Errorf("writing %s: %w", tmpPath, werr)
	}
	if cerr != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, cerr)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	// Link, not rename: a rename would replace an id a concurrent mint just
	// published, leaving two callers holding different ids.
	if err := os.Link(tmpPath, path); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// readCityDoltDatabaseID returns the city's minted id, or "" when there is
// none or the file is not in the shape mint writes. A file that exists but
// cannot be read, or is not in that shape, also yields "" (the city then uses
// the legacy "hq" database) and logs a warning, since the fallback would
// otherwise bind the city to the shared database without any sign of why.
func readCityDoltDatabaseID(cityPath string) string {
	path := cityDoltDatabaseIDPath(cityPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("gc: ignoring city dolt database id %s: %v; falling back to the legacy %q database", path, err, legacySharedDoltDatabase)
		}
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !cityDoltDatabaseIDRE.MatchString(id) {
		log.Printf("gc: ignoring city dolt database id %s: content is not 8 lowercase hex digits; falling back to the legacy %q database", path, legacySharedDoltDatabase)
		return ""
	}
	return id
}
