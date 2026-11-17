//go:build !windows

package session

// orphanScanErrorsRefuseStart reports whether the pre-start orphan gate
// (killExistingOrphans) refuses a start when its process-table scan fails or
// surfaces a root it cannot classify. Off Windows it does not: the gate logs
// the error and proceeds once it has dealt with the roots the scan returned, as
// ProcessTableScanner allows.
const orphanScanErrorsRefuseStart = false
