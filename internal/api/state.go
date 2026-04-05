// Package api implements the GC HTTP API server.
//
// The server embeds in the controller process and serves typed JSON
// endpoints over REST, replacing subprocess-based data access. It
// activates via [api] port = N in city.toml (progressive activation).
package api

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// State provides read access to controller-managed state.
// The controller implements this with RWMutex-protected hot-reload.
type State interface {
	// Config returns the current city config snapshot.
	Config() *config.City

	// SessionProvider returns the current session provider.
	SessionProvider() runtime.Provider

	// BeadStore returns the bead store for a rig (by name).
	// Returns nil if the rig doesn't exist.
	BeadStore(rig string) beads.Store

	// BeadStores returns all rig names and their stores.
	BeadStores() map[string]beads.Store

	// MailProvider returns the mail provider for a rig.
	// Returns nil if the rig doesn't exist.
	MailProvider(rig string) mail.Provider

	// MailProviders returns all rig names and their mail providers.
	MailProviders() map[string]mail.Provider

	// EventProvider returns the event provider, or nil if events are disabled.
	EventProvider() events.Provider

	// CityName returns the city name.
	CityName() string

	// CityPath returns the city root directory.
	CityPath() string

	// Version returns the GC binary version string.
	Version() string

	// StartedAt returns when the controller was started.
	StartedAt() time.Time

	// IsQuarantined reports whether an agent (by session name) is
	// currently quarantined due to crash-loop detection.
	IsQuarantined(sessionName string) bool

	// ClearCrashHistory removes in-memory crash tracking for a session.
	// Called by wake to prevent the in-memory tracker from immediately
	// re-quarantining a session whose dolt metadata was just cleared.
	ClearCrashHistory(sessionName string)

	// CityBeadStore returns the city-level bead store for session beads.
	// Returns nil if no store is available.
	CityBeadStore() beads.Store

	// Orders returns the current set of scanned orders.
	// Returns nil if orders are not configured.
	Orders() []orders.Order

	// Poke signals the controller to trigger an immediate reconciler tick.
	// Used after sling assigns work so WakeWork wakes the target without
	// waiting for the next patrol interval. Best-effort: no-op if poke
	// is not available (e.g., in tests).
	Poke()

	// ServiceRegistry returns the workspace service registry, or nil when
	// workspace services are not enabled for this city.
	ServiceRegistry() workspacesvc.Registry

	// ExtMsgServices returns the external messaging services, or nil when
	// external messaging is not enabled.
	ExtMsgServices() *extmsg.Services

	// AdapterRegistry returns the external messaging adapter registry, or
	// nil when external messaging is not enabled.
	AdapterRegistry() *extmsg.AdapterRegistry
}

// AgentUpdate holds optional fields for a partial agent update. Pointer fields
// distinguish "not set" from "set to zero value."
type AgentUpdate struct {
	Provider  string
	Scope     string
	Suspended *bool
}

// RigUpdate holds optional fields for a partial rig update. Pointer fields
// distinguish "not set" from "set to zero value."
type RigUpdate struct {
	Path      string
	Prefix    string
	Suspended *bool
}

// ProviderUpdate holds optional fields for a partial provider update.
// Pointer fields distinguish "not set" from "set to zero value."
type ProviderUpdate struct {
	DisplayName  *string
	Command      *string
	Args         []string // nil = not set, non-nil = replace
	PromptMode   *string
	PromptFlag   *string
	ReadyDelayMs *int
	Env          map[string]string // nil = not set, non-nil = additive merge
}

// RawConfigProvider is optionally implemented by State to provide the
// raw (pre-expansion) config for provenance detection. Used by the
// /v0/config/explain endpoint to distinguish inline vs pack-derived agents.
type RawConfigProvider interface {
	RawConfig() *config.City
}

// StateMutator extends State with write operations for mutation endpoints.
type StateMutator interface {
	State

	// --- Desired-state mutations (write to city.toml) ---

	// SuspendAgent marks an agent as suspended in the config.
	SuspendAgent(name string) error

	// ResumeAgent marks an agent as no longer suspended.
	ResumeAgent(name string) error

	// SuspendRig suspends a rig in the config.
	SuspendRig(name string) error

	// ResumeRig resumes a rig in the config.
	ResumeRig(name string) error

	// SuspendCity sets workspace.suspended = true.
	SuspendCity() error

	// ResumeCity sets workspace.suspended = false.
	ResumeCity() error

	// CreateAgent adds a new agent to city.toml.
	CreateAgent(a config.Agent) error

	// UpdateAgent partially updates an existing agent definition in city.toml.
	UpdateAgent(name string, patch AgentUpdate) error

	// DeleteAgent removes an agent from city.toml.
	DeleteAgent(name string) error

	// CreateRig adds a new rig to city.toml.
	CreateRig(r config.Rig) error

	// UpdateRig partially updates a rig in city.toml.
	UpdateRig(name string, patch RigUpdate) error

	// DeleteRig removes a rig from city.toml.
	DeleteRig(name string) error

	// CreateProvider adds a new city-level provider to city.toml.
	CreateProvider(name string, spec config.ProviderSpec) error

	// UpdateProvider partially updates an existing city-level provider.
	UpdateProvider(name string, patch ProviderUpdate) error

	// DeleteProvider removes a city-level provider from city.toml.
	DeleteProvider(name string) error

	// --- Patch resource mutations ---

	// SetAgentPatch creates or replaces an agent patch.
	SetAgentPatch(patch config.AgentPatch) error

	// DeleteAgentPatch removes an agent patch by qualified name.
	DeleteAgentPatch(name string) error

	// SetRigPatch creates or replaces a rig patch.
	SetRigPatch(patch config.RigPatch) error

	// DeleteRigPatch removes a rig patch by name.
	DeleteRigPatch(name string) error

	// SetProviderPatch creates or replaces a provider patch.
	SetProviderPatch(patch config.ProviderPatch) error

	// DeleteProviderPatch removes a provider patch by name.
	DeleteProviderPatch(name string) error

	// --- Order overrides ---

	// EnableOrder enables an order via overrides in city.toml.
	EnableOrder(name, rig string) error

	// DisableOrder disables an order via overrides in city.toml.
	DisableOrder(name, rig string) error
}
