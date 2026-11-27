package main

// requestBeadCacheRescans asks every distinct bead cache this state feeds
// (sweepCaches: the city cache, the rig caches and the relocated class
// caches) for a full re-scan, and returns how many it asked. It does no I/O
// and waits on no scan: each cache runs the re-scan on its own reconcile
// loop, once its reconcile gate (a quiescent city, a suspended rig) and its
// failure backoff allow (beads.CachingStore.RequestReconcile).
func (cs *controllerState) requestBeadCacheRescans() int {
	caches := cs.sweepCaches()
	for _, cache := range caches {
		cache.RequestReconcile()
	}
	return len(caches)
}
