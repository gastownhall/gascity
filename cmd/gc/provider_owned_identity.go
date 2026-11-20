package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
)

// providerOwnedIdentityBudget bounds one provider-owned scope's identity
// reconcile, from endpoint open to the last layer write. It runs after the
// provider op has returned, so it is outside that op's timeout, and it matches
// the managed reconciler's budget.
const providerOwnedIdentityBudget = 5 * time.Second

// providerOwnedIdentityBinding is the part of bd's persisted binding
// (.beads/metadata.json) that decides whether and where a provider-owned
// scope's identity is reconciled. It is read from disk rather than from the
// ownership journal, whose intent is cleared once the scope is ready.
type providerOwnedIdentityBinding struct {
	Backend      string `json:"backend"`
	DoltMode     string `json:"dolt_mode"`
	DoltDatabase string `json:"dolt_database"`
}

// proxied reports whether bd reaches this scope's store through its own
// local proxy.
func (b providerOwnedIdentityBinding) proxied() bool {
	return contract.IsProxiedDoltMode(b.Backend, b.DoltMode)
}

// providerOwnedIdentityEndpoint is the one SQL endpoint a provider-owned
// scope's L3 identity is read and seeded through.
type providerOwnedIdentityEndpoint struct {
	// ProxyPort is bd's live local proxy port for a proxied scope, and zero
	// for a direct one.
	ProxyPort int
	// Target is the direct endpoint for a server-mode scope.
	Target contract.DoltConnectionTarget
	// Database is the scope's metadata dolt_database.
	Database string
}

// readProviderOwnedIdentityBinding reads dir's persisted binding and reports
// whether its identity is reconciled here: a Dolt server or proxied-server
// binding with a recorded database, in a city that is neither DoltLite-backed
// nor reaching its store through a hosted credential provider. Every other
// scope (no metadata, embedded, another backend) has no Dolt identity layer
// for gc to reach.
func readProviderOwnedIdentityBinding(cityPath, dir string) (providerOwnedIdentityBinding, bool, error) {
	path := scopeMetadataJSONPath(dir)
	data, err := os.ReadFile(path)
	if scopeArtifactAbsent(err) {
		return providerOwnedIdentityBinding{}, false, nil
	}
	if err != nil {
		return providerOwnedIdentityBinding{}, false, fmt.Errorf("read beads metadata for scope %q: %w", dir, err)
	}
	var binding providerOwnedIdentityBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return providerOwnedIdentityBinding{}, false, fmt.Errorf("parse beads metadata for scope %q: %w", dir, err)
	}
	binding.Backend = strings.ToLower(strings.TrimSpace(binding.Backend))
	binding.DoltMode = strings.ToLower(strings.TrimSpace(binding.DoltMode))
	binding.DoltDatabase = strings.TrimSpace(binding.DoltDatabase)
	if binding.Backend != "" && binding.Backend != "dolt" {
		return binding, false, nil
	}
	if binding.DoltMode != "server" && binding.DoltMode != "proxied-server" {
		return binding, false, nil
	}
	if binding.DoltDatabase == "" || cityUsesDoltliteBeadsBackend(cityPath) {
		return binding, false, nil
	}
	hosted, err := citySelectsHostedBeadsCredentialProvider(cityPath)
	if err != nil {
		return binding, false, fmt.Errorf("provider-owned scope %q identity eligibility: %w", dir, err)
	}
	return binding, !hosted, nil
}

// resolveProviderOwnedIdentityEndpoint resolves the endpoint dir's L3
// identity is reached through. A proxied scope always goes through bd's live
// local proxy, which authenticates to any upstream itself, so the sidecar's
// upstream is never dialed. A direct scope takes the target the connection
// contract resolves: bd's own live server, bd's persisted external binding,
// or an explicit or city-canonical endpoint.
func resolveProviderOwnedIdentityEndpoint(cityPath, dir string, binding providerOwnedIdentityBinding, processTable proxyendpoint.ProcessTable) (providerOwnedIdentityEndpoint, error) {
	if binding.proxied() {
		root, err := proxyendpoint.ProviderRoot(dir)
		if err != nil {
			return providerOwnedIdentityEndpoint{}, fmt.Errorf("resolve bd proxy root for scope %q: %w", dir, err)
		}
		endpoint := proxyendpoint.Inspect(root, processTable)
		if !endpoint.Verdict.Live() {
			if endpoint.Err != nil {
				return providerOwnedIdentityEndpoint{}, fmt.Errorf("bd proxy for scope %q at %s is not live (%s): %w", dir, root, endpoint.Verdict, endpoint.Err)
			}
			return providerOwnedIdentityEndpoint{}, fmt.Errorf("bd proxy for scope %q at %s is not live (%s)", dir, root, endpoint.Verdict)
		}
		return providerOwnedIdentityEndpoint{ProxyPort: endpoint.Record.Port, Database: binding.DoltDatabase}, nil
	}
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, dir)
	if err != nil {
		return providerOwnedIdentityEndpoint{}, fmt.Errorf("resolve Dolt endpoint for scope %q: %w", dir, err)
	}
	if strings.TrimSpace(target.Socket) == "" && strings.TrimSpace(target.Port) == "" {
		return providerOwnedIdentityEndpoint{}, fmt.Errorf("resolve Dolt endpoint for scope %q: binding names no SQL endpoint", dir)
	}
	target.Database = binding.DoltDatabase
	return providerOwnedIdentityEndpoint{Target: target, Database: binding.DoltDatabase}, nil
}

// openProviderOwnedScopeL3Fn opens the L3 identity store at a resolved
// endpoint. Tests replace it to stand in for Dolt.
var openProviderOwnedScopeL3Fn = openProviderOwnedScopeL3

// openProviderOwnedScopeL3 opens endpoint's L3 identity store and returns it
// with its close func. A proxied endpoint gets one private session to bd's
// proxy, closed by the close func. A direct endpoint gets the pooled handle
// the managed reconciler uses, which internal/doltpool owns.
func openProviderOwnedScopeL3(ctx context.Context, endpoint providerOwnedIdentityEndpoint) (projectIdentityL3Store, func() error, error) {
	if endpoint.ProxyPort != 0 {
		conn, closeSession, err := proxyendpoint.OpenSession(ctx, endpoint.ProxyPort, endpoint.Database)
		if err != nil {
			return nil, nil, fmt.Errorf("open bd proxy session %s:%d/%s: %w", proxyendpoint.Host, endpoint.ProxyPort, endpoint.Database, err)
		}
		return sqlProjectIdentityL3{db: conn}, closeSession, nil
	}
	target := endpoint.Target
	var (
		db  *sql.DB
		err error
	)
	if strings.TrimSpace(target.Socket) != "" {
		db, err = managedDoltOpenDatabaseSocket(target.Socket, target.User, endpoint.Database)
	} else {
		db, err = managedDoltOpenDatabase(target.Host, target.Port, target.User, endpoint.Database)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open Dolt endpoint %s: %w", describeDoltConnectionTarget(target), err)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, nil, fmt.Errorf("ping Dolt endpoint %s: %w", describeDoltConnectionTarget(target), err)
	}
	// Pooled handle owned by internal/doltpool; do not Close.
	return sqlProjectIdentityL3{db: db}, func() error { return nil }, nil
}

// describeDoltConnectionTarget names a direct target's socket or host:port
// for an error message.
func describeDoltConnectionTarget(target contract.DoltConnectionTarget) string {
	if socket := strings.TrimSpace(target.Socket); socket != "" {
		return "socket " + socket
	}
	return strings.TrimSpace(target.Host) + ":" + strings.TrimSpace(target.Port)
}

// providerOwnedIdentityProcessTable is the process table a proxied scope's
// proxy record is checked against. Tests replace it to stand in for a live
// proxy.
var providerOwnedIdentityProcessTable = proxyendpoint.DefaultProcessTable

// reconcileProviderOwnedScopeIdentityFn is the provider-owned init branch's
// identity step. Tests replace it to observe or fail the step.
var reconcileProviderOwnedScopeIdentityFn = reconcileProviderOwnedScopeIdentity

// reconcileProviderOwnedScopeIdentity reconciles a provider-owned scope's
// three project-identity layers (L1 .beads/identity.toml, L2 metadata
// project_id, L3 Dolt _project_id) through the endpoint its persisted binding
// resolves to, under providerOwnedIdentityBudget. An ineligible scope is left
// alone. Layers already written stay written when a later step fails.
func reconcileProviderOwnedScopeIdentity(ctx context.Context, cityPath, dir string) error {
	binding, eligible, err := readProviderOwnedIdentityBinding(cityPath, dir)
	if err != nil || !eligible {
		return err
	}
	endpoint, err := resolveProviderOwnedIdentityEndpoint(cityPath, dir, binding, providerOwnedIdentityProcessTable())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, providerOwnedIdentityBudget)
	defer cancel()
	l3, closeL3, err := openProviderOwnedScopeL3Fn(ctx, endpoint)
	if err != nil {
		return err
	}
	rec, closeRecorder := openProjectIdentityEventRecorder(cityPath, io.Discard)
	defer closeRecorder()
	_, err = reconcileScopeProjectIdentity(ctx, fsys.OSFS{}, dir, scopeMetadataJSONPath(dir), cityPath, l3, rec)
	if closeErr := closeL3(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close L3 identity session for scope %q: %w", dir, closeErr))
	}
	return err
}
