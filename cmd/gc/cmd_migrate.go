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
		Use:    "migrate",
		Hidden: true,
		Short:  "Legacy migration shim",
		Long: `gc import migrate is no longer a public migration surface.

Use "gc doctor" to detect legacy Pack/City v1 shapes and "gc doctor --fix"
to apply safe mechanical repairs. For the remaining manual migration steps,
follow docs/guides/migrating-to-pack-vnext.md.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, `gc import migrate has been removed`) //nolint:errcheck // best-effort stderr
			fmt.Fprintln(stderr, `use "gc doctor" to detect legacy Pack/City v1 shapes`) //nolint:errcheck // best-effort stderr
			fmt.Fprintln(stderr, `use "gc doctor --fix" for safe mechanical repairs`)     //nolint:errcheck // best-effort stderr
			fmt.Fprintln(stderr, `see docs/guides/migrating-to-pack-vnext.md for the remaining manual migration steps`) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "ignored legacy flag kept for compatibility")
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
