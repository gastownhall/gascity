package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// envExternalOrigin is the env var the controller sets when nudging a session
// for an externally-originated message. Its value is a JSON-encoded
// externalOriginEnvelope carrying the ConversationRef the session should reply to.
//
// This is per-invocation session-context data, not a rollout/capability knob —
// it carries no on/off or mode semantic and selects no code path, so it does
// not belong in internal/rollout (rollout.Spec has no shape for it; see
// rollout.Spec.SelectsBetween). Same pattern as GC_EXEC_INFO
// (internal/clientauth) and GC_GRANT_INFO (internal/clientgrant): structured
// per-call data handed to a fresh process via env because there is no other
// side channel. Design ruling: ga-wsfbqe.
const envExternalOrigin = "GC_EXTERNAL_ORIGIN"

// externalOriginEnvelope mirrors extmsg.ExternalOriginEnvelope for JSON
// decoding without importing internal/extmsg. Field names are PascalCase
// because the source struct has no json tags (Go default encoding).
type externalOriginEnvelope struct {
	Conversation genclient.ConversationRef `json:"Conversation"`
}

type extmsgReplyJSONResult struct {
	Delivered      bool   `json:"delivered"`
	ConversationID string `json:"conversation_id"`
	Sequence       int64  `json:"sequence"`
}

// extmsgReplyStdin is the stdin reader for --stdin. Indirected for tests.
var extmsgReplyStdin = func() io.Reader { return os.Stdin }

// extmsgReplyAPIClient is indirected for tests — they inject a client pointed
// at an httptest.Server or nil to force error paths.
var extmsgReplyAPIClient = apiClient

// newExtMsgCmd groups the external-conversation binding verbs. These are
// thin projections over the extmsg binding service via the city API —
// there is deliberately no local fallback: conversation bindings are live
// controller state, and mutating the bead store behind a running
// controller's back would race its delivery pipeline.
func newExtMsgCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "extmsg",
		Short: "Manage external-conversation bindings",
		Long: `Manage bindings between external conversations (telegram, discord, ...)
and gc sessions or configured agents.

A conversation bound to an agent name survives session restarts: inbound
messages resolve a live session for the agent at delivery time, cold-waking
one when none is live. "handoff" rebinds a conversation to another agent —
the front-desk pattern: a default-routed agent inspects the conversation
and hands it to the right specialist.

These commands require the city API server; they have no local fallback.`,
	}
	cmd.AddCommand(newExtMsgBindCmd(stdout, stderr))
	cmd.AddCommand(newExtMsgHandoffCmd(stdout, stderr))
	cmd.AddCommand(newExtMsgUnbindCmd(stdout, stderr))
	cmd.AddCommand(newExtmsgReplyCmd(stdout, stderr))
	return cmd
}

// extMsgConversationFlags collects the flags identifying one conversation.
type extMsgConversationFlags struct {
	scopeID              string
	provider             string
	accountID            string
	conversationID       string
	parentConversationID string
	kind                 string
}

func addExtMsgConversationFlags(cmd *cobra.Command, f *extMsgConversationFlags) {
	cmd.Flags().StringVar(&f.provider, "provider", "", "External messaging provider (required)")
	cmd.Flags().StringVar(&f.conversationID, "conversation-id", "", "Provider conversation ID (required)")
	cmd.Flags().StringVar(&f.accountID, "account-id", "default", "Adapter account ID")
	cmd.Flags().StringVar(&f.scopeID, "scope-id", "", "Conversation scope (default: the city name)")
	cmd.Flags().StringVar(&f.parentConversationID, "parent-conversation-id", "", "Parent conversation ID for thread conversations")
	cmd.Flags().StringVar(&f.kind, "kind", "dm", "Conversation kind: dm, room, or thread")
}

// conversationRef materializes the flags into a ConversationRef, defaulting
// the scope to the city name (the convention bridges stamp on inbound).
func (f *extMsgConversationFlags) conversationRef(cityPath string) (extmsg.ConversationRef, error) {
	if strings.TrimSpace(f.provider) == "" {
		return extmsg.ConversationRef{}, fmt.Errorf("--provider is required")
	}
	if strings.TrimSpace(f.conversationID) == "" {
		return extmsg.ConversationRef{}, fmt.Errorf("--conversation-id is required")
	}
	scope := strings.TrimSpace(f.scopeID)
	if scope == "" {
		cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			return extmsg.ConversationRef{}, fmt.Errorf("resolving default scope from city config: %w", err)
		}
		scope = loadedCityName(cfg, cityPath)
	}
	return extmsg.ConversationRef{
		ScopeID:              scope,
		Provider:             f.provider,
		AccountID:            f.accountID,
		ConversationID:       f.conversationID,
		ParentConversationID: f.parentConversationID,
		Kind:                 extmsg.ConversationKind(f.kind),
	}, nil
}

// conversationRefIfSet returns nil when no conversation flags were given,
// letting unbind filter purely by session/agent.
func (f *extMsgConversationFlags) conversationRefIfSet(cityPath string) (*extmsg.ConversationRef, error) {
	if strings.TrimSpace(f.provider) == "" && strings.TrimSpace(f.conversationID) == "" {
		return nil, nil
	}
	ref, err := f.conversationRef(cityPath)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// extMsgClient resolves the city and returns its API client, failing with
// a uniform message when the API is unavailable.
func extMsgClient(verb string, stderr io.Writer) (*api.Client, string, bool) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg %s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return nil, "", false
	}
	c := apiClient(cityPath)
	if c == nil {
		fmt.Fprintf(stderr, "gc extmsg %s: requires the city API server (no local fallback for conversation bindings)\n", verb) //nolint:errcheck // best-effort stderr
		return nil, "", false
	}
	return c, cityPath, true
}

func extMsgReportBindError(verb string, c *api.Client, err error, stderr io.Writer) int {
	if api.ShouldFallback(c, err) {
		fmt.Fprintf(stderr, "gc extmsg %s: city API unreachable (no local fallback for conversation bindings): %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stderr, "gc extmsg %s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
	return 1
}

func printExtMsgBinding(stdout io.Writer, jsonOutput bool, record extmsg.SessionBindingRecord, action string) int {
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(record); err != nil {
			return 1
		}
		return 0
	}
	target := record.SessionID
	if record.AgentName != "" {
		target = "agent " + record.AgentName
	}
	fmt.Fprintf(stdout, "%s %s/%s -> %s (binding %s, generation %d)\n", //nolint:errcheck // best-effort stdout
		action, record.Conversation.Provider, record.Conversation.ConversationID, target, record.ID, record.BindingGeneration)
	return 0
}

func newExtMsgBindCmd(stdout, stderr io.Writer) *cobra.Command {
	var conv extMsgConversationFlags
	var agentName, sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "bind",
		Short: "Bind a conversation to a session or configured agent",
		Long: `Bind an external conversation to a concrete session (--session) or to a
configured agent (--agent). Agent bindings survive session restarts:
delivery resolves a live session for the agent each time, cold-waking one
when none is live. Binding an actively-bound conversation conflicts; use
"gc extmsg handoff" to rebind.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdExtMsgBind(conv, agentName, sessionID, false, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	addExtMsgConversationFlags(cmd, &conv)
	cmd.Flags().StringVar(&agentName, "agent", "", "Configured agent identity to bind (mutually exclusive with --session)")
	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID to bind (mutually exclusive with --agent)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the binding record as JSON")
	return cmd
}

func newExtMsgHandoffCmd(stdout, stderr io.Writer) *cobra.Command {
	var conv extMsgConversationFlags
	var to string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Rebind a conversation to another configured agent",
		Long: `Rebind an external conversation to another configured agent, replacing
the active binding. Run from inside an agent session to hand a
conversation to the right specialist — the routing judgment lives in the
agent's prompt, this verb is pure transport.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(to) == "" {
				fmt.Fprintln(stderr, "gc extmsg handoff: --to is required") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdExtMsgBind(conv, to, "", true, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	addExtMsgConversationFlags(cmd, &conv)
	cmd.Flags().StringVar(&to, "to", "", "Configured agent identity to hand the conversation to (required)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the binding record as JSON")
	return cmd
}

// cmdExtMsgBind backs both bind (replace=false) and handoff (replace=true).
func cmdExtMsgBind(conv extMsgConversationFlags, agentName, sessionID string, replace, jsonOutput bool, stdout, stderr io.Writer) int {
	verb := "bind"
	action := "bound"
	if replace {
		verb = "handoff"
		action = "handed off"
	}
	agentName = strings.TrimSpace(agentName)
	sessionID = strings.TrimSpace(sessionID)
	switch {
	case agentName == "" && sessionID == "":
		fmt.Fprintf(stderr, "gc extmsg %s: --agent or --session is required\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	case agentName != "" && sessionID != "":
		fmt.Fprintf(stderr, "gc extmsg %s: --agent and --session are mutually exclusive\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	}
	c, cityPath, ok := extMsgClient(verb, stderr)
	if !ok {
		return 1
	}
	ref, err := conv.conversationRef(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg %s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	record, err := c.BindExtMsgConversation(api.ExtMsgBindSpec{
		Conversation: ref,
		SessionID:    sessionID,
		AgentName:    agentName,
		Replace:      replace,
	})
	if err != nil {
		return extMsgReportBindError(verb, c, err, stderr)
	}
	return printExtMsgBinding(stdout, jsonOutput, record, action)
}

func newExtMsgUnbindCmd(stdout, stderr io.Writer) *cobra.Command {
	var conv extMsgConversationFlags
	var agentName, sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "unbind",
		Short: "Remove active conversation bindings",
		Long: `Remove active external-conversation bindings. Filter by conversation
(--provider/--conversation-id), by --agent, by --session, or a
combination. At least one filter is required.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdExtMsgUnbind(conv, agentName, sessionID, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	addExtMsgConversationFlags(cmd, &conv)
	cmd.Flags().StringVar(&agentName, "agent", "", "Unbind conversations bound to this configured agent")
	cmd.Flags().StringVar(&sessionID, "session", "", "Unbind conversations bound to this session ID")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the removed binding records as JSON")
	return cmd
}

func cmdExtMsgUnbind(conv extMsgConversationFlags, agentName, sessionID string, jsonOutput bool, stdout, stderr io.Writer) int {
	agentName = strings.TrimSpace(agentName)
	sessionID = strings.TrimSpace(sessionID)
	c, cityPath, ok := extMsgClient("unbind", stderr)
	if !ok {
		return 1
	}
	ref, err := conv.conversationRefIfSet(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg unbind: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if ref == nil && agentName == "" && sessionID == "" {
		fmt.Fprintln(stderr, "gc extmsg unbind: a conversation (--provider/--conversation-id), --agent, or --session is required") //nolint:errcheck // best-effort stderr
		return 1
	}
	unbound, err := c.UnbindExtMsgConversation(ref, sessionID, agentName)
	if err != nil {
		return extMsgReportBindError("unbind", c, err, stderr)
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(unbound); err != nil {
			return 1
		}
		return 0
	}
	if len(unbound) == 0 {
		fmt.Fprintln(stdout, "No active bindings matched.") //nolint:errcheck // best-effort stdout
		return 0
	}
	for _, record := range unbound {
		target := record.SessionID
		if record.AgentName != "" {
			target = "agent " + record.AgentName
		}
		fmt.Fprintf(stdout, "unbound %s/%s from %s (binding %s)\n", //nolint:errcheck // best-effort stdout
			record.Conversation.Provider, record.Conversation.ConversationID, target, record.ID)
	}
	return 0
}

// newExtmsgReplyCmd returns the "gc extmsg reply" subcommand.
func newExtmsgReplyCmd(stdout, stderr io.Writer) *cobra.Command {
	var refJSON string
	var fromStdin bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "reply [flags] [text]",
		Short: "Reply to an external client over the outbound SSE channel",
		Long: `Reply to an external client over the outbound SSE channel.

When no --ref is given, the ConversationRef is read from GC_EXTERNAL_ORIGIN,
which the controller sets when nudging a session for an externally-originated
message. If GC_EXTERNAL_ORIGIN is not set, the command exits non-zero.`,
		Example: `  gc extmsg reply "one-liner response"
  gc extmsg reply --stdin <<'EOF'
  multi-line response
  EOF
  gc extmsg reply --ref '{"provider":"llm-client","account_id":"c1","conversation_id":"cv1","kind":"direct"}' "hello"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc extmsg reply: %v\n", err) //nolint:errcheck
				return errExit
			}
			c := extmsgReplyAPIClient(cityPath)
			if c == nil {
				fmt.Fprintln(stderr, "gc extmsg reply: no API server available (is the controller running?)") //nolint:errcheck
				return errExit
			}
			if runExtmsgReply(c, refJSON, fromStdin, args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&refJSON, "ref", "", "Explicit ConversationRef JSON; overrides session context")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "Read reply text from stdin instead of text argument")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output delivery status as JSON")

	return cmd
}

// runExtmsgReply is the core logic for gc extmsg reply, extracted for
// testability. refJSON is "" to read from GC_EXTERNAL_ORIGIN.
func runExtmsgReply(c *api.Client, refJSON string, fromStdin bool, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if fromStdin && len(args) > 0 {
		fmt.Fprintln(stderr, "gc extmsg reply: --stdin and positional text argument are mutually exclusive") //nolint:errcheck
		return 1
	}

	var text string
	if fromStdin {
		data, err := io.ReadAll(extmsgReplyStdin())
		if err != nil {
			fmt.Fprintf(stderr, "gc extmsg reply: reading stdin: %v\n", err) //nolint:errcheck
			return 1
		}
		text = string(data)
	} else if len(args) > 0 {
		text = args[0]
	}

	conv, err := resolveExtmsgConv(refJSON)
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg reply: %v\n", err) //nolint:errcheck
		return 1
	}

	sessionID := os.Getenv("GC_SESSION_ID")
	if sessionID == "" {
		fmt.Fprintln(stderr, "gc extmsg reply: GC_SESSION_ID not set; command must be called from within a running session") //nolint:errcheck
		return 1
	}

	result, err := c.ExtMsgOutbound(context.Background(), sessionID, conv, text)
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg reply: error: %v\n", err) //nolint:errcheck
		return 1
	}

	if jsonOutput {
		out := extmsgReplyJSONResult{
			Delivered:      result.Delivered,
			ConversationID: result.ConversationID,
			Sequence:       result.Sequence,
		}
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			fmt.Fprintf(stderr, "gc extmsg reply: encoding JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}

	if result.Delivered {
		fmt.Fprintf(stdout, "delivered %s seq:%d\n", result.ConversationID, result.Sequence) //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "no-subscriber %s (transcript appended; client not connected)\n", result.ConversationID) //nolint:errcheck
	}
	return 0
}

// resolveExtmsgConv resolves the ConversationRef for gc extmsg reply:
// if refJSON is non-empty, unmarshal it directly; otherwise decode the
// ExternalOriginEnvelope from GC_EXTERNAL_ORIGIN.
func resolveExtmsgConv(refJSON string) (genclient.ConversationRef, error) {
	if refJSON != "" {
		var conv genclient.ConversationRef
		if err := json.Unmarshal([]byte(refJSON), &conv); err != nil {
			return genclient.ConversationRef{}, fmt.Errorf("--ref: invalid ConversationRef JSON: %w", err)
		}
		return conv, nil
	}
	raw := os.Getenv(envExternalOrigin)
	if raw == "" {
		return genclient.ConversationRef{}, fmt.Errorf(
			"no ConversationRef: --ref not given and %s is not set in the session context", envExternalOrigin,
		)
	}
	var env externalOriginEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return genclient.ConversationRef{}, fmt.Errorf("decoding %s: %w", envExternalOrigin, err)
	}
	return env.Conversation, nil
}
