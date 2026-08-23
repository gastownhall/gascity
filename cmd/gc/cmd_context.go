package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/spf13/cobra"
)

// newContextCmd builds `gc context`, the client-side registry of named remote
// cities (the kubeconfig analog) stored in ~/.gc/contexts.toml. It manages
// where a remote city is, which city it is, and how to authenticate to it;
// actual remote operation is driven by the --context/--city-url flags resolved
// in resolveContext.
func newContextCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage named remote cities (~/.gc/contexts.toml)",
		Long: `Manage the client-side registry of named remote cities.

A context names a remote city the gc CLI can operate over the HTTP+SSE control
plane: its URL, the remote city name, and an optional credential command. Select
a context per-invocation with --context <name>, or set a sticky default with
'gc context use <name>' (a discoverable local city always wins over the default).`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newContextAddCmd(stdout, stderr),
		newContextListCmd(stdout, stderr),
		newContextUseCmd(stdout, stderr),
		newContextCurrentCmd(stdout, stderr),
		newContextRemoveCmd(stdout, stderr),
		newContextShowCmd(stdout, stderr),
	)
	return cmd
}

// contextJSON is the wire shape for `gc context list/show -o json`.
type contextJSON struct {
	Name              string `json:"name"`
	URL               string `json:"url"`
	City              string `json:"city"`
	Default           bool   `json:"default"`
	CredentialCommand string `json:"credential_command,omitempty"`
	GrantCommand      string `json:"grant_command,omitempty"`
}

func newContextAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var c clientcontext.Context
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a named remote city",
		Long: `Add a named remote city to ~/.gc/contexts.toml.

--url is required and must be https for a non-loopback host. --city sets the
remote city name (defaults to <name>). At most one credential technique applies:
--grant-command mints an X-GC-City-Write grant for a direct hardened self-host;
--credential-command mints a transport bearer consumed by an edge/proxy.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c.Name = args[0]
			if doContextAdd(c, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&c.URL, "url", "", "remote city base URL (https required for non-loopback)")
	f.StringVar(&c.City, "city", "", "remote city name (default: <name>)")
	f.StringVar(&c.GrantCommand, "grant-command", "", "command that mints an X-GC-City-Write grant (direct hardened self-host)")
	f.StringVar(&c.CredentialCommand, "credential-command", "", "command that mints a transport bearer (edge/proxy fronted)")
	f.StringVar(&c.CAFile, "ca-file", "", "PEM CA bundle to verify the server certificate")
	f.StringVar(&c.TLSServerName, "tls-server-name", "", "override the TLS SNI / certificate name")
	f.BoolVar(&c.InsecureSkipVerify, "insecure-skip-verify", false, "skip TLS verification (dev only)")
	f.StringVar(&c.Timeout, "timeout", "", "REST request timeout, e.g. 120s (never applied to SSE streams)")
	return cmd
}

func doContextAdd(c clientcontext.Context, stdout, stderr io.Writer) int {
	if err := c.Validate(); err != nil {
		fmt.Fprintf(stderr, "gc context add: %v\n", err) //nolint:errcheck
		return 1
	}
	path := DefaultPath()
	file, err := clientcontext.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "gc context add: %v\n", err) //nolint:errcheck
		return 1
	}
	if _, exists := file.Lookup(c.Name); exists {
		fmt.Fprintf(stderr, "gc context add: context %q already exists (remove it first with 'gc context remove %s')\n", c.Name, c.Name) //nolint:errcheck
		return 1
	}
	file.Contexts = append(file.Contexts, c)
	if err := file.Validate(); err != nil {
		fmt.Fprintf(stderr, "gc context add: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := file.Save(path); err != nil {
		fmt.Fprintf(stderr, "gc context add: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Added context %q -> %s @ %s\n", c.Name, c.EffectiveCity(), c.URL) //nolint:errcheck
	return 0
}

func newContextListCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List named remote cities",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doContextList(jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&jsonOut, "json", "", false, "emit one JSONL record per context")
	return cmd
}

func doContextList(jsonOut bool, stdout, stderr io.Writer) int {
	path := DefaultPath()
	file, err := clientcontext.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "gc context list: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOut {
		for i := range file.Contexts {
			c := file.Contexts[i]
			if err := writeCLIJSONLine(stdout, contextToJSON(c, c.Name == file.Default)); err != nil {
				fmt.Fprintf(stderr, "gc context list: writing JSON: %v\n", err) //nolint:errcheck
				return 1
			}
		}
		return 0
	}
	if len(file.Contexts) == 0 {
		fmt.Fprintln(stdout, "No contexts. Use 'gc context add <name> --url <url>' to add one.") //nolint:errcheck
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\tNAME\tCITY\tURL\tCRED") //nolint:errcheck
	for i := range file.Contexts {
		c := file.Contexts[i]
		star := " "
		if c.Name == file.Default {
			star = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", star, c.Name, c.EffectiveCity(), c.URL, credLabel(c)) //nolint:errcheck
	}
	tw.Flush() //nolint:errcheck
	return 0
}

func newContextUseCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "use <name>",
		Short: "Set the sticky default context",
		Long: `Set the sticky default remote city.

The default is used only when no local city is discoverable from the current
directory — a local city always wins (git-like). Clear it with 'gc context use'
with no arguments is not supported; remove the default by removing the context.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doContextUse(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func doContextUse(name string, stdout, stderr io.Writer) int {
	path := DefaultPath()
	file, err := clientcontext.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "gc context use: %v\n", err) //nolint:errcheck
		return 1
	}
	if _, ok := file.Lookup(name); !ok {
		fmt.Fprintf(stderr, "gc context use: context %q is not defined (run 'gc context list')\n", name) //nolint:errcheck
		return 1
	}
	file.Default = name
	if err := file.Save(path); err != nil {
		fmt.Fprintf(stderr, "gc context use: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Default context set to %q (subordinate to a local city in the current directory).\n", name) //nolint:errcheck
	return 0
}

func newContextRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove <name>",
		Short:   "Remove a named remote city",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doContextRemove(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func doContextRemove(name string, stdout, stderr io.Writer) int {
	path := DefaultPath()
	file, err := clientcontext.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "gc context remove: %v\n", err) //nolint:errcheck
		return 1
	}
	idx := -1
	for i := range file.Contexts {
		if file.Contexts[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		fmt.Fprintf(stderr, "gc context remove: context %q is not defined\n", name) //nolint:errcheck
		return 1
	}
	file.Contexts = append(file.Contexts[:idx], file.Contexts[idx+1:]...)
	clearedDefault := false
	if file.Default == name {
		file.Default = ""
		clearedDefault = true
	}
	if err := file.Save(path); err != nil {
		fmt.Fprintf(stderr, "gc context remove: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Removed context %q.\n", name) //nolint:errcheck
	if clearedDefault {
		fmt.Fprintln(stdout, "It was the default; no default context is set now.") //nolint:errcheck
	}
	return 0
}

func newContextShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a named remote city",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doContextShow(args[0], jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&jsonOut, "json", "", false, "emit a JSONL record")
	return cmd
}

func doContextShow(name string, jsonOut bool, stdout, stderr io.Writer) int {
	path := DefaultPath()
	file, err := clientcontext.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "gc context show: %v\n", err) //nolint:errcheck
		return 1
	}
	c, ok := file.Lookup(name)
	if !ok {
		fmt.Fprintf(stderr, "gc context show: context %q is not defined\n", name) //nolint:errcheck
		return 1
	}
	isDefault := file.Default == name
	if jsonOut {
		if err := writeCLIJSONLine(stdout, contextToJSON(*c, isDefault)); err != nil {
			fmt.Fprintf(stderr, "gc context show: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "name:\t%s\n", c.Name)              //nolint:errcheck
	fmt.Fprintf(tw, "url:\t%s\n", c.URL)                //nolint:errcheck
	fmt.Fprintf(tw, "city:\t%s\n", c.EffectiveCity())   //nolint:errcheck
	fmt.Fprintf(tw, "default:\t%t\n", isDefault)        //nolint:errcheck
	fmt.Fprintf(tw, "credential:\t%s\n", credLabel(*c)) //nolint:errcheck
	if c.CAFile != "" {
		fmt.Fprintf(tw, "ca_file:\t%s\n", c.CAFile) //nolint:errcheck
	}
	if c.TLSServerName != "" {
		fmt.Fprintf(tw, "tls_server_name:\t%s\n", c.TLSServerName) //nolint:errcheck
	}
	if c.Timeout != "" {
		fmt.Fprintf(tw, "timeout:\t%s\n", c.Timeout) //nolint:errcheck
	}
	tw.Flush() //nolint:errcheck
	return 0
}

func newContextCurrentCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "current",
		Short: "Show which city the current flags/env/cwd would target",
		Long: `Dry-run the target resolver and report the winning tier.

Applies the same precedence as every command — explicit flag > explicit env >
local city discovery > sticky default — and prints the target it would use,
noting what was shadowed. Makes no network call.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doContextCurrent(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func doContextCurrent(stdout, stderr io.Writer) int {
	path := DefaultPath()
	file, err := clientcontext.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "gc context current: %v\n", err) //nolint:errcheck
		return 1
	}
	sel := readRemoteSelection()
	target, handled, err := resolveRemoteSelection(sel, file)
	if err != nil {
		fmt.Fprintf(stderr, "gc context current: %v\n", err) //nolint:errcheck
		return 1
	}
	if handled {
		fmt.Fprintln(stdout, formatRemoteTarget(target)) //nolint:errcheck
		return 0
	}
	// No explicit remote selector: local city discovery wins if a city exists.
	if cityPath, ok := probeLocalCity(); ok {
		fmt.Fprintf(stdout, "local city: %s (source: local discovery)\n", cityPath) //nolint:errcheck
		if file.Default != "" {
			fmt.Fprintf(stdout, "  note: default context %q is shadowed by the local city\n", file.Default) //nolint:errcheck
		}
		return 0
	}
	// No local city: the sticky default (if any) is the last resort.
	if def, ok, derr := resolveStickyDefault(file); derr != nil {
		fmt.Fprintf(stderr, "gc context current: %v\n", derr) //nolint:errcheck
		return 1
	} else if ok {
		fmt.Fprintln(stdout, formatRemoteTarget(def)) //nolint:errcheck
		return 0
	}
	fmt.Fprintln(stdout, "no city resolvable: not in a city directory, and no --context/GC_CITY_CONTEXT or default context is set") //nolint:errcheck
	return 0
}

// probeLocalCity mirrors the local tiers of resolveContext for the current
// dry-run: explicit --city flag, explicit city env, GC_DIR, then cwd walk-up.
// It reports only whether a local city resolves and to which path; it never
// errors, since `gc context current` must not hard-fail outside a city.
func probeLocalCity() (string, bool) {
	if cityFlag != "" {
		if cp, err := resolveCityFlagValue(cityFlag); err == nil {
			return cp, true
		}
	}
	if cp, ok := resolveExplicitCityPathEnv(); ok {
		return cp, true
	}
	if cp, ok := resolveCityPathFromGCDir(); ok {
		return cp, true
	}
	return resolveCityPathFromCwd()
}

// formatRemoteTarget renders the one-line target echo used by `gc context
// current` (and, later, every remote invocation): the city, URL, context name,
// credential technique, and winning tier.
func formatRemoteTarget(t *remoteTarget) string {
	ctxName := "ad-hoc"
	cred := "none"
	if t.Ctx != nil {
		ctxName = t.Ctx.Name
		cred = credLabel(*t.Ctx)
	} else if t.Token != "" {
		cred = "token"
	}
	return fmt.Sprintf("target: %s @ %s (context: %s, cred: %s, source: %s)",
		t.CityName, t.BaseURL, ctxName, cred, t.Source)
}

// credLabel summarizes which credential technique a context configures.
func credLabel(c clientcontext.Context) string {
	switch {
	case c.GrantCommand != "":
		return "grant:" + c.GrantCommand
	case c.CredentialCommand != "":
		return "exec:" + c.CredentialCommand
	default:
		return "none"
	}
}

func contextToJSON(c clientcontext.Context, isDefault bool) contextJSON {
	return contextJSON{
		Name:              c.Name,
		URL:               c.URL,
		City:              c.EffectiveCity(),
		Default:           isDefault,
		CredentialCommand: c.CredentialCommand,
		GrantCommand:      c.GrantCommand,
	}
}
