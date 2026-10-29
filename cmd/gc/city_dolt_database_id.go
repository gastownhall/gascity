package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

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
// if its metadata is lost), and is never renamed.
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
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("minting city dolt database id: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, []byte(hex.EncodeToString(raw[:])+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// readCityDoltDatabaseID returns the city's minted id, or "" when there is
// none or the file is not in the shape mint writes.
func readCityDoltDatabaseID(cityPath string) string {
	data, err := os.ReadFile(cityDoltDatabaseIDPath(cityPath))
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !cityDoltDatabaseIDRE.MatchString(id) {
		return ""
	}
	return id
}
