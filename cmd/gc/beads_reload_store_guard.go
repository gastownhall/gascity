package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
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
		{"storage_endpoint", b.StorageEndpoint},
		{"storage_database", b.StorageDatabase},
	} {
		if f.value != "" {
			parts = append(parts, f.key+"="+f.value)
		}
	}
	return strings.Join(parts, " ")
}

// cityStoreRecord is the bd store binding a city runtime was constructed on.
// Runtimes are constructed only after the boot lifecycle has initialized the
// city scope, so this is the store every later reload must keep.
type cityStoreRecord struct {
	Recorded bool
	Binding  cityStoreBinding
	// Database is the Dolt database the boot lifecycle used for the city:
	// dolt_database, or the historical default for legacy metadata.
	Database string
}

// recordCityStore captures the city's bd store binding at runtime
// construction. Not recorded for non-bd providers or unusable metadata; a
// reload of such a city then refuses to initialize a bd store.
func recordCityStore(cityPath string) cityStoreRecord {
	if cityPath == "" || !cityUsesBdStoreContract(cityPath) {
		return cityStoreRecord{}
	}
	data, readErr := os.ReadFile(scopeMetadataJSONPath(cityPath))
	binding, err := parseCityStoreBinding(data, readErr)
	if err != nil {
		return cityStoreRecord{}
	}
	database := binding.DoltDatabase
	if database == "" {
		database = defaultScopeDoltDatabase(cityPath, cityPath, "")
	}
	return cityStoreRecord{Recorded: true, Binding: binding, Database: database}
}

// parseCityStoreBinding reads a binding out of metadata.json content. Missing,
// unreadable, torn, null, and non-object files are refused, as is metadata
// that names no store at all (an empty object or pre-Dolt bd metadata): the
// lifecycle would read any of them as "fresh city" and fall back to "hq".
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
	str := func(key string) string {
		s, _ := meta[key].(string)
		return strings.TrimSpace(s)
	}
	binding := cityStoreBinding{
		Backend:         str("backend"),
		DoltDatabase:    str("dolt_database"),
		StorageEndpoint: str("storage_endpoint"),
		StorageDatabase: str("storage_database"),
	}
	if binding == (cityStoreBinding{}) {
		return cityStoreBinding{}, errors.New("names no store (no backend, dolt_database, storage_endpoint, or storage_database)")
	}
	return binding, nil
}

// reloadCityStoreDatabase decides whether a reload may run the bead lifecycle
// against the city store, given the binding recorded at boot and the current
// metadata.json content, and returns the Dolt database the lifecycle must use.
func reloadCityStoreDatabase(rec cityStoreRecord, path string, data []byte, readErr error) (string, error) {
	if !rec.Recorded {
		return "", fmt.Errorf("city booted without a bd store binding in %s; reload will not run bd init or create a database; restart the city to initialize its bd store", path)
	}
	current, err := parseCityStoreBinding(data, readErr)
	if err != nil {
		return "", fmt.Errorf("city bead store metadata %s is unusable (%w); the city booted on store [%s], so reload will not run bd init or create a database; restore metadata.json to that binding, or restart the city", path, err, rec.Binding)
	}
	if current != rec.Binding {
		return "", fmt.Errorf("city bead store metadata %s now names store [%s], but the city booted on [%s]; reload will not rebind the city or create a database; restore metadata.json to the booted binding, or restart the city to adopt the new one", path, current, rec.Binding)
	}
	return rec.Database, nil
}

func checkReloadCityStore(cityPath string, rec cityStoreRecord) (string, error) {
	path := scopeMetadataJSONPath(cityPath)
	data, readErr := os.ReadFile(path)
	return reloadCityStoreDatabase(rec, path, data, readErr)
}

// startBeadsLifecycleForReload is the bead lifecycle a config reload runs.
//
// The lifecycle's city init step is idempotent only while metadata.json names
// the store: with it missing or rewritten, init falls back to the default
// "hq", stamps metadata naming it, and lets bd init create an empty database
// the city is rebound to (mc-s8duk, where a git fast-forward in the city
// checkout deleted a once-tracked metadata.json). A reload therefore checks
// the metadata against the binding recorded at boot before any lifecycle step,
// and again at init's point of use — after the provider start op, which can
// take seconds — handing init the booted database instead of letting it
// resolve one.
func startBeadsLifecycleForReload(cityPath, cityName string, cfg *config.City, rec cityStoreRecord, stderr io.Writer) error {
	if !cityUsesBdStoreContract(cityPath) {
		return startBeadsLifecycle(cityPath, cityName, cfg, stderr)
	}
	if _, err := checkReloadCityStore(cityPath, rec); err != nil {
		return err
	}
	return startBeadsLifecycleResolvingCityDatabase(cityPath, cfg, stderr, func() (string, error) {
		return checkReloadCityStore(cityPath, rec)
	})
}
