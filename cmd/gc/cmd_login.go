package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/cliauth"
	"github.com/spf13/cobra"
)

// defaultServiceURL is the compiled-in default hosted Gas City service. Like the
// pack registry's default, it is configuration data — a flag default, not
// policy: `gc login --at <url>` targets any server that implements the Gas City
// Service Protocol v0 (docs/reference/specs/service-protocol-v0.md).
const defaultServiceURL = "https://gascity.com"

const (
	serviceURLEnv   = "GC_SERVICE_URL"
	serviceTokenEnv = "GC_SERVICE_TOKEN"
)

type loginOptions struct {
	ServiceURL string
	Token      string
	Label      string
	Device     bool
	NoBrowser  bool
	Timeout    time.Duration
}

func newLoginCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := loginOptions{
		Label:   defaultTokenLabel(),
		Timeout: 15 * time.Minute,
	}
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to a hosted Gas City service",
		Long: `Log in to a hosted Gas City service and store a local API token.

By default this targets ` + defaultServiceURL + `; pass --at <url> to log in to
any server that implements the Gas City Service Protocol v0. It opens a browser
to sign in; use --device for headless shells, or --token to store an existing
token. The token is stored per service under ~/.gc/credentials.json.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if doLogin(cmd.Context(), opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.ServiceURL, "at", "", "service base URL; defaults to "+serviceURLEnv+", the stored default, then "+defaultServiceURL)
	cmd.Flags().StringVar(&opts.Token, "token", "", "existing API token to store; defaults to "+serviceTokenEnv)
	cmd.Flags().StringVar(&opts.Label, "label", opts.Label, "label for the minted token")
	cmd.Flags().BoolVar(&opts.Device, "device", false, "use device-code login instead of browser callback login")
	cmd.Flags().BoolVar(&opts.NoBrowser, "no-browser", false, "print the browser login URL instead of opening it")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "maximum time to wait for interactive login")
	return cmd
}

func newWhoamiCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := loginOptions{
		Timeout: 30 * time.Second,
	}
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the authenticated hosted Gas City account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if doWhoami(cmd.Context(), opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.ServiceURL, "at", "", "service base URL; defaults to "+serviceURLEnv+", the stored default, then "+defaultServiceURL)
	cmd.Flags().StringVar(&opts.Token, "token", "", "API token to check; defaults to "+serviceTokenEnv+" or the stored login")
	return cmd
}

func doLogin(ctx context.Context, opts loginOptions, stdout, stderr io.Writer) int {
	store := cliauth.NewStore(cliauth.DefaultStorePath())
	baseURL, err := resolveServiceBaseURL(opts.ServiceURL, store)
	if err != nil {
		fmt.Fprintf(stderr, "gc login: %v\n", err) //nolint:errcheck
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Secrets resolve at execution time, never as flag defaults, so help output
	// cannot render credential values from the environment.
	token := strings.TrimSpace(registryFirstNonEmpty(opts.Token, os.Getenv(serviceTokenEnv)))
	client := cliauth.NewClient(baseURL, stdout)
	client.OpenBrowser = openURL
	if token == "" {
		token, err = client.Login(ctx, cliauth.LoginOptions{
			Label:     opts.Label,
			Device:    opts.Device,
			NoBrowser: opts.NoBrowser,
		})
		if err != nil {
			fmt.Fprintf(stderr, "gc login: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	user, err := client.Whoami(ctx, token)
	if err != nil {
		fmt.Fprintf(stderr, "gc login: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := store.SetToken(baseURL, token); err != nil {
		fmt.Fprintf(stderr, "gc login: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Logged in to %s as @%s\n", baseURL, user.Handle) //nolint:errcheck
	printServiceMessage(stdout, user)
	return 0
}

func doWhoami(ctx context.Context, opts loginOptions, stdout, stderr io.Writer) int {
	store := cliauth.NewStore(cliauth.DefaultStorePath())
	baseURL, err := resolveServiceBaseURL(opts.ServiceURL, store)
	if err != nil {
		fmt.Fprintf(stderr, "gc whoami: %v\n", err) //nolint:errcheck
		return 1
	}
	token := strings.TrimSpace(registryFirstNonEmpty(opts.Token, os.Getenv(serviceTokenEnv)))
	if token == "" {
		token, err = store.Token(baseURL)
		if err != nil {
			fmt.Fprintf(stderr, "gc whoami: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if token == "" {
		fmt.Fprintln(stderr, "gc whoami: not logged in; run `gc login`") //nolint:errcheck
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	user, err := cliauth.NewClient(baseURL, stdout).Whoami(ctx, token)
	if err != nil {
		fmt.Fprintf(stderr, "gc whoami: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "@%s (%s) at %s\n", user.Handle, user.ID, baseURL) //nolint:errcheck
	printServiceMessage(stdout, user)
	return 0
}

// printServiceMessage prints the server-authored message verbatim. The CLI
// never composes account/commercial copy itself; it only relays what the
// service sends (spec §5).
func printServiceMessage(stdout io.Writer, user cliauth.User) {
	if msg := strings.TrimSpace(user.Message); msg != "" {
		fmt.Fprintln(stdout, msg) //nolint:errcheck
	}
}

// resolveServiceBaseURL resolves the service base URL from the explicit --at
// flag, the GC_SERVICE_URL environment variable, the stored login default, then
// the compiled-in default, and normalizes the winner.
func resolveServiceBaseURL(explicit string, store *cliauth.Store) (string, error) {
	raw := registryFirstNonEmpty(explicit, os.Getenv(serviceURLEnv))
	if raw == "" {
		def, err := store.DefaultURL()
		if err != nil {
			return "", err
		}
		raw = def
	}
	return normalizeServiceBaseURL(raw)
}

func normalizeServiceBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultServiceURL
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid service URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid service URL %q: missing host", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func defaultTokenLabel() string {
	host, _ := os.Hostname()
	user := registryFirstNonEmpty(os.Getenv("USER"), os.Getenv("USERNAME"))
	switch {
	case user != "" && host != "":
		return user + "@" + host
	case host != "":
		return host
	default:
		return "gc CLI login"
	}
}
