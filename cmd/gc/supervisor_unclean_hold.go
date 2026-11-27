package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// shouldHoldCitiesAfterUncleanExit reports whether the supervisor holds its
// registered cities before their first reconcile: the operator opted in with
// hold_cities_after_unclean_exit and the previous instance crashed or its
// exit could not be classified.
func shouldHoldCitiesAfterUncleanExit(sec supervisor.Section, previousExit string) bool {
	if !sec.HoldCitiesAfterUncleanExit {
		return false
	}
	return previousExit == supervisor.PreviousExitCrash || previousExit == supervisor.PreviousExitUnknown
}

// holdRegisteredCitiesAfterUncleanExit records each registered city whose
// directory exists as suspended, over any earlier explicit resume, records a
// city.suspended event naming the cause in the city's event log, and prints
// one line per held city naming the command that releases it. A city whose
// suspension file cannot be written gets the error printed and is not held.
func holdRegisteredCitiesAfterUncleanExit(cities []supervisor.CityEntry, previousExit string, stderr io.Writer) {
	yes := true
	for _, c := range cities {
		if _, err := os.Stat(c.Path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(stderr, "gc supervisor: city %q not held after previous_exit=%s: %v\n", c.Name, previousExit, err) //nolint:errcheck
			}
			continue
		}
		if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, c.Path, &yes); err != nil {
			fmt.Fprintf(stderr, "gc supervisor: city %q at %s not held after previous_exit=%s: %v\n", c.Name, c.Path, previousExit, err) //nolint:errcheck
			continue
		}
		if rec, err := openCityEventsLog(c.Path, stderr); err != nil {
			fmt.Fprintf(stderr, "gc supervisor: city %q: recording city.suspended: %v\n", c.Name, err) //nolint:errcheck
		} else {
			rec.Record(events.Event{
				Type:    events.CitySuspended,
				Actor:   "supervisor",
				Subject: c.Name,
				Message: "held: previous_exit=" + previousExit,
			})
			rec.Close() //nolint:errcheck
		}
		fmt.Fprintf(stderr, "gc supervisor: holding city %q suspended after previous_exit=%s; run `gc resume %s` to start its sessions\n", c.Name, previousExit, c.Path) //nolint:errcheck
	}
}
