package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// providerOwnedIdentityTimeout bounds one whole provider-owned identity
// reconciliation: endpoint resolution, the L3 session, and the writes.
const providerOwnedIdentityTimeout = 5 * time.Second

// providerOwnedIdentityEndpoint is where a provider-owned scope's L3 (the
// Dolt metadata table) can be reached for reconciliation. A proxied endpoint
// with port 0 is the no-record shape: nothing to dial this pass.
type providerOwnedIdentityEndpoint struct {
	proxied  bool
	port     int
	target   contract.DoltConnectionTarget
	database string
}

// providerOwnedIdentityL3Session is the L3 surface of one provider-owned
// scope: a read of the metadata table's _project_id, a seed of it, and the
// close of whatever carries them (nil when the handle is pooled).
type providerOwnedIdentityL3Session struct {
	read  func(context.Context) (string, bool, error)
	seed  func(context.Context, string) (bool, error)
	close func() error
}

// Seams for tests: the reconcile entry, the L3 opener, the recorder, and the
// process table the proxy inspection reads.
var (
	reconcileProviderOwnedScopeIdentityFn = reconcileProviderOwnedScopeIdentity
	openProviderOwnedIdentityL3Fn         = openProviderOwnedIdentityL3
	openProviderOwnedIdentityRecorderFn   = func(cityPath string) (events.Recorder, func()) {
		return openProjectIdentityEventRecorder(cityPath, io.Discard)
	}
	providerOwnedIdentityProcessTableFn = proxyendpoint.DefaultProcessTable
)

// reconcileProviderOwnedScopeIdentity reconciles a provider-owned scope's
// project identity — L1 (.beads/identity.toml), L2 (metadata.json's
// project_id), L3 (the Dolt metadata table) — after the provider's own init
// has stamped L2. bd never reconciles L1 for a scope it owns, so gc does it
// here, on every lifecycle pass, so the layers cannot drift apart between
// init and the first command that needs a coherent identity.
//
// A scope with no SQL Dolt to reconcile against — embedded storage, a
// doltlite-backed city, a hosted credential-provider binding, a mode the
// reconciler does not know — is not an error: there is no L3, and the call
// returns nil without touching anything.
func reconcileProviderOwnedScopeIdentity(ctx context.Context, cityPath, dir string) error {
	metadataPath := scopeMetadataJSONPath(dir)
	endpoint, ok, err := resolveProviderOwnedIdentityEndpoint(cityPath, dir, metadataPath)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, providerOwnedIdentityTimeout)
	defer cancel()
	session, err := openProviderOwnedIdentityL3Fn(ctx, endpoint)
	if err != nil {
		return err
	}
	if session.close != nil {
		defer func() { _ = session.close() }() //nolint:errcheck // the session is spent; reconciliation's own error, if any, is the one to report
	}
	rec, closeRecorder := openProviderOwnedIdentityRecorderFn(cityPath)
	defer closeRecorder()
	_, err = reconcileScopeProjectIdentityOverL3(ctx, fsys.OSFS{}, dir, metadataPath, cityPath, session.read, session.seed, rec)
	return err
}

// resolveProviderOwnedIdentityEndpoint decides whether a provider-owned
// scope has an SQL Dolt endpoint whose _project_id gc should reconcile, and
// where it is. ok=false means the scope is ineligible and reconciliation is
// a no-op; err means the scope's own records could not be read.
func resolveProviderOwnedIdentityEndpoint(cityPath, dir, metadataPath string) (providerOwnedIdentityEndpoint, bool, error) {
	fs := fsys.OSFS{}
	backend, backendOK, err := contract.ReadMetadataBackend(fs, metadataPath)
	if err != nil {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("read provider-owned scope %q metadata backend: %w", dir, err)
	}
	if !backendOK || !contract.IsDoltBackend(backend) {
		return providerOwnedIdentityEndpoint{}, false, nil
	}
	// The same exclusions every other dolt-touching lifecycle helper applies:
	// a doltlite-backed city routes beads through its own store, an embedded
	// scope keeps its Dolt in-process behind no SQL endpoint, and a hosted
	// credential-provider binding lives outside this city's Dolt entirely.
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return providerOwnedIdentityEndpoint{}, false, nil
	}
	embedded, err := scopeMetadataRecordsEmbeddedDolt(fs, dir)
	if err != nil {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("inspect provider-owned scope %q dolt mode: %w", dir, err)
	}
	if embedded {
		return providerOwnedIdentityEndpoint{}, false, nil
	}
	hosted, err := citySelectsHostedBeadsCredentialProvider(cityPath)
	if err != nil {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("inspect city %q hosted beads binding: %w", cityPath, err)
	}
	if hosted {
		return providerOwnedIdentityEndpoint{}, false, nil
	}
	mode, modeOK, err := contract.ReadDoltMode(fs, metadataPath)
	if err != nil {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("read provider-owned scope %q dolt mode: %w", dir, err)
	}
	database, databaseOK, err := contract.ReadDoltDatabase(fs, metadataPath)
	if err != nil {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("read provider-owned scope %q dolt database: %w", dir, err)
	}
	database = strings.TrimSpace(database)
	// A mode the metadata does not record, a mode the reconciler does not
	// know, or no database named: none of these is a fault of the provider's
	// init, and none leaves an L3 to reconcile against.
	if !modeOK || database == "" || !databaseOK {
		return providerOwnedIdentityEndpoint{}, false, nil
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "proxied-server":
		return resolveProviderOwnedProxiedIdentityEndpoint(dir, database)
	case "server":
		return resolveProviderOwnedDirectIdentityEndpoint(cityPath, dir, database)
	default:
		return providerOwnedIdentityEndpoint{}, false, nil
	}
}

// resolveProviderOwnedProxiedIdentityEndpoint resolves the L3 endpoint for a
// proxied-server scope: bd's sidecar proxy on loopback, when a record claims
// one. No record at all is the proxy's ordinary stopped state — bd removes
// the record on an orderly exit — so the file layers reconcile now and L3
// completes on a later pass once the proxy is up; if the layers would
// disagree, that later pass refuses rather than repairs. Any other non-live
// verdict is a real fault: a record exists, but its proxy is dead or is not
// ours to use.
func resolveProviderOwnedProxiedIdentityEndpoint(dir, database string) (providerOwnedIdentityEndpoint, bool, error) {
	root, err := proxyendpoint.ProviderRoot(dir)
	if err != nil {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("resolve proxy root for provider-owned scope %q: %w", dir, err)
	}
	endpoint := proxyendpoint.Inspect(root, providerOwnedIdentityProcessTableFn())
	if endpoint.Verdict.Live() {
		return providerOwnedIdentityEndpoint{proxied: true, port: endpoint.Record.Port, database: database}, true, nil
	}
	if endpoint.Verdict == proxyendpoint.VerdictNoRecord || errors.Is(endpoint.Err, proxyendpoint.ErrNoProxy) {
		return providerOwnedIdentityEndpoint{proxied: true, port: 0, database: database}, true, nil
	}
	if endpoint.Err == nil {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("provider-owned proxy endpoint %s is not live (%s)", root, endpoint.Verdict)
	}
	return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("provider-owned proxy endpoint %s is not live (%s): %w", root, endpoint.Verdict, endpoint.Err)
}

// resolveProviderOwnedDirectIdentityEndpoint resolves the L3 endpoint for a
// server-mode scope: whatever SQL binding the scope records — a bd-owned
// local server or a persisted external one. The database comes from the
// scope's own metadata, not from the city-level default.
func resolveProviderOwnedDirectIdentityEndpoint(cityPath, dir, database string) (providerOwnedIdentityEndpoint, bool, error) {
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, dir)
	if err != nil {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("resolve provider-owned Dolt binding for %q: %w", dir, err)
	}
	target.Socket = strings.TrimSpace(target.Socket)
	target.Host = strings.TrimSpace(target.Host)
	target.Port = strings.TrimSpace(target.Port)
	if target.Socket == "" && target.Port == "" {
		return providerOwnedIdentityEndpoint{}, false, fmt.Errorf("provider-owned Dolt binding for %q has no SQL endpoint", dir)
	}
	target.Database = database
	return providerOwnedIdentityEndpoint{target: target, database: database}, true, nil
}

// openProviderOwnedIdentityL3 opens the L3 session an endpoint resolved to.
// A proxied endpoint with no record has nothing to dial: L3 reads as absent
// and seeding is a no-op for this pass. A proxied endpoint with a live
// record gets a private session that is closed before the pass returns — a
// pooled handle there would pin a connection against the proxy's idle
// watcher and defer its retirement. A direct endpoint rides the pooled
// handles, like every other gc Dolt client.
func openProviderOwnedIdentityL3(ctx context.Context, endpoint providerOwnedIdentityEndpoint) (providerOwnedIdentityL3Session, error) {
	if endpoint.proxied {
		if endpoint.port == 0 {
			return providerOwnedIdentityL3Session{
				read: func(context.Context) (string, bool, error) { return "", false, nil },
				seed: func(context.Context, string) (bool, error) { return false, nil },
			}, nil
		}
		conn, closeSession, err := proxyendpoint.OpenSession(ctx, endpoint.port, endpoint.database)
		if err != nil {
			return providerOwnedIdentityL3Session{}, fmt.Errorf("open proxy session for provider-owned scope identity: %w", err)
		}
		return providerOwnedIdentityL3Session{
			read: func(ctx context.Context) (string, bool, error) { return readDatabaseProjectID(ctx, conn) },
			seed: func(ctx context.Context, projectID string) (bool, error) {
				return seedDatabaseProjectID(ctx, conn, projectID)
			},
			close: closeSession,
		}, nil
	}
	// Pooled handle owned by internal/doltpool; do not Close.
	var db *sql.DB
	var err error
	if endpoint.target.Socket != "" {
		db, err = managedDoltOpenDatabaseSocket(endpoint.target.Socket, endpoint.target.User, endpoint.database)
	} else {
		db, err = managedDoltOpenDatabase(endpoint.target.Host, endpoint.target.Port, endpoint.target.User, endpoint.database)
	}
	if err != nil {
		return providerOwnedIdentityL3Session{}, fmt.Errorf("open Dolt handle for provider-owned scope identity: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return providerOwnedIdentityL3Session{}, err
	}
	return providerOwnedIdentityL3Session{
		read: func(ctx context.Context) (string, bool, error) { return readDatabaseProjectID(ctx, db) },
		seed: func(ctx context.Context, projectID string) (bool, error) {
			return seedDatabaseProjectID(ctx, db, projectID)
		},
	}, nil
}
