package main

import (
	"fmt"
	"strconv"
	"time"
)

// managedDoltInitReadyTimeout is the maximum time waitForManagedDoltInitReady
// will block waiting for a running managed Dolt process to become TCP-reachable.
var managedDoltInitReadyTimeout = 60 * time.Second

// managedDoltInitReadyPollInterval is how often waitForManagedDoltInitReady
// polls for runtime-state publication and TCP reachability.
var managedDoltInitReadyPollInterval = 200 * time.Millisecond

// Injectable function vars for unit testing.
var (
	managedDoltInitWaitIsManaged = cityUsesManagedDoltBeadsLifecycle
	managedDoltInitWaitReadState = readValidProviderManagedDoltState
	managedDoltInitWaitReachable = managedDoltTCPReachable
	managedDoltInitWaitPidAlive  = pidAlive
)

// waitForManagedDoltInitReady blocks until the managed Dolt process for
// cityPath is TCP-reachable, or timeout expires, or the process exits.
//
// Returns nil immediately when cityPath is not a managed-Dolt city, so
// callers can invoke it unconditionally.
//
// Callers should use the production default via the package-level timeout var:
//
//	err := waitForManagedDoltInitReady(cityPath, managedDoltInitReadyTimeout)
func waitForManagedDoltInitReady(cityPath string, timeout time.Duration) error {
	if !managedDoltInitWaitIsManaged(cityPath) {
		return nil
	}
	if timeout <= 0 {
		timeout = managedDoltInitReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		state, ok := managedDoltInitWaitReadState(cityPath)
		if ok && state.Port > 0 {
			if state.PID > 0 && !managedDoltInitWaitPidAlive(state.PID) {
				return fmt.Errorf("managed Dolt process (pid %d) exited before becoming TCP-ready", state.PID)
			}
			host := managedDoltConnectHost("")
			port := strconv.Itoa(state.Port)
			if managedDoltInitWaitReachable(host, port) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if ok && state.Port > 0 {
				return fmt.Errorf("timed out after %s waiting for managed Dolt on port %d to become TCP-ready", timeout, state.Port)
			}
			return fmt.Errorf("timed out after %s waiting for managed Dolt runtime state to be published", timeout)
		}
		time.Sleep(managedDoltInitReadyPollInterval)
	}
}
