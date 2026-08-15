package beads

import (
	"errors"
	"testing"
)

// leaseRenewingBackingStore wraps a Store with a recording RenewLease so the
// CachingStore delegation tests can observe forwarding without a real bd.
type leaseRenewingBackingStore struct {
	Store
	renewCalls []string
	renewErr   error
}

func (l *leaseRenewingBackingStore) RenewLease(id, holder string) error {
	l.renewCalls = append(l.renewCalls, id+"/"+holder)
	return l.renewErr
}

// TestCachingStoreRenewLeaseDelegatesToBacking proves the caching layer
// forwards renewals to a lease-capable backing store. Lease fields are not
// part of gc's Bead projection, so no cache refresh is required — the renewal
// only has to reach the backend.
func TestCachingStoreRenewLeaseDelegatesToBacking(t *testing.T) {
	backing := &leaseRenewingBackingStore{Store: NewMemStore()}
	c := NewCachingStoreForTest(backing, nil)

	if err := c.RenewLease("gas-1", "holder-a"); err != nil {
		t.Fatalf("RenewLease() = %v, want nil", err)
	}
	if len(backing.renewCalls) != 1 || backing.renewCalls[0] != "gas-1/holder-a" {
		t.Fatalf("backing renew calls = %v, want [gas-1/holder-a]", backing.renewCalls)
	}
}

// TestCachingStoreRenewLeaseWithoutCapabilityReportsUnsupported proves a
// backing store without lease semantics (e.g. the native SQLite store) is
// surfaced as ErrLeaseRenewalUnsupported so callers can skip it rather than
// treat the miss as a renewal failure.
func TestCachingStoreRenewLeaseWithoutCapabilityReportsUnsupported(t *testing.T) {
	c := NewCachingStoreForTest(NewMemStore(), nil)

	err := c.RenewLease("gas-1", "holder-a")
	if !errors.Is(err, ErrLeaseRenewalUnsupported) {
		t.Fatalf("RenewLease() = %v, want ErrLeaseRenewalUnsupported", err)
	}
}
