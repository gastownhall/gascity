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
// Directory lookup errors, decode errors, Unbind failures, and
// ReassignSessionBindings failures abort the sweep and are returned to the
// caller so the reconciler logs the failed tick and retries.
//
// The sweep is idempotent and safe to run on every reconciler tick; it must run
// after session beads have been synced for the tick so a respawned session's
// replacement bead is already visible.
func ReapStaleBindings(ctx context.Context, store beads.Store, now time.Time) (BindingReapStats, error) {
	return ReapStaleBindingsWithSessionDirectory(ctx, store, session.NewStore(beads.SessionStore{Store: store}), now)
}

// ReapStaleBindingsWithSessionDirectory reconciles Messaging binding records
// using the independently selected typed Sessions address/liveness directory.
func ReapStaleBindingsWithSessionDirectory(ctx context.Context, store beads.Store, sessions session.AddressDirectory, now time.Time) (BindingReapStats, error) {
	var stats BindingReapStats
	if err := checkContext(ctx); err != nil {
		return stats, err
	}
	if store == nil {
		return stats, nil
	}
	if nilAddressDirectory(sessions) {
		return stats, errors.New("reaping stale bindings requires sessions directory")
	}
	items, err := store.List(beads.ListQuery{Label: labelBindingBase})
	if err != nil {
		return stats, fmt.Errorf("list active bindings: %w", err)
	}
	svc, err := NewServicesWithSessionDirectory(store, sessions)
	if err != nil {
		return stats, err
	}
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

		liveID, dead, err := bindingLiveTarget(sessions, record)
		if err != nil {
			return stats, newSafeOperationError("resolve stale binding live session", err)
		}
		switch {
		case dead:
			if _, err := svc.Bindings.Unbind(ctx, caller, UnbindInput{
				Conversation: &record.Conversation,
				Now:          now,
			}); err != nil {
				return stats, newSafeOperationError("clear stale session binding", err)
			}
			stats.Cleared++
		case liveID != "" && liveID != record.SessionID:
			if _, ok := reassigned[record.SessionID]; ok {
				break
			}
			if err := ReassignSessionBindings(ctx, store, record.SessionID, liveID, now); err != nil {
				return stats, newSafeOperationError("reassign stale session bindings", err)
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
// recorded name, or whose name definitively no longer resolves to a live
// session, are left untouched: RemoveParticipant and CloseSessionBindings own
// participant teardown, and a genuine respawn always re-resolves to a live
// bead. Indeterminate directory failures abort the sweep so the reconciler
// reports the failure and retries on its next tick.
//
// The sweep is idempotent and safe to run on every reconciler tick; it must run
// after session beads have been synced for the tick so a respawned session's
// replacement bead is already visible.
func ReapStaleParticipants(ctx context.Context, store beads.Store) (ParticipantReapStats, error) {
	return ReapStaleParticipantsWithSessionDirectory(ctx, store, session.NewStore(beads.SessionStore{Store: store}))
}

// ReapStaleParticipantsWithSessionDirectory heals Messaging participants from
// the independently selected typed Sessions address/liveness directory.
func ReapStaleParticipantsWithSessionDirectory(ctx context.Context, store beads.Store, sessions session.AddressDirectory) (ParticipantReapStats, error) {
	var stats ParticipantReapStats
	if err := checkContext(ctx); err != nil {
		return stats, err
	}
	if store == nil {
		return stats, nil
	}
	if nilAddressDirectory(sessions) {
		return stats, errors.New("reaping stale participants requires sessions directory")
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
		live, err := resolveLiveSession(sessions, name)
		switch {
		case errors.Is(err, session.ErrSessionNotFound):
			continue
		case err != nil:
			return stats, newSafeOperationError("resolve stale participant live session", err)
		}
		liveID, err := resolvedLiveSessionID(live)
		if err != nil {
			return stats, newSafeOperationError("resolve stale participant live session", err)
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
				return stats, newSafeOperationError("reassign stale session participants", err)
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
				return stats, newSafeOperationError("finish pending stale session participant cleanup", err)
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
// cleared). Indeterminate directory failures are returned so the reconciler
// reports the failed sweep and retries on its next tick.
func bindingLiveTarget(sessions session.AddressDirectory, record SessionBindingRecord) (liveID string, dead bool, err error) {
	name := record.SessionName
	if name != "" {
		info, err := resolveLiveSession(sessions, name)
		switch {
		case errors.Is(err, session.ErrSessionNotFound):
			return "", true, nil
		case err != nil:
			return "", false, err
		default:
			liveID, err := resolvedLiveSessionID(info)
			return liveID, false, err
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
		return "", false, nil
	}
	info, err := sessions.ResolveAddress(stored, true)
	if errors.Is(err, session.ErrSessionNotFound) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Closed || !session.IsSessionBeadOrRepairableInfo(info) {
		return "", true, nil
	}
	return stored, false, nil
}
