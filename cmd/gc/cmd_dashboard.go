package main

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/cmd/gc/dashboard"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

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
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return err
	}

	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return err
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}

	apiURL, initialCityScope, err := resolveDashboardAPI(cityPath, cfg, apiURLOverride)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return err
	}

	// cityName/cityPath/initialCityScope are no longer used by the
	// dashboard Go layer — the SPA reads the city scope from its
	// query string and calls the supervisor directly. They survive
	// here only to keep the resolve* helpers consistent across
	// commands; if they become pure dead code elsewhere, delete.
	_ = cityName
	_ = initialCityScope

	if err := dashboard.Serve(port, apiURL); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return err
	}
	return nil
}

func resolveDashboardAPI(cityPath string, cfg *config.City, apiURLOverride string) (apiURL, initialCityScope string, err error) {
	if override := strings.TrimSpace(apiURLOverride); override != "" {
		return strings.TrimRight(override, "/"), "", nil
	}

	if supervisorURL, cityScope, ok, err := discoverSupervisorDashboardAPI(cityPath); err != nil {
		return "", "", err
	} else if ok {
		return supervisorURL, cityScope, nil
	}

	if standaloneURL, ok := discoverStandaloneDashboardAPI(cfg); ok {
		return standaloneURL, "", nil
	}

	return "", "", fmt.Errorf("could not auto-discover a GC API server; start the city with %q or pass --api explicitly", "gc start")
}

func discoverSupervisorDashboardAPI(cityPath string) (apiURL, cityScope string, ok bool, err error) {
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil {
		return "", "", false, err
	}
	if !registered || supervisorAliveHook() == 0 {
		return "", "", false, nil
	}
	running, _, known := supervisorCityRunningHook(cityPath)
	if !known || !running {
		return "", "", false, nil
	}
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return "", "", false, err
	}
	return strings.TrimRight(baseURL, "/"), entry.EffectiveName(), true, nil
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
