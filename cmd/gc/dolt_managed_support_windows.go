//go:build windows

package main

import "fmt"

// managedDoltNativeIdentitySupported reports that managed Dolt lifecycle
// actions have no safe implementation on Windows yet.
//
// Every destructive step in that lifecycle is fenced by an ownership proof —
// the PID's creation time, its --config/--data-dir arguments, its working
// directory, and which process holds the port — and each of those proofs is
// built from procfs reads, lsof, and ps. None of them answer on Windows.
//
// Without the proof the code cannot distinguish "this is not our server" from
// "we cannot tell", and the two have opposite safe actions: the first clears
// the recorded runtime state, the second must leave it alone because the server
// may still be serving. Reporting a stop that did not happen loses the only
// record of a live server, and the data directory it is still writing to then
// looks free to back up, move, or delete.
//
// Lifting this gate needs native equivalents first: PID plus creation-time
// identity, command-line and config inspection, working-directory and data-dir
// comparison, and TCP port ownership.
func managedDoltNativeIdentitySupported() error {
	return fmt.Errorf("managed Dolt lifecycle actions are unsupported on Windows: %w", errManagedDoltIdentityUnavailable)
}
