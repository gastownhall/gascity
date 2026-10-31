package main

// Work-store identity (S5 §6.3, §7.2).
//
// A binding at .gc/store holds the infrastructure state of ONE work store: the
// rows its proven copy carried out of it. Two failures follow from losing track
// of which one.
//
//   - A reused directory. `.beads` is recreated (a fresh bd init mints a new
//     project id) while `.gc` is kept. The convergence marker is still there,
//     the new work store holds no infrastructure row, containment passes
//     vacuously, and the city serves the previous city's sessions and mail.
//   - A shared store. Two cities point their work stores at one database. A
//     migration of one copies, and then clears, rows the other owns.
//
// Both are answered by the same tuple: the work store's project id (L1
// contract.ProjectIdentityPath, else L2 .beads/metadata.json), its database and
// endpoint, and its scope path. The marker records it and boot compares it.
//
// The tuple is read from files only. Boot cannot afford a server round trip
// per process (the gate already runs in every one-shot CLI), and the files are
// what a recreated `.beads` changes. The endpoint is resolved only for the
// shared-store comparison, never for the boot comparison. Even there a managed
// Dolt is named by the data directory it serves, not by its port: the port can
// change at every boot, and there is none while the city is stopped, which is
// what every writing storage verb requires.
//
// The cross-city comparison (shared stores) is a different question and has
// one implementation, shared with the rig importer: storeDoltIdentityOf,
// normalizeDoltHost and findStoreSharers (storage_shared_store.go).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// workStoreIdentityLinePrefix starts the marker line that records the work
// store a binding belongs to. The shape matches S4b's rig records
// (`store <root>`): a keyword, then the value.
const workStoreIdentityLinePrefix = "work-store "

// workStoreIdentity is the identity tuple of one scope's work store.
type workStoreIdentity struct {
	// ProjectID is the beads project id: L1 (contract.ProjectIdentityPath), else L2 metadata.json.
	ProjectID string
	// Database is the Dolt database the scope pins, or empty.
	Database string
	// Path is the canonical scope root.
	Path string
}

// empty reports a tuple with nothing to compare.
func (id workStoreIdentity) empty() bool {
	return id.ProjectID == "" && id.Database == "" && id.Path == ""
}

// String renders the tuple as the marker records it. Values are
// query-escaped so a path with spaces stays one field.
func (id workStoreIdentity) String() string {
	field := func(k, v string) string {
		if v == "" {
			v = "-"
		} else {
			v = url.QueryEscape(v)
		}
		return k + "=" + v
	}
	return strings.Join([]string{
		field("project", id.ProjectID),
		field("database", id.Database),
		field("path", id.Path),
	}, " ")
}

// parseWorkStoreIdentity reads String's rendering back.
func parseWorkStoreIdentity(s string) (workStoreIdentity, error) {
	var id workStoreIdentity
	for _, f := range strings.Fields(s) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return workStoreIdentity{}, fmt.Errorf("work-store identity field %q has no value", f)
		}
		if v == "-" {
			v = ""
		} else {
			unescaped, err := url.QueryUnescape(v)
			if err != nil {
				return workStoreIdentity{}, fmt.Errorf("work-store identity field %q: %w", f, err)
			}
			v = unescaped
		}
		switch k {
		case "project":
			id.ProjectID = v
		case "database":
			id.Database = v
		case "path":
			id.Path = v
		default:
			// A later build may record more; an unknown key is not a mismatch.
		}
	}
	return id, nil
}

// readWorkStoreIdentity reads a scope's identity from its files: no server,
// no endpoint. A scope with no .beads yields only its path.
func readWorkStoreIdentity(scopeRoot string) (workStoreIdentity, error) {
	id := workStoreIdentity{Path: canonicalIdentityPath(scopeRoot)}
	l1, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil {
		return workStoreIdentity{}, err
	}
	if ok {
		id.ProjectID = l1
	}
	metadataPath := scopeMetadataJSONPath(scopeRoot)
	if id.ProjectID == "" {
		l2, err := readManagedMetadataProjectID(metadataPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return workStoreIdentity{}, err
		}
		id.ProjectID = l2
	}
	db, _, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return workStoreIdentity{}, err
	}
	id.Database = strings.TrimSpace(db)
	if id.Database == "" {
		// A hosted bd store names its database as storage_database.
		hosted, err := readHostedStoreBinding(scopeRoot)
		if err != nil {
			return workStoreIdentity{}, err
		}
		id.Database = hosted.Database
	}
	return id, nil
}

func canonicalIdentityPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}

// workStoreIdentityMismatch compares the identity a marker recorded with the
// work store's current one, and returns why they name different stores, or ""
// when nothing recorded contradicts the current store.
//
// The project id decides when both carry one: it is what a recreated `.beads`
// changes. Without it on both sides the database decides. The path never
// does: a city directory moved with its `.gc` is the same city.
func workStoreIdentityMismatch(recorded, current workStoreIdentity) string {
	switch {
	case recorded.ProjectID != "" && current.ProjectID != "":
		if recorded.ProjectID != current.ProjectID {
			return fmt.Sprintf("the binding was converged from work store project %s and this city's work store is project %s", recorded.ProjectID, current.ProjectID)
		}
		return ""
	case recorded.Database != "" && current.Database != "":
		if recorded.Database != current.Database {
			return fmt.Sprintf("the binding was converged from work-store database %s and this city's work store is database %s", recorded.Database, current.Database)
		}
	case recorded.ProjectID != "" && current.ProjectID == "" && current.Database == "":
		return fmt.Sprintf("the binding was converged from work store project %s and this city's work store carries no project identity at all", recorded.ProjectID)
	}
	return ""
}

// readMarkerWorkStoreIdentity reads the identity line of a convergence marker.
// recorded is false for a legacy marker with no such line.
func readMarkerWorkStoreIdentity(markerPath string) (id workStoreIdentity, recorded bool, err error) {
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return workStoreIdentity{}, false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), strings.TrimSpace(workStoreIdentityLinePrefix)+" ")
		if !ok {
			continue
		}
		id, err := parseWorkStoreIdentity(rest)
		if err != nil {
			return workStoreIdentity{}, false, fmt.Errorf("reading %s: %w", markerPath, err)
		}
		return id, true, nil
	}
	return workStoreIdentity{}, false, nil
}

// currentWorkStoreIdentity is the city work store's identity as boot and the
// marker writer read it. An identity file that exists and cannot be read or
// parsed is an error, never an empty identity: callers must not read it as
// "a different work store" (or record a marker without one).
func currentWorkStoreIdentity(cityPath string) (workStoreIdentity, error) {
	id, err := readWorkStoreIdentity(cityPath)
	if err != nil {
		return workStoreIdentity{}, fmt.Errorf("reading this city's work-store identity (%s, %s): %w", contract.ProjectIdentityPath(cityPath), scopeMetadataJSONPath(cityPath), err)
	}
	return id, nil
}

// backfillMarkerWorkStoreIdentity adds the work-store line to a legacy marker,
// keeping its content after it, atomically (temp + rename).
func backfillMarkerWorkStoreIdentity(target infraBindingTarget, id workStoreIdentity) error {
	data, err := os.ReadFile(target.MarkerPath())
	if err != nil {
		return err
	}
	// Prepended: the write time stays the file's last field (readInfraMarkerBound).
	body := workStoreIdentityLinePrefix + id.String() + "\n" + string(data)
	tmp, err := os.CreateTemp(target.Root, infraMigratedMarkerName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, target.MarkerPath()); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// backfillMarkerWorkStoreIdentityGuarded back-fills under the migration
// guard, re-reading the marker once it holds it. A guard another process (or
// this one's own migration) holds skips the back-fill: the next boot does it.
func backfillMarkerWorkStoreIdentityGuarded(cityPath string, target infraBindingTarget, id workStoreIdentity) error {
	guard, err := acquireStorageMigrationGuard(context.Background(), cityPath)
	if err != nil {
		return nil //nolint:nilerr // a held guard defers the back-fill to a later boot
	}
	defer guard.Release() //nolint:errcheck // best-effort release
	if _, recorded, err := readMarkerWorkStoreIdentity(target.MarkerPath()); err != nil || recorded {
		return err
	}
	return backfillMarkerWorkStoreIdentity(target, id)
}
