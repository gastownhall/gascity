package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/spf13/cobra"
)

// newAccountCmd creates the "gc account" parent command with subcommands
// for managing the account registry.
func newAccountCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "Manage provider account registrations",
		Long: `Register, list, and manage provider accounts for the city.

Accounts map a short handle to an API key configuration directory.
Use gc account add to register accounts, gc account default to set
the preferred account, and gc account list to view all registrations.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc account: missing subcommand (list, add, default, remove, status)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc account: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
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

// newAccountListCmd creates the "gc account list" command.
func newAccountListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all registered accounts",
		RunE: func(_ *cobra.Command, _ []string) error {
			if doAccountList(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// doAccountList lists all registered accounts in a formatted table.
func doAccountList(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc account list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	regPath := citylayout.AccountsFilePath(cityPath)
	reg, err := account.Load(regPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc account list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(reg.Accounts) == 0 {
		fmt.Fprintln(stderr, "error: no accounts registered. Run gc account add to register at least one account.") //nolint:errcheck // best-effort stderr
		return 1
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HANDLE\tEMAIL\tDESCRIPTION\tCONFIG DIR\tDEFAULT") //nolint:errcheck // best-effort stdout
	for _, acct := range reg.Accounts {
		def := ""
		if acct.Handle == reg.Default {
			def = "default"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", acct.Handle, acct.Email, acct.Description, acct.ConfigDir, def) //nolint:errcheck // best-effort stdout
	}
	w.Flush() //nolint:errcheck // best-effort flush
	return 0
}

// newAccountAddCmd creates the "gc account add" command.
func newAccountAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var handle, email, description, configDir string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Register a new provider account",
		Example: `  gc account add --handle work1 --email user@example.com --config-dir ~/.claude-accounts/work1
  gc account add --handle work2 --email user2@example.com --description "Second account" --config-dir ~/.claude-accounts/work2`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doAccountAdd(handle, email, description, configDir, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&handle, "handle", "", "short name for the account (required)")
	cmd.Flags().StringVar(&email, "email", "", "email address associated with the account (required)")
	cmd.Flags().StringVar(&description, "description", "", "optional description")
	cmd.Flags().StringVar(&configDir, "config-dir", "", "path to the API key configuration directory (required)")
	return cmd
}

// doAccountAdd validates and registers a new account.
func doAccountAdd(handle, email, description, configDir string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc account add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	regPath := citylayout.AccountsFilePath(cityPath)
	reg, err := account.Load(regPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc account add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	acct := account.Account{
		Handle:      handle,
		Email:       email,
		Description: description,
		ConfigDir:   configDir,
	}

	if err := account.ValidateNewAccount(reg, acct); err != nil {
		fmt.Fprintf(stderr, "gc account add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	reg.Accounts = append(reg.Accounts, acct)
	if err := account.Save(regPath, reg); err != nil {
		fmt.Fprintf(stderr, "gc account add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "account %s registered\n", handle) //nolint:errcheck // best-effort stdout
	return 0
}

// newAccountDefaultCmd creates the "gc account default" command.
func newAccountDefaultCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "default <handle>",
		Short: "Set the default account for this city",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doAccountDefault(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// doAccountDefault sets the default account handle.
func doAccountDefault(handle string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc account default: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	regPath := citylayout.AccountsFilePath(cityPath)
	reg, err := account.Load(regPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc account default: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(reg.Accounts) == 0 {
		fmt.Fprintln(stderr, "error: no accounts registered. Run gc account add to register at least one account.") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Verify the handle exists in the registry.
	found := false
	for _, acct := range reg.Accounts {
		if acct.Handle == handle {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(stderr, "gc account default: account %q is not registered\n", handle) //nolint:errcheck // best-effort stderr
		return 1
	}

	reg.Default = handle
	if err := account.Save(regPath, reg); err != nil {
		fmt.Fprintf(stderr, "gc account default: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "default account set to %s\n", handle) //nolint:errcheck // best-effort stdout
	return 0
}

// newAccountRemoveCmd creates the "gc account remove" command.
func newAccountRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <handle>",
		Short: "Deregister an account by handle",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc account remove: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			ops := DefaultTmuxOps(filepath.Base(cityPath))
			if doAccountRemove(args[0], ops, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// doAccountRemove removes an account from the registry. If active tmux
// sessions reference the account being removed, a warning is emitted to
// stderr (informational only — removal still proceeds). If the removed
// account is the current default, the default is cleared and a warning
// is emitted.
func doAccountRemove(handle string, ops TmuxOps, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc account remove: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	regPath := citylayout.AccountsFilePath(cityPath)
	reg, err := account.Load(regPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc account remove: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Find the account being removed so we can check its config dir.
	idx := -1
	var removedConfigDir string
	for i, acct := range reg.Accounts {
		if acct.Handle == handle {
			idx = i
			removedConfigDir = acct.ConfigDir
			break
		}
	}
	if idx == -1 {
		fmt.Fprintf(stderr, "gc account remove: account %q is not registered\n", handle) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Warn if any active tmux sessions reference this account (GAP-2 fix).
	// This is best-effort: if tmux is not running, skip silently.
	if ops.IsRunning != nil && ops.IsRunning() {
		if panes, err := ops.ListPanes(); err == nil {
			var affectedSessions []string
			for _, pane := range panes {
				configDir, _ := ops.ShowEnv(pane.SessionName, "CLAUDE_CONFIG_DIR")
				if configDir == removedConfigDir {
					affectedSessions = append(affectedSessions, pane.SessionName)
				}
			}
			if len(affectedSessions) > 0 {
				fmt.Fprintf(stderr, "warning: account %s is in use by active session(s): %s. Proceeding with removal.\n", handle, strings.Join(affectedSessions, ", ")) //nolint:errcheck // best-effort stderr
			}
		}
	}

	reg.Accounts = append(reg.Accounts[:idx], reg.Accounts[idx+1:]...)

	// If the removed account was the default, clear the default and warn.
	wasDefault := reg.Default == handle
	if wasDefault {
		reg.Default = ""
	}

	if err := account.Save(regPath, reg); err != nil {
		fmt.Fprintf(stderr, "gc account remove: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Remove the handle's entry from quota.json if it exists (GAP-1 fix).
	if err := removeQuotaEntry(cityPath, handle); err != nil {
		fmt.Fprintf(stderr, "gc account remove: warning: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal — the account was already removed from accounts.json.
	}

	fmt.Fprintf(stdout, "account %s removed\n", handle) //nolint:errcheck // best-effort stdout
	if wasDefault {
		fmt.Fprintf(stderr, "warning: %s was the default account — run gc account default to set a new one\n", handle) //nolint:errcheck // best-effort stderr
	}
	return 0
}

// newAccountStatusCmd creates the "gc account status" command.
// It reads CLAUDE_CONFIG_DIR from each tmux session's environment and
// reverse-maps the path to the matching account handle in the registry.
func newAccountStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the active account for sessions",
		Long: `Show which account each active session is using.

This command reads CLAUDE_CONFIG_DIR from tmux session environments and
reverse-maps the path to the matching account handle.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc account status: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			regPath := citylayout.AccountsFilePath(cityPath)
			reg, err := account.Load(regPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc account status: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			ops := DefaultTmuxOps(filepath.Base(cityPath))
			if doAccountStatus(ops, reg, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// doAccountStatus displays per-session account information by reading
// CLAUDE_CONFIG_DIR from each tmux session and reverse-mapping it to an
// account handle. Returns 0 on success, 1 on error.
func doAccountStatus(ops TmuxOps, reg account.Registry, stdout, stderr io.Writer) int {
	if len(reg.Accounts) == 0 {
		fmt.Fprintln(stderr, "error: no accounts registered. Run gc account add to register at least one account.") //nolint:errcheck // best-effort stderr
		return 1
	}

	if !ops.IsRunning() {
		fmt.Fprintln(stderr, "error: tmux is not running. gc account status requires an active tmux server.") //nolint:errcheck // best-effort stderr
		return 1
	}

	panes, err := ops.ListPanes()
	if err != nil {
		fmt.Fprintf(stderr, "gc account status: listing panes: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(panes) == 0 {
		fmt.Fprintln(stdout, "no active sessions") //nolint:errcheck // best-effort stdout
		return 0
	}

	// Build a reverse map from config dir to account handle.
	dirToHandle := make(map[string]string, len(reg.Accounts))
	for _, acct := range reg.Accounts {
		dirToHandle[acct.ConfigDir] = acct.Handle
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tACCOUNT\tCONFIG DIR") //nolint:errcheck // best-effort stdout
	for _, pane := range panes {
		configDir, _ := ops.ShowEnv(pane.SessionName, "CLAUDE_CONFIG_DIR")
		handle := "(no account)"
		if configDir != "" {
			if h, ok := dirToHandle[configDir]; ok {
				handle = h
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", pane.SessionName, handle, configDir) //nolint:errcheck // best-effort stdout
	}
	w.Flush() //nolint:errcheck // best-effort flush
	return 0
}

// removeQuotaEntry removes the given handle's entry from quota.json using
// the formal quota I/O layer (loadQuotaState/saveQuotaState). If quota.json
// does not exist, loadQuotaState returns an empty state and no file is
// written (Level 0 compatibility). If the handle is not present in the
// quota state, this is a no-op.
func removeQuotaEntry(cityPath, handle string) error {
	quotaPath := citylayout.QuotaFilePath(cityPath)

	state, err := loadQuotaState(quotaPath)
	if err != nil {
		return fmt.Errorf("loading quota state: %w", err)
	}

	// If the handle is not in the accounts map, nothing to do.
	if _, exists := state.Accounts[handle]; !exists {
		return nil
	}

	delete(state.Accounts, handle)

	return saveQuotaState(quotaPath, state)
}
