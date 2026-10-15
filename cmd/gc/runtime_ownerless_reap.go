package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The ownerless-runtime reap (CONTRACT v5.10j, mc-gjc95). A gc runtime whose
// GC_SESSION_ID names a row no session store holds (purged, deleted, or
// written to a store this city no longer reads) has no owner to reap it:
// the closed-owner reap needs a closed row, and the per-row arms need a row.
// Legacy's start recycled such a dead runtime under any name; v2's
// FreshOnly start refuses a held name, so a named session over one stayed
// down. The tick now reaps it, fenced: only a runtime that proves it is this
// city's (GC_CITY_PATH), whose owner every store a session row can live in
// answers not found (ownerLookup) on two sightings at least a patrol apart,
// which reads dead on a fresh read (a corpse, or a zombie by its own
// published process names), is older than the age grace, and has no client
// attached, under the name's runtime lease. Each reap records an event.
// Anything short of that is left running, and gc doctor's
// ownerless-runtimes finding names it.

// ownerlessReapConfirmAfter is the least time an owner must stay not found
// between the two sightings a reap needs, and the patrol interval raises it
// (ownerlessReap.gap): a store read that lags a create, or a row written to
// a store this pass did not read, gets a pass to show up.
const ownerlessReapConfirmAfter = 30 * time.Second

// ownerlessAgeGrace is the runtime-age grace: the city's startup_timeout,
// config's default without one.
func ownerlessAgeGrace(cfg *config.City) time.Duration {
	if cfg != nil {
		if d := cfg.Session.StartupTimeoutDuration(); d > 0 {
			return d
		}
	}
	return 60 * time.Second
}

// checkNow is the time of the final check under the lease.
func (o *ownerlessReap) checkNow() time.Time {
	if o.clock != nil {
		return o.clock()
	}
	return time.Now()
}

// newOwnerlessReap is the tick's reap of runtimes whose owner row is
// missing, for cityPath at now: the sessions store leads the owner lookup,
// which rigs supplies on demand; the sightings are a patrol apart (at least
// ownerlessReapConfirmAfter); the age grace is ownerlessAgeGrace.
func newOwnerlessReap(cityPath string, cfg *config.City, sessions beads.Store, rigs func() map[string]beads.Store, sightings *ownerlessSightings, rec events.Recorder, now time.Time) *ownerlessReap {
	var patrol time.Duration
	if cfg != nil {
		patrol = cfg.Daemon.PatrolIntervalDuration()
	}
	return &ownerlessReap{
		sightings: sightings,
		lookup:    func() ownerLookup { return sessionOwnerLookup(cityPath, cfg, sessions, rigs()) },
		rec:       rec,
		gap:       max(ownerlessReapConfirmAfter, patrol),
		grace:     ownerlessAgeGrace(cfg),
		now:       now,
		clock:     time.Now,
	}
}

// ownerlessReap is what the closed-bead reap needs to reap a runtime whose
// owner row is missing (CONTRACT v5.10j): the controller's sightings, every
// store a session row can live in, the event recorder, the least gap
// between the two sightings (one patrol, at least
// ownerlessReapConfirmAfter), the runtime-age grace (startup_timeout: a
// runtime that young may be a start whose row is not readable yet), and
// the tick's time.
type ownerlessReap struct {
	// clock is the time of the final check under the lease (time.Now; a
	// test's own), so its fresh read is as of the check, not the tick.
	clock     func() time.Time
	sightings *ownerlessSightings
	owners    ownerLookup
	// lookup, when set, plans owners on the first ownerless runtime a pass
	// meets, so a pass with none plans nothing.
	lookup func() ownerLookup
	rec    events.Recorder
	gap    time.Duration
	grace  time.Duration
	now    time.Time
}

// ownerlessVerdict is a runtime's ownerless classification, the reap's and
// the doctor finding's.
type ownerlessVerdict struct {
	name    string
	ownerID string // GC_SESSION_ID
	reason  string // why it is not reapable; "" when it is
}

// The reasons a runtime whose owner is not found is left running.
const (
	ownerlessCityUnreadable = "city-unreadable" // GC_CITY_PATH cannot be read on this leaf: doctor finding only
	ownerlessCityForeign    = "city-foreign"    // GC_CITY_PATH names another city
	ownerlessAlive          = "alive"           // the agent may be alive: reported, never stopped
	ownerlessLivenessError  = "liveness-error"  // its liveness could not be read
	ownerlessGone           = "gone"            // nothing under the name any more
	ownerlessAttached       = "attached"        // a client is attached, or attachment cannot be read
	ownerlessYoung          = "young"           // younger than the age grace, or its age cannot be read
)

// classifyOwnerlessRuntime classifies name, whose owner ownerID the stores
// answered not found, for cityPath at now: reapable (reason "") only when it
// is this city's, reads dead on a fresh read, is older than grace and has no
// client attached.
func classifyOwnerlessRuntime(sp runtime.Provider, cityPath, name, ownerID string, now time.Time, grace time.Duration) ownerlessVerdict {
	v := ownerlessVerdict{name: name, ownerID: ownerID}
	city, err := sp.GetMeta(name, "GC_CITY_PATH")
	switch {
	case err != nil || strings.TrimSpace(city) == "":
		v.reason = ownerlessCityUnreadable
		return v
	case filepath.Clean(strings.TrimSpace(city)) != filepath.Clean(cityPath):
		v.reason = ownerlessCityForeign
		return v
	}
	dead, present, created, err := ownerlessRuntimeDead(sp, name, now)
	switch {
	case err != nil:
		v.reason = ownerlessLivenessError
		return v
	case !present:
		v.reason = ownerlessGone
		return v
	case !dead:
		v.reason = ownerlessAlive
		return v
	case created.IsZero() || now.Sub(created) < grace:
		v.reason = ownerlessYoung
		return v
	}
	if attached, err := runtime.IsAttachedWithError(sp, name); err != nil || attached {
		v.reason = ownerlessAttached
	}
	return v
}

// ownerlessRuntimeDead reads name's liveness fresh (as of now, past any
// provider cache) by the process names its start published
// (GT_PROCESS_NAMES), with no row to supply them: a corpse (no pane alive),
// or a zombie (a pane up, none of its agent's processes alive), and when the
// runtime was created (zero when the provider cannot say). A runtime without
// published names that runs cannot be told from a live agent, so it is not
// dead.
func ownerlessRuntimeDead(sp runtime.Provider, name string, now time.Time) (dead, present bool, created time.Time, err error) {
	raw, err := sp.GetMeta(name, "GT_PROCESS_NAMES")
	if err != nil && !runtime.IsSessionGone(err) {
		return false, false, time.Time{}, err
	}
	var names []string
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	live, err := runtime.ObserveLivenessSince(sp, name, names, now)
	if err != nil {
		return false, false, time.Time{}, err
	}
	if secs, perr := strconv.ParseInt(strings.TrimSpace(live.ObjectCreated), 10, 64); perr == nil && secs > 0 {
		created = time.Unix(secs, 0)
	}
	switch {
	case live.Corpse:
		return true, true, created, nil
	case !live.Running:
		return false, false, created, nil
	}
	return len(names) > 0 && !live.Alive, true, created, nil
}

// ownerlessSightings is the controller's in-memory record of ownerless
// runtimes between ticks: when each (name, owner) pair was first seen not
// found, and which alive ones it has reported. A restart forgets it, which
// only delays a reap by a sighting (CONTRACT v5.10j).
type ownerlessSightings struct {
	mu       sync.Mutex
	first    map[string]time.Time
	reported map[string]bool
	seen     map[string]bool // this pass's sightings
}

func ownerlessKey(name, ownerID string) string { return name + "\x00" + ownerID }

// begin starts a pass: a pair not sighted again by end is forgotten, so a
// runtime that recovers, or a pass that does not list it, starts over.
func (o *ownerlessSightings) begin() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = map[string]bool{}
}

// sight records key's sighting at now and reports whether an earlier one at
// least gap ago confirms it.
func (o *ownerlessSightings) sight(key string, now time.Time, gap time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.first == nil {
		o.first = map[string]time.Time{}
	}
	o.seen[key] = true
	first, ok := o.first[key]
	if !ok {
		o.first[key] = now
		return false
	}
	return now.Sub(first) >= gap
}

// report reports whether key is reported for the first time.
func (o *ownerlessSightings) report(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.reported == nil {
		o.reported = map[string]bool{}
	}
	o.seen[key] = true
	if o.reported[key] {
		return false
	}
	o.reported[key] = true
	return true
}

func (o *ownerlessSightings) end() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for key := range o.first {
		if !o.seen[key] {
			delete(o.first, key)
		}
	}
	for key := range o.reported {
		if !o.seen[key] {
			delete(o.reported, key)
		}
	}
}

// ownerLookup is every store a session row can live in (the sessions
// binding, the city work store and every rig store, suspended rigs
// included: storeref's session plan), or why they could not all be opened.
type ownerLookup struct {
	stores []beads.Store
	err    error
}

// notFound reports that no store holds row ownerID, on a live read of each:
// every one answers beads.ErrNotFound. A store that errors, a lookup missing
// a store, or no store at all proves nothing, so the owner is not missing.
func (l ownerLookup) notFound(ownerID string) bool {
	if l.err != nil || len(l.stores) == 0 {
		return false
	}
	for _, store := range l.stores {
		if _, err := beads.HandlesFor(store).Live.Get(ownerID); !errors.Is(err, beads.ErrNotFound) {
			return false
		}
	}
	return true
}

// sessionOwnerLookup is the lookup over cityPath's session plan: sessions
// leads, then the work store and every rig store (suspended ones too: a row
// in a suspended rig still owns its runtime). A configured rig whose store
// is not open fails the lookup, which then proves no owner missing.
func sessionOwnerLookup(cityPath string, cfg *config.City, sessions beads.Store, rigs map[string]beads.Store) ownerLookup {
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) != "" && rigs[rig.Name] == nil {
				// A row in that rig cannot be ruled out: no owner is provable.
				return ownerLookup{err: fmt.Errorf("rig %s's store is not open", rig.Name)}
			}
		}
	}
	legs, err := sessionCensusStoreCandidates(cityPath, cfg, sessions, rigs, nil)
	if err != nil {
		return ownerLookup{err: err}
	}
	l := ownerLookup{}
	for _, leg := range legs {
		if leg.store != nil {
			l.stores = append(l.stores, leg.store)
		}
	}
	return l
}

// reapOwnerlessRuntime reaps name, whose owner ownerID the sessions store
// just answered not found, when every session store answers not found and
// classifyOwnerlessRuntime finds it reapable on a confirmed sighting; under
// the name's runtime lease (with nameOwnerID's record, the open row that
// owns the name now, if any) it reads the identity, the owner, the liveness
// and the attachment again before the Stop, and records the reap. It
// reports whether it stopped name.
func reapOwnerlessRuntime(store beads.Store, cityPath, name, ownerID, nameOwnerID string, sp runtime.Provider, o *ownerlessReap, stderr io.Writer) bool {
	key, sightings, now := ownerlessKey(name, ownerID), o.sightings, o.now
	if o.lookup != nil {
		o.owners, o.lookup = o.lookup(), nil
	}
	if !o.owners.notFound(ownerID) {
		return false
	}
	v := classifyOwnerlessRuntime(sp, cityPath, name, ownerID, now, o.grace)
	switch v.reason {
	case "":
	case ownerlessAlive, ownerlessAttached:
		if sightings.report(key) {
			fmt.Fprintf(stderr, "session reconciler: runtime %q is bound to session bead %s, which the store does not hold; left running (%s): see gc doctor ownerless-runtimes\n", name, ownerID, v.reason) //nolint:errcheck
		}
		return false
	default:
		return false
	}
	if !sightings.sight(key, now, o.gap) {
		return false
	}
	_, release, err := tryRuntimeLease(store, cityPath, name, nameOwnerID, session.RuntimeLeaseTTL(0))
	if err != nil && nameOwnerID != "" && !errors.Is(err, session.ErrRuntimeLeaseBusy) {
		fmt.Fprintf(stderr, "session reconciler: reaping ownerless runtime %q under its flock alone: %v\n", name, err) //nolint:errcheck
		_, release, err = tryRuntimeLease(nil, cityPath, name, "", session.RuntimeLeaseTTL(0))
	}
	if err != nil {
		if !errors.Is(err, session.ErrRuntimeLeaseBusy) {
			fmt.Fprintf(stderr, "session reconciler: reaping ownerless runtime %q skipped: %v\n", name, err) //nolint:errcheck
		}
		return false
	}
	defer release()
	if again, err := sp.GetMeta(name, "GC_SESSION_ID"); err != nil || strings.TrimSpace(again) != ownerID {
		return false
	}
	if !o.owners.notFound(ownerID) || classifyOwnerlessRuntime(sp, cityPath, name, ownerID, o.checkNow(), o.grace).reason != "" { // read as of now, under the lease
		return false
	}
	if err := sp.Stop(name); err != nil {
		if !runtime.IsSessionGone(err) {
			fmt.Fprintf(stderr, "session reconciler: reaping ownerless runtime %q: %v\n", name, err) //nolint:errcheck
		}
		return false
	}
	fmt.Fprintf(stderr, "session reconciler: reaped dead runtime %q bound to session bead %s, which no store holds\n", name, ownerID) //nolint:errcheck
	if o.rec != nil {
		o.rec.Record(events.Event{
			Type: events.SessionStopped, Actor: "gc", Subject: name, SessionID: ownerID,
			Message: "reaped dead runtime whose owner row is missing",
			Payload: api.SessionLifecyclePayloadJSON(ownerID, "", "ownerless-reap"),
		})
	}
	return true
}

// ownerlessRuntimeFindings lists the runtimes sp lists for cityPath that no
// row owns: no readable GC_SESSION_ID, or one no session store holds
// (owners), each with why the tick leaves it (or that it reaps it). The
// doctor's finding.
func ownerlessRuntimeFindings(owners ownerLookup, sp runtime.Provider, cityPath string, now time.Time, grace time.Duration) ([]string, error) {
	names, err := sp.ListRunning("")
	if err != nil && !runtime.IsPartialListError(err) {
		return nil, err
	}
	var findings []string
	for _, name := range slices.Sorted(slices.Values(names)) {
		id, idErr := sp.GetMeta(name, "GC_SESSION_ID")
		id = strings.TrimSpace(id)
		switch {
		case idErr != nil:
			findings = append(findings, fmt.Sprintf("%s: GC_SESSION_ID unreadable (%v); no owner can be proven", name, idErr))
			continue
		case id == "":
			findings = append(findings, fmt.Sprintf("%s: no GC_SESSION_ID; no owner can be proven", name))
			continue
		case !owners.notFound(id):
			continue
		}
		v := classifyOwnerlessRuntime(sp, cityPath, name, id, now, grace)
		why := v.reason
		if why == "" {
			why = "dead: the controller reaps it"
		}
		findings = append(findings, fmt.Sprintf("%s: session bead %s not found (%s)", name, id, why))
	}
	return findings, err
}
