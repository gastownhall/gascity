package main

import (
	"context"
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
// session.ErrRuntimeLeaseBusy, and the caller defers. A relative city path
// has no runtime dir to lock in, and refuses (session.ErrRuntimeLeaseNoCity). The
// empty path is legacy's "no city" (as the session Manager reads it): its
// callers without one, all in tests, lock nothing, and lease is nil.
// release is idempotent.
func tryRuntimeLease(store beads.Store, cityPath, name, id string, ttl time.Duration) (lease *session.RuntimeLease, release func(), err error) {
	return waitRuntimeLease(context.Background(), store, cityPath, name, id, ttl, 0)
}

// waitRuntimeLease is tryRuntimeLease waiting up to bound, and while ctx
// lasts, for a busy name.
func waitRuntimeLease(ctx context.Context, store beads.Store, cityPath, name, id string, ttl, bound time.Duration) (lease *session.RuntimeLease, release func(), err error) {
	switch {
	case cityPath == "":
		return nil, func() {}, nil
	case !filepath.IsAbs(cityPath):
		return nil, nil, fmt.Errorf("%w: %q", session.ErrRuntimeLeaseNoCity, cityPath)
	}
	var front *session.Store
	if store != nil && id != "" {
		front = sessionFrontDoor(store)
	} else {
		id = ""
	}
	if lease, err = session.WaitRuntimeLease(ctx, front, session.RuntimeLeaseRequest{City: cityPath, Name: name, ID: id, TTL: ttl}, bound); err != nil {
		return nil, nil, err
	}
	return lease, lease.Release, nil
}

// controllerStopLease takes, without waiting, the runtime lease a controller
// stop sequence holds across all its kills (a kill, its confirm-dead
// re-kills, an escalation's process-table kill), and returns ctx carrying it
// to each. A lease failure other than busy (the store unreachable, the row
// closed) is logged, and the sequence runs under the name's flock alone: a
// store never holds a stop hostage. Busy is session.ErrRuntimeLeaseBusy, which
// the caller defers to a later tick. Without a city path there is nothing to
// lock.
func controllerStopLease(store beads.Store, cityPath, name, sessionID string, stderr io.Writer) (context.Context, func(), error) {
	if cityPath == "" {
		return session.WithoutLeaseWait(context.Background()), func() {}, nil // no city, no names to lock
	}
	lease, release, err := tryRuntimeLease(store, cityPath, name, sessionID, session.RuntimeLeaseTTL(0))
	if err != nil && !errors.Is(err, session.ErrRuntimeLeaseBusy) {
		fmt.Fprintf(stderr, "session reconciler: stopping %s under the name's flock alone: %v\n", name, err) //nolint:errcheck
		lease, release, err = tryRuntimeLease(nil, cityPath, name, "", 0)
	}
	if err != nil {
		return nil, func() {}, err
	}
	return session.ContextWithRuntimeLease(session.WithoutLeaseWait(context.Background()), lease), release, nil
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
		return name, nil, causeLeaseNoCity, fmt.Errorf("%w: %q", session.ErrRuntimeLeaseNoCity, w.CityPath)
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
