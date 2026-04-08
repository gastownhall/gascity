package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// bdTestCmd is a minimal bd CLI implementation for testscript use.
// It wraps the file-based bead store so txtar tests can exercise bead
// CRUD without requiring a Dolt server. Registered as "bd" in TestMain.
//
// Mutation commands (create, close) emit events to .gc/events.jsonl
// so tests that verify event recording continue to pass.
func bdTestCmd() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "bd: missing subcommand")
		os.Exit(1)
	}

	subcmd := args[0]
	rest := args[1:]

	// Resolve city root: honor GC_CITY (exact validation, no walk-up)
	// then fall back to bounded parent discovery — mirroring cityForStoreDir.
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd: %v\n", err)
		os.Exit(1)
	}
	cityPath := cityForStoreDir(cwd)

	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, ".gc", "beads.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd: %v\n", err)
		os.Exit(1)
	}

	var rec events.Recorder
	if fr, err := events.NewFileRecorder(
		filepath.Join(cityPath, ".gc", "events.jsonl"), os.Stderr); err == nil {
		rec = fr
	} else {
		rec = events.Discard
	}

	var code int
	switch subcmd {
	case "create":
		code = doBdCreate(store, rec, rest)
	case "close":
		code = doBdClose(store, rec, rest)
	case "list":
		code = doBdList(store, rest)
	case "show":
		code = doBdShow(store, rest)
	case "ready":
		code = doBdReady(store, rest)
	default:
		fmt.Fprintf(os.Stderr, "bd: unknown subcommand %q\n", subcmd)
		code = 1
	}
	os.Exit(code)
}

func doBdCreate(store beads.Store, rec events.Recorder, args []string) int {
	format, args := parseBeadFormat(args)
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd create: missing title")
		return 1
	}
	b, err := store.Create(beads.Bead{Title: strings.Join(args, " ")})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd create: %v\n", err)
		return 1
	}
	rec.Record(events.Event{
		Type:    events.BeadCreated,
		Actor:   eventActor(),
		Subject: b.ID,
		Message: b.Title,
	})
	if format == "json" {
		writeBeadJSON(b, os.Stdout)
		return 0
	}
	fmt.Fprintf(os.Stdout, "Created bead: %s  (status: %s)\n", b.ID, b.Status) //nolint:errcheck // best-effort stdout
	return 0
}

func doBdClose(store beads.Store, rec events.Recorder, args []string) int {
	format, args := parseBeadFormat(args)
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd close: missing bead ID")
		return 1
	}
	if err := store.Close(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "bd close: %v\n", err)
		return 1
	}
	rec.Record(events.Event{
		Type:    events.BeadClosed,
		Actor:   eventActor(),
		Subject: args[0],
	})
	if format == "json" {
		b, err := store.Get(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bd close: %v\n", err)
			return 1
		}
		writeBeadJSON(b, os.Stdout)
		return 0
	}
	fmt.Fprintf(os.Stdout, "Closed bead: %s\n", args[0]) //nolint:errcheck // best-effort stdout
	return 0
}

func doBdList(store beads.Store, args []string) int {
	filters, args := parseBeadFilters(args)
	format, _ := parseBeadFormat(args)
	var all []beads.Bead
	var err error
	switch {
	case filters.status != "":
		all, err = store.ListOpen(filters.status)
	case filters.all:
		open, err := store.ListOpen()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bd list: %v\n", err)
			return 1
		}
		closed, err := store.ListOpen("closed")
		if err != nil {
			fmt.Fprintf(os.Stderr, "bd list: %v\n", err)
			return 1
		}
		all = make([]beads.Bead, 0, len(open)+len(closed))
		all = append(all, open...)
		all = append(all, closed...)
		slices.SortFunc(all, func(a, b beads.Bead) int {
			switch {
			case a.CreatedAt.Before(b.CreatedAt):
				return -1
			case a.CreatedAt.After(b.CreatedAt):
				return 1
			default:
				return strings.Compare(a.ID, b.ID)
			}
		})
	default:
		all, err = store.ListOpen()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd list: %v\n", err)
		return 1
	}
	all = filterBeads(all, filters)
	switch format {
	case "json":
		writeBeadsJSON(all, os.Stdout)
	default:
		writeBeadTable(all, os.Stdout, true)
	}
	return 0
}

func doBdShow(store beads.Store, args []string) int {
	format, args := parseBeadFormat(args)
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bd show: missing bead ID")
		return 1
	}
	b, err := store.Get(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd show: %v\n", err)
		return 1
	}
	switch format {
	case "json":
		writeBeadJSON(b, os.Stdout)
	default:
		writeBeadDetail(b, os.Stdout)
	}
	return 0
}

func doBdReady(store beads.Store, args []string) int {
	filters, args := parseBeadFilters(args)
	format, _ := parseBeadFormat(args)
	ready, err := store.Ready()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bd ready: %v\n", err)
		return 1
	}
	ready = filterBeads(ready, filters)
	switch format {
	case "json":
		writeBeadsJSON(ready, os.Stdout)
	default:
		writeBeadTable(ready, os.Stdout, false)
	}
	return 0
}
