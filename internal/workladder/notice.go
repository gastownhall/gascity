package workladder

import (
	"slices"
	"time"
)

// NoticeMark is the asynchronous delivery an entry's last nudge was handed to
// (W5: a conversation-lane notice op), until the caller settles it. While an
// entry carries a mark, Decide holds it (no nudge, recycle, release or alert
// for it) and the mark takes the seat's nudge slot, so the seat has at most
// one delivery outstanding and attempt n+1 waits until attempt n is settled.
type NoticeMark struct {
	Op     string    // the delivery's id: opaque here
	At     time.Time // when the nudge was reserved (the entry's Last then)
	Paused bool      // the nudge was paused (a degraded provider): it counted nothing
}

// IsZero reports whether m marks nothing.
func (m NoticeMark) IsZero() bool { return m.Op == "" }

// NoticeFate is what became of a marked nudge's delivery.
type NoticeFate uint8

// Notice fates.
const (
	// NoticeDelivered: it reached the seat. It counts, and the seat's
	// refusals end.
	NoticeDelivered NoticeFate = iota + 1
	// NoticeFailed: it failed, perhaps after reaching the seat. It counts,
	// as a delivery error does.
	NoticeFailed
	// NoticeRefused: the seat could not take it within its window, for a
	// cause the caller found refuses a nudge (W3's: a pending interaction,
	// the runtime unavailable). It is re-counted as paused, and the third
	// refusal in a row raises the nudge_refused alert, repeated while the
	// refusals go on (NudgeRefused's rule).
	NoticeRefused
	// NoticeWithdrawn: it was canceled or skipped before delivery. It is
	// re-counted as paused, with no refusal.
	NoticeWithdrawn
)

// MarkNotice is r with key's entry marked m. ok is false when r holds no
// entry for key.
func MarkNotice(r Record, key ClaimKey, m NoticeMark) (Record, bool) {
	i := slices.IndexFunc(r.Entries, func(e Entry) bool { return e.Key == key })
	if i < 0 {
		return r, false
	}
	r.Entries = slices.Clone(r.Entries)
	r.Entries[i].Notice = m
	return r, true
}

// SettleNotice is r after the delivery op ended with fate: the mark cleared
// and, only within the episode the nudge was reserved in (the entry's Since
// not after the mark's At), a counted nudge re-counted per fate. A paused
// mark records nothing beyond the clear, and a delivered one's end of the
// refusals. alert reports a refusal that raises the nudge_refused alert
// (refusalAlerts: the third in a row, then periodically). ok is false when
// no entry carries the mark.
func SettleNotice(r Record, op string, fate NoticeFate) (out Record, alert, ok bool) {
	if op == "" {
		return r, false, false
	}
	i := slices.IndexFunc(r.Entries, func(e Entry) bool { return e.Notice.Op == op })
	if i < 0 {
		return r, false, false
	}
	r.Entries = slices.Clone(r.Entries)
	e := &r.Entries[i]
	m := e.Notice
	e.Notice = NoticeMark{}
	if e.Since.After(m.At) {
		return r, false, true // a later episode: its counts are its own
	}
	switch {
	case fate == NoticeDelivered:
		e.Refused = 0
	case m.Paused:
	case fate == NoticeRefused:
		alert = recountPaused(e, true)
	case fate == NoticeWithdrawn:
		recountPaused(e, false)
	}
	return r, alert, true
}

// recountPaused moves e's last counted nudge to its paused count, as a nudge
// that never reached the seat; refused also counts it a refusal. It reports
// whether the refusals just reached a count that alerts (refusalAlerts).
func recountPaused(e *Entry, refused bool) bool {
	if e.Attempts > 0 {
		e.Attempts--
	}
	e.Paused++
	if !refused {
		return false
	}
	e.Refused++
	return refusalAlerts(e.Refused)
}

// anyMarked reports whether an entry of r carries a notice mark.
func anyMarked(r Record) bool {
	return slices.ContainsFunc(r.Entries, func(e Entry) bool { return !e.Notice.IsZero() })
}
