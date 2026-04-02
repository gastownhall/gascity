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
		Use:   "logs <session>",
		Short: "Show session logs for a session",
		Long: `Show structured session log messages from a session's JSONL file.

Reads the session log, resolves the conversation DAG, and prints
messages in chronological order. Searches default paths (~/.claude/projects/)
and any extra paths from [daemon] observe_paths in city.toml.

Use --tail to control how many compaction segments to show (0 = all).
Use -f to follow new messages as they arrive.`,
		Example: `  gc session logs mayor
  gc session logs gc-123 --tail 0
  gc session logs s-gc-123 -f`,
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

// cmdSessionLogs is the CLI entry point for viewing session logs.
func cmdSessionLogs(args []string, follow bool, tail int, stdout, stderr io.Writer) int {
	identifier := args[0]

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

	var logCtx sessionLogContext
	var ok bool
	store, err := tryOpenCityStore()
	if err == nil && store != nil {
		logCtx, ok = resolveSessionLogContext(cityPath, cfg, store, identifier)
	}
	if !ok {
		workDir, found := resolveConfiguredSessionLogContext(cityPath, cfg, identifier)
		if !found {
			fmt.Fprintf(stderr, "gc session logs: session %q not found\n", identifier) //nolint:errcheck // best-effort stderr
			return 1
		}
		logCtx.workDir = workDir
	}

	searchPaths := sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)
	var path string
	if logCtx.sessionKey != "" {
		path = sessionlog.FindSessionFileByID(searchPaths, logCtx.workDir, logCtx.sessionKey)
	}
	if path == "" {
		path = sessionlog.FindSessionFileForProvider(searchPaths, logCtx.provider, logCtx.workDir)
	}
	if path == "" {
		fmt.Fprintf(stderr, "gc session logs: no session file found for %q\n", identifier) //nolint:errcheck // best-effort stderr
		return 1
	}

	return doSessionLogs(path, logCtx.provider, follow, tail, stdout, stderr)
}

type sessionLogContext struct {
	workDir    string
	sessionKey string
	provider   string
}

func resolveSessionLogContext(cityPath string, cfg *config.City, store beads.Store, identifier string) (sessionLogContext, bool) {
	if store == nil {
		return sessionLogContext{}, false
	}
	sessionID, err := resolveSessionIDAllowClosedWithConfig(cityPath, cfg, store, identifier)
	if err != nil {
		return sessionLogContext{}, false
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return sessionLogContext{}, false
	}
	workDir := strings.TrimSpace(b.Metadata["work_dir"])
	if workDir == "" {
		return sessionLogContext{}, false
	}
	return sessionLogContext{
		workDir:    workDir,
		sessionKey: strings.TrimSpace(b.Metadata["session_key"]),
		provider:   strings.TrimSpace(b.Metadata["provider"]),
	}, true
}

func resolveConfiguredSessionLogContext(cityPath string, cfg *config.City, identifier string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	identifier = normalizeNamedSessionTarget(identifier)
	if identifier == "" {
		return "", false
	}
	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	if spec, ok, _ := findNamedSessionSpecForTarget(cfg, cityName, nil, identifier); ok && spec.Agent != nil {
		workDir, err := resolveWorkDir(cityPath, cfg, spec.Agent)
		if err != nil || strings.TrimSpace(workDir) == "" {
			return "", false
		}
		return workDir, true
	}
	for i := range cfg.Agents {
		agentCfg := cfg.Agents[i]
		if isMultiSessionCfgAgent(&agentCfg) || strings.TrimSpace(agentCfg.QualifiedName()) != identifier {
			continue
		}
		workDir, err := resolveWorkDir(cityPath, cfg, &agentCfg)
		if err != nil || strings.TrimSpace(workDir) == "" {
			return "", false
		}
		return workDir, true
	}
	return "", false
}

// doSessionLogs reads the session file and prints messages. If follow is true,
// it polls for new messages every 2 seconds.
func doSessionLogs(path, provider string, follow bool, tail int, stdout, stderr io.Writer) int {
	if tail < 0 {
		fmt.Fprintln(stderr, "gc session logs: --tail must be >= 0") //nolint:errcheck // best-effort stderr
		return 1
	}

	sess, readErr := sessionlog.ReadProviderFile(provider, path, tail)
	if readErr != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", readErr) //nolint:errcheck // best-effort stderr
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
		full, err := readSessionFile(provider, path, 0)
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

		sess, readErr = readSessionFile(provider, path, 0)
		if readErr != nil {
			consecErrors++
			if consecErrors >= maxConsecErrors {
				fmt.Fprintf(stderr, "gc session logs: %d consecutive read errors, last: %v\n", consecErrors, readErr) //nolint:errcheck // best-effort stderr
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

func readSessionFile(provider, path string, tail int) (*sessionlog.Session, error) {
	return sessionlog.ReadProviderFile(provider, path, tail)
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
