package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// NudgeFramingBracketedPaste names the only framing gc uses to put a nudge or
// an injected reminder into an agent TUI: the whole payload is handed to the
// terminal inside one bracketed-paste, so the TUI consumes it as a single
// paste rather than as a stream of individual keypresses.
//
// The framing is not cosmetic. Typed as raw keys, a payload's embedded
// newlines are ordinary Enter presses: a busy TUI submits whatever it has
// buffered at the first one and drops the rest, which is exactly how four
// founder messages arrived as tails on 2026-08-27/28 (pc_2e2378b9918e).
// Recording the framing on the receipt is what lets a downstream consumer tell
// a delivery that could not have been split from one that could.
const NudgeFramingBracketedPaste = "bracketed_paste"

// NudgeReceipt is evidence about ONE nudge's delivery into a session's
// terminal. It answers the question a sender actually has — "did my COMPLETE
// payload reach the agent?" — which "the send call returned no error" does
// not: an errorless raw key burst is precisely the failure mode that
// truncates.
//
// A receipt is only ever created after the payload has been framed and handed
// to the terminal, so its existence is the delivery claim and its fields are
// the evidence behind it. Consumers that must not double-deliver (the Slack
// adapter's same-ts twin dedup, gp-32q) gate on the receipt rather than on a
// nil error.
type NudgeReceipt struct {
	// ID is unique per delivery, so two deliveries of identical text are
	// distinguishable. Digest is not: it is content-addressed on purpose.
	ID string `json:"id"`
	// Target is the tmux session or pane the payload was framed into.
	Target string `json:"target"`
	// Bytes is the payload length actually written into the paste buffer —
	// the number a caller compares against what it handed in to prove
	// nothing was dropped before the terminal.
	Bytes int `json:"bytes"`
	// Digest is NudgePayloadDigest of the payload. Any layer holding the same
	// payload can compute it without plumbing, so a caller several layers
	// above the terminal can still name the delivery it is waiting on.
	Digest string `json:"digest"`
	// Framing is how the payload entered the terminal; see
	// NudgeFramingBracketedPaste.
	Framing string `json:"framing"`
	// Submitted reports whether the agent was observed to accept the submit
	// (it went busy). False means the text was framed and delivered but the
	// submit was not confirmed — for providers with no reliable busy
	// indicator this is the normal best-effort outcome, NOT evidence of
	// failure. Never read it as "the payload was truncated".
	Submitted bool `json:"submitted"`
	// At is when the receipt was issued (after the last keystroke).
	At time.Time `json:"at"`
}

// NudgePayloadDigest returns the stable short content address of a nudge
// payload: the first 16 hex characters of its SHA-256.
//
// Truncated to 16 chars because this is a correlation handle inside one city's
// logs, not a security boundary — 64 bits is far past collision risk for the
// handful of in-flight nudges a session ever has, and a short id stays
// readable in a log line an operator greps.
func NudgePayloadDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// String renders the receipt as one stable, greppable log field set. The
// leading "nudge-receipt" token is the anchor consumers match on; keep it and
// the key=value shape stable.
func (r NudgeReceipt) String() string {
	return fmt.Sprintf("nudge-receipt id=%s target=%q bytes=%d digest=%s framing=%s submitted=%t",
		r.ID, r.Target, r.Bytes, r.Digest, r.Framing, r.Submitted)
}

// NudgeReceiptSink receives every receipt the runtime issues. A nil sink means
// the runtime's own default (a log line); an installed sink replaces it, so a
// sink must not panic and should not block — it runs on the delivery path.
type NudgeReceiptSink func(NudgeReceipt)
