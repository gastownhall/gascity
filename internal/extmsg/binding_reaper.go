package extmsg

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// ParticipantReapStats summarizes a single ReapStaleParticipants sweep.
type ParticipantReapStats struct {
	// Scanned counts active group participants examined.
	Scanned int
	// Reassigned counts retired session IDs whose group participants were
	// re-pointed at a new live bead after respawn. Each unique stale session ID
	// is counted once even when several participants share it
	// (ReassignSessionParticipants migrates all participants for the session).
	Reassigned int
}

// ReapStaleParticipants reconciles active group participants against live
// session identity — the participant-side analog of ReapStaleBindings. A group
// participant stores the volatile session bead ID it was created against plus the
// stable session name; when that session respawns under the same name it gets a
// fresh bead ID, leaving the participant pointing at a retired bead.
//
// Routing self-heals at read time via overlayLiveParticipantSessionID, but the
// group-owned transcript membership (keyed by session ID) has no read-time
// overlay. The canonical respawn-handover paths (session-bead reconciliation and
// API materialization repair) already call ReassignSessionParticipants, but a
// respawn reconciled only by this backstop — e.g. a binding-less group
// participant whose session the binding reaper never observes — would otherwise
// strand its membership on the dead session. This sweep re-points such
// participants at the live bead and carries their transcript membership on the
// same NDI cadence as the binding reaper.
//
// It acts on two stale shapes. First, a participant whose stable session name
// resolves to a different live bead is re-pointed at that bead. Second, a
// participant whose session_id already names the live bead but whose
// previous_session_id_pending_cleanup still lists a retired session — the
// residue of a handover that committed the session_id swap and then failed
// mid-migration — has that pending handover finished so its stranded
// transcript membership is migrated to the live bead. Participants with no
// recorded name, or whose name no longer resolves to a live session, are left
// untouched: RemoveParticipant and CloseSessionBindings own participant
// teardown, and a genuine respawn always re-resolves to a live bead.
//
// The sweep is idempotent and safe to run on every reconciler tick; it must run
// after session beads have been synced for the tick so a respawned session's
// replacement bead is already visible.
func ReapStaleParticipants(ctx context.Context, store beads.Store) (ParticipantReapStats, error) {
	var stats ParticipantReapStats
	if err := checkContext(ctx); err != nil {
		return stats, err
	}
	if store == nil {
		return stats, nil
	}
	items, err := store.List(beads.ListQuery{Label: labelGroupParticipantBase})
	if err != nil {
		return stats, fmt.Errorf("list active group participants: %w", err)
	}
	// reassigned tracks retired session IDs already handed over so we don't call
	// ReassignSessionParticipants (which migrates all participants for a session)
	// more than once per session per sweep.
	reassigned := make(map[string]struct{})
	for _, item := range items {
		if err := checkContext(ctx); err != nil {
			return stats, err
		}
		if !hasLabel(item, "gc:extmsg-participant") || item.Status == "closed" {
			continue
		}
		record, err := decodeParticipantBead(item)
		if err != nil {
			return stats, fmt.Errorf("decode participant %s: %w", item.ID, err)
		}
		stats.Scanned++
		name := strings.TrimSpace(record.SessionName)
		oldID := strings.TrimSpace(record.SessionID)
		if name == "" || oldID == "" {
			continue
		}
		liveID, err := resolveLiveSessionID(store, name)
		if err != nil || liveID == "" {
			continue
		}
		if liveID != oldID {
			// session_id still names a retired bead: re-point the participant at
			// the live replacement, carrying its transcript membership. This
			// handover appends oldID to (and processes) any cleanup already
			// pending on the bead, so it subsumes the pending-cleanup pass below.
			if _, done := reassigned[oldID]; done {
				continue
			}
			if err := ReassignSessionParticipants(ctx, store, oldID, liveID); err != nil {
				return stats, fmt.Errorf("reassign participants for retired session %s to live bead %s: %w", oldID, liveID, err)
			}
			reassigned[oldID] = struct{}{}
			stats.Reassigned++
			continue
		}
		// session_id already names the live bead, but a prior handover may have
		// committed the session_id swap and then failed mid-migration, leaving a
		// retired session in previous_session_id_pending_cleanup with its
		// transcript membership still stranded on the dead bead. The
		// liveID == oldID fast path never retries those, so finish each pending
		// handover by re-driving it to the live bead: participantReassignmentPending
		// recognizes the already-swapped state and migrateParticipantGroupMembership
		// completes the membership migration and clears the pending record.
		for _, pendingOldID := range pendingCleanupSessionIDsFromMetadata(item.Metadata) {
			if pendingOldID == "" || pendingOldID == oldID {
				continue
			}
			if _, done := reassigned[pendingOldID]; done {
				continue
			}
			if err := ReassignSessionParticipants(ctx, store, pendingOldID, oldID); err != nil {
				return stats, fmt.Errorf("finish pending participant cleanup from retired session %s to live bead %s: %w", pendingOldID, oldID, err)
			}
			reassigned[pendingOldID] = struct{}{}
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
