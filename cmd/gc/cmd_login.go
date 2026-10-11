package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
	cmd.Flags().StringVar(&opts.Label, "label", "", "label for the minted token; defaults to <user>@<host>")
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

func newLogoutCmd(stdout, stderr io.Writer) *cobra.Command {
	var serviceURL string
	var all bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Log out of a hosted Gas City service (revoke the session and forget the token)",
		Long: `Log out of a hosted Gas City service: revoke the session server-side, then
remove the stored token. Because the session is the only long-lived credential,
this is the kill switch for a leaked ~/.gc/credentials.json — the local token is
always removed even if the server-side revoke fails or is not yet supported.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if doLogout(cmd.Context(), serviceURL, all, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serviceURL, "at", "", "service base URL; defaults to "+serviceURLEnv+", the stored default, then "+defaultServiceURL)
	cmd.Flags().BoolVar(&all, "all", false, "log out of every stored service")
	return cmd
}

func doLogout(ctx context.Context, serviceURL string, all bool, stdout, stderr io.Writer) int {
	store := cliauth.NewStore(cliauth.DefaultStorePath())
	var targets []string
	if all {
		svcs, err := store.Services()
		if err != nil {
			fmt.Fprintf(stderr, "gc logout: %v\n", err) //nolint:errcheck
			return 1
		}
		targets = svcs
	} else {
		base, err := resolveServiceBaseURL(serviceURL, store)
		if err != nil {
			fmt.Fprintf(stderr, "gc logout: %v\n", err) //nolint:errcheck
			return 1
		}
		targets = []string{base}
	}

	code := 0
	loggedOut := 0
	for _, base := range targets {
		token, err := store.Token(base)
		if err != nil {
			fmt.Fprintf(stderr, "gc logout: %v\n", err) //nolint:errcheck
			code = 1
			continue
		}
		if token == "" {
			continue
		}
		// Revoke server-side first (best-effort), then always remove locally.
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = cliauth.NewClient(base, stdout).Logout(ctx, token)
		cancel()
		switch {
		case err == nil:
			fmt.Fprintf(stdout, "Revoked session at %s\n", base) //nolint:errcheck
		case errors.Is(err, cliauth.ErrRevokeUnsupported):
			fmt.Fprintf(stderr, "gc logout: %s does not support server-side revocation yet; removed the local token only\n", base) //nolint:errcheck
		default:
			fmt.Fprintf(stderr, "gc logout: could not revoke at %s: %v — remove it from your account's session list to be safe\n", base, err) //nolint:errcheck
			code = 1
		}
		if err := store.Remove(base); err != nil {
			fmt.Fprintf(stderr, "gc logout: %v\n", err) //nolint:errcheck
			code = 1
			continue
		}
		loggedOut++
	}
	if loggedOut == 0 && code == 0 {
		fmt.Fprintln(stdout, "Not logged in to any service.") //nolint:errcheck
	}
	return code
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
		if looksLikeCI() {
			fmt.Fprintln(stderr, "gc login: this looks like CI — a human session is not a CI credential; use a machine principal for automation") //nolint:errcheck
		}
		token, err = client.Login(ctx, cliauth.LoginOptions{
			// Resolve the label at execution time so the --label flag default
			// stays empty and generated CLI docs never bake in this builder's
			// user/host (mirrors the token resolution above).
			Label:     loginLabelOrDefault(opts.Label),
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
		var authErr *cliauth.AuthError
		if errors.As(err, &authErr) && authErr.Unauthenticated() {
			fmt.Fprintln(stderr, "gc whoami: not logged in; run `gc login`") //nolint:errcheck
			return 1
		}
		fmt.Fprintf(stderr, "gc whoami: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "@%s (%s) at %s\n", user.Handle, user.ID, baseURL) //nolint:errcheck
	printSessionInfo(stdout, stderr, user.Session)
	printServiceMessage(stdout, user)
	return 0
}

// printSessionInfo shows display-only session metadata the server reported, and
// warns when the session is close to expiry. The client never parses the token.
func printSessionInfo(stdout, stderr io.Writer, s cliauth.SessionInfo) {
	if s.CreatedAt != "" {
		fmt.Fprintf(stdout, "  session created %s\n", s.CreatedAt) //nolint:errcheck
	}
	if s.LastUsed != "" {
		fmt.Fprintf(stdout, "  last used %s\n", s.LastUsed) //nolint:errcheck
	}
	if s.ExpiresAt == "" {
		return
	}
	fmt.Fprintf(stdout, "  expires %s\n", s.ExpiresAt) //nolint:errcheck
	if exp, err := time.Parse(time.RFC3339, s.ExpiresAt); err == nil {
		if d := time.Until(exp); d > 0 && d < 72*time.Hour {
			fmt.Fprintf(stderr, "  session expires in ~%s — run `gc login` to refresh\n", d.Round(time.Hour)) //nolint:errcheck
		}
	}
}

// looksLikeCI reports whether we appear to be running in CI/automation, where a
// human session is the wrong credential (machine principals should be used).
func looksLikeCI() bool {
	for _, k := range []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "CIRCLECI"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
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
	// The session token is sent as a bearer, so require https. Plain http is
	// allowed only against loopback (local development / tests).
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("service URL %q must use https; http is allowed only for localhost", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// loginLabelOrDefault resolves the token label at execution time. The --label
// flag defaults to empty so generated CLI docs stay host-independent; when the
// user supplies no label we fall back to the builder-independent
// defaultTokenLabel().
func loginLabelOrDefault(label string) string {
	if strings.TrimSpace(label) == "" {
		return defaultTokenLabel()
	}
	return label
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
