package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newSessionResetCmd creates the "gc session reset <id-or-alias>" command.
func newSessionResetCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "reset <session-id-or-alias>",
		Short: "Restart a session fresh while preserving the bead",
		Long: `Request a fresh restart for an existing session without closing its bead.

The controller stops the current runtime and starts the same session again with
fresh provider conversation state. Session identity, alias, mail, and queued
work remain attached to the existing session bead.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionReset(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSessionReset is the CLI entry point for "gc session reset".
//
// This command intentionally requires a managed controller. The controller owns
// the fresh restart lifecycle, including key rotation and immediate restart of
// already-desired sessions.
func cmdSessionReset(args []string, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc session reset")
	if store == nil {
		return code
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !cityUsesManagedReconciler(cityPath) {
		fmt.Fprintln(stderr, "gc session reset: a managed controller must be running") //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := pokeController(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cfg, _ := loadCityConfig(cityPath)

	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := store.SetMetadataBatch(sessionID, map[string]string{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	}); err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	_ = pokeController(cityPath)

	fmt.Fprintf(stdout, "Session %s reset requested. Controller will restart it fresh.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}
