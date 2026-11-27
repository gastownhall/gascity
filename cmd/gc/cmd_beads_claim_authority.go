package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func newBeadsClaimAuthorityCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claim-authority",
		Short: "Operate the claim-authority boundary of the city's bead stores",
		Long: `Operate the claim-authority boundary of the city's bead stores.

An armed store refuses a routed claim by any identity no city published.
The controller arms stores under [beads] claim_authority = "auto" or
"require" and never disarms one; "gc doctor" reports each store's state.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc beads claim-authority: missing subcommand (disarm)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc beads claim-authority: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(newBeadsClaimAuthorityDisarmCmd(stdout, stderr))
	return cmd
}

func newBeadsClaimAuthorityDisarmCmd(stdout, stderr io.Writer) *cobra.Command {
	var store string
	var force bool
	cmd := &cobra.Command{
		Use:   "disarm",
		Short: "Remove the claim-authority triggers from the city's bead stores",
		Long: `Remove the claim-authority triggers from the city's bead stores.

Refuses a store while any publisher heartbeat advertises auto or require:
set those cities to off first, or the next controller tick re-arms it.
Refuses a store with no heartbeat at all, where co-tenant intent is
unknown, unless --force is given. Published rows are left in place.`,
		Example: `  gc beads claim-authority disarm
  gc beads claim-authority disarm --store rig/frontend
  gc beads claim-authority disarm --force`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc beads claim-authority disarm: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc beads claim-authority disarm: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if doBeadsClaimAuthorityDisarm(cityPath, cfg, store, force, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&store, "store", "", `disarm only this store ("city" or "rig/<name>")`)
	cmd.Flags().BoolVar(&force, "force", false, "disarm a store that has no publisher heartbeat")
	return cmd
}

// doBeadsClaimAuthorityDisarm disarms every selected bd scope, reporting each
// one, and returns 1 when any scope was refused or failed.
func doBeadsClaimAuthorityDisarm(cityPath string, cfg *config.City, store string, force bool, stdout, stderr io.Writer) int {
	store = strings.TrimSpace(store)
	matched, failed := false, false
	for _, scope := range claimAuthorityScopes(cityPath, cfg) {
		if store != "" && scope.ID != store {
			continue
		}
		matched = true
		var dropped bool
		reached, err := claimAuthorityStoreConn(cityPath, scope.Root, func(ctx context.Context, conn *sql.Conn) error {
			var derr error
			dropped, derr = beads.DisarmClaimAuthorityGuarded(ctx, conn, force)
			return derr
		})
		switch {
		case err != nil:
			failed = true
			fmt.Fprintf(stderr, "gc beads claim-authority disarm: %s: %v\n", scope.ID, err) //nolint:errcheck // best-effort stderr
		case !reached:
			fmt.Fprintf(stdout, "%s: no direct SQL endpoint; nothing to disarm\n", scope.ID) //nolint:errcheck // best-effort stdout
		case dropped:
			fmt.Fprintf(stdout, "%s: disarmed\n", scope.ID) //nolint:errcheck // best-effort stdout
		default:
			fmt.Fprintf(stdout, "%s: not armed\n", scope.ID) //nolint:errcheck // best-effort stdout
		}
	}
	if store != "" && !matched {
		fmt.Fprintf(stderr, "gc beads claim-authority disarm: no bd store %q in this city\n", store) //nolint:errcheck // best-effort stderr
		return 1
	}
	if failed {
		return 1
	}
	return 0
}
