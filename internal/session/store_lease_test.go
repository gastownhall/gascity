package session

import (
	"errors"
	"testing"
	"time"
)

// TestLeasedWritesRefuseALostLease: PreWake's CAS and the start commit write
// nothing once the lease was taken over.
func TestLeasedWritesRefuseALostLease(t *testing.T) {
	f := newLeaseFixture(t)
	onHost(t, "host-b")
	stale, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.front.Get(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.front.ApplyPatchIfLifecycleUnchangedUnder(snapshot, MetadataPatch{"generation": "2"}, stale); !ok || err != nil {
		t.Fatalf("PreWake under the lease = %v, %v", ok, err)
	}
	if ok, err := f.front.CommitStartedIfCurrentUnder(snapshot, MetadataPatch{"started_config_hash": "h1"}, stale); !ok || err != nil {
		t.Fatalf("commit under the lease = %v, %v", ok, err)
	}
	onHost(t, "host-a")
	taker, err := TryRuntimeLease(f.front, f.req(t.TempDir(), leaseT0.Add(leaseTTL)))
	if err != nil {
		t.Fatal(err)
	}
	defer taker.Release()
	if ok, err := f.front.ApplyPatchIfLifecycleUnchangedUnder(snapshot, MetadataPatch{"generation": "3"}, stale); ok || err != nil {
		t.Fatalf("PreWake under a lost lease = %v, %v; want refused", ok, err)
	}
	if ok, err := f.front.CommitStartedIfCurrentUnder(snapshot, MetadataPatch{"started_config_hash": "h2"}, stale); ok || err != nil {
		t.Fatalf("commit under a lost lease = %v, %v; want refused", ok, err)
	}
	if meta := f.meta(t); meta["generation"] != "2" || meta["started_config_hash"] != "h1" {
		t.Fatalf("row = %v, want the lost lease's writes refused", meta)
	}
}

// TestTakeRecordFailureLeavesTheLeaseFlockOnly: a record that cannot be taken
// leaves the name's flock held and the lease with no record, so its release
// writes nothing, and a later TakeRecord can still take one.
func TestTakeRecordFailureLeavesTheLeaseFlockOnly(t *testing.T) {
	f, city := newLeaseFixture(t), t.TempDir()
	l, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: city, Name: "s-lease"})
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, MetadataPatch{
		RuntimeLeaseHolderKey: "host-b/1/n", RuntimeLeaseEpochKey: "4", RuntimeLeaseTTLKey: "240",
		RuntimeLeaseExpiresKey: leaseT0.Add(time.Minute).Format(time.RFC3339),
	})
	if err := l.TakeRecord(f.front, f.req(city, leaseT0)); !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("TakeRecord over another holder = %v, want busy", err)
	}
	if l.Epoch() != 0 || l.id != "" || l.holder != "" {
		t.Fatalf("lease after a refused TakeRecord = epoch %d id %q holder %q, want flock-only", l.Epoch(), l.id, l.holder)
	}
	if err := l.TakeRecord(f.front, f.req(city, leaseT0.Add(time.Hour))); err != nil || l.Epoch() != 5 {
		t.Fatalf("TakeRecord after the other's expiry: %v, epoch %d", err, l.Epoch())
	}
	l.Release()
	if m := f.meta(t); m[RuntimeLeaseHolderKey] != "" || m[RuntimeLeaseEpochKey] != "5" {
		t.Fatalf("record after release = %v", m)
	}
}

// TestRuntimeLeaseTTLClampsANegativeStartupTimeout: a misconfigured negative
// startup_timeout never yields a TTL below the margin.
func TestRuntimeLeaseTTLClampsANegativeStartupTimeout(t *testing.T) {
	if got := RuntimeLeaseTTL(-5 * time.Minute); got != RuntimeLeaseMargin {
		t.Fatalf("RuntimeLeaseTTL(-5m) = %v, want %v", got, RuntimeLeaseMargin)
	}
}
