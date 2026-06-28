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

// openDashboardURLHook opens the resolved dashboard URL in the user's browser.
// It is a package variable so tests can stub the browser launch.
var openDashboardURLHook = openURL

// newDashboardCmd creates the "gc dashboard" command group.
//
// The dashboard is no longer a standalone cross-origin static server. The
// compiled SPA is embedded into the gc binary and served same-origin by the
// supervisor, so this command resolves where it is served and opens the user's
// browser there (or prints how to start the supervisor).
func newDashboardCmd(stdout, stderr io.Writer) *cobra.Command {
	var apiURL string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the web dashboard in your browser",
		Long: `Open the GC dashboard in your browser.

The dashboard SPA is embedded in the gc binary and served same-origin by the
supervisor; it is no longer a separate static server. This command resolves the
supervisor URL, opens it in your default browser, and prints it too (or tells
you how to start the supervisor). Use --no-open to print the URL without
launching a browser.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDashboardNotice(apiURL, noOpen, stdout, stderr)
		},
	}
	bindDashboardFlags(cmd, &apiURL, &noOpen)
	cmd.AddCommand(newDashboardServeCmd(stdout, stderr))
	return cmd
}

// newDashboardServeCmd creates the "gc dashboard serve" subcommand.
//
// Retained for backwards compatibility: the dashboard is served by the
// supervisor, so this resolves and prints the supervisor URL rather than
// starting a server. It does not open a browser.
func newDashboardServeCmd(stdout, stderr io.Writer) *cobra.Command {
	var apiURL string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Print where the web dashboard is served",
		Long: `Report the URL where the GC dashboard is served.

The dashboard SPA is embedded in the gc binary and served same-origin by the
supervisor; "gc dashboard serve" no longer starts a static server. It resolves
and prints the supervisor URL (or tells you how to start the supervisor).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// "serve" is the legacy print-only entry point: never open a browser.
			return runDashboardNotice(apiURL, true, stdout, stderr)
		},
	}
	bindDashboardFlags(cmd, &apiURL, &noOpen)
	return cmd
}

func bindDashboardFlags(cmd *cobra.Command, apiURL *string, noOpen *bool) {
	cmd.Flags().StringVar(apiURL, "api", "", "GC API server URL override (auto-discovered by default)")
	cmd.Flags().BoolVar(noOpen, "no-open", false, "print the dashboard URL instead of opening a browser")
}

// runDashboardNotice resolves where the supervisor serves the dashboard SPA,
// opens it in the user's browser, and prints the URL. It is purely
// informational and always exits 0: city/config resolution only feeds the
// standalone-controller fallback, so a failure there is non-fatal — the command
// falls back to live supervisor discovery and still prints a useful answer (the
// supervisor URL, or how to start it).
//
// Browser-open is best-effort and only happens on the served path: when noOpen
// is false and the URL actually resolves, it launches the browser via
// openDashboardURLHook; a launch failure just falls back to the printed URL and
// never errors the command. The not-running path (URL unresolvable) prints the
// start hint and never opens a (dead) URL.
func runDashboardNotice(apiURLOverride string, noOpen bool, stdout, stderr io.Writer) error {
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

	// The URL is live (supervisor/standalone/override resolved): open it in the
	// browser unless --no-open was passed, then always print it so it stays
	// copyable when the browser does not open.
	if !noOpen {
		if openErr := openDashboardURLHook(apiURL); openErr != nil {
			fmt.Fprintf(stdout, "Could not open a browser automatically; open this URL:\n%s\n", apiURL) //nolint:errcheck // best-effort stdout
			return nil
		}
		fmt.Fprintf(stdout, "Opened the dashboard in your browser: %s\n", apiURL) //nolint:errcheck // best-effort stdout
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
