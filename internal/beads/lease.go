package beads

import "errors"

// ErrLeaseRenewalUnsupported reports that a store cannot renew claim leases.
// Callers sweeping mixed store fleets treat it as "skip this store", not as a
// failed renewal.
var ErrLeaseRenewalUnsupported = errors.New("claim lease renewal unsupported")

// LeaseRenewer renews the claim lease on an in_progress bead on behalf of its
// current holder. The backend enforces holder identity: only the bead's
// current assignee may renew, so implementations act as holder rather than as
// the store's own identity. Stores whose backend has no lease semantics simply
// do not implement it (gas-76r).
type LeaseRenewer interface {
	RenewLease(id, holder string) error
}
