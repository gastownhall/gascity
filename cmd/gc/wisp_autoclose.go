package main

import (
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

func newWispCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "wisp",
		Short:  "Wisp lifecycle operations",
		Hidden: true,
	}
	cmd.AddCommand(newWispAutocloseCmd(stdout, stderr))
	return cmd
}

func newWispAutocloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "autoclose <bead-id>",
		Short:  "Auto-close open molecule children of a closed bead",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			doWispAutoclose(args[0], stdout, stderr)
			return nil // always succeed — best-effort infrastructure
		},
	}
}

// doWispAutoclose is the CLI entry point for wisp autoclose.
// It creates a cwd-rooted BdStore (matching the bd process that invoked
// the hook) and delegates to the testable core.
func doWispAutoclose(beadID string, stdout, _ io.Writer) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	store := bdStoreForDir(cwd)
	doWispAutocloseWith(store, beadID, stdout)
}

// doWispAutocloseWith closes any open attached molecule/workflow roots for the
// given bead. Metadata-based attachments are preferred, with child traversal as
// a fallback for legacy data. Called from the bd on_close hook to ensure
// attached wisps don't outlive their parent work bead. All errors are silently
// swallowed — this is best-effort infrastructure.
func doWispAutocloseWith(store beads.Store, beadID string, stdout io.Writer) {
	parent, err := store.Get(beadID)
	if err != nil {
		return
	}
	attachments, err := collectAttachedBeads(parent, store, store)
	if err != nil && len(attachments) == 0 {
		return
	}
	for _, attached := range attachments {
		if attached.Status == "closed" {
			continue
		}
		if err := store.Close(attached.ID); err != nil {
			continue
		}
		fmt.Fprintf(stdout, "Auto-closed %s %s on %s\n", attachmentLabel(attached), attached.ID, beadID) //nolint:errcheck // best-effort stdout
	}
}
