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
	transport, target = resolved.Intent.Transport, resolved.Intent.Target
	if target == "external" {
		if !o.enabled() {
			return fmt.Errorf("--beads-target external requires --dolt-host (or %s)", envDoltHost)
		}
		if err := o.validate(); err != nil {
			return err
		}
		if err := o.applyToCityConfig(cfg); err != nil {
			return err
		}
	} else {
		if o.enabled() {
			return fmt.Errorf("local beads target cannot be combined with --dolt-host or endpoint flags")
		}
		// Explicit local intent clears stale compatibility endpoint values on a
		// fresh config. Persisted initialized scopes are handled before this
		// function and never reach this mutation path.
		cfg.Dolt.Host = ""
		cfg.Dolt.Port = 0
	}
	if transport == "proxied" {
		cfg.Dolt.Mode = "proxied-server"
	} else {
		cfg.Dolt.Mode = "server"
	}
	return nil
}

func configDoltInitIntent(cfg config.City) contract.InitIntent {
	mode := strings.ToLower(strings.TrimSpace(cfg.Dolt.Mode))
	transport := ""
	switch mode {
	case "server":
		transport = "direct"
	case "proxied-server":
		transport = "proxied"
	}
	target := ""
	if strings.TrimSpace(cfg.Dolt.Host) != "" || cfg.Dolt.Port != 0 {
		target = "external"
	}
	if transport == "" && target == "" {
		return contract.InitIntent{}
	}
	if target == "" {
		target = "local"
	}
	return contract.InitIntent{Transport: transport, Target: target}
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
