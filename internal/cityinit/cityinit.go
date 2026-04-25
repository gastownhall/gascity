// Package cityinit is the domain contract for city scaffolding and
// finalization. It defines the typed request, result, and sentinel
// errors that both projections — the CLI (cmd/gc/cmd_init.go) and the
// HTTP API (internal/api/huma_handlers_supervisor.go:humaHandleCityCreate) —
// use when creating a new city.
//
// The Initializer interface is implemented in cmd/gc (where the
// scaffold + finalize body currently lives) and injected into the
// HTTP supervisor at construction. The HTTP handler calls
// Initializer.Init in-process; there is no subprocess, no
// 30-second deadline, no stderr-scraping for error dispatch.
//
// A follow-up refactor will physically move the scaffold/finalize
// body into this package so the domain logic lives in internal/
// (per engdocs/architecture/api-control-plane.md §1). Until then, injecting the
// implementation from cmd/gc at startup preserves the architectural
// intent that "the CLI and the HTTP API are projections over the
// shared object model" — both surfaces drive the same code path via
// the same typed contract.
package cityinit

import (
	"context"
	"errors"
)

// Typed sentinel errors. Both projections map them to their own
// surface: the CLI renders human-readable blocker lists; the HTTP
// handler maps each to the appropriate status code (409, 400, 503,
// etc.). Error strings are suitable for display in either surface.
var (
	// ErrAlreadyInitialized indicates the target directory already
	// contains a Gas City scaffold. The HTTP API maps this to 409
	// Conflict. The CLI can either ignore (idempotent reinit) or
	// surface, depending on flags.
	ErrAlreadyInitialized = errors.New("city already initialized")

	// ErrInvalidProvider indicates an unknown builtin provider. The
	// HTTP API maps this to 400 Bad Request (or 422 Unprocessable
	// Entity at the typed-input layer).
	ErrInvalidProvider = errors.New("invalid provider")

	// ErrInvalidBootstrapProfile indicates an unrecognized
	// bootstrap_profile value.
	ErrInvalidBootstrapProfile = errors.New("invalid bootstrap profile")

	// ErrMissingDependency indicates a hard runtime dependency
	// (tmux, git, dolt, bd, flock, jq, pgrep, lsof) is missing or
	// too old. Maps to 503 Service Unavailable at the HTTP layer.
	// The error message lists every missing dependency so the CLI
	// can render install hints without another probe pass.
	ErrMissingDependency = errors.New("missing hard dependency")

	// ErrProviderNotReady indicates at least one provider the city
	// references is not ready (no auth, not installed, invalid
	// config, or probe failure). Only returned when
	// InitRequest.SkipProviderReadiness is false. Maps to 503 at
	// the HTTP layer.
	ErrProviderNotReady = errors.New("provider not ready")

	// ErrConfigLoad indicates the city was scaffolded but its
	// on-disk configuration could not be re-parsed after write.
	// Usually a bug in the scaffold step; maps to 500.
	ErrConfigLoad = errors.New("loading city config")

	// ErrNotWired indicates the HTTP handler was called before a
	// concrete Initializer was injected into the supervisor. This
	// is a programmer-bug tripwire: every SupervisorMux constructed
	// at runtime must have a non-nil Initializer.
	ErrNotWired = errors.New("cityinit: no Initializer wired into supervisor")

	// ErrNotRegistered indicates Unregister was called for a city
	// that is not in the supervisor registry. Maps to 404 Not Found
	// at the HTTP layer.
	ErrNotRegistered = errors.New("city not registered with supervisor")
)

// InitRequest is the typed input. Both projections populate it from
// their own surface (CLI flags, HTTP request body) and hand it to
// Initializer.Init; neither duplicates validation or logic.
type InitRequest struct {
	// Dir is the absolute path of the new city directory. Callers
	// resolve relative paths before invoking Init (the CLI uses
	// filepath.Abs; the API handler joins against $HOME when
	// relative).
	Dir string

	// Provider is the builtin provider key ("claude", "codex",
	// "gemini", ...) for the city's default workspace. Empty iff
	// StartCommand is set.
	Provider string

	// StartCommand is an opt-in custom workspace provider command.
	// Empty unless the caller wants a non-builtin workspace.
	StartCommand string

	// BootstrapProfile is one of "", "k8s-cell", "kubernetes",
	// "kubernetes-cell", "single-host-compat".
	BootstrapProfile string

	// NameOverride is an explicit city name. Empty means derive
	// from filepath.Base(Dir).
	NameOverride string

	// SkipProviderReadiness skips the provider-auth preflight when
	// true. The async HTTP create handler defaults to true and
	// surfaces dependency/provider blockers later via the terminal
	// city.init_failed event on /v0/events/stream. The CLI defaults
	// to false so first-time users see auth-needed errors
	// immediately.
	SkipProviderReadiness bool

	// ConfigName selects the scaffold template. One of "tutorial"
	// (default), "gastown", or "custom". Empty is treated as
	// "tutorial". The CLI wizard resolves this; the HTTP API
	// always leaves it empty.
	ConfigName string
}

// InitResult describes what Init produced. Callers build their own
// surface-specific response from it (CLI status messages, HTTP JSON
// body).
type InitResult struct {
	// CityName is the name persisted to city.toml.
	CityName string

	// CityPath is the absolute city directory (same as
	// InitRequest.Dir after normalization).
	CityPath string

	// ProviderUsed is the resolved provider name.
	ProviderUsed string

	// Resumed is true when Init detected an existing scaffold and
	// skipped to finalization only.
	Resumed bool
}

// Initializer is the domain contract for city lifecycle on the
// supervisor: scaffolding, finalization, and unregistration. Exactly
// one implementation exists per process, supplied at supervisor
// construction (see internal/api.NewSupervisorMux). Both projections
// — CLI and HTTP API — drive the same code path via this interface.
type Initializer interface {
	// Init scaffolds + finalizes a new city.
	//
	// Preconditions: req.Dir is an absolute path; exactly one of
	// req.Provider / req.StartCommand is set; req.BootstrapProfile
	// is a known value.
	//
	// Postconditions on nil error: the directory contains a
	// complete city scaffold; the bead provider is initialized; the
	// city is registered with any running supervisor.
	//
	// Errors returned wrap one of the ErrXxx sentinels in this
	// package so callers can dispatch via errors.Is.
	Init(ctx context.Context, req InitRequest) (*InitResult, error)

	// Scaffold writes the new city's on-disk shape and registers it
	// with the supervisor — the fast portion of Init. Used by the
	// HTTP API handler behind POST /v0/city so it can return 202
	// Accepted immediately instead of blocking on the slow finalize
	// work. The supervisor reconciler takes over from there; city
	// readiness is signaled via city.ready / city.init_failed
	// events on the supervisor event bus, not via the handler's
	// response body.
	//
	// The implementation emits a city.created event before
	// returning so subscribers of /v0/events/stream observe the
	// new city before Finalize begins.
	Scaffold(ctx context.Context, req InitRequest) (*InitResult, error)

	// Unregister removes the city from the supervisor's registry
	// and signals the supervisor to reconcile. Used by the HTTP API
	// handler behind POST /v0/city/{cityName}/unregister so it can
	// return 202 Accepted immediately while the reconciler stops
	// the controller asynchronously.
	//
	// Returns ErrNotRegistered if the named city is not in the
	// registry. On nil error, emits a city.unregister_requested
	// event to the city's event log so subscribers of
	// /v0/events/stream observe the start of the teardown. The
	// terminal completion event (city.unregistered or
	// city.unregister_failed) is emitted by the supervisor
	// reconciler once the city's controller finishes stopping.
	//
	// The city directory itself is NOT touched. Users that want to
	// purge the directory remove it separately.
	Unregister(ctx context.Context, req UnregisterRequest) (*UnregisterResult, error)
}

// UnregisterRequest is the typed input for Initializer.Unregister.
type UnregisterRequest struct {
	// CityName is the supervisor-registered name (effective name,
	// e.g. workspace.name from city.toml, or directory basename if
	// unset). Required; looked up in the registry by name.
	CityName string
}

// UnregisterResult describes what Unregister produced. Callers
// build their own surface-specific response from it.
type UnregisterResult struct {
	// CityName is the resolved registry name.
	CityName string

	// CityPath is the absolute city directory whose entry was
	// removed from the registry. Useful for clients that want to
	// filter completion events by path as well as name.
	CityPath string
}
