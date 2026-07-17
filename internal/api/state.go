// Package api implements the GC HTTP API server.
//
// The server embeds in the controller process and serves typed JSON
// endpoints over REST, replacing subprocess-based data access. It
// activates via [api] port = N in city.toml (progressive activation).
package api

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/usage"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// MaintenanceProvider is the subset of supervisor.StoreMaintenanceLoop that
// the API layer consumes. Defining it here keeps handlers from importing
// the full supervisor runtime and lets tests substitute a fake without
// wiring a real loop goroutine. The concrete *supervisor.StoreMaintenanceLoop
// satisfies this interface directly — no adapter is required.
type MaintenanceProvider interface {
	// LastRunAt returns the start time of the most recent maintenance
	// run, or the zero value when none has completed.
	LastRunAt() time.Time

	// History returns a copy of the bounded run history in chronological
	// order (oldest first).
	History() []supervisor.MaintenanceRun

	// InFlightStart reports the start time of the currently in-flight run
	// and whether one is running. Non-blocking — safe to call from the
	// /status handler while a real cycle holds the lease for minutes.
	InFlightStart() (time.Time, bool)

	// TriggerNow runs one maintenance cycle synchronously. When the lease
	// is held it returns *supervisor.MaintenanceInProgressError so the
	// POST handler can translate to 409 Conflict.
	TriggerNow(ctx context.Context) (supervisor.MaintenanceRun, error)
}

// State provides read access to controller-managed state.
// The controller implements this with RWMutex-protected hot-reload.
//
// The per-coordination-class store seam is exposed as named accessors
// (GraphBeadStore / SessionsBeadStore / NudgesBeadStore) so a future per-class
// backend becomes a change in the implementation rather than at every call
// site. Callers that need the store for a specific bead class route through
// those accessors; on a single-store city every one collapses to the same
// concrete store CityBeadStore() returns, so they are byte-identical today.
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

	// UsageSink returns the usage-fact sink (never nil; usage.Discard when off).
	UsageSink() usage.Sink

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

	// ScopedStoreLike returns a throwaway, ctx-bound clone of existing when
	// existing is (or wraps) a bd-CLI-shell-backed store: cancellation kills
	// the backend bd subprocess instead of abandoning it to run past ctx's
	// deadline, unlike existing's own long-lived runner (fixed to
	// context.Background() at construction). Returns (nil, nil) when
	// existing is not bd-CLI backed (e.g. a native, file, or in-memory
	// store) — those have no subprocess to leak, so callers should keep
	// reading through existing directly in that case.
	//
	// Read paths with their own short request budget (e.g. GET /status) use
	// this instead of reading through the shared store so a slow bd command
	// cannot pin a Dolt connection past the caller's own deadline
	// (gascity ga-cdmx6x). Implementations must observe ctx during resolution
	// and finish any work they start before returning after cancellation.
	ScopedStoreLike(ctx context.Context, existing beads.Store) (beads.Store, error)

	// NudgesBeadStore returns the store backing the nudge-queue shadow beads
	// (gc:nudge). At the default backend this is the same store as
	// CityBeadStore; when [beads.classes.nudges] is relocated it is the
	// per-class store, so nudge-shadow ops (e.g. withdrawing wait nudges on
	// session close/wake) reach the relocated beads instead of orphaning them
	// on the work store. The strongly-typed beads.NudgesStore return makes the
	// nudges class statically visible at the call site; its embedded .Store is
	// nil when no store is available.
	NudgesBeadStore() beads.NudgesStore

	// SessionsBeadStore returns the store backing session-class beads — session
	// lifecycle (type=session/gc:session) and durable session waits
	// (type=gate/gc:wait). At the default backend this is the same store as
	// CityBeadStore; when [beads.classes.sessions] is relocated it is the
	// per-class store, so session/wait reads and writes reach the relocated
	// beads instead of the work store. Session handlers source their store from
	// here (not CityBeadStore); cross-class WORK-bead reads stay on
	// CityBeadStore. The strongly-typed beads.SessionStore return makes the
	// session class statically visible at the call site; its embedded .Store is
	// nil when no store is available.
	SessionsBeadStore() beads.SessionStore

	// GraphBeadStore returns the store backing graph-class beads — the formula-v2
	// execution topology and control lane (molecules, wisps, convoys, control
	// steps). At the default backend this is the same store as CityBeadStore;
	// when [beads.classes.graph] is relocated it is the dedicated graph store at
	// the legacy <cityPath>/.gc/beads.sqlite (SQLite) or the gcg Postgres schema,
	// so graph reads and writes reach the relocated beads instead of the work
	// store. This is the class-aware graph leg; cross-class WORK-bead reads stay
	// on CityBeadStore. The strongly-typed beads.GraphStore return makes the graph
	// class statically visible at the call site; its embedded .Store is nil when no
	// store is available.
	GraphBeadStore() beads.GraphStore

	// Orders returns the current active set of scanned orders.
	// Returns nil if orders are not configured.
	Orders() []orders.Order

	// OrdersAll returns the current set of scanned orders after overrides,
	// including disabled orders that management endpoints still need to address.
	// Returns nil if orders are not configured.
	OrdersAll() []orders.Order

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

	// MaintenanceLoop returns the Dolt store-maintenance loop when
	// [maintenance.dolt] enabled=true in city.toml, or nil otherwise.
	// Handlers treat nil as "feature disabled" and respond with a typed
	// 503 so the CLI can print a useful message.
	MaintenanceLoop() MaintenanceProvider
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
	Path          string
	Prefix        string
	DefaultBranch string
	Suspended     *bool
}

// ProviderUpdate holds optional fields for a partial provider update.
// Pointer fields distinguish "not set" from "set to zero value."
//
// Base uses **string so callers can distinguish four PATCH cases:
//
//   - nil              → no-op (don't touch Base)
//   - &(*string)(nil)  → clear Base declaration (remove the TOML key)
//   - &(&"")           → set explicit empty (standalone opt-out)
//   - &(&"<name>")     → set concrete value
type ProviderUpdate struct {
	DisplayName        *string
	Base               **string
	Command            *string
	ACPCommand         *string
	Args               []string // nil = not set, non-nil = replace
	ACPArgs            []string // nil = not set, non-nil = replace
	ArgsAppend         []string // nil = not set, non-nil = replace
	PromptMode         *string
	PromptFlag         *string
	ReadyDelayMs       *int
	Env                map[string]string // nil = not set, non-nil = additive merge
	OptionsSchemaMerge *string
	OptionsSchema      []config.ProviderOption // nil = not set, non-nil = replace
	OptionDefaults     map[string]string       // nil = not set, non-nil = additive merge
}

// RawConfigProvider is optionally implemented by State to provide the
// raw (pre-expansion) config for provenance detection. Used by the
// /v0/config/explain endpoint to distinguish inline vs pack-derived agents.
type RawConfigProvider interface {
	RawConfig() *config.City
}

// WebhookDispatchProvider is optionally implemented by State to expose the live
// order dispatcher the supervisor webhook receiver (E3) fires verified+matched
// deliveries through. It is the H1/E0.5 dispatch seam: the dispatch machine lives
// in cmd/gc (memoryOrderDispatcher.dispatchOne), which internal/api cannot import,
// so the city runtime implements this accessor over the same dispatchOne core the
// tick loop uses. A State that does not implement it disables webhook dispatch —
// the receiver returns 503 rather than firing a stub, so the perimeter/verify/
// match guards still run but no order is launched. Modeled on the optional
// RawConfigProvider/AgentVisibilityWaiter capability pattern rather than a core
// State method so the two production State implementers and the test fakes are not
// all forced to grow a dispatcher they may not have.
type WebhookDispatchProvider interface {
	// WebhookDispatcher returns the order dispatcher, or nil when webhook dispatch
	// is unavailable for this city.
	WebhookDispatcher() orderdispatch.Dispatcher
}

// RolloutFlagsProvider is optionally implemented by State to expose the
// boot-latched rollout-gate snapshot resolved once at controller construction
// (internal/rollout). Modeled on RawConfigProvider/WebhookDispatchProvider so
// the test fakes are not forced to grow it: a State without it gets a
// Resolve-from-Config() fallback at Server construction (see newServer).
type RolloutFlagsProvider interface {
	RolloutFlags() rollout.Flags
}

// AgentVisibilityWaiter is an optional capability for states whose Config()
// snapshot may briefly lag a successful agent mutation. Callers that need
// strict read-after-write semantics for agent target resolution can type-assert
// this interface after CreateAgent to ensure the new agent is visible through
// findAgent before returning a success response. The interface is deliberately
// agent-scoped because POST /sling resolves targets through the agent
// projection immediately after create; rig and provider create endpoints do not
// currently expose the same follow-up target-resolution contract.
type AgentVisibilityWaiter interface {
	// WaitForAgentVisibility blocks until findAgent in the current Config()
	// resolves the given qualified agent name, or returns an error if the
	// projection does not converge before ctx is done.
	WaitForAgentVisibility(ctx context.Context, qualifiedName string) error
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

	// ProvisionRigFromGit clones gitURL into the rig's working tree and
	// provisions the rig, reusing CreateRig's config-write handshake under the
	// per-city guard. The clone runs OUTSIDE that guard (a WAN fetch must not
	// freeze config writes); the git URL host is SSRF-fenced (fail-closed)
	// before any clone. When r.Path is empty the server derives rigs/<name>.
	// onStep, when non-nil, receives incremental provisioning progress (step
	// name, human detail, warn flag) for typed-event projection. onManifest,
	// when non-nil, is called record-then-create at each resource-creation
	// checkpoint (before the clone with CreatedDir set; after init with any
	// minted DoltDB) so the caller can persist the G14 rollback manifest and
	// capture it for teardown. It returns the provisioned rig so the caller can
	// report its resolved prefix/branch. This is the async server-side rig-add
	// path (C4b/C4c); the sync CreateRig stays git-blind.
	ProvisionRigFromGit(ctx context.Context, r config.Rig, gitURL string, onStep func(step, detail string, warn bool), onManifest func(RigProvisionManifest)) (config.Rig, error)

	// TeardownPartialRig removes the created rig working tree and drops the
	// managed Dolt database named in the manifest (best-effort), then repairs
	// routes from the on-disk config. It is the physical half of the G14 atomic
	// rollback the async goroutine, the re-clone poison pre-drop, and the boot
	// sweep all share. It never removes a dir or store the manifest does not
	// claim this request created. A non-nil return means debris may remain, so
	// the caller must not mark the idempotency record rolled_back.
	TeardownPartialRig(ctx context.Context, m RigProvisionManifest) error

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

// FormulaMutator is an optional State extension for editing city-local formula
// sources (separate TOML files under <cityRoot>/formulas, not city.toml). Like
// StateMutator it is type-asserted by handlers, so a State that does not support
// formula edits simply does not implement it.
type FormulaMutator interface {
	// FormulaSource returns the raw TOML of an editable city-local formula, or
	// ok=false when no such source exists.
	FormulaSource(name string) (content []byte, ok bool, err error)
	// UpsertFormula creates or replaces a city-local formula source. Callers
	// must validate the content first; the write is atomic and refreshes state.
	UpsertFormula(name string, content []byte) error
	// DeleteFormula removes a city-local formula source.
	DeleteFormula(name string) error
}

// ConfigWriteSerializer is an optional State extension that runs fn under the
// per-city config write lock. Pack import add/remove mutate city config files
// (pack.toml, packs.lock, and sometimes city.toml) outside the
// configedit.Editor callback shape, so running them through this seam
// serializes them against the agent/rig/provider/formula mutations that take
// the same Editor lock — otherwise two concurrent net/http goroutines could
// interleave load→mutate→write and lose an update or desync manifest and
// lockfile. Like StateMutator it is type-asserted by handlers; a State that
// does not implement it runs the mutation without extra serialization.
type ConfigWriteSerializer interface {
	// SerializeConfigWrite runs fn while holding the per-city config write lock.
	SerializeConfigWrite(fn func() error) error
}
