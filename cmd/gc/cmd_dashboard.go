package main

import (
	"fmt"
	"io"
	"net"
	"strconv"
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
		Short: "Web dashboard for monitoring the city",
		Args:  cobra.NoArgs,
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
		Args:  cobra.NoArgs,
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
	cityPath, cfg, err := resolveDashboardContext()
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

func resolveDashboardContext() (cityPath string, cfg *config.City, err error) {
	cityPath, err = resolveCity()
	if err != nil {
		if strings.TrimSpace(cityFlag) == "" && strings.Contains(err.Error(), "not in a city directory") {
			return "", nil, nil
		}
		return "", nil, err
	}
	cfg, err = loadCityConfig(cityPath)
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
	if _, ok := discoverStandaloneDashboardAPI(cfg); ok {
		return "", fmt.Errorf("dashboard requires the supervisor API; standalone city APIs do not expose /v0/city/{cityName}/... routes. Start the supervisor with %q or pass --api to a supervisor endpoint explicitly", "gc supervisor start")
	}
	return "", fmt.Errorf("could not auto-discover the supervisor API for %q; start the supervisor with %q or pass --api explicitly", cityPath, "gc supervisor start")
}

func discoverStandaloneDashboardAPI(cfg *config.City) (string, bool) {
	if cfg == nil || cfg.API.Port <= 0 {
		return "", false
	}
	bind := normalizeDashboardLocalBind(cfg.API.BindOrDefault())
	return fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))), true
}

func normalizeDashboardLocalBind(bind string) string {
	switch bind {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::", "[::]":
		return "::1"
	default:
		return bind
	}
}
