package main

import (
	"strings"
	"sync"
	"time"
)

// routedWorkAllocationKey is the exact key the keyed pool allocation owns: one
// routed work item, its pool target, and the store it lives in. It is the same
// triple the pool-allocation hint carries and the same one the durable claim
// stamps at create.
type routedWorkAllocationKey struct {
	WorkID      string
	PoolTarget  string
	SourceStore string
}

func routedWorkAllocationKeyFor(workID, poolTarget, sourceStore string) routedWorkAllocationKey {
	return routedWorkAllocationKey{
		WorkID:      strings.TrimSpace(workID),
		PoolTarget:  strings.TrimSpace(poolTarget),
		SourceStore: strings.TrimSpace(sourceStore),
	}
}

func (k routedWorkAllocationKey) valid() bool {
	return k.WorkID != "" && k.PoolTarget != ""
}

// routedWorkAllocationReservationLapse bounds how long one reservation can fence
// the legacy pool builder. It is a LAPSE bound, not a schedule: the normal
// release is handleRoutedWorkPoolAllocation's deferred release, which runs on
// every path including refusal and error. The bound exists because a keyed
// controller that stops between enqueue and handling would otherwise fence
// legacy forever — the ga-ij8mh round-6 no-lapse hazard, applied to this
// boundary. Two default patrol intervals is long enough that no ordinary
// allocation ever reaches it and short enough that a lapse costs one extra
// patrol.
const routedWorkAllocationReservationLapse = 60 * time.Second

// routedWorkAllocationReservations is the ALLOCATION-OWNERSHIP SEAM: the yield
// signal that makes the keyed allocation, not the legacy pool builder, the
// winner of a routed work item's materialization (ga-f7v2ft.126's cutover arm).
//
// Before it, the two builders were only mutually exclusive AFTER a member
// existed: revalidatePlannedPoolMemberDemand re-reads the durable claim the
// keyed allocation stamps at CREATE, so first-creator-wins — and legacy, which
// plans from a per-tick snapshot and creates immediately, usually created first.
// The keyed side's ownership began too late to be a yield.
//
// The reservation moves ownership to the moment the exact key enters the keyed
// lane. It is the round-6 stand-down template applied at the pool-member
// creation boundary: legacy consults the installed keyed seam on CURRENT state,
// inside the serialization point the seam itself provides, with full-supersede
// semantics — the planned member is not created and contributes no demand — and
// with a bounded lapse so a released or stranded reservation leaves legacy free
// on its very next pass.
//
// It is process-wide for the same reason the respawn circuit breaker is: the
// legacy pool builder is a free function reached through a build-fn value that
// carries no runtime handle, and threading a predicate through every caller of
// buildDesiredStateWithSessionBeads would be a far larger edit than the seam.
type routedWorkAllocationReservations struct {
	mu   sync.Mutex
	held map[routedWorkAllocationKey]routedWorkAllocationReservation
}

type routedWorkAllocationReservation struct {
	count      int
	reservedAt time.Time
}

// keyedRoutedWorkAllocations is the installed seam. A city with no keyed pool
// allocation never reserves, so every predicate answers false and the legacy
// builder behaves exactly as before.
var keyedRoutedWorkAllocations = &routedWorkAllocationReservations{}

// reserve claims the key for the keyed allocation lane. Repeat reservations of
// the same key coalesce by count, so a replayed hint does not release the
// original owner's fence when it completes.
func (r *routedWorkAllocationReservations) reserve(key routedWorkAllocationKey, now time.Time) {
	if r == nil || !key.valid() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held == nil {
		r.held = make(map[routedWorkAllocationKey]routedWorkAllocationReservation)
	}
	existing, ok := r.held[key]
	if !ok || now.Sub(existing.reservedAt) >= routedWorkAllocationReservationLapse {
		r.held[key] = routedWorkAllocationReservation{count: 1, reservedAt: now}
		return
	}
	existing.count++
	r.held[key] = existing
}

// release drops one claim on the key. The last release retires the reservation,
// which is what makes the stand-down lease-triggered rather than
// candidacy-triggered: legacy is free again on its next pass.
func (r *routedWorkAllocationReservations) release(key routedWorkAllocationKey) {
	if r == nil || !key.valid() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.held[key]
	if !ok {
		return
	}
	if existing.count <= 1 {
		delete(r.held, key)
		return
	}
	existing.count--
	r.held[key] = existing
}

// owns reports whether the keyed allocation lane currently holds the key. It
// answers on CURRENT state — a reservation past the lapse bound is retired here
// rather than fencing forever, so a stranded reservation costs one patrol and
// never wedges the fleet.
func (r *routedWorkAllocationReservations) owns(key routedWorkAllocationKey, now time.Time) bool {
	if r == nil || !key.valid() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.held[key]
	if !ok {
		return false
	}
	if now.Sub(existing.reservedAt) >= routedWorkAllocationReservationLapse {
		delete(r.held, key)
		return false
	}
	return true
}

// reset drops every reservation. Tests use it to isolate the process-wide seam.
func (r *routedWorkAllocationReservations) reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.held = nil
	r.mu.Unlock()
}
