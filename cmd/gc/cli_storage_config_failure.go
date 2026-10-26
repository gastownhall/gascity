package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
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
//   - the city has served a split, or cleared its work store (the durable
//     notes storageBootGate already holds a revert with);
//   - the root city.toml, the city pack.toml, or one of the fragments either
//     of them includes cannot be read, or is remote;
//   - any of those files declares a [storage] table. A file that does not
//     parse as TOML is scanned as text for a storage table or key instead:
//     the commonest load failure is a syntax error, and a syntax error in a
//     city that never declared [storage] says nothing about where its classes
//     live. (The served-binding note above is the stronger signal anyway:
//     every city that has ever served a split carries it.)
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
	root, verdict := scanConfigFile(filepath.Join(cityPath, "city.toml"))
	if verdict != configFileSilent {
		return true
	}
	var includes []string
	if root != nil {
		var ok bool
		if includes, ok = tomlStrings(root["include"]); !ok {
			return true
		}
	}
	packPath := filepath.Join(cityPath, "pack.toml")
	if _, err := os.Stat(packPath); err == nil {
		pack, verdict := scanConfigFile(packPath)
		if verdict != configFileSilent {
			return true
		}
		if section, isTable := pack["pack"].(map[string]any); isTable {
			packIncludes, ok := tomlStrings(section["includes"])
			if !ok {
				return true
			}
			includes = append(includes, packIncludes...)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return true
	}
	for _, inc := range includes {
		if remotesource.IsRemote(inc) || strings.Contains(inc, "://") {
			return true
		}
		if _, verdict := scanConfigFile(localIncludePath(inc, cityPath)); verdict != configFileSilent {
			return true
		}
	}
	return false
}

// configFileVerdict is what one config file says about [storage].
type configFileVerdict int

const (
	// configFileSilent: the file was read and declares no storage.
	configFileSilent configFileVerdict = iota
	// configFileDeclaresStorage: the file declares a storage table or key.
	configFileDeclaresStorage
	// configFileUnknown: the file could not be read.
	configFileUnknown
)

// storageDeclarationText matches a storage table header ([storage],
// [storage.classes], [[storage...]]) or a dotted/inline storage key at the
// start of a line, for a file that does not parse as TOML.
var storageDeclarationText = regexp.MustCompile(`(?m)^[ \t]*(\[\[?[ \t]*["']?storage["']?[ \t]*[\].]|["']?storage["']?[ \t]*[.=])`)

// scanConfigFile reads one config file and reports whether it declares
// storage. A file that parses returns its decoded table; one that does not is
// scanned as text and returns a nil table.
func scanConfigFile(path string) (map[string]any, configFileVerdict) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, configFileUnknown
	}
	table := map[string]any{}
	if _, err := toml.Decode(string(data), &table); err != nil {
		if storageDeclarationText.Match(data) {
			return nil, configFileDeclaresStorage
		}
		return nil, configFileSilent
	}
	if _, declared := table["storage"]; declared {
		return table, configFileDeclaresStorage
	}
	return table, configFileSilent
}

// localIncludePath resolves a local include the way config composition does:
// "//x" is city-root relative, an absolute path is itself, anything else is
// relative to the city root (where city.toml and pack.toml both live).
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
