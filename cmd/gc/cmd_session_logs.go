package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/spf13/cobra"
)

func newSessionLogsCmd(stdout, stderr io.Writer) *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <agent-name>",
		Short: "Show session logs for an agent",
		Long: `Show structured session log messages from an agent's JSONL session file.

Reads the agent's session log, resolves the conversation DAG, and prints
messages in chronological order. Searches default paths (~/.claude/projects/)
and any extra paths from [daemon] observe_paths in city.toml.

Use --tail to control how many compaction segments to show (0 = all).
Use -f to follow new messages as they arrive.`,
		Example: `  gc session logs mayor
  gc session logs mayor --tail 0
  gc session logs myrig/polecat-1 -f`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionLogs(args, follow, tail, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow new messages as they arrive")
	cmd.Flags().IntVar(&tail, "tail", 1, "Number of compaction segments to show (0 = all)")
	return cmd
}

// cmdSessionLogs is the CLI entry point for viewing agent session logs.
func cmdSessionLogs(args []string, follow bool, tail int, stdout, stderr io.Writer) int {
	agentName := args[0]

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if store, err := openCityStoreAt(cityPath); err == nil {
		if workDir, ok := resolveSessionLogWorkDir(store, agentName); ok {
			path := sessionlog.FindSessionFile(sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths), workDir)
			if path == "" {
				fmt.Fprintf(stderr, "gc session logs: no session file found for %q\n", agentName) //nolint:errcheck // best-effort stderr
				return 1
			}
			return doSessionLogs(path, follow, tail, stdout, stderr)
		}
	}

	found, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		fmt.Fprintln(stderr, agentNotFoundMsg("gc session logs", agentName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	workDir := resolveAgentWorkDir(found, cfg, cityPath)
	if workDir == "" {
		fmt.Fprintf(stderr, "gc session logs: cannot resolve working directory for %q\n", agentName) //nolint:errcheck // best-effort stderr
		return 1
	}

	searchPaths := sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)

	// For pool instances (e.g. "claude-2"), look up the session bead to
	// get the session key. This resolves the correct JSONL file when
	// multiple pool agents share the same working directory.
	var path string
	readDoltPort(cityPath)
	if store, code := openCityStore(stderr, "gc session logs"); store != nil {
		cityName := cfg.Workspace.Name
		if cityName == "" {
			cityName = filepath.Base(cityPath)
		}
		sn := lookupSessionNameOrLegacy(store, cityName, found.QualifiedName(), cfg.Workspace.SessionTemplate)
		if b, err := store.ListByLabel("agent:"+sn, 1); err == nil && len(b) > 0 {
			if sk := b[0].Metadata["session_key"]; sk != "" {
				path = sessionlog.FindSessionFileByID(searchPaths, workDir, sk)
			}
		}
	} else if code != 0 {
		// Store unavailable — fall through to work-dir lookup.
		_ = code
	}

	if path == "" {
		path = sessionlog.FindSessionFile(searchPaths, workDir)
	}
	if path == "" {
		fmt.Fprintf(stderr, "gc session logs: no session file found for %q\n", agentName) //nolint:errcheck // best-effort stderr
		return 1
	}

	return doSessionLogs(path, follow, tail, stdout, stderr)
}

func resolveSessionLogWorkDir(store beads.Store, identifier string) (string, bool) {
	if store == nil {
		return "", false
	}
	sessionID, err := resolveSessionIDAllowClosed(store, identifier)
	if err != nil {
		return "", false
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return "", false
	}
	workDir := strings.TrimSpace(b.Metadata["work_dir"])
	if workDir == "" {
		return "", false
	}
	return workDir, true
}

// resolveAgentWorkDir returns the absolute working directory for an agent,
// honoring work_dir template expansion.
func resolveAgentWorkDir(a config.Agent, cfg *config.City, cityPath string) string {
	cityName := filepath.Base(cityPath)
	if cfg != nil && cfg.Workspace.Name != "" {
		cityName = cfg.Workspace.Name
	}
	var rigs []config.Rig
	if cfg != nil {
		rigs = cfg.Rigs
	}
	return lookupConfiguredWorkDir(cityPath, cityName, &a, rigs)
}

// doSessionLogs reads the session file and prints messages. If follow is true,
// it polls for new messages every 2 seconds.
func doSessionLogs(path string, follow bool, tail int, stdout, stderr io.Writer) int {
	if tail < 0 {
		fmt.Fprintln(stderr, "gc session logs: --tail must be >= 0") //nolint:errcheck // best-effort stderr
		return 1
	}

	sess, err := sessionlog.ReadFile(path, tail)
	if err != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	seen := make(map[string]bool)
	for _, msg := range sess.Messages {
		printLogEntry(stdout, msg)
		seen[msg.UUID] = true
	}

	if !follow {
		return 0
	}

	// Seed 'seen' with ALL existing messages so the tail=0 re-reads in the
	// follow loop don't replay messages that were intentionally excluded by
	// the initial tail window.
	if tail > 0 {
		full, err := sessionlog.ReadFile(path, 0)
		if err == nil {
			for _, msg := range full.Messages {
				seen[msg.UUID] = true
			}
		}
	}

	// Follow mode: poll every 2 seconds for new messages.
	// Use tail=0 (all) for re-reads so compaction boundaries don't cause
	// missed messages. The seen map prevents re-printing.
	const maxConsecErrors = 5
	consecErrors := 0
	for {
		time.Sleep(2 * time.Second)

		sess, err = sessionlog.ReadFile(path, 0)
		if err != nil {
			consecErrors++
			if consecErrors >= maxConsecErrors {
				fmt.Fprintf(stderr, "gc session logs: %d consecutive read errors, last: %v\n", consecErrors, err) //nolint:errcheck // best-effort stderr
				return 1
			}
			continue
		}
		consecErrors = 0

		for _, msg := range sess.Messages {
			if seen[msg.UUID] {
				continue
			}
			printLogEntry(stdout, msg)
			seen[msg.UUID] = true
		}
	}
}

// resolveMessage handles both message formats found in Claude JSONL files:
// object format: {"role":"user","content":"hello"}
// string format: "{\"role\":\"user\",\"content\":\"hello\"}" (escaped JSON string)
// Returns the message content struct if parseable.
func resolveMessage(raw json.RawMessage) *sessionlog.MessageContent {
	if len(raw) == 0 {
		return nil
	}
	// Try object format first.
	var mc sessionlog.MessageContent
	if err := json.Unmarshal(raw, &mc); err == nil && mc.Role != "" {
		return &mc
	}
	// Try string format (JSON-encoded string containing the object).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if err := json.Unmarshal([]byte(s), &mc); err == nil && mc.Role != "" {
			return &mc
		}
	}
	return nil
}

// printLogEntry prints a single session log entry to stdout.
func printLogEntry(w io.Writer, e *sessionlog.Entry) {
	if e.IsCompactBoundary() {
		fmt.Fprintln(w, "── context compacted ──") //nolint:errcheck
		return
	}

	// Timestamp prefix.
	ts := ""
	if !e.Timestamp.IsZero() {
		ts = e.Timestamp.Format("15:04:05") + " "
	}

	// Type badge.
	typeStr := strings.ToUpper(e.Type)

	mc := resolveMessage(e.Message)
	if mc == nil {
		// Unparseable message — print raw truncated.
		if len(e.Message) > 0 {
			raw := string(e.Message)
			if len(raw) > 200 {
				raw = raw[:200] + "..."
			}
			fmt.Fprintf(w, "%s[%s] %s\n", ts, typeStr, raw) //nolint:errcheck
		}
		return
	}

	// Try content as plain string.
	var text string
	if json.Unmarshal(mc.Content, &text) == nil && text != "" {
		fmt.Fprintf(w, "%s[%s] %s\n", ts, typeStr, text) //nolint:errcheck
		return
	}

	// Try content as array of blocks.
	var blocks []sessionlog.ContentBlock
	if json.Unmarshal(mc.Content, &blocks) == nil && len(blocks) > 0 {
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					fmt.Fprintf(w, "%s[%s] %s\n", ts, typeStr, b.Text) //nolint:errcheck
				}
			case "tool_use":
				fmt.Fprintf(w, "%s[%s] tool_use: %s\n", ts, typeStr, b.Name) //nolint:errcheck
			case "tool_result":
				if b.IsError {
					fmt.Fprintf(w, "%s[%s] tool_result: error\n", ts, typeStr) //nolint:errcheck
				}
			}
		}
		return
	}

	// Fallback: print raw content truncated.
	raw := string(mc.Content)
	if len(raw) > 200 {
		raw = raw[:200] + "..."
	}
	fmt.Fprintf(w, "%s[%s] %s\n", ts, typeStr, raw) //nolint:errcheck
}
