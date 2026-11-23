package legacyop

import (
	"errors"
	"slices"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// How a legacy path carried out an op, the outcome's result.via.
const (
	ViaLegacy          = "legacy"
	ViaLegacyDelivered = "legacy.delivered"
	ViaLegacyQueued    = "legacy.queued"
	ViaLegacyRollback  = "legacy.rollback"
	// ViaLegacyDeleted is a close that also deleted its session's bead.
	ViaLegacyDeleted = "legacy.deleted"
)

// Refusal reasons the classifiers write after refused:.
const (
	RefusedNotASession = "not_a_session"
)

// Refusal is a legacy sentinel that refuses a sync verb for a reason a retry
// can change: it closes failed{refused:<Why>} on a minted key and leaves a
// client-keyed op open.
type Refusal struct {
	Err error
	Why string
}

// Sentinels maps the legacy paths' typed sentinel errors onto op outcomes
// (DES-O0API §6.3 and §7.2). The sentinels live above this package's import
// boundary (internal/session, internal/runtime), so producers fill each slot
// with theirs; the classifiers match with errors.Is only, never on text.
type Sentinels struct {
	// Unsupported closes failed{unsupported}.
	Unsupported []error
	// Stale closes skipped{stale} (sync verbs).
	Stale []error
	// NotNeeded closes a kill skipped{not_needed}.
	NotNeeded []error
	// NotASession closes failed{refused:not_a_session} (sync verbs).
	NotASession []error
	// ConversationClosed closes a message skipped{conversation_closed}.
	ConversationClosed []error
	// SessionGone closes a message failed{session_gone}.
	SessionGone []error
	// ProviderUnavailable is failed{provider_unavailable}; retryable on
	// sync verbs.
	ProviderUnavailable []error
	// DeliveryTimeout closes a message failed{deadline_in_flight}.
	DeliveryTimeout []error
	// NoBoundary is failed{no_boundary}, retryable (a stop that sent but
	// saw no boundary).
	NoBoundary []error
	// Refusals are retryable failed{refused:<why>} answers.
	Refusals []Refusal
}

// MessageResult maps an async message delivery's legacy result: delivered
// (queued false) or queued, else the error's outcome. As in SyncResult, the
// definitive answers are consulted first, then a Refusals match
// (failed{refused:<why>}), then the retryable ones: an error that joins a
// definitive failure to a refusal (a rollback that met a busy queue) reports
// the definitive one. A delivery timeout is definitive (the message may have
// been delivered) unless the error also says the runtime was unavailable: a
// wait for an unavailable runtime that ended on a deadline never sent the
// message, so it is provider_unavailable. Message ops always close
// (DES-O0API D10).
func (s Sentinels) MessageResult(queued bool, err error) Result {
	switch {
	case err == nil && queued:
		return Result{Outcome: succeeded(ViaLegacyQueued)}
	case err == nil:
		return Result{Outcome: succeeded(ViaLegacyDelivered)}
	case matches(err, s.Unsupported):
		return Result{Outcome: failed(opjournal.ReasonUnsupported)}
	case matches(err, s.ConversationClosed):
		return Result{Outcome: skipped(opjournal.ReasonConversationClosed)}
	case matches(err, s.SessionGone):
		return Result{Outcome: failed(opjournal.ReasonSessionGone)}
	case matches(err, s.DeliveryTimeout) && !matches(err, s.ProviderUnavailable):
		return Result{Outcome: failed(opjournal.ReasonDeadlineInFlight)}
	}
	if why, ok := s.refusal(err); ok {
		return Result{Outcome: failed(opjournal.RefusedReasonPrefix + why)}
	}
	if matches(err, s.ProviderUnavailable) {
		return Result{Outcome: failed(opjournal.ReasonProviderUnavailable)}
	}
	return Result{Outcome: failed(opjournal.ReasonOther)}
}

// SyncResult maps a synchronous verb's legacy result. Success is
// succeeded{via: legacy} (a reset that rolled back a pending create sets
// ViaLegacyRollback itself). Definitive answers close; a retryable error
// leaves a client-keyed op open, so a same-key retry before its finish_by
// adopts and re-executes, and closes a minted-key op with its outcome
// (DES-O0API D10). LeaveOpen is a request: Finish honors it only on a surface
// that adopts (the API).
func (s Sentinels) SyncResult(kind opjournal.Kind, err error, minted bool) Result {
	switch {
	case err == nil:
		return Result{Outcome: succeeded(ViaLegacy)}
	case kind == opjournal.KindKill && matches(err, s.NotNeeded):
		return Result{Outcome: skipped(opjournal.ReasonNotNeeded)}
	case matches(err, s.Unsupported):
		return Result{Outcome: failed(opjournal.ReasonUnsupported)}
	case matches(err, s.Stale):
		return Result{Outcome: skipped(opjournal.ReasonStale)}
	case matches(err, s.NotASession):
		return Result{Outcome: failed(opjournal.RefusedReasonPrefix + RefusedNotASession)}
	}
	outcome := failed(opjournal.ReasonOther)
	why, refused := s.refusal(err)
	switch {
	case refused:
		outcome = failed(opjournal.RefusedReasonPrefix + why)
	case matches(err, s.NoBoundary):
		outcome = failed(opjournal.ReasonNoBoundary)
	case matches(err, s.ProviderUnavailable):
		outcome = failed(opjournal.ReasonProviderUnavailable)
	}
	return Result{Outcome: outcome, LeaveOpen: !minted}
}

// refusal returns the reason of the first Refusals entry err matches.
func (s Sentinels) refusal(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	i := slices.IndexFunc(s.Refusals, func(r Refusal) bool { return r.Err != nil && errors.Is(err, r.Err) })
	if i < 0 {
		return "", false
	}
	return s.Refusals[i].Why, true
}

func matches(err error, sentinels []error) bool {
	return slices.ContainsFunc(sentinels, func(s error) bool { return s != nil && errors.Is(err, s) })
}

func succeeded(via string) opjournal.Outcome {
	return opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: via}}
}

func failed(reason string) opjournal.Outcome {
	return opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: reason}
}

func skipped(reason string) opjournal.Outcome {
	return opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: reason}
}
