package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

// indefiniteHoldDuration is the canonical "suspended indefinitely" sentinel
// used when setting held_until on a session bead. The reconciler treats any
// held_until in the future as "do not wake." 100 years is effectively forever
// without risking time arithmetic overflow.
const indefiniteHoldDuration = 100 * 365 * 24 * time.Hour

func newSessionCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage interactive chat sessions",
		Long: `Create, resume, suspend, and close persistent conversations with agents.

Sessions are conversations backed by agent templates. They can be
suspended to free resources and resumed later with full conversation
continuity.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc session: missing subcommand (new, list, attach, submit, suspend, pin, unpin, reset, close, rename, prune, peek, kill, nudge, logs, wake, wait)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc session: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newSessionNewCmd(stdout, stderr),
		newSessionListCmd(stdout, stderr),
		newSessionAttachCmd(stdout, stderr),
		newSessionSubmitCmd(stdout, stderr),
		newSessionSuspendCmd(stdout, stderr),
		newSessionPinCmd(stdout, stderr),
		newSessionUnpinCmd(stdout, stderr),
		newSessionResetCmd(stdout, stderr),
		newSessionCloseCmd(stdout, stderr),
		newSessionRenameCmd(stdout, stderr),
		newSessionPruneCmd(stdout, stderr),
		newSessionPeekCmd(stdout, stderr),
		newSessionKillCmd(stdout, stderr),
		newSessionNudgeCmd(stdout, stderr),
		newSessionLogsCmd(stdout, stderr),
		newSessionWakeCmd(stdout, stderr),
		newSessionWaitCmd(stdout, stderr),
	)
	return cmd
}

func newSessionSubmitCmd(stdout, stderr io.Writer) *cobra.Command {
	var intent string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "submit <id-or-alias> <message...>",
		Short: "Submit a message with semantic delivery intent",
		Long: `Submit a user message to a session without choosing provider transport details.

The runtime decides whether to wake, inject immediately, or queue the message
according to the selected semantic intent.`,
		Example: `  gc session submit mayor "status update"
  gc session submit mayor "after this run, handle docs" --intent follow_up
  gc session submit mayor "stop and do this instead" --intent interrupt_now`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			parsedIntent, err := parseSessionSubmitIntent(intent)
			if err != nil {
				fmt.Fprintf(stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdSessionSubmit(args, parsedIntent, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().StringVar(&intent, "intent", string(session.SubmitIntentDefault), "submit intent: default, follow_up, or interrupt_now")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	return cmd
}

// newSessionNewCmd creates the "gc session new <template>" command.
func newSessionNewCmd(stdout, stderr io.Writer) *cobra.Command {
	var title string
	var alias string
	var titleHint string
	var noAttach bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "new <template>",
		Short: "Create a new chat session from an agent template",
		Long: `Create a new persistent conversation from an agent template defined
in the loaded city configuration. By default, attaches the terminal
after creation.

When --title-hint is provided without --title, the session title is
auto-generated from the hint text: a short version is set immediately
and refined by the title model in the background.

If the template config sets tmux_alias, it controls the runtime tmux
session_name. --alias still sets the public command and mail alias.`,
		Example: `  gc session new helper
  gc session new helper --alias sky
  gc session new helper --title "debugging auth"
  gc session new helper --title-hint "fix the login redirect loop"
  gc session new helper --no-attach`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionNew(args, alias, title, titleHint, noAttach, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "human-friendly session identifier for commands and mail")
	cmd.Flags().StringVar(&title, "title", "", "human-readable session title")
	cmd.Flags().StringVar(&titleHint, "title-hint", "", "text to auto-generate a session title from")
	cmd.Flags().BoolVar(&noAttach, "no-attach", false, "create session without attaching")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	return cmd
}

// cmdSessionNew is the CLI entry point for "gc session new".
//
// Phase 2: creates a session bead and pokes the controller. The reconciler
// handles process lifecycle (start). If the controller is not running,
// falls back to direct process start via the session manager.
func cmdSessionNew(args []string, alias, title, titleHint string, noAttach, jsonOutput bool, stdout, stderr io.Writer) int {
	templateName := args[0]
	if jsonOutput && !noAttach {
		fmt.Fprintln(stderr, "gc session new: --json requires --no-attach because attaching is interactive") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Find the template agent. Session creation targets configured templates,
	// not concrete pool member names like worker-2.
	found, ok := resolveSessionTemplate(cfg, templateName, currentRigContext(cfg))
	if !ok {
		fmt.Fprintln(stderr, agentNotFoundMsg("gc session new", templateName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Resolve the provider.
	resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sessionTransport := config.ResolveSessionCreateTransport(found.Session, resolved)
	requestedAlias, err := session.ValidateAlias(alias)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	alias = requestedAlias
	if alias != "" && found.SupportsMultipleSessions() {
		alias = workdirutil.SessionQualifiedName(cityPath, found, cfg.Rigs, requestedAlias, "")
	}
	cityName := loadedCityName(cfg, cityPath)
	explicitName, err := sessionExplicitNameForNewSession(cityPath, cityName, cfg.Rigs, &found, alias)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Open the bead store.
	store, code := openCityStore(stderr, "gc session new")
	if store == nil {
		return code
	}

	sp := newSessionProvider()
	if err := validateResolvedSessionTransport(resolved, sessionTransport, sp); err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Build the work directory.
	sessionQualifiedName := workdirutil.SessionQualifiedName(cityPath, found, cfg.Rigs, requestedAlias, explicitName)
	workDir, err := resolveWorkDirForQualifiedName(
		cityPath,
		cfg,
		&found,
		sessionQualifiedName,
	)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Store the canonical qualified name so the reconciler can match it
	// via findAgentByTemplate (which compares against QualifiedName()).
	canonicalTemplate := found.QualifiedName()
	configuredOwner := sessionNewAliasOwner(cfg, &found)
	reservationIDs := []string{alias, explicitName}
	reserveConcreteIdentity := found.SupportsMultipleSessions() && strings.TrimSpace(sessionQualifiedName) != ""
	if reserveConcreteIdentity {
		reservationIDs = append(reservationIDs, sessionQualifiedName)
	}

	// Resolve the workspace default provider for title generation. This
	// mirrors api.Server.resolveTitleProvider: use an empty Agent so we
	// get workspace-level title model settings, not the agent's own provider.
	titleProvider, err := config.ResolveProvider(&config.Agent{}, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		titleProvider = nil
	}
	sessionCommand, err := resolvedSessionCommand(cityPath, resolved, nil, sessionTransport)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Try reconciler-first path only when this specific city is managed by a
	// standalone controller or the machine-wide supervisor. A reachable
	// supervisor socket alone is not enough for unmanaged ad-hoc cities.
	if cityUsesManagedReconciler(cityPath) {
		if pokeErr := pokeController(cityPath); pokeErr == nil {
			// Controller is running — create bead only, let reconciler start it.
			kindMeta := map[string]string{
				"agent_name":     sessionQualifiedName,
				"session_origin": "manual",
			}
			if family := resolvedProviderFamilyMetadata(resolved); family != "" {
				kindMeta["provider_kind"] = family
			}
			if resolved.BuiltinAncestor != "" && resolved.BuiltinAncestor != resolved.Name {
				kindMeta["builtin_ancestor"] = resolved.BuiltinAncestor
			}
			kindMeta, err = newSessionStoredMCPMetadata(
				cityPath,
				cfg,
				alias,
				canonicalTemplate,
				resolved.Name,
				workDir,
				sessionTransport,
				kindMeta,
			)
			if err != nil {
				fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			handle, err := newWorkerSessionHandleForResolvedRuntimeWithConfig(
				cityPath,
				store,
				sp,
				cfg,
				alias,
				explicitName,
				canonicalTemplate,
				title,
				sessionCommand,
				found.Provider,
				workDir,
				sessionTransport,
				resolved,
				kindMeta,
			)
			if err != nil {
				fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			var info session.Info
			err = session.WithCitySessionIdentifierLocks(cityPath, reservationIDs, func() error {
				if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, alias, "", configuredOwner); err != nil {
					return err
				}
				if reserveConcreteIdentity && sessionQualifiedName != alias {
					if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, sessionQualifiedName, "", configuredOwner); err != nil {
						return err
					}
				}
				if err := session.EnsureSessionNameAvailableWithConfig(store, cfg, explicitName, ""); err != nil {
					return err
				}
				var createErr error
				info, createErr = handle.Create(context.Background(), worker.CreateModeDeferred)
				return createErr
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}

			titleDone := maybeAutoTitle(store, info.ID, title, titleHint, titleProvider, info.WorkDir, stderr)
			defer func() { <-titleDone }() // ensure title goroutine completes on all exit paths

			// Poke again after bead creation to trigger immediate reconciler tick.
			_ = pokeController(cityPath)

			if jsonOutput {
				if err := writeSessionNewJSON(stdout, stderr, sessionNewJSON{
					SchemaVersion: "1",
					OK:            true,
					SessionID:     info.ID,
					SessionName:   info.SessionName,
					Alias:         info.Alias,
					Template:      canonicalTemplate,
					Transport:     sessionTransport,
					WorkDir:       info.WorkDir,
					DeferredStart: true,
					Attached:      false,
				}); err != nil {
					return 1
				}
			} else {
				fmt.Fprintf(stdout, "Session %s created from template %q (reconciler will start it).\n", info.ID, canonicalTemplate) //nolint:errcheck // best-effort stdout
			}

			if !shouldAttachNewSession(noAttach, sessionTransport) {
				if sessionTransport == config.SessionTransportACP && !noAttach && !jsonOutput {
					fmt.Fprintln(stdout, "Session uses ACP transport; not attaching.") //nolint:errcheck // best-effort stdout
				}
				return 0
			}

			// Wait for the reconciler to start the session before attaching.
			fmt.Fprintln(stdout, "Waiting for session to start...") //nolint:errcheck // best-effort stdout
			if waitErr := waitForSession(sp, info.SessionName, 30*time.Second, store, info.ID, stderr); waitErr != nil {
				fmt.Fprintf(stderr, "gc session new: %v\n", waitErr) //nolint:errcheck // best-effort stderr
				return 1
			}
			fmt.Fprintln(stdout, "Attaching...") //nolint:errcheck // best-effort stdout
			if err := handle.Attach(context.Background()); err != nil {
				fmt.Fprintf(stderr, "gc session new: attaching: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			return 0
		}
	}

	// Fallback: controller not running — direct start via session manager.
	kindMeta := map[string]string{
		"agent_name":     sessionQualifiedName,
		"session_origin": "manual",
	}
	if family := resolvedProviderFamilyMetadata(resolved); family != "" {
		kindMeta["provider_kind"] = family
	}
	if resolved.BuiltinAncestor != "" && resolved.BuiltinAncestor != resolved.Name {
		kindMeta["builtin_ancestor"] = resolved.BuiltinAncestor
	}
	kindMeta, err = newSessionStoredMCPMetadata(
		cityPath,
		cfg,
		alias,
		canonicalTemplate,
		resolved.Name,
		workDir,
		sessionTransport,
		kindMeta,
	)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	handle, err := newWorkerSessionHandleForResolvedRuntimeWithConfig(
		cityPath,
		store,
		sp,
		cfg,
		alias,
		explicitName,
		canonicalTemplate,
		title,
		sessionCommand,
		found.Provider,
		workDir,
		sessionTransport,
		resolved,
		kindMeta,
	)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	var info session.Info
	err = session.WithCitySessionIdentifierLocks(cityPath, reservationIDs, func() error {
		if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, alias, "", configuredOwner); err != nil {
			return err
		}
		if reserveConcreteIdentity && sessionQualifiedName != alias {
			if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, sessionQualifiedName, "", configuredOwner); err != nil {
				return err
			}
		}
		if err := session.EnsureSessionNameAvailableWithConfig(store, cfg, explicitName, ""); err != nil {
			return err
		}
		var createErr error
		info, createErr = handle.Create(context.Background(), worker.CreateModeStarted)
		return createErr
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	titleDone := maybeAutoTitle(store, info.ID, title, titleHint, titleProvider, info.WorkDir, stderr)
	defer func() { <-titleDone }() // ensure title goroutine completes on all exit paths

	if jsonOutput {
		if err := writeSessionNewJSON(stdout, stderr, sessionNewJSON{
			SchemaVersion: "1",
			OK:            true,
			SessionID:     info.ID,
			SessionName:   info.SessionName,
			Alias:         info.Alias,
			Template:      canonicalTemplate,
			Transport:     sessionTransport,
			WorkDir:       info.WorkDir,
			DeferredStart: false,
			Attached:      false,
		}); err != nil {
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "Session %s created from template %q.\n", info.ID, canonicalTemplate) //nolint:errcheck // best-effort stdout
	}

	if !shouldAttachNewSession(noAttach, sessionTransport) {
		if sessionTransport == config.SessionTransportACP && !noAttach && !jsonOutput {
			fmt.Fprintln(stdout, "Session uses ACP transport; not attaching.") //nolint:errcheck // best-effort stdout
		}
		return 0
	}

	fmt.Fprintln(stdout, "Attaching...") //nolint:errcheck // best-effort stdout
	if err := handle.Attach(context.Background()); err != nil {
		fmt.Fprintf(stderr, "gc session new: attaching: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func newSessionStoredMCPMetadata(
	cityPath string,
	cfg *config.City,
	alias, template, provider, workDir, transport string,
	metadata map[string]string,
) (map[string]string, error) {
	if strings.TrimSpace(transport) != config.SessionTransportACP {
		return metadata, nil
	}
	mcpServers, err := resolvedRuntimeMCPServersWithConfig(
		cityPath,
		cfg,
		alias,
		template,
		provider,
		workDir,
		transport,
		metadata,
	)
	if err != nil {
		return nil, err
	}
	return session.WithStoredMCPMetadata(
		metadata,
		firstNonEmptyGCString(metadata[session.MCPIdentityMetadataKey], metadata["agent_name"]),
		mcpServers,
	)
}

// maybeAutoTitle runs the auto-title flow for a newly created session.
// The provider should already be resolved by the caller. It returns a
// channel that is closed when background title generation completes.
// Short-lived CLI paths (e.g. --no-attach) should block on it before
// exiting to ensure the model-refined title is persisted.
func maybeAutoTitle(store beads.Store, beadID, userTitle, titleHint string, provider *config.ResolvedProvider, workDir string, stderr io.Writer) <-chan struct{} {
	return api.MaybeGenerateTitleAsync(store, beadID, userTitle, titleHint, provider, workDir, func(format string, args ...any) {
		fmt.Fprintf(stderr, "session %s: "+format+"\n", append([]any{beadID}, args...)...) //nolint:errcheck // best-effort stderr
	})
}

type acpRouteRegistrar interface {
	RouteACP(name string)
}

func validateResolvedSessionTransport(resolved *config.ResolvedProvider, transport string, sp runtime.Provider) error {
	transport = strings.TrimSpace(transport)
	switch transport {
	case "":
		return nil
	case config.SessionTransportTmux:
		if sessionProviderSupportsTmux(sp) {
			return nil
		}
		providerName := transport
		if resolved != nil && resolved.Name != "" {
			providerName = resolved.Name
		}
		return fmt.Errorf("provider %q requires tmux transport but the session provider cannot route tmux sessions", providerName)
	case config.SessionTransportACP:
	default:
		return fmt.Errorf("unknown session transport %q", transport)
	}
	providerName := ""
	if resolved != nil {
		providerName = resolved.Name
		if !resolved.SupportsACP {
			if providerName == "" {
				providerName = transport
			}
			return fmt.Errorf("provider %q does not support ACP transport", providerName)
		}
	}
	if sessionProviderSupportsACP(sp) {
		return nil
	}
	if providerName == "" {
		providerName = transport
	}
	return fmt.Errorf("provider %q requires ACP transport but the session provider cannot route ACP sessions", providerName)
}

func sessionProviderSupportsACP(sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	if provider, ok := sp.(runtime.TransportCapabilityProvider); ok {
		return provider.SupportsTransport(config.SessionTransportACP)
	}
	if _, ok := sp.(acpRouteRegistrar); ok {
		return true
	}
	return false
}

func sessionProviderSupportsTmux(sp runtime.Provider) bool {
	if provider, ok := sp.(runtime.TransportCapabilityProvider); ok {
		return provider.SupportsTransport(config.SessionTransportTmux)
	}
	return true
}

func resolvedSessionCommand(cityPath string, resolved *config.ResolvedProvider, optionOverrides map[string]string, transport string) (string, error) {
	if resolved == nil {
		return "", fmt.Errorf("resolved provider is nil")
	}
	launchCommand, err := config.BuildProviderLaunchCommand(cityPath, resolved, optionOverrides, transport)
	if err != nil {
		return "", fmt.Errorf("resolving provider launch command: %w", err)
	}
	return launchCommand.Command, nil
}

func resolveSessionTemplate(cfg *config.City, input, currentRigDir string) (config.Agent, bool) {
	input = normalizeNamedSessionTarget(input)
	if strings.HasPrefix(input, templateTargetPrefix) {
		input = normalizeNamedSessionTarget(strings.TrimPrefix(input, templateTargetPrefix))
	}
	if cfg == nil || input == "" {
		return config.Agent{}, false
	}
	if currentRigDir != "" && !strings.Contains(input, "/") && strings.Contains(input, ".") {
		for _, a := range cfg.Agents {
			if a.QualifiedName() == currentRigDir+"/"+input {
				return a, true
			}
		}
	}

	// Inputs that include a rig separator ("/") or a binding prefix (".")
	// must match a qualified name exactly after any current-rig lookup.
	if strings.ContainsAny(input, "/.") {
		for _, a := range cfg.Agents {
			if a.QualifiedName() == input {
				return a, true
			}
		}
		return config.Agent{}, false
	}

	var matches []config.Agent
	for _, a := range cfg.Agents {
		if a.Name != input {
			continue
		}
		if a.Dir == "" || (currentRigDir != "" && a.Dir == currentRigDir) {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		for _, a := range cfg.Agents {
			if a.QualifiedName() == matches[0].QualifiedName() {
				return a, true
			}
		}
	}
	return config.Agent{}, false
}

func sessionNewAliasOwner(cfg *config.City, agent *config.Agent) string {
	if cfg == nil || agent == nil {
		return ""
	}
	for i := range cfg.NamedSessions {
		named := &cfg.NamedSessions[i]
		if named.TemplateQualifiedName() == agent.QualifiedName() && named.IdentityName() == agent.Name {
			return named.QualifiedName()
		}
	}
	return ""
}

// waitForSession polls the provider until the session is running or timeout.
// If a bead store is provided, it checks for early failure (bead transitioned
// to "closed" state) and logs progress every 5 seconds.
func waitForSession(sp runtime.Provider, sessionName string, timeout time.Duration, store beads.Store, beadID string, stderr io.Writer) error {
	deadline := time.Now().Add(timeout)
	lastProgress := time.Now()
	for time.Now().Before(deadline) {
		if sp.IsRunning(sessionName) {
			return nil
		}
		// Check for early failure: bead closed or stuck in creating.
		if store != nil && beadID != "" {
			if b, err := store.Get(beadID); err == nil {
				if b.Status == "closed" {
					return fmt.Errorf("session %q failed to start (bead %s closed)", sessionName, beadID)
				}
			}
		}
		// Log progress every 5 seconds.
		if time.Since(lastProgress) >= 5*time.Second {
			fmt.Fprintf(stderr, "  still waiting for session %q...\n", sessionName) //nolint:errcheck
			lastProgress = time.Now()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("session %q did not start within %s (bead %s may be stuck in creating state — check controller logs)", sessionName, timeout, beadID)
}

// newSessionListCmd creates the "gc session list" command.
func newSessionListCmd(stdout, stderr io.Writer) *cobra.Command {
	var stateFilter string
	var templateFilter string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List chat sessions",
		Long:  `List all chat sessions. By default shows active and suspended sessions.`,
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdSessionList(stateFilter, templateFilter, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stateFilter, "state", "", `filter by state: "active", "suspended", "closed", "all"`)
	cmd.Flags().StringVar(&templateFilter, "template", "", "filter by template name")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	return cmd
}

// sessionListAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests inject a client pointed at httptest.Server or force a
// specific fallback reason without spinning up a real controller.
var sessionListAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeSessionList dispatches `session list` to the supervisor API when a
// controller is up; otherwise falls back to the local iterator. Emits
// exactly one route=... log line per exit path (gated on GC_DEBUG).
func routeSessionList(_ string, stateFilter, templateFilter string, c *api.Client, nilReason string, jsonOutput bool, stdout, stderr io.Writer) int {
	const cmdName = "session list"
	if c != nil {
		cr, err := c.ListSessions(stateFilter, templateFilter, false)
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderSessionListFromAPI(cr, jsonOutput, stdout)
		}
		if !api.ShouldFallbackForRead(err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc session list: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doSessionListFallback(stateFilter, templateFilter, jsonOutput, stdout, stderr)
}

// sessionListJSONEnvelope is the API-path --json output shape for
// `gc session list`. It wraps the items array so _cache_age_s can sit
// alongside at the envelope level — the shape documented in the
// ga-h6w designer's D5 contract.
type sessionListJSONEnvelope struct {
	CacheAgeS float64       `json:"_cache_age_s"`
	Sessions  []SessionView `json:"sessions"`
}

// SessionView mirrors api.SessionView for CLI JSON output. Defined as an
// alias so cmd/gc/ can document the JSON shape without exposing genclient.
type SessionView = api.SessionView

// renderSessionListFromAPI formats the API-sourced session list. On --json
// the output is the sessionListJSONEnvelope with _cache_age_s; human output
// mirrors the fallback tabwriter format and appends a staleness banner when
// the supervisor cache age crosses the threshold.
func renderSessionListFromAPI(cr api.CachedRead[[]SessionView], jsonOutput bool, stdout io.Writer) int {
	if jsonOutput {
		env := sessionListJSONEnvelope{
			CacheAgeS: cr.AgeSeconds,
			Sessions:  cr.Body,
		}
		if env.Sessions == nil {
			env.Sessions = []SessionView{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(env) //nolint:errcheck // best-effort stdout
		return 0
	}

	if len(cr.Body) == 0 {
		fmt.Fprintln(stdout, "No sessions found.") //nolint:errcheck // best-effort stdout
		return 0
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTEMPLATE\tSTATE\tREASON\tTARGET\tTITLE\tAGE\tLAST ACTIVE") //nolint:errcheck // best-effort stdout
	for _, s := range cr.Body {
		state := s.State
		if state == "" {
			state = "closed"
		}
		reason := s.Reason
		if reason == "" {
			reason = "-"
		}
		target := sessionViewTarget(s)
		title := sessionViewTitle(s)
		age := sessionViewAge(s.CreatedAt)
		lastActive := sessionViewLastActive(s.LastActive)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Template, state, reason, target, title, age, lastActive) //nolint:errcheck // best-effort stdout
	}
	_ = w.Flush() //nolint:errcheck // best-effort stdout

	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck
	}
	return 0
}

// sessionViewTarget mirrors sessionListTarget's fallback behavior, but
// against the SessionView shape the API returns.
func sessionViewTarget(s SessionView) string {
	if s.Alias != "" {
		return s.Alias
	}
	if s.SessionName != "" {
		return s.SessionName
	}
	return "-"
}

// sessionViewTitle mirrors sessionListTitle against the API-returned shape.
func sessionViewTitle(s SessionView) string {
	title := s.Title
	if title == "" {
		return "-"
	}
	if len(title) > 30 {
		return title[:27] + "..."
	}
	return title
}

// sessionViewAge formats a CreatedAt RFC3339 string the same way the
// fallback formats time.Since(s.CreatedAt). Empty or unparseable strings
// render as "-".
func sessionViewAge(createdAt string) string {
	if createdAt == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return "-"
	}
	return formatDuration(time.Since(t))
}

// sessionViewLastActive formats a LastActive RFC3339 string the same way
// the fallback formats time.Since. Empty or unparseable strings render as
// "-".
func sessionViewLastActive(lastActive string) string {
	if lastActive == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, lastActive)
	if err != nil {
		return "-"
	}
	return formatDuration(time.Since(t)) + " ago"
}

// cmdSessionList is the CLI entry point for "gc session list". It routes
// through the supervisor API when a controller is up and falls back to the
// local iterator otherwise.
func cmdSessionList(stateFilter, templateFilter string, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	c, reason := sessionListAPIClient(cityPath)
	return routeSessionList(cityPath, stateFilter, templateFilter, c, reason, jsonOutput, stdout, stderr)
}

// doSessionListFallback is the direct-bd path for "gc session list".
func doSessionListFallback(stateFilter, templateFilter string, jsonOutput bool, stdout, stderr io.Writer) int {
	storeStderr := stderr
	if jsonOutput {
		storeStderr = io.Discard
	}
	store, code := openCityStore(storeStderr, "gc session list")
	if store == nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "store_open_failed", "gc session list: opening bead store failed", code)
		}
		return code
	}

	providerCtx := loadSessionProviderContext()

	// Launch readyWaitSet concurrently with the shared session-bead load,
	// but only on the non-JSON path — JSON output returns early and doesn't
	// need wait-state computation.
	type waitResult struct {
		set map[string]bool
		err error
	}
	var waitCh chan waitResult

	if !jsonOutput {
		waitCh = make(chan waitResult, 1)

		go func() {
			set, err := readyWaitSetForList(store)
			waitCh <- waitResult{set: set, err: err}
		}()
	}

	allSessionBeads, err := session.ListAllSessionBeads(store, beads.ListQuery{
		Sort: beads.SortCreatedDesc,
	})
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "session_list_failed", fmt.Sprintf("gc session list: listing sessions: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc session list: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sessionBeads := newSessionBeadSnapshot(allSessionBeads)
	sp := newSessionProviderFromContext(providerCtx, sessionBeads)
	catalog, err := workerSessionCatalogWithConfig("", store, sp, providerCtx.cfg)
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "session_catalog_failed", fmt.Sprintf("gc session list: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc session list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	listResult := catalog.ListFullFromBeads(allSessionBeads, stateFilter, templateFilter)
	sessions := listResult.Sessions

	if jsonOutput {
		return writeSessionListJSON(sessions, stateFilter, templateFilter, stdout, stderr)
	}

	// Build bead index from the beads already fetched by ListFull (no duplicate query).
	beadIndex := make(map[string]beads.Bead, len(listResult.Beads))
	for _, b := range listResult.Beads {
		beadIndex[b.ID] = b
	}

	waitRes := <-waitCh
	if waitRes.err != nil {
		fmt.Fprintf(stderr, "gc session list: ready wait indicators degraded: %v\n", waitRes.err) //nolint:errcheck // best-effort stderr
	}
	readyWaitSet := waitRes.set
	cfg := providerCtx.cfg
	poolDesired := cliPoolDesired(cfg)

	// Build attachment cache. Active sessions already have Info.Attached
	// populated by ListFullFromBeads; for inactive sessions, query the
	// provider directly while preserving the old "running and attached"
	// semantics. Going through workerSessionTargetAttachedWithConfig here
	// triggered 2-3 extra bd show subprocess lookups per session.
	attachedSet := buildAttachmentCache(sessions, func(info session.Info) (bool, error) {
		if info.State == session.StateActive || sp == nil {
			return info.Attached, nil
		}
		return sessionAttachedForWakeReason(sp, info.SessionName), nil
	})

	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No sessions found.") //nolint:errcheck // best-effort stdout
		return 0
	}

	// Wrap sp with an attachment cache to avoid redundant IsAttached calls
	// in wakeReasons.
	cachedSP := &attachmentCachingProvider{Provider: sp, cache: attachedSet}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTEMPLATE\tSTATE\tREASON\tTARGET\tTITLE\tAGE\tLAST ACTIVE\tLAST NUDGE") //nolint:errcheck // best-effort stdout
	for _, s := range sessions {
		state := string(s.State)
		if s.State == "" {
			state = "closed"
		}
		reason := sessionReason(s, beadIndex, cfg, cachedSP, poolDesired, readyWaitSet)
		target := sessionListTarget(s)
		title := sessionListTitle(s)
		age := formatDuration(time.Since(s.CreatedAt))
		lastActive := "-"
		if !s.LastActive.IsZero() {
			lastActive = formatDuration(time.Since(s.LastActive)) + " ago"
		}
		lastNudge := "-"
		if !s.LastNudgeDeliveredAt.IsZero() {
			lastNudge = formatDuration(time.Since(s.LastNudgeDeliveredAt)) + " ago"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Template, state, reason, target, title, age, lastActive, lastNudge) //nolint:errcheck // best-effort stdout
	}
	_ = w.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

type sessionListJSONRow struct {
	ID                   string        `json:"id"`
	Name                 string        `json:"name,omitempty"`
	Template             string        `json:"template"`
	Provider             string        `json:"provider,omitempty"`
	State                session.State `json:"state"`
	Title                string        `json:"title,omitempty"`
	Rig                  string        `json:"rig,omitempty"`
	Alias                string        `json:"alias,omitempty"`
	AgentName            string        `json:"agent_name,omitempty"`
	Transport            string        `json:"transport,omitempty"`
	Command              string        `json:"command,omitempty"`
	WorkDir              string        `json:"work_dir,omitempty"`
	SessionName          string        `json:"session_name,omitempty"`
	SessionKey           string        `json:"session_key,omitempty"`
	ResumeFlag           string        `json:"resume_flag,omitempty"`
	ResumeStyle          string        `json:"resume_style,omitempty"`
	ResumeCommand        string        `json:"resume_command,omitempty"`
	CreatedAt            time.Time     `json:"created_at"`
	LastActive           time.Time     `json:"last_active"`
	LastNudgeDeliveredAt *time.Time    `json:"last_nudge_delivered_at,omitempty"`
	Attached             bool          `json:"attached"`
	Closed               bool          `json:"closed"`
}

type sessionListJSON struct {
	SchemaVersion string               `json:"schema_version"`
	Filters       sessionListFilters   `json:"filters"`
	Sessions      []sessionListJSONRow `json:"sessions"`
	Summary       sessionListSummary   `json:"summary"`
}

type sessionListFilters struct {
	State    string `json:"state,omitempty"`
	Template string `json:"template,omitempty"`
}

type sessionListSummary struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Suspended int `json:"suspended"`
	Closed    int `json:"closed"`
}

func writeSessionListJSON(sessions []session.Info, stateFilter, templateFilter string, stdout, stderr io.Writer) int {
	rows := sessionListJSONRows(sessions)
	result := sessionListJSON{
		SchemaVersion: "1",
		Filters:       sessionListFilters{State: stateFilter, Template: templateFilter},
		Sessions:      rows,
		Summary:       summarizeSessionList(rows),
	}
	if err := writeCLIJSONLine(stdout, result); err != nil {
		fmt.Fprintf(stderr, "gc session list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func summarizeSessionList(rows []sessionListJSONRow) sessionListSummary {
	summary := sessionListSummary{Total: len(rows)}
	for _, row := range rows {
		switch {
		case row.Closed:
			summary.Closed++
		case row.State == session.StateActive:
			summary.Active++
		case row.State == session.StateSuspended:
			summary.Suspended++
		}
	}
	return summary
}

type sessionNewJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	SessionID     string `json:"session_id"`
	SessionName   string `json:"session_name"`
	Alias         string `json:"alias,omitempty"`
	Template      string `json:"template"`
	Transport     string `json:"transport"`
	WorkDir       string `json:"work_dir"`
	DeferredStart bool   `json:"deferred_start"`
	Attached      bool   `json:"attached"`
}

func writeSessionNewJSON(stdout, stderr io.Writer, result sessionNewJSON) error {
	return writeCLIJSONLineOrErr(stdout, stderr, "gc session new", result)
}

func sessionListJSONRows(sessions []session.Info) []sessionListJSONRow {
	rows := make([]sessionListJSONRow, len(sessions))
	for i, s := range sessions {
		rows[i] = sessionListJSONRow{
			ID:            s.ID,
			Name:          sessionListJSONName(s),
			Template:      s.Template,
			State:         s.State,
			Closed:        s.Closed,
			Title:         s.Title,
			Rig:           sessionListJSONRig(s),
			Alias:         s.Alias,
			AgentName:     s.AgentName,
			Provider:      s.Provider,
			Transport:     s.Transport,
			Command:       s.Command,
			WorkDir:       s.WorkDir,
			SessionName:   s.SessionName,
			SessionKey:    s.SessionKey,
			ResumeFlag:    s.ResumeFlag,
			ResumeStyle:   s.ResumeStyle,
			ResumeCommand: s.ResumeCommand,
			CreatedAt:     s.CreatedAt,
			LastActive:    s.LastActive,
			Attached:      s.Attached,
		}
		if !s.LastNudgeDeliveredAt.IsZero() {
			stamp := s.LastNudgeDeliveredAt.UTC()
			rows[i].LastNudgeDeliveredAt = &stamp
		}
	}
	return rows
}

func sessionListJSONName(s session.Info) string {
	if s.Alias != "" {
		return s.Alias
	}
	if s.SessionName != "" {
		return s.SessionName
	}
	return s.ID
}

func sessionListJSONRig(s session.Info) string {
	template := strings.TrimSpace(s.Template)
	if before, _, ok := strings.Cut(template, "/"); ok {
		return before
	}
	name := strings.TrimSpace(s.SessionName)
	if before, _, ok := strings.Cut(name, "--"); ok {
		return before
	}
	return ""
}

func sessionListTarget(s session.Info) string {
	if s.Alias != "" {
		return s.Alias
	}
	if s.SessionName != "" {
		return s.SessionName
	}
	return "-"
}

func sessionListTitle(s session.Info) string {
	title := s.Title
	if title == "" {
		title = "-"
	}
	if len(title) > 30 {
		title = title[:27] + "..."
	}
	return title
}

// attachmentCachingProvider wraps a runtime.Provider and caches IsAttached
// results to avoid redundant tmux subprocess calls. wakeReasons calls
// IsAttached per session, but cmdSessionList already queried it.
type attachmentCachingProvider struct {
	runtime.Provider
	cache map[string]bool
}

func (p *attachmentCachingProvider) GetMeta(name, key string) (string, error) {
	if p.Provider == nil {
		return "", nil
	}
	return p.Provider.GetMeta(name, key)
}

func (p *attachmentCachingProvider) IsAttached(name string) bool {
	if v, ok := p.cache[name]; ok {
		return v
	}
	if p.Provider == nil {
		return false
	}
	return p.Provider.IsAttached(name)
}

func (p *attachmentCachingProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	if scp, ok := p.Provider.(runtime.SleepCapabilityProvider); ok {
		return scp.SleepCapability(name)
	}
	return runtime.SessionSleepCapabilityDisabled
}

func (p *attachmentCachingProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	if ip, ok := p.Provider.(runtime.InteractionProvider); ok {
		return ip.Pending(name)
	}
	return nil, runtime.ErrInteractionUnsupported
}

func (p *attachmentCachingProvider) Respond(name string, response runtime.InteractionResponse) error {
	if ip, ok := p.Provider.(runtime.InteractionProvider); ok {
		return ip.Respond(name, response)
	}
	return runtime.ErrInteractionUnsupported
}

func sessionAttachedForWakeReason(sp runtime.Provider, name string) bool {
	if sp == nil || strings.TrimSpace(name) == "" {
		return false
	}
	if !sp.IsAttached(name) {
		return false
	}
	// attachmentCachingProvider caches the already-vetted attachment state for
	// the list path, so re-checking IsRunning here would just add the extra
	// tmux probe this shortcut was introduced to avoid.
	if _, ok := sp.(*attachmentCachingProvider); ok {
		return true
	}
	return sp.IsRunning(name)
}

func buildAttachmentCache(sessions []session.Info, observe ...func(session.Info) (bool, error)) map[string]bool {
	cache := make(map[string]bool)
	var observeFn func(session.Info) (bool, error)
	if len(observe) > 0 {
		observeFn = observe[0]
	}
	for _, s := range sessions {
		if s.State == "" || s.SessionName == "" {
			continue
		}
		attached := s.Attached
		if observeFn != nil {
			if observed, err := observeFn(s); err == nil {
				attached = observed
			}
		}
		cache[s.SessionName] = attached
	}
	return cache
}

const (
	resetPendingReason = "reset-pending"
	circuitOpenReason  = "circuit-open"
)

// sessionReason computes the REASON column for a session in gc session list.
// For awake sessions, shows wake reasons (e.g., "config", "attached").
// For asleep sessions, shows the sleep reason (e.g., "user-hold", "quarantine").
// For closed sessions, shows "-".
func sessionReason(s session.Info, beadIndex map[string]beads.Bead, cfg *config.City, sp runtime.Provider, poolDesired map[string]int, readyWaitSet map[string]bool) string {
	if s.State == "" {
		return "-" // closed
	}

	b, ok := beadIndex[s.ID]
	if !ok {
		return "-" // no bead data available
	}

	now := time.Now().UTC()
	lifecycle := session.ProjectLifecycle(session.LifecycleInput{
		Status:   b.Status,
		Metadata: b.Metadata,
		Now:      now,
	})
	if lifecycle.BaseState == session.BaseStateArchived && !lifecycle.ContinuityEligible {
		return "-"
	}
	if resetPendingReasonVisible(s, b, sp, now) {
		return resetPendingReason
	}
	if circuitOpenReasonVisible(b) {
		return circuitOpenReason
	}

	if reason := session.LifecycleDisplayReason(b.Status, b.Metadata, now); reason != "" {
		return reason
	}

	// If config is available and no lifecycle reason blocks display, compute
	// full wake reasons (including WakeConfig).
	if cfg != nil {
		reasons := wakeReasons(b, cfg, sp, poolDesired, nil, readyWaitSet, clock.Real{})
		if pinAwakeWakeReasonVisible(b, cfg, time.Now().UTC()) && !containsWakeReason(reasons, WakePin) {
			reasons = append(reasons, WakePin)
		}
		if len(reasons) > 0 {
			parts := make([]string, len(reasons))
			for i, r := range reasons {
				parts[i] = string(r)
			}
			return strings.Join(parts, ",")
		}
	}

	return "-"
}

// resetPendingReasonVisible keeps the fallback renderer aligned with the API
// lifecycle reason rules for live reset requests.
func resetPendingReasonVisible(s session.Info, b beads.Bead, sp runtime.Provider, now time.Time) bool {
	var isRunning func(string) bool
	if sp != nil {
		isRunning = sp.IsRunning
	}
	return session.LifecycleResetPendingReasonVisible(b.Status, b.Metadata, now, s.SessionName, isRunning)
}

func circuitOpenReasonVisible(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[sessionCircuitStateMetadata]) == circuitOpen.String()
}

func pinAwakeWakeReasonVisible(b beads.Bead, cfg *config.City, now time.Time) bool {
	if strings.TrimSpace(b.Metadata["pin_awake"]) != "true" || cfg == nil {
		return false
	}
	state := sessionMetadataState(b)
	if b.Status == "closed" || state == "closed" || state == "suspended" {
		return false
	}
	if isDrainedSessionBead(b) || b.Metadata["dependency_only"] == "true" || b.Metadata["wait_hold"] != "" {
		return false
	}
	if metadataTimeInFuture(b.Metadata["held_until"], now) || metadataTimeInFuture(b.Metadata["quarantined_until"], now) {
		return false
	}
	agent := findAgentByTemplate(cfg, normalizedSessionTemplate(b, cfg))
	if agent == nil {
		return false
	}
	return !citySuspended(cfg) && !isAgentEffectivelySuspended(cfg, agent)
}

func metadataTimeInFuture(raw string, now time.Time) bool {
	if raw == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, raw)
	return err == nil && !t.IsZero() && now.Before(t)
}

func readyWaitSetForList(store beads.Store) (map[string]bool, error) {
	items, err := loadWaitBeads(store)
	ready := make(map[string]bool)
	for _, item := range items {
		if item.Metadata["state"] != waitStateReady {
			continue
		}
		sessionID := item.Metadata["session_id"]
		if sessionID != "" {
			ready[sessionID] = true
		}
	}
	return ready, err
}

// cliPoolDesired computes a static pool desired count from config.
// Uses pool.Max as an approximation since the CLI doesn't run the
// dynamic pool evaluator. This ensures pool sessions within Max
// show "config" as a wake reason. Pools with Max < 0 (unlimited)
// are omitted — without the dynamic evaluator, we can't determine
// their desired count, so they won't show "config" reason.
func cliPoolDesired(cfg *config.City) map[string]int {
	if cfg == nil {
		return nil
	}
	counts := make(map[string]int)
	for _, a := range cfg.Agents {
		sp := scaleParamsFor(&a)
		if a.SupportsInstanceExpansion() && sp.Max > 0 {
			counts[a.QualifiedName()] = sp.Max
		}
	}
	return counts
}

// newSessionAttachCmd creates the "gc session attach <id-or-alias>" command.
func newSessionAttachCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "attach <session-id-or-alias>",
		Short: "Attach to (or resume) a chat session",
		Long: `Attach to a running session or resume a suspended one.

If the session is active with a live tmux session, reattaches.
If the session is suspended or the tmux session died, resumes
using the provider's resume mechanism (if supported) or restarts.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionAttach(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
}

// cmdSessionAttach is the CLI entry point for "gc session attach".
func cmdSessionAttach(args []string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc session attach")
	if store == nil {
		return code
	}

	sessionID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	catalog, err := workerSessionCatalogWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Get the session to find its template.
	info, err := catalog.Get(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Attaching to session %s (%s)...\n", sessionID, info.Template) //nolint:errcheck // best-effort stdout
	if err := handle.Attach(context.Background()); err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// buildResumeCommand constructs the command and runtime.Config for resuming
// a session. Uses provider resume if the session has a session key and the
// provider supports resume; otherwise falls back to the stored command.
//
// cityPath is needed to resolve the --settings flag for Claude sessions.
// Without it, SessionStart hooks defined in .gc/settings.json are not loaded
// when gc session attach starts the process (as opposed to the reconciler).
// For Claude providers, the managed settings file is projected here via
// ensureClaudeSettingsArgs so `gc session attach` on a fresh city still
// emits `--settings` even when the reconciler hasn't run yet.
//
// stderr receives projection errors (use io.Discard to ignore).
//
// sessionKind is the persisted session kind when available. A provider session
// was created from a bare provider name, so agent-template lookup must be
// skipped to avoid agent/provider name collisions.
func buildResumeCommand(cityPath string, cfg *config.City, info session.Info, sessionKind string, metadata map[string]string, stderr io.Writer) (string, runtime.Config) {
	cmd := session.BuildResumeCommand(info)
	if cfg == nil {
		return cmd, runtime.Config{WorkDir: info.WorkDir}
	}

	buildResolved := func(resolved *config.ResolvedProvider) (string, runtime.Config) {
		if resolved == nil {
			return cmd, runtime.Config{WorkDir: info.WorkDir}
		}
		resolvedInfo := info
		// Build command with default args and settings, matching the
		// reconciler's template_resolve.go command construction.
		command := resolved.CommandString()
		resumeCommand := resolved.ResumeCommand
		appendDefaultArgs := func() {
			if defaultArgs := resolved.ResolveDefaultArgs(); len(defaultArgs) > 0 {
				command = command + " " + shellquote.Join(defaultArgs)
			}
		}
		if overrides, err := session.ParseTemplateOverrides(metadata); err == nil {
			transport := strings.TrimSpace(info.Transport)
			launchCommand, err := config.BuildProviderLaunchCommand(cityPath, resolved, overrides, transport)
			if err == nil && strings.TrimSpace(launchCommand.Command) != "" {
				command = launchCommand.Command
			} else {
				appendDefaultArgs()
			}
			if command, err := config.BuildProviderResumeCommand(resolved, overrides); err == nil && strings.TrimSpace(command) != "" {
				resumeCommand = command
			}
		} else {
			appendDefaultArgs()
		}
		// buildResumeCommand is best-effort: log projection failures and
		// continue so `gc session attach` still starts the agent. The strict
		// path is resolveTemplate at reconciler time, which fails agent
		// creation on projection errors.
		providerFamily := resolvedProviderLaunchFamily(resolved)
		sa, saErr := ensureClaudeSettingsArgs(fsys.OSFS{}, cityPath, providerFamily, stderr)
		if saErr == nil && sa != "" && !storedCommandHasSettingsArg(command) {
			command = command + " " + sa
		} else if saErr != nil {
			// Projection failed this tick. Fall back to the last-known-good
			// projection on disk, but require the file to be actually
			// readable — not just Stat-present. Pointing Claude at an
			// unreadable --settings path would fail agent startup worse
			// than launching without --settings at all. On a fresh city
			// with a malformed override, attach therefore launches without
			// --settings; on an older city with a readable prior projection,
			// attach uses that projection.
			if probe := settingsArgsIfReadable(cityPath, providerFamily); probe != "" {
				command = command + " " + probe
			}
		}
		resolvedInfo.Command = command
		resolvedInfo.Provider = resolved.Name
		resolvedInfo.ResumeFlag = resolved.ResumeFlag
		resolvedInfo.ResumeStyle = resolved.ResumeStyle
		resolvedInfo.ResumeCommand = resumeCommand
		return session.BuildResumeCommand(resolvedInfo), runtime.Config{
			WorkDir:                info.WorkDir,
			Lifecycle:              runtime.Lifecycle(resolved.Lifecycle),
			ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
			ReadyDelayMs:           resolved.ReadyDelayMs,
			ProcessNames:           resolved.ProcessNames,
			EmitsPermissionWarning: resolved.EmitsPermissionWarning,
			AcceptStartupDialogs:   resolved.AcceptStartupDialogs,
			Env:                    resolved.Env,
		}
	}

	// Prefer the current resolved agent template/provider config over stale
	// stored command text so submit/restart paths honor provider overrides.
	// Use the same collision guard as the runtime resolver so provider-track
	// sessions do not accidentally resolve through an agent with the same name.
	found, foundAgent := resolveAgentIdentity(cfg, info.Template, "")
	if session.UseAgentTemplateForProviderResolution(sessionKind, metadata, info.Provider, found.Provider, foundAgent) {
		if foundAgent {
			if resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
				return buildResolved(resolved)
			}
		}
	}

	// Fallback for provider-only sessions. Prefer the persisted provider so
	// resumed sessions use the same schema-backed provider selected at create.
	for _, providerName := range []string{info.Provider, info.Template} {
		providerName = strings.TrimSpace(providerName)
		if providerName == "" {
			continue
		}
		if resolved, err := config.ResolveProvider(&config.Agent{Provider: providerName}, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
			return buildResolved(resolved)
		}
	}

	return cmd, runtime.Config{WorkDir: info.WorkDir}
}

// newSessionSuspendCmd creates the "gc session suspend <id-or-alias>" command.
func newSessionSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "suspend <session-id-or-alias>",
		Short: "Suspend a session (save state, free resources)",
		Long: `Suspend an active session by stopping its runtime process.
The session bead persists and can be resumed later.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionSuspend(args, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	return cmd
}

// cmdSessionSuspend is the CLI entry point for "gc session suspend".
//
// Phase 2: sets held_until metadata on the session bead and pokes the
// controller. The reconciler handles the actual process stop. Falls back
// to direct suspend via the session manager if the controller isn't running.
func cmdSessionSuspend(args []string, stdout, stderr io.Writer, jsonOutput ...bool) int {
	asJSON := sessionJSONRequested(jsonOutput)
	store, code := openCityStore(stderr, "gc session suspend")
	if store == nil {
		return code
	}

	cityPath, cityErr := resolveCity()
	var cfg *config.City
	if cityErr == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Try reconciler-first path: set held_until metadata, poke controller.
	// Only use this path when the city is managed by a standalone controller
	// or the machine-wide supervisor — not for unmanaged ad-hoc cities.
	if cityErr == nil && cityUsesManagedReconciler(cityPath) {
		if pokeErr := pokeController(cityPath); pokeErr == nil {
			// Controller is running — metadata-only suspend.
			// Set held_until far in the future so the reconciler drains/stops the session.
			heldUntil := time.Now().Add(indefiniteHoldDuration).UTC().Format(time.RFC3339)
			if err := store.SetMetadataBatch(sessionID, map[string]string{
				"held_until":   heldUntil,
				"sleep_intent": "user-hold",
				"state":        "suspended",
			}); err != nil {
				fmt.Fprintf(stderr, "gc session suspend: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			// Poke again to trigger immediate reconciler tick.
			_ = pokeController(cityPath)
			if asJSON {
				if err := writeSessionActionJSON(stdout, sessionActionResult{
					Action:    "suspend",
					SessionID: sessionID,
					Mode:      "managed",
					State:     "suspended",
				}); err != nil {
					fmt.Fprintf(stderr, "gc session suspend: %v\n", err) //nolint:errcheck // best-effort stderr
					return 1
				}
				return 0
			}
			fmt.Fprintf(stdout, "Session %s suspended. Resume with: gc session wake %s\n", sessionID, sessionID) //nolint:errcheck // best-effort stdout
			return 0
		}
	}

	// Fallback: controller not running — direct suspend via worker handle.
	sp := newSessionProvider()
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := handle.Stop(context.Background()); err != nil {
		fmt.Fprintf(stderr, "gc session suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if asJSON {
		if err := writeSessionActionJSON(stdout, sessionActionResult{
			Action:    "suspend",
			SessionID: sessionID,
			Mode:      "direct",
			State:     "suspended",
		}); err != nil {
			fmt.Fprintf(stderr, "gc session suspend: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Session %s suspended. Resume with: gc session attach %s\n", sessionID, sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionCloseCmd creates the "gc session close <id-or-alias>" command.
func newSessionCloseCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "close <session-id-or-alias>",
		Short: "Close a session permanently",
		Long: `End a conversation. Stops the runtime if active and closes the bead.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionClose(args, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	return cmd
}

// cmdSessionClose is the CLI entry point for "gc session close".
func cmdSessionClose(args []string, stdout, stderr io.Writer, jsonOutput ...bool) int {
	asJSON := sessionJSONRequested(jsonOutput)
	store, code := openCityStore(stderr, "gc session close")
	if store == nil {
		return code
	}

	cityPath, cityErr := resolveCity()
	var cfg *config.City
	if cityErr == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	closeResult, err := handle.CloseDetailed(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cityErr == nil {
		if err := withdrawQueuedWaitNudges(cityPath, closeResult.WaitNudgeIDs); err != nil {
			fmt.Fprintf(stderr, "gc session close: warning: withdrawing queued wait nudges: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	if asJSON {
		if err := writeSessionActionJSON(stdout, sessionActionResult{
			Action:              "close",
			SessionID:           sessionID,
			State:               "closed",
			WaitNudgesWithdrawn: len(closeResult.WaitNudgeIDs),
		}); err != nil {
			fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Session %s closed.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionRenameCmd creates the "gc session rename <id-or-alias> <title>" command.
func newSessionRenameCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "rename <session-id-or-alias> <title>",
		Short: "Rename a session",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionRename(args, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	return cmd
}

// cmdSessionRename is the CLI entry point for "gc session rename".
func cmdSessionRename(args []string, stdout, stderr io.Writer, jsonOutput ...bool) int {
	asJSON := sessionJSONRequested(jsonOutput)
	title := args[1]

	store, code := openCityStore(stderr, "gc session rename")
	if store == nil {
		return code
	}

	cityPath, err := resolveCity()
	var cfg *config.City
	if err == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session rename: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session rename: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := handle.Rename(context.Background(), title); err != nil {
		fmt.Fprintf(stderr, "gc session rename: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if asJSON {
		if err := writeSessionActionJSON(stdout, sessionActionResult{
			Action:    "rename",
			SessionID: sessionID,
			Title:     title,
		}); err != nil {
			fmt.Fprintf(stderr, "gc session rename: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Session %s renamed to %q.\n", sessionID, title) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionPruneCmd creates the "gc session prune" command.
func newSessionPruneCmd(stdout, stderr io.Writer) *cobra.Command {
	var beforeStr string
	var statesStr string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Close old dormant sessions",
		Long: `Close dormant sessions older than a given age. By default only
suspended sessions are affected — active sessions are never pruned. Pass
--state to opt asleep or drained sessions into the same cleanup pass; multiple
states may be comma-separated.`,
		Example: `  gc session prune --before 7d
  gc session prune --before 24h
  gc session prune --state asleep,suspended,drained --before 1h`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdSessionPrune(beforeStr, statesStr, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&beforeStr, "before", "7d", "prune sessions older than this duration (e.g., 7d, 24h)")
	cmd.Flags().StringVar(&statesStr, "state", "suspended", "comma-separated states to prune (suspended, asleep, drained)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	return cmd
}

// cmdSessionPrune is the CLI entry point for "gc session prune".
func cmdSessionPrune(beforeStr, statesStr string, stdout, stderr io.Writer, jsonOutput ...bool) int {
	asJSON := sessionJSONRequested(jsonOutput)
	dur, err := parsePruneDuration(beforeStr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session prune: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	states, err := parsePruneStates(statesStr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session prune: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc session prune")
	if store == nil {
		return code
	}

	sp := newSessionProvider()
	catalog, err := workerSessionCatalogWithConfig("", store, sp, nil)
	if err != nil {
		fmt.Fprintf(stderr, "gc session prune: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cutoff := time.Now().Add(-dur)
	result, err := catalog.PruneBefore(cutoff, states...)
	if err != nil {
		fmt.Fprintf(stderr, "gc session prune: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cityPath, err := resolveCity(); err == nil {
		if err := withdrawQueuedWaitNudges(cityPath, result.WaitNudgeIDs); err != nil {
			fmt.Fprintf(stderr, "gc session prune: warning: withdrawing queued wait nudges: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	if asJSON {
		if err := writeSessionActionJSON(stdout, sessionActionResult{
			Action: "prune",
			Count:  &result.Count,
			Before: beforeStr,
			Cutoff: cutoff.UTC().Format(time.RFC3339),
			State:  formatPruneStates(states),
		}); err != nil {
			fmt.Fprintf(stderr, "gc session prune: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	if result.Count == 0 {
		fmt.Fprintln(stdout, "No sessions to prune.") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "Pruned %d session(s).\n", result.Count) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// parsePruneStates parses a comma-separated list of session state names
// for `gc session prune --state`. Only terminal-dormant states are accepted
// (suspended, asleep, drained) — active or in-flight states are rejected to
// keep the prune pass safe.
func parsePruneStates(s string) ([]worker.SessionState, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("--state must not be empty")
	}
	seen := map[worker.SessionState]struct{}{}
	var out []worker.SessionState
	for _, raw := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		var st worker.SessionState
		switch name {
		case string(worker.SessionStateSuspended):
			st = worker.SessionStateSuspended
		case string(worker.SessionStateAsleep):
			st = worker.SessionStateAsleep
		case string(worker.SessionStateDrained):
			st = worker.SessionStateDrained
		default:
			return nil, fmt.Errorf("unsupported state %q (allowed: suspended, asleep, drained)", name)
		}
		if _, dup := seen[st]; dup {
			continue
		}
		seen[st] = struct{}{}
		out = append(out, st)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--state must list at least one state")
	}
	return out, nil
}

func formatPruneStates(states []worker.SessionState) string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, string(state))
	}
	return strings.Join(names, ",")
}

// parsePruneDuration parses a duration string like "7d", "24h", "30m".
// Extends time.ParseDuration with support for "d" (days).
// Rejects negative and zero durations.
func parsePruneDuration(s string) (time.Duration, error) {
	var dur time.Duration
	if strings.HasSuffix(s, "d") {
		numStr := strings.TrimSuffix(s, "d")
		n, err := strconv.Atoi(numStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		if n <= 0 {
			return 0, fmt.Errorf("duration must be positive, got %q", s)
		}
		dur = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		dur, err = time.ParseDuration(s)
		if err != nil {
			return 0, err
		}
		if dur <= 0 {
			return 0, fmt.Errorf("duration must be positive, got %q", s)
		}
	}
	return dur, nil
}

// newSessionPeekCmd creates the "gc session peek <id-or-alias>" command.
func newSessionPeekCmd(stdout, stderr io.Writer) *cobra.Command {
	var lines int
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "peek <session-id-or-alias>",
		Short: "View session output without attaching",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionPeek(args, lines, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().IntVar(&lines, "lines", 50, "number of lines to capture")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result")
	return cmd
}

type sessionPeekJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Target        string `json:"target"`
	Lines         int    `json:"lines"`
	LineCount     int    `json:"line_count"`
	Output        string `json:"output"`
}

// sessionPeekAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests can inject one.
var sessionPeekAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeSessionPeek dispatches `session peek` to the supervisor API when a
// controller is up; otherwise falls back to the local runtime provider.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
// The API path passes the raw target to the server which resolves aliases;
// fallback resolves locally via resolveSessionIDWithConfig.
func routeSessionPeek(_, target string, lines int, c *api.Client, nilReason string, jsonOutput bool, stdout, stderr io.Writer) int {
	const cmdName = "session peek"
	if c != nil {
		cr, err := c.GetSession(target, true, lines)
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderSessionPeekFromAPI(cr, target, lines, jsonOutput, stdout, stderr)
		}
		if !api.ShouldFallbackForRead(err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doSessionPeekFallback(target, lines, jsonOutput, stdout, stderr)
}

// renderSessionPeekFromAPI writes the API-sourced peek output to stdout,
// appending a staleness banner on stderr-or-stdout when the supervisor
// cache age crosses the threshold. Matches the fallback path's text output
// semantics: trailing newline if the preview doesn't already end in one.
func renderSessionPeekFromAPI(cr api.CachedRead[api.SessionView], target string, lines int, jsonOutput bool, stdout, stderr io.Writer) int {
	output := cr.Body.LastOutput
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, sessionPeekJSONResult{
			SchemaVersion: "1",
			SessionID:     cr.Body.ID,
			Target:        target,
			Lines:         lines,
			LineCount:     outputLineCount(output),
			Output:        output,
		}); err != nil {
			fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprint(stdout, output) //nolint:errcheck // best-effort stdout
	if !strings.HasSuffix(output, "\n") {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
	}
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck
	}
	return 0
}

// cmdSessionPeek is the CLI entry point for "gc session peek". It routes
// through the supervisor API when a controller is up and falls back to the
// local runtime provider otherwise.
func cmdSessionPeek(args []string, lines int, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	c, reason := sessionPeekAPIClient(cityPath)
	return routeSessionPeek(cityPath, args[0], lines, c, reason, jsonOutput, stdout, stderr)
}

// doSessionPeekFallback is the direct runtime-provider path for
// "gc session peek".
func doSessionPeekFallback(target string, lines int, jsonOutput bool, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc session peek")
	if store == nil {
		return code
	}

	cityPath, err := resolveCity()
	var cfg *config.City
	if err == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, store, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	output, err := handle.Peek(context.Background(), lines)
	if err != nil {
		fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOutput {
		if err := writeCLIJSONLine(stdout, sessionPeekJSONResult{
			SchemaVersion: "1",
			SessionID:     sessionID,
			Target:        target,
			Lines:         lines,
			LineCount:     outputLineCount(output),
			Output:        output,
		}); err != nil {
			fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	fmt.Fprint(stdout, output) //nolint:errcheck // best-effort stdout
	if !strings.HasSuffix(output, "\n") {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func outputLineCount(output string) int {
	if output == "" {
		return 0
	}
	count := strings.Count(output, "\n")
	if !strings.HasSuffix(output, "\n") {
		count++
	}
	return count
}

// newSessionKillCmd creates the "gc session kill <id-or-alias>" command.
func newSessionKillCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "kill <session-id-or-alias>",
		Short: "Force-kill session runtime (reconciler restarts)",
		Long: `Force-kill the runtime process for a session without changing its bead state.

The session remains marked as active, so the reconciler will detect the dead
process and restart it according to the session's lifecycle rules. This is
useful for unsticking a session without losing its conversation history.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionKill(args, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	return cmd
}

// cmdSessionKill is the CLI entry point for "gc session kill".
func cmdSessionKill(args []string, stdout, stderr io.Writer, jsonOutput ...bool) int {
	asJSON := sessionJSONRequested(jsonOutput)
	store, code := openCityStore(stderr, "gc session kill")
	if store == nil {
		return code
	}

	cityPath, err := resolveCity()
	var cfg *config.City
	if err == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session kill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	bead, beadErr := store.Get(sessionID)
	identity := ""
	runtimeAlreadyInactive := false
	if beadErr == nil {
		identity = namedSessionIdentity(bead)
		runtimeAlreadyInactive = sessionKillRuntimeAlreadyInactive(bead, sp)
	}

	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session kill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	killErr := handle.Kill(context.Background())
	if killErr != nil && (identity == "" || !runtimeAlreadyInactive) {
		fmt.Fprintf(stderr, "gc session kill: %v\n", killErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	if beadErr != nil {
		fmt.Fprintf(stderr, "gc session kill: warning: loading session %s for circuit breaker clear: %v\n", sessionID, beadErr) //nolint:errcheck // best-effort stderr
	} else if identity != "" {
		if err := resetSessionCircuitBreakerAfterExplicitKill(cityPath, store, sessionID, identity); err != nil {
			fmt.Fprintf(stderr, "gc session kill: warning: clearing session circuit breaker for %q: %v\n", identity, err) //nolint:errcheck // best-effort stderr
			if killErr != nil {
				fmt.Fprintf(stderr, "gc session kill: %v\n", killErr) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
	}
	if killErr != nil {
		fmt.Fprintf(stderr, "gc session kill: warning: session %s runtime was already inactive; cleared named-session circuit breaker\n", sessionID) //nolint:errcheck // best-effort stderr
	}

	// Use the resolved session ID as the canonical Subject for event
	// consumers. This ensures a stable key regardless of how the user
	// specified the target (session ID or alias).
	rec := openCityRecorder(stderr)
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   eventActor(),
		Subject: sessionID,
		Message: "killed",
		Payload: api.SessionLifecyclePayloadJSON(sessionID, "", "killed"),
	})
	if asJSON {
		if err := writeSessionActionJSON(stdout, sessionActionResult{
			Action:    "kill",
			SessionID: sessionID,
		}); err != nil {
			fmt.Fprintf(stderr, "gc session kill: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Session %s killed.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

func sessionKillRuntimeAlreadyInactive(bead beads.Bead, sp runtime.Provider) bool {
	switch session.State(strings.TrimSpace(bead.Metadata["state"])) {
	case session.StateActive, session.StateStartPending, session.StateCreating, session.StateDraining, session.StateAwake:
		return false
	}
	sessionName := strings.TrimSpace(bead.Metadata["session_name"])
	return sp != nil && sessionName != "" && !sp.IsRunning(sessionName)
}

// newSessionNudgeCmd creates the "gc session nudge <id-or-alias> <message>" command.
func newSessionNudgeCmd(stdout, stderr io.Writer) *cobra.Command {
	var delivery string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "nudge <id-or-alias> <message...>",
		Short: "Send a text message to a running session",
		Long: `Send text input to a running session via the runtime provider.

The message is delivered as text content to the session's input. This is
equivalent to typing the message into the session's terminal.

Accepts a session ID or session alias. Multi-word messages are
joined automatically.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			mode, err := parseNudgeDeliveryMode(delivery)
			if err != nil {
				fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdSessionNudge(args, mode, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().StringVar(&delivery, "delivery", string(nudgeDeliveryWaitIdle), "delivery mode: immediate, wait-idle, or queue")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	return cmd
}

func parseSessionSubmitIntent(raw string) (session.SubmitIntent, error) {
	switch strings.TrimSpace(raw) {
	case "", string(session.SubmitIntentDefault):
		return session.SubmitIntentDefault, nil
	case "follow-up", string(session.SubmitIntentFollowUp):
		return session.SubmitIntentFollowUp, nil
	case "interrupt-now", string(session.SubmitIntentInterruptNow):
		return session.SubmitIntentInterruptNow, nil
	default:
		return "", fmt.Errorf("unknown submit intent %q (want default, follow_up, or interrupt_now)", raw)
	}
}

type sessionSubmitJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Target        string `json:"target"`
	Intent        string `json:"intent"`
	Queued        bool   `json:"queued"`
	Outcome       string `json:"outcome"`
}

func cmdSessionSubmit(args []string, intent session.SubmitIntent, jsonOutput bool, stdout, stderr io.Writer) int {
	target := args[0]
	message := strings.Join(args[1:], " ")

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if c := apiClient(cityPath); c != nil {
		resp, err := c.SubmitSession(target, message, intent)
		if err == nil {
			return emitSessionSubmitResult(stdout, stderr, target, intent, resp.Queued, jsonOutput)
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, code := openCityStore(stderr, "gc session submit")
	if store == nil {
		return code
	}

	sessionID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	outcome, err := handle.Message(context.Background(), worker.MessageRequest{
		Text:     message,
		Delivery: workerDeliveryIntentForSubmitIntent(intent),
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return emitSessionSubmitResult(stdout, stderr, target, intent, outcome.Queued, jsonOutput)
}

func emitSessionSubmitResult(stdout, stderr io.Writer, target string, intent session.SubmitIntent, queued, jsonOutput bool) int {
	if jsonOutput {
		outcome := "submitted"
		if queued {
			outcome = "queued"
		} else if intent == session.SubmitIntentInterruptNow {
			outcome = "interrupted"
		}
		return writeCLIJSONLineOrExit(stdout, stderr, "gc session submit", sessionSubmitJSON{
			SchemaVersion: "1",
			OK:            true,
			Target:        target,
			Intent:        string(intent),
			Queued:        queued,
			Outcome:       outcome,
		})
	}
	switch {
	case queued:
		fmt.Fprintf(stdout, "Queued follow-up for %s\n", target) //nolint:errcheck // best-effort stdout
	case intent == session.SubmitIntentFollowUp:
		fmt.Fprintf(stdout, "Submitted follow-up to %s\n", target) //nolint:errcheck // best-effort stdout
	case intent == session.SubmitIntentInterruptNow:
		fmt.Fprintf(stdout, "Interrupted and submitted to %s\n", target) //nolint:errcheck // best-effort stdout
	default:
		fmt.Fprintf(stdout, "Submitted to %s\n", target) //nolint:errcheck // best-effort stdout
	}
	return 0
}

type sessionNudgeJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Target        string `json:"target"`
	SessionID     string `json:"session_id,omitempty"`
	SessionName   string `json:"session_name,omitempty"`
	Delivery      string `json:"delivery"`
	Queued        bool   `json:"queued"`
	Outcome       string `json:"outcome"`
}

// cmdSessionNudge is the CLI entry point for "gc session nudge".
func cmdSessionNudge(args []string, delivery nudgeDeliveryMode, jsonOutput bool, stdout, stderr io.Writer) int {
	target := args[0]
	message := strings.Join(args[1:], " ")

	targetInfo, err := resolveNudgeTarget(target)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return deliverSessionNudge(targetInfo, message, delivery, jsonOutput, stdout, stderr)
}

// resolveWorkDir determines the working directory for a session based on the
// agent config. work_dir overrides dir, while dir still carries rig identity.
func resolveWorkDir(cityPath string, cfg *config.City, agent *config.Agent) (string, error) {
	return resolveWorkDirForQualifiedName(cityPath, cfg, agent, "")
}

func resolveWorkDirForQualifiedName(cityPath string, cfg *config.City, agent *config.Agent, qualifiedName string) (string, error) {
	cityName := loadedCityName(cfg, cityPath)
	var rigs []config.Rig
	if cfg != nil {
		rigs = cfg.Rigs
	}
	return resolveConfiguredWorkDir(cityPath, cityName, qualifiedName, agent, rigs)
}

func sessionExplicitNameForNewSession(cityPath, cityName string, rigs []config.Rig, agent *config.Agent, alias string) (string, error) {
	if agent == nil {
		return "", nil
	}
	// tmux_alias takes precedence: when set, the resolved name becomes the
	// explicit session_name regardless of whether --alias was supplied. This
	// is what gives crew sessions readable tmux names like "crew--<rig>".
	if resolved, err := workdirutil.ResolveTmuxAlias(cityPath, cityName, *agent, rigs); err != nil {
		return "", fmt.Errorf("resolving tmux_alias: %w", err)
	} else if resolved != "" {
		return session.ValidateExplicitName(resolved)
	}
	if !agent.SupportsMultipleSessions() || strings.TrimSpace(alias) != "" {
		return "", nil
	}
	return session.GenerateAdhocExplicitName(agent.Name)
}

func shouldAttachNewSession(noAttach bool, transport string) bool {
	return !noAttach && transport != config.SessionTransportACP
}

// formatDuration formats a duration for human display.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
