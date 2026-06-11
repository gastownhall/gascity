package extmsg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// BindingReapStats summarizes a single ReapStaleBindings sweep.
type BindingReapStats struct {
	// Scanned counts active bindings examined.
	Scanned int
	// Reassigned counts sessions whose bindings were re-pointed at a new live
	// bead after respawn. Each unique stale-session-ID is counted once even
	// if that session has multiple active conversation bindings (ReassignSessionBindings
	// updates all bindings for the session atomically).
	Reassigned int
	// Cleared counts bindings closed because their session has no live owner.
	Cleared int
}

// ReapStaleBindings reconciles active conversation bindings against live
// session identity. A binding stores the volatile session bead ID it was
// created against; when that session crashes and respawns under the same name
// it gets a fresh bead ID, leaving the binding pointing at a dead session.
// Inbound routing then resolves to the dead bead and silently drops, and a
// fresh bind is rejected with ErrBindingConflict because the conversation is
// still bound.
//
// For each active binding the reaper:
//   - re-points it at the session's current live bead when the binding's stable
//     session name now resolves to a different (respawned) bead ID;
//   - clears it (closing the binding and its delivery/membership state) when no
//     live session owns the name, or — for legacy bindings with no recorded
//     name — when the stored bead ID is no longer a live session.
//
// Bindings whose live target already matches the stored ID are left untouched.
// Error tolerance: lookup errors inside bindingLiveTarget (transient store
// reads) cause the individual binding to be skipped. Decode errors, Unbind
// failures, and ReassignSessionBindings failures abort the sweep and are
// returned to the caller (the reconciler wiring logs them).
//
// The sweep is idempotent and safe to run on every reconciler tick; it must run
// after session beads have been synced for the tick so a respawned session's
// replacement bead is already visible.
func ReapStaleBindings(ctx context.Context, store beads.Store, now time.Time) (BindingReapStats, error) {
	var stats BindingReapStats
	if err := checkContext(ctx); err != nil {
		return stats, err
	}
	if store == nil {
		return stats, nil
	}
	items, err := store.List(beads.ListQuery{Label: labelBindingBase})
	if err != nil {
		return stats, fmt.Errorf("list active bindings: %w", err)
	}
	svc := NewServices(store)
	caller := Caller{Kind: CallerController, ID: "binding-reaper"}
	now = zeroNow(now)
	// reassigned tracks stale session IDs already processed so we don't call
	// ReassignSessionBindings (which operates on all bindings for a session)
	// more than once per session per sweep.
	reassigned := make(map[string]struct{})
	for _, item := range items {
		if err := checkContext(ctx); err != nil {
			return stats, err
		}
		record, err := decodeBindingBead(item)
		if err != nil {
			return stats, fmt.Errorf("decode binding %s: %w", item.ID, err)
		}
		if record.Status != BindingActive {
			continue
		}
		stats.Scanned++

		liveID, dead := bindingLiveTarget(store, record)
		switch {
		case dead:
			if _, err := svc.Bindings.Unbind(ctx, caller, UnbindInput{
				Conversation: &record.Conversation,
				Now:          now,
			}); err != nil {
				return stats, fmt.Errorf("clear dead binding %s: %w", record.ID, err)
			}
			stats.Cleared++
		case liveID != "" && liveID != record.SessionID:
			if _, ok := reassigned[record.SessionID]; ok {
				break
			}
			if err := ReassignSessionBindings(ctx, store, record.SessionID, liveID, now); err != nil {
				return stats, fmt.Errorf("reassign session %s to live bead %s: %w", record.SessionID, liveID, err)
			}
			reassigned[record.SessionID] = struct{}{}
			stats.Reassigned++
		}
	}
	return stats, nil
}

// bindingLiveTarget resolves the current live session bead a binding should
// point at. It returns (liveID, false) when a live target exists, ("", true)
// when the binding's session is definitively gone (so the binding should be
// cleared), and ("", false) when the state is indeterminate and the binding
// should be left untouched (e.g. a transient store error or an ambiguous name).
func bindingLiveTarget(store beads.Store, record SessionBindingRecord) (liveID string, dead bool) {
	name := record.SessionName
	if name != "" {
		id, err := resolveLiveSessionID(store, name)
		switch {
		case errors.Is(err, session.ErrSessionNotFound):
			return "", true
		case err != nil:
			return "", false
		default:
			return id, false
		}
	}
	// Legacy binding with no recorded name: it can only ever point at the bead
	// ID it stored, which never recovers across respawn (the replacement gets a
	// fresh ID). Clear it once that bead is gone or closed; otherwise leave it.
	// This path is self-eliminating: new bindings always record a SessionName,
	// and re-binds opportunistically backfill it on existing entries. Legacy
	// bindings that are never re-bound are eventually cleared here when their
	// session is retired — no active migration is needed.
	stored := record.SessionID
	if stored == "" {
		return "", false
	}
	bead, err := store.Get(stored)
	if errors.Is(err, beads.ErrNotFound) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	if bead.Status == "closed" || !session.IsSessionBeadOrRepairable(bead) {
		return "", true
	}
	return stored, false
}
