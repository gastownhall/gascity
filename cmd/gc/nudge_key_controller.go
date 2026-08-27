package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

const nudgeKeyControllerMaxDistinct = 4096

// discoverDueExactNudgeSessionIDs returns canonical session IDs from due queue
// records in their durable order. It deliberately treats the queue as
// authoritative: the returned IDs are only exact-read scheduling hints.
func discoverDueExactNudgeSessionIDs(state nudgequeue.State, now time.Time) []string {
	ids := make([]string, 0, len(state.Pending)+len(state.InFlight))
	seen := make(map[string]struct{}, cap(ids))
	add := func(item nudgequeue.Item, due bool) {
		id := strings.TrimSpace(item.SessionID)
		if !due || id == "" || id != item.SessionID || !validExactNudgeSessionID(id) {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, item := range state.Pending {
		add(item, item.DeliverAfter.IsZero() || !item.DeliverAfter.After(now))
	}
	for _, item := range state.InFlight {
		add(item, !item.LeaseUntil.IsZero() && item.LeaseUntil.Before(now))
	}
	return ids
}

func validExactNudgeSessionID(id string) bool {
	if len(id) > sessionStartAdmissionMaxIDBytes || !strings.ContainsRune(id, '-') {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// nudgeKeyController is a narrow adapter around the existing bounded keyed
// controller. Nudge and start share the same mechanical invariant: an in-memory
// key is only a hint; the worker rereads durable state before doing any effect.
type nudgeKeyController struct {
	controller *sessionStartController
}

type nudgeKeyControllerOptions struct {
	Workers     int
	MaxDistinct int
	MaxRetries  int
	Reconcile   func(context.Context, string) error
	Observer    func(sessionStartReconcileResult)
}

func newNudgeKeyController(opts nudgeKeyControllerOptions) (*nudgeKeyController, error) {
	if opts.Reconcile == nil {
		return nil, fmt.Errorf("creating nudge-key controller: reconcile function is nil")
	}
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     opts.Workers,
		MaxDistinct: opts.MaxDistinct,
		MaxRetries:  opts.MaxRetries,
		Reconcile: func(ctx context.Context, admission sessionStartAdmission) error {
			return opts.Reconcile(ctx, admission.SessionID)
		},
		Observer: opts.Observer,
	})
	if err != nil {
		return nil, fmt.Errorf("creating nudge-key controller: %w", err)
	}
	return &nudgeKeyController{controller: controller}, nil
}

func (c *nudgeKeyController) Start(ctx context.Context) error {
	if c == nil || c.controller == nil {
		return fmt.Errorf("starting nudge-key controller: controller is nil")
	}
	return c.controller.Start(ctx)
}

func (c *nudgeKeyController) Admit(sessionID string) (sessionStartAdmissionOutcome, error) {
	if !validExactNudgeSessionID(sessionID) {
		return "", fmt.Errorf("admitting exact nudge session %q: ID is not canonical", sessionID)
	}
	if c == nil || c.controller == nil {
		return "", fmt.Errorf("admitting exact nudge session %q: controller is nil", sessionID)
	}
	return c.controller.Admit(sessionID, sessionStartAdmissionSocket)
}

func (c *nudgeKeyController) Stop() {
	if c != nil && c.controller != nil {
		c.controller.Stop()
	}
}

func (c *nudgeKeyController) RequestAudit() {
	if c != nil && c.controller != nil {
		c.controller.RequestAudit()
	}
}

func (c *nudgeKeyController) TakeAuditRequest() bool {
	return c != nil && c.controller != nil && c.controller.TakeAuditRequest()
}

type exactQueuedNudgeParams struct {
	CityPath     string
	Config       *config.City
	Provider     runtime.Provider
	SessionStore beads.Store
	NudgeStore   beads.Store
}

// reconcileExactQueuedNudge rereads exactly one session and delegates the
// physical delivery and claim/ack fence to the established queue helper.
func reconcileExactQueuedNudge(ctx context.Context, sessionID string, params exactQueuedNudgeParams) error {
	if ctx == nil {
		return fmt.Errorf("reconciling exact queued nudge %q: context is nil", sessionID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validExactNudgeSessionID(sessionID) {
		return fmt.Errorf("reconciling exact queued nudge %q: session ID is not canonical", sessionID)
	}
	if params.Config == nil || params.Provider == nil || params.SessionStore == nil || params.NudgeStore == nil {
		return fmt.Errorf("reconciling exact queued nudge %q: coherent runtime snapshot is incomplete", sessionID)
	}
	info, _, err := getAuthoritativeSessionStartRecord(params.SessionStore, sessionID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("reconciling exact queued nudge %q: reading session: %w", sessionID, err)
	}
	if info.ID != sessionID || info.Closed {
		return nil
	}
	target := resolveNudgeTargetFromSessionInfo(params.CityPath, params.Config, info)
	if target.sessionID != sessionID {
		return fmt.Errorf("reconciling exact queued nudge %q: target resolved to %q", sessionID, target.sessionID)
	}
	obs, err := workerObserveNudgeTarget(target, params.SessionStore, params.Provider)
	if err != nil {
		return fmt.Errorf("reconciling exact queued nudge %q: observing target: %w", sessionID, err)
	}
	if !obs.Running {
		return nil
	}
	if obs.Attached {
		return nil
	}
	_, err = tryDeliverQueuedNudgesByPollerMatching(target, params.NudgeStore, params.SessionStore, params.Provider, defaultNudgePollQuiescence, obs, func(item queuedNudge) bool {
		return item.SessionID == sessionID
	})
	if err != nil {
		return fmt.Errorf("reconciling exact queued nudge %q: delivering: %w", sessionID, err)
	}
	return nil
}
