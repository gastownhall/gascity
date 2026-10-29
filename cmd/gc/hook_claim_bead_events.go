package main

// bead.* announcement for the writes `gc hook --claim` makes to a work store.
//
// The hook claims a step from a one-shot worker process by writing the agent's
// work store directly (a bd subprocess, hookClaimBdStoreContext). Nothing in
// that path records a bead.* event, and the supervisor's run views
// (GET /runs, /runs/{id}, /runs/{id}/steps) are a fold of exactly those events
// in <city>/.gc/events.jsonl. So a claimed step stayed pending with no assignee
// in the run views until the supervisor happened to read the row back: a live
// read that refetched it, or its cache reconcile 30 to 120 seconds later.
//
// Every successful work-store write the hook makes is therefore announced with
// the bead.updated the supervisor's CachingStore would have recorded for it,
// built by the one-shot emitter the relocated class stores already use
// (emittingClassStore): the canonical snapshot from beads.EncodeBeadEventPayload,
// the run/session/step correlation on the envelope, and an explicit edge set so
// the supervisor's cache, which applies the event too, does not read it as
// edge coverage unknown. The run tailer folds it on its next poll, about a
// second later.
//
// The writes covered are the claim CAS (fresh claims and ready-assignment
// promotions), the execution-identity stamp, continuation preassignment, the
// adoption re-stamp, the undelivered-claim release and the stale-lease reclaim.
// A write through the relocated class binding (claim_class_route.go) is not
// announced here: that store is an emittingClassStore and announces itself.
//
// # When the announcement runs
//
// Not inside the write. The claim tier stamps the instant the CAS returns and
// decides from it whether the landed claim is kept (claimLandingWindowSpent),
// and a stale-lease reclaim runs before the claim's start fence
// (claimWindowSpent). Store reads made inside those writes would spend the
// window the fences measure, and on a slow store could turn a claim that
// landed in time into a straddle unwind. So each write only NOTES what it
// changed, and the notes are announced together: by
// writeHookClaimWorkResultForBead once the claim is kept and stamped, before
// its result line is written (so the worker cannot close the step before the
// claim is in the log), and by claimHookWork on the way out for whatever is
// left (a release, a reclaim, a claim that stopped on an error).
//
// A note carries the post-write row when it can be derived from rows already in
// hand: the CAS's canonical readback, updated by the stamp's metadata patch or
// the re-stamp's assignee, or a preassigned sibling's listed row with its new
// assignee. Only a row nothing derives (a release, a reclaim, a stamp of an
// adopted bead) is read back, all in one batched read, and the edges of rows
// that carry none are confirmed in one more. Those reads share a small budget
// of their own (hookClaimAnnounceBudget).
//
// Recording is best-effort. The writes have already committed, so a log that
// cannot be opened, a read that fails or runs out of budget, or an append that
// times out on the log's lock leaves them in place and is reported once on
// stderr (warnClassStoreEmit); the supervisor's read-back still catches the
// change up as before. The append goes through the city's secondary
// FileRecorder (openCityEventsLog), which serializes it with the supervisor's
// own appends under the log's cross-process lock and allocates its seq there,
// so the supervisor does not need to be running.
//
// A repeat is harmless. The supervisor may announce the same change when it
// reads the row back; the run fold is last-snapshot-wins per bead, so a second
// identical snapshot leaves the projection where the first put it.

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// hookClaimAnnounceBudget bounds all the store reads one announcement pass
// makes. A pass that runs out drops what it could not read, never retries: the
// writes it describes have already landed.
var hookClaimAnnounceBudget = 3 * time.Second

// withHookClaimBeadEvents returns ops whose work-store writes are announced to
// cityPath's event log, and whose AnnounceBeadEvents announces the writes noted
// so far. cityPath is the city whose supervisor projects these stores: every
// leg the hook federates (the agent's own store, its rigs, the city store)
// belongs to it, and the supervisor records every one of their caches into
// this one log.
//
// It wraps the seams applyDefaults resolves, so it must run before
// classRoutedHookClaimOps: the class route escalates to the binding only
// after a work-store write fails, and a failed write notes nothing.
func withHookClaimBeadEvents(ops hookClaimOps, cityPath string) hookClaimOps {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath == "" {
		return ops
	}
	ops.applyDefaults()
	base := ops
	a := &hookClaimAnnouncer{cityPath: cityPath}
	setAssignee := func(assignee string) func(*beads.Bead) *beads.Bead {
		return func(prev *beads.Bead) *beads.Bead {
			if prev == nil {
				return nil
			}
			next := *prev
			next.Assignee = assignee
			return &next
		}
	}

	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		claimed, ok, err := base.Claim(ctx, dir, env, beadID, assignee)
		if ok {
			// A committed claim whose canonical readback failed (ok with err)
			// is still a committed claim; its row is read back when announced.
			var row *beads.Bead
			if err == nil && strings.TrimSpace(claimed.ID) == beadID {
				row = &claimed
			}
			a.note(dir, env, beadID, func(*beads.Bead) *beads.Bead { return row })
		}
		return claimed, ok, err
	}
	ops.StampWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
		err := base.StampWorkMeta(ctx, dir, env, beadID, assignee, patch)
		if err == nil {
			a.note(dir, env, beadID, func(prev *beads.Bead) *beads.Bead {
				if prev == nil {
					return nil
				}
				next := *prev
				next.Metadata = make(map[string]string, len(prev.Metadata)+len(patch))
				maps.Copy(next.Metadata, prev.Metadata)
				maps.Copy(next.Metadata, patch)
				return &next
			})
		}
		return err
	}
	ops.ListContinuation = func(ctx context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
		siblings, err := base.ListContinuation(ctx, dir, env, rootID, group)
		if err == nil {
			a.remember(dir, env, siblings)
		}
		return siblings, err
	}
	ops.AssignContinuation = func(ctx context.Context, dir string, env []string, beadID, assignee string) error {
		err := base.AssignContinuation(ctx, dir, env, beadID, assignee)
		if err == nil {
			a.note(dir, env, beadID, setAssignee(assignee))
		}
		return err
	}
	ops.RestampAdopted = func(ctx context.Context, dir string, env []string, beadID, fromAssignee, toAssignee string) (bool, error) {
		moved, err := base.RestampAdopted(ctx, dir, env, beadID, fromAssignee, toAssignee)
		if moved && err == nil {
			a.note(dir, env, beadID, setAssignee(toAssignee))
		}
		return moved, err
	}
	ops.Release = func(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
		released, err := base.Release(ctx, dir, env, beadID, assignee)
		if released && err == nil {
			a.note(dir, env, beadID, nil)
		}
		return released, err
	}
	ops.ReclaimStale = func(ctx context.Context, dir string, env []string, beadID string) (bool, string, error) {
		reclaimed, previousOwner, err := base.ReclaimStale(ctx, dir, env, beadID)
		if reclaimed && err == nil {
			a.note(dir, env, beadID, nil)
		}
		return reclaimed, previousOwner, err
	}
	ops.AnnounceBeadEvents = a.announce
	return ops
}

// announceBeadEvents announces every write noted so far, if these ops note any.
func (ops *hookClaimOps) announceBeadEvents() {
	if ops.AnnounceBeadEvents != nil {
		ops.AnnounceBeadEvents()
	}
}

// hookClaimAnnouncer collects the hook's landed work-store writes, one note per
// bead per store, until they are announced.
type hookClaimAnnouncer struct {
	cityPath string

	mu      sync.Mutex
	pending []*hookClaimNote
	byKey   map[string]*hookClaimNote
	// listed holds continuation rows a list returned, so a preassignment of
	// one derives its post-write row instead of reading it back.
	listed map[string]beads.Bead
}

// hookClaimNote is one bead whose writes are not announced yet. row is its
// post-write state when every write to it so far could be derived, and nil
// when it must be read back.
type hookClaimNote struct {
	dir string
	env []string
	id  string
	row *beads.Bead
}

func hookClaimStoreKey(dir string, env []string) string {
	return dir + "\x00" + strings.Join(env, "\x00")
}

// remember records rows a read returned, as the base a later write derives from.
func (a *hookClaimAnnouncer) remember(dir string, env []string, rows []beads.Bead) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listed == nil {
		a.listed = map[string]beads.Bead{}
	}
	store := hookClaimStoreKey(dir, env)
	for _, row := range rows {
		if id := strings.TrimSpace(row.ID); id != "" {
			a.listed[store+"\x00"+id] = row
		}
	}
}

// note records a landed write to beadID. derive maps the bead's last known row
// (nil when unknown) to its post-write row, or to nil when the write cannot be
// derived from it; a nil derive marks a write that never can be.
func (a *hookClaimAnnouncer) note(dir string, env []string, beadID string, derive func(*beads.Bead) *beads.Bead) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byKey == nil {
		a.byKey = map[string]*hookClaimNote{}
	}
	key := hookClaimStoreKey(dir, env) + "\x00" + beadID
	n, ok := a.byKey[key]
	if !ok {
		n = &hookClaimNote{dir: dir, env: append([]string(nil), env...), id: beadID}
		if row, listed := a.listed[key]; listed {
			n.row = &row
		}
		a.byKey[key] = n
		a.pending = append(a.pending, n)
	}
	if derive == nil {
		n.row = nil
		return
	}
	n.row = derive(n.row)
}

// announce records a bead.updated for every noted bead, store by store, and
// forgets the notes. A row that has to be read back is read through the bd
// context of the store the write went to.
func (a *hookClaimAnnouncer) announce() {
	a.mu.Lock()
	pending := a.pending
	a.pending, a.byKey = nil, nil
	a.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimAnnounceBudget)
	defer cancel()
	var order []string
	byStore := map[string][]*hookClaimNote{}
	for _, n := range pending {
		key := hookClaimStoreKey(n.dir, n.env)
		if _, seen := byStore[key]; !seen {
			order = append(order, key)
		}
		byStore[key] = append(byStore[key], n)
	}
	for _, key := range order {
		notes := byStore[key]
		store := hookClaimBdStoreContext(ctx, notes[0].dir, notes[0].env, "")
		emitter := &emittingClassStore{Store: store, cityPath: a.cityPath}
		rows := announcedRows(emitter, notes)
		emissions := make([]classStoreEmission, 0, len(rows))
		for _, bead := range rows {
			emissions = append(emissions, classStoreEmission{eventType: events.BeadUpdated, bead: bead})
		}
		emitter.emit(emissions...)
	}
}

// announcedRows returns the post-write rows of notes, in order: derived rows as
// noted, the rest read back in one batch, every row's edges confirmed.
func announcedRows(emitter *emittingClassStore, notes []*hookClaimNote) []beads.Bead {
	var unread []string
	for _, n := range notes {
		if n.row == nil {
			unread = append(unread, n.id)
		}
	}
	read := readBackRows(emitter, unread, events.BeadUpdated)
	rows := make([]beads.Bead, 0, len(notes))
	for _, n := range notes {
		if n.row != nil {
			rows = append(rows, *n.row)
		} else if row, ok := read[n.id]; ok {
			rows = append(rows, row)
		}
	}
	return withConfirmedEdgesBatch(emitter, rows)
}
