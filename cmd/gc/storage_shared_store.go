package main

// Shared stores: one identity resolver, one comparator, one attestation.
//
// A city store or a rig store that another registered city also uses holds
// rows that city owns, and every writing storage verb copies and then clears
// rows. The city migration (S5-A) and the rig importer (S4b) ask the same
// question, so they share this file: storeDoltIdentityOf resolves where a
// scope's store lives, findStoreSharers compares it with every store (the
// city store and each rig store) of every other registered city, and
// sharedStoreRefusal refuses, or, under --force=shared-store:<identity>,
// requires every co-tenant to be stopped.
//
// The rules are S4b's, the stricter ones:
//   - the same scope directory is one store;
//   - a scope with no database (a file store) shares nothing else;
//   - the database is compared from the RESOLVED connection target, and with
//     its normalised endpoint, so two databases that share only a name are
//     distinct; the beads project id does not decide (two stores with the
//     same database at the same endpoint are one store whatever their ids);
//   - an endpoint that cannot be resolved is compared by the database the
//     scope pins in .beads/metadata.json, and a scope that resolves nothing
//     and pins nothing cannot be told apart from anything, so it refuses;
//   - a hosted bd store is its storage_database at its storage_endpoint.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// storeDoltIdentity is the database a scope's store lives in.
type storeDoltIdentity struct {
	// Database is the database name; empty for a scope with no
	// authoritative Dolt target (a file store).
	Database string
	// Endpoint is the normalised server address (loopback spellings fold to
	// one; a host name resolves to its sorted addresses), the socket path,
	// or a hosted bd endpoint.
	Endpoint string
	// Unresolved reports that the endpoint could not be resolved, so the
	// identity names a database without saying where it lives.
	Unresolved bool
	Err        error
}

// token is the identity an operator names in --force=shared-store:<identity>.
func (id storeDoltIdentity) token(root string) string {
	switch {
	case id.Database != "" && id.Endpoint != "":
		return id.Database + "@" + id.Endpoint
	case id.Database != "":
		return id.Database
	}
	return canonicalIdentityPath(root)
}

// hostedStoreBinding is a hosted bd scope's storage target.
type hostedStoreBinding struct {
	Database string
	Endpoint string
}

// readHostedStoreBinding reads storage_database and storage_endpoint from a
// scope's .beads/metadata.json; zero without them.
func readHostedStoreBinding(scopeRoot string) (hostedStoreBinding, error) {
	data, err := os.ReadFile(scopeMetadataJSONPath(scopeRoot))
	if errors.Is(err, os.ErrNotExist) {
		return hostedStoreBinding{}, nil
	}
	if err != nil {
		return hostedStoreBinding{}, err
	}
	var meta struct {
		StorageEndpoint string `json:"storage_endpoint"`
		StorageDatabase string `json:"storage_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return hostedStoreBinding{}, fmt.Errorf("parsing %s: %w", scopeMetadataJSONPath(scopeRoot), err)
	}
	return hostedStoreBinding{Database: strings.TrimSpace(meta.StorageDatabase), Endpoint: strings.TrimSpace(meta.StorageEndpoint)}, nil
}

// normalizeHostedEndpoint drops credentials and folds the host spelling, so
// two spellings of one hosted bd server compare equal.
func normalizeHostedEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "hosted:" + strings.ToLower(strings.TrimSpace(endpoint))
	}
	host := u.Hostname()
	if norm, err := normalizeDoltHost(host); err == nil {
		host = norm
	}
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	return "hosted:" + strings.ToLower(u.Scheme) + "://" + host + strings.TrimRight(u.Path, "/")
}

// storeDoltIdentityOf resolves the database a scope's store lives in; a test
// seam.
var storeDoltIdentityOf = func(cityPath, scopeRoot string) storeDoltIdentity {
	if hosted, err := readHostedStoreBinding(scopeRoot); err == nil && hosted.Database != "" {
		return storeDoltIdentity{Database: hosted.Database, Endpoint: normalizeHostedEndpoint(hosted.Endpoint)}
	}
	target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot)
	if err != nil {
		// The endpoint could not be resolved (that city's Dolt is down, its
		// runtime state is gone). The database name is still the scope's own
		// pin in .beads/metadata.json: compare by it, and leave it empty,
		// which refuses, only when that cannot be read either.
		db, _, _ := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
		return storeDoltIdentity{Database: strings.TrimSpace(db), Unresolved: true, Err: err}
	}
	if !ok || strings.TrimSpace(target.Database) == "" {
		return storeDoltIdentity{}
	}
	id := storeDoltIdentity{Database: strings.TrimSpace(target.Database)}
	if socket := strings.TrimSpace(target.Socket); socket != "" {
		id.Endpoint = "unix:" + filepath.Clean(socket)
		return id
	}
	host, herr := normalizeDoltHost(target.Host)
	if herr != nil {
		id.Unresolved, id.Err = true, herr
		return id
	}
	id.Endpoint = host + ":" + strings.TrimSpace(target.Port)
	return id
}

// normalizeDoltHost folds the loopback spellings (localhost, 127.0.0.1, ::1,
// empty) to one and resolves any other host name to its sorted addresses,
// so two spellings of one server compare equal.
func normalizeDoltHost(host string) (string, error) {
	host = strings.Trim(strings.TrimSpace(strings.ToLower(host)), "[]")
	switch host {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0", "::":
		// A wildcard listen address names this host too: a client
		// configured with it reaches the server over loopback.
		return "loopback", nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return "loopback", nil
		}
		return ip.String(), nil
	}
	addrs, err := doltHostLookup(host)
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

// doltHostLookup resolves a host name; a test seam.
var doltHostLookup = net.LookupHost

// storeSharerCities lists the other cities registered on this machine; a
// test seam.
var storeSharerCities = func(cityPath string) ([]string, error) {
	entries, err := otherRegisteredCities(cityPath)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return paths, nil
}

// storeSharer is one store of another registered city that a store of this
// city is, or cannot be told apart from.
type storeSharer struct {
	// OtherPath is the other city's directory.
	OtherPath string
	// Refusal is the sentence that names the two stores and why.
	Refusal string
}

// findStoreSharers compares the store at selfRoot (label names it, e.g.
// "this city's work store" or "rig packs") with every store of every other
// registered city. It returns this store's attestation token and every
// sharer; an error is a registry or config that could not be read.
func findStoreSharers(cityPath, label, selfRoot string) (string, []storeSharer, error) {
	self := storeDoltIdentityOf(cityPath, selfRoot)
	token := self.token(selfRoot)
	others, err := storeSharerCities(cityPath)
	if err != nil {
		return token, nil, fmt.Errorf("%s: reading the city registry to rule out a shared store: %w; nothing was written", label, err)
	}
	// A store with no Dolt database (a file store) shares none but its
	// directory. One whose endpoint cannot be resolved and which pins no
	// database name cannot be told apart from any other city's Dolt store,
	// so it is compared, and refused, rather than skipped.
	noDatabase := self.Database == "" && !self.Unresolved
	var sharers []storeSharer
	for _, otherPath := range others {
		cfg, err := loadCityConfigWithoutBuiltinPackRefresh(otherPath, io.Discard)
		if err == nil && cfg == nil {
			err = errors.New("it loaded no configuration")
		}
		if err != nil {
			return token, nil, fmt.Errorf("%s: city %s is registered but its config cannot be read (%w), so it cannot be ruled out as sharing the store; nothing was written", label, otherPath, err)
		}
		type scope struct{ name, root string }
		scopes := []scope{{"city store", otherPath}}
		for _, other := range cfg.Rigs {
			if strings.TrimSpace(other.Path) != "" {
				scopes = append(scopes, scope{"rig " + other.Name, rigStoreRoot(otherPath, other)})
			}
		}
		for _, sc := range scopes {
			add := func(format string, args ...any) {
				sharers = append(sharers, storeSharer{OtherPath: otherPath, Refusal: fmt.Sprintf(format, args...)})
			}
			if samePath(sc.root, selfRoot) {
				add("%s's store %s is also city %s's %s: a shared store holds rows another city owns", label, selfRoot, otherPath, sc.name)
				continue
			}
			if noDatabase {
				continue
			}
			theirs := storeDoltIdentityOf(otherPath, sc.root)
			if theirs.Database == "" && !theirs.Unresolved {
				continue
			}
			switch {
			case self.Unresolved && self.Database == "":
				add("%s's Dolt endpoint cannot be resolved (%v) and its .beads/metadata.json pins no dolt_database, so it cannot be ruled out as city %s's %s's database: start its Dolt server so the endpoint resolves, or pin its database name", label, self.Err, otherPath, sc.name)
			case self.Unresolved || theirs.Unresolved:
				if theirs.Database != "" && theirs.Database != self.Database {
					continue
				}
				why := theirs.Err
				if self.Unresolved {
					why = self.Err
				}
				add("%s's Dolt database %q may also be city %s's %s, and the endpoints cannot be compared (%v): start that city's Dolt server (or this one) so they can be, or unregister that city if it is gone", label, self.Database, otherPath, sc.name, why)
			case theirs.Database == self.Database && theirs.Endpoint == self.Endpoint:
				add("%s's Dolt database %s at %s is also city %s's %s: a shared store holds rows another city owns", label, self.Database, self.Endpoint, otherPath, sc.name)
			}
		}
	}
	return token, sharers, nil
}

// sharedStoreRefusal refuses the store at selfRoot when another registered
// city uses it, unless force attests its token; under the attestation every
// co-tenant city must be stopped: its controller lock must be free (a live
// one refuses even then). attested reports that the attestation was used.
func sharedStoreRefusal(cityPath, label, selfRoot string, force storageForce) (attested bool, err error) {
	token, sharers, err := findStoreSharers(cityPath, label, selfRoot)
	if err != nil {
		return false, err
	}
	if len(sharers) == 0 {
		return false, nil
	}
	if !force.attestsSharedStore(token) {
		return false, fmt.Errorf("%s. If this city is the single writer of what it migrates, attest it with --%s=%s:%s: rows another city's scope names still refuse, and every other city on the store must be stopped; nothing was written",
			sharers[0].Refusal, storageForceFlag, storageHazardSharedStore, token)
	}
	seen := map[string]bool{}
	for _, s := range sharers {
		if seen[s.OtherPath] {
			continue
		}
		seen[s.OtherPath] = true
		probe, err := acquireControllerLock(s.OtherPath)
		if errors.Is(err, errControllerAlreadyRunning) {
			return false, fmt.Errorf("city %s shares %s and its controller is live; --%s=%s:%s does not lift that. Stop it (gc stop %s) and run this again", s.OtherPath, label, storageForceFlag, storageHazardSharedStore, token, s.OtherPath)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("probing city %s's controller lock: %w", s.OtherPath, err)
		}
		if probe != nil {
			probe.Close() //nolint:errcheck // probe only
		}
	}
	return true, nil
}
