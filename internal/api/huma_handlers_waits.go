package api

import (
	"context"
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// Handlers for the durable-wait wire (GET /v0/city/{cityName}/waits and
// /wait/{id}). Both read through session.Store over SessionsBeadStore(), so a
// [beads.classes.sessions] relocation serves relocated wait beads that the
// generic ListBeads(label=gc:wait) leg (which reads CityBeadStore/BeadStores())
// would miss. Bead serialization is confined to session.Store + waitViewFromInfo.

// humaHandleWaitList serves GET /v0/city/{cityName}/waits?state=&session=. The
// list is created-DESC (the CLI applies its own stable ascending sort); a capped
// lookup surfaces the truncation via body.capped rather than an error.
func (s *Server) humaHandleWaitList(_ context.Context, input *WaitListInput) (*WaitListOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	if err := cacheLiveOr503(store.Store); err != nil {
		return nil, err
	}
	waits, err := session.NewStore(store).ListWaits(input.State, input.Session)
	out := &WaitListOutput{CacheAgeS: cacheAgeSeconds(store.Store)}
	if err != nil {
		switch {
		case beads.IsLookupLimitError(err):
			out.Body.Capped = true
		case beads.IsPartialResult(err):
			// A degraded store read carried the surviving rows through ListWaits.
			// Mirror the generic /beads contract: answer 200 with the reachable
			// waits plus partial metadata rather than 500-ing and hiding them.
			out.Body.Partial = true
			out.Body.PartialErrors = []string{err.Error()}
		default:
			return nil, humaStoreError(err)
		}
	}
	out.Body.Waits = make([]WaitView, 0, len(waits))
	for _, w := range waits {
		out.Body.Waits = append(out.Body.Waits, waitViewFromInfo(w))
	}
	return out, nil
}

// humaHandleWaitGet serves GET /v0/city/{cityName}/wait/{id}. A missing wait maps
// to a wait-not-found 404 ("not_found: <id>"); a bead that exists but is not a
// durable wait maps to the same wait-not-found type with a machine-matchable
// "not_a_wait: <id>" 404 detail the CLI branches on.
func (s *Server) humaHandleWaitGet(_ context.Context, input *WaitGetInput) (*WaitGetOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	if err := cacheLiveOr503(store.Store); err != nil {
		return nil, err
	}
	w, err := session.NewStore(store).GetWait(input.ID)
	if err != nil {
		if errors.Is(err, session.ErrNotAWait) {
			return nil, apierr.WaitNotFound.Msg("not_a_wait: " + input.ID)
		}
		// A missing wait id surfaces the store's wrapped beads.ErrNotFound; map it
		// to wait-not-found (not session-not-found, which humaStoreError would emit)
		// so RFC 9457 consumers see the correct missing-resource type.
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.WaitNotFound.Msg("not_found: " + input.ID)
		}
		return nil, humaStoreError(err)
	}
	out := &WaitGetOutput{CacheAgeS: cacheAgeSeconds(store.Store)}
	out.Body = waitViewFromInfo(w)
	return out, nil
}

// waitViewFromInfo projects a session.WaitInfo onto its wire view. CreatedAt is
// carried at RFC3339Nano (full precision, UTC), not RFC3339: the CLI still
// renders it at second precision via formatOptionalTime, so the emitted
// created_at string is unchanged, but the sort key the CLI parses back
// (sort.SliceStable on CreatedAt) keeps sub-second precision. Otherwise two
// waits created within the same second on a nanosecond backend (mem/file store)
// would render in a different row order on this typed rung than on the legacy
// (/beads, RFC3339Nano) and local (raw time.Time) rungs, breaking the
// byte-identical-across-rungs contract. A zero time still maps to "".
func waitViewFromInfo(w session.WaitInfo) WaitView {
	created := ""
	if !w.CreatedAt.IsZero() {
		created = w.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return WaitView{
		ID:              w.ID,
		SessionID:       w.SessionID,
		SessionName:     w.SessionName,
		Kind:            w.Kind,
		State:           w.State,
		DepIDs:          w.DepIDs,
		DepMode:         w.DepMode,
		RegisteredEpoch: w.RegisteredEpoch,
		DeliveryAttempt: w.DeliveryAttempt,
		NudgeID:         w.NudgeID,
		ExpiresAt:       w.ExpiresAt,
		Note:            w.Note,
		Status:          w.Status,
		CreatedAt:       created,
		Labels:          w.Labels,
	}
}
