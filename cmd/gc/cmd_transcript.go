package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/spf13/cobra"
)

func newTranscriptCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "transcript",
		Short: "Check and read shared conversation transcripts",
		Long: `Check and read shared external conversation transcripts.

Transcripts are shared message histories for external conversations
(e.g., Discord threads). Use "gc transcript check --inject" in agent
hooks to deliver unread messages into agent prompts.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc transcript: missing subcommand (check, read)") //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "gc transcript: unknown subcommand %q\n", args[0]) //nolint:errcheck
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newTranscriptCheckCmd(stdout, stderr),
		newTranscriptReadCmd(stdout, stderr),
	)
	return cmd
}

func newTranscriptCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	cmd := &cobra.Command{
		Use:   "check [session]",
		Short: "Check for unread transcript entries (use --inject for hook output)",
		Long: `Check for unread external conversation messages.

Without --inject: prints the count per conversation and exits 0 if
unread entries exist, 1 if empty. With --inject: outputs a
<system-reminder> block for hook injection (always exits 0).
Session defaults to $GC_SESSION_ID.`,
		Example: `  gc transcript check
  gc transcript check --inject`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdTranscriptCheck(args, inject, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "output <system-reminder> block for hook injection")
	return cmd
}

func newTranscriptReadCmd(stdout, stderr io.Writer) *cobra.Command {
	var ack bool
	cmd := &cobra.Command{
		Use:   "read [session]",
		Short: "Read unread transcript entries",
		Long: `Read unread external conversation messages.

Prints unread entries formatted for agent consumption. With --ack,
advances LastReadSequence so entries are not repeated on the next
check. Session defaults to $GC_SESSION_ID.`,
		Example: `  gc transcript read
  gc transcript read --ack`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdTranscriptRead(args, ack, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&ack, "ack", false, "advance LastReadSequence after reading")
	return cmd
}

func defaultTranscriptSession() string {
	if sid := strings.TrimSpace(os.Getenv("GC_SESSION_ID")); sid != "" {
		return sid
	}
	if sname := strings.TrimSpace(os.Getenv("GC_SESSION_NAME")); sname != "" {
		return sname
	}
	return ""
}

func openTranscriptServices(stderr io.Writer, cmdName string) (*extmsg.Services, int) {
	store, code := openCityStore(stderr, cmdName)
	if store == nil {
		return nil, code
	}
	svc := extmsg.NewServices(store)
	return &svc, 0
}

func cmdTranscriptCheck(args []string, inject bool, stdout, stderr io.Writer) int {
	// Check city-level suspension.
	if cityPath, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cityPath); err == nil {
			if citySuspended(cfg) {
				if inject {
					return 0
				}
				fmt.Fprintln(stderr, "gc transcript check: city is suspended") //nolint:errcheck
				return 1
			}
		}
	}

	svc, code := openTranscriptServices(stderr, "gc transcript check")
	if svc == nil {
		if inject {
			return 0 // --inject always exits 0
		}
		return code
	}

	sessionID := defaultTranscriptSession()
	if len(args) > 0 {
		sessionID = args[0]
	}
	if sessionID == "" {
		if inject {
			return 0
		}
		fmt.Fprintln(stderr, "gc transcript check: session identity required (set GC_SESSION_ID or pass argument)") //nolint:errcheck
		return 1
	}

	ctx := context.Background()
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: sessionID}

	memberships, err := svc.Transcript.ListConversationsBySession(ctx, caller, sessionID)
	if err != nil {
		if inject {
			fmt.Fprintf(stderr, "gc transcript check: %v\n", err) //nolint:errcheck
			return 0
		}
		fmt.Fprintf(stderr, "gc transcript check: %v\n", err) //nolint:errcheck
		return 1
	}

	// Collect unread entries per conversation.
	var unread []transcriptConversationUnread
	totalUnread := 0

	for _, m := range memberships {
		entries, err := svc.Transcript.ListBackfill(ctx, extmsg.ListBackfillInput{
			Caller:       caller,
			Conversation: m.Conversation,
			SessionID:    sessionID,
			Limit:        50,
		})
		if err != nil {
			fmt.Fprintf(stderr, "gc transcript check: backfill %s/%s: %v\n", //nolint:errcheck
				m.Conversation.Provider, m.Conversation.ConversationID, err)
			continue
		}
		if len(entries) == 0 {
			continue
		}
		label := m.Conversation.Provider + "/" + m.Conversation.ConversationID
		unread = append(unread, transcriptConversationUnread{Label: label, Entries: entries})
		totalUnread += len(entries)
	}

	if inject {
		if totalUnread > 0 {
			fmt.Fprint(stdout, formatTranscriptInjectOutput(unread)) //nolint:errcheck
		}
		return 0
	}

	if totalUnread == 0 {
		return 1
	}
	for _, cu := range unread {
		fmt.Fprintf(stdout, "%d unread in %s\n", len(cu.Entries), cu.Label) //nolint:errcheck
	}
	return 0
}

func cmdTranscriptRead(args []string, ack bool, stdout, stderr io.Writer) int {
	// Check city-level suspension.
	if cityPath, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cityPath); err == nil {
			if citySuspended(cfg) {
				fmt.Fprintln(stderr, "gc transcript read: city is suspended") //nolint:errcheck
				return 1
			}
		}
	}

	svc, code := openTranscriptServices(stderr, "gc transcript read")
	if svc == nil {
		return code
	}

	sessionID := defaultTranscriptSession()
	if len(args) > 0 {
		sessionID = args[0]
	}
	if sessionID == "" {
		fmt.Fprintln(stderr, "gc transcript read: session identity required (set GC_SESSION_ID or pass argument)") //nolint:errcheck
		return 1
	}

	ctx := context.Background()
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: sessionID}

	memberships, err := svc.Transcript.ListConversationsBySession(ctx, caller, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc transcript read: %v\n", err) //nolint:errcheck
		return 1
	}

	for _, m := range memberships {
		entries, err := svc.Transcript.ListBackfill(ctx, extmsg.ListBackfillInput{
			Caller:       caller,
			Conversation: m.Conversation,
			SessionID:    sessionID,
			Limit:        50,
		})
		if err != nil {
			label := m.Conversation.Provider + "/" + m.Conversation.ConversationID
			fmt.Fprintf(stderr, "gc transcript read: backfill %s: %v\n", label, err) //nolint:errcheck
			continue
		}
		if len(entries) == 0 {
			continue
		}
		label := m.Conversation.Provider + "/" + m.Conversation.ConversationID
		fmt.Fprintf(stdout, "## %s (%d unread)\n", label, len(entries)) //nolint:errcheck
		for _, e := range entries {
			actorKind := "agent"
			if !e.Actor.IsBot {
				actorKind = "human"
			}
			fmt.Fprintf(stdout, "- [seq %d] %s (%s): %s\n", e.Sequence, e.Actor.DisplayName, actorKind, e.Text) //nolint:errcheck
		}
		fmt.Fprintln(stdout) //nolint:errcheck

		if ack && len(entries) > 0 {
			lastSeq := entries[len(entries)-1].Sequence
			if err := svc.Transcript.Ack(ctx, extmsg.AckMembershipInput{
				Caller:       caller,
				Conversation: m.Conversation,
				SessionID:    sessionID,
				Sequence:     lastSeq,
			}); err != nil {
				fmt.Fprintf(stderr, "gc transcript read: ack %s: %v\n", label, err) //nolint:errcheck
			}
		}
	}
	return 0
}

type transcriptConversationUnread struct {
	Label   string
	Entries []extmsg.ConversationTranscriptRecord
}

// sanitizeForInject escapes untrusted text to prevent breaking out of
// the <system-reminder> wrapper. Uses HTML escaping which covers <, >,
// &, and quotes — sufficient to prevent tag injection.
func sanitizeForInject(s string) string {
	return html.EscapeString(s)
}

func formatTranscriptInjectOutput(conversations []transcriptConversationUnread) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	fmt.Fprintf(&sb, "You have unread messages in %d shared conversation(s).\n\n", len(conversations))
	for _, cu := range conversations {
		fmt.Fprintf(&sb, "## %s (%d unread)\n", cu.Label, len(cu.Entries))
		for _, e := range cu.Entries {
			actorKind := "agent"
			if !e.Actor.IsBot {
				actorKind = "human"
			}
			fmt.Fprintf(&sb, "- [seq %d] %s (%s): %s\n",
				e.Sequence,
				sanitizeForInject(e.Actor.DisplayName),
				actorKind,
				sanitizeForInject(e.Text))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Use your judgment — respond where you can add value.\n")
	sb.WriteString("To reply in Discord, write your response to a file and run:\n")
	for _, cu := range conversations {
		convID := ""
		if len(cu.Entries) > 0 {
			convID = cu.Entries[0].Conversation.ConversationID
		}
		if convID != "" {
			fmt.Fprintf(&sb, "  gc discord reply-current --conversation-id %s --body-file <path>\n", convID)
		}
	}
	sb.WriteString("Prefix your reply with your agent handle in bold (e.g., **sky:** your message).\n")
	sb.WriteString("Run 'gc transcript read --ack' after responding to mark as read.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}
