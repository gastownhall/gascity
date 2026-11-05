package workspacesvc

import (
	"log"
	"time"
)

// proxyProcessRestartBackoffMax is the ceiling of the respawn backoff: a
// service that keeps failing to start is retried at most once per this long.
const proxyProcessRestartBackoffMax = 5 * time.Minute

// spawnBackoffDelay returns how long to wait after the failures-th consecutive
// failed start before the next attempt. The first failure waits
// proxyProcessRestartBackoff and every further one doubles it, clamped at
// proxyProcessRestartBackoffMax. A count below one is treated as one. Every
// proxy_process respawn loop (process restart, construction retry, publication
// refresh) shares this schedule.
func spawnBackoffDelay(failures int) time.Duration {
	delay := proxyProcessRestartBackoff
	for i := 1; i < failures; i++ {
		if delay >= proxyProcessRestartBackoffMax/2 {
			return proxyProcessRestartBackoffMax
		}
		delay *= 2
	}
	if delay > proxyProcessRestartBackoffMax {
		return proxyProcessRestartBackoffMax
	}
	return delay
}

// logSpawnFailure logs one failed start, with how many have failed in a row
// and when the next attempt is due, so an operator can tell a service backing
// off from one that is wedged.
func logSpawnFailure(service, what string, err error, failures int, next time.Time) {
	log.Printf("workspacesvc: %s %s: %v (failure %d, next attempt at %s)",
		what, service, err, failures, next.UTC().Format(time.RFC3339))
}
