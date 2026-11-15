package legacyop

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
)

var (
	errUnsupported = errors.New("unsupported")
	errStale       = errors.New("no pending interaction")
	errInactive    = errors.New("session inactive")
	errNotSession  = errors.New("not a session")
	errClosed      = errors.New("conversation closed")
	errGone        = errors.New("session gone")
	errUnavailable = errors.New("runtime unavailable")
	errTimeout     = errors.New("delivery timeout")
	errNoBoundary  = errors.New("no boundary")
	errStarting    = errors.New("starting")
)

var testSentinels = Sentinels{
	Unsupported:         []error{errUnsupported},
	Stale:               []error{errStale},
	NotNeeded:           []error{errInactive},
	NotASession:         []error{errNotSession},
	ConversationClosed:  []error{errClosed},
	SessionGone:         []error{errGone},
	ProviderUnavailable: []error{errUnavailable},
	DeliveryTimeout:     []error{errTimeout},
	NoBoundary:          []error{errNoBoundary},
	Refusals:            []Refusal{{Err: errStarting, Why: "session_starting"}},
}

func wrap(err error) error { return fmt.Errorf("handler: %w", err) }

func TestMessageResult(t *testing.T) {
	for _, c := range []struct {
		queued bool
		err    error
		want   opjournal.Outcome
	}{
		{false, nil, succeeded(ViaLegacyDelivered)},
		{true, nil, succeeded(ViaLegacyQueued)},
		{false, wrap(errUnsupported), failed(opjournal.ReasonUnsupported)},
		{false, wrap(errClosed), skipped(opjournal.ReasonConversationClosed)},
		{false, wrap(errGone), failed(opjournal.ReasonSessionGone)},
		{false, wrap(errUnavailable), failed(opjournal.ReasonProviderUnavailable)},
		{false, wrap(errTimeout), failed(opjournal.ReasonDeadlineInFlight)},
		{false, errors.New("unsupported"), failed(opjournal.ReasonOther)}, // same text, not the sentinel
		{false, wrap(errStarting), failed(opjournal.RefusedReasonPrefix + "session_starting")},
		{false, errors.New("starting"), failed(opjournal.ReasonOther)}, // same text, not the refusal
		// A definitive answer outranks a refusal joined to it: the refusal
		// is a side failure (a rollback), not what happened to the message.
		{false, errors.Join(wrap(errGone), wrap(errStarting)), failed(opjournal.ReasonSessionGone)},
		{false, errors.Join(wrap(errClosed), wrap(errStarting)), skipped(opjournal.ReasonConversationClosed)},
		// A refusal outranks a retryable answer, as in SyncResult.
		{false, errors.Join(wrap(errUnavailable), wrap(errStarting)), failed(opjournal.RefusedReasonPrefix + "session_starting")},
		// An unavailable runtime whose wait ended on a deadline (an
		// observation that went unanswered) never sent the message: it is
		// provider_unavailable, not a delivery in flight.
		{false, fmt.Errorf("observation unanswered: %w: %w", errUnavailable, errTimeout), failed(opjournal.ReasonProviderUnavailable)},
		{false, errors.Join(wrap(errTimeout), wrap(errUnavailable)), failed(opjournal.ReasonProviderUnavailable)},
		// A delivery in flight outranks a refusal joined to it: the message
		// may have been delivered.
		{false, errors.Join(wrap(errTimeout), wrap(errStarting)), failed(opjournal.ReasonDeadlineInFlight)},
	} {
		got := testSentinels.MessageResult(c.queued, c.err)
		if got.Outcome != c.want || got.LeaveOpen {
			t.Errorf("MessageResult(%t, %v) = %+v, want %+v closed", c.queued, c.err, got, c.want)
		}
	}
}

func TestSyncResult(t *testing.T) {
	for _, c := range []struct {
		kind      opjournal.Kind
		err       error
		want      opjournal.Outcome
		retryable bool
	}{
		{opjournal.KindWake, nil, succeeded(ViaLegacy), false},
		{opjournal.KindKill, wrap(errInactive), skipped(opjournal.ReasonNotNeeded), false},
		{opjournal.KindSuspend, wrap(errInactive), failed(opjournal.ReasonOther), true},
		{opjournal.KindInterrupt, wrap(errUnsupported), failed(opjournal.ReasonUnsupported), false},
		{opjournal.KindRespond, wrap(errStale), skipped(opjournal.ReasonStale), false},
		{opjournal.KindClose, wrap(errNotSession), failed("refused:not_a_session"), false},
		{opjournal.KindWake, wrap(errStarting), failed("refused:session_starting"), true},
		{opjournal.KindInterrupt, wrap(errNoBoundary), failed(opjournal.ReasonNoBoundary), true},
		{opjournal.KindKill, wrap(errUnavailable), failed(opjournal.ReasonProviderUnavailable), true},
		{opjournal.KindKill, errors.New("store down"), failed(opjournal.ReasonOther), true},
	} {
		for _, minted := range []bool{false, true} {
			got := testSentinels.SyncResult(c.kind, c.err, minted)
			wantOpen := c.retryable && !minted
			if got.Outcome != c.want || got.LeaveOpen != wantOpen {
				t.Errorf("SyncResult(%s, %v, minted %t) = %+v, want %+v leave-open %t", c.kind, c.err, minted, got, c.want, wantOpen)
			}
		}
	}
	if got := (Sentinels{Unsupported: []error{nil}}).SyncResult(opjournal.KindWake, errors.New("x"), true); got.Outcome != failed(opjournal.ReasonOther) {
		t.Errorf("a nil sentinel matched: %+v", got)
	}
}
