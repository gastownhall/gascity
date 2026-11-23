package coordclass

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// The coordination-class marks CoordinationOwned matches beyond those
// Classify already mirrors. Canonical: session.WaitBeadType and
// session.LegacyWaitBeadType (guard_test pins them), and
// internal/orders labelOrderRunPrefix (pinned by classify_test's golden row).
const (
	typeWait       = "gate"
	typeWaitLegacy = "wait"
	// labelOrderRunPrefix keys an order's runs: the orders store's open-work
	// and last-run lookups read it.
	labelOrderRunPrefix = "order-run:"
	// labelSessionPrefix with a wait's session_id marks a wait without the
	// gc:wait label (session.IsWaitBead).
	labelSessionPrefix = "session:"
	// metaSessionID is the metadata key naming the session a wait belongs to
	// (session.IsWaitBead).
	metaSessionID = "session_id"
)

// CoordinationOwned reports a coordination-class bead only its owning
// routes write (architecture B v2, mc-v8g4p): a mail message (/mail), a
// session wait (gc session wait and the controller's waits step), a queued
// nudge or its shadow (the nudge queue), an order's tracking (order
// dispatch), or an external-messaging row (/extmsg/*). The generic bead API
// writes none, on either side of a write, as with ControllerOwned. Type and
// label are matched whatever their case and surrounding space. Wisps stay
// generic: their writes are workflow-step work.
func CoordinationOwned(b beads.Bead) bool {
	return CoordinationOwnedType(b.Type) || CoordinationOwnedLabel(b.Type, b.Labels, b.Metadata) >= 0
}

// CoordinationOwnedType reports a type that alone makes a bead
// CoordinationOwned: a mail message.
func CoordinationOwnedType(typ string) bool {
	return strings.EqualFold(strings.TrimSpace(typ), typeMessage)
}

// CoordinationOwnedLabel is the index of the first label that makes a bead of
// type typ and metadata meta CoordinationOwned, or -1: gc:wait, gc:nudge,
// gc:nudge-queue, order-tracking, an order-run: or gc:extmsg- label, or, on a
// wait-typed bead (gate, wait), the session:<session_id> label that marks a
// wait without gc:wait.
func CoordinationOwnedLabel(typ string, labels []string, meta map[string]string) int {
	t := strings.TrimSpace(typ)
	waitTyped := strings.EqualFold(t, typeWait) || strings.EqualFold(t, typeWaitLegacy)
	sessionID := strings.TrimSpace(meta[metaSessionID])
	for i, raw := range labels {
		l := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case l == labelWait, l == labelNudge, l == labelNudgeQueue, l == labelOrderTracking,
			strings.HasPrefix(l, labelOrderRunPrefix), strings.HasPrefix(l, labelExtmsgPrefix):
			return i
		case waitTyped && sessionID != "" && l == strings.ToLower(labelSessionPrefix+sessionID):
			return i
		}
	}
	return -1
}
