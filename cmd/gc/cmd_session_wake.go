package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// newSessionWakeCmd creates the "gc session wake <id-or-alias>" command.
func newSessionWakeCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "wake <session-id-or-alias>",
		Short: "Wake a session (clear hold and quarantine)",
		Long: `Release a user hold and/or crash-loop quarantine on a session.

After waking, the reconciler will start the session on its next tick
if it has wake reasons (e.g., a matching config agent). If the session
has no wake reasons, it remains asleep.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Example: `  gc session wake gc-42
  gc session wake mayor`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionWake(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSessionWake is the CLI entry point for "gc session wake".
func cmdSessionWake(args []string, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc session wake")
	if store == nil {
		return code
	}

	cityPath, cityErr := resolveCity()
	var cfg *config.City
	if cityErr == nil {
		cfg, _ = loadCityConfig(cityPath)
	}
	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session wake: %v\n", err) //nolint:errcheck
		return 1
	}

	b, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc session wake: %v\n", err) //nolint:errcheck
		return 1
	}
	if !session.IsSessionBeadOrRepairable(b) {
		fmt.Fprintf(stderr, "gc session wake: %s is not a session\n", id) //nolint:errcheck
		return 1
	}
	session.RepairEmptyType(store, &b)
	if b.Status == "closed" {
		fmt.Fprintf(stderr, "gc session wake: session %s is closed\n", id) //nolint:errcheck
		return 1
	}

	nudgeIDs, err := session.WakeSession(store, b, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "gc session wake: updating metadata: %v\n", err) //nolint:errcheck
		return 1
	}
	if cityErr == nil {
		if err := withdrawQueuedWaitNudges(cityPath, nudgeIDs); err != nil {
			fmt.Fprintf(stderr, "gc session wake: warning: withdrawing queued wait nudges: %v\n", err) //nolint:errcheck
		}
		if cityUsesManagedReconciler(cityPath) {
			if err := pokeController(cityPath); err != nil {
				fmt.Fprintf(stderr, "gc session wake: warning: poke failed: %v\n", err) //nolint:errcheck
			}
		}
	}

	fmt.Fprintf(stdout, "Session %s: hold and quarantine cleared.\n", id) //nolint:errcheck
	return 0
}
