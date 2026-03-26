package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/quota"
	"github.com/spf13/cobra"
)

func newAccountCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "Manage provider accounts for quota rotation",
	}
	cmd.AddCommand(
		newAccountListCmd(stdout, stderr),
		newAccountAddCmd(stdout, stderr),
		newAccountDefaultCmd(stdout, stderr),
		newAccountRemoveCmd(stdout, stderr),
		newAccountStatusCmd(stdout, stderr),
	)
	return cmd
}

func newAccountListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered accounts",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			reg, err := account.Load(cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc account list: %v\n", err) //nolint:errcheck
				return errExit
			}
			if len(reg.Accounts) == 0 {
				fmt.Fprintln(stdout, "No accounts registered. Add one with: gc account add <handle> --config-dir <path>") //nolint:errcheck
				return nil
			}
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "HANDLE\tPROVIDER\tCONFIG DIR\tDEFAULT") //nolint:errcheck
			for _, a := range reg.Accounts {
				def := ""
				if a.Handle == reg.Default {
					def = "*"
				}
				prov := a.Provider
				if prov == "" {
					prov = "(any)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Handle, prov, a.ConfigDir, def) //nolint:errcheck
			}
			tw.Flush() //nolint:errcheck
			return nil
		},
	}
}

func newAccountAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var configDir string
	var provider string
	var description string
	var setDefault bool

	cmd := &cobra.Command{
		Use:   "add <handle>",
		Short: "Register a new provider account",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle := args[0]
			if configDir == "" {
				fmt.Fprintln(stderr, "gc account add: --config-dir is required") //nolint:errcheck
				return errExit
			}
			// Verify config dir exists.
			if fi, err := os.Stat(configDir); err != nil || !fi.IsDir() {
				fmt.Fprintf(stderr, "gc account add: config-dir %q does not exist or is not a directory\n", configDir) //nolint:errcheck
				return errExit
			}
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			err = account.WithRegistry(cityPath, func(reg *account.Registry) error {
				if err := account.Add(reg, account.Account{
					Handle:      handle,
					Description: description,
					ConfigDir:   configDir,
					Provider:    provider,
				}); err != nil {
					return err
				}
				if setDefault || len(reg.Accounts) == 1 {
					reg.Default = handle
				}
				return nil
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc account add: %v\n", err) //nolint:errcheck
				return errExit
			}
			fmt.Fprintf(stdout, "Added account %q (config-dir: %s)\n", handle, configDir) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&configDir, "config-dir", "", "path to provider config directory (CLAUDE_CONFIG_DIR)")
	cmd.Flags().StringVar(&provider, "provider", "", "provider name (e.g., claude, gemini)")
	cmd.Flags().StringVar(&description, "description", "", "human-readable description")
	cmd.Flags().BoolVar(&setDefault, "default", false, "set as default account")
	return cmd
}

func newAccountDefaultCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "default <handle>",
		Short: "Set the default account",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			err = account.WithRegistry(cityPath, func(reg *account.Registry) error {
				return account.SetDefault(reg, args[0])
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc account default: %v\n", err) //nolint:errcheck
				return errExit
			}
			fmt.Fprintf(stdout, "Default account set to %q\n", args[0]) //nolint:errcheck
			return nil
		},
	}
}

func newAccountRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <handle>",
		Short: "Remove a registered account",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			err = account.WithRegistry(cityPath, func(reg *account.Registry) error {
				return account.Remove(reg, args[0])
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc account remove: %v\n", err) //nolint:errcheck
				return errExit
			}
			fmt.Fprintf(stdout, "Removed account %q\n", args[0]) //nolint:errcheck
			return nil
		},
	}
}

func newAccountStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show accounts with quota state",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			reg, err := account.Load(cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc account status: %v\n", err) //nolint:errcheck
				return errExit
			}
			if len(reg.Accounts) == 0 {
				fmt.Fprintln(stdout, "No accounts registered.") //nolint:errcheck
				return nil
			}
			qs, _ := quota.LoadState(cityPath)

			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "HANDLE\tPROVIDER\tQUOTA\tLAST USED\tUSES") //nolint:errcheck
			for _, a := range reg.Accounts {
				prov := a.Provider
				if prov == "" {
					prov = "(any)"
				}
				qStatus := "available"
				lastUsed := "-"
				uses := "0"
				if aq := qs.Get(a.Handle); aq != nil {
					qStatus = string(aq.Status)
					if !aq.LastUsed.IsZero() {
						lastUsed = aq.LastUsed.Format("2006-01-02 15:04")
					}
					uses = fmt.Sprintf("%d", aq.UseCount)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Handle, prov, qStatus, lastUsed, uses) //nolint:errcheck
			}
			tw.Flush() //nolint:errcheck
			return nil
		},
	}
}
