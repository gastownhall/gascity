package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// tryRuntimeLease takes cityPath's runtime lease on name for a legacy
// starter or stopper (I-LEASE): the name's flock, which excludes every
// holder in this process and on this host, and, when id is set, the record
// on that open row. It never waits: a busy name is
// session.ErrRuntimeLeaseBusy, and the caller defers. A city path that is
// not absolute, the empty one included, has no runtime dir to lock in, and
// refuses (session.RefuseWithoutCity). release is idempotent.
func tryRuntimeLease(store beads.Store, cityPath, name, id string, ttl time.Duration) (lease *session.RuntimeLease, release func(), err error) {
	if !filepath.IsAbs(cityPath) {
		return nil, nil, session.RefuseWithoutCity(cityPath, fmt.Sprintf("runtime %q", name))
	}
	var front *session.Store
	if store != nil && id != "" {
		front = sessionFrontDoor(store)
	} else {
		id = ""
	}
	if lease, err = session.TryRuntimeLease(front, session.RuntimeLeaseRequest{City: cityPath, Name: name, ID: id, TTL: ttl}); err != nil {
		return nil, nil, err
	}
	return lease, lease.Release, nil
}

// controllerStopLease takes, without waiting, the runtime lease a controller
// stop sequence holds across all its kills (a kill, its confirm-dead
// re-kills, an escalation's process-table kill), and returns the controller's
// Actor carrying it to each. A lease failure other than busy (the store unreachable, the row
// closed) is logged, and the sequence runs under the name's flock alone: a
// store never holds a stop hostage. Busy is session.ErrRuntimeLeaseBusy, which
// the caller defers to a later tick. A city path that is not absolute has no
// runtime dir to lock in: the stop refuses with session.ErrRuntimeLeaseNoCity
// and kills nothing.
func controllerStopLease(store beads.Store, cityPath, name, sessionID string, stderr io.Writer) (session.Actor, func(), error) {
	by := sessionActor(session.ActorController, cityPath)
	if !filepath.IsAbs(cityPath) {
		return by, func() {}, session.RefuseWithoutCity(cityPath, fmt.Sprintf("the controller's stop of runtime %q", name))
	}
	lease, release, err := tryRuntimeLease(store, cityPath, name, sessionID, session.RuntimeLeaseTTL(0))
	if err != nil && !errors.Is(err, session.ErrRuntimeLeaseBusy) {
		fmt.Fprintf(stderr, "session reconciler: stopping %s under the name's flock alone: %v\n", name, err) //nolint:errcheck
		lease, release, err = tryRuntimeLease(nil, cityPath, name, "", 0)
	}
	if err != nil {
		return by, func() {}, err
	}
	by.Lease = lease
	return by, release, nil
}

// The v2 effects' shared refusal causes for a row's runtime. A refusal backs
// the row off (P4).
const (
	causeNameBusy        = "name-busy"        // another effect, or a reaper, holds the runtime name
	causeRouteUnknown    = "route-unknown"    // no backend resolves the runtime name
	causeLivenessUnknown = "liveness-unknown" // the fresh liveness read was incomplete or failed
	causeNotPresent      = "not-present"      // the runtime the intent acts on is gone
	causeLeaseStore      = "lease-store"      // the row's lease record could not be read
	causeLeaseContended  = "lease-contended"  // the record's write lost its fence on every attempt
	causeLeaseRenamed    = "lease-renamed"    // the row's runtime name moved before its record was taken
	causeLeaseLocalFS    = "lease-local-fs"   // the name's lock file failed on the local filesystem
	causeLeaseNoCity     = "lease-no-city"    // the city path is not absolute: no runtime dir to lock in
	causeLeaseNoCAS      = "lease-no-cas"     // the session store cannot fence the lease record
	causeLeaseRowClosed  = "lease-row-closed" // the row closed before its lease record was taken
)

// lockRuntimeName takes w's city runtime lease on row's runtime name, as
// every v2 effect that reads or calls the provider for a row takes it: the
// name's flock and, with front (an effect that creates, destroys or
// restarts a runtime, or writes the incarnation or the commit: needs.Lease),
// the record on the row, for ttl. It never waits: a busy name, or a record
// that cannot be taken, is the refusal cause with its error (a busy one
// names the holder and its expiry), and the lease nil. A row with no
// runtime name is causeRouteUnknown, and a city path that is not absolute
// causeLeaseNoCity.
func lockRuntimeName(w *World, row session.Info, front *session.Store, ttl time.Duration) (name string, lease *session.RuntimeLease, cause string, err error) {
	name = strings.TrimSpace(row.SessionName)
	switch {
	case name == "":
		return "", nil, causeRouteUnknown, nil
	case !filepath.IsAbs(w.CityPath):
		return name, nil, causeLeaseNoCity, session.RefuseWithoutCity(w.CityPath, fmt.Sprintf("runtime %q", name))
	}
	req := session.RuntimeLeaseRequest{City: w.CityPath, Name: name}
	if front != nil {
		req.ID, req.TTL = row.ID, ttl
	}
	lease, err = session.TryRuntimeLease(front, req)
	switch {
	case err == nil:
		return name, lease, "", nil
	case errors.Is(err, session.ErrRuntimeLeaseBusy):
		return name, nil, causeNameBusy, err
	case errors.Is(err, session.ErrRuntimeLeaseNoCAS):
		return name, nil, causeLeaseNoCAS, err
	case errors.Is(err, session.ErrRuntimeLeaseRowClosed):
		return name, nil, causeLeaseRowClosed, err
	case errors.Is(err, session.ErrRuntimeLeaseContention):
		return name, nil, causeLeaseContended, err
	case errors.Is(err, session.ErrRuntimeLeaseRenamed):
		return name, nil, causeLeaseRenamed, err
	case errors.Is(err, session.ErrRuntimeLeaseLocalFS):
		return name, nil, causeLeaseLocalFS, err
	}
	return name, nil, causeLeaseStore, err
}
