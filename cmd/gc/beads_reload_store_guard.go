package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/config"
)

// startBeadsLifecycleForReload is the bead lifecycle a config reload runs.
//
// A reload only happens on a running city, and a city runtime only runs once
// its boot lifecycle has initialized the city scope (a boot lifecycle failure
// is fatal). So on reload the city store already exists, and the lifecycle's
// init step is expected to be an idempotent re-assertion of it. That holds
// only while .beads/metadata.json names the store: with the file missing or
// unreadable, init cannot tell which database the city uses, falls back to
// the default "hq", stamps fresh metadata naming it, and lets bd init create
// an empty database the city is then rebound to (mc-s8duk, where a git
// fast-forward in the city checkout deleted a once-tracked metadata.json).
// Fail closed instead: the reload keeps the old config and live store, and
// the error names the file an operator has to restore.
func startBeadsLifecycleForReload(cityPath, cityName string, cfg *config.City, stderr io.Writer) error {
	if cityUsesBdStoreContract(cityPath) {
		path := scopeMetadataJSONPath(cityPath)
		data, readErr := os.ReadFile(path)
		if err := establishedCityStoreMetadataError(path, data, readErr); err != nil {
			return err
		}
	}
	return startBeadsLifecycle(cityPath, cityName, cfg, stderr)
}

// establishedCityStoreMetadataError reports whether an initialized city's
// metadata.json can still identify its store. Metadata that parses is left to
// the lifecycle (legacy files without dolt_database keep their historical
// default); an absent, unreadable, or torn file cannot be trusted to name the
// database and must never be replaced by init.
func establishedCityStoreMetadataError(path string, data []byte, readErr error) error {
	reason := ""
	if readErr != nil {
		reason = readErr.Error()
	} else {
		var meta map[string]any
		if err := json.Unmarshal(data, &meta); err != nil {
			reason = fmt.Sprintf("unparseable: %v", err)
		}
	}
	if reason == "" {
		return nil
	}
	return fmt.Errorf("city bead store metadata %s is unusable (%s); the city store is already initialized, so reload will not run bd init or create a database; restore metadata.json (it names the city's dolt_database) and reload", path, reason)
}
