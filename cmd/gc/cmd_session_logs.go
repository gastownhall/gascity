package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
	var jsonOutput bool
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
			if cmdSessionLogs(args, follow, tail, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow new messages as they arrive")
	cmd.Flags().IntVar(&tail, "tail", 10, "Number of most recent transcript entries to show (0 = all; compact dividers count as entries)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL result for the bounded snapshot")
	return cmd
}

// cmdSessionLogs is the CLI entry point for viewing session logs.
func cmdSessionLogs(args []string, follow bool, tail int, jsonOutput bool, stdout, stderr io.Writer) int {
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
		var diagnostic string
		path, provider, ok, diagnostic = resolveStoredSessionLogSource(cityPath, cfg, cliSessionFrontDoor(store, cfg, cityPath), identifier, searchPaths)
		if ok && path == "" && diagnostic != "" {
			fmt.Fprintf(stderr, "gc session logs: %s\n", diagnostic) //nolint:errcheck // best-effort stderr
			return 1
		}
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

	if jsonOutput {
		return doSessionLogsJSON(path, provider, identifier, follow, tail, stdout, stderr)
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

func sessionLogFallbackCandidateLive(info sessionpkg.Info) bool {
	if info.Closed {
		return false
	}
	switch sessionpkg.State(strings.TrimSpace(info.MetadataState)) {
	case sessionpkg.StateActive, sessionpkg.StateAwake, sessionpkg.StateStartPending, sessionpkg.StateCreating, sessionpkg.StateDraining:
		return true
	default:
		return false
	}
}

func ambiguousSessionLogDiagnostic(logCtx sessionLogContext) string {
	sessionID := strings.TrimSpace(logCtx.sessionID)
	if sessionID == "" {
		sessionID = "requested session"
	}
	provider := strings.TrimSpace(logCtx.provider)
	if provider == "" {
		provider = "provider"
	}
	if strings.TrimSpace(logCtx.sessionKey) == "" {
		return fmt.Sprintf("session %q has no session_key and workdir fallback is ambiguous for %s work_dir %q", sessionID, provider, logCtx.workDir)
	}
	return fmt.Sprintf("no exact transcript found for session %q and workdir fallback is ambiguous for %s work_dir %q", sessionID, provider, logCtx.workDir)
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

type sessionLogsJSONResult struct {
	SchemaVersion  string                `json:"schema_version"`
	Target         string                `json:"target"`
	Provider       string                `json:"provider,omitempty"`
	TranscriptPath string                `json:"transcript_path"`
	Tail           int                   `json:"tail"`
	EntryCount     int                   `json:"entry_count"`
	Entries        []sessionLogEntryJSON `json:"entries"`
}

type sessionLogEntryJSON struct {
	UUID            string                       `json:"uuid,omitempty"`
	ParentUUID      string                       `json:"parent_uuid,omitempty"`
	Type            string                       `json:"type"`
	Subtype         string                       `json:"subtype,omitempty"`
	Role            string                       `json:"role,omitempty"`
	Timestamp       string                       `json:"timestamp,omitempty"`
	CompactBoundary bool                         `json:"compact_boundary,omitempty"`
	Text            string                       `json:"text,omitempty"`
	Blocks          []transcriptContentBlockJSON `json:"blocks,omitempty"`
	Message         json.RawMessage              `json:"message,omitempty"`
}

type transcriptContentBlockJSON struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	State     string          `json:"state,omitempty"`
	Text      string          `json:"text,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	Options   []string        `json:"options,omitempty"`
	Action    string          `json:"action,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func doSessionLogsJSON(path, provider, target string, follow bool, tail int, stdout, stderr io.Writer) int {
	if follow {
		fmt.Fprintln(stderr, "gc session logs: --json is only supported for bounded snapshots; omit --follow") //nolint:errcheck // best-effort stderr
		return 1
	}
	if tail < 0 {
		fmt.Fprintln(stderr, "gc session logs: --tail must be >= 0") //nolint:errcheck // best-effort stderr
		return 1
	}
	factory, err := worker.NewFactory(worker.FactoryConfig{})
	if err != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return runSessionLogsJSON(factory, provider, path, target, tail, stdout, stderr, readSessionFile)
}

func runSessionLogsJSON(factory *worker.Factory, provider, path, target string, tail int, stdout, stderr io.Writer, read sessionLogsReader) int {
	sess, readErr := read(factory, provider, path)
	if readErr != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", readErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	messages := tailMessages(sess.Messages, tail)
	entries := make([]sessionLogEntryJSON, 0, len(messages))
	for _, msg := range messages {
		entries = append(entries, sessionLogEntryToJSON(msg))
	}
	if err := writeCLIJSONLine(stdout, sessionLogsJSONResult{
		SchemaVersion:  "1",
		Target:         target,
		Provider:       provider,
		TranscriptPath: path,
		Tail:           tail,
		EntryCount:     len(entries),
		Entries:        entries,
	}); err != nil {
		fmt.Fprintf(stderr, "gc session logs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func sessionLogEntryToJSON(e *worker.TranscriptEntry) sessionLogEntryJSON {
	if e == nil {
		return sessionLogEntryJSON{}
	}
	entry := sessionLogEntryJSON{
		UUID:            e.UUID,
		ParentUUID:      e.ParentUUID,
		Type:            e.Type,
		Subtype:         e.Subtype,
		CompactBoundary: e.IsCompactBoundary(),
	}
	if !e.Timestamp.IsZero() {
		entry.Timestamp = e.Timestamp.Format(time.RFC3339Nano)
	}
	if len(e.Message) > 0 {
		entry.Message = e.Message
	}
	mc := resolveMessage(e.Message)
	if mc == nil {
		return entry
	}
	entry.Role = mc.Role
	var text string
	if json.Unmarshal(mc.Content, &text) == nil {
		entry.Text = text
		return entry
	}
	var blocks []worker.TranscriptContentBlock
	if json.Unmarshal(mc.Content, &blocks) == nil && len(blocks) > 0 {
		entry.Blocks = transcriptContentBlocksToJSON(blocks)
	}
	return entry
}

func transcriptContentBlocksToJSON(blocks []worker.TranscriptContentBlock) []transcriptContentBlockJSON {
	out := make([]transcriptContentBlockJSON, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, transcriptContentBlockJSON{
			Type:      block.Type,
			ID:        block.ID,
			RequestID: block.RequestID,
			Kind:      block.Kind,
			State:     block.State,
			Text:      block.Text,
			Prompt:    block.Prompt,
			Options:   block.Options,
			Action:    block.Action,
			Metadata:  block.Metadata,
			Name:      block.Name,
			Input:     block.Input,
			ToolUseID: block.ToolUseID,
			Content:   block.Content,
			IsError:   block.IsError,
		})
	}
	return out
}

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
		printed := false
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					fmt.Fprintf(w, "%s[%s] %s\n", ts, typeStr, b.Text) //nolint:errcheck
					printed = true
				}
			case "tool_use":
				fmt.Fprintf(w, "%s[%s] tool_use: %s\n", ts, typeStr, b.Name) //nolint:errcheck
				printed = true
			case "tool_result":
				if b.IsError {
					fmt.Fprintf(w, "%s[%s] tool_result: error\n", ts, typeStr) //nolint:errcheck
				} else {
					fmt.Fprintf(w, "%s[%s] tool_result: ok\n", ts, typeStr) //nolint:errcheck
				}
				printed = true
			}
		}
		// Invariant: every transcript entry that occupies a tail slot renders at
		// least one line. Without this, a `--tail N` window landing on entries
		// whose blocks are all non-rendering (empty text, thinking, or any block
		// type not handled above) yields empty stdout with exit 0 -- the source
		// of the "session logs --tail N output is empty" flake.
		if !printed {
			fmt.Fprintf(w, "%s[%s] (no displayable content)\n", ts, typeStr) //nolint:errcheck
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
