// Package worker owns the canonical in-memory worker boundary and catalog APIs.
package worker

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type (
	SessionInfo                   = sessionpkg.Info
	SessionListResult             = sessionpkg.ListResult
	SessionPruneResult            = sessionpkg.PruneResult
	SessionSubmissionCapabilities = sessionpkg.SubmissionCapabilities
)

// SessionCatalog exposes worker-owned session discovery and maintenance
// helpers so higher layers do not depend on session.Manager directly.
type SessionCatalog struct {
	manager *sessionpkg.Manager
}

func NewSessionCatalog(manager *sessionpkg.Manager) (*SessionCatalog, error) {
	if manager == nil {
		return nil, fmt.Errorf("%w: manager is required", ErrHandleConfig)
	}
	return &SessionCatalog{manager: manager}, nil
}

func (c *SessionCatalog) List(stateFilter, templateFilter string) ([]SessionInfo, error) {
	return c.manager.List(stateFilter, templateFilter)
}

func (c *SessionCatalog) Get(id string) (SessionInfo, error) {
	return c.manager.Get(id)
}

func (c *SessionCatalog) ListFullFromBeads(all []beads.Bead, stateFilter, templateFilter string) *SessionListResult {
	return c.manager.ListFullFromBeads(all, stateFilter, templateFilter)
}

func (c *SessionCatalog) SubmissionCapabilities(id string) (SessionSubmissionCapabilities, error) {
	return c.manager.SubmissionCapabilities(id)
}

func (c *SessionCatalog) UpdatePresentation(id string, title, alias *string) error {
	return c.manager.UpdatePresentation(id, title, alias)
}

func (c *SessionCatalog) PruneBefore(before time.Time) (SessionPruneResult, error) {
	return c.manager.PruneDetailed(before)
}
