// Package worker owns the canonical in-memory worker boundary and catalog APIs.
package worker

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type (
	// SessionInfo describes a single session as exposed through the worker catalog.
	SessionInfo = sessionpkg.Info
	// SessionListResult carries a bead-backed catalog listing result.
	SessionListResult = sessionpkg.ListResult
	// SessionPruneResult reports the outcome of catalog pruning.
	SessionPruneResult = sessionpkg.PruneResult
	// SessionSubmissionCapabilities describes submit/nudge support for a session.
	SessionSubmissionCapabilities = sessionpkg.SubmissionCapabilities
	// SessionPersistedResponse carries the persisted half of a session's API
	// response (status + metadata) projected from the session bead.
	SessionPersistedResponse = sessionpkg.PersistedResponse
)

// SessionCatalog exposes worker-owned session discovery and maintenance
// helpers so higher layers do not depend on session.Manager directly.
type SessionCatalog struct {
	manager *sessionpkg.Manager
}

// NewSessionCatalog constructs a worker-owned session catalog facade.
func NewSessionCatalog(manager *sessionpkg.Manager) (*SessionCatalog, error) {
	if manager == nil {
		return nil, fmt.Errorf("%w: manager is required", ErrHandleConfig)
	}
	return &SessionCatalog{manager: manager}, nil
}

// List returns sessions filtered by state and template.
func (c *SessionCatalog) List(stateFilter, templateFilter string) ([]SessionInfo, error) {
	return c.manager.List(stateFilter, templateFilter)
}

// Get loads one session by ID.
func (c *SessionCatalog) Get(id string) (SessionInfo, error) {
	return c.manager.Get(id)
}

// GetWithPersistedResponse loads one session by ID, returning the
// runtime-enriched Info plus the persisted-response projection (status +
// metadata) in a single fetch, so the API response path avoids a redundant raw
// store.Get beside Get.
func (c *SessionCatalog) GetWithPersistedResponse(id string) (SessionInfo, SessionPersistedResponse, error) {
	return c.manager.GetWithPersistedResponse(id)
}

// ListFullFromBeads expands a bead set into full session listing results.
func (c *SessionCatalog) ListFullFromBeads(all []beads.Bead, stateFilter, templateFilter string) *SessionListResult {
	return c.manager.ListFullFromBeads(all, stateFilter, templateFilter)
}

// SubmissionCapabilities reports whether the session can accept submit-style input.
func (c *SessionCatalog) SubmissionCapabilities(id string) (SessionSubmissionCapabilities, error) {
	return c.manager.SubmissionCapabilities(id)
}

// UpdatePresentation updates session display metadata such as title and alias.
func (c *SessionCatalog) UpdatePresentation(id string, title, alias *string) error {
	return c.manager.UpdatePresentation(id, title, alias)
}

// SessionState aliases session.State so callers can name terminal states
// without importing the session package directly.
type SessionState = sessionpkg.State

// Session state constants re-exported for the worker boundary.
const (
	SessionStateSuspended = sessionpkg.StateSuspended
	SessionStateAsleep    = sessionpkg.StateAsleep
	SessionStateDrained   = sessionpkg.StateDrained
)

// PruneBefore removes sessions in the given states older than the provided
// cutoff and reports the result. When states is empty it defaults to
// [SessionStateSuspended].
func (c *SessionCatalog) PruneBefore(before time.Time, states ...SessionState) (SessionPruneResult, error) {
	return c.manager.PruneDetailed(before, states...)
}
