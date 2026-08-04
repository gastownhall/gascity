package main

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

func newBeadsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "beads",
		Short: "Manage the beads provider",
		Long: `Manage the beads provider (backing store for issue tracking).

Subcommands for topology operations, health checking, diagnostics, and
read-only list/show routed through the supervisor API with transparent
fallback to direct bd reads.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc beads: missing subcommand (city, health, list, show)") //nolint:errcheck // best-effort stderr
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

Supports --label, --status, --all, and --format. --format=json emits
JSON (API-path JSON includes _cache_age_s; fallback-path JSON omits
it). The bare --json flag is reserved by the CLI's JSON-contract layer
and is not wired for this command; use --format=json.`,
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
	cmd.Flags().BoolVar(&all, "all", false, "include closed beads (default: open only)")
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

Supports --format. --format=json emits JSON (API-path JSON includes
_cache_age_s; fallback-path JSON omits it). The bare --json flag is
reserved by the CLI's JSON-contract layer and is not wired for this
command; use --format=json.`,
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
func routeBeadsList(cityPath string, c *api.Client, nilReason, format string, filters beadFilters, stdout, stderr io.Writer) int {
	var cr api.CachedRead[[]beads.Bead]
	return routeRead(c, "beads list", nilReason, stderr,
		func() error {
			var err error
			cr, err = c.ListBeads(api.ListBeadsOpts{
				Label:  filters.label,
				Status: filters.status,
				All:    filters.all,
			})
			return err
		},
		func() int { return renderBeadsListFromAPI(cr, format, filters, stdout) },
		func() int { return doBeadsListFallback(cityPath, format, filters, stdout, stderr) },
	)
}

// renderBeadsListFromAPI formats the API-sourced bead list using the same
// bead_format.go helpers as the fallback path. JSON output adds the
// _cache_age_s envelope field; human output appends a staleness banner
// when cache age > 30s.
func renderBeadsListFromAPI(cr api.CachedRead[[]beads.Bead], format string, filters beadFilters, stdout io.Writer) int {
	filtered := filterBeads(cr.Body, filters)
	sortBeadsForList(filtered)
	if format == "json" {
		writeBeadsJSONWithCache(filtered, cr.AgeSeconds, stdout)
	} else {
		writeBeadTable(filtered, stdout, true)
		if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
			fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
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
func doBeadsListFallback(cityPath, format string, filters beadFilters, stdout, stderr io.Writer) int {
	stores, code := openAllConvoyStoresAt(cityPath, stderr, "gc beads list")
	if stores == nil {
		return code
	}
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
		func() int { return renderBeadsShowFromAPI(cr, format, stdout) },
		func() int { return doBeadsShowFallback(cityPath, beadID, format, stdout, stderr) },
	)
}

func renderBeadsShowFromAPI(cr api.CachedRead[beads.Bead], format string, stdout io.Writer) int {
	if format == "json" {
		writeBeadJSONWithCache(cr.Body, cr.AgeSeconds, stdout)
	} else {
		writeBeadDetail(cr.Body, stdout)
		if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
			fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
		}
	}
	return 0
}

func doBeadsShowFallback(cityPath, beadID, format string, stdout, stderr io.Writer) int {
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
func collectBeadsAcrossStores(stores []convoyStoreView, filters beadFilters) ([]beads.Bead, error) {
	q := beads.ListQuery{
		Label:         filters.label,
		Status:        filters.status,
		IncludeClosed: filters.all,
		AllowScan:     true,
	}
	all := make([]beads.Bead, 0)
	for _, candidate := range stores {
		list, err := candidate.store.List(q)
		if err != nil {
			return nil, err
		}
		all = append(all, list...)
	}
	return all, nil
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
