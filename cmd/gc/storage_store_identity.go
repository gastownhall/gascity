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
// .beads/identity.toml, else L2 .beads/metadata.json), its database and
// endpoint, and its scope path. The marker records it; boot compares it; the
// storage verbs compare it against every other registered city.
//
// The tuple is read from files only. Boot cannot afford a server round trip
// per process (the gate already runs in every one-shot CLI), and the files are
// what a recreated `.beads` changes. The endpoint is resolved only for the
// shared-store comparison, where a managed Dolt's port (which changes every
// boot) is meaningful, and never for the boot comparison, where it is not.
//
// S4b (#8486) carries its own Dolt-host normaliser for the rig importer;
// storeIdentityDoltHost has the same semantics and is the one it should adopt.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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
	// ProjectID is the beads project id: L1 identity.toml, else L2 metadata.json.
	ProjectID string
	// Database is the Dolt database the scope pins, or empty.
	Database string
	// Endpoint is the normalised server address (loopback folds to one
	// spelling, a host name resolves to its sorted addresses) or "unix:<socket>".
	// It is filled only by resolveWorkStoreIdentity, never recorded for the
	// boot comparison.
	Endpoint string
	// EndpointErr is why the endpoint could not be resolved, when it could not.
	EndpointErr error
	// Path is the canonical scope root.
	Path string
}

// empty reports a tuple with nothing to compare.
func (id workStoreIdentity) empty() bool {
	return id.ProjectID == "" && id.Database == "" && id.Path == ""
}

// Token is the identity an operator names in --force=shared-store:<identity>:
// the project id when there is one, else database@endpoint, else the path.
func (id workStoreIdentity) Token() string {
	switch {
	case id.ProjectID != "":
		return id.ProjectID
	case id.Database != "" && id.Endpoint != "":
		return id.Database + "@" + id.Endpoint
	case id.Database != "":
		return id.Database
	}
	return id.Path
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
	return id, nil
}

// resolveWorkStoreIdentity is readWorkStoreIdentity plus the endpoint, for
// comparing stores across cities.
func resolveWorkStoreIdentity(cityPath, scopeRoot string) (workStoreIdentity, error) {
	id, err := readWorkStoreIdentity(scopeRoot)
	if err != nil {
		return id, err
	}
	if id.Database == "" {
		return id, nil
	}
	target, ok, err := workStoreIdentityDoltTarget(cityPath, scopeRoot)
	switch {
	case err != nil:
		id.EndpointErr = err
	case !ok:
	case strings.TrimSpace(target.Socket) != "":
		id.Endpoint = "unix:" + filepath.Clean(strings.TrimSpace(target.Socket))
	default:
		host, herr := storeIdentityDoltHost(target.Host)
		if herr != nil {
			id.EndpointErr = herr
			break
		}
		id.Endpoint = host + ":" + strings.TrimSpace(target.Port)
	}
	return id, nil
}

// workStoreIdentityDoltTarget resolves a scope's Dolt connection target; a
// test seam.
var workStoreIdentityDoltTarget = canonicalScopeDoltTarget

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

// storeIdentityDoltHost folds the loopback and wildcard spellings to one and
// resolves any other host name to its sorted addresses, so two spellings of
// one server compare equal.
func storeIdentityDoltHost(host string) (string, error) {
	host = strings.Trim(strings.TrimSpace(strings.ToLower(host)), "[]")
	switch host {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0", "::":
		return "loopback", nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return "loopback", nil
		}
		return ip.String(), nil
	}
	addrs, err := storeIdentityLookupHost(host)
	if err == nil && len(addrs) == 0 {
		err = errors.New("no addresses")
	}
	if err != nil {
		return "", fmt.Errorf("resolving Dolt host %q: %w", host, err)
	}
	norm := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.IsLoopback() {
			norm = append(norm, "loopback")
			continue
		}
		norm = append(norm, a)
	}
	sort.Strings(norm)
	return strings.Join(norm, ","), nil
}

// storeIdentityLookupHost resolves a host name; a test seam.
var storeIdentityLookupHost = net.LookupHost

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

// workStoreShare is how two work stores compare across cities.
type workStoreShare int

const (
	workStoresDistinct workStoreShare = iota
	workStoresShared
	// workStoresUndecided names the same database where an endpoint could not
	// be resolved and no project id tells them apart.
	workStoresUndecided
)

// compareWorkStores decides whether two scopes' stores are one store.
func compareWorkStores(a, b workStoreIdentity) (workStoreShare, string) {
	if a.Path != "" && a.Path == b.Path {
		return workStoresShared, "the same store directory " + a.Path
	}
	if a.ProjectID != "" && b.ProjectID != "" {
		if a.ProjectID == b.ProjectID {
			return workStoresShared, "beads project " + a.ProjectID
		}
		return workStoresDistinct, ""
	}
	if a.Database == "" || a.Database != b.Database {
		return workStoresDistinct, ""
	}
	if a.EndpointErr != nil || b.EndpointErr != nil || a.Endpoint == "" || b.Endpoint == "" {
		why := a.EndpointErr
		if why == nil {
			why = b.EndpointErr
		}
		if why == nil {
			why = errors.New("no endpoint resolved")
		}
		return workStoresUndecided, fmt.Sprintf("Dolt database %s, whose endpoints cannot be compared (%v)", a.Database, why)
	}
	if a.Endpoint == b.Endpoint {
		return workStoresShared, fmt.Sprintf("Dolt database %s at %s", a.Database, a.Endpoint)
	}
	return workStoresDistinct, ""
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
// marker writer read it. A scope whose identity files cannot be read records
// nothing (a legacy-shaped marker, back-filled once they can) rather than
// failing a boot over a file other checks already report.
func currentWorkStoreIdentity(cityPath string) workStoreIdentity {
	id, err := readWorkStoreIdentity(cityPath)
	if err != nil {
		return workStoreIdentity{}
	}
	return id
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
