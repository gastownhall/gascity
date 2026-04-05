package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/cmd/gc/dashboard"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// newDashboardCmd creates the "gc dashboard" command group.
func newDashboardCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Web dashboard for monitoring the city",
	}
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
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc dashboard serve: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
			if err != nil {
				fmt.Fprintf(stderr, "gc dashboard serve: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			cityName := cfg.Workspace.Name
			if cityName == "" {
				cityName = filepath.Base(cityPath)
			}

			if apiURL == "" {
				fmt.Fprintf(stderr, "gc dashboard serve: --api flag is required (GC API server URL)\n") //nolint:errcheck // best-effort stderr
				return errExit
			}

			return dashboard.Serve(port, cityPath, cityName, apiURL)
		},
	}
	cmd.Flags().IntVar(&port, "port", 8080, "HTTP port")
	cmd.Flags().StringVar(&apiURL, "api", "", "GC API server URL (e.g. standalone http://127.0.0.1:9443, supervisor http://127.0.0.1:8372)")
	return cmd
}
