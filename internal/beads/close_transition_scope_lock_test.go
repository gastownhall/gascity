package beads

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/unix"
)

func waitForCloseTransitionScopeRefs(t *testing.T, scope string) {
	t.Helper()
	const want = 2
	deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer deadline.Stop()
	key := closeTransitionScopeKey(scope)
	for {
		lifecycleMutationMutexRegistry.Lock()
		entry := lifecycleMutationMutexRegistry.entries[key]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		lifecycleMutationMutexRegistry.Unlock()
		if refs >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("close transition scope %q refs = %d, want at least %d", scope, refs, want)
		default:
			runtime.Gosched()
		}
	}
}

func TestLockCloseTransitionScopeUsesStableFileAndReleases(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	unlock, err := lockCloseTransitionScope(scope)
	if err != nil {
		t.Fatalf("first lockCloseTransitionScope: %v", err)
	}
	lockPath := filepath.Join(beadsDir, closeTransitionScopeLockFilename)
	if _, err := os.Stat(lockPath); err != nil {
		unlock()
		t.Fatalf("stable scope lock %s: %v", lockPath, err)
	}
	unlock()

	done := make(chan error, 1)
	go func() {
		secondUnlock, err := lockCloseTransitionScope(scope)
		if err == nil {
			secondUnlock()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second lockCloseTransitionScope: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("scope lock remained held after unlock")
	}
}

func TestLockCloseTransitionScopeSerializesFallbackWithoutBeadsDirectory(t *testing.T) {
	scope := filepath.Join(t.TempDir(), "direct-store")
	unlock, err := lockCloseTransitionScope(scope)
	if err != nil {
		t.Fatalf("first lockCloseTransitionScope: %v", err)
	}

	acquired := make(chan error, 1)
	releaseSecond := make(chan struct{})
	go func() {
		secondUnlock, err := lockCloseTransitionScope(scope)
		if err != nil {
			acquired <- err
			return
		}
		acquired <- nil
		<-releaseSecond
		secondUnlock()
	}()
	waitForCloseTransitionScopeRefs(t, scope)
	select {
	case err := <-acquired:
		unlock()
		if err != nil {
			t.Fatalf("second lockCloseTransitionScope: %v", err)
		}
		t.Fatal("fallback scope lock did not serialize callers")
	default:
	}

	unlock()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("second lockCloseTransitionScope: %v", err)
		}
		close(releaseSecond)
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("fallback scope waiter did not acquire after unlock")
	}
}

func TestLifecycleMutationLeaseRejectsSymlinkLockWithoutClobberingTarget(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	lockPath := filepath.Join(scope, ".beads", lifecycleMutationLockFilename)
	victimPath := filepath.Join(scope, "victim")
	wantVictim := []byte("must remain unchanged\n")
	if err := os.WriteFile(victimPath, wantVictim, 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(victimPath, lockPath); err != nil {
		t.Fatalf("symlink lifecycle lock to victim: %v", err)
	}

	lease, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if lease != nil {
		lease.Unlock()
	}
	if err == nil {
		t.Fatal("acquire lifecycle mutation lease through symlink succeeded")
	}
	gotVictim, readErr := os.ReadFile(victimPath)
	if readErr != nil {
		t.Fatalf("read victim after rejected acquisition: %v", readErr)
	}
	if !bytes.Equal(gotVictim, wantVictim) {
		t.Fatalf("victim content = %q, want unchanged %q", gotVictim, wantVictim)
	}
	proofs, err := filepath.Glob(filepath.Join(scope, ".beads", "gc-lifecycle-mutation.owner-*.lock"))
	if err != nil {
		t.Fatalf("glob lifecycle owner proofs: %v", err)
	}
	if len(proofs) != 0 {
		t.Fatalf("lifecycle owner proofs after failed initialization = %v, want none", proofs)
	}
}

func TestLifecycleMutationOwnerProofCreationRejectsExistingLeaf(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			scope := newLifecycleMutationLeaseScope(t)
			root, err := openLifecycleMutationScopeLock(scope)
			if err != nil {
				t.Fatalf("open root lifecycle lock: %v", err)
			}
			if root == nil {
				t.Fatal("open root lifecycle lock returned nil")
			}
			defer root.Close() //nolint:errcheck // assertions below are authoritative

			victimPath := filepath.Join(scope, "victim")
			wantVictim := []byte("must remain unchanged\n")
			if err := os.WriteFile(victimPath, wantVictim, 0o600); err != nil {
				t.Fatalf("write victim: %v", err)
			}
			proofName := lifecycleMutationOwnerProofLockFilename(strings.Repeat("a", 64))
			proofPath := filepath.Join(scope, ".beads", proofName)
			switch kind {
			case "symlink":
				if err := os.Symlink(victimPath, proofPath); err != nil {
					t.Fatalf("create proof symlink: %v", err)
				}
			case "hardlink":
				if err := os.Link(victimPath, proofPath); err != nil {
					t.Fatalf("create proof hardlink: %v", err)
				}
			}

			proof, err := openLifecycleMutationChildFileLock(root, proofName, true, true)
			if proof != nil {
				_ = proof.Close()
			}
			if err == nil {
				t.Fatal("exclusive lifecycle owner proof creation through existing leaf succeeded")
			}
			gotVictim, readErr := os.ReadFile(victimPath)
			if readErr != nil {
				t.Fatalf("read victim: %v", readErr)
			}
			if !bytes.Equal(gotVictim, wantVictim) {
				t.Fatalf("victim content = %q, want unchanged %q", gotVictim, wantVictim)
			}
		})
	}
}

func TestLifecycleMutationLeaseRejectsReplacedOwnerRecordWithoutClobberingTarget(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire lifecycle mutation owner: %v", err)
	}
	ownerHeld := true
	defer func() {
		if ownerHeld {
			owner.Unlock()
		}
	}()

	ownerEnv := owner.CommandEnv()
	ownerDelegation, ok := parseLifecycleMutationDelegation(ownerEnv[lifecycleMutationTokenEnv])
	if !ok {
		t.Fatalf("owner delegation token = %q, want valid delegation", ownerEnv[lifecycleMutationTokenEnv])
	}
	mismatchedRoot := []byte(ownerDelegation.rootToken)
	if mismatchedRoot[0] == '0' {
		mismatchedRoot[0] = '1'
	} else {
		mismatchedRoot[0] = '0'
	}
	mismatched := lifecycleMutationInheritance{
		scope: ownerEnv[lifecycleMutationScopeEnv],
		token: encodeLifecycleMutationDelegation(string(mismatchedRoot), ownerDelegation.generation),
	}
	started, contenderDone := startLifecycleMutationLeaseAttempt(scope, mismatched)
	<-started
	waitForCloseTransitionScopeRefs(t, scope)

	lockPath := filepath.Join(scope, ".beads", lifecycleMutationLockFilename)
	parkedLockPath := filepath.Join(scope, ".beads", "parked-lifecycle-mutation.lock")
	if err := os.Rename(lockPath, parkedLockPath); err != nil {
		t.Fatalf("park contended lifecycle lock: %v", err)
	}
	victimPath := filepath.Join(scope, "victim")
	wantVictim := []byte("must not be reopened as a lock\n")
	if err := os.WriteFile(victimPath, wantVictim, 0o600); err != nil {
		t.Fatalf("write replacement victim: %v", err)
	}
	if err := os.Symlink(victimPath, lockPath); err != nil {
		t.Fatalf("replace lifecycle lock path with symlink: %v", err)
	}

	owner.Unlock()
	ownerHeld = false
	contender := waitLifecycleMutationLeaseAttempt(t, contenderDone)
	if contender.err == nil {
		t.Fatal("contender acquired through replaced symlink owner record")
	}
	gotVictim, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("read replacement victim: %v", err)
	}
	if !bytes.Equal(gotVictim, wantVictim) {
		t.Fatalf("replacement victim content = %q, want unchanged %q", gotVictim, wantVictim)
	}
}

func TestLifecycleMutationLeaseRootDirectoryFlockSurvivesRegularOwnerRecordReplacement(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	owner, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire lifecycle mutation owner: %v", err)
	}
	ownerHeld := true
	defer func() {
		if ownerHeld {
			owner.Unlock()
		}
	}()

	lockPath := filepath.Join(scope, ".beads", lifecycleMutationLockFilename)
	parkedLockPath := filepath.Join(scope, ".beads", "parked-lifecycle-mutation.lock")
	if err := os.Rename(lockPath, parkedLockPath); err != nil {
		t.Fatalf("park lifecycle owner record: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("regular replacement\n"), 0o600); err != nil {
		t.Fatalf("write lifecycle owner record replacement: %v", err)
	}

	acquiredPath := filepath.Join(scope, "fresh-contender-acquired")
	helper := startLifecycleMutationSiblingHelper(t, scope, acquiredPath, "-", "-", nil)
	waitForLifecycleMutationHelperFile(t, acquiredPath+".attempting", helper)
	select {
	case err := <-helper.done:
		t.Fatalf("fresh contender crossed replaced owner record while directory lease held: %v; output=%s", err, helper.output)
	case <-time.After(250 * time.Millisecond):
	}
	if _, err := os.Stat(acquiredPath); !os.IsNotExist(err) {
		t.Fatalf("fresh contender acquired through replacement while owner held lease: stat err=%v", err)
	}

	owner.Unlock()
	ownerHeld = false
	waitForLifecycleMutationSiblingHelper(t, helper)
	if _, err := os.Stat(acquiredPath); err != nil {
		t.Fatalf("fresh contender did not acquire after owner release: %v", err)
	}
}

func TestLifecycleMutationLeaseRejectsHardlinkedOwnerRecord(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	victimPath := filepath.Join(scope, "victim")
	wantVictim := []byte("must remain unchanged\n")
	if err := os.WriteFile(victimPath, wantVictim, 0o600); err != nil {
		t.Fatalf("write hardlink victim: %v", err)
	}
	lockPath := filepath.Join(scope, ".beads", lifecycleMutationLockFilename)
	if err := os.Link(victimPath, lockPath); err != nil {
		t.Fatalf("hardlink lifecycle owner record to victim: %v", err)
	}

	lease, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if lease != nil {
		lease.Unlock()
	}
	if err == nil {
		t.Fatal("acquire lifecycle mutation lease through hardline owner record succeeded")
	}
	gotVictim, readErr := os.ReadFile(victimPath)
	if readErr != nil {
		t.Fatalf("read hardlink victim after rejected acquisition: %v", readErr)
	}
	if !bytes.Equal(gotVictim, wantVictim) {
		t.Fatalf("hardlink victim content = %q, want unchanged %q", gotVictim, wantVictim)
	}
}

func TestLifecycleMutationLeaseToleratesGroupWritableBeadsDirectory(t *testing.T) {
	// A .beads created under umask 0002 / a setgid shared group is a legitimate
	// multi-writer layout, not a hijack: the lease must still acquire (relying on
	// flock coordination) rather than fail every lifecycle mutation.
	scope := newLifecycleMutationLeaseScope(t)
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.Chmod(beadsDir, 0o775); err != nil {
		t.Fatalf("make .beads group-writable: %v", err)
	}

	lease, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if err != nil {
		t.Fatalf("acquire lifecycle mutation lease in group-writable .beads directory: %v", err)
	}
	if lease == nil {
		t.Fatal("group-writable .beads directory produced no lease")
	}
	lease.Unlock()
}

func TestLifecycleMutationLeaseRejectsWorldWritableBeadsDirectory(t *testing.T) {
	// A world-writable .beads directory is a genuine hijack vector — any local
	// account can swap the lock inode — so it must stay a hard failure.
	scope := newLifecycleMutationLeaseScope(t)
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.Chmod(beadsDir, 0o777); err != nil {
		t.Fatalf("make .beads world-writable: %v", err)
	}

	lease, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if lease != nil {
		lease.Unlock()
	}
	if err == nil {
		t.Fatal("acquire lifecycle mutation lease in world-writable .beads directory succeeded")
	}
}

func TestLifecycleMutationLockValidationDowngradesForeignOwnerToWarning(t *testing.T) {
	foreignUID := uint32(os.Geteuid()) + 1
	if foreignUID == uint32(os.Geteuid()) {
		foreignUID--
	}
	// A foreign-owned but otherwise safe lock object (controller service user vs a
	// human's gc) is accepted so shared-permission deployments keep mutating.
	for _, tt := range []struct {
		name  string
		stat  unix.Stat_t
		check func(string, *unix.Stat_t) error
	}{
		{
			name:  "directory",
			stat:  unix.Stat_t{Mode: unix.S_IFDIR | 0o755, Uid: foreignUID},
			check: validateLifecycleMutationDirectoryStat,
		},
		{
			name:  "leaf",
			stat:  unix.Stat_t{Mode: unix.S_IFREG | 0o600, Nlink: 1, Uid: foreignUID},
			check: validateLifecycleMutationLeafStat,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.check("foreign-owner-"+tt.name, &tt.stat); err != nil {
				t.Fatalf("foreign-owned lifecycle mutation lock object rejected: %v", err)
			}
		})
	}
}

func TestLifecycleMutationLockValidationRejectsDangerousLayouts(t *testing.T) {
	// The genuinely dangerous layouts still fail closed: a world-writable
	// directory and a hardline (nlink>1) lock leaf.
	for _, tt := range []struct {
		name  string
		stat  unix.Stat_t
		check func(string, *unix.Stat_t) error
	}{
		{
			name:  "world-writable-directory",
			stat:  unix.Stat_t{Mode: unix.S_IFDIR | 0o707, Uid: uint32(os.Geteuid())},
			check: validateLifecycleMutationDirectoryStat,
		},
		{
			name:  "hardline-leaf",
			stat:  unix.Stat_t{Mode: unix.S_IFREG | 0o600, Nlink: 2, Uid: uint32(os.Geteuid())},
			check: validateLifecycleMutationLeafStat,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.check("dangerous-"+tt.name, &tt.stat); err == nil {
				t.Fatalf("dangerous lifecycle mutation lock layout %q was accepted", tt.name)
			}
		})
	}
}

func TestLifecycleMutationLeaseRejectsSymlinkedBeadsDirectory(t *testing.T) {
	scope := t.TempDir()
	realBeadsDir := filepath.Join(scope, "real-beads")
	if err := os.Mkdir(realBeadsDir, 0o755); err != nil {
		t.Fatalf("mkdir real beads directory: %v", err)
	}
	if err := os.Symlink(realBeadsDir, filepath.Join(scope, ".beads")); err != nil {
		t.Fatalf("symlink .beads directory: %v", err)
	}

	lease, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
	if lease != nil {
		lease.Unlock()
	}
	if err == nil {
		t.Fatal("acquire lifecycle mutation lease through symlinked .beads directory succeeded")
	}
}

func TestLifecycleMutationLeaseCreatesAndRepairsPrivateLockMode(t *testing.T) {
	for _, tt := range []struct {
		name      string
		precreate bool
	}{
		{name: "new"},
		{name: "preexisting", precreate: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := newLifecycleMutationLeaseScope(t)
			lockPath := filepath.Join(scope, ".beads", lifecycleMutationLockFilename)
			if tt.precreate {
				if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
					t.Fatalf("precreate lifecycle mutation lock: %v", err)
				}
			}
			lease, err := acquireLifecycleMutationLease(scope, lifecycleMutationInheritance{})
			if err != nil {
				t.Fatalf("acquire lifecycle mutation lease: %v", err)
			}
			lease.Unlock()
			info, err := os.Stat(lockPath)
			if err != nil {
				t.Fatalf("stat lifecycle mutation lock: %v", err)
			}
			if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
				t.Fatalf("lifecycle mutation lock mode = %04o, want %04o", got, want)
			}
		})
	}
}
