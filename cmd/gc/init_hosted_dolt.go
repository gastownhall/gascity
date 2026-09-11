package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// selectorExternalInitOptions retains the one-shot external database identity
// only until bd has written its own metadata. Generic selectors deliberately
// do not make city.toml a second endpoint or database store.
var selectorExternalInitOptions sync.Map // canonical city path -> hostedDoltInitOptions

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
	envBeadsTransport = "GC_BEADS_TRANSPORT"
	envBeadsTarget    = "GC_BEADS_TARGET"
)

// hostedDoltInitFlagValues is the raw --dolt-* flag input captured by the
// init command, before environment fallback is applied.
type hostedDoltInitFlagValues struct {
	Host      string
	Port      string
	User      string
	Database  string
	ProjectID string
	Transport string
	Target    string
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
	Transport string
	Target    string
}

func (o hostedDoltInitOptions) validateSelectors() error {
	t, g := strings.ToLower(strings.TrimSpace(o.Transport)), strings.ToLower(strings.TrimSpace(o.Target))
	if t == "" && g == "" {
		return nil
	}
	if t == "" || g == "" {
		return fmt.Errorf("--beads-transport and --beads-target must be provided together")
	}
	if t != "direct" && t != "proxied" {
		return fmt.Errorf("unsupported --beads-transport %q", o.Transport)
	}
	if g != "local" && g != "external" {
		return fmt.Errorf("unsupported --beads-target %q", o.Target)
	}
	return nil
}

// applySelectorToCityConfig resolves the provider-neutral init axes onto the
// in-memory config used by the selected beads provider. The axes are an
// ephemeral front-door intent; the adapter persists whatever provider-owned
// marker is required for the resulting scope. Omitted axes leave the config
// untouched so the provider's own default remains authoritative.
func (o hostedDoltInitOptions) applySelectorToCityConfig(cfg *config.City) error {
	if cfg == nil {
		return fmt.Errorf("cannot apply beads selector to nil city config")
	}
	if err := o.validateSelectors(); err != nil {
		return err
	}
	transport := strings.ToLower(strings.TrimSpace(o.Transport))
	target := strings.ToLower(strings.TrimSpace(o.Target))
	if transport == "" && target == "" && !o.enabled() {
		return nil
	}
	// Compatibility: legacy --dolt-host is direct/external.
	if transport == "" && target == "" && o.enabled() {
		transport, target = "direct", "external"
	}
	resolved, err := contract.ResolveInitIntent(contract.InitScopeState{}, contract.InitIntent{Transport: transport, Target: target}, contract.InitIntent{}, configDoltInitIntent(*cfg), contract.InitIntent{Transport: "proxied", Target: "local"})
	if err != nil {
		return err
	}
	target = resolved.Intent.Target
	selectorRequested := strings.TrimSpace(o.Transport) != "" || strings.TrimSpace(o.Target) != ""
	if target == "external" {
		if !o.enabled() {
			return fmt.Errorf("--beads-target external requires --dolt-host (or %s)", envDoltHost)
		}
		if err := o.validate(); err != nil {
			return err
		}
		if !selectorRequested {
			if err := o.applyToCityConfig(cfg); err != nil {
				return err
			}
		}
	} else {
		if o.enabled() {
			return fmt.Errorf("local beads target cannot be combined with --dolt-host or endpoint flags")
		}
		if !selectorRequested {
			// Legacy endpoint flags retain their existing compatibility behavior.
			cfg.Dolt.Host = ""
			cfg.Dolt.Port = 0
		}
	}
	return nil
}

func (o hostedDoltInitOptions) selectorRequested() bool {
	return strings.TrimSpace(o.Transport) != "" || strings.TrimSpace(o.Target) != ""
}

// registerSelectorEndpointForInit keeps a generic external selector's
// endpoint in the current process only. bd receives it during pending init
// and persists its own binding; city.toml never becomes a second topology
// store. A later retry without that binding must supply the endpoint again.
func (o hostedDoltInitOptions) registerSelectorEndpointForInit(cityPath string) error {
	if !o.selectorRequested() || strings.ToLower(strings.TrimSpace(o.Target)) != "external" {
		return nil
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return fmt.Errorf("load city config for selector authority: %w", err)
	}
	if _, initialized, err := o.persistedSelectorAuthority(cityPath, *cfg); err != nil {
		return err
	} else if initialized {
		// The canonical Beads binding already supplies this endpoint. Keep the
		// one-shot selector out of process state so it cannot shadow it.
		return nil
	}
	if err := o.validate(); err != nil {
		return err
	}
	port, err := strconv.Atoi(strings.TrimSpace(o.Port))
	if err != nil {
		return fmt.Errorf("invalid selector endpoint port: %w", err)
	}
	registerCityDoltConfig(cityPath, config.DoltConfig{Host: strings.TrimSpace(o.Host), Port: port})
	selectorExternalInitOptions.Store(normalizePathForCompare(cityPath), o)
	return nil
}

// selectorExternalInitDatabase resolves the external database a pending city
// init must create. The in-process selector serves the first attempt; the
// pending journal record serves every later retry, which would otherwise have
// nothing to pass to bd.
func selectorExternalInitDatabase(cityPath, scopeRoot string) string {
	if !samePath(cityPath, scopeRoot) {
		return ""
	}
	if value, ok := selectorExternalInitOptions.Load(normalizePathForCompare(cityPath)); ok {
		if opts, ok := value.(hostedDoltInitOptions); ok {
			if database := strings.TrimSpace(opts.Database); database != "" {
				return database
			}
		}
	}
	return pendingProviderScopeEndpoint(cityPath, scopeRoot).Database
}

func hasSelectorExternalInitOptions(cityPath string) bool {
	_, ok := selectorExternalInitOptions.Load(normalizePathForCompare(cityPath))
	return ok
}

func clearSelectorExternalInitOptions(cityPath, scopeRoot string) {
	if samePath(cityPath, scopeRoot) {
		selectorExternalInitOptions.Delete(normalizePathForCompare(cityPath))
	}
}

// persistedSelectorAuthority validates a generic selector against an already
// initialized city binding. The selector is one-shot input for a fresh scope;
// it must not override or be silently ignored by an existing provider-owned
// (or legacy) binding.
func (o hostedDoltInitOptions) persistedSelectorAuthority(cityPath string, cfg config.City) (providerScopeIntent, bool, error) {
	if !o.selectorRequested() {
		return providerScopeIntent{}, false, nil
	}
	if entry, owned, err := providerScopeOwnership(cityPath, cityPath); err != nil {
		return providerScopeIntent{}, false, err
	} else if owned && entry.State == providerScopeInitializing {
		requested, err := o.providerOwnershipIntent(cfg)
		if err != nil {
			return providerScopeIntent{}, false, err
		}
		if entry.Intent != requested {
			return providerScopeIntent{}, false, fmt.Errorf("conflicting provider initialization intent for scope %q", cityPath)
		}
		// A durable pending record predates any partial provider artifacts. It
		// remains the authority until bd commits metadata, so a matching retry
		// can re-register its one-shot external endpoint.
		return entry.Intent, false, nil
	}
	initialized, err := scopeHasPersistedBeadsIdentity(cityPath)
	if err != nil {
		return providerScopeIntent{}, false, err
	}
	if !initialized {
		return providerScopeIntent{}, false, nil
	}
	metadata, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(cityPath))
	if err != nil {
		return providerScopeIntent{}, false, fmt.Errorf("load initialized beads metadata for selector: %w", err)
	}
	if !ok {
		return providerScopeIntent{}, false, fmt.Errorf("cannot apply beads selector to initialized scope without canonical beads metadata")
	}
	if !contract.IsDoltBackend(strings.TrimSpace(metadata.Backend)) || strings.EqualFold(strings.TrimSpace(metadata.DoltMode), "embedded") {
		return providerScopeIntent{}, false, fmt.Errorf("cannot apply beads selector to initialized backend %q", metadata.Backend)
	}
	persisted, err := providerOwnershipIntentFromPersistedCity(cityPath)
	if err != nil {
		return providerScopeIntent{}, false, err
	}
	requested, err := o.providerOwnershipIntent(cfg)
	if err != nil {
		return providerScopeIntent{}, false, err
	}
	if persisted != requested {
		return providerScopeIntent{}, false, fmt.Errorf("conflicting provider initialization intent for scope %q", cityPath)
	}
	return persisted, true, nil
}

// providerOwnershipIntent resolves the one-shot selector for a fresh scope.
// The selector is never written to city.toml: the provider receives it during
// initialization and owns the resulting backend metadata.
func (o hostedDoltInitOptions) providerOwnershipIntent(city config.City) (providerScopeIntent, error) {
	if err := o.validateSelectors(); err != nil {
		return providerScopeIntent{}, err
	}
	transport := strings.ToLower(strings.TrimSpace(o.Transport))
	target := strings.ToLower(strings.TrimSpace(o.Target))
	if transport == "" && target == "" && o.enabled() {
		transport, target = "direct", "external"
	}
	resolved, err := contract.ResolveInitIntent(contract.InitScopeState{}, contract.InitIntent{Transport: transport, Target: target}, contract.InitIntent{}, configDoltInitIntent(city), contract.InitIntent{Transport: "proxied", Target: "local"})
	if err != nil {
		return providerScopeIntent{}, err
	}
	return normalizeProviderScopeIntent(providerScopeIntent{Transport: resolved.Intent.Transport, Target: resolved.Intent.Target})
}

// providerScopeEndpoint projects the one-shot external endpoint this init
// supplied into the durable form the ownership journal keeps. Without it the
// endpoint lived only in this process, so a `gc init` interrupted after the
// pending record was written left every retry unable to reach its upstream.
func (o hostedDoltInitOptions) providerScopeEndpoint(intent providerScopeIntent) providerScopeEndpoint {
	if intent.Target != "external" {
		return providerScopeEndpoint{}
	}
	return providerScopeEndpoint{
		Host:     strings.TrimSpace(o.Host),
		Port:     strings.TrimSpace(o.Port),
		Database: strings.TrimSpace(o.Database),
	}
}

func persistFreshProviderOwnership(cityPath string, opts hostedDoltInitOptions) error {
	// Legacy --dolt-host initialization retains its established canonical
	// endpoint path. Generic selectors are the new provider-owned contract.
	if opts.enabled() && !opts.selectorRequested() {
		return nil
	}
	if err := selectorBackendError(cityPath, opts); err != nil {
		return err
	}
	if !cityUsesManagedDoltBeadsLifecycle(cityPath) {
		return nil
	}
	// Initialization may precede installation of remote imports. Use the same
	// narrow raw-config fallback as provider preflight so ownership is durable
	// before that recoverable import boundary.
	cfg, err := loadInitProviderPreflightConfig(cityPath)
	if err != nil {
		// The filesystem-backed scaffold tests intentionally stop before the
		// real lifecycle boundary. They must not cause an OS ownership write
		// for their synthetic city path.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load configured rig scopes for provider ownership: %w", err)
	}
	intent, err := opts.providerOwnershipIntent(*cfg)
	if err != nil {
		return err
	}
	if persisted, initialized, err := opts.persistedSelectorAuthority(cityPath, *cfg); err != nil {
		return err
	} else if initialized {
		intent = persisted
	}
	if existing, owned, ownershipErr := providerScopeOwnership(cityPath, cityPath); ownershipErr != nil {
		return ownershipErr
	} else if owned && existing.State == providerScopeInitializing {
		if !opts.selectorRequested() && !opts.enabled() {
			// A resume path has no one-shot selector input. Its durable pending
			// record is the only topology authority until bd writes metadata.
			intent = existing.Intent
		} else if existing.Intent != intent {
			return fmt.Errorf("conflicting provider initialization intent for scope %q", cityPath)
		}
	}
	cityInitialized, err := scopeHasPersistedBeadsIdentity(cityPath)
	if err != nil {
		return err
	}
	if !cityInitialized {
		if err := persistProviderScopeOwnershipWithEndpoint(cityPath, cityPath, intent, opts.providerScopeEndpoint(intent)); err != nil {
			return err
		}
	}
	// A rig only becomes provider-owned when the city already is. Re-running
	// init over a grandfathered GC-managed city must leave its rigs on the
	// legacy inherited-city path; converting an existing city is `bd migrate`'s
	// job, not a side effect of `gc init` (D6).
	if inherits, err := cityGrantsProviderOwnershipToFreshScopes(cityPath, cityInitialized); err != nil {
		return err
	} else if !inherits {
		return nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		initialized, err := scopeHasPersistedBeadsIdentity(rig.Path)
		if err != nil {
			return fmt.Errorf("inspect rig %q beads identity: %w", rig.Name, err)
		}
		if initialized {
			continue
		}
		if err := persistProviderScopeOwnership(cityPath, rig.Path, intent); err != nil {
			return fmt.Errorf("record provider ownership for rig %q: %w", rig.Name, err)
		}
	}
	return nil
}

func scopeHasPersistedBeadsIdentity(scopeRoot string) (bool, error) {
	for _, name := range []string{"metadata.json", "config.yaml"} {
		if _, err := os.Stat(filepath.Join(scopeRoot, ".beads", name)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect %s: %w", filepath.Join(scopeRoot, ".beads", name), err)
		}
	}
	return false, nil
}

func configDoltInitIntent(cfg config.City) contract.InitIntent {
	if strings.TrimSpace(cfg.Dolt.Host) != "" || cfg.Dolt.Port != 0 {
		// Existing city endpoints predate provider-owned metadata. Keep their
		// established direct external interpretation; transport for initialized
		// provider scopes is read from Beads' canonical binding instead.
		return contract.InitIntent{Transport: "direct", Target: "external"}
	}
	return contract.InitIntent{}
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
		Transport: pick(flags.Transport, envBeadsTransport),
		Target:    pick(flags.Target, envBeadsTarget),
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
	if err := o.validateSelectors(); err != nil {
		return err
	}
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
	// The database names WHICH database on a server somebody else operates.
	// Omitting it would let bd derive one from the issue prefix and attach to,
	// or create, the wrong database on a shared server, so it stays required on
	// both paths.
	if strings.TrimSpace(o.Database) == "" {
		return fmt.Errorf("--dolt-database (or %s) is required for an external beads target", envDoltDatabase)
	}
	if isReservedManagedDoltDatabase(o.Database) {
		return fmt.Errorf("invalid --dolt-database %q: reserved internally by managed Dolt; choose the provisioner-created project database", o.Database)
	}
	// The project id is consumed only on the legacy --dolt-host path, by
	// contract.WriteProjectIdentity. A selector-driven init skips that write
	// (cmd_init.go gates it on !selectorRequested), journals host/port/database
	// only, and hands bd just --database; bd resolves project_id itself,
	// adopting the hosted database's _project_id or minting one. Requiring it
	// there made the new front door refuse its own documented invocation, and
	// honoring it is not something gc can promise.
	if strings.TrimSpace(o.ProjectID) == "" && !o.selectorRequested() {
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
	// A hosted city's controller runs out-of-session; the control dispatcher and
	// gc CLI reach it only through the HTTP API, and every API consumer treats
	// cfg.API.Port == 0 as "API disabled". Neither plain init nor the hosted
	// endpoint flags write an [api] section (only the k8s-cell bootstrap profile
	// does), so default the API port here — otherwise a hosted init yields a city
	// whose control plane is unreachable until an [api] section is hand-added.
	// applyBootstrapProfile runs first, so a profile that already pinned a
	// port/bind (e.g. k8s-cell's 0.0.0.0 + allow_mutations) wins.
	if cfg.API.Port == 0 {
		cfg.API.Port = config.DefaultAPIPort
	}
	return nil
}

// configState builds the canonical .beads/config.yaml endpoint state for the
// hosted endpoint: an external city-canonical endpoint recorded as
// unverified. gc start performs the live verification once credentials are
// wired.
func (o hostedDoltInitOptions) configState(issuePrefix string) contract.ConfigState {
	mode := "server"
	if strings.EqualFold(strings.TrimSpace(o.Transport), "proxied") {
		mode = "proxied-server"
	}
	return contract.ConfigState{
		IssuePrefix:    issuePrefix,
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       strings.TrimSpace(o.Host),
		DoltPort:       strings.TrimSpace(o.Port),
		DoltUser:       strings.TrimSpace(o.User),
		DoltMode:       mode,
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
// selectorBackendError applies the generic transport/target contract before
// the selected provider can create a ledger. These selectors belong to bd's
// server/proxy lifecycle; file and DoltLite providers have no compatible
// topology to honor.
func selectorBackendError(cityPath string, opts hostedDoltInitOptions) error {
	if !opts.selectorRequested() {
		return nil
	}
	if !cityUsesBdStoreContract(cityPath) {
		return fmt.Errorf("--beads-transport and --beads-target require a bd-backed beads provider")
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return fmt.Errorf("--beads-transport and --beads-target are incompatible with the doltlite beads backend")
	}
	return nil
}

// selectorBackendErrorForFileConfig applies the generic selector contract to
// a --file source's effective config. That source controls the file bootstrap,
// so ambient GC_BEADS for another city must not admit a selector that the
// copied file provider cannot honor.
func selectorBackendErrorForFileConfig(cfg *config.City, opts hostedDoltInitOptions) error {
	if !opts.selectorRequested() {
		return nil
	}
	if cfg == nil {
		return fmt.Errorf("cannot validate beads selector without file config")
	}
	provider := strings.TrimSpace(cfg.Beads.Provider)
	if provider == "" {
		provider = "bd"
	}
	if !providerUsesBdStoreContract(provider) {
		return fmt.Errorf("--beads-transport and --beads-target require a bd-backed beads provider")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Beads.Backend), "doltlite") {
		return fmt.Errorf("--beads-transport and --beads-target are incompatible with the doltlite beads backend")
	}
	return nil
}

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
