package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

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
				fmt.Fprintln(stderr, "gc session: missing subcommand (new, list, attach, suspend, close, rename, prune, peek, kill, logs)") //nolint:errcheck // best-effort stderr
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
		newSessionSuspendCmd(stdout, stderr),
		newSessionCloseCmd(stdout, stderr),
		newSessionRenameCmd(stdout, stderr),
		newSessionPruneCmd(stdout, stderr),
		newSessionPeekCmd(stdout, stderr),
		newSessionKillCmd(stdout, stderr),
		newSessionNudgeCmd(stdout, stderr),
		newSessionLogsCmd(stdout, stderr),
		newSessionWakeCmd(stdout, stderr),
	)
	return cmd
}

// newSessionNewCmd creates the "gc session new <template>" command.
func newSessionNewCmd(stdout, stderr io.Writer) *cobra.Command {
	var title string
	var noAttach bool
	cmd := &cobra.Command{
		Use:   "new <template>",
		Short: "Create a new chat session from an agent template",
		Long: `Create a new persistent conversation from an agent template defined in
city.toml. By default, attaches the terminal after creation.`,
		Example: `  gc session new helper
  gc session new helper --title "debugging auth"
  gc session new helper --no-attach`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionNew(args, title, noAttach, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "human-readable session title")
	cmd.Flags().BoolVar(&noAttach, "no-attach", false, "create session without attaching")
	return cmd
}

// cmdSessionNew is the CLI entry point for "gc session new".
func cmdSessionNew(args []string, title string, noAttach bool, stdout, stderr io.Writer) int {
	templateName := args[0]

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath)
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

	// Open the bead store.
	store, code := openCityStore(stderr, "gc session new")
	if store == nil {
		return code
	}

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)

	// Build the work directory.
	workDir := resolveWorkDir(cityPath, &found)

	// Build runtime.Config hints from provider.
	hints := runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
	}

	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		SessionIDFlag: resolved.SessionIDFlag,
	}

	templateQN := found.QualifiedName()
	info, err := mgr.CreateWithTransport(context.Background(), templateQN, title, resolved.CommandString(), workDir, resolved.Name, found.Session, resolved.Env, resume, hints)
	if err != nil {
		fmt.Fprintf(stderr, "gc session new: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Session %s created from template %q.\n", info.ID, templateQN) //nolint:errcheck // best-effort stdout

	if !shouldAttachNewSession(noAttach, found.Session) {
		if found.Session == "acp" && !noAttach {
			fmt.Fprintln(stdout, "Session uses ACP transport; not attaching.") //nolint:errcheck // best-effort stdout
		}
		return 0
	}

	fmt.Fprintln(stdout, "Attaching...") //nolint:errcheck // best-effort stdout
	if err := sp.Attach(info.SessionName); err != nil {
		fmt.Fprintf(stderr, "gc session new: attaching: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func resolveSessionTemplate(cfg *config.City, input, currentRigDir string) (config.Agent, bool) {
	found, ok := resolveAgentIdentity(cfg, input, currentRigDir)
	if !ok {
		return config.Agent{}, false
	}
	for _, a := range cfg.Agents {
		if a.QualifiedName() == found.QualifiedName() {
			return a, true
		}
	}
	return config.Agent{}, false
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

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)

	sessions, err := mgr.List(stateFilter, templateFilter)
	if err != nil {
		fmt.Fprintf(stderr, "gc session list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sessions) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Build a bead index for REASON column (always, even without config —
	// sleep_reason, held_until, quarantine metadata live on the bead itself).
	beadIndex := make(map[string]beads.Bead)
	if all, err := store.ListByLabel(session.LabelSession, 0); err == nil {
		for _, b := range all {
			beadIndex[b.ID] = b
		}
	}

	// Load config for wake reason computation (best-effort).
	// Only WakeConfig reason needs config; sleep/hold/quarantine come from beads.
	var cfg *config.City
	var poolDesired map[string]int
	if cityPath, err := resolveCity(); err == nil {
		if c, err := loadCityConfig(cityPath); err == nil {
			cfg = c
			poolDesired = cliPoolDesired(cfg)
		}
	}

	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No sessions found.") //nolint:errcheck // best-effort stdout
		return 0
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTEMPLATE\tSTATE\tREASON\tTITLE\tAGE\tLAST ACTIVE") //nolint:errcheck // best-effort stdout
	for _, s := range sessions {
		state := string(s.State)
		if s.State == "" {
			state = "closed"
		}
		reason := sessionReason(s, beadIndex, cfg, sp, poolDesired)
		title := s.Title
		if title == "" {
			title = "-"
		}
		if len(title) > 30 {
			title = title[:27] + "..."
		}
		age := formatDuration(time.Since(s.CreatedAt))
		lastActive := "-"
		if !s.LastActive.IsZero() {
			lastActive = formatDuration(time.Since(s.LastActive)) + " ago"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Template, state, reason, title, age, lastActive) //nolint:errcheck // best-effort stdout
	}
	_ = w.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

// sessionReason computes the REASON column for a session in gc session list.
// For awake sessions, shows wake reasons (e.g., "config", "attached").
// For asleep sessions, shows the sleep reason (e.g., "user-hold", "quarantine").
// For closed sessions, shows "-".
func sessionReason(s session.Info, beadIndex map[string]beads.Bead, cfg *config.City, sp runtime.Provider, poolDesired map[string]int) string {
	if s.State == "" {
		return "-" // closed
	}

	b, ok := beadIndex[s.ID]
	if !ok {
		return "-" // no bead data available
	}

	// If config is available, compute full wake reasons (including WakeConfig).
	// Otherwise, only bead metadata (sleep/hold/quarantine) is shown.
	if cfg != nil {
		reasons := wakeReasons(b, cfg, sp, poolDesired, nil, clock.Real{})
		if len(reasons) > 0 {
			parts := make([]string, len(reasons))
			for i, r := range reasons {
				parts[i] = string(r)
			}
			return strings.Join(parts, ",")
		}
	}

	// No wake reasons (or no config) — show why it's asleep from bead metadata.
	if sr := b.Metadata["sleep_reason"]; sr != "" {
		return sr
	}
	if b.Metadata["quarantined_until"] != "" {
		return "quarantine"
	}
	if b.Metadata["held_until"] != "" {
		return "user-hold"
	}
	return "-"
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
		if a.Pool != nil {
			pool := a.EffectivePool()
			if pool.Max > 0 {
				counts[a.QualifiedName()] = pool.Max
			}
		}
	}
	return counts
}

// newSessionAttachCmd creates the "gc session attach <id-or-name>" command.
func newSessionAttachCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "attach <session-id-or-name>",
		Short: "Attach to (or resume) a chat session",
		Long: `Attach to a running session or resume a suspended one.

If the session is active with a live tmux session, reattaches.
If the session is suspended or the tmux session died, resumes
using the provider's resume mechanism (if supported) or restarts.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).`,
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
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc session attach")
	if store == nil {
		return code
	}

	sessionID, err := resolveSessionID(store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)

	// Get the session to find its template.
	info, err := mgr.Get(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Build the resume command from the template's provider.
	resumeCmd, hints := buildResumeCommand(cfg, info)

	fmt.Fprintf(stdout, "Attaching to session %s (%s)...\n", sessionID, info.Template) //nolint:errcheck // best-effort stdout
	if err := mgr.Attach(context.Background(), sessionID, resumeCmd, hints); err != nil {
		fmt.Fprintf(stderr, "gc session attach: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// buildResumeCommand constructs the command and runtime.Config for resuming
// a session. Uses provider resume if the session has a session key and the
// provider supports resume; otherwise falls back to the stored command.
func buildResumeCommand(cfg *config.City, info session.Info) (string, runtime.Config) {
	// Build the resume command from stored session info.
	// This handles --resume <key> for providers that support it.
	cmd := session.BuildResumeCommand(info)

	// Try to resolve the template for startup hints and env.
	found, ok := resolveAgentIdentity(cfg, info.Template, "")
	if !ok {
		return cmd, runtime.Config{WorkDir: info.WorkDir}
	}
	resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return cmd, runtime.Config{WorkDir: info.WorkDir}
	}
	hints := runtime.Config{
		WorkDir:                info.WorkDir,
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		Env:                    resolved.Env,
	}
	return cmd, hints
}

// newSessionSuspendCmd creates the "gc session suspend <id-or-name>" command.
func newSessionSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <session-id-or-name>",
		Short: "Suspend a session (save state, free resources)",
		Long: `Suspend an active session by stopping its runtime process.
The session bead persists and can be resumed later.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).`,
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
func cmdSessionSuspend(args []string, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc session suspend")
	if store == nil {
		return code
	}

	sessionID, err := resolveSessionID(store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)

	if err := mgr.Suspend(sessionID); err != nil {
		fmt.Fprintf(stderr, "gc session suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Session %s suspended. Resume with: gc session attach %s\n", sessionID, sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionCloseCmd creates the "gc session close <id-or-name>" command.
func newSessionCloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "close <session-id-or-name>",
		Short: "Close a session permanently",
		Long: `End a conversation. Stops the runtime if active and closes the bead.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).`,
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

	sessionID, err := resolveSessionID(store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)

	if err := mgr.Close(sessionID); err != nil {
		fmt.Fprintf(stderr, "gc session close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Session %s closed.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionRenameCmd creates the "gc session rename <id-or-name> <title>" command.
func newSessionRenameCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <session-id-or-name> <title>",
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

	sessionID, err := resolveSessionID(store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session rename: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)

	if err := mgr.Rename(sessionID, title); err != nil {
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
	mgr := newSessionManager(store, sp)

	cutoff := time.Now().Add(-dur)
	pruned, err := mgr.Prune(cutoff)
	if err != nil {
		fmt.Fprintf(stderr, "gc session prune: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if pruned == 0 {
		fmt.Fprintln(stdout, "No sessions to prune.") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "Pruned %d session(s).\n", pruned) //nolint:errcheck // best-effort stdout
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

// newSessionPeekCmd creates the "gc session peek <id-or-name>" command.
func newSessionPeekCmd(stdout, stderr io.Writer) *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "peek <session-id-or-name>",
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

	sessionID, err := resolveSessionID(store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)

	output, err := mgr.Peek(sessionID, lines)
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

// newSessionKillCmd creates the "gc session kill <id-or-name>" command.
func newSessionKillCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "kill <session-id-or-name>",
		Short: "Force-kill session runtime (reconciler restarts)",
		Long: `Force-kill the runtime process for a session without changing its bead state.

The session remains marked as active, so the reconciler will detect the dead
process and restart it according to the session's lifecycle rules. This is
useful for unsticking a session without losing its conversation history.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).`,
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

	sessionID, err := resolveSessionID(store, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session kill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)

	if err := mgr.Kill(sessionID); err != nil {
		fmt.Fprintf(stderr, "gc session kill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Use the user-supplied name (agent or session ID) as Subject for
	// consistency with other SessionStopped events which use agent names.
	rec := openCityRecorder(stderr)
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   eventActor(),
		Subject: args[0],
		Message: "killed",
	})

	fmt.Fprintf(stdout, "Session %s killed.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// newSessionNudgeCmd creates the "gc session nudge <id-or-name> <message>" command.
func newSessionNudgeCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "nudge <session-id-or-name> <message>",
		Short: "Send a text message to a running session",
		Long: `Send text input to a running session via the runtime provider.

The message is delivered as text content to the session's input. This is
equivalent to typing the message into the session's terminal.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionNudge(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSessionNudge is the CLI entry point for "gc session nudge".
func cmdSessionNudge(args []string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}

	target := args[0]
	message := strings.Join(args[1:], " ")

	// Resolve target to an agent for session name construction.
	found, ok := resolveAgentIdentity(cfg, target, currentRigContext(cfg))
	if !ok {
		fmt.Fprintln(stderr, agentNotFoundMsg("gc session nudge", target, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	sn := sessionName(cityName, found.QualifiedName(), cfg.Workspace.SessionTemplate)

	if !sp.IsRunning(sn) {
		fmt.Fprintf(stderr, "gc session nudge: session %q is not running\n", found.QualifiedName()) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := sp.Nudge(sn, runtime.TextContent(message)); err != nil {
		fmt.Fprintf(stderr, "gc session nudge: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Nudged %s\n", found.QualifiedName()) //nolint:errcheck // best-effort stdout
	return 0
}

// resolveWorkDir determines the working directory for a session based on
// the agent config. Uses the rig path if set, otherwise the city directory.
func resolveWorkDir(cityPath string, agent *config.Agent) string {
	if agent.Dir != "" {
		// Rig-scoped agent: use rig path.
		rigPath := filepath.Join(cityPath, "rigs", agent.Dir)
		return rigPath
	}
	return cityPath
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
