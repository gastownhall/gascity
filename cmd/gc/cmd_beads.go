package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

func newBeadsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "beads",
		Short: "Manage the beads provider",
		Long: `Manage the beads provider (backing store for issue tracking).

Subcommands for topology operations, health checking, diagnostics, exact-store
metadata compare-and-set, and read-only list/show routed through the supervisor
API with transparent fallback to direct bd reads.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc beads: missing subcommand (city, health, list, metadata-cas, show)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc beads: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newBeadsCityCmd(stdout, stderr),
		newBeadsHealthCmd(stdout, stderr),
		newBeadsListCmd(stdout, stderr),
		newBeadsMetadataCASCmd(stdout, stderr),
		newBeadsShowCmd(stdout, stderr),
	)
	return cmd
}

func newBeadsListCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		label, status, format string
		all                   bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List beads (API-routed with bd fallback)",
		Long: `List beads across all rigs, routed through the supervisor API when
the controller is alive and falling back to a direct multi-store read
otherwise.

Supports --label, --status, --all, and --format. --format=json prints a
JSON array of beads, the same on every route for a local city. For a
remote city (--context, --city-url/--city-name, GC_CITY_URL,
GC_CITY_CONTEXT, or the context selected by gc context use) it prints
{"beads": [...], "_cache_age_s": N}, where _cache_age_s is the age in
seconds of the remote supervisor's bead cache.

For a local city the array carries no cache age: when the supervisor
API answers, the beads come from the supervisor's bead cache, which can
lag the stores, so a bare array is not evidence of an uncached read. Set
GC_NO_API=1 to read the stores directly, or name the city with
--city-url and --city-name to get the envelope with its _cache_age_s.

A city or rig bead store that cannot be opened or read fails the command
(exit 1, naming the store) rather than being left out of the list. For
a local city whose relocated class binding cannot be read, the command
warns on stderr, lists the remaining stores and exits 0.

The bare --json flag is reserved by the CLI's JSON-contract layer and is
not wired for this command; use --format=json.`,
		Example: `  gc beads list
  gc beads list --label ready-to-build
  gc beads list --status open --format=json`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBeadsList(format, beadFilters{label: label, status: status, all: all}, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "filter to beads carrying this label")
	cmd.Flags().StringVar(&status, "status", "", "filter to beads in this status")
	cmd.Flags().BoolVar(&all, "all", false, "include closed beads (default: all nonclosed statuses)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	return cmd
}

func newBeadsShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "show <bead-id>",
		Short: "Show a single bead (API-routed with bd fallback)",
		Long: `Show one bead by ID, routed through the supervisor API when the
controller is alive and falling back to a direct multi-store lookup
otherwise.

Supports --format. --format=json prints the bead as a JSON object, the
same on every route for a local city. For a remote city (--context,
--city-url/--city-name, GC_CITY_URL, GC_CITY_CONTEXT, or the context
selected by gc context use) it prints {"bead": {...}, "_cache_age_s": N},
where _cache_age_s is the age in seconds of the remote supervisor's bead
cache.

For a local city the object carries no cache age: when the supervisor
API answers, the bead comes from the supervisor's bead cache, which can
lag the stores, so a bare object is not evidence of an uncached read.
Set GC_NO_API=1 to read the stores directly, or name the city with
--city-url and --city-name to get the envelope with its _cache_age_s.

The bare --json flag is reserved by the CLI's JSON-contract layer and is
not wired for this command; use --format=json.`,
		Example: `  gc beads show ga-abc
  gc beads show ga-abc --format=json`,
		// MaximumNArgs(1), not ExactArgs(1): a missing id must reach the internal
		// guard AFTER resolveReadTarget (so a resolve error still takes
		// precedence), which the routeReadCmdWithHooks ordering in cmdBeadsShow
		// depends on. ExactArgs would reject the zero-arg case in cobra, before
		// the resolver, inverting that documented ordering.
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			if cmdBeadsShow(id, format, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	return cmd
}

// cmdBeadsList is the CLI entry point for "gc beads list". The output format
// and filters are parsed by cobra (see newBeadsListCmd) and passed in. Routes
// through the supervisor API when a controller is up and falls back to a direct
// multi-store read otherwise.
func cmdBeadsList(format string, filters beadFilters, stdout, stderr io.Writer) int {
	return routeReadCmd("beads list", stderr, beadsListAPIClient, func(cityPath string, c *api.Client, nilReason string) int {
		return routeBeadsList(cityPath, c, nilReason, format, filters, stdout, stderr)
	})
}

// beadsListAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests inject a client pointed at httptest.Server or force a
// specific fallback reason without spinning up a real controller.
var beadsListAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeBeadsList dispatches `beads list` to the supervisor API when a
// controller is up; otherwise falls back to the local multi-store iterator.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
//
// A partial API answer (the server could not read one of the city's stores) is
// an error, not a shorter list: the direct read fails the same way on a store it
// cannot open or read, so neither route prints part of the city as the whole.
func routeBeadsList(cityPath string, c *api.Client, nilReason, format string, filters beadFilters, stdout, stderr io.Writer) int {
	var cr api.CachedRead[api.BeadListView]
	return routeRead(c, "beads list", nilReason, stderr,
		func() error {
			var err error
			cr, err = c.ListBeads(api.ListBeadsOpts{
				Label:  filters.label,
				Status: filters.status,
				All:    filters.all,
			})
			if err != nil {
				return err
			}
			if cr.Body.Partial {
				return errorAfterFetch{Detail: beadsListPartialErrorDetail(cr.Body.PartialErrors, c.IsRemote())}
			}
			return nil
		},
		func() int {
			return renderBeadsListFromAPI(cr.Body.Beads, cr.AgeSeconds, format, filters, c.IsRemote(), stdout)
		},
		func() int { return doBeadsListFallback(cityPath, format, filters, stdout, stderr) },
	)
}

// beadsListPartialErrorDetail is the error gc beads list prints when the API
// could build the list only from some of the city's stores. For a local target
// it adds the remedy: the store itself may be broken (gc doctor finds it), or
// only the supervisor's view of it may be stale (a store that failed to open
// when the supervisor started stays failed there), and then GC_NO_API=1 reads
// the stores directly. A remote target has neither remedy on this host.
func beadsListPartialErrorDetail(errs []string, remote bool) string {
	detail := "incomplete bead list from the API, not printed: the server reported a failed bead store without naming it"
	if len(errs) > 0 {
		detail = "incomplete bead list from the API, not printed: " + strings.Join(errs, "; ")
	}
	if remote {
		return detail
	}
	return detail + "\n" + `hint: run "gc doctor" to find the broken store; if it is healthy and only the supervisor's view of it is stale, GC_NO_API=1 gc beads list reads the stores directly`
}

// renderBeadsListFromAPI formats the API-sourced bead list using the same
// bead_format.go helpers as the fallback path. JSON output is the bare array
// the fallback prints, except for a remote target (remote), which gets the
// {beads, _cache_age_s} envelope; human output appends a staleness banner
// when cache age > 30s.
func renderBeadsListFromAPI(bs []beads.Bead, cacheAgeS float64, format string, filters beadFilters, remote bool, stdout io.Writer) int {
	filtered := filterBeads(bs, filters)
	sortBeadsForList(filtered)
	if format == "json" {
		if remote {
			writeBeadsJSONWithCache(filtered, cacheAgeS, stdout)
		} else {
			writeBeadsJSON(filtered, stdout)
		}
	} else {
		writeBeadTable(filtered, stdout, true)
		if cacheAgeS > cacheAgeBannerThresholdSeconds {
			fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cacheAgeS) //nolint:errcheck // best-effort stdout
		}
	}
	return 0
}

// doBeadsListFallback is the direct-bd path for "gc beads list". Opens every
// rig store plus the city store, collects beads, applies the filters, and
// renders using the shared bead_format.go helpers.
//
// This lane is UNBOUNDED: the store list uses Limit 0 (unlimited), so every
// matching bead is returned. api.Client.ListBeads follows next_cursor to match
// this coverage on the API/remote lanes (previously it truncated to page 1).
//
// A city or rig store that cannot be opened or read fails the listing, naming
// the store: the supervisor reports the same store as a partial answer, which
// the API route refuses, so both routes fail rather than print part of the
// city. An unreadable relocated class binding only warns
// (convoyStoreViewsForRead), so reads keep working while a city's storage
// configuration is mid-repair.
func doBeadsListFallback(cityPath, format string, filters beadFilters, stdout, stderr io.Writer) int {
	stores, code := openBeadsListStoresAt(cityPath, stderr)
	if stores == nil {
		return code
	}
	stores = convoyStoreViewsForRead(cityPath, stores, stderr, "gc beads list")
	all, err := collectBeadsAcrossStores(stores, filters)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sortBeadsForList(all)
	if format == "json" {
		writeBeadsJSON(all, stdout)
	} else {
		writeBeadTable(all, stdout, true)
	}
	return 0
}

// openBeadsListStoresAt opens every bead store gc beads list reads for a
// pre-resolved cityPath. Unlike openAllConvoyStoresAt, which skips a store it
// cannot open, any open failure here fails the listing: a list that silently
// left out a rig's beads would read as the city's complete answer, while the
// supervisor reports that same store as a partial answer the API route refuses.
// gc beads show keeps openAllConvoyStoresAt, so one broken rig does not stop
// showing a bead held by a healthy store.
func openBeadsListStoresAt(cityPath string, stderr io.Writer) ([]convoyStoreView, int) {
	const cmdName = "gc beads list"
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	emitLoadCityConfigWarnings(stderr, prov)
	candidates := convoyStoreCandidates(cfg, cityPath, "") // residency:allow — openAllConvoyStoresAt's own enumeration, re-walked so a store that cannot open fails gc beads list instead of vanishing from it (the supervisor reports the same store as a partial answer)
	failed := 0
	stores := openConvoyStoreDirs(candidates, openStoreForCity(cityPath), func(dir string, err error) {
		fmt.Fprintf(stderr, "%s: opening the bead store at %s: %v\n", cmdName, dir, err) //nolint:errcheck // best-effort stderr
		failed++
	})
	if failed > 0 || len(stores) == 0 {
		if len(stores) == 0 && failed == 0 {
			fmt.Fprintf(stderr, "%s: no bead stores available\n", cmdName) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return stores, 0
}

// cmdBeadsShow is the CLI entry point for "gc beads show". The bead id and
// output format are parsed by cobra (see newBeadsShowCmd) and passed in. Routes
// through the supervisor API and falls back to a direct store lookup.
//
// It uses routeReadCmdWithHooks with a guard hook because the missing-id guard
// must fire AFTER resolveReadTarget (so a resolve error still takes precedence)
// but BEFORE the local beadsShowAPIClient seam (whose apiClient() call has
// observable side effects — the classifyGCNoAPI stderr warning on a malformed
// GC_NO_API, plus a controller-liveness probe and config.Load). The hook runs in
// exactly that slot.
func cmdBeadsShow(id, format string, stdout, stderr io.Writer) int {
	return routeReadCmdWithHooks("beads show", stderr, readCmdHooks{
		guard: func() (int, bool) {
			if id == "" {
				fmt.Fprintln(stderr, "gc beads show: missing bead id") //nolint:errcheck // best-effort stderr
				return 1, true
			}
			return 0, false
		},
	}, beadsShowAPIClient, func(cityPath string, c *api.Client, nilReason string) int {
		return routeBeadsShow(cityPath, c, nilReason, id, format, stdout, stderr)
	})
}

var beadsShowAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeBeadsShow dispatches `beads show <id>` to the supervisor API and
// falls back otherwise. Exactly one route=... line per exit path.
func routeBeadsShow(cityPath string, c *api.Client, nilReason, beadID, format string, stdout, stderr io.Writer) int {
	var cr api.CachedRead[beads.Bead]
	return routeRead(c, "beads show", nilReason, stderr,
		func() error {
			var err error
			cr, err = c.GetBead(beadID)
			return err
		},
		func() int { return renderBeadsShowFromAPI(cr, format, c.IsRemote(), stdout) },
		func() int { return doBeadsShowFallback(cityPath, beadID, format, stdout, stderr) },
	)
}

// renderBeadsShowFromAPI formats the API-sourced bead. JSON output is the bare
// object the fallback prints, except for a remote target (remote), which gets
// the {bead, _cache_age_s} envelope; human output appends a staleness banner
// when cache age > 30s.
func renderBeadsShowFromAPI(cr api.CachedRead[beads.Bead], format string, remote bool, stdout io.Writer) int {
	if format == "json" {
		if remote {
			writeBeadJSONWithCache(cr.Body, cr.AgeSeconds, stdout)
		} else {
			writeBeadJSON(cr.Body, stdout)
		}
	} else {
		writeBeadDetail(cr.Body, stdout)
		if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
			fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
		}
	}
	return 0
}

// doBeadsShowFallback serves `gc beads show` from the stores directly when the
// API is unreachable.
//
// The relocated class binding is asked first, through the shared residency
// contract, because it is not one of the directories the scan below walks: on a
// converged city an infrastructure bead would otherwise be shown from the copy
// `gc storage migrate` retained in the city store, frozen at migration time and
// reported as though it were current.
func doBeadsShowFallback(cityPath, beadID, format string, stdout, stderr io.Writer) int {
	owner, ownedByBinding, err := cliByIDBindingOwner(cityPath, beadID)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads show: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if ownedByBinding {
		b, err := beadForOwner(owner, beadID)
		if err != nil {
			fmt.Fprintf(stderr, "gc beads show: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if format == "json" {
			writeBeadJSON(b, stdout)
		} else {
			writeBeadDetail(b, stdout)
		}
		return 0
	}

	stores, code := openAllConvoyStoresAt(cityPath, stderr, "gc beads show")
	if stores == nil {
		return code
	}
	for _, candidate := range stores {
		b, err := candidate.store.Get(beadID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			fmt.Fprintf(stderr, "gc beads show: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if format == "json" {
			writeBeadJSON(b, stdout)
		} else {
			writeBeadDetail(b, stdout)
		}
		return 0
	}
	fmt.Fprintf(stderr, "gc beads show: bead %s not found\n", beadID) //nolint:errcheck // best-effort stderr
	return 1
}

// collectBeadsAcrossStores iterates every opened bead store, applies the
// CLI-side filters, and returns a merged slice. The caller is responsible
// for sorting. --all maps to IncludeClosed (matching `bd list --all`); the
// CLI always opts into AllowScan because an unfiltered list is a valid
// default UX.
//
// The merge is mergeConvoyViewRows, so a relocated class binding's rows
// supersede the frozen copies the migration retained in the city store — asked
// of the binding by id rather than read off these filtered results, because a
// filter the caller chose must not decide which store owns a bead. Rows from two
// scanned stores that happen to share an id are two different beads and both
// survive.
func collectBeadsAcrossStores(stores []convoyStoreView, filters beadFilters) ([]beads.Bead, error) {
	q := beads.ListQuery{
		Label:         filters.label,
		Status:        filters.status,
		IncludeClosed: filters.all,
		AllowScan:     true,
	}
	rows := make([][]beads.Bead, len(stores))
	for i, candidate := range stores {
		list, err := candidate.store.List(q)
		if err != nil {
			return nil, fmt.Errorf("listing beads in %s: %w", candidate.path, err)
		}
		rows[i] = list
	}
	return mergeConvoyViewRows(stores, rows, func(b beads.Bead) string { return b.ID })
}

// sortBeadsForList orders beads by ID so output is stable across store
// merge ordering. Stable sort on the single key.
func sortBeadsForList(bs []beads.Bead) {
	sort.SliceStable(bs, func(i, j int) bool {
		return bs[i].ID < bs[j].ID
	})
}

func newBeadsHealthCmd(stdout, stderr io.Writer) *cobra.Command {
	var quiet, jsonOut bool
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check beads provider health",
		Long: `Check beads provider health and attempt recovery on failure.

Delegates to the provider's lifecycle health operation. For exec
providers (including bd/dolt), the script handles multi-tier checking
and recovery internally. For the file provider, always succeeds (no-op).

Also used by the beads-health system order for periodic monitoring.`,
		Example: `  gc beads health
  gc beads health --quiet
  gc beads health --json`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doBeadsHealth(quiet, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&quiet, "quiet", false,
		"silent on success, stderr on failure")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

type beadsHealthJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	CityPath      string `json:"city_path"`
	Provider      string `json:"provider"`
	Status        string `json:"status"`
}

// doBeadsHealth runs the beads provider health check.
// Returns 0 if healthy, 1 if unhealthy/recovery-failed.
func doBeadsHealth(quiet, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads health: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := healthBeadsProvider(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc beads health: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if jsonOut {
		if err := writeCLIJSONLine(stdout, beadsHealthJSONResult{
			SchemaVersion: "1",
			OK:            true,
			CityPath:      cityPath,
			Provider:      rawBeadsProvider(cityPath),
			Status:        "healthy",
		}); err != nil {
			fmt.Fprintf(stderr, "gc beads health: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	if !quiet {
		fmt.Fprintln(stdout, "Beads provider: healthy") //nolint:errcheck // best-effort stdout
	}
	return 0
}
