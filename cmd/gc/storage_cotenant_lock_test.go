package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReverseToWorkWithACoTenantSharingTheWorkStoreAndARig: one co-tenant
// city shares this city's work store and its rig store, the operator attests
// the store, and the co-tenant is stopped. The reverse takes the co-tenant's
// controller lock for the work store and again for the rig; the second take,
// on a fresh descriptor in the same process, used to conflict with the first
// and refuse the reverse as if the stopped co-tenant held its own lock.
func TestReverseToWorkWithACoTenantSharingTheWorkStoreAndARig(t *testing.T) {
	r := newReverseRigFixture(t)
	r.writeSplitCityTOML(t)
	other := otherCityWithRig(t, filepath.Join(t.TempDir(), "elsewhere"))
	r.otherCities = []string{other}
	stubStoreIdentity(t, storeDoltIdentity{Database: "bd_shared", Endpoint: "loopback:3399"})
	r.request.Force = storageForce{SharedStores: []string{"bd_shared@loopback:3399"}}

	code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToWork})
	if code != 0 {
		t.Fatalf("--to-work beside a stopped co-tenant of the work store and a rig = %d: %s%s", code, stdout, stderr)
	}
	r.assertRigAsSeeded(t)
	lock, err := acquireControllerLock(other)
	if err != nil {
		t.Fatalf("the reverse left the co-tenant's controller lock held: %v", err)
	}
	lock.Close() //nolint:errcheck // test cleanup
}

// TestCoTenantLockIsSharedInProcess: a second take of one co-tenant's lock
// in this process shares the held one, a release drops it only with the last
// holder, and a lock another process (here: a descriptor outside the
// registry) holds still refuses.
func TestCoTenantLockIsSharedInProcess(t *testing.T) {
	other := t.TempDir()
	first, err := acquireCoTenantControllerLock(other)
	if err != nil {
		t.Fatal(err)
	}
	// Another spelling of the same city: the registry keys the canonical path.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	second, err := acquireCoTenantControllerLock(link)
	if err != nil {
		t.Fatalf("a second take in this process = %v, want the held lock shared", err)
	}
	first.release()
	first.release() // a second release of one share drops nothing more
	if _, err := acquireControllerLock(other); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("the lock was dropped while a holder remained: %v", err)
	}
	second.release()
	outside, err := acquireControllerLock(other)
	if err != nil {
		t.Fatalf("the last release left the lock held: %v", err)
	}
	defer outside.Close() //nolint:errcheck // test cleanup
	if _, err := acquireCoTenantControllerLock(other); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("a lock held outside the registry = %v, want refused", err)
	}
}

// TestSharedStoreRefusalsShareOneCoTenantLock: two exclusions over one
// co-tenant (the work store's and a rig's) both hold it, and it is released
// only when both are.
func TestSharedStoreRefusalsShareOneCoTenantLock(t *testing.T) {
	request, _ := exclusionCity(t)
	other := sharedStoreCities(t, request)
	force := storageForce{SharedStores: []string{sharedToken}}
	city, err := sharedWorkStoreRefusal(request.CityPath, force)
	if err != nil {
		t.Fatal(err)
	}
	rig, err := sharedStoreRefusal(request.CityPath, "rig packs", t.TempDir(), force)
	if err != nil {
		t.Fatalf("the rig's exclusion over the same co-tenant = %v", err)
	}
	if strings.TrimSpace(other) == "" {
		t.Fatal("no co-tenant")
	}
	city.Release()
	if _, err := acquireControllerLock(other); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("releasing one exclusion freed a co-tenant the other still holds: %v", err)
	}
	rig.Release()
	lock, err := acquireControllerLock(other)
	if err != nil {
		t.Fatalf("both exclusions released, the co-tenant is still locked: %v", err)
	}
	lock.Close() //nolint:errcheck // test cleanup
}
