package api

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionGetEnriched is the read-model Get composition: the persisted read
// (session.Store.GetPersistedResponse) plus the runtime overlay
// (Manager.EnrichInfo), returning the same (Info, PersistedResponse) pair the
// retired Manager.GetWithPersistedResponse produced. It is the single-handle
// twin of the list read model — persisted reads go through the Store front door,
// the live overlay through the Manager.
//
// It bridges the two behavior deltas between Store.GetPersistedResponse and the
// old Manager.GetWithPersistedResponse (which loaded via loadSessionBead):
//
//  1. Error contract (bridgeSessionGetError): the Store rejects a present-but-
//     non-session bead with ErrSessionNotFound and wraps an absent id as
//     "loading session %q"; the Manager path returned ErrNotSession and wrapped
//     absence as "getting session". The bridge maps ErrSessionNotFound back to
//     ErrNotSession so the API mapping keeps its 400 (vs a 500 fall-through);
//     absence stays on the beads.ErrNotFound chain (→ 404) either way.
//  2. Empty-type heal: loadSessionBead called RepairEmptyType (a type-only write
//     when the bead lost its type). Store.GetPersistedResponse omits it. The heal
//     is re-issued here, conditionally and byte-equivalently — RepairType writes
//     only when info.Type is empty, exactly as RepairEmptyType did — so a
//     type-lost session bead is still healed on a GET, not just on the reconciler
//     tick. The re-fetch handlers (rename/patch/permission-mode) already healed
//     the bead before calling this, so info.Type is set there and no write fires.
//
// The ACP routing side effect loadSessionBead performed is preserved by
// EnrichInfo, which routes ACP itself; nothing is silently dropped.
func sessionGetEnriched(sessFront *session.Store, mgr *session.Manager, id string) (session.Info, session.PersistedResponse, error) {
	info, pr, err := sessFront.GetPersistedResponse(id)
	if err != nil {
		return session.Info{}, session.PersistedResponse{}, bridgeSessionGetError(id, err)
	}
	if info.Type == "" {
		sessFront.RepairTypeBestEffort(id)
		info.Type = session.BeadType
	}
	info = mgr.EnrichInfo(info)
	if ep, epErr := sessFront.LoadStartupHealthEpisode(info.SessionName); epErr == nil {
		pr.Metadata = overlayStartupHealthMetadata(pr.Metadata, ep)
	}
	return info, pr, nil
}

// overlayStartupHealthMetadata returns metadata with the startup-health
// episode's consecutive-count and quarantined-until fields overlaid, so
// session.LifecycleDisplayReasonWithLiveness (the metadata-based twin
// sessionResponseWithReason calls) can render "start-failing (N)" for a
// quarantined session. Never mutates metadata — a PersistedResponse row can be
// a cached/shared value (ListAllWithResponses' CacheFirst tier) — so it
// returns metadata unchanged for the common case (ep is the zero episode:
// never failed to start, or recovered) and only allocates a fresh copy when
// there is something to add.
func overlayStartupHealthMetadata(metadata map[string]string, ep session.StartupHealthEpisode) map[string]string {
	if ep.QuarantinedUntil.IsZero() {
		return metadata
	}
	merged := make(map[string]string, len(metadata)+2)
	for k, v := range metadata {
		merged[k] = v
	}
	merged[session.StartupHealthConsecutiveMetadataKey] = strconv.Itoa(ep.ConsecutiveCount)
	merged[session.StartupHealthQuarantinedUntilMetadataKey] = ep.QuarantinedUntil.Format(time.RFC3339)
	return merged
}

// startupHealthEpisodesByName bulk-loads every persisted startup-health
// episode keyed by session name, for the list paths' per-row overlay
// (both the legacy handleSessionList and the Huma-typed
// humaHandleSessionList). Uses the cache-first peek (ListStartupHealthEpisodesCacheFirst)
// so this per-request overlay does not add a backing-store List call beside
// the session union's own cache-first read. Best effort: a lookup failure
// yields a nil map (no "start-failing" reasons shown, since a nil map read
// returns the zero episode) rather than failing the whole session list — the
// same tolerance RepairTypeBestEffort applies to this class of supplementary
// read.
func startupHealthEpisodesByName(sessFront *session.Store) map[string]session.StartupHealthEpisode {
	episodes, err := sessFront.ListStartupHealthEpisodesCacheFirst()
	if err != nil {
		return nil
	}
	byName := make(map[string]session.StartupHealthEpisode, len(episodes))
	for _, ep := range episodes {
		byName[ep.SessionName] = ep
	}
	return byName
}

// bridgeSessionGetError maps a session.Store persisted-read error to the error
// contract the API session-manager mappers (writeSessionManagerError /
// humaSessionManagerError) expect from the old Manager.GetWithPersistedResponse,
// preserving the status codes. A present-but-non-session bead swaps
// ErrSessionNotFound for ErrNotSession (→ 400); every other error (including the
// beads.ErrNotFound-chained absence that yields 404) passes through unchanged.
func bridgeSessionGetError(id string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, session.ErrSessionNotFound) && !errors.Is(err, beads.ErrNotFound) {
		return fmt.Errorf("%w: %s", session.ErrNotSession, id)
	}
	return err
}
