package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// What a one-shot command does with its coordination classes when city.toml
// does not load (split-default read audit R1).
//
// The old answer was "treat it as a city with no [storage]": every relocatable
// class resolved to the work store. On a split city that is a silent misroute —
// reads go empty and writes (a `gc mail send`, a session materialize) strand in
// the work ledger, where the next boot's containment check refuses the whole
// city until a fleet-stopped recovery. So the answer is now a refusal whenever
// the city MAY relocate a class.
//
// "May" is decided from evidence that does not need the composed config, and
// every unknown counts as yes:
//
//   - the city has served a split or migrated onto one, or cleared its work
//     store (the durable notes storageBootGate already holds a revert with;
//     `gc storage migrate` writes the served note too), or has a binding root
//     at the default .gc/store: one holding anything but the legacy sessions
//     database, which a single-store city keeps there too;
//   - city.toml, or a fragment its include list names, cannot be read or is
//     remote;
//   - any of those files declares a storage table or key;
//   - city.toml does not parse, so its include list is unknown — unless its
//     text names neither "include" nor "storage" at all, in which case there
//     is no list to hide and no table to miss — and no quoted key spelled
//     with a backslash escape, which could spell either. (A bare-word scan, so it errs
//     toward refusing: a comment mentioning either word is enough.) The
//     commonest load failure is a syntax error in a single-store city, and
//     that exemption is what keeps it working.
//   - a fragment that does not parse counts as declaring storage when its text
//     mentions storage at all (fragments cannot include further fragments).
//
// [pack] includes in pack.toml are NOT scanned: they name pack directories,
// which compose through pack expansion, and a pack cannot declare [storage].
//
// Only a city that is provably single-store keeps the old answer, which for it
// is the right one: the work store is where its classes have always lived, and
// refusing would take a merely misconfigured city offline for no safety gain.

// cityConfigLoadRefusal returns the error a relocatable class must answer with
// after city.toml failed to load with loadErr, or nil when the city is provably
// single-store and the work store is its classes' real home.
func cityConfigLoadRefusal(cityPath string, loadErr error) error {
	if !cityMayRelocateClasses(cityPath) {
		return nil
	}
	return standingStorageRefusal{err: fmt.Errorf("%s: refusing to route coordination classes (sessions, mail, nudges, orders, graph): %s failed to load, and this city may store them outside the work store, so falling back to it could strand writes there: %w. Fix the config and rerun the command",
		cliStorageLogPrefix, filepath.Join(cityPath, "city.toml"), loadErr)}
}

// cityMayRelocateClasses reports whether anything short of a successful config
// load suggests cityPath stores a coordination class outside its work store.
func cityMayRelocateClasses(cityPath string) bool {
	if _, present, err := readBornSplitServedNote(cityPath); present || err != nil {
		return true
	}
	if _, present, err := readInfraClearedNote(cityPath); present || err != nil {
		return true
	}
	if defaultBindingRootInUse(cityPath) {
		return true
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return true
	}
	data = stripUTF8BOM(data)
	root := map[string]any{}
	if _, err := toml.Decode(string(data), &root); err != nil {
		text := strings.ToLower(string(data))
		return strings.Contains(text, "include") || strings.Contains(text, "storage") || escapedQuotedKey.Match(data)
	}
	if _, declared := root["storage"]; declared {
		return true
	}
	includes, ok := tomlStrings(root["include"])
	if !ok {
		return true
	}
	for _, inc := range includes {
		if remotesource.IsRemote(inc) || strings.Contains(inc, "://") {
			return true
		}
		if fragmentMayDeclareStorage(localIncludePath(inc, cityPath)) {
			return true
		}
	}
	return false
}

// legacySessionsDatabaseEntries are the files a single-store city may keep at
// the default binding root: the legacy sessions database
// (sqlite.ReadLegacySessionsSnapshot) and its SQLite sidecars. None is a
// binding's.
var legacySessionsDatabaseEntries = map[string]bool{
	"sessions.db": true, "sessions.db-wal": true, "sessions.db-shm": true, "sessions.db-journal": true,
}

// defaultBindingRootInUse reports whether the default binding root inside
// cityPath holds anything but the legacy sessions database, or cannot be read:
// then a binding has been created there, whatever the config now says. The
// directory alone is no evidence, because a single-store city keeps that
// database in it.
func defaultBindingRootInUse(cityPath string) bool {
	entries, err := os.ReadDir(filepath.Join(cityPath, config.DefaultSQLiteStoragePath))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	for _, e := range entries {
		if !legacySessionsDatabaseEntries[e.Name()] {
			return true
		}
	}
	return false
}

// escapedQuotedKey matches a quoted key (or table header) whose spelling uses
// a backslash escape, e.g. "incl\u0075de" or [ "stor\u0061ge" ]: a key the
// bare-word scan cannot read in an unparseable file, so it counts as unknown.
var escapedQuotedKey = regexp.MustCompile(`(?m)^[ \t]*\[{0,2}[ \t]*"[^"\n]*\\`)

// fragmentMayDeclareStorage reports whether one include fragment declares, or
// may declare, a storage table: unreadable, or parsed with a storage key, or
// unparseable with storage anywhere in its text.
func fragmentMayDeclareStorage(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	data = stripUTF8BOM(data)
	table := map[string]any{}
	if _, err := toml.Decode(string(data), &table); err != nil {
		return strings.Contains(strings.ToLower(string(data)), "storage") || escapedQuotedKey.Match(data)
	}
	_, declared := table["storage"]
	return declared
}

// stripUTF8BOM drops a leading UTF-8 byte-order mark, which an editor may
// write and which would otherwise hide the first table header from a scan.
func stripUTF8BOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte("\ufeff"))
}

// localIncludePath resolves a local include the way config composition does:
// "//x" is city-root relative, an absolute path is itself, anything else is
// relative to the city root.
func localIncludePath(inc, cityPath string) string {
	if strings.HasPrefix(inc, "//") {
		return filepath.Join(cityPath, strings.TrimPrefix(inc, "//"))
	}
	if filepath.IsAbs(inc) {
		return inc
	}
	return filepath.Join(cityPath, inc)
}

// tomlStrings reads an optional array of strings; a value of any other shape
// is not-ok.
func tomlStrings(v any) ([]string, bool) {
	if v == nil {
		return nil, true
	}
	items, isList := v.([]any)
	if !isList {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, isString := item.(string)
		if !isString {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
