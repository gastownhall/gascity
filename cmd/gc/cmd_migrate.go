package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/migrate"
	"github.com/spf13/cobra"
)

func newImportMigrateCmd(stdout, stderr io.Writer) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate a V1 city layout to the V2 pack shape",
		Long: `Rewrite a legacy city into the V2 migration shape.

Moves workspace.includes into pack imports, converts [[agent]] tables
into agents/<name>/ directories, and stages prompt/overlay/namepool
assets into their V2 locations.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doImportMigrate(dryRun, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would change without writing")
	return cmd
}

func doImportMigrate(dryRun bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc import migrate: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	report, err := migrate.Apply(cityPath, migrate.Options{DryRun: dryRun})
	if err != nil {
		fmt.Fprintf(stderr, "gc import migrate: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(report.Changes) == 0 {
		fmt.Fprintln(stdout, "No migration changes needed.") //nolint:errcheck // best-effort stdout
	} else {
		header := "Applied changes"
		if dryRun {
			header = "Planned changes"
		}
		fmt.Fprintf(stdout, "%s for %s:\n", header, cityPath) //nolint:errcheck // best-effort stdout
		for _, change := range report.Changes {
			fmt.Fprintf(stdout, "  - %s\n", change) //nolint:errcheck // best-effort stdout
		}
	}

	for _, warning := range report.Warnings {
		fmt.Fprintf(stdout, "warning: %s\n", warning) //nolint:errcheck // best-effort stdout
	}

	if dryRun {
		fmt.Fprintln(stdout, "No side effects executed (--dry-run).") //nolint:errcheck // best-effort stdout
	}

	return 0
}
