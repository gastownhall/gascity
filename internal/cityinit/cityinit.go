// Package cityinit owns the typed city scaffolding/finalization service
// used by the CLI and HTTP API projections.
//
// The scaffold + finalize bodies are still being split out of cmd/gc,
// so Service receives those side-effecting operations as dependencies.
// The orchestration, validation, rollback, and lifecycle event emission
// now live here instead of in the transport layers.
//
// The HTTP handler calls Service.Scaffold in-process; there is no
// subprocess, no 30-second deadline, and no stderr-scraping for error
// dispatch.
package cityinit

import (
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

	// ErrInvalidDirectory indicates the requested city directory is
	// missing or not absolute. The HTTP API maps this to 422
	// Unprocessable Entity.
	ErrInvalidDirectory = errors.New("invalid city directory")

	// ErrInvalidProvider indicates an unknown builtin provider. The
	// HTTP API maps this to 422 Unprocessable Entity.
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

	// ErrNotWired indicates the service was constructed without a
	// required dependency. This is a programmer-bug tripwire for
	// process wiring.
	ErrNotWired = errors.New("cityinit: service dependency not wired")

	// ErrNotRegistered indicates Unregister was called for a city
	// that is not in the supervisor registry. Maps to 404 Not Found
	// at the HTTP layer.
	ErrNotRegistered = errors.New("city not registered with supervisor")
)

// InitRequest is the typed input. Both projections populate it from
// their own surface (CLI flags, HTTP request body) and hand it to
// Service.Init or Service.Scaffold; neither duplicates validation or logic.
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

	// ReloadWarning is non-empty when the supervisor reload after
	// scaffold succeeded but returned a best-effort error.
	ReloadWarning string
}

// UnregisterRequest is the typed input for Service.Unregister.
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

	// ReloadWarning is non-empty when the supervisor reload after
	// unregister succeeded but returned a best-effort error.
	ReloadWarning string
}
