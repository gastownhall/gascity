//go:build windows

package main

import (
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// cityStoreQuiesceTimeout bounds how long quiesceCityStoreWork waits.
var cityStoreQuiesceTimeout = 30 * time.Second

// cityStoreDeferredProviderStopTimeout bounds a provider stop abandoned by a
// returned quiesce timeout.
var cityStoreDeferredProviderStopTimeout = 10 * time.Minute

var (
	controllerStateAfterStoreSwapForTest     func()
	cityStoreDeferredProviderStopDoneForTest func()
)

func afterControllerStateStoreSwapForTest() {
	if controllerStateAfterStoreSwapForTest != nil {
		controllerStateAfterStoreSwapForTest()
	}
}

func markCityStoreDeferredProviderStopDoneForTest() {
	if cityStoreDeferredProviderStopDoneForTest != nil {
		cityStoreDeferredProviderStopDoneForTest()
	}
}

func controllerStateStoreUpdateAllowed(cs *controllerState) bool {
	return cs == nil || cs.cacheCtx == nil || cs.cacheCtx.Err() == nil
}

// quiesceCityStoreWork stops the background reconcilers of mc's bead stores
// and waits, up to cityStoreQuiesceTimeout, for a reconcile pass already in
// flight to finish.
//
// Cancelling a city ends its reconcile loops only between passes; a pass in
// flight keeps reading through bd. On a scope whose Dolt server bd owns, any
// bd command starts that server again if it is down, so a read landing after
// the provider has stopped the server leaves one running for a city that is
// gone. On Windows every such read is slow enough that a pass is nearly always
// in flight when the city is cancelled.
func quiesceCityStoreWork(mc *managedCity, stderr io.Writer) error {
	_, err := waitForCityStoreQuiesce(mc, stderr)
	return err
}

func waitForCityStoreQuiesce(mc *managedCity, stderr io.Writer) (<-chan struct{}, error) {
	stores := managedCityCachingStores(mc)
	if len(stores) == 0 {
		done := make(chan struct{})
		close(done)
		return done, nil
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		var wg sync.WaitGroup
		for _, store := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				store.StopReconciler()
			}()
		}
		wg.Wait()
	}()
	select {
	case <-stopped:
		return stopped, nil
	case <-time.After(cityStoreQuiesceTimeout):
		err := fmt.Errorf("city %q: bead store reconcile still running after %s (%d store(s) not drained)", mc.name, cityStoreQuiesceTimeout, len(stores))
		fmt.Fprintf(stderr, "gc supervisor: %v; leaving the bead store provider running\n", err) //nolint:errcheck // best-effort stderr
		return stopped, err
	}
}

func stopBeadsProviderAfterQuiesceDrain(mc *managedCity, cityPath string, quiesced <-chan struct{}) {
	if quiesced == nil {
		return
	}
	cityName := ""
	generation := uint64(0)
	if mc != nil {
		cityName = mc.name
		generation = mc.generation
	}
	go func() {
		defer markCityStoreDeferredProviderStopDoneForTest()
		timer := time.NewTimer(cityStoreDeferredProviderStopTimeout)
		defer timer.Stop()
		select {
		case <-quiesced:
		case <-timer.C:
			log.Printf("gc supervisor: city %q: deferred bead store stop gave up after %s waiting for quiesce drain", cityName, cityStoreDeferredProviderStopTimeout)
			return
		}
		if !managedCityGenerationStillCurrent(cityPath, generation) {
			log.Printf("gc supervisor: city %q: skipped deferred bead store stop because a newer city instance is published", cityName)
			return
		}
		log.Printf("gc supervisor: city %q: running deferred bead store stop after quiesce drain", cityName)
		if err := shutdownBeadsProvider(cityPath); err != nil {
			log.Printf("gc supervisor: city %q: deferred bead store stop: %v", cityName, err)
		}
	}()
}

// managedCityPlatformState holds the generation markPublished assigns when the
// supervisor publishes a managed city.
type managedCityPlatformState struct {
	generation uint64
}

var managedCityGenerations = struct {
	sync.Mutex
	byPath map[string]uint64
}{
	byPath: map[string]uint64{},
}

// markPublished assigns the next generation for the city published at path.
func (s *managedCityPlatformState) markPublished(path string) {
	managedCityGenerations.Lock()
	defer managedCityGenerations.Unlock()
	generation := managedCityGenerations.byPath[path] + 1
	managedCityGenerations.byPath[path] = generation
	s.generation = generation
}

func managedCityGenerationStillCurrent(path string, generation uint64) bool {
	if generation == 0 {
		return true
	}
	managedCityGenerations.Lock()
	defer managedCityGenerations.Unlock()
	return managedCityGenerations.byPath[path] == generation
}

var retiringCityStores = struct {
	sync.Mutex
	byController map[*controllerState]map[reconcilerStopper]int
}{
	byController: map[*controllerState]map[reconcilerStopper]int{},
}

func trackRetiringBeadStoreHandle(cs *controllerState, store beads.Store) func() {
	if cs == nil {
		return func() {}
	}
	cached := cachingStoreBehind(store)
	if cached == nil {
		return func() {}
	}
	retiringCityStores.Lock()
	byStore := retiringCityStores.byController[cs]
	if byStore == nil {
		byStore = map[reconcilerStopper]int{}
		retiringCityStores.byController[cs] = byStore
	}
	byStore[cached]++
	retiringCityStores.Unlock()
	return func() {
		retiringCityStores.Lock()
		defer retiringCityStores.Unlock()
		byStore := retiringCityStores.byController[cs]
		if byStore == nil {
			return
		}
		count := byStore[cached]
		if count > 1 {
			byStore[cached] = count - 1
			return
		}
		delete(byStore, cached)
		if len(byStore) == 0 {
			delete(retiringCityStores.byController, cs)
		}
	}
}

func retiringCityCachingStores(cs *controllerState) []reconcilerStopper {
	if cs == nil {
		return nil
	}
	retiringCityStores.Lock()
	defer retiringCityStores.Unlock()
	byStore := retiringCityStores.byController[cs]
	if len(byStore) == 0 {
		return nil
	}
	out := make([]reconcilerStopper, 0, len(byStore))
	for store := range byStore {
		out = append(out, store)
	}
	return out
}

// managedCityCachingStores returns the caching stores behind mc's current and
// retiring city and rig bead stores.
func managedCityCachingStores(mc *managedCity) []reconcilerStopper {
	if mc == nil || mc.cr == nil || mc.cr.cs == nil {
		return nil
	}
	cs := mc.cr.cs
	cs.mu.RLock()
	candidates := make([]beads.Store, 0, len(cs.beadStores)+1)
	candidates = append(candidates, cs.cityBeadStore)
	for _, store := range cs.beadStores {
		candidates = append(candidates, store)
	}
	retiring := retiringCityCachingStores(cs)
	cs.mu.RUnlock()
	var out []reconcilerStopper
	seen := map[reconcilerStopper]bool{}
	for _, store := range candidates {
		if cached := cachingStoreBehind(store); cached != nil && !seen[cached] {
			seen[cached] = true
			out = append(out, cached)
		}
	}
	for _, cached := range retiring {
		if cached != nil && !seen[cached] {
			seen[cached] = true
			out = append(out, cached)
		}
	}
	return out
}

// reconcilerStopper is the caching-store capability quiesce needs: stopping
// the background reconciler and waiting for a pass in flight. Its dynamic
// value is the *beads.CachingStore itself, so two stoppers are equal exactly
// when they are the same caching store.
type reconcilerStopper interface {
	StopReconciler()
}

// cachingStoreBehind unwraps bead-policy wrappers down to a caching store and
// returns its reconcilerStopper, or nil when store is not one.
func cachingStoreBehind(store beads.Store) reconcilerStopper {
	for store != nil {
		if base, _, ok := unwrapBeadPolicyStore(store); ok {
			store = base
			continue
		}
		stopper, _ := beads.As[reconcilerStopper](store)
		return stopper
	}
	return nil
}
