package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/quota"
	"github.com/spf13/cobra"
)

func newQuotaCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Manage account quota state and rotation",
	}
	cmd.AddCommand(
		newQuotaStatusCmd(stdout, stderr),
		newQuotaClearCmd(stdout, stderr),
	)
	return cmd
}

func newQuotaStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show per-account quota state",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			state, err := quota.LoadState(cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc quota status: %v\n", err) //nolint:errcheck
				return errExit
			}
			if len(state.Accounts) == 0 {
				fmt.Fprintln(stdout, "No quota state recorded yet.") //nolint:errcheck
				return nil
			}
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "HANDLE\tSTATUS\tLIMITED AT\tRESETS AT\tLAST USED\tUSES") //nolint:errcheck
			for _, aq := range state.Accounts {
				limited := "-"
				resets := "-"
				lastUsed := "-"
				if !aq.LimitedAt.IsZero() {
					limited = aq.LimitedAt.Format("15:04:05")
				}
				if !aq.ResetsAt.IsZero() {
					resets = aq.ResetsAt.Format("15:04:05")
				}
				if !aq.LastUsed.IsZero() {
					lastUsed = aq.LastUsed.Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n",
					aq.Handle, aq.Status, limited, resets, lastUsed, aq.UseCount) //nolint:errcheck
			}
			tw.Flush() //nolint:errcheck
			return nil
		},
	}
}

func newQuotaClearCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "clear [handle]",
		Short: "Reset account(s) to available",
		Long:  "Reset a specific account to available, or all accounts if no handle is given.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			err = quota.WithState(cityPath, func(state *quota.QuotaState) error {
				if len(args) == 1 {
					handle := args[0]
					if state.Get(handle) == nil {
						return fmt.Errorf("account %q not found in quota state", handle)
					}
					quota.ClearAccount(state, handle)
					state.Updated = time.Now()
				} else {
					quota.ClearAll(state)
					state.Updated = time.Now()
				}
				return nil
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc quota clear: %v\n", err) //nolint:errcheck
				return errExit
			}
			if len(args) == 1 {
				fmt.Fprintf(stdout, "Cleared quota for %q\n", args[0]) //nolint:errcheck
			} else {
				fmt.Fprintln(stdout, "Cleared all account quotas") //nolint:errcheck
			}
			return nil
		},
	}
}
