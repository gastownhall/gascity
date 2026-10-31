//go:build linux || darwin

package relay

import "time"

// deathWait bounds how long dieBySignal waits for a signal it sent its own
// process to land: delivery to another thread is asynchronous.
const deathWait = time.Second

// awaitDeath waits for a fatal signal already sent to this process to end
// it; it returns only if the signal did not.
func awaitDeath() {
	time.Sleep(deathWait)
}
