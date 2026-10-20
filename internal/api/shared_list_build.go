package api

import (
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"golang.org/x/sync/singleflight"
)

// listStoreReadBudget is the longest GET /agents and GET /sessions wait for a
// full list before they answer from what is cheap. A full list reads the bead
// store. On a busy city whose store is Dolt, whenever the agent pool was full
// a full list took 15 to 30 seconds, so a dashboard gave up and showed a
// timeout instead of agents. It is a var so tests can shorten it.
var listStoreReadBudget = time.Second

// listLastGoodMaxAge is the oldest saved list boundedListBuild will serve.
// Past it, the store-free list is more useful: its names and running state are
// live, while an old list can show agents that stopped long ago.
const listLastGoodMaxAge = 5 * time.Minute

// listLastGoodMaxKeys caps how many saved lists are kept. Each set of query
// parameters is its own key, so without a cap a caller could grow the map
// without end.
const listLastGoodMaxKeys = 32

// listBuilds holds the list builds that are running now, and the last answer
// each kind of list gave. The zero value is ready to use.
type listBuilds struct {
	flight singleflight.Group

	mu       sync.Mutex
	lastGood map[string]savedList
}

// savedList is one finished full list and when it finished.
type savedList struct {
	out any // a *ListOutput[T], for the T of the endpoint the key names
	at  time.Time
}

// boundedListBuild answers a costly list request without making the caller
// wait on the bead store for longer than listStoreReadBudget. Every list
// endpoint whose build reads the bead store should answer through it.
//
// build makes the full list. It may read the bead store. It runs in the
// background and does NOT stop when the caller stops waiting: it keeps going
// until it lands, and then its answer is saved for the next caller. Overlapping
// callers with the same key share one build, so a busy page never piles up
// copies of it.
//
// storeFree makes the list from config and the runtime alone. It must never
// read the bead store, because it is the answer for exactly the moment the
// store is too slow.
//
// The caller gets the first of these that is ready:
//  1. the full list, when it lands inside the budget;
//  2. the last full list, marked partial with its age, when one is saved and
//     is younger than listLastGoodMaxAge;
//  3. storeFree's list, marked partial as still loading.
//
// A build that fails inside the budget returns its error, so a bad cursor
// still gets its 400. A build that fails later is only logged: its caller has
// already been answered.
func boundedListBuild[T any](b *listBuilds, key string, build func() (*ListOutput[T], error), storeFree func() *ListOutput[T]) (*ListOutput[T], error) {
	ch := b.flight.DoChan(key, func() (v any, err error) {
		// singleflight re-panics a DoChan panic where nothing can recover it,
		// which would stop the whole supervisor. Before this helper a panic
		// here reached the HTTP server, which turned it into a 500. Keep it
		// that way.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("api: building list %q panicked: %v\n%s", key, r, debug.Stack())
				v, err = nil, apierr.Internal.Msg(fmt.Sprintf("building %s failed", key))
			}
		}()
		out, err := build()
		if err != nil {
			return nil, err
		}
		b.save(key, out)
		return out, nil
	})

	timer := time.NewTimer(listStoreReadBudget)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		out := res.Val.(*ListOutput[T])
		if !res.Shared {
			return out, nil
		}
		// Each sharer gets its own deep copy, the way response-cache hits
		// do, so one caller can never change what another serves.
		return cloneListOutput(out)
	case <-timer.C:
	}

	if saved, age, ok := b.recall(key); ok {
		out, err := cloneListOutput(saved.(*ListOutput[T]))
		if err != nil {
			return nil, err
		}
		out.CacheAgeS += age.Seconds()
		markDetailsPending(&out.Body, fmt.Sprintf(
			"the bead store did not answer within %s, so this list is from %s ago; a fresh read is running",
			listStoreReadBudget, age.Round(time.Second)))
		return out, nil
	}
	out := storeFree()
	markDetailsPending(&out.Body, fmt.Sprintf(
		"the bead store did not answer within %s, so only live names and running state are shown; details are still loading",
		listStoreReadBudget))
	return out, nil
}

// save keeps out as the last full list for key.
func (b *listBuilds) save(key string, out any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastGood == nil {
		b.lastGood = make(map[string]savedList)
	}
	if _, exists := b.lastGood[key]; !exists && len(b.lastGood) >= listLastGoodMaxKeys {
		oldestKey, oldestAt := "", time.Time{}
		for k, v := range b.lastGood {
			if oldestKey == "" || v.at.Before(oldestAt) {
				oldestKey, oldestAt = k, v.at
			}
		}
		delete(b.lastGood, oldestKey)
	}
	b.lastGood[key] = savedList{out: out, at: time.Now()}
}

// recall returns the last full list for key and its age, unless there is none
// or it is older than listLastGoodMaxAge.
func (b *listBuilds) recall(key string) (any, time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	saved, ok := b.lastGood[key]
	if !ok {
		return nil, 0, false
	}
	age := time.Since(saved.at)
	if age > listLastGoodMaxAge {
		return nil, 0, false
	}
	return saved.out, age, true
}

// cloneListOutput deep-copies a list so the copy can be changed or served
// without touching the original.
func cloneListOutput[T any](out *ListOutput[T]) (*ListOutput[T], error) {
	body, ok := cloneCachedValue[ListBody[T]](out.Body)
	if !ok {
		return nil, apierr.Internal.Msg("copying a shared list failed")
	}
	return &ListOutput[T]{Index: out.Index, CacheAgeS: out.CacheAgeS, Body: body}, nil
}

// markDetailsPending marks a list partial and says why first, ahead of any
// partial error the list already carried.
func markDetailsPending[T any](body *ListBody[T], why string) {
	body.Partial = true
	body.PartialErrors = append([]string{why}, body.PartialErrors...)
}
