package main

// The claim-loop guard (ga-ylpbsy.30).
//
// The controller spawns a pool seat when it counts a routed bead as demand, and
// the seat claims its own work. When the claim of that bead fails the same way
// for every seat — a store the claim cannot reach, a write the store refuses —
// the two never converge: the bead stays ready, the controller keeps counting
// it, and each seat it spawns fails the claim, drains claims_errored and is
// replaced. On maintainer-city one wisp the work store could not claim drew
// 2,228 Opus seats over 31 hours that way.
//
// The guard is the mechanical backstop for that whole class, whatever the
// failure's cause:
//
//   - Input: bead.claim_failed, which `gc hook --claim` records for each served
//     candidate whose claim write failed (template, bead, error text), read off
//     the city's event journal by the same feed as the other delta lanes.
//   - Trip: claimLoopTripFailures failures in a row of one template's claims of
//     one bead with the SAME error text, the first and the last within
//     claimLoopWindow. A different error starts a new streak: only a failure that
//     repeats exactly is a loop.
//   - Hold: the default routed-work demand count stops counting the bead for that
//     template (withhold), so no seat is spawned for it, and bead.claim_loop_held
//     names the template, the bead and the error for the operator.
//   - Back off: when a hold runs out, the bead counts again until one probe seat
//     has had claimLoopProbeGrace to claim it. A probe that fails the same way
//     doubles the hold, up to claimLoopHoldMax.
//   - Resume: any journal event naming the bead (updated, closed, deleted) drops
//     its records, and a config reload that changes the revision drops every
//     record. A successful claim is itself a bead update.
//
// It holds no judgment: the thresholds are constants, the comparison is string
// equality, and it never touches the bead — seats keep claiming whatever their
// work query serves; only the controller's spawn count changes. The state is in
// memory, so a controller restart forgets it and the guard re-learns after
// claimLoopTripFailures more failures.
//
// A custom scale_check is a number per template with no beads behind it, so the
// guard cannot withhold one bead from it; such a pool still gets the alert.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/events"
)

// Thresholds are consts, not config, for the reason route recovery's are: each
// is a knob that could recreate the loop it exists to stop.
const (
	// claimLoopTripFailures is how many claims in a row, with one error, trip a
	// hold. Three seats is minutes of a loop and still more than the one or two
	// failures an ordinary race or restart produces.
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
	// runs out before the bead is withheld again. A seat claims within a minute
	// or two of being spawned; a probe that never spawned or never reported is
	// retried after this.
	claimLoopProbeGrace = 10 * time.Minute
	// claimLoopForgetAfter drops a record that has seen no failure for this long:
	// with probes at most claimLoopHoldMax+claimLoopProbeGrace apart, a bead still
	// failing reports far more often, so a record this quiet is about a bead that
	// left demand without an event saying so.
	claimLoopForgetAfter = 6 * time.Hour
	// claimLoopErrorMax bounds the error text compared and carried in events.
	claimLoopErrorMax = 512
)

// claimLoopKey is one template's claims of one bead.
type claimLoopKey struct {
	template string
	beadID   string
}

// claimLoopRecord is the streak of identical claim failures for one key.
type claimLoopRecord struct {
	err      string    // the error text the streak repeats
	failures int       // consecutive failures with err
	first    time.Time // the streak's first failure
	last     time.Time // the streak's last failure
	holds    int       // holds in this streak; 0 = not held
	until    time.Time // while held: withheld until this time
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

// claimLoopErrorText is the comparable, bounded form of a claim error's text.
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
	text := claimLoopErrorText(payload.Error)
	at := evt.Ts
	if at.IsZero() {
		at = g.now()
	}
	var alert *events.Event
	g.mu.Lock()
	rec := g.records[key]
	switch {
	case rec == nil || rec.err != text:
		rec = &claimLoopRecord{err: text, failures: 1, first: at, last: at}
		g.records[key] = rec
	case rec.holds == 0 && at.Sub(rec.first) > claimLoopWindow:
		*rec = claimLoopRecord{err: text, failures: 1, first: at, last: at}
	default:
		rec.failures++
		rec.last = at
	}
	switch {
	case rec.holds == 0 && rec.failures >= claimLoopTripFailures:
		rec.holds = 1
		rec.until = at.Add(claimLoopHold(rec.holds))
		alert = g.heldEvent(key, rec, at)
		fmt.Fprintf(g.stderr, "claim-loop guard: holding %s for %s: %d claims in a row failed with %q; not counted as spawn demand until %s\n", //nolint:errcheck // best-effort stderr
			key.beadID, key.template, rec.failures, rec.err, rec.until.UTC().Format(time.RFC3339))
	case rec.holds > 0:
		// A probe failed the same way: hold again, longer.
		rec.holds++
		rec.until = at.Add(claimLoopHold(rec.holds))
		fmt.Fprintf(g.stderr, "claim-loop guard: probe of %s for %s failed again (%d in a row); holding until %s\n", //nolint:errcheck // best-effort stderr
			key.beadID, key.template, rec.failures, rec.until.UTC().Format(time.RFC3339))
	}
	g.mu.Unlock()
	if alert != nil && g.record != nil {
		g.record(*alert)
	}
}

// heldEvent is the bead.claim_loop_held alert for a hold that just began.
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
	return &events.Event{
		Type:    events.BeadClaimLoopHeld,
		Ts:      at.UTC(),
		Actor:   "controller",
		Subject: key.beadID,
		Message: fmt.Sprintf("%d claims of %s by %s seats failed with the same error; it is not counted as spawn demand until %s or until it changes", rec.failures, key.beadID, key.template, rec.until.UTC().Format(time.RFC3339)),
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
	// The hold ran out: count the bead this time, so one seat can probe it,
	// and withhold it again until the probe has had time to report.
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

// claimLoopGuards maps a normalized city path to its controller's guard, so the
// demand passes, which know only the city path, can consult it. A process that
// runs no controller — every CLI command — registers none, and its demand reads
// are untouched.
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
