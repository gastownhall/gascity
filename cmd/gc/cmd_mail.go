package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

// nudgeFunc is an optional callback for nudging an agent after sending mail.
// When non-nil, it is called with the recipient name. Errors are non-fatal.
type nudgeFunc func(recipient string) error

func newMailCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mail",
		Short: "Send and receive messages between agents and humans",
		Long: `Send and receive messages between agents and humans.

Mail is implemented as beads with type="message". Messages have a
sender, recipient, subject, and body. Use "gc mail check --inject" in agent
hooks to deliver mail notifications into agent prompts.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc mail: missing subcommand (archive, check, count, delete, inbox, mark-read, mark-unread, peek, read, reply, send, thread)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc mail: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newMailArchiveCmd(stdout, stderr),
		newMailCheckCmd(stdout, stderr),
		newMailCountCmd(stdout, stderr),
		newMailDeleteCmd(stdout, stderr),
		newMailSendCmd(stdout, stderr),
		newMailInboxCmd(stdout, stderr),
		newMailMarkReadCmd(stdout, stderr),
		newMailMarkUnreadCmd(stdout, stderr),
		newMailPeekCmd(stdout, stderr),
		newMailReadCmd(stdout, stderr),
		newMailReplyCmd(stdout, stderr),
		newMailThreadCmd(stdout, stderr),
	)
	return cmd
}

func newMailArchiveCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "archive <id>",
		Short: "Archive a message without reading it",
		Long: `Close a message bead without displaying its contents.

Use this to dismiss a message without reading it. The message is marked
as closed and will no longer appear in mail check or inbox results.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailArchive(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdMailArchive is the CLI entry point for archiving a message.
func cmdMailArchive(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail archive")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailArchive(mp, rec, args, stdout, stderr)
}

// doMailArchive closes a message without displaying it. Accepts an
// injected provider and recorder for testability.
func doMailArchive(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail archive: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]

	if err := mp.Archive(id); err != nil {
		if errors.Is(err, mail.ErrAlreadyArchived) {
			fmt.Fprintf(stdout, "Already archived %s\n", id) //nolint:errcheck // best-effort stdout
			return 0
		}
		telemetry.RecordMailOp(context.Background(), "archive", err)
		fmt.Fprintf(stderr, "gc mail archive: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "archive", nil)
	rec.Record(events.Event{
		Type:    events.MailArchived,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	fmt.Fprintf(stdout, "Archived message %s\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

func newMailCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	cmd := &cobra.Command{
		Use:   "check [agent]",
		Short: "Check for unread mail (use --inject for hook output)",
		Long: `Check for unread mail addressed to an agent.

Without --inject: prints the count and exits 0 if mail exists, 1 if
empty. With --inject: outputs a <system-reminder> block suitable for
hook injection (always exits 0). The recipient defaults to $GC_AGENT
or "human".`,
		Example: `  gc mail check
  gc mail check --inject
  gc mail check mayor`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailCheck(args, inject, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "output <system-reminder> block for hook injection")
	return cmd
}

// cmdMailCheck is the CLI entry point for checking mail.
func cmdMailCheck(args []string, inject bool, stdout, stderr io.Writer) int {
	// Check city-level suspension before opening the store.
	if cityPath, err := resolveCity(); err == nil {
		if cfg, err := loadCityConfig(cityPath); err == nil {
			if citySuspended(cfg) {
				if inject {
					return 0
				}
				fmt.Fprintln(stderr, "gc mail check: city is suspended") //nolint:errcheck // best-effort stderr
				return 1
			}
		}
	}

	mp, code := openCityMailProvider(stderr, "gc mail check")
	if mp == nil {
		if inject {
			return 0 // --inject always exits 0
		}
		return code
	}

	recipient := os.Getenv("GC_AGENT")
	if recipient == "" {
		recipient = "human"
	}
	if len(args) > 0 {
		recipient = args[0]
	}

	return doMailCheck(mp, recipient, inject, stdout, stderr)
}

// doMailCheck checks for unread messages. Without --inject, prints the count
// and returns 0 if mail exists, 1 if empty. With --inject, outputs a
// <system-reminder> block for hook injection and always returns 0.
func doMailCheck(mp mail.Provider, recipient string, inject bool, stdout, stderr io.Writer) int {
	messages, err := mp.Check(recipient)
	if err != nil {
		if inject {
			fmt.Fprintf(stderr, "gc mail check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 0                                        // --inject always exits 0
		}
		fmt.Fprintf(stderr, "gc mail check: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if inject {
		if len(messages) > 0 {
			fmt.Fprint(stdout, formatInjectOutput(messages)) //nolint:errcheck // best-effort stdout
		}
		return 0 // --inject always exits 0
	}

	// Non-inject mode: print count, return 0 if mail, 1 if empty.
	if len(messages) == 0 {
		return 1
	}
	fmt.Fprintf(stdout, "%d unread message(s) for %s\n", len(messages), recipient) //nolint:errcheck // best-effort stdout
	return 0
}

// formatInjectOutput formats messages as a <system-reminder> block for
// injection into an agent's prompt via a UserPromptSubmit hook.
func formatInjectOutput(messages []mail.Message) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	fmt.Fprintf(&sb, "You have %d unread message(s).\n\n", len(messages))
	for _, m := range messages {
		if m.Subject != "" {
			fmt.Fprintf(&sb, "- %s from %s [%s]: %s\n", m.ID, m.From, m.Subject, m.Body)
		} else {
			fmt.Fprintf(&sb, "- %s from %s: %s\n", m.ID, m.From, m.Body)
		}
	}
	sb.WriteString("\nRun 'gc mail read <id>' for full details, or 'gc mail inbox' to see all.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

func newMailSendCmd(stdout, stderr io.Writer) *cobra.Command {
	var notify bool
	var all bool
	var from string
	var to string
	var subject string
	var message string
	cmd := &cobra.Command{
		Use:   "send [<to>] [<body>]",
		Short: "Send a message to an agent or human",
		Long: `Send a message to an agent or human.

Creates a message bead addressed to the recipient. The sender defaults
to $GC_AGENT (in agent sessions) or "human". Use --notify to nudge
the recipient after sending. Use --from to override the sender identity.
Use --to as an alternative to the positional <to> argument.
Use -s/--subject for the summary line and -m/--message for the body text.
Use --all to broadcast to all agents (excluding sender and "human").`,
		Example: `  gc mail send mayor "Build is green"
  gc mail send mayor -s "Build is green"
  gc mail send mayor/ -s "ESCALATION: Auth broken" -m "Token refresh fails after 30min"
  gc mail send --to mayor "Build is green"
  gc mail send human "Review needed for PR #42"
  gc mail send polecat "Priority task" --notify
  gc mail send --all "Status update: tests passing"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailSend(args, notify, all, from, to, subject, message, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&notify, "notify", false, "nudge the recipient after sending")
	cmd.Flags().BoolVar(&all, "all", false, "broadcast to all agents (excludes sender and human)")
	cmd.Flags().StringVar(&from, "from", "", "sender identity (default: $GC_AGENT or \"human\")")
	cmd.Flags().StringVar(&to, "to", "", "recipient address (alternative to positional argument)")
	cmd.Flags().StringVarP(&subject, "subject", "s", "", "message subject line")
	cmd.Flags().StringVarP(&message, "message", "m", "", "message body text")
	cmd.MarkFlagsMutuallyExclusive("to", "all")
	return cmd
}

func newMailInboxCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "inbox [agent]",
		Short: "List unread messages (defaults to your inbox)",
		Long: `List all unread messages for an agent or human.

Shows message ID, sender, subject, and body in a table. The recipient defaults
to $GC_AGENT or "human". Pass an agent name to view another agent's inbox.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailInbox(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newMailReadCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "read <id>",
		Short: "Read a message and mark it as read",
		Long: `Display a message and mark it as read.

Shows the full message details (ID, sender, recipient, subject, date, body).
The message stays in the store — use "gc mail archive" to permanently close it.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailRead(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newMailPeekCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "peek <id>",
		Short: "Show a message without marking it as read",
		Long: `Display a message without marking it as read.

Same output as "gc mail read" but does not change the message's read status.
The message will continue to appear in inbox results.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailPeek(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newMailReplyCmd(stdout, stderr io.Writer) *cobra.Command {
	var subject string
	var message string
	var notify bool
	cmd := &cobra.Command{
		Use:   "reply <id> [-s subject] [-m body]",
		Short: "Reply to a message",
		Long: `Reply to a message. The reply is addressed to the original sender.

Inherits the thread ID from the original message for conversation tracking.
Use -s/--subject for the reply subject and -m/--message for the reply body.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailReply(args, subject, message, notify, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&subject, "subject", "s", "", "reply subject line")
	cmd.Flags().StringVarP(&message, "message", "m", "", "reply body text")
	cmd.Flags().BoolVar(&notify, "notify", false, "nudge the recipient after replying")
	return cmd
}

func newMailMarkReadCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "mark-read <id>",
		Short: "Mark a message as read",
		Long:  `Mark a message as read without displaying it. The message will no longer appear in inbox results.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailMarkRead(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newMailMarkUnreadCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "mark-unread <id>",
		Short: "Mark a message as unread",
		Long:  `Mark a message as unread. The message will appear again in inbox results.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailMarkUnread(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newMailDeleteCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a message (closes the bead)",
		Long:  `Delete a message by closing the bead. Same effect as archive but with different user intent.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailDelete(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newMailThreadCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "thread <thread-id>",
		Short: "List all messages in a thread",
		Long:  `Show all messages sharing a thread ID, ordered by time.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailThread(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newMailCountCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "count [agent]",
		Short: "Show total/unread message count",
		Long: `Show total and unread message counts for an agent or human.
The recipient defaults to $GC_AGENT or "human".`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailCount(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdMailSend is the CLI entry point for sending mail. It opens the provider,
// loads config for recipient validation, and delegates to doMailSend.
// The to parameter is the --to flag value (empty if not set).
func cmdMailSend(args []string, notify bool, all bool, from string, to string, subject string, message string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail send")
	if mp == nil {
		return code
	}

	// Load city config for recipient validation. When using an exec provider
	// outside a city directory (e.g. K8s agent pods), skip validation —
	// the exec script handles its own recipient routing.
	var validRecipients map[string]bool
	var cfg *config.City
	cityPath, err := resolveCity()
	if err == nil {
		cfg, err = loadCityConfig(cityPath)
	}
	if err != nil && !strings.HasPrefix(mailProviderName(), "exec:") {
		fmt.Fprintf(stderr, "gc mail send: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cfg != nil {
		validRecipients = make(map[string]bool)
		validRecipients["human"] = true
		for _, a := range cfg.Agents {
			validRecipients[a.QualifiedName()] = true
		}
	}

	sender := from
	if sender == "" {
		sender = os.Getenv("GC_AGENT")
	}
	if sender == "" {
		sender = "human"
	}

	var nf nudgeFunc
	if notify && cfg != nil {
		nf = func(recipient string) error {
			found, ok := resolveAgentIdentity(cfg, recipient, currentRigContext(cfg))
			if !ok {
				return fmt.Errorf("agent %q not found", recipient)
			}
			target, err := resolveNudgeTarget(found.QualifiedName())
			if err != nil {
				return err
			}
			return sendMailNotify(target, sender)
		}
	}

	// When --to is set, prepend it to args so doMailSend sees [to, body].
	if to != "" && !all {
		args = append([]string{to}, args...)
	}

	// When -s/-m flags provide subject/body, use them.
	if subject != "" || message != "" {
		if all {
			args = []string{subject, message}
		} else {
			if len(args) < 1 {
				fmt.Fprintln(stderr, "gc mail send: missing recipient") //nolint:errcheck // best-effort stderr
				return 1
			}
			args = []string{args[0], subject, message}
		}
	}

	if all {
		rec := openCityRecorder(stderr)
		return doMailSendAll(mp, rec, validRecipients, sender, args, nf, stdout, stderr)
	}

	rec := openCityRecorder(stderr)
	return doMailSend(mp, rec, validRecipients, sender, args, nf, stdout, stderr)
}

// doMailSend creates a message addressed to a recipient. args is [to, subject, body]
// or [to, body] (subject="" if no -s flag). When nudgeFn is non-nil, the
// recipient is nudged after message creation (skipped for "human").
func doMailSend(mp mail.Provider, rec events.Recorder, validRecipients map[string]bool, sender string, args []string, nudgeFn nudgeFunc, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "gc mail send: usage: gc mail send <to> <body>  OR  gc mail send <to> -s <subject> [-m <body>]") //nolint:errcheck // best-effort stderr
		return 1
	}
	to := args[0]

	var subject, body string
	if len(args) >= 3 {
		// [to, subject, body] — from -s/-m flags.
		subject = args[1]
		body = args[2]
	} else {
		// [to, body] — positional arg, no subject.
		body = strings.Join(args[1:], " ")
	}

	if validRecipients != nil && !validRecipients[to] {
		fmt.Fprintf(stderr, "gc mail send: unknown recipient %q\n", to) //nolint:errcheck // best-effort stderr
		return 1
	}

	m, err := mp.Send(sender, to, subject, body)
	telemetry.RecordMailOp(context.Background(), "send", err)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail send: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailSent,
		Actor:   sender,
		Subject: m.ID,
		Message: to,
		Payload: mailEventPayload(&m),
	})
	fmt.Fprintf(stdout, "Sent message %s to %s\n", m.ID, to) //nolint:errcheck // best-effort stdout

	// Nudge recipient if requested and recipient is not human.
	if nudgeFn != nil && to != "human" {
		if err := nudgeFn(to); err != nil {
			fmt.Fprintf(stderr, "gc mail send: nudge failed: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
	return 0
}

// doMailSendAll broadcasts a message to all configured agents (excluding the
// sender and "human"). With --all, args is [subject, body] or [body].
func doMailSendAll(mp mail.Provider, rec events.Recorder, validRecipients map[string]bool, sender string, args []string, nudgeFn nudgeFunc, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail send --all: usage: gc mail send --all <body>") //nolint:errcheck // best-effort stderr
		return 1
	}

	var subject, body string
	if len(args) >= 2 {
		subject = args[0]
		body = args[1]
	} else {
		body = args[0]
	}

	// Collect recipients in sorted order for deterministic output.
	var recipients []string
	for r := range validRecipients {
		if r == sender || r == "human" {
			continue
		}
		recipients = append(recipients, r)
	}
	sort.Strings(recipients)

	if len(recipients) == 0 {
		fmt.Fprintln(stderr, "gc mail send --all: no recipients (all agents excluded)") //nolint:errcheck // best-effort stderr
		return 1
	}

	for _, to := range recipients {
		m, err := mp.Send(sender, to, subject, body)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send --all: sending to %s: %v\n", to, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		rec.Record(events.Event{
			Type:    events.MailSent,
			Actor:   sender,
			Subject: m.ID,
			Message: to,
			Payload: mailEventPayload(&m),
		})
		fmt.Fprintf(stdout, "Sent message %s to %s\n", m.ID, to) //nolint:errcheck // best-effort stdout

		if nudgeFn != nil {
			if err := nudgeFn(to); err != nil {
				fmt.Fprintf(stderr, "gc mail send --all: nudge %s failed: %v\n", to, err) //nolint:errcheck // best-effort stderr
			}
		}
	}
	return 0
}

// cmdMailInbox is the CLI entry point for checking the inbox.
func cmdMailInbox(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail inbox")
	if mp == nil {
		return code
	}

	recipient := os.Getenv("GC_AGENT")
	if recipient == "" {
		recipient = "human"
	}
	if len(args) > 0 {
		recipient = args[0]
	}

	return doMailInbox(mp, recipient, stdout, stderr)
}

// doMailInbox lists unread messages for a recipient.
func doMailInbox(mp mail.Provider, recipient string, stdout, stderr io.Writer) int {
	messages, err := mp.Inbox(recipient)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail inbox: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(messages) == 0 {
		fmt.Fprintf(stdout, "No unread messages for %s\n", recipient) //nolint:errcheck // best-effort stdout
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFROM\tSUBJECT\tBODY") //nolint:errcheck // best-effort stdout
	for _, m := range messages {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ID, m.From, m.Subject, truncate(m.Body, 60)) //nolint:errcheck // best-effort stdout
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailRead is the CLI entry point for reading a message.
func cmdMailRead(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail read")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailRead(mp, rec, args, stdout, stderr)
}

// doMailRead displays a message and marks it as read. Accepts an injected
// provider and recorder for testability.
func doMailRead(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail read: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]

	m, err := mp.Read(id)
	telemetry.RecordMailOp(context.Background(), "read", err)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail read: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	printMessage(m, stdout)

	rec.Record(events.Event{
		Type:    events.MailRead,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	return 0
}

// cmdMailPeek shows a message without marking it as read.
func cmdMailPeek(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail peek")
	if mp == nil {
		return code
	}
	return doMailPeek(mp, args, stdout, stderr)
}

// doMailPeek displays a message without marking it as read.
func doMailPeek(mp mail.Provider, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail peek: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]

	m, err := mp.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail peek: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	printMessage(m, stdout)
	return 0
}

// cmdMailReply replies to a message.
func cmdMailReply(args []string, subject, message string, notify bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail reply: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}

	mp, code := openCityMailProvider(stderr, "gc mail reply")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)

	sender := os.Getenv("GC_AGENT")
	if sender == "" {
		sender = "human"
	}

	// Determine body from remaining args if -m not set.
	body := message
	if body == "" && len(args) > 1 {
		body = strings.Join(args[1:], " ")
	}

	return doMailReply(mp, rec, args[0], sender, subject, body, notify, stdout, stderr)
}

// doMailReply creates a reply to an existing message.
func doMailReply(mp mail.Provider, rec events.Recorder, id, sender, subject, body string, _ bool, stdout, stderr io.Writer) int {
	reply, err := mp.Reply(id, sender, subject, body)
	telemetry.RecordMailOp(context.Background(), "reply", err)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail reply: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rec.Record(events.Event{
		Type:    events.MailReplied,
		Actor:   sender,
		Subject: reply.ID,
		Message: reply.To,
		Payload: mailEventPayload(&reply),
	})
	fmt.Fprintf(stdout, "Replied to %s — sent message %s to %s\n", id, reply.ID, reply.To) //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailMarkRead marks a message as read.
func cmdMailMarkRead(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail mark-read")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailMarkRead(mp, rec, args, stdout, stderr)
}

// doMailMarkRead marks a message as read.
func doMailMarkRead(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail mark-read: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]
	if err := mp.MarkRead(id); err != nil {
		telemetry.RecordMailOp(context.Background(), "mark_read", err)
		fmt.Fprintf(stderr, "gc mail mark-read: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "mark_read", nil)
	rec.Record(events.Event{
		Type:    events.MailMarkedRead,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	fmt.Fprintf(stdout, "Marked %s as read\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailMarkUnread marks a message as unread.
func cmdMailMarkUnread(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail mark-unread")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailMarkUnread(mp, rec, args, stdout, stderr)
}

// doMailMarkUnread marks a message as unread.
func doMailMarkUnread(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail mark-unread: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]
	if err := mp.MarkUnread(id); err != nil {
		telemetry.RecordMailOp(context.Background(), "mark_unread", err)
		fmt.Fprintf(stderr, "gc mail mark-unread: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "mark_unread", nil)
	rec.Record(events.Event{
		Type:    events.MailMarkedUnread,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	fmt.Fprintf(stdout, "Marked %s as unread\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailDelete deletes a message.
func cmdMailDelete(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail delete")
	if mp == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doMailDelete(mp, rec, args, stdout, stderr)
}

// doMailDelete closes a message bead (same as archive but different intent).
func doMailDelete(mp mail.Provider, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail delete: missing message ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]
	if err := mp.Delete(id); err != nil {
		if errors.Is(err, mail.ErrAlreadyArchived) {
			fmt.Fprintf(stdout, "Already deleted %s\n", id) //nolint:errcheck // best-effort stdout
			return 0
		}
		telemetry.RecordMailOp(context.Background(), "delete", err)
		fmt.Fprintf(stderr, "gc mail delete: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "delete", nil)
	rec.Record(events.Event{
		Type:    events.MailDeleted,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})
	fmt.Fprintf(stdout, "Deleted message %s\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailThread lists messages in a thread.
func cmdMailThread(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail thread")
	if mp == nil {
		return code
	}
	return doMailThread(mp, args, stdout, stderr)
}

// doMailThread shows all messages in a thread.
func doMailThread(mp mail.Provider, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc mail thread: missing thread ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	threadID := args[0]

	msgs, err := mp.Thread(threadID)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail thread: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(msgs) == 0 {
		fmt.Fprintf(stdout, "No messages in thread %s\n", threadID) //nolint:errcheck // best-effort stdout
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFROM\tTO\tSUBJECT\tSENT\tREAD") //nolint:errcheck // best-effort stdout
	for _, m := range msgs {
		readStr := " "
		if m.Read {
			readStr = "✓"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.From, m.To, m.Subject, //nolint:errcheck // best-effort stdout
			m.CreatedAt.Format("2006-01-02 15:04"), readStr)
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailCount shows total/unread count.
func cmdMailCount(args []string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail count")
	if mp == nil {
		return code
	}

	recipient := os.Getenv("GC_AGENT")
	if recipient == "" {
		recipient = "human"
	}
	if len(args) > 0 {
		recipient = args[0]
	}

	return doMailCount(mp, recipient, stdout, stderr)
}

// doMailCount displays total/unread message counts.
func doMailCount(mp mail.Provider, recipient string, stdout, stderr io.Writer) int {
	total, unread, err := mp.Count(recipient)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail count: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "%d total, %d unread for %s\n", total, unread, recipient) //nolint:errcheck // best-effort stdout
	return 0
}

// printMessage displays a message's full details.
func printMessage(m mail.Message, stdout io.Writer) {
	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w(fmt.Sprintf("ID:       %s", m.ID))
	w(fmt.Sprintf("From:     %s", m.From))
	w(fmt.Sprintf("To:       %s", m.To))
	if m.Subject != "" {
		w(fmt.Sprintf("Subject:  %s", m.Subject))
	}
	w(fmt.Sprintf("Sent:     %s", m.CreatedAt.Format("2006-01-02 15:04:05")))
	if m.Body != "" {
		w(fmt.Sprintf("Body:     %s", m.Body))
	}
}

// truncate shortens s to n characters, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// mailEventRig returns the rig name for mail event payloads.
// Reads GC_RIG (set for agents running in rig context).
func mailEventRig() string {
	return os.Getenv("GC_RIG")
}

// mailEventPayload builds a JSON payload for mail events so SSE consumers
// (Mission Control) can route updates to the correct rig.
// For sent/replied events, pass the full message; for state changes pass nil.
func mailEventPayload(msg *mail.Message) json.RawMessage {
	m := map[string]any{"rig": mailEventRig()}
	if msg != nil {
		m["message"] = msg
	}
	b, _ := json.Marshal(m)
	return b
}
