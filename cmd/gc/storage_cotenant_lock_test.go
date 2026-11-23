package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
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
// registry) holds still refuses, and the refused take registers nothing.
func TestCoTenantLockIsSharedInProcess(t *testing.T) {
	other := t.TempDir()
	first, err := acquireCoTenantControllerLock(other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.release)
	// Another spelling of the same city: the registry keys the canonical path.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	second, err := acquireCoTenantControllerLock(link)
	if err != nil {
		t.Fatalf("a second take in this process = %v, want the held lock shared", err)
	}
	t.Cleanup(second.release)
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
	t.Cleanup(func() { outside.Close() }) //nolint:errcheck // test cleanup
	// Twice: a refused take that registered the lock anyway would hand the
	// second take a share of a lock nobody holds.
	for i := 0; i < 2; i++ {
		if lock, err := acquireCoTenantControllerLock(other); !errors.Is(err, errControllerAlreadyRunning) {
			if lock != nil {
				lock.release()
			}
			t.Fatalf("take %d of a lock held outside the registry = %v, want refused", i+1, err)
		}
	}
	outside.Close() //nolint:errcheck // the outside holder goes
	third, err := acquireCoTenantControllerLock(other)
	if err != nil {
		t.Fatalf("a take after the outside holder left = %v", err)
	}
	t.Cleanup(third.release)
	if lock, err := acquireControllerLock(other); !errors.Is(err, errControllerAlreadyRunning) {
		if lock != nil {
			lock.Close() //nolint:errcheck // test cleanup
		}
		t.Fatalf("the take after a refusal holds no lock: %v", err)
	}
}

// TestCoTenantLockConcurrentTakesShareOneLock: takes and releases racing in
// one process all share one lock, and the last release frees it. A take that
// checked the registry and then flocked without holding it across both would
// race another take of the same city into a second flock, which conflicts.
func TestCoTenantLockConcurrentTakesShareOneLock(t *testing.T) {
	other := t.TempDir()
	const workers = 16
	for round := 0; round < 60; round++ {
		var wg sync.WaitGroup
		locks := make([]*coTenantLock, workers)
		errs := make([]error, workers)
		start := make(chan struct{})
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				locks[i], errs[i] = acquireCoTenantControllerLock(other)
			}(i)
		}
		close(start)
		wg.Wait()
		for _, lock := range locks {
			if lock != nil {
				t.Cleanup(lock.release)
			}
		}
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, take %d: %v", round, i, err)
			}
		}
		if lock, err := acquireControllerLock(other); !errors.Is(err, errControllerAlreadyRunning) {
			if lock != nil {
				lock.Close() //nolint:errcheck // test cleanup
			}
			t.Fatalf("round %d: the lock is not held with %d shares: %v", round, workers, err)
		}
		release := make(chan struct{})
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-release
				locks[i].release()
				locks[i].release()
			}(i)
		}
		close(release)
		wg.Wait()
		lock, err := acquireControllerLock(other)
		if err != nil {
			t.Fatalf("round %d: every share released, the lock is still held: %v", round, err)
		}
		lock.Close() //nolint:errcheck // test cleanup
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
	t.Cleanup(city.Release)
	rig, err := sharedStoreRefusal(request.CityPath, "rig packs", t.TempDir(), force)
	if err != nil {
		t.Fatalf("the rig's exclusion over the same co-tenant = %v", err)
	}
	t.Cleanup(rig.Release)
	if len(city.locks) != 1 || len(rig.locks) != 1 {
		t.Fatalf("the exclusions hold %d and %d co-tenant locks, want 1 each", len(city.locks), len(rig.locks))
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

// TestSharedStoreExclusionReleasesEveryCoTenant: an exclusion over two
// co-tenants holds both, and its release frees both.
func TestSharedStoreExclusionReleasesEveryCoTenant(t *testing.T) {
	request, _ := exclusionCity(t)
	x, y := t.TempDir(), t.TempDir()
	for _, path := range []string{x, y} {
		if err := os.WriteFile(filepath.Join(path, "city.toml"), []byte("[workspace]\nname = \"co-tenant\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stubOtherCities(t, x, y)
	stubStoreIdentity(t, storeDoltIdentity{Database: "bd_shared", Endpoint: "loopback:3307"})
	held, err := sharedWorkStoreRefusal(request.CityPath, storageForce{SharedStores: []string{sharedToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(held.Release)
	for _, path := range []string{x, y} {
		if _, err := acquireControllerLock(path); !errors.Is(err, errControllerAlreadyRunning) {
			t.Fatalf("the exclusion does not hold co-tenant %s: %v", path, err)
		}
	}
	held.Release()
	for _, path := range []string{x, y} {
		lock, err := acquireControllerLock(path)
		if err != nil {
			t.Fatalf("the exclusion released, co-tenant %s is still locked: %v", path, err)
		}
		lock.Close() //nolint:errcheck // test cleanup
	}
}

// TestReverseToRigsWithACoTenantSharingTheWorkStoreAndARig: --to-rigs holds
// the work store's exclusion and the rig's at once, as --to-work does.
func TestReverseToRigsWithACoTenantSharingTheWorkStoreAndARig(t *testing.T) {
	r := newReverseRigFixture(t)
	other := otherCityWithRig(t, filepath.Join(t.TempDir(), "elsewhere"))
	r.otherCities = []string{other}
	stubStoreIdentity(t, storeDoltIdentity{Database: "bd_shared", Endpoint: "loopback:3399"})
	r.request.Force = storageForce{SharedStores: []string{"bd_shared@loopback:3399"}}

	code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	if code != 0 {
		t.Fatalf("--to-rigs beside a stopped co-tenant of the work store and a rig = %d: %s%s", code, stdout, stderr)
	}
	r.assertRigAsSeeded(t)
	lock, err := acquireControllerLock(other)
	if err != nil {
		t.Fatalf("the reverse left the co-tenant's controller lock held: %v", err)
	}
	lock.Close() //nolint:errcheck // test cleanup
}

// TestAbortReverseWithACoTenantSharingTheWorkStoreAndARig: --abort-reverse
// holds the work store's exclusion and the rig's at once, as --to-work does.
func TestAbortReverseWithACoTenantSharingTheWorkStoreAndARig(t *testing.T) {
	r := newReverseRigFixture(t)
	prev := infraEngineCrashPoint
	t.Cleanup(func() { infraEngineCrashPoint = prev })
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse rig:packs verify" {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	other := otherCityWithRig(t, filepath.Join(t.TempDir(), "elsewhere"))
	r.otherCities = []string{other}
	stubStoreIdentity(t, storeDoltIdentity{Database: "bd_shared", Endpoint: "loopback:3399"})
	r.request.Force = storageForce{SharedStores: []string{"bd_shared@loopback:3399"}}

	var stdout, stderr bytes.Buffer
	if code := doStorageAbortReverse(context.Background(), r.request, &stdout, &stderr); code != 0 {
		t.Fatalf("--abort-reverse beside a stopped co-tenant of the work store and a rig = %d: %s%s", code, stdout.String(), stderr.String())
	}
	lock, err := acquireControllerLock(other)
	if err != nil {
		t.Fatalf("the abort left the co-tenant's controller lock held: %v", err)
	}
	lock.Close() //nolint:errcheck // test cleanup
}
