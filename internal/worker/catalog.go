// Package worker owns the canonical in-memory worker boundary and catalog APIs.
package worker

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type (
	// SessionInfo describes a single session as exposed through the worker catalog.
	SessionInfo = sessionpkg.Info
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

// sessionRecordViaManager is the canonical worker-boundary session read: it
// composes the persisted read (session.Store.GetPersistedResponse) with the
// read-path empty-type heal (RepairTypeBestEffort, a write only when the type is
// empty) and the live runtime overlay (Manager.EnrichInfo). This is byte-identical
// to the retired Manager.GetWithBead (loadSessionBead's heal + infoFromBead's
// enrich) but returns the typed (Info, PersistedResponse) record instead of a raw
// beads.Bead, so no bead crosses the boundary. It is the single source of truth
// for every worker read that needs both the enriched Info and the persisted
// metadata (catalog Get, factory construction, handle lifecycle/telemetry).
//
// The error is bridged back to the retired GetWithBead contract
// (bridgeSessionRecordError): loadSessionBead rejected a present-but-non-session
// bead with ErrNotSession (which the API factory-lane mappers map to 400),
// whereas Store.GetPersistedResponse rejects it with ErrSessionNotFound (unmapped
// → 500). Absence keeps the beads.ErrNotFound chain (→ 404) unchanged. This
// mirrors the GET-lane bridge at internal/api/session_get_read.go exactly.
func sessionRecordViaManager(m *sessionpkg.Manager, id string) (sessionpkg.Info, sessionpkg.PersistedResponse, error) {
	front := m.PersistedStore()
	info, pr, err := front.GetPersistedResponse(id)
	if err != nil {
		return sessionpkg.Info{}, sessionpkg.PersistedResponse{}, bridgeSessionRecordError(id, err)
	}
	if info.Type == "" {
		front.RepairTypeBestEffort(id)
		info.Type = sessionpkg.BeadType
	}
	return m.EnrichInfo(info), pr, nil
}

// bridgeSessionRecordError maps a session.Store persisted-read error back to the
// error contract the API session-manager mappers (writeSessionManagerError /
// humaSessionManagerError) and cmd/gc nudge fall-through expected from the retired
// Manager.GetWithBead, preserving the status codes. A present-but-non-session bead
// swaps ErrSessionNotFound for ErrNotSession (→ 400); every other error (including
// the beads.ErrNotFound-chained absence that yields 404) passes through unchanged.
// It is the worker-lane twin of internal/api.bridgeSessionGetError.
func bridgeSessionRecordError(id string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sessionpkg.ErrSessionNotFound) && !errors.Is(err, beads.ErrNotFound) {
		return fmt.Errorf("%w: %s", sessionpkg.ErrNotSession, id)
	}
	return err
}

// ListFromInfos filters a pre-loaded persisted Info feed by state and template
// and applies the live runtime overlay to the survivors. It is the typed
// pre-fed listing the CLI session snapshot feeds (the Info analog of the retired
// ListFullFromBeads), keeping cmd/gc on the worker boundary while it lists off a
// snapshot it already loaded.
func (c *SessionCatalog) ListFromInfos(infos []SessionInfo, stateFilter, templateFilter string) []SessionInfo {
	return c.manager.ListFromInfos(infos, stateFilter, templateFilter)
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
