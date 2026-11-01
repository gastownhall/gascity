package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// wispGCRefusalRecheck is how long a refused closure (one holding a bead that
// is not closed) is left alone before the purge walks it again, and how long
// the abandoned-root sweep trusts a holder answer, as long as the root has not
// changed. A root whose updated_at moves is re-read at once. Package var so
// tests can shrink it.
var wispGCRefusalRecheck = time.Hour

// wispGCNotes is what one wisp GC tracker remembers across passes so a pass
// neither repeats expensive work nor floods the log:
//
//   - refusals: a closure the purge refused is not walked again until its
//     root changes or wispGCRefusalRecheck passes. On a ledger served by bd
//     every walk is a burst of subprocesses, and a refusal re-walked every
//     pass would cost that burst forever while removing nothing;
//   - holder answers: whether a bead still holds a stepless root, as the
//     abandoned-root sweep's reads found it (wispGCHolders), reused until the
//     root changes or wispGCRefusalRecheck passes, for the same reason;
//   - per-root detail: a pass logs one summary line per arm; the line naming
//     a single root is printed only when that root's state is new since the
//     previous pass (or with GC_DEBUG on), so a stable backlog does not repeat
//     thousands of lines a day.
//
// A nil *wispGCNotes remembers nothing: every refusal is re-walked and every
// detail line printed, which is what a one-off caller wants.
type wispGCNotes struct {
	refused map[string]wispGCRefusal
	// reported is when each per-root line was last printed or seen.
	reported map[string]time.Time
	seen     map[string]struct{}
}

type wispGCRefusal struct {
	updatedAt time.Time
	until     time.Time
	// reason is the outcome last reported for the root; a new refusal with
	// the same reason is not logged again, even after the recheck expired.
	reason string
}

// endPass rolls the per-root detail memo over: what this pass reported is
// what the next pass treats as already said.
func (n *wispGCNotes) endPass() {
	if n == nil {
		return
	}
	n.reported = make(map[string]time.Time, len(n.seen))
	for k := range n.seen {
		n.reported[k] = time.Time{}
	}
	n.seen = nil
}

// endPartialPass is endPass for a pass that reaches only some roots: a line
// seen this pass is remembered as of now, and a line not seen is forgotten only
// once wispGCRefusalRecheck has passed since it was last seen, so a root a
// budgeted pass did not reach is not re-logged on the pass that next reaches
// it.
func (n *wispGCNotes) endPartialPass(now time.Time) {
	if n == nil {
		return
	}
	if n.reported == nil {
		n.reported = make(map[string]time.Time)
	}
	for k := range n.seen {
		n.reported[k] = now
	}
	for k, at := range n.reported {
		if now.Sub(at) > wispGCRefusalRecheck {
			delete(n.reported, k)
		}
	}
	n.seen = nil
}

// detail reports whether the per-root line keyed by key should be printed:
// always under GC_DEBUG, else only when the previous pass did not print it.
func (n *wispGCNotes) detail(key string) bool {
	if n == nil {
		return true
	}
	if n.seen == nil {
		n.seen = make(map[string]struct{})
	}
	n.seen[key] = struct{}{}
	if gcDebugEnabled() {
		return true
	}
	_, said := n.reported[key]
	return !said
}

// refusedNow reports whether root's closure was refused recently enough, and
// root is unchanged since, that the purge should not walk it this pass.
func (n *wispGCNotes) refusedNow(arm string, root beads.Bead, now time.Time) bool {
	_, ok := n.remembered(arm, root, now)
	return ok
}

// remembered returns the reason last recorded for root under arm while it
// still stands: root unchanged since, and wispGCRefusalRecheck not passed.
func (n *wispGCNotes) remembered(arm string, root beads.Bead, now time.Time) (string, bool) {
	if n == nil {
		return "", false
	}
	r, ok := n.refused[arm+"\x00"+root.ID]
	if !ok || !r.updatedAt.Equal(root.UpdatedAt) || !now.Before(r.until) {
		return "", false
	}
	return r.reason, true
}

// noteRefusal records that root was refused at now for reason, and reports
// whether the reason differs from the one last recorded for it (a new outcome,
// worth a log line). A recheck that finds the same outcome is not.
func (n *wispGCNotes) noteRefusal(arm string, root beads.Bead, now time.Time, reason string) (changed bool) {
	if n == nil {
		return true
	}
	if n.refused == nil {
		n.refused = make(map[string]wispGCRefusal)
	}
	key := arm + "\x00" + root.ID
	prev, had := n.refused[key]
	n.refused[key] = wispGCRefusal{updatedAt: root.UpdatedAt, until: now.Add(wispGCRefusalRecheck), reason: reason}
	return !had || prev.reason != reason || gcDebugEnabled()
}

// pruneRefusals drops arm's refusals for roots not in present: roots deleted
// or closed elsewhere, which would otherwise be remembered forever.
func (n *wispGCNotes) pruneRefusals(arm string, present map[string]bool) {
	if n == nil {
		return
	}
	prefix := arm + "\x00"
	for key := range n.refused {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix && !present[key[len(prefix):]] {
			delete(n.refused, key)
		}
	}
}

// forgetRefusal drops root's refusal, after its closure was purged.
func (n *wispGCNotes) forgetRefusal(arm, rootID string) {
	if n == nil {
		return
	}
	delete(n.refused, arm+"\x00"+rootID)
}
