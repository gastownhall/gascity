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
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List beads (API-routed with bd fallback)",
		Long: `List beads across all rigs, routed through the supervisor API when
the controller is alive and falling back to a direct multi-store read
otherwise.

Supports --label, --status, --all, and --format flags. --json is an
alias for --format=json. API-path JSON output includes _cache_age_s;
fallback-path JSON omits it.`,
		Example: `  gc beads list
  gc beads list --label ready-to-build
  gc beads list --status open --json
  gc beads list --format=toon`,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdBeadsList(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func newBeadsShowCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <bead-id>",
		Short: "Show a single bead (API-routed with bd fallback)",
		Long: `Show one bead by ID, routed through the supervisor API when the
controller is alive and falling back to a direct multi-store lookup
otherwise.

Supports --format and --json. API-path JSON output includes
_cache_age_s; fallback-path JSON omits it.`,
		Example: `  gc beads show ga-abc
  gc beads show ga-abc --json`,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdBeadsShow(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

// cmdBeadsList is the CLI entry point for "gc beads list". Routes through
// the supervisor API when a controller is up and falls back to direct bd
// multi-store reads otherwise.
func cmdBeadsList(args []string, stdout, stderr io.Writer) int {
	remoteC, isRemote, cityPath, err := resolveReadTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	format, rest := parseBeadFormat(args)
	filters, _ := parseBeadFilters(rest)
	if isRemote {
		return routeBeadsList("", remoteC, "", format, filters, stdout, stderr)
	}
	c, reason := beadsListAPIClient(cityPath)
	return routeBeadsList(cityPath, c, reason, format, filters, stdout, stderr)
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
	const cmdName = "beads list"
	if c != nil {
		cr, err := c.ListBeads(api.ListBeadsOpts{
			Label:  filters.label,
			Status: filters.status,
			All:    filters.all,
		})
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderBeadsListFromAPI(cr, format, filters, stdout)
		}
		if !api.ShouldFallbackForRead(c, err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc beads list: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doBeadsListFallback(cityPath, format, filters, stdout, stderr)
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

// cmdBeadsShow is the CLI entry point for "gc beads show". Routes through
// the supervisor API and falls back to a direct store lookup.
func cmdBeadsShow(args []string, stdout, stderr io.Writer) int {
	remoteC, isRemote, cityPath, err := resolveReadTarget()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads show: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	format, rest := parseBeadFormat(args)
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "gc beads show: missing bead id") //nolint:errcheck // best-effort stderr
		return 1
	}
	beadID := rest[0]
	if isRemote {
		return routeBeadsShow("", remoteC, "", beadID, format, stdout, stderr)
	}
	c, reason := beadsShowAPIClient(cityPath)
	return routeBeadsShow(cityPath, c, reason, beadID, format, stdout, stderr)
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
	const cmdName = "beads show"
	if c != nil {
		cr, err := c.GetBead(beadID)
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderBeadsShowFromAPI(cr, format, stdout)
		}
		if !api.ShouldFallbackForRead(c, err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc beads show: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doBeadsShowFallback(cityPath, beadID, format, stdout, stderr)
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
