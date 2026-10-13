package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The ownerless-runtime reap (CONTRACT v5.10j, mc-gjc95). A gc runtime whose
// GC_SESSION_ID names a row the session store no longer holds (purged,
// deleted, or written to a store this city no longer reads) has no owner to
// reap it: the closed-owner reap needs a closed row, and the per-row arms
// need a row. Legacy's start recycled such a dead runtime under any name;
// v2's FreshOnly start refuses a held name, so a named session over one
// stayed down. The tick now reaps it, fenced: only a runtime that proves it is
// this city's (GC_CITY_PATH), whose owner the store answers not found on two
// sightings at least ownerlessReapConfirmAfter apart, which reads dead (a
// corpse, or a zombie by its own published process names), with no client
// attached, under the name's runtime lease. Anything short of that is left
// running, and gc doctor's ownerless-runtimes finding names it.

// ownerlessReapConfirmAfter is how long an owner must stay not found between
// the two sightings a reap needs: a store read that lags a create, or a row
// written to a store this pass did not read, gets a pass to show up.
const ownerlessReapConfirmAfter = 30 * time.Second

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
)

// classifyOwnerlessRuntime classifies name, whose owner ownerID the store
// answered not found, for cityPath: reapable (reason "") only when it is this
// city's, reads dead and has no client attached.
func classifyOwnerlessRuntime(sp runtime.Provider, cityPath, name, ownerID string) ownerlessVerdict {
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
	dead, present, err := ownerlessRuntimeDead(sp, name)
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
	}
	if attached, err := runtime.IsAttachedWithError(sp, name); err != nil || attached {
		v.reason = ownerlessAttached
	}
	return v
}

// ownerlessRuntimeDead reads name's liveness by the process names its start
// published (GT_PROCESS_NAMES), with no row to supply them: a corpse (no pane
// alive), or a zombie (a pane up, none of its agent's processes alive). A
// runtime without published names that runs cannot be told from a live
// agent, so it is not dead.
func ownerlessRuntimeDead(sp runtime.Provider, name string) (dead, present bool, err error) {
	if checker, ok := sp.(runtime.DeadRuntimeSessionChecker); ok {
		corpse, err := checker.IsDeadRuntimeSession(name)
		if err != nil {
			return false, false, err
		}
		if corpse {
			return true, true, nil
		}
	}
	if !sp.IsRunning(name) {
		return false, false, nil
	}
	raw, err := sp.GetMeta(name, "GT_PROCESS_NAMES")
	if err != nil {
		return false, true, err
	}
	var names []string
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return len(names) > 0 && !sp.ProcessAlive(name, names), true, nil
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
// least ownerlessReapConfirmAfter ago confirms it.
func (o *ownerlessSightings) sight(key string, now time.Time) bool {
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
	return now.Sub(first) >= ownerlessReapConfirmAfter
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

// ownerNotFound reports a live store read that finds no row ownerID: only
// beads.ErrNotFound counts, never another error.
func ownerNotFound(store beads.Store, ownerID string) bool {
	_, err := beads.HandlesFor(store).Live.Get(ownerID)
	return errors.Is(err, beads.ErrNotFound)
}

// reapOwnerlessRuntime reaps name, whose owner ownerID the store just
// answered not found, when classifyOwnerlessRuntime finds it reapable on a
// confirmed sighting; under the name's runtime lease (with nameOwnerID's
// record, the open row that owns the name now, if any) it reads the
// identity, the owner, the liveness and the attachment again before the
// Stop. It reports whether it stopped name.
func reapOwnerlessRuntime(store beads.Store, cityPath, name, ownerID, nameOwnerID string, sp runtime.Provider, sightings *ownerlessSightings, now time.Time, stderr io.Writer) bool {
	key := ownerlessKey(name, ownerID)
	v := classifyOwnerlessRuntime(sp, cityPath, name, ownerID)
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
	if !sightings.sight(key, now) {
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
	if !ownerNotFound(store, ownerID) || classifyOwnerlessRuntime(sp, cityPath, name, ownerID).reason != "" {
		return false
	}
	if err := sp.Stop(name); err != nil {
		if !runtime.IsSessionGone(err) {
			fmt.Fprintf(stderr, "session reconciler: reaping ownerless runtime %q: %v\n", name, err) //nolint:errcheck
		}
		return false
	}
	fmt.Fprintf(stderr, "session reconciler: reaped dead runtime %q bound to session bead %s, which the store does not hold\n", name, ownerID) //nolint:errcheck
	return true
}

// ownerlessRuntimeFindings lists the runtimes sp lists for cityPath that no
// row owns: no readable GC_SESSION_ID, or one the store does not hold, each
// with why the tick leaves it (or "reapable"). The doctor's finding.
func ownerlessRuntimeFindings(store beads.Store, sp runtime.Provider, cityPath string) ([]string, error) {
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
		case !ownerNotFound(store, id):
			continue
		}
		v := classifyOwnerlessRuntime(sp, cityPath, name, id)
		why := v.reason
		if why == "" {
			why = "dead: the controller reaps it"
		}
		findings = append(findings, fmt.Sprintf("%s: session bead %s not found (%s)", name, id, why))
	}
	return findings, err
}
