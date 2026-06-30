package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Environment fallbacks for the hosted-dolt init flags. These mirror the
// variables the create-city controller already exports, so a controller
// entrypoint can supply the external Dolt endpoint through the environment as
// "set env -> gc init -> gc start" without passing the --dolt-* flags
// explicitly. The env vars only fill the --dolt-* endpoint inputs; the
// controller still selects the city template and provider, because a
// non-interactive bd-backed `gc init` requires --template/--default-provider.
const (
	envDoltHost       = "GC_DOLT_HOST"
	envDoltPort       = "GC_DOLT_PORT"
	envDoltUser       = "GC_DOLT_USER"
	envDoltDatabase   = "GC_DOLT_DATABASE"
	envBeadsProjectID = "GC_BEADS_PROJECT_ID"
)

// hostedDoltInitFlagValues is the raw --dolt-* flag input captured by the
// init command, before environment fallback is applied.
type hostedDoltInitFlagValues struct {
	Host      string
	Port      string
	User      string
	Database  string
	ProjectID string
}

// hostedDoltInitOptions is the resolved external/hosted Dolt endpoint that
// `gc init` pins for a city's beads ledger. When enabled, init writes the
// canonical external endpoint config (gc.endpoint_origin=city_canonical,
// gc.endpoint_status=unverified) plus the project identity, and the existing
// lifecycle machinery skips the managed-local Dolt bootstrap.
type hostedDoltInitOptions struct {
	Host      string
	Port      string
	User      string
	Database  string
	ProjectID string
}

// resolveHostedDoltInitOptions merges explicit flag values with environment
// fallbacks — flags win, env fills the gaps. When no project id is supplied
// it is derived from a "bd_"-prefixed database name (the create-city
// provisioner builds dolt_database as "bd_"+project_id, so the suffix is the
// authoritative id by construction). getenv is injected for testability;
// production callers pass os.Getenv.
func resolveHostedDoltInitOptions(flags hostedDoltInitFlagValues, getenv func(string) string) hostedDoltInitOptions {
	pick := func(flag, env string) string {
		if v := strings.TrimSpace(flag); v != "" {
			return v
		}
		return strings.TrimSpace(getenv(env))
	}
	opts := hostedDoltInitOptions{
		Host:      pick(flags.Host, envDoltHost),
		Port:      pick(flags.Port, envDoltPort),
		User:      pick(flags.User, envDoltUser),
		Database:  pick(flags.Database, envDoltDatabase),
		ProjectID: pick(flags.ProjectID, envBeadsProjectID),
	}
	if opts.ProjectID == "" {
		opts.ProjectID = deriveProjectIDFromDoltDatabase(opts.Database)
	}
	return opts
}

// deriveProjectIDFromDoltDatabase returns the beads project id encoded in a
// "bd_"-prefixed managed database name, or "" when the name is not in that
// form.
func deriveProjectIDFromDoltDatabase(database string) string {
	database = strings.TrimSpace(database)
	if rest, ok := strings.CutPrefix(database, "bd_"); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

// enabled reports whether a hosted/external Dolt endpoint was requested.
func (o hostedDoltInitOptions) enabled() bool {
	return strings.TrimSpace(o.Host) != ""
}

// validate enforces the hosted-dolt init contract. It performs no live
// connection (R5): a hosted endpoint is recorded as unverified and verified
// later by gc start, so init never requires credentials.
func (o hostedDoltInitOptions) validate() error {
	if !o.enabled() {
		if strings.TrimSpace(o.Port) != "" || strings.TrimSpace(o.User) != "" ||
			strings.TrimSpace(o.Database) != "" || strings.TrimSpace(o.ProjectID) != "" {
			return fmt.Errorf("--dolt-host (or %s) is required when any other --dolt-* flag is set", envDoltHost)
		}
		return nil
	}
	if err := validateExplicitExternalHost(o.Host); err != nil {
		return err
	}
	port := strings.TrimSpace(o.Port)
	if port == "" {
		return fmt.Errorf("--dolt-port (or %s) is required with --dolt-host", envDoltPort)
	}
	if value, err := strconv.Atoi(port); err != nil || value <= 0 {
		return fmt.Errorf("invalid --dolt-port %q", port)
	}
	if strings.TrimSpace(o.Database) == "" {
		return fmt.Errorf("--dolt-database (or %s) is required with --dolt-host", envDoltDatabase)
	}
	if isReservedManagedDoltDatabase(o.Database) {
		return fmt.Errorf("invalid --dolt-database %q: reserved internally by managed Dolt; choose the provisioner-created project database", o.Database)
	}
	if strings.TrimSpace(o.ProjectID) == "" {
		return fmt.Errorf("--dolt-project-id (or %s) is required with --dolt-host: the beads project_id is needed for the identity handshake (or pass a bd_<id> --dolt-database to derive it)", envBeadsProjectID)
	}
	return nil
}

// applyToCityConfig pins the external Dolt host/port into the in-memory city
// config so doInit serializes a [dolt] section into city.toml. This is what
// makes the lifecycle ownership probe (resolveConfiguredCityDoltTarget) and
// the runtime resolve the city as external rather than managed-local.
func (o hostedDoltInitOptions) applyToCityConfig(cfg *config.City) error {
	port, err := strconv.Atoi(strings.TrimSpace(o.Port))
	if err != nil {
		return fmt.Errorf("invalid --dolt-port %q: %w", o.Port, err)
	}
	cfg.Dolt.Host = strings.TrimSpace(o.Host)
	cfg.Dolt.Port = port
	return nil
}

// configState builds the canonical .beads/config.yaml endpoint state for the
// hosted endpoint: an external city-canonical endpoint recorded as
// unverified. gc start performs the live verification once credentials are
// wired.
func (o hostedDoltInitOptions) configState(issuePrefix string) contract.ConfigState {
	return contract.ConfigState{
		IssuePrefix:    issuePrefix,
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       strings.TrimSpace(o.Host),
		DoltPort:       strings.TrimSpace(o.Port),
		DoltUser:       strings.TrimSpace(o.User),
	}
}

// cityExternalDoltEndpointUnverified reports whether the city's canonical
// endpoint config pins an external (city_canonical) Dolt endpoint that has not
// yet been verified. init-time bd init against such an endpoint must be
// deferred to gc start, which carries the credential command — init itself
// never requires a live connection (R5).
func cityExternalDoltEndpointUnverified(cityPath string) bool {
	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil || !ok {
		return false
	}
	return state.EndpointOrigin == contract.EndpointOriginCityCanonical &&
		state.EndpointStatus == contract.EndpointStatusUnverified
}

// hostedDoltBackendError reports why a city's effective beads backend cannot
// host the external Dolt *server* endpoint pinned by --dolt-host, or nil when
// the backend is compatible. The effective provider and backend are resolved
// from the same env/city.toml inputs the runtime uses, so this init-time guard
// agrees with how the city will actually resolve its ledger:
//
//   - a non-bd (file) store cannot carry the bd Dolt-server contract; and
//   - the doltlite backend is a local embedded store, not an external server,
//     so pinning --dolt-host would write backend=dolt server metadata that
//     permanently disagrees with the configured doltlite backend (split-brain)
//     and skip the external-endpoint init defer.
//
// Both incompatibilities must be rejected before any canonical hosted-Dolt
// files are written so a rejected init leaves no mixed ledger state behind.
func hostedDoltBackendError(cityPath string) error {
	if !cityUsesBdStoreContract(cityPath) {
		return fmt.Errorf("--dolt-host requires a bd-backed beads provider (use the gascity or gastown template)")
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return fmt.Errorf("--dolt-host configures an external Dolt server and is incompatible with the doltlite beads backend; unset the doltlite backend (GC_BEADS_BACKEND or [beads] backend) to use the dolt (server) backend")
	}
	return nil
}

// applyInitHostedDoltCanonicalConfig writes the full canonical external
// endpoint config for a freshly scaffolded city (R3/R4/R5), identical in
// shape to what `gc beads city use-external --adopt-unverified` produces plus
// the pinned dolt_database and project identity:
//
//   - the L1 project identity (contract.ProjectIdentityPath) — the
//     authoritative project_id, written via contract.WriteProjectIdentity
//   - .beads/config.yaml     — city_canonical + unverified + dolt host/port/user
//   - .beads/metadata.json   — backend=dolt, dolt_mode=server, dolt_database,
//     and project_id (stamped from the L1 identity)
//
// It writes the identity first so the canonical metadata write picks up
// project_id. No live connection is attempted.
func applyInitHostedDoltCanonicalConfig(fs fsys.FS, cityPath, issuePrefix string, opts hostedDoltInitOptions) error {
	if !opts.enabled() {
		return nil
	}
	if err := opts.validate(); err != nil {
		return err
	}
	if err := contract.WriteProjectIdentity(fs, cityPath, strings.TrimSpace(opts.ProjectID)); err != nil {
		return fmt.Errorf("writing project identity: %w", err)
	}
	if err := ensureCanonicalScopeConfigState(fs, cityPath, opts.configState(issuePrefix)); err != nil {
		return fmt.Errorf("writing canonical endpoint config: %w", err)
	}
	if err := enforceCanonicalScopeMetadataForInit(fs, cityPath, strings.TrimSpace(opts.Database)); err != nil {
		return fmt.Errorf("writing canonical metadata: %w", err)
	}
	return nil
}
