package main

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// newDashboardCmd creates the "gc dashboard" command group.
//
// The dashboard is no longer a standalone cross-origin static server. The
// compiled SPA is embedded into the gc binary and served same-origin by the
// supervisor, so these commands only point the user at the supervisor URL.
func newDashboardCmd(stdout, stderr io.Writer) *cobra.Command {
	var apiURL string
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Print where the web dashboard is served",
		Long: `Report the URL where the GC dashboard is served.

The dashboard SPA is embedded in the gc binary and served same-origin by the
supervisor; it is no longer a separate static server. This command resolves and
prints the supervisor URL (or tells you how to start the supervisor).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDashboardNotice(apiURL, stdout, stderr)
		},
	}
	bindDashboardFlags(cmd, &apiURL)
	cmd.AddCommand(newDashboardServeCmd(stdout, stderr))
	return cmd
}

// newDashboardServeCmd creates the "gc dashboard serve" subcommand.
//
// Retained for backwards compatibility: the dashboard is served by the
// supervisor, so this prints the same notice as "gc dashboard" rather than
// starting a server.
func newDashboardServeCmd(stdout, stderr io.Writer) *cobra.Command {
	var apiURL string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Print where the web dashboard is served",
		Long: `Report the URL where the GC dashboard is served.

The dashboard SPA is embedded in the gc binary and served same-origin by the
supervisor; "gc dashboard serve" no longer starts a static server. It resolves
and prints the supervisor URL (or tells you how to start the supervisor).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDashboardNotice(apiURL, stdout, stderr)
		},
	}
	bindDashboardFlags(cmd, &apiURL)
	return cmd
}

func bindDashboardFlags(cmd *cobra.Command, apiURL *string) {
	cmd.Flags().StringVar(apiURL, "api", "", "GC API server URL override (auto-discovered by default)")
}

// runDashboardNotice prints where the supervisor serves the dashboard SPA. It
// is purely informational and always exits 0: city/config resolution only
// feeds the standalone-controller fallback, so a failure there is non-fatal —
// the command falls back to live supervisor discovery and still prints a
// useful answer (the supervisor URL, or how to start it).
func runDashboardNotice(apiURLOverride string, stdout, stderr io.Writer) error {
	// A city-resolution error (not in a city, or an unreadable city.toml) must
	// not abort: the supervisor may be running regardless of the current dir.
	cityPath, cfg, err := resolveDashboardContext(stderr)
	if err != nil {
		cityPath, cfg = "", nil
	}

	apiURL, err := resolveDashboardAPI(cityPath, cfg, apiURLOverride)
	if err != nil {
		fmt.Fprintf(stdout, "The dashboard is served by the gc supervisor; start it with %q, then open the printed URL.\n", "gc supervisor start") //nolint:errcheck // best-effort stdout
		return nil
	}

	fmt.Fprintf(stdout, "The dashboard is served by the gc supervisor at %s\n", apiURL) //nolint:errcheck // best-effort stdout
	return nil
}

func resolveDashboardContext(warningWriter ...io.Writer) (cityPath string, cfg *config.City, err error) {
	cityPath, err = resolveCity()
	if err != nil {
		if strings.TrimSpace(cityFlag) == "" && strings.Contains(err.Error(), "not in a city directory") {
			return "", nil, nil
		}
		return "", nil, err
	}
	cfg, err = loadCityConfig(cityPath, warningWriter...)
	if err != nil {
		return "", nil, err
	}
	return cityPath, cfg, nil
}

func resolveDashboardAPI(cityPath string, cfg *config.City, apiURLOverride string) (apiURL string, err error) {
	if override := strings.TrimSpace(apiURLOverride); override != "" {
		return strings.TrimRight(override, "/"), nil
	}

	if supervisorAliveHook() != 0 {
		baseURL, err := supervisorAPIBaseURL()
		if err != nil {
			return "", err
		}
		return strings.TrimRight(baseURL, "/"), nil
	}

	if cityPath == "" {
		return "", fmt.Errorf("could not auto-discover the supervisor API; start the supervisor with %q or pass --api explicitly", "gc supervisor start")
	}
	// Standalone-controller mode: the controller's API (cfg.API.Port)
	// now serves the same /v0/city/{cityName}/... surface as the
	// supervisor via api.NewSupervisorMux, so it is a valid target
	// for `gc dashboard`. Return the local address when the config
	// declares a listening port; the dashboard will call ListCities
	// to discover which city/cities are served.
	if hasStandaloneDashboardAPI(cfg) {
		return standaloneAPIBaseURL(cfg), nil
	}
	return "", fmt.Errorf("could not auto-discover the supervisor API for %q; start the supervisor with %q or pass --api explicitly", cityPath, "gc supervisor start")
}

func hasStandaloneDashboardAPI(cfg *config.City) bool {
	return cfg != nil && cfg.API.Port > 0
}

// standaloneAPIBaseURL assembles the local URL of the controller's API.
// The controller publishes /v0/city/{cityName}/... routes, so the CLI
// can target it the same way it targets the supervisor.
//
// Bind normalization:
//   - "" → 127.0.0.1 (empty = default in config.API.BindOrDefault edge cases)
//   - "0.0.0.0" → 127.0.0.1 (listener accepts any v4; connect to loopback)
//   - "::" → ::1 (listener accepts any v6; connect to loopback)
//
// Non-wildcard binds (explicit 127.0.0.1, ::1, 192.168.x.x, 2001::...) are
// passed through unchanged. net.JoinHostPort wraps IPv6 literals in
// brackets so the URL parser sees `http://[::1]:8080/...` correctly;
// plain fmt.Sprintf would produce `http://::1:8080` which parses as
// host=":" port="1:8080" and fails.
func standaloneAPIBaseURL(cfg *config.City) string {
	bind := cfg.API.BindOrDefault()
	switch bind {
	case "", "0.0.0.0":
		bind = "127.0.0.1"
	case "::", "[::]":
		bind = "::1"
	}
	return "http://" + net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))
}
