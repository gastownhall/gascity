package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/cmd/gc/dashboard"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

var dashboardServeHook = dashboard.Serve

// newDashboardCmd creates the "gc dashboard" command group.
func newDashboardCmd(stdout, stderr io.Writer) *cobra.Command {
	var port int
	var apiURL string
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Web dashboard for monitoring the supervisor and managed cities",
		Long: `Open the static GC dashboard against the machine-wide supervisor API.

Without a city in scope, the dashboard shows supervisor-level state and managed
city tabs. From a city directory or with --city, city-specific panels and action
forms are enabled for that city.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runDashboardServe("gc dashboard", port, apiURL, stderr) != nil {
				return errExit
			}
			return nil
		},
	}
	bindDashboardServeFlags(cmd, &port, &apiURL)
	cmd.AddCommand(newDashboardServeCmd(stdout, stderr))
	return cmd
}

// newDashboardServeCmd creates the "gc dashboard serve" subcommand.
func newDashboardServeCmd(_, stderr io.Writer) *cobra.Command {
	var port int
	var apiURL string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the web dashboard",
		Long: `Start the static GC dashboard against the machine-wide supervisor API.

Without a city in scope, the dashboard shows supervisor-level state and managed
city tabs. From a city directory or with --city, city-specific panels and action
forms are enabled for that city.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runDashboardServe("gc dashboard serve", port, apiURL, stderr) != nil {
				return errExit
			}
			return nil
		},
	}
	bindDashboardServeFlags(cmd, &port, &apiURL)
	return cmd
}

func bindDashboardServeFlags(cmd *cobra.Command, port *int, apiURL *string) {
	cmd.Flags().IntVar(port, "port", 8080, "HTTP port")
	cmd.Flags().StringVar(apiURL, "api", "", "GC API server URL override (auto-discovered by default)")
}

func runDashboardServe(commandName string, port int, apiURLOverride string, stderr io.Writer) error {
	cityPath, cfg, err := resolveDashboardContext(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return err
	}

	apiURL, err := resolveDashboardAPI(cityPath, cfg, apiURLOverride)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return err
	}

	if err := dashboardServeHook(port, apiURL); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return err
	}
	return nil
}

func resolveDashboardContext(warningWriter ...io.Writer) (cityPath string, cfg *config.City, err error) {
	cityPath, err = resolveCity()
	if err != nil {
		if strings.TrimSpace(cityFlag) == "" && dashboardCanRunWithoutCity(err) {
			return "", nil, nil
		}
		return "", nil, err
	}
	if strings.TrimSpace(cityFlag) == "" && !dashboardCityTomlExists(cityPath) {
		return "", nil, nil
	}
	cfg, err = loadCityConfig(cityPath, warningWriter...)
	if err != nil {
		return "", nil, err
	}
	return cityPath, cfg, nil
}

func dashboardCanRunWithoutCity(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not in a city directory") || strings.Contains(msg, "not a city directory")
}

func dashboardCityTomlExists(cityPath string) bool {
	if strings.TrimSpace(cityPath) == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err == nil {
		return true
	}
	return false
}

func resolveDashboardAPI(cityPath string, cfg *config.City, apiURLOverride string) (apiURL string, err error) {
	if override := strings.TrimSpace(apiURLOverride); override != "" {
		return strings.TrimRight(override, "/"), nil
	}

	if baseURL, ok := discoverReachableSupervisorAPIBaseURL(); ok {
		return baseURL, nil
	}

	if supervisorAliveHook() != 0 {
		baseURL, err := effectiveAPIBaseURLHook()
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
