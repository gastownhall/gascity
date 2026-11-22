package nudgequeue

import (
	"sort"
	"time"
)

// ReferenceKindStartupTurn is the Reference kind of a startup turn: the first
// turn a runtime handed back from a start instead of typing it, queued as a
// deferred submit (SourceSubmit) and bound to the launch that produced it.
// Its Reference ID is that launch's id (runtime.LaunchID of the launched
// process's instance token), never the token itself. The ID format is
// frozen: a new format needs a new kind.
//
// A claim judges a session's startup turns against the launch running now
// (JudgeStartupTurns): a turn is delivered only to its own launch, ahead of
// the session's other items, and dropped only on proof that its launch is
// gone. A queue that does not know this kind delivers the turn as the plain
// submit it also is.
const ReferenceKindStartupTurn = "startup_turn"

// The LastError of a startup turn a claim dead-lettered.
const (
	// StaleLaunchReason marks a startup turn whose launch is gone: the
	// session runs another launch now. Its terminal state is failed.
	StaleLaunchReason = "stale-launch"
	// SupersededReason marks a startup turn a newer turn of the same launch
	// replaced. Its terminal state is superseded.
	SupersededReason = "superseded"
)

// StartupTurnReference is the Reference a startup turn of launchID is queued
// under, or nil when launchID is empty: a turn with no launch to bind to is
// queued as a plain submit.
func StartupTurnReference(launchID string) *Reference {
	if launchID == "" {
		return nil
	}
	return &Reference{Kind: ReferenceKindStartupTurn, ID: launchID}
}

// StartupTurnLaunch returns the launch id item is bound to, and whether item
// is a startup turn at all.
func StartupTurnLaunch(item Item) (string, bool) {
	if item.Reference == nil || item.Reference.Kind != ReferenceKindStartupTurn || item.Reference.ID == "" {
		return "", false
	}
	return item.Reference.ID, true
}

// IsStartupTurn reports whether item is a startup turn.
func IsStartupTurn(item Item) bool {
	_, ok := StartupTurnLaunch(item)
	return ok
}

// HasLiveStartupTurn reports whether items, read from a queue at now, hold a
// startup turn of session sessionID that a send made under deliveryKey
// would reach the agent ahead of: one not expired, fenced to sessionID or
// unfenced, whether pending or in flight, and not pinned to deliveryKey. A
// send under the key of the batch a claim pinned a turn to (DeliveryBatch)
// is the queue's delivery of that turn, so the turn does not hold it; an
// empty deliveryKey matches no turn. Any other send typed straight into the
// session would reach its agent ahead of the turn, so the sender queues it
// instead, where the claim's barrier delivers the turn first.
func HasLiveStartupTurn(sessionID, deliveryKey string, items []Item, now time.Time) bool {
	for _, item := range items {
		if !IsStartupTurn(item) || expiredAt(item, now) {
			continue
		}
		if deliveryKey != "" && item.DeliveryBatch == deliveryKey {
			continue
		}
		if item.SessionID == "" || item.SessionID == sessionID {
			return true
		}
	}
	return false
}

// LiveLaunch is what a claim learned about the launch its target runs now.
type LiveLaunch struct {
	// Asked reports that the claim asked. A judgement made before the ask
	// that needs the answer says so (StartupJudgement.NeedsLaunch).
	Asked bool
	// ID is the live launch's id, meaningful only when Known.
	ID string
	// Known reports that the ask named a launch. An unknown launch (an
	// unreadable runtime, no session, no token, or no reader) holds the
	// session's startup turns: none is claimed and none is dropped.
	Known bool
	// Seen holds the ids of the startup turns the claim read before it
	// asked (StartupJudgement.Members of that read). Only those are judged
	// against the answer: a turn first seen after the ask may belong to a
	// launch the answer predates, so it is held.
	Seen map[string]bool
}

// StartupJudgement is what a claim of one target does with the startup turns
// of the target's session.
type StartupJudgement struct {
	// Members are the ids of the session's live startup turns, in the order
	// given.
	Members []string
	// NeedsLaunch reports that a member is due and the live launch was not
	// asked: the claim asks, outside any exclusive section, and judges again.
	NeedsLaunch bool
	// Hold reports that the claim takes none of the target's other items:
	// a startup turn still stands in front of them.
	Hold bool
	// Claim is the id of the startup turn to claim, alone, or empty.
	Claim string
	// Stale are the members whose launch is gone, to dead-letter with
	// StaleLaunchReason whether pending or in flight.
	Stale []string
	// Superseded are the pending members a newer turn of the same launch
	// replaced, to dead-letter with SupersededReason.
	Superseded []string
}

// Drops reports whether the judgement dead-letters anything.
func (j StartupJudgement) Drops() bool { return len(j.Stale) > 0 || len(j.Superseded) > 0 }

// JudgeStartupTurns judges, for a claim at now by the target fenced to
// sessionID, the target's live items: pending holds the ones that read as
// pending (a lapsed lease included) and inFlight the ones under a live lease,
// all addressed to the target's queue keys. The session's startup turns are
// the startup turns among them that are unexpired and fenced to sessionID or
// unfenced; a turn fenced to another session is not judged here, and no claim
// of this target may take it.
//
//   - No turn: the claim runs as it would without startup turns (Hold false).
//   - No turn due (pending with DeliverAfter at or before now): the claim
//     takes nothing and launch is not needed.
//   - Otherwise, before launch is asked, NeedsLaunch: the claim takes nothing
//     until it asks. Once asked, a turn in launch.Seen bound to the known
//     live launch is live, one bound to another launch is stale, and any
//     other turn (unknown launch, or not seen before the ask) is held.
//     Every pending live turn but the newest is superseded. A held turn or a
//     live one in flight leaves the claim nothing; otherwise the newest live
//     turn is claimed alone when it is due.
//   - When neither a live nor a held turn remains, the claim runs over the
//     rest (Hold false): the stale turns no longer stand in front of it.
func JudgeStartupTurns(sessionID string, pending, inFlight []Item, now time.Time, launch LiveLaunch) StartupJudgement {
	type member struct {
		item     Item
		launch   string
		inFlight bool
	}
	var members []member
	var judgement StartupJudgement
	due := false
	collect := func(items []Item, inFlight bool) {
		for _, item := range items {
			id, ok := StartupTurnLaunch(item)
			if !ok || expiredAt(item, now) || (item.SessionID != "" && item.SessionID != sessionID) {
				continue
			}
			members = append(members, member{item: item, launch: id, inFlight: inFlight})
			judgement.Members = append(judgement.Members, item.ID)
			if !inFlight && dueAt(item, now) {
				due = true
			}
		}
	}
	collect(pending, false)
	collect(inFlight, true)
	if len(members) == 0 {
		return judgement
	}
	judgement.Hold = true
	if !due {
		return judgement
	}
	if !launch.Asked {
		judgement.NeedsLaunch = true
		return judgement
	}
	var live []member
	held := false
	for _, m := range members {
		switch {
		case !launch.Known || !launch.Seen[m.item.ID]:
			held = true
		case m.launch == launch.ID:
			live = append(live, m)
		default:
			judgement.Stale = append(judgement.Stale, m.item.ID)
		}
	}
	if len(live) == 0 && !held {
		judgement.Hold = false
		return judgement
	}
	sort.SliceStable(live, func(i, j int) bool { return createdBefore(live[i].item, live[j].item) })
	liveInFlight := false
	for i, m := range live {
		newest := i == len(live)-1
		if m.inFlight {
			liveInFlight = true
			continue
		}
		if !newest {
			judgement.Superseded = append(judgement.Superseded, m.item.ID)
		}
	}
	if held || liveInFlight || len(live) == 0 {
		return judgement
	}
	if newest := live[len(live)-1]; dueAt(newest.item, now) {
		judgement.Claim = newest.item.ID
	}
	return judgement
}

// StartupBarrier is the read-free half of the claim rule, for a reader with no
// target and no runtime (DueKeys): the sessions and queue keys that have a
// startup turn that is not yet claimable, which holds their other items.
type StartupBarrier struct {
	sessions map[string]struct{}
	agents   map[string]struct{}
}

// Add records item when it is an unexpired startup turn that is not yet
// claimable at now: pending before its DeliverAfter, or in flight (inFlight)
// under a live lease. The caller passes every live item it reads.
func (b *StartupBarrier) Add(item Item, inFlight bool, now time.Time) {
	if !IsStartupTurn(item) || expiredAt(item, now) {
		return
	}
	if inFlight {
		if !item.LeaseUntil.After(now) {
			return
		}
	} else if dueAt(item, now) {
		return
	}
	if b.sessions == nil {
		b.sessions, b.agents = map[string]struct{}{}, map[string]struct{}{}
	}
	if item.SessionID != "" {
		b.sessions[item.SessionID] = struct{}{}
	}
	if item.Agent != "" {
		b.agents[item.Agent] = struct{}{}
	}
}

// Holds reports whether item, due, still waits: it is not a startup turn, and
// it shares its SessionID or queue key with a startup turn that is not yet
// claimable. A due startup turn is never held. The rule is conservative: it
// may hold a key the claim itself would deliver.
func (b StartupBarrier) Holds(item Item) bool {
	if b.sessions == nil || IsStartupTurn(item) {
		return false
	}
	if _, ok := b.sessions[item.SessionID]; ok && item.SessionID != "" {
		return true
	}
	_, ok := b.agents[item.Agent]
	return ok && item.Agent != ""
}

// dueAt reports whether a pending item may be claimed at now.
func dueAt(item Item, now time.Time) bool {
	return item.DeliverAfter.IsZero() || !item.DeliverAfter.After(now)
}

// expiredAt reports whether item's deliver-by deadline has passed at now.
func expiredAt(item Item, now time.Time) bool {
	return !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)
}

// createdBefore orders startup turns oldest first.
func createdBefore(left, right Item) bool {
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	return left.ID < right.ID
}
