package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
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

Use --tail to print only the last N transcript entries (0 = all).
Semantics match Unix 'tail -n': '--tail 5' prints the final 5 entries,
not the first 5. A single assistant turn with multiple tool-use blocks
still counts as one entry. Compact-boundary dividers count as entries
when they fall inside the final window.

Compatibility note: before 1.0, --tail mapped to compaction segments.
As of 1.0, --tail trims the displayed transcript entry window instead.
The HTTP API's tail query parameter still uses compaction-segment
semantics.
Use -f to follow new messages as they arrive.`,
		Example: `  gc session logs mayor
  gc session logs mayor --tail 2
  gc session logs gc-123 --tail 20
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
	cmd.Flags().IntVar(&tail, "tail", 10, "Number of most recent transcript entries to show (0 = all; compact dividers count as entries)")
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
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	searchPaths := worker.MergeSearchPaths(cfg.Daemon.ObservePaths)

	store, err := tryOpenCityStore()
	var (
		path     string
		provider string
		ok       bool
	)
	if err == nil && store != nil {
		path, provider, ok = resolveStoredSessionLogSource(cityPath, cfg, store, identifier, searchPaths)
	}
	if !ok {
		workDir, found := resolveConfiguredSessionLogContext(cityPath, cfg, identifier)
		if !found {
			fmt.Fprintf(stderr, "gc session logs: session %q not found\n", identifier) //nolint:errcheck // best-effort stderr
			return 1
		}
		path = resolveSessionLogPath(searchPaths, sessionLogContext{workDir: workDir})
	}
	if path == "" {
		fmt.Fprintf(stderr, "gc session logs: no session file found for %q\n", identifier) //nolint:errcheck // best-effort stderr
		return 1
	}

	return doSessionLogs(path, provider, follow, tail, stdout, stderr)
}

func resolveSessionLogPath(searchPaths []string, logCtx sessionLogContext) string {
	factory, err := worker.NewFactory(worker.FactoryConfig{SearchPaths: searchPaths})
	if err != nil {
		return ""
	}
	return factory.DiscoverTranscript(logCtx.provider, logCtx.workDir, logCtx.sessionKey)
}

func resolveStoredSessionLogSource(cityPath string, cfg *config.City, store beads.Store, identifier string, searchPaths []string) (string, string, bool) {
	logCtx, ok := resolveSessionLogContext(cityPath, cfg, store, identifier)
	if !ok {
		return "", "", false
	}
	if logCtx.sessionID != "" {
		handle, err := workerHandleForSessionWithConfig(cityPath, store, newSessionProvider(), cfg, logCtx.sessionID)
		if err == nil {
			if path, pathErr := handle.TranscriptPath(context.Background()); pathErr == nil && strings.TrimSpace(path) != "" {
				return path, logCtx.provider, true
			}
		}
	}
	path := ""
	fallbackAllowed := canFallbackStoredSessionLogByWorkDir(store, logCtx)
	if strings.TrimSpace(logCtx.sessionKey) != "" {
		path = resolveSessionKeyedLogPath(searchPaths, logCtx)
		if path == "" && fallbackAllowed {
			path = resolveSessionLogPath(searchPaths, logCtx)
		}
	} else if fallbackAllowed {
		path = resolveSessionLogPath(searchPaths, logCtx)
	}
	if !sessionLogPathFreshEnough(path, logCtx.createdAt) {
		path = ""
	}
	if path == "" && fallbackAllowed {
		factory, err := worker.NewFactory(worker.FactoryConfig{SearchPaths: searchPaths})
		if err == nil {
			path = factory.DiscoverWorkDirTranscript(logCtx.provider, logCtx.workDir)
		}
	}
	if !sessionLogPathFreshEnough(path, logCtx.createdAt) {
		path = ""
	}
	return path, logCtx.provider, true
}

func resolveSessionKeyedLogPath(searchPaths []string, logCtx sessionLogContext) string {
	return workertranscript.DiscoverKeyedPath(searchPaths, logCtx.provider, logCtx.workDir, logCtx.sessionKey)
}

type sessionLogContext struct {
	sessionID  string
	workDir    string
	sessionKey string
	provider   string
	createdAt  time.Time
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
	provider := strings.TrimSpace(b.Metadata["provider_kind"])
	if provider == "" {
		provider = strings.TrimSpace(b.Metadata["provider"])
	}
	return sessionLogContext{
		sessionID:  sessionID,
		workDir:    workDir,
		sessionKey: strings.TrimSpace(b.Metadata["session_key"]),
		provider:   provider,
		createdAt:  b.CreatedAt,
	}, true
}

func sessionLogPathFreshEnough(path string, sessionCreatedAt time.Time) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	if sessionCreatedAt.IsZero() {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.ModTime().Before(sessionCreatedAt.Add(-2 * time.Second))
}

func canFallbackStoredSessionLogByWorkDir(store beads.Store, logCtx sessionLogContext) bool {
	if store == nil || strings.TrimSpace(logCtx.sessionID) == "" || strings.TrimSpace(logCtx.workDir) == "" {
		return false
	}
	all, err := sessionLogFallbackCandidates(store, logCtx.workDir, logCtx.provider)
	if err != nil {
		return false
	}
	targetLive := false
	for _, b := range all {
		if b.ID == logCtx.sessionID {
			targetLive = sessionLogFallbackCandidateLive(b)
			break
		}
	}
	matches := 0
	for _, b := range all {
		if !sessionpkg.IsSessionBeadOrRepairable(b) {
			continue
		}
		if strings.TrimSpace(b.Metadata["work_dir"]) != logCtx.workDir {
			continue
		}
		provider := strings.TrimSpace(b.Metadata["provider_kind"])
		if provider == "" {
			provider = strings.TrimSpace(b.Metadata["provider"])
		}
		if logCtx.provider != "" && provider != "" && provider != logCtx.provider {
			continue
		}
		if targetLive && b.ID != logCtx.sessionID && !sessionLogFallbackCandidateLive(b) {
			continue
		}
		matches++
		if matches > 1 {
			return false
		}
	}
	return matches == 1
}

func sessionLogFallbackCandidates(store beads.Store, workDir, provider string) ([]beads.Bead, error) {
	candidates := make(map[string]beads.Bead)
	add := func(filters map[string]string) error {
		found, err := store.ListByMetadata(filters, 0)
		if err != nil {
			return err
		}
		for _, b := range found {
			candidates[b.ID] = b
		}
		return nil
	}
	if strings.TrimSpace(provider) == "" {
		if err := add(map[string]string{"work_dir": workDir}); err != nil {
			return nil, err
		}
	} else {
		if err := add(map[string]string{"work_dir": workDir, "provider": provider}); err != nil {
			return nil, err
		}
		if err := add(map[string]string{"work_dir": workDir, "provider_kind": provider}); err != nil {
			return nil, err
		}
	}
	out := make([]beads.Bead, 0, len(candidates))
	for _, b := range candidates {
		out = append(out, b)
	}
	return out, nil
}

func sessionLogFallbackCandidateLive(b beads.Bead) bool {
	if b.Status == "closed" {
		return false
	}
	switch sessionpkg.State(strings.TrimSpace(b.Metadata["state"])) {
	case sessionpkg.StateActive, sessionpkg.StateAwake, sessionpkg.StateCreating, sessionpkg.StateDraining:
		return true
	default:
		return false
	}
}

func resolveConfiguredSessionLogContext(cityPath string, cfg *config.City, identifier string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	identifier = normalizeNamedSessionTarget(identifier)
	if identifier == "" {
		return "", false
	}
	cityName := loadedCityName(cfg, cityPath)
	if spec, ok, _ := findNamedSessionSpecForTarget(cfg, cityName, identifier); ok && spec.Agent != nil {
		workDirQualifiedName := workdirutil.SessionQualifiedName(cityPath, *spec.Agent, cfg.Rigs, spec.Identity, "")
		workDir, err := resolveWorkDirForQualifiedName(cityPath, cfg, spec.Agent, workDirQualifiedName)
		if err != nil || strings.TrimSpace(workDir) == "" {
			return "", false
		}
		return workDir, true
	}
	for i := range cfg.Agents {
		agentCfg := cfg.Agents[i]
		if agentCfg.SupportsInstanceExpansion() || strings.TrimSpace(agentCfg.QualifiedName()) != identifier {
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
//
// The tail parameter specifies how many of the most recent log entries to
// print (0 = all). Semantics match the conventional Unix `tail -n` flag:
// `--tail 5` prints the LAST 5 entries of the transcript, not the first 5.
func doSessionLogs(path, provider string, follow bool, tail int, stdout, stderr io.Writer) int {
	if tail < 0 {
		fmt.Fprintln(stderr, "gc session logs: --tail must be >= 0") //nolint:errcheck // best-effort stderr
		return 1
	}
	factory, err := worker.NewFactory(worker.FactoryConfig{})
	if err != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	return runSessionLogs(factory, provider, path, follow, tail, stdout, stderr, time.Sleep, readSessionFile)
}

type sessionLogsReader func(factory *worker.Factory, provider, path string) (*worker.TranscriptSession, error)

func runSessionLogs(factory *worker.Factory, provider, path string, follow bool, tail int, stdout, stderr io.Writer, sleep func(time.Duration), read sessionLogsReader) int {
	// Always read the full session; apply tail trimming locally so semantics
	// are a true "last N entries" window regardless of compaction boundaries.
	sess, readErr := read(factory, provider, path)
	if readErr != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", readErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	seen := make(map[string]bool)
	// Seed 'seen' with ALL existing messages so that any entries trimmed by
	// --tail do not get replayed on subsequent follow-mode re-reads, and so
	// follow-mode only surfaces entries that arrive AFTER the initial snapshot.
	for _, msg := range sess.Messages {
		seen[msg.UUID] = true
	}

	for _, msg := range tailMessages(sess.Messages, tail) {
		printLogEntry(stdout, msg)
	}

	if !follow {
		return 0
	}

	// Follow mode: poll every 2 seconds for new messages.
	// Use tail=0 (all) for re-reads so compaction boundaries don't cause
	// missed messages. The seen map prevents re-printing.
	const maxConsecErrors = 5
	consecErrors := 0
	for {
		sleep(2 * time.Second)

		sess, readErr = read(factory, provider, path)
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

func readSessionFile(factory *worker.Factory, provider, path string) (*worker.TranscriptSession, error) {
	result, err := factory.ReadTranscript(worker.TranscriptRequest{
		Provider:        provider,
		TranscriptPath:  path,
		TailCompactions: 0,
	})
	if err != nil {
		return nil, err
	}
	return result.Session, nil
}

// tailMessages returns the last n entries of msgs. When n <= 0 the full slice
// is returned unchanged. When n >= len(msgs) the full slice is also returned.
// Implements the --tail semantics: "last N log entries" (matches Unix
// `tail -n` rather than "N compaction segments").
func tailMessages(msgs []*worker.TranscriptEntry, n int) []*worker.TranscriptEntry {
	if n <= 0 || n >= len(msgs) {
		return msgs
	}
	return msgs[len(msgs)-n:]
}

// resolveMessage handles both message formats found in Claude JSONL files:
// object format: {"role":"user","content":"hello"}
// string format: "{\"role\":\"user\",\"content\":\"hello\"}" (escaped JSON string)
// Returns the message content struct if parseable.
func resolveMessage(raw json.RawMessage) *worker.TranscriptMessageContent {
	if len(raw) == 0 {
		return nil
	}
	// Try object format first.
	var mc worker.TranscriptMessageContent
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
func printLogEntry(w io.Writer, e *worker.TranscriptEntry) {
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
		// Unparseable message; print raw truncated.
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
	var blocks []worker.TranscriptContentBlock
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
