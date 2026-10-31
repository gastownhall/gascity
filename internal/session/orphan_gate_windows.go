//go:build windows

package session

// orphanScanErrorsRefuseStart reports whether the pre-start orphan gate
// (killExistingOrphans) refuses a start when its process-table scan fails or
// surfaces a root it cannot classify. On Windows it does: a process another
// integrity level started can be running yet unreadable here, so an incomplete
// scan cannot establish that no orphan of the session survives.
const orphanScanErrorsRefuseStart = true
