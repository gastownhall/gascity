package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// coTenantLiveness says why another city that shares a store this city's
// split imports is live, or "" when nothing shows it is (S5-H, owner
// decision: a live rig co-tenant still refuses at the rig step, and the
// rehearsal reports it up front). It takes no lock: a rehearsal that held a
// co-tenant's controller lock, even for an instant, could fail that city's
// start. It asks the controller's socket, then who holds the controller
// lock, where the platform can say (procfs on Linux, lsof on Darwin). A
// holder that is this process's own co-tenant exclusion (coTenantLocks) is
// not the co-tenant. A test seam.
var coTenantLiveness = func(otherPath string) string {
	if pid := controllerAlive(otherPath); pid != 0 {
		return fmt.Sprintf("its controller, PID %d, answers", pid)
	}
	lockPath := filepath.Join(otherPath, ".gc", "controller.lock")
	if _, err := os.Stat(lockPath); err != nil {
		return ""
	}
	coTenantLocks.Lock()
	ours := coTenantLocks.held[canonicalIdentityPath(otherPath)] != nil
	coTenantLocks.Unlock()
	if ours {
		return ""
	}
	pids, err := managedDoltLockHolderPIDs(lockPath, "/proc/locks")
	if err != nil || len(pids) == 0 {
		return ""
	}
	return fmt.Sprintf("PID %d holds its controller lock", pids[0])
}
