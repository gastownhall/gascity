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
			if cmdSessionSubmit(args, parsedIntent, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&intent, "intent", string(session.SubmitIntentDefault), "submit intent: default, follow_up, or interrupt_now")
	return cmd
}

// newSessionNewCmd creates the "gc session new <template>" command.
func newSessionNewCmd(stdout, stderr io.Writer) *cobra.Command {
	var title string
	var alias string
	var titleHint string
	var noAttach bool
	cmd := &cobra.Command{
		Use:   "new <template>",
		Short: "Create a new chat session from an agent template",
		Long: `Create a new persistent conversation from an agent template defined
in the loaded city configuration. By default, attaches the terminal
after creation.

When --title-hint is provided without --title, the session title is
auto-generated from the hint text: a short version is set immediately
and refined by the title model in the background.`,
		Example: `  gc session new helper
  gc session new helper --alias sky
  gc session new helper --title "debugging auth"
  gc session new helper --title-hint "fix the login redirect loop"
  gc session new helper --no-attach`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionNew(args, alias, title, titleHint, noAttach, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&alias, "alias", "", "human-friendly session identifier for commands and mail")
	cmd.Flags().StringVar(&title, "title", "", "human-readable session title")
	cmd.Flags().StringVar(&titleHint, "title-hint", "", "text to auto-generate a session title from")
	cmd.Flags().BoolVar(&noAttach, "no-attach", false, "create session without attaching")
	return cmd
}

// cmdSessionNew is the CLI entry point for "gc session new".
//
// Phase 2: creates a session bead and pokes the controller. The reconciler
// handles process lifecycle (start). If the controller is not running,
// falls back to direct process start via the session manager.
func cmdSessionNew(args []string, alias, title, titleHint string, noAttach bool, stdout, stderr io.Writer) int {
	templateName := args[0]

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
	requestedAlias, err := session.ValidateAlias(alias)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	alias = requestedAlias
	if alias != "" && found.SupportsMultipleSessions() {
		alias = workdirutil.SessionQualifiedName(cityPath, found, cfg.Rigs, requestedAlias, "")
	}
	explicitName, err := sessionExplicitNameForNewSession(&found, alias)
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
	sessionCommand, err := resolvedSessionCommand(cityPath, resolved, nil)
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
				found.Session,
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

			fmt.Fprintf(stdout, "Session %s created from template %q (reconciler will start it).\n", info.ID, canonicalTemplate) //nolint:errcheck // best-effort stdout

			if !shouldAttachNewSession(noAttach, found.Session) {
				if found.Session == "acp" && !noAttach {
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
		found.Session,
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

	fmt.Fprintf(stdout, "Session %s created from template %q.\n", info.ID, canonicalTemplate) //nolint:errcheck // best-effort stdout

	if !shouldAttachNewSession(noAttach, found.Session) {
		if found.Session == "acp" && !noAttach {
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

func resolvedSessionCommand(cityPath string, resolved *config.ResolvedProvider, optionOverrides map[string]string) (string, error) {
	if resolved == nil {
		return "", fmt.Errorf("resolved provider is nil")
	}
	launchCommand, err := config.BuildProviderLaunchCommand(cityPath, resolved, optionOverrides)
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
	if strings.Contains(input, "/") {
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

// cmdSessionList is the CLI entry point for "gc session list".
func cmdSessionList(stateFilter, templateFilter string, jsonOutput bool, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc session list")
	if store == nil {
		return code
	}

	providerCtx := loadSessionProviderContext()

	// Launch readyWaitSet concurrently with the shared session-bead load,
	// but only on the non-JSON path — JSON output returns early and doesn't
	// need wait-state computation.
	type waitResult struct {
		set map[string]bool
	}
	var waitCh chan waitResult

	if !jsonOutput {
		waitCh = make(chan waitResult, 1)

		go func() {
			waitCh <- waitResult{set: readyWaitSetForList(store)}
		}()
	}

	allSessionBeads, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc session list: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sessionBeads := newSessionBeadSnapshot(allSessionBeads)
	sp := newSessionProviderFromContext(providerCtx, sessionBeads)
	catalog, err := workerSessionCatalogWithConfig("", store, sp, providerCtx.cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc session list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	listResult := catalog.ListFullFromBeads(allSessionBeads, stateFilter, templateFilter)
	sessions := listResult.Sessions

	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sessions) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Build bead index from the beads already fetched by ListFull (no duplicate query).
	beadIndex := make(map[string]beads.Bead, len(listResult.Beads))
	for _, b := range listResult.Beads {
		beadIndex[b.ID] = b
	}

	readyWaitSet := (<-waitCh).set
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
	fmt.Fprintln(w, "ID\tTEMPLATE\tSTATE\tREASON\tTARGET\tTITLE\tAGE\tLAST ACTIVE") //nolint:errcheck // best-effort stdout
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Template, state, reason, target, title, age, lastActive) //nolint:errcheck // best-effort stdout
	}
	_ = w.Flush() //nolint:errcheck // best-effort stdout
	return 0
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

	// If config is available, compute full wake reasons (including WakeConfig).
	// Otherwise, only bead metadata (sleep/hold/quarantine) is shown.
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

	// No wake reasons (or no config) — show why it's asleep from lifecycle metadata.
	if reason := session.LifecycleDisplayReason(b.Status, b.Metadata, now); reason != "" {
		return reason
	}
	return "-"
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

func readyWaitSetForList(store beads.Store) map[string]bool {
	items, err := loadWaitBeads(store)
	if err != nil {
		return nil
	}
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
	return ready
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
// sessionKind mirrors the mc_session_kind bead metadata: "provider" means
// the session was created from a bare provider name (not an agent template),
// so the agent-template lookup should be skipped. This matches the guard in
// the API handler (handler_session_chat.go).
func buildResumeCommand(cityPath string, cfg *config.City, info session.Info, sessionKind string, stderr io.Writer) (string, runtime.Config) {
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
		if defaultArgs := resolved.ResolveDefaultArgs(); len(defaultArgs) > 0 {
			command = command + " " + shellquote.Join(defaultArgs)
		}
		// buildResumeCommand is best-effort: log projection failures and
		// continue so `gc session attach` still starts the agent. The strict
		// path is resolveTemplate at reconciler time, which fails agent
		// creation on projection errors.
		providerFamily := resolvedProviderLaunchFamily(resolved)
		sa, saErr := ensureClaudeSettingsArgs(fsys.OSFS{}, cityPath, providerFamily, stderr)
		if saErr == nil && sa != "" {
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
		resolvedInfo.ResumeCommand = resolved.ResumeCommand
		return session.BuildResumeCommand(resolvedInfo), runtime.Config{
			WorkDir:                info.WorkDir,
			ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
			ReadyDelayMs:           resolved.ReadyDelayMs,
			ProcessNames:           resolved.ProcessNames,
			EmitsPermissionWarning: resolved.EmitsPermissionWarning,
			Env:                    resolved.Env,
		}
	}

	// Check persisted kind to avoid agent/provider name collisions.
	// If kind is "provider", skip the agent template lookup entirely.
	if sessionKind != "provider" {
		// Prefer the current resolved agent template/provider config over stale
		// stored command text so submit/restart paths honor provider overrides.
		if found, ok := resolveAgentIdentity(cfg, info.Template, ""); ok {
			if resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
				return buildResolved(resolved)
			}
		}
	}

	// Fallback for provider-only sessions whose Template is a provider name.
	if resolved, err := config.ResolveProvider(&config.Agent{Provider: info.Template}, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
		return buildResolved(resolved)
	}

	return cmd, runtime.Config{WorkDir: info.WorkDir}
}

// newSessionSuspendCmd creates the "gc session suspend <id-or-alias>" command.
func newSessionSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <session-id-or-alias>",
		Short: "Suspend a session (save state, free resources)",
		Long: `Suspend an active session by stopping its runtime process.
The session bead persists and can be resumed later.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionSuspend(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSessionSuspend is the CLI entry point for "gc session suspend".
//
// Phase 2: sets held_until metadata on the session bead and pokes the
// controller. The reconciler handles the actual process stop. Falls back
// to direct suspend via the session manager if the controller isn't running.
func cmdSessionSuspend(args []string, stdout, stderr io.Writer) int {
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

	fmt.Fprintf(stdout, "Session %s suspended. Resume with: gc session attach %s\n", sessionID, sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionCloseCmd creates the "gc session close <id-or-alias>" command.
func newSessionCloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "close <session-id-or-alias>",
		Short: "Close a session permanently",
		Long: `End a conversation. Stops the runtime if active and closes the bead.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionClose(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSessionClose is the CLI entry point for "gc session close".
func cmdSessionClose(args []string, stdout, stderr io.Writer) int {
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
	nudgeIDs, err := waitNudgeIDsForSession(store, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := handle.Close(context.Background()); err != nil {
		fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cityErr == nil {
		if err := withdrawQueuedWaitNudges(cityPath, nudgeIDs); err != nil {
			fmt.Fprintf(stderr, "gc session close: warning: withdrawing queued wait nudges: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	fmt.Fprintf(stdout, "Session %s closed.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionRenameCmd creates the "gc session rename <id-or-alias> <title>" command.
func newSessionRenameCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <session-id-or-alias> <title>",
		Short: "Rename a session",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionRename(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSessionRename is the CLI entry point for "gc session rename".
func cmdSessionRename(args []string, stdout, stderr io.Writer) int {
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

	fmt.Fprintf(stdout, "Session %s renamed to %q.\n", sessionID, title) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionPruneCmd creates the "gc session prune" command.
func newSessionPruneCmd(stdout, stderr io.Writer) *cobra.Command {
	var beforeStr string
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Close old suspended sessions",
		Long: `Close suspended sessions older than a given age. Only suspended
sessions are affected — active sessions are never pruned.`,
		Example: `  gc session prune --before 7d
  gc session prune --before 24h`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdSessionPrune(beforeStr, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&beforeStr, "before", "7d", "prune sessions older than this duration (e.g., 7d, 24h)")
	return cmd
}

// cmdSessionPrune is the CLI entry point for "gc session prune".
func cmdSessionPrune(beforeStr string, stdout, stderr io.Writer) int {
	dur, err := parsePruneDuration(beforeStr)
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
	result, err := catalog.PruneBefore(cutoff)
	if err != nil {
		fmt.Fprintf(stderr, "gc session prune: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cityPath, err := resolveCity(); err == nil {
		if err := withdrawQueuedWaitNudges(cityPath, result.WaitNudgeIDs); err != nil {
			fmt.Fprintf(stderr, "gc session prune: warning: withdrawing queued wait nudges: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	if result.Count == 0 {
		fmt.Fprintln(stdout, "No sessions to prune.") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "Pruned %d session(s).\n", result.Count) //nolint:errcheck // best-effort stdout
	}
	return 0
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
	cmd := &cobra.Command{
		Use:   "peek <session-id-or-alias>",
		Short: "View session output without attaching",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionPeek(args, lines, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&lines, "lines", 50, "number of lines to capture")
	return cmd
}

// cmdSessionPeek is the CLI entry point for "gc session peek".
func cmdSessionPeek(args []string, lines int, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc session peek")
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

	fmt.Fprint(stdout, output) //nolint:errcheck // best-effort stdout
	if !strings.HasSuffix(output, "\n") {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// newSessionKillCmd creates the "gc session kill <id-or-alias>" command.
func newSessionKillCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "kill <session-id-or-alias>",
		Short: "Force-kill session runtime (reconciler restarts)",
		Long: `Force-kill the runtime process for a session without changing its bead state.

The session remains marked as active, so the reconciler will detect the dead
process and restart it according to the session's lifecycle rules. This is
useful for unsticking a session without losing its conversation history.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionKill(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSessionKill is the CLI entry point for "gc session kill".
func cmdSessionKill(args []string, stdout, stderr io.Writer) int {
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
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session kill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := handle.Kill(context.Background()); err != nil {
		fmt.Fprintf(stderr, "gc session kill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
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
	})

	fmt.Fprintf(stdout, "Session %s killed.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionNudgeCmd creates the "gc session nudge <id-or-alias> <message>" command.
func newSessionNudgeCmd(stdout, stderr io.Writer) *cobra.Command {
	var delivery string
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
			if cmdSessionNudge(args, mode, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&delivery, "delivery", string(nudgeDeliveryWaitIdle), "delivery mode: immediate, wait-idle, or queue")
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

func cmdSessionSubmit(args []string, intent session.SubmitIntent, stdout, stderr io.Writer) int {
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
			emitSessionSubmitResult(stdout, target, intent, resp.Queued)
			return 0
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
	emitSessionSubmitResult(stdout, target, intent, outcome.Queued)
	return 0
}

func emitSessionSubmitResult(stdout io.Writer, target string, intent session.SubmitIntent, queued bool) {
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
}

// cmdSessionNudge is the CLI entry point for "gc session nudge".
func cmdSessionNudge(args []string, delivery nudgeDeliveryMode, stdout, stderr io.Writer) int {
	target := args[0]
	message := strings.Join(args[1:], " ")

	targetInfo, err := resolveNudgeTarget(target)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return deliverSessionNudge(targetInfo, message, delivery, stdout, stderr)
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

func sessionExplicitNameForNewSession(agent *config.Agent, alias string) (string, error) {
	if agent == nil || !agent.SupportsMultipleSessions() || strings.TrimSpace(alias) != "" {
		return "", nil
	}
	return session.GenerateAdhocExplicitName(agent.Name)
}

func shouldAttachNewSession(noAttach bool, transport string) bool {
	return !noAttach && transport != "acp"
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
