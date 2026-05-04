package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

func newSessionPinCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "pin <session-id-or-alias>",
		Short: "Keep a session awake",
		Long: `Keep a session awake by setting its durable pin override.

Pinning does not clear suspend holds or other hard blockers. If the target is
a configured named session that has not been materialized yet, pin creates its
canonical bead so the reconciler can start it when unblocked.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionPin(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
}

func newSessionUnpinCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "unpin <session-id-or-alias>",
		Short: "Remove a session awake pin",
		Long: `Remove only the durable pin override from a session.

Unpinning does not force an immediate stop. The reconciler will apply the
normal wake/sleep rules on its next pass.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionUnpin(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
}

func cmdSessionPin(args []string, stdout, stderr io.Writer) int {
	return cmdSessionSetPin(args, true, stdout, stderr)
}

func cmdSessionUnpin(args []string, stdout, stderr io.Writer) int {
	return cmdSessionSetPin(args, false, stdout, stderr)
}

func cmdSessionSetPin(args []string, pinned bool, stdout, stderr io.Writer) int {
	action := "unpin"
	if pinned {
		action = "pin"
	}
	store, code := openCityStore(stderr, "gc session "+action)
	if store == nil {
		return code
	}

	cityPath, cityErr := resolveCity()
	var cfg *config.City
	if cityErr == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}

	var (
		id                 string
		err                error
		materializedForPin bool
	)
	if pinned {
		id, err = resolveSessionIDWithConfig(cityPath, cfg, store, args[0])
		if err != nil {
			id, err = resolveSessionIDMaterializingNamedWithMetadata(cityPath, cfg, store, args[0], map[string]string{
				"pin_awake":            "true",
				"pending_create_claim": "",
			})
			materializedForPin = err == nil
		}
	} else {
		id, err = resolveSessionIDWithConfig(cityPath, cfg, store, args[0])
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc session %s: %v\n", action, err) //nolint:errcheck
		return 1
	}

	b, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc session %s: %v\n", action, err) //nolint:errcheck
		return 1
	}
	if !session.IsSessionBeadOrRepairable(b) {
		fmt.Fprintf(stderr, "gc session %s: %s is not a session\n", action, id) //nolint:errcheck
		return 1
	}
	session.RepairEmptyType(store, &b)
	if b.Status == "closed" {
		fmt.Fprintf(stderr, "gc session %s: session %s is closed\n", action, id) //nolint:errcheck
		return 1
	}

	value := ""
	if pinned {
		value = "true"
	}
	if !materializedForPin {
		if err := store.SetMetadata(id, "pin_awake", value); err != nil {
			fmt.Fprintf(stderr, "gc session %s: updating metadata: %v\n", action, err) //nolint:errcheck
			return 1
		}
	}
	pokeSessionPinController(cityErr, cityPath)

	if pinned {
		fmt.Fprintf(stdout, "Session %s pinned awake.\n", id) //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "Session %s unpinned.\n", id) //nolint:errcheck
	}
	return 0
}

func pokeSessionPinController(cityErr error, cityPath string) {
	if cityErr != nil || !cityUsesManagedReconciler(cityPath) {
		return
	}
	_ = pokeController(cityPath)
}
