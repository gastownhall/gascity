package legacyop

import (
	"errors"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// Check runs, in require, the refusals Admit makes before it reads the
// request's conversation: an invalid client key, a payload the journal
// refuses whatever the conversation (over opjournal.MaxInlinePayload, not
// valid UTF-8, or empty for a kind that requires one: see
// opjournal.ValidatePayload) and an op store that does not open. A producer
// that must do something visible before it knows the conversation (the API
// materializing a configured named session, DES-O0API §6.4) checks first, so
// a request require refuses for these reasons has no effect at all. It
// returns the error Admit would (opjournal.ErrInvalidRequest,
// opjournal.ErrPayloadTooLarge, ErrUnavailable), counted and degraded as
// Admit counts and degrades it, and nil in off and auto, which run such a
// request unrecorded. A nil error does not promise Admit will record:
// Propose can still fail.
func (r *Recorder) Check(req Request) error {
	if r.Mode != gate.Require {
		return nil
	}
	if req.Key != "" {
		if err := ValidateKey(req.Kind, req.KeyNamespace, req.Key); err != nil {
			r.count(req.Kind, StageAdmit, ResultRefused, ReasonInvalidKey)
			return err
		}
	}
	if err := opjournal.ValidatePayload(req.Kind, req.Payload); err != nil {
		reason := ReasonInvalidRequest
		if errors.Is(err, opjournal.ErrPayloadTooLarge) {
			reason = ReasonPayloadTooLarge
		}
		r.count(req.Kind, StageAdmit, ResultRefused, reason)
		return err
	}
	if _, err := r.Open(); err != nil {
		_, refused := r.cannotRecord(req.Kind, req.Key, false, StageOpen, openReason(err), err)
		return refused
	}
	return nil
}
