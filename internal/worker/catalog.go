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

// PruneBefore removes sessions older than the provided cutoff and reports the result.
func (c *SessionCatalog) PruneBefore(before time.Time) (SessionPruneResult, error) {
	return c.manager.PruneDetailed(before)
}
