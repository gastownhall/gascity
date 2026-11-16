package main

// The claim-loop guard (ga-ylpbsy.30).
//
// The controller spawns a pool seat when it counts a routed bead as demand, and
// the seat claims its own work. When the claim of that bead fails the same way
// for every seat — a store the claim cannot reach, a write the store refuses —
// the two never converge: the bead stays ready, the controller keeps counting
// it, and each seat it spawns fails the claim, drains claims_errored and is
// replaced, for as long as the bead stays ready. The incident behind this guard
// was one such bead drawing thousands of seats over a day and a half.
//
// The guard is the mechanical backstop for that whole class, whatever the
// failure's cause:
//
//   - Input: bead.claim_failed, which `gc hook --claim` records for each routed
//     candidate it skips because the claim write failed (template, seat, bead,
//     error text), read off the city's event journal by the same feed as the
//     delta lanes. A seat is a session and its runtime epoch, so a session
//     woken again for the same bead counts again.
//   - Trip: claimLoopTripFailures distinct seats of one template failing to
//     claim one bead with the SAME error text, the first and the last within
//     claimLoopWindow. Until a hold begins, a different error starts a new
//     streak: only a failure that repeats exactly is a loop. A seat failing
//     twice counts once.
//   - Hold: the default routed-work demand count stops counting the bead for
//     that template (withhold), so no seat is spawned for it, and
//     bead.claim_loop_held names the template, the bead and the error for the
//     operator. A hold is about the bead, not the error: seats already running
//     when it begins may still fail, the same way or another, and they neither
//     lengthen it nor end it.
//   - Back off: when a hold runs out, the bead counts for one demand pass, so a
//     probe seat can be spawned, and is withheld again for claimLoopProbeGrace
//     while the probe reports. A probe that fails, whatever its error, doubles
//     the hold, up to claimLoopHoldMax, and raises bead.claim_loop_held again
//     with the hold's new end; one that never reports is followed by another.
//   - Resume: any journal event naming the bead (created, updated, closed,
//     deleted) drops its records, and a config reload that changes the revision
//     drops every record. A successful claim is itself a bead update. So is any
//     other write to the bead, an operator's or a controller repair's, which
//     costs at most a fresh streak.
//
// It holds no judgment: the thresholds are constants, the comparison is string
// equality (after removing a server request id, which differs on every answer),
// and it never touches the bead — seats keep claiming whatever their work query
// serves; only the controller's spawn count changes. The state is in memory, so
// a controller restart forgets it and the guard re-learns after
// claimLoopTripFailures more seats fail.
//
// The guard withholds only default routed-work demand, the one demand count
// built bead by bead. A custom scale_check is a number per template with no
// beads behind it, and assigned work wakes the seat it is assigned to, so
// neither can be withheld here; the routed tier is the only one that reports.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/events"
)

// Thresholds are consts, not config, for the reason route recovery's are: each
// is a knob that could recreate the loop it exists to stop.
const (
	// claimLoopTripFailures is how many distinct seats failing with one error
	// trip a hold. Three seats is minutes of a loop and still more than the one
	// or two failures an ordinary race or restart produces.
	claimLoopTripFailures = 3
	// claimLoopWindow bounds a streak: a failure later than this after the
	// streak's first starts a new one. It does not apply once a hold is in
	// place, where the hold itself is why failures are spaced out.
	claimLoopWindow = 30 * time.Minute
	// claimLoopHoldBase is the first hold; each probe that fails the same way
	// doubles it, up to claimLoopHoldMax.
	claimLoopHoldBase = 5 * time.Minute
	claimLoopHoldMax  = 30 * time.Minute
	// claimLoopProbeGrace is how long one probe has to report back after a hold
	// runs out before the bead is admitted for another. A seat claims within a
	// minute or two of being spawned; a probe that never spawned or never
	// reported is followed by another after this.
	claimLoopProbeGrace = 10 * time.Minute
	// claimLoopForgetAfter drops a record that has seen no failure for this long:
	// with probes at most claimLoopHoldMax+claimLoopProbeGrace apart, a bead still
	// failing reports far more often, so a record this quiet is about a bead that
	// left demand without an event saying so.
	claimLoopForgetAfter = 6 * time.Hour
	// claimLoopErrorMax bounds the error text compared and carried in events.
	claimLoopErrorMax = 512
)

// claimLoopRequestID matches the correlation id a server appends to a 5xx
// answer ("(request_id ...)"). It differs on every answer, so comparing it would
// keep the same failure from ever repeating.
var claimLoopRequestID = regexp.MustCompile(`\s*\(request_id [^)]*\)`)

// claimLoopKey is one template's claims of one bead.
type claimLoopKey struct {
	template string
	beadID   string
}

// claimLoopRecord is the streak of identical claim failures for one key.
type claimLoopRecord struct {
	err      string              // the error text the streak repeats; after a failed probe, the probe's
	seats    map[string]struct{} // the seats (claimLoopSeat) counted in the streak
	failures int                 // distinct failing seats; one without a session id counts on its own
	first    time.Time           // the streak's first failure
	last     time.Time           // the streak's last failure
	holds    int                 // holds in this streak; 0 = not held
	until    time.Time           // while held: withheld until this time
	probing  bool                // a probe was admitted and has not reported
}

// claimLoopGuard is one city's guard. Its methods are safe for concurrent use:
// observe runs on the journal feed's goroutine and withhold on the tick's.
type claimLoopGuard struct {
	mu      sync.Mutex
	records map[claimLoopKey]*claimLoopRecord
	now     func() time.Time
	// record writes the guard's alert to the city event log; nil writes none.
	record func(events.Event)
	stderr io.Writer
}

func newClaimLoopGuard(record func(events.Event), stderr io.Writer) *claimLoopGuard {
	if stderr == nil {
		stderr = io.Discard
	}
	return &claimLoopGuard{records: map[claimLoopKey]*claimLoopRecord{}, now: time.Now, record: record, stderr: stderr}
}

// claimLoopErrorText is the bounded form of a claim error's text: trimmed and
// cut on a rune boundary. It is bead.claim_failed's message, request id
// included, because that id is an operator's handle on the server's log line.
func claimLoopErrorText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= claimLoopErrorMax {
		return text
	}
	cut := claimLoopErrorMax
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// claimLoopComparable is the form the guard compares, which bead.claim_failed's
// payload carries: the text without a server request id, then bounded. The id
// goes first because the bound can fall inside it, leaving each answer's own
// partial id, or after an id whose length varies, keeping a different amount
// of the text that follows it; either way no two seats would match.
func claimLoopComparable(text string) string {
	return claimLoopErrorText(claimLoopRequestID.ReplaceAllString(text, ""))
}

// claimLoopSeat names the seat a failure came from. A session bead keeps its
// id when its runtime restarts (a woken named singleton, a resumed seat), and
// the runtime epoch advances on each start, so the pair counts each start as a
// seat while a seat retrying within one start counts once. No session id is an
// anonymous seat.
func claimLoopSeat(payload events.BeadClaimFailedPayload) string {
	session := strings.TrimSpace(payload.SessionID)
	if session == "" || payload.RuntimeEpoch == 0 {
		return session
	}
	return fmt.Sprintf("%s@%d", session, payload.RuntimeEpoch)
}

// claimLoopHold is how long the holds-th hold of a streak lasts.
func claimLoopHold(holds int) time.Duration {
	hold := claimLoopHoldBase
	for i := 1; i < holds && hold < claimLoopHoldMax; i++ {
		hold *= 2
	}
	return min(hold, claimLoopHoldMax)
}

// observe folds one journal event into the guard: a claim failure extends or
// restarts its streak, and any event naming a bead the guard tracks resumes it.
func (g *claimLoopGuard) observe(evt events.Event) {
	if g == nil {
		return
	}
	switch evt.Type {
	case events.BeadClaimFailed:
		g.observeFailure(evt)
	case events.BeadCreated, events.BeadUpdated, events.BeadClosed, events.BeadDeleted:
		g.forgetBead(strings.TrimSpace(evt.Subject), "bead "+strings.TrimPrefix(evt.Type, "bead."))
	}
}

func (g *claimLoopGuard) observeFailure(evt events.Event) {
	var payload events.BeadClaimFailedPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return
	}
	key := claimLoopKey{template: strings.TrimSpace(payload.Template), beadID: strings.TrimSpace(evt.Subject)}
	if key.template == "" || key.beadID == "" {
		return
	}
	text := claimLoopComparable(payload.Error)
	seat := claimLoopSeat(payload)
	at := evt.Ts
	if at.IsZero() {
		at = g.now()
	}
	var alert *events.Event
	g.mu.Lock()
	g.sweep(at)
	rec := g.records[key]
	if rec == nil || (rec.holds == 0 && (rec.err != text || at.Sub(rec.first) > claimLoopWindow)) {
		rec = &claimLoopRecord{err: text, seats: map[string]struct{}{}, first: at}
		g.records[key] = rec
	}
	rec.last = at
	if _, counted := rec.seats[seat]; seat != "" && counted {
		g.mu.Unlock()
		return // the same seat failing again is not another seat failing
	}
	if seat != "" {
		rec.seats[seat] = struct{}{}
	}
	rec.failures++
	switch {
	case rec.holds == 0 && rec.failures >= claimLoopTripFailures:
		rec.holds = 1
		rec.until = at.Add(claimLoopHold(rec.holds))
		alert = g.heldEvent(key, rec, at)
		fmt.Fprintf(g.stderr, "claim-loop guard: holding %s for %s: %d seats in a row failed to claim it with %q; withheld from default routed-work demand until %s\n", //nolint:errcheck // best-effort stderr
			key.beadID, key.template, rec.failures, rec.err, rec.until.UTC().Format(time.RFC3339))
	case rec.probing:
		// The probe failed, whatever its error: the bead still does not
		// claim. Hold again, longer.
		rec.probing = false
		rec.holds++
		rec.err = text
		rec.until = at.Add(claimLoopHold(rec.holds))
		alert = g.heldEvent(key, rec, at)
		fmt.Fprintf(g.stderr, "claim-loop guard: probe of %s for %s failed with %q (%d seats); holding until %s\n", //nolint:errcheck // best-effort stderr
			key.beadID, key.template, rec.err, rec.failures, rec.until.UTC().Format(time.RFC3339))
	}
	g.mu.Unlock()
	if alert != nil && g.record != nil {
		g.record(*alert)
	}
}

// heldEvent is the bead.claim_loop_held alert for a hold that just began or,
// after a probe failed, was extended.
func (g *claimLoopGuard) heldEvent(key claimLoopKey, rec *claimLoopRecord, at time.Time) *events.Event {
	payload, err := json.Marshal(events.BeadClaimLoopHeldPayload{
		Template:  key.template,
		Error:     rec.err,
		Failures:  rec.failures,
		HeldUntil: rec.until.UTC(),
	})
	if err != nil {
		return nil
	}
	cause := fmt.Sprintf("%d %s seats in a row failed to claim %s with the same error", rec.failures, key.template, key.beadID)
	if rec.holds > 1 {
		cause = fmt.Sprintf("the probe seat of %s failed to claim %s too (%d seats in all)", key.template, key.beadID, rec.failures)
	}
	return &events.Event{
		Type:    events.BeadClaimLoopHeld,
		Ts:      at.UTC(),
		Actor:   "controller",
		Subject: key.beadID,
		Message: fmt.Sprintf("%s; it is withheld from %s's default routed-work demand until %s or until it changes (a custom scale_check count is not withheld)", cause, key.template, rec.until.UTC().Format(time.RFC3339)),
		Payload: payload,
	}
}

// forgetBead drops every record for beadID: the bead changed, so whatever made
// its claims fail may have changed with it.
func (g *claimLoopGuard) forgetBead(beadID, why string) {
	if beadID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, rec := range g.records {
		if key.beadID != beadID {
			continue
		}
		if rec.holds > 0 {
			fmt.Fprintf(g.stderr, "claim-loop guard: %s counts as demand for %s again: %s\n", beadID, key.template, why) //nolint:errcheck // best-effort stderr
		}
		delete(g.records, key)
	}
}

// configChanged drops every record: a new config may have fixed what the claims
// were failing on.
func (g *claimLoopGuard) configChanged() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, rec := range g.records {
		if rec.holds > 0 {
			fmt.Fprintf(g.stderr, "claim-loop guard: %s counts as demand for %s again: config changed\n", key.beadID, key.template) //nolint:errcheck // best-effort stderr
		}
	}
	clear(g.records)
}

// heldNow reports whether key's bead is withheld from key's template right now,
// admitting one probe when a hold has run out. Caller holds g.mu.
func (g *claimLoopGuard) heldNow(key claimLoopKey, now time.Time) bool {
	rec := g.records[key]
	if rec == nil || rec.holds == 0 {
		return false
	}
	if now.Before(rec.until) {
		return true
	}
	// The hold ran out: count the bead this pass, so one seat can probe it,
	// and withhold it again while the probe reports.
	rec.probing = true
	rec.until = now.Add(claimLoopProbeGrace)
	return false
}

// sweep drops records too quiet to describe a live loop. Caller holds g.mu.
func (g *claimLoopGuard) sweep(now time.Time) {
	for key, rec := range g.records {
		if now.Sub(rec.last) > claimLoopForgetAfter || (rec.holds == 0 && now.Sub(rec.first) > claimLoopWindow) {
			delete(g.records, key)
		}
	}
}

// withhold removes the beads this guard holds from one pass's default
// routed-work demand: from each template's bead list and its count. It never
// edits its arguments; with nothing held it returns them unchanged.
func (g *claimLoopGuard) withhold(counts map[string]int, demand map[string]scaleCheckDemand) (map[string]int, map[string]scaleCheckDemand) {
	if g == nil {
		return counts, demand
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(now)
	if len(g.records) == 0 {
		return counts, demand
	}
	var outCounts map[string]int
	var outDemand map[string]scaleCheckDemand
	for template, entry := range demand {
		var keep []string
		withheld := 0
		for i, id := range entry.WorkBeadIDs {
			if !g.heldNow(claimLoopKey{template: template, beadID: id}, now) {
				if keep != nil {
					keep = append(keep, id)
				}
				continue
			}
			if keep == nil {
				keep = append(make([]string, 0, len(entry.WorkBeadIDs)-1), entry.WorkBeadIDs[:i]...)
			}
			withheld++
		}
		if withheld == 0 {
			continue
		}
		if outDemand == nil {
			outDemand = cloneScaleCheckDemandMap(demand)
			outCounts = make(map[string]int, len(counts))
			for k, v := range counts {
				outCounts[k] = v
			}
		}
		entry.WorkBeadIDs = keep
		entry.Count = max(entry.Count-withheld, 0)
		outDemand[template] = entry
		outCounts[template] = max(outCounts[template]-withheld, 0)
	}
	if outDemand == nil {
		return counts, demand
	}
	return outCounts, outDemand
}

// cloneScaleCheckDemandMap copies the map, not the entries' inner maps: an
// entry is replaced whole when it changes, and the per-bead maps it keeps are
// only read by bead id, so a withheld id left in them is never consulted.
func cloneScaleCheckDemandMap(in map[string]scaleCheckDemand) map[string]scaleCheckDemand {
	out := make(map[string]scaleCheckDemand, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// claimLoopGuards maps a normalized city path to its controller's guard. The
// legacy demand pass runs behind a build function that holds the city path but
// not the runtime, so the registry is how both demand passes reach the guard of
// the controller running their city. A process that runs no controller — every
// CLI command — registers none, and its demand reads are untouched.
var claimLoopGuards sync.Map // normalized city path → *claimLoopGuard

// registerClaimLoopGuard publishes g for cityPath until ctx ends, and returns
// the unregistration, which also runs when ctx ends. It removes g only while g
// is still the city's guard, so a stale one never unpublishes its successor.
func registerClaimLoopGuard(ctx context.Context, cityPath string, g *claimLoopGuard) (unregister func()) {
	key := normalizePathForCompare(cityPath)
	claimLoopGuards.Store(key, g)
	unregister = func() { claimLoopGuards.CompareAndDelete(key, g) }
	if ctx != nil {
		context.AfterFunc(ctx, unregister)
	}
	return unregister
}

// claimLoopGuardFor is cityPath's registered guard, or nil.
func claimLoopGuardFor(cityPath string) *claimLoopGuard {
	if strings.TrimSpace(cityPath) == "" {
		return nil
	}
	g, ok := claimLoopGuards.Load(normalizePathForCompare(cityPath))
	if !ok {
		return nil
	}
	return g.(*claimLoopGuard)
}
