package main

import (
	"fmt"
	"github.com/spf13/cobra"
	"io"
)

func newImportMigrateCmd(stdout, stderr io.Writer) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:        "migrate",
		Short:      "Deprecated compatibility shim for legacy migration",
		Hidden:     true,
		Deprecated: `use "gc doctor" and "gc doctor --fix"`,
		Long: `Deprecated compatibility shim.

Use "gc doctor" to inspect legacy PackV1 surfaces and
"gc doctor --fix" to rewrite the safe mechanical cases.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doImportMigrate(dryRun, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "deprecated no-op compatibility flag")
	return cmd
}

func doImportMigrate(dryRun bool, stdout, stderr io.Writer) int {
	_ = dryRun
	fmt.Fprintln(stderr, "gc import migrate has been deprecated.")                                                                                 //nolint:errcheck // best-effort stderr
	fmt.Fprintln(stderr, `Use "gc doctor" to inspect legacy PackV1 surfaces.`)                                                                     //nolint:errcheck // best-effort stderr
	fmt.Fprintln(stderr, `Use "gc doctor --fix" to rewrite the safe mechanical cases, then rerun "gc doctor".`)                                    //nolint:errcheck // best-effort stderr
	fmt.Fprintln(stderr, `This shim no longer performs in-place PackV1-to-PackV2 rewrites.`)                                                       //nolint:errcheck // best-effort stderr
	fmt.Fprintln(stderr, `See docs/guides/migrating-to-pack-vnext.md for the remaining manual migration steps and repo-content cleanup guidance.`) //nolint:errcheck // best-effort stderr
	return 1
}
