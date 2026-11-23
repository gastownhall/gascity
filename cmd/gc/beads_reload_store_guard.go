package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/gitcred"
)

// cityStoreBinding is the part of a city's .beads/metadata.json that says
// which bead store the city is bound to.
type cityStoreBinding struct {
	Backend         string
	DoltDatabase    string
	StorageEndpoint string
	StorageDatabase string
}

func (b cityStoreBinding) String() string {
	var parts []string
	for _, f := range []struct{ key, value string }{
		{"backend", b.Backend},
		{"dolt_database", b.DoltDatabase},
		{"storage_endpoint", gitcred.RedactUserinfo(b.StorageEndpoint)},
		{"storage_database", b.StorageDatabase},
	} {
		if f.value != "" {
			parts = append(parts, f.key+"="+f.value)
		}
	}
	return strings.Join(parts, " ")
}

// doltFamily reports whether the binding names a Dolt database gc's managed
// lifecycle initializes (and so resolves a name for).
func (b cityStoreBinding) doltFamily() bool {
	switch b.Backend {
	case "", "dolt", "doltlite":
		return true
	default:
		return false
	}
}

// cityStoreRecord is the bd store binding a city booted on. Every later
// reload must keep it: rebinding the city store is restart-only.
type cityStoreRecord struct {
	Recorded bool
	Binding  cityStoreBinding
	// Database is the Dolt database boot used for a Dolt-family binding:
	// dolt_database, or the historical default for legacy metadata. Empty
	// for any other backend.
	Database string
	// BdBoot is set when the city booted on a bd-contract provider. With
	// Recorded false it separates a provider switch (restart initializes the
	// bd store) from a bd city whose metadata was already unusable when the
	// record was taken (Problem), which a restart would re-bind to a default.
	BdBoot  bool
	Problem string
}

// recordCityStore reads the city's bd store binding.
func recordCityStore(cityPath string) cityStoreRecord {
	if cityPath == "" || !cityUsesBdStoreContract(cityPath) {
		return cityStoreRecord{}
	}
	data, readErr := os.ReadFile(scopeMetadataJSONPath(cityPath))
	binding, err := parseCityStoreBinding(data, readErr)
	if err != nil {
		return cityStoreRecord{BdBoot: true, Problem: err.Error()}
	}
	rec := cityStoreRecord{Recorded: true, BdBoot: true, Binding: binding}
	if binding.doltFamily() {
		rec.Database = binding.DoltDatabase
		if rec.Database == "" {
			rec.Database = defaultScopeDoltDatabase(cityPath, cityPath, "")
		}
	}
	return rec
}

// bootCityStores holds, per city path, the store binding the last successful
// boot lifecycle left behind, for the runtime constructed after it.
var bootCityStores sync.Map

// recordBootCityStore records the city's binding as the boot lifecycle
// leaves it, so the runtime does not depend on re-reading metadata later.
func recordBootCityStore(cityPath string) {
	bootCityStores.Store(filepath.Clean(cityPath), recordCityStore(cityPath))
}

// bootCityStoreFor returns the binding recorded by the city's boot
// lifecycle. Runtimes built without one (tests) record it now.
func bootCityStoreFor(cityPath string) cityStoreRecord {
	if v, ok := bootCityStores.Load(filepath.Clean(cityPath)); ok {
		return v.(cityStoreRecord)
	}
	return recordCityStore(cityPath)
}

// parseCityStoreBinding reads a binding out of metadata.json content. Missing,
// unreadable, torn, null, and non-object files are refused, as is metadata
// that names no store at all (an empty object or pre-Dolt bd metadata): the
// lifecycle would read any of them as "fresh city" and fall back to "hq". A
// binding field holding a non-string value is refused too, since the
// lifecycle's own readers (canonicalScopeDoltDatabase,
// scopeHasCompleteStorageBinding) would read it differently.
func parseCityStoreBinding(data []byte, readErr error) (cityStoreBinding, error) {
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return cityStoreBinding{}, errors.New("missing")
		}
		var pathErr *fs.PathError
		if errors.As(readErr, &pathErr) {
			return cityStoreBinding{}, pathErr.Err
		}
		return cityStoreBinding{}, readErr
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return cityStoreBinding{}, fmt.Errorf("unparseable: %w", err)
	}
	if meta == nil {
		return cityStoreBinding{}, errors.New("null")
	}
	var binding cityStoreBinding
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"backend", &binding.Backend},
		{"dolt_database", &binding.DoltDatabase},
		{"storage_endpoint", &binding.StorageEndpoint},
		{"storage_database", &binding.StorageDatabase},
	} {
		switch v := meta[f.key].(type) {
		case nil:
		case string:
			*f.dst = strings.TrimSpace(v)
		default:
			return cityStoreBinding{}, fmt.Errorf("%s is not a string", f.key)
		}
	}
	if binding == (cityStoreBinding{}) {
		return cityStoreBinding{}, errors.New("names no store (no backend, dolt_database, storage_endpoint, or storage_database)")
	}
	return binding, nil
}

// doNotRestartUnrestored is the warning every lost-binding refusal carries:
// boot has no record to hold the city to.
const doNotRestartUnrestored = "do NOT restart until it is restored: boot treats a missing binding as a new city and creates the default database"

// reloadCityStoreError decides whether a reload may proceed against the city
// store, given the binding recorded at boot and the current metadata.json.
func reloadCityStoreError(rec cityStoreRecord, path string, data []byte, readErr error) error {
	if !rec.Recorded {
		if !rec.BdBoot {
			return fmt.Errorf("city booted on a non-bd beads provider, so no bd store binding was recorded for %s; reload will not run bd init or create a database; restart the city to initialize its bd store", path)
		}
		return fmt.Errorf("city bead store metadata %s was already unusable when the city's binding was recorded at boot (%s), so the store this city runs on is unknown; reload will not run bd init or create a database; restore metadata.json to the city's real binding, and %s", path, rec.Problem, doNotRestartUnrestored)
	}
	current, err := parseCityStoreBinding(data, readErr)
	if err != nil {
		return fmt.Errorf("city bead store metadata %s is unusable (%w); reload will not run bd init or create a database; restore metadata.json to the booted binding [%s], and %s", path, err, rec.Binding, doNotRestartUnrestored)
	}
	if current != rec.Binding {
		return fmt.Errorf("city bead store metadata %s now names store [%s], but the city booted on [%s]; reload will not rebind the city store or create a database; if the change is deliberate, restart the city to adopt it (boot binds to whatever metadata.json names and creates that database if absent), otherwise restore metadata.json to the booted binding", path, current, rec.Binding)
	}
	return nil
}

// checkReloadCityStore checks the city's current metadata.json against the
// binding recorded at boot.
func checkReloadCityStore(cityPath string, rec cityStoreRecord) error {
	path := scopeMetadataJSONPath(cityPath)
	data, readErr := os.ReadFile(path)
	return reloadCityStoreError(rec, path, data, readErr)
}

// reloadCityStoreDatabase is the city's Dolt database name for the reload
// lifecycle: the booted one, confirmed against metadata at the point of use.
// It never falls back to a default.
func reloadCityStoreDatabase(cityPath string, rec cityStoreRecord) (string, error) {
	if err := checkReloadCityStore(cityPath, rec); err != nil {
		return "", err
	}
	if rec.Database == "" {
		return "", fmt.Errorf("city booted on store [%s], which has no managed Dolt database; reload will not initialize one", rec.Binding)
	}
	return rec.Database, nil
}

// startBeadsLifecycleForReload is the bead lifecycle a config reload runs.
//
// The lifecycle's city init step is idempotent only while metadata.json names
// the store: with it missing or rewritten, init falls back to the default
// "hq", stamps metadata naming it, and lets bd init create an empty database
// the city is rebound to (mc-s8duk, where a git fast-forward in the city
// checkout deleted a once-tracked metadata.json). A reload therefore checks
// the metadata against the binding recorded at boot before any lifecycle step,
// again when city init begins — after the provider start op, which can take
// seconds — whichever way init classifies the scope, and wherever the
// lifecycle names the city's database, handing it the booted database instead
// of letting it resolve one.
func startBeadsLifecycleForReload(cityPath, _ string, cfg *config.City, rec cityStoreRecord, stderr io.Writer) error {
	if !cityUsesBdStoreContract(cityPath) {
		return startBeadsLifecycleResolvingCityDatabase(cityPath, cfg, stderr, nil)
	}
	if err := checkReloadCityStore(cityPath, rec); err != nil {
		return err
	}
	return startBeadsLifecycleResolvingCityDatabase(cityPath, cfg, stderr, &rec)
}
