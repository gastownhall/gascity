package workspacesvc

import "time"

// proxyProcessRestartBackoffMax is the ceiling of the respawn backoff.
const proxyProcessRestartBackoffMax = 5 * time.Minute

// spawnBackoffDelay returns how long to wait after the failures-th consecutive
// failed start before the next attempt.
func spawnBackoffDelay(failures int) time.Duration {
	_ = failures
	return proxyProcessRestartBackoff
}
