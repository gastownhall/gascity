package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/supervisor"
)

const (
	// supervisorBootPriorityEnv names the cities that boot first, comma
	// separated, in the order given. Everything else follows in lexical order.
	//
	// It is deliberately unset by default. The supervisor has no opinion about
	// which of an operator's cities matters most, and hardcoding one here would
	// put a specific deployment's city name into SDK code.
	supervisorBootPriorityEnv = "GC_SUPERVISOR_BOOT_PRIORITY"

	// supervisorBootConcurrencyEnv bounds how many cities the supervisor brings
	// up at once. 1 restores the strictly serial boot.
	supervisorBootConcurrencyEnv = "GC_SUPERVISOR_BOOT_CONCURRENCY"
)

// defaultSupervisorBootConcurrency is the default number of cities started in
// parallel.
//
// Two, not "as many as there are cities": each city start opens its own bead
// store, materializes formulas and runs boot sweeps, so an unbounded fan-out
// turns a fleet boot into a host-wide IO spike. Two halves the wall clock of a
// six-city boot without any city seeing meaningfully different conditions from
// the ones it sees in steady state, where all six run at once anyway. Raise it
// on hosts with IO headroom.
const defaultSupervisorBootConcurrency = 2

// cityInitStatusQueued is the initStatus a city carries between being selected
// to start and its start actually beginning. See selectCitiesToStart.
const cityInitStatusQueued = "queued"

// supervisorBootPriority returns the operator's preferred boot order.
func supervisorBootPriority() []string {
	raw := strings.Split(os.Getenv(supervisorBootPriorityEnv), ",")
	names := make([]string, 0, len(raw))
	for _, name := range raw {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// supervisorBootConcurrency returns how many cities may start at once. An
// unparseable value falls back to the default rather than failing the boot; a
// value below 1 clamps to serial.
func supervisorBootConcurrency() int {
	raw := strings.TrimSpace(os.Getenv(supervisorBootConcurrencyEnv))
	if raw == "" {
		return defaultSupervisorBootConcurrency
	}
	bound, err := strconv.Atoi(raw)
	if err != nil {
		return defaultSupervisorBootConcurrency
	}
	if bound < 1 {
		return 1
	}
	return bound
}

// selectCitiesToStart picks the registered cities that are neither running nor
// already initializing, marks each one queued, and returns them in boot order.
//
// The marking is the point, and it has to happen in the SAME locked pass as the
// selection. Cities start outside the registry lock — that is what makes a
// bounded-parallel boot possible at all — but the supervisor's reconcile pass is
// re-entrant (ticker, SIGHUP, the reload socket command), and it identifies
// startable cities by the absence of an initStatus entry. Selecting under the
// lock and marking later leaves a window in which a second pass selects a city
// whose start is already in flight and starts it twice.
//
// Serially this was safe by accident: the loop body set "loading_config" before
// it did anything slow, and nothing else could run until the whole pass
// returned. Neither of those holds once workers exist.
func selectCitiesToStart(cr *cityRegistry, desired map[string]supervisor.CityEntry, priority []string) []supervisor.CityEntry {
	var toStart []supervisor.CityEntry
	cr.BatchUpdate(func(
		cities map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		for path, entry := range desired {
			if _, running := cities[path]; running {
				continue
			}
			if _, initializing := initStatus[path]; initializing {
				continue
			}
			initStatus[path] = cityInitProgress{name: entry.EffectiveName(), status: cityInitStatusQueued}
			toStart = append(toStart, entry)
		}
	})
	return sortCityStartOrder(toStart, priority)
}

// releaseQueuedCityStart clears a city's initStatus entry, so an attempt that
// ended without recording anything does not wedge the city out of every future
// reconcile pass.
//
// The delete is UNCONDITIONAL, and that is the whole of its value. Keying it on
// the queued status would only cover the early returns that happen before the
// config load, because startOneCity replaces the marker with "loading_config"
// and then with each phase name within milliseconds of starting. A panic
// anywhere in the body after that skips recordInitFailure and
// publishManagedCity alike, so a conditional release would find a phase name
// and do nothing — leaving the city permanently mid-init, never selected again
// (selectCitiesToStart skips any path holding an entry, before either backoff),
// with no retry and nothing short of a supervisor restart to recover it.
//
// Unconditional is safe because there is never a competing marker to destroy:
// reconcileCities runs on the supervisor's single loop goroutine and
// startCityWorkers does not return until its workers have, so no other
// selection pass can be running while this city's start is alive. On every
// ordinary exit the entry has already been deleted by the normal clears and
// this is a no-op.
func releaseQueuedCityStart(cr *cityRegistry, path string) {
	cr.BatchUpdate(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		delete(initStatus, path)
	})
}

// sortCityStartOrder returns entries in boot order: the priority names first, in
// the order they were named, then everything else lexically by name and path.
//
// The list this replaces came straight off a map range, so boot order was
// randomized per process — which city booted first, and which one waited behind
// every other, changed on every restart. A bounded pool makes that worse rather
// than better: the first wave would be a random draw.
func sortCityStartOrder(entries []supervisor.CityEntry, priority []string) []supervisor.CityEntry {
	rank := make(map[string]int, len(priority))
	for i, name := range priority {
		if _, seen := rank[name]; !seen {
			rank[name] = i
		}
	}
	// A name that is not in the list sorts after every name that is.
	rankOf := func(entry supervisor.CityEntry) int {
		if i, ok := rank[entry.EffectiveName()]; ok {
			return i
		}
		return len(priority)
	}

	ordered := make([]supervisor.CityEntry, len(entries))
	copy(ordered, entries)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := rankOf(ordered[i]), rankOf(ordered[j])
		if ri != rj {
			return ri < rj
		}
		if ordered[i].EffectiveName() != ordered[j].EffectiveName() {
			return ordered[i].EffectiveName() < ordered[j].EffectiveName()
		}
		return ordered[i].Path < ordered[j].Path
	})
	return ordered
}

// startCityWorkers runs start over entries with at most bound of them in flight,
// submitting them in the order given so the first wave is the head of the boot
// order. It returns the entries it never submitted because shutdown was
// requested, so the caller can release their queued markers.
//
// A panic in one city's start is recovered here rather than left to the caller.
// Serially it would have unwound into the supervisor's reconcile-level recover,
// costing the rest of that pass; from a worker goroutine there is no recover
// above it at all, so it would take the whole supervisor down with it.
//
// Shutdown stops SUBMISSION, not the cities already starting. A fleet boot is
// minutes long, and without this a SIGTERM arriving early in one still booted
// every remaining city before the supervisor's shutdown case ran — long enough
// for systemd to hit TimeoutStopSec and SIGKILL the supervisor mid-boot, which
// skips session preservation entirely. Canceling the in-flight wave instead
// would abandon half-built cities with their stores open, so stop latency is
// deliberately bounded to that wave rather than to zero.
//
// bound 1 runs inline on the calling goroutine — the same serial boot as before
// this pool existed, with no goroutine to schedule.
func startCityWorkers(ctx context.Context, entries []supervisor.CityEntry, bound int, stderr io.Writer, start func(supervisor.CityEntry)) []supervisor.CityEntry {
	if len(entries) == 0 {
		return nil
	}
	if bound < 1 {
		bound = 1
	}
	if bound > len(entries) {
		bound = len(entries)
	}

	startOne := func(entry supervisor.CityEntry) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "gc supervisor: city '%s': start panicked: %v\n", entry.EffectiveName(), r) //nolint:errcheck // best-effort stderr
			}
		}()
		start(entry)
	}

	if bound == 1 {
		for i, entry := range entries {
			if ctx.Err() != nil {
				return entries[i:]
			}
			startOne(entry)
		}
		return nil
	}

	work := make(chan supervisor.CityEntry)
	var wg sync.WaitGroup
	for range bound {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range work {
				startOne(entry)
			}
		}()
	}

	var skipped []supervisor.CityEntry
submit:
	for i, entry := range entries {
		// Checked on the send as well as before it: with every worker busy the
		// submit loop is parked in the handover, which is exactly where a
		// shutdown arriving mid-boot finds it.
		select {
		case <-ctx.Done():
			skipped = entries[i:]
			break submit
		default:
		}
		select {
		case work <- entry:
		case <-ctx.Done():
			skipped = entries[i:]
			break submit
		}
	}
	close(work)
	wg.Wait()
	return skipped
}
