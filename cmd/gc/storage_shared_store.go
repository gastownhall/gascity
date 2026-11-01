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
// requires every co-tenant to be stopped and keeps it stopped until the verb
// is done. The registry is all the check sees, so an attestation that finds no
// registered co-tenant refuses too.
//
// The rules are S4b's, the stricter ones:
//   - the same scope directory is one store;
//   - a scope with no database (a file store) shares nothing else;
//   - the database is compared from the RESOLVED connection target, and with
//     its normalised endpoint, so two databases that share only a name are
//     distinct; the beads project id does not decide (two stores with the
//     same database at the same endpoint are one store whatever their ids);
//   - a store on the Dolt server gc manages for its city is named by the
//     data directory that server serves (<city>/.beads/dolt), read from files
//     whether or not the server is up: the writing verbs run with the city
//     stopped, when it has no port, and the port can change at every boot.
//     An endpoint at the address the server's runtime state records is that
//     server too;
//   - any other endpoint that cannot be resolved is compared by the database
//     the scope pins in .beads/metadata.json, and a scope that resolves
//     nothing and pins nothing cannot be told apart from anything, so it
//     refuses;
//   - a hosted bd store is its storage_database at its storage_endpoint, with
//     the scheme's default port the same as none.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// one; a host name resolves to its sorted addresses), the socket path, a
	// hosted bd endpoint, or the data directory the Dolt server gc manages
	// for a city serves (managedStoreIdentity).
	Endpoint string
	// ServedAt is the address a managed server's runtime state records, live
	// or stopped: an endpoint another city pins there names this server.
	ServedAt string
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

// sharedAt reports whether two resolved identities name one server, and
// spells where: their endpoint, or both halves of a managed server and an
// endpoint at the address its runtime state records.
func (id storeDoltIdentity) sharedAt(other storeDoltIdentity) (string, bool) {
	switch {
	case id.Endpoint == other.Endpoint:
		return id.Endpoint, true
	case id.ServedAt != "" && id.ServedAt == other.Endpoint:
		return id.Endpoint + " (served at " + id.ServedAt + ")", true
	case other.ServedAt != "" && other.ServedAt == id.Endpoint:
		return id.Endpoint + " (where " + other.Endpoint + " is served)", true
	}
	return "", false
}

// managedDoltEndpointPrefix starts the endpoint of a store on the Dolt server
// gc manages for its city, which names the data directory that server serves.
const managedDoltEndpointPrefix = "managed:"

// doltLoopbackHost is every loopback spelling of a Dolt host, folded.
const doltLoopbackHost = "loopback"

// managedStoreIdentity names database on the Dolt server gc manages for the
// city at cityPath. That server serves exactly <city>/.beads/dolt, so the data
// directory names it, live or stopped, and two cities' managed servers are two
// stores whatever their databases are called. Its port does not name it: the
// port can change at every boot, and there is none while the city is stopped,
// which is what every writing storage verb requires.
func managedStoreIdentity(cityPath, database string) storeDoltIdentity {
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	return storeDoltIdentity{
		Database: strings.TrimSpace(database),
		Endpoint: managedDoltEndpointPrefix + canonicalIdentityPath(dataDir),
		ServedAt: managedDoltRecordedAddr(cityPath, dataDir),
	}
}

// managedDoltRecordedAddr is the address the runtime state of a city's managed
// Dolt server records for its data directory, read whether or not the server
// is up (a stop keeps the port in the provider's state). The server listens on
// loopback. Empty when no state records one.
func managedDoltRecordedAddr(cityPath, dataDir string) string {
	for _, path := range []string{managedDoltStatePath(cityPath), providerManagedDoltStatePath(cityPath)} {
		state, err := readDoltRuntimeStateFile(path)
		if err == nil && state.Port > 0 && samePath(strings.TrimSpace(state.DataDir), dataDir) {
			return doltLoopbackHost + ":" + strconv.Itoa(state.Port)
		}
	}
	return ""
}

// gcManagedDoltScope reports whether the store at scopeRoot is on the Dolt
// server gc manages for the city at cityPath, by the marker only gc writes,
// which contract.ResolveDoltConnectionTarget rests on too: the city's
// .beads/config.yaml says managed_city, and a rig's says inherited_city.
func gcManagedDoltScope(cityPath, scopeRoot string) bool {
	origin := func(root string) contract.EndpointOrigin {
		cfg, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(root, ".beads", "config.yaml"))
		if err != nil || !ok {
			// Not managed then: the resolution reads the same file and
			// reports the error as the reason the endpoint is unresolved.
			return ""
		}
		return cfg.EndpointOrigin
	}
	if origin(cityPath) != contract.EndpointOriginManagedCity {
		return false
	}
	return samePath(scopeRoot, cityPath) || origin(scopeRoot) == contract.EndpointOriginInheritedCity
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

// hostedSchemeDefaultPorts are the ports a hosted endpoint's scheme implies.
// A URL that spells its scheme's default port names the same server as one
// that spells none (RFC 3986 §6.2.3).
var hostedSchemeDefaultPorts = map[string]string{"http": "80", "https": "443"}

// normalizeHostedEndpoint drops credentials and folds the host spelling and
// the port spelling (the number it names, and none for the scheme's default),
// so two spellings of one hosted bd server compare equal.
func normalizeHostedEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "hosted:" + strings.ToLower(strings.TrimSpace(endpoint))
	}
	scheme := strings.ToLower(u.Scheme)
	host := u.Hostname()
	if norm, err := normalizeDoltHost(host); err == nil {
		host = norm
	}
	port := u.Port()
	if n, err := strconv.ParseUint(port, 10, 16); err == nil {
		port = strconv.FormatUint(n, 10)
	}
	if port != "" && port != hostedSchemeDefaultPorts[scheme] {
		host += ":" + port
	}
	return "hosted:" + scheme + "://" + host + strings.TrimRight(u.Path, "/")
}

// storeDoltIdentityOf resolves the database a scope's store lives in; a test
// seam.
var storeDoltIdentityOf = func(cityPath, scopeRoot string) storeDoltIdentity {
	if hosted, err := readHostedStoreBinding(scopeRoot); err == nil && hosted.Database != "" {
		return storeDoltIdentity{Database: hosted.Database, Endpoint: normalizeHostedEndpoint(hosted.Endpoint)}
	}
	// A store on the Dolt server gc manages for its city is that server's data
	// directory, up or down, unless beads proxies it to an external upstream,
	// which is a server like any other.
	managed := gcManagedDoltScope(cityPath, scopeRoot)
	target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot)
	if err != nil {
		db, pinned, dbErr := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
		// A managed server that is not running (`gc stop` stops it) has no
		// runtime state to resolve, and is where it was.
		if managed && contract.IsManagedRuntimeUnavailable(err) && dbErr == nil {
			if !pinned {
				db = contract.DefaultDoltDatabase
			}
			return managedStoreIdentity(cityPath, db)
		}
		// Any other endpoint that cannot be resolved is compared by the
		// database the scope pins in .beads/metadata.json, and by none, which
		// refuses, when that cannot be read either.
		return storeDoltIdentity{Database: strings.TrimSpace(db), Unresolved: true, Err: err}
	}
	if !ok || strings.TrimSpace(target.Database) == "" {
		return storeDoltIdentity{}
	}
	if managed && !target.External {
		return managedStoreIdentity(cityPath, target.Database)
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
		return doltLoopbackHost, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return doltLoopbackHost, nil
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
			norm = append(norm, doltLoopbackHost)
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
		cfg, err := readOtherCityConfig(otherPath)
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
				add("%s's Dolt endpoint cannot be resolved (%v) and its .beads/metadata.json pins no dolt_database, so it cannot be ruled out as city %s's %s's database: fix what keeps the endpoint from resolving, or pin its database name", label, self.Err, otherPath, sc.name)
			case self.Unresolved || theirs.Unresolved:
				if theirs.Database != "" && theirs.Database != self.Database {
					continue
				}
				why := theirs.Err
				if self.Unresolved {
					why = self.Err
				}
				add("%s's Dolt database %q may also be city %s's %s, and the endpoints cannot be compared (%v): fix what keeps that endpoint from resolving so they can be, or unregister that city if it is gone", label, self.Database, otherPath, sc.name, why)
			case theirs.Database == self.Database:
				if at, shared := self.sharedAt(theirs); shared {
					add("%s's Dolt database %s at %s is also city %s's %s: a shared store holds rows another city owns", label, self.Database, at, otherPath, sc.name)
				}
			}
		}
	}
	return token, sharers, nil
}

// sharedStoreExclusion is what the shared-store check leaves held. Under an
// accepted attestation it holds the controller lock of every co-tenant city,
// so none of them can start, and write the shared store, while the verb copies
// and clears it: a co-tenant stopped for one instant is the ping-grade proof
// storage_verb_exclusion.go rejects, since the supervisor restarts every
// registered city.
type sharedStoreExclusion struct {
	// Attested reports that the attestation lifted a refusal.
	Attested bool
	// token is the identity the lifting attestation named.
	token string
	locks []*os.File
}

// Release drops the co-tenant locks. The verb releases them when it exits,
// after its writes and their cleanup, and before its own controller lock.
func (e sharedStoreExclusion) Release() {
	for _, lock := range e.locks {
		lock.Close() //nolint:errcheck // releasing a held lock
	}
}

// hazards lists, for the record, the attestation the exclusion was accepted
// under: none unless it lifted a refusal.
func (e sharedStoreExclusion) hazards() []string {
	if !e.Attested {
		return nil
	}
	return []string{storageHazardSharedStore + ":" + e.token}
}

// sharedStoreFinding is what the shared-store check finds before it takes any
// lock: the store's attestation token, every store of another registered city
// it shares, and whether the operator attested the token.
type sharedStoreFinding struct {
	Token    string
	Sharers  []storeSharer
	Attested bool
}

// coTenants lists each other city that shares the store once, in the order
// found.
func (f sharedStoreFinding) coTenants() []string {
	seen := map[string]bool{}
	var paths []string
	for _, s := range f.Sharers {
		if !seen[s.OtherPath] {
			seen[s.OtherPath] = true
			paths = append(paths, s.OtherPath)
		}
	}
	return paths
}

// findSharedStore is the shared-store check up to the co-tenant locks. It
// refuses what no lock changes: a store another registered city uses, unless
// force attests its token, and an attestation of this store that finds no
// registered co-tenant. The registry is all this check sees, and `gc stop`
// unregisters the city it stops: a co-tenant stopped that way would go unseen,
// the foreign-row refusal that runs under a lifting attestation would be
// skipped, and its rows could be carried and cleared as this city's.
//
// It takes no lock and writes nothing, so `gc storage preflight` runs it as it
// is, and an attested run with co-tenants is all that is left to the lock.
func findSharedStore(cityPath, label, selfRoot string, force storageForce) (sharedStoreFinding, error) {
	token, sharers, err := findStoreSharers(cityPath, label, selfRoot)
	if err != nil {
		return sharedStoreFinding{Token: token}, err
	}
	finding := sharedStoreFinding{Token: token, Sharers: sharers, Attested: force.attestsSharedStore(token)}
	switch {
	case len(sharers) == 0 && finding.Attested:
		return finding, fmt.Errorf("--%s=%s:%s attests that another city shares %s, but no other registered city does. This check sees only registered cities, and `%s` unregisters the city it stops (as `gc unregister` does), so a co-tenant stopped that way goes unseen and its rows could be carried and cleared as this city's. Register it again (`gc register <path>`), stop it with `%s`, which keeps it registered, and run this again. If no other city uses this store any more, run this again without the attestation; nothing was written",
			storageForceFlag, storageHazardSharedStore, token, label, storageStopCommand, storageCoTenantStopCommand)
	case len(sharers) > 0 && !finding.Attested:
		refusals := make([]string, 0, len(sharers))
		for _, s := range sharers {
			refusals = append(refusals, s.Refusal)
		}
		return finding, fmt.Errorf("%s. If this city is the single writer of what it migrates, attest it with --%s=%s:%s: rows another city's scope names still refuse, and every other city on the store must be stopped and stay registered (`%s` does both; `%s` unregisters a city, which hides it from this check); nothing was written",
			strings.Join(refusals, "; "), storageForceFlag, storageHazardSharedStore, token, storageCoTenantStopCommand, storageStopCommand)
	}
	return finding, nil
}

// sharedStoreRefusal refuses the store at selfRoot as findSharedStore does.
// Under an attestation that lifts its refusal every co-tenant city must be
// stopped, and it stays stopped: its controller lock is taken (a held one
// refuses even then) and held in the returned exclusion, which the caller
// releases when it exits. A refusal releases every lock it took.
func sharedStoreRefusal(cityPath, label, selfRoot string, force storageForce) (sharedStoreExclusion, error) {
	finding, err := findSharedStore(cityPath, label, selfRoot, force)
	if err != nil || len(finding.Sharers) == 0 {
		return sharedStoreExclusion{}, err
	}
	held := sharedStoreExclusion{Attested: true, token: finding.Token}
	for _, otherPath := range finding.coTenants() {
		lock, err := acquireCoTenantControllerLock(otherPath)
		if err != nil {
			held.Release()
			if errors.Is(err, errControllerAlreadyRunning) {
				return sharedStoreExclusion{}, fmt.Errorf("city %s shares %s, and %s holds its controller lock (.gc/controller.lock); --%s=%s:%s does not lift that. Stop it so that it stays registered (`%s` stops every supervisor-hosted city and keeps its registration; `%s` unregisters a city, which hides it from this check), and run this again",
					otherPath, label, controllerLockHolder(probeControllerIdentity(otherPath)), storageForceFlag, storageHazardSharedStore, finding.Token, storageCoTenantStopCommand, storageStopCommand)
			}
			return sharedStoreExclusion{}, fmt.Errorf("taking city %s's controller lock to keep it stopped: %w", otherPath, err)
		}
		held.locks = append(held.locks, lock)
	}
	return held, nil
}

// acquireCoTenantControllerLock takes a co-tenant city's controller lock. A
// city that never started has no .gc yet, and its first start creates it and
// then takes the lock, so the directory is created here as that start would:
// otherwise the city could start mid-run.
func acquireCoTenantControllerLock(otherPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Join(otherPath, ".gc"), 0o755); err != nil {
		return nil, fmt.Errorf("creating its runtime directory: %w", err)
	}
	return acquireControllerLock(otherPath)
}
