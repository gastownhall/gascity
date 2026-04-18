package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

// nudgeFunc is an optional callback for nudging an agent after sending or
// replying to mail. When non-nil, it is called with the recipient name.
// Errors are non-fatal.
type nudgeFunc func(recipient string) error

func newMailNudgeFunc(sender string) nudgeFunc {
	return func(recipient string) error {
		target, err := resolveNudgeTarget(recipient)
		if err != nil {
			return err
		}
		return sendMailNotify(target, sender)
	}
}

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
	var hookFormat string
	cmd := &cobra.Command{
		Use:   "check [session]",
		Short: "Check for unread mail (use --inject for hook output)",
		Long: `Check for unread mail addressed to a session alias or mailbox.

Without --inject: prints the count and exits 0 if mail exists, 1 if
empty. With --inject: outputs a <system-reminder> block suitable for
hook injection (always exits 0). The recipient defaults to $GC_ALIAS,
$GC_SESSION_ID, or "human".`,
		Example: `  gc mail check
  gc mail check --inject
  gc mail check mayor`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailCheckWithFormat(args, inject, hookFormat, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "output <system-reminder> block for hook injection")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	return cmd
}

func cmdMailCheckWithFormat(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
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

	recipient := defaultMailIdentity()
	if len(args) > 0 {
		recipient = args[0]
	}
	target, ok := resolveMailTargetsForCommand(recipient, stderr, "gc mail check")
	if !ok {
		if inject {
			return 0
		}
		return 1
	}

	return doMailCheckTargetWithFormat(mp, target, inject, hookFormat, stdout, stderr)
}

// doMailCheck checks for unread messages. Without --inject, prints the count
// and returns 0 if mail exists, 1 if empty. With --inject, outputs a
// <system-reminder> block for hook injection and always returns 0.
func doMailCheck(mp mail.Provider, recipient string, inject bool, stdout, stderr io.Writer) int {
	return doMailCheckTarget(mp, resolvedMailTarget{display: recipient, recipients: []string{recipient}}, inject, stdout, stderr)
}

func doMailCheckTarget(mp mail.Provider, target resolvedMailTarget, inject bool, stdout, stderr io.Writer) int {
	return doMailCheckTargetWithFormat(mp, target, inject, "", stdout, stderr)
}

func doMailCheckTargetWithFormat(mp mail.Provider, target resolvedMailTarget, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	messages, err := collectMailMessages(mp.Check, target.recipients)
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
			_ = writeProviderHookContext(stdout, hookFormat, formatInjectOutput(messages))
		}
		return 0 // --inject always exits 0
	}

	// Non-inject mode: print count, return 0 if mail, 1 if empty.
	if len(messages) == 0 {
		return 1
	}
	fmt.Fprintf(stdout, "%d unread message(s) for %s\n", len(messages), target.display) //nolint:errcheck // best-effort stdout
	return 0
}

// formatInjectOutput formats messages as a <system-reminder> block for
// injection into an agent's prompt via a UserPromptSubmit hook.
func formatInjectOutput(messages []mail.Message) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	fmt.Fprintf(&sb, "You have %d unread message(s).\n\n", len(messages))
	for _, m := range messages {
		subject := strings.TrimSpace(m.Subject)
		body := strings.TrimSpace(m.Body)
		if subject != "" && subject != body {
			fmt.Fprintf(&sb, "- %s from %s [%s]: %s\n", m.ID, m.From, m.Subject, m.Body)
		} else {
			fmt.Fprintf(&sb, "- %s from %s: %s\n", m.ID, m.From, m.Body)
		}
	}
	sb.WriteString("\nRun 'gc mail read <id>' for full details, or 'gc mail inbox' to see all.\n")
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

func defaultMailIdentity() string {
	if alias := strings.TrimSpace(os.Getenv("GC_ALIAS")); alias != "" {
		return alias
	}
	if sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID")); sessionID != "" {
		return sessionID
	}
	if agent := strings.TrimSpace(os.Getenv("GC_AGENT")); agent != "" {
		return agent
	}
	return "human"
}

func sessionMailboxAddress(b beads.Bead) string {
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
		return alias
	}
	if b.ID != "" {
		return b.ID
	}
	return strings.TrimSpace(b.Metadata["session_name"])
}

func sessionMailboxAddresses(b beads.Bead) []string {
	seen := map[string]bool{}
	var addresses []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		addresses = append(addresses, value)
	}
	add(sessionMailboxAddress(b))
	add(b.ID)
	for _, alias := range session.AliasHistory(b.Metadata) {
		add(alias)
	}
	if len(addresses) == 0 {
		add(strings.TrimSpace(b.Metadata["session_name"]))
	}
	return addresses
}

func resolveMailIdentity(store beads.Store, identifier string) (string, error) {
	if identifier == "" || identifier == "human" {
		return "human", nil
	}
	sessionID, err := resolveSessionID(store, identifier)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			if target, matched, targetErr := resolveLiveConfiguredNamedMailTarget(store, identifier); targetErr != nil {
				return "", targetErr
			} else if matched {
				return target.display, nil
			}
			if address, ok := configuredMailboxAddress(identifier); ok {
				return address, nil
			}
		}
		return "", err
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return "", err
	}
	address := sessionMailboxAddress(b)
	if address == "" {
		return "", fmt.Errorf("session %q has no mailbox identity", identifier)
	}
	return address, nil
}

func resolveMailIdentityWithConfig(cityPath string, cfg *config.City, store beads.Store, identifier string) (string, error) {
	if identifier == "" || identifier == "human" {
		return "human", nil
	}
	if store != nil && cfg != nil {
		sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, store, identifier)
		if err == nil {
			b, err := store.Get(sessionID)
			if err != nil {
				return "", err
			}
			address := sessionMailboxAddress(b)
			if address == "" {
				return "", fmt.Errorf("session %q has no mailbox identity", identifier)
			}
			return address, nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	if target, matched, targetErr := resolveLiveConfiguredNamedMailTarget(store, identifier); targetErr != nil {
		return "", targetErr
	} else if matched {
		return target.display, nil
	}
	if address, ok := configuredMailboxAddressWithConfig(cityPath, cfg, identifier); ok {
		return address, nil
	}
	return resolveMailIdentity(store, identifier)
}

func resolveMailRecipientIdentity(cityPath string, cfg *config.City, store beads.Store, identifier string) (string, error) {
	if identifier == "" || identifier == "human" {
		return "human", nil
	}
	if store != nil && cfg != nil {
		sessionID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, identifier)
		if err == nil {
			b, err := store.Get(sessionID)
			if err != nil {
				return "", err
			}
			address := sessionMailboxAddress(b)
			if address == "" {
				return "", fmt.Errorf("session %q has no mailbox identity", identifier)
			}
			return address, nil
		}
		if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	if target, matched, targetErr := resolveLiveConfiguredNamedMailTarget(store, identifier); targetErr != nil {
		return "", targetErr
	} else if matched {
		return target.display, nil
	}
	return resolveMailIdentityWithConfig(cityPath, cfg, store, identifier)
}

func configuredMailboxAddress(identifier string) (string, bool) {
	identifier = normalizeNamedSessionTarget(identifier)
	if identifier == "" || identifier == "human" {
		return "", false
	}
	cityPath, err := resolveCity()
	if err != nil {
		return "", false
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return "", false
	}
	return configuredMailboxAddressWithConfig(cityPath, cfg, identifier)
}

func configuredMailboxAddressWithConfig(cityPath string, cfg *config.City, identifier string) (string, bool) {
	identifier = normalizeNamedSessionTarget(identifier)
	if identifier == "" || identifier == "human" || cfg == nil {
		return "", false
	}
	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	spec, ok, err := findNamedSessionSpecForTarget(cfg, cityName, identifier)
	if err != nil || !ok {
		return "", false
	}
	return spec.Identity, true
}

func listLiveSessionMailboxes(store beads.Store) (map[string]bool, error) {
	recipients := map[string]bool{"human": true}
	if store == nil {
		return recipients, nil
	}
	all, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
	})
	if err != nil {
		return nil, err
	}
	for _, b := range all {
		if !session.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
			continue
		}
		if address := sessionMailboxAddress(b); address != "" {
			recipients[address] = true
		}
	}
	return recipients, nil
}

type resolvedMailTarget struct {
	display    string
	recipients []string
}

func resolveLiveConfiguredNamedMailTarget(store beads.Store, identifier string) (resolvedMailTarget, bool, error) {
	identifier = normalizeNamedSessionTarget(identifier)
	if store == nil || identifier == "" || identifier == "human" || strings.Contains(identifier, "/") {
		return resolvedMailTarget{}, false, nil
	}
	all, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
	})
	if err != nil {
		return resolvedMailTarget{}, false, err
	}

	matches := make(map[string]resolvedMailTarget)
	order := make([]string, 0, 2)
	for _, b := range all {
		if !session.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
			continue
		}
		identity := strings.TrimSpace(b.Metadata[namedSessionIdentityMetadata])
		if identity == "" || targetBasename(identity) != identifier {
			continue
		}
		addresses := sessionMailboxAddresses(b)
		if len(addresses) == 0 {
			continue
		}
		display := sessionMailboxAddress(b)
		if display == "" {
			display = addresses[0]
		}
		if _, ok := matches[display]; ok {
			continue
		}
		matches[display] = resolvedMailTarget{
			display:    display,
			recipients: addresses,
		}
		order = append(order, display)
	}

	switch len(order) {
	case 0:
		return resolvedMailTarget{}, false, nil
	case 1:
		return matches[order[0]], true, nil
	default:
		return resolvedMailTarget{}, true, fmt.Errorf("%w: %q matches %d live configured named sessions: %s",
			session.ErrAmbiguous, identifier, len(order), strings.Join(order, ", "))
	}
}

func resolveMailTargets(store beads.Store, identifier string) (resolvedMailTarget, error) {
	if identifier == "" || identifier == "human" {
		return resolvedMailTarget{display: "human", recipients: []string{"human"}}, nil
	}
	sessionID, err := resolveSessionID(store, identifier)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			if target, matched, targetErr := resolveLiveConfiguredNamedMailTarget(store, identifier); targetErr != nil {
				return resolvedMailTarget{}, targetErr
			} else if matched {
				return target, nil
			}
			if address, ok := configuredMailboxAddress(identifier); ok {
				return resolvedMailTarget{display: address, recipients: []string{address}}, nil
			}
		}
		return resolvedMailTarget{}, err
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return resolvedMailTarget{}, err
	}
	addresses := sessionMailboxAddresses(b)
	if len(addresses) == 0 {
		return resolvedMailTarget{}, fmt.Errorf("session %q has no mailbox identity", identifier)
	}
	return resolvedMailTarget{
		display:    addresses[0],
		recipients: addresses,
	}, nil
}

func resolveMailTargetsForCommand(identifier string, stderr io.Writer, cmdName string) (resolvedMailTarget, bool) {
	if identifier == "" || identifier == "human" {
		return resolvedMailTarget{display: "human", recipients: []string{"human"}}, true
	}
	if rawTarget, ok := resolveRawMailTargetForStorelessProvider(identifier, stderr, cmdName); ok {
		return rawTarget, true
	}
	store, code := openCityStore(stderr, cmdName)
	if store == nil {
		_ = code
		return resolvedMailTarget{}, false
	}
	target, err := resolveMailTargets(store, identifier)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return resolvedMailTarget{}, false
	}
	return target, true
}

func resolveRawMailTargetForStorelessProvider(identifier string, stderr io.Writer, cmdName string) (resolvedMailTarget, bool) {
	v := mailProviderName()
	if !strings.HasPrefix(v, "exec:") && v != "fake" && v != "fail" {
		return resolvedMailTarget{}, false
	}
	store, err := openMailTargetStore()
	if err != nil {
		if isNoCityStoreError(err) {
			return resolvedMailTarget{display: identifier, recipients: []string{identifier}}, true
		}
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return resolvedMailTarget{}, false
	}
	if err == nil && store != nil {
		target, resolveErr := resolveMailTargets(store, identifier)
		if resolveErr == nil {
			return target, true
		}
		if !errors.Is(resolveErr, session.ErrSessionNotFound) {
			fmt.Fprintf(stderr, "%s: %v\n", cmdName, resolveErr) //nolint:errcheck // best-effort stderr
			return resolvedMailTarget{}, false
		}
	}
	return resolvedMailTarget{display: identifier, recipients: []string{identifier}}, true
}

func isNoCityStoreError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not in a city directory") || strings.Contains(msg, "not a city directory")
}

var openMailTargetStore = tryOpenCityStore

func tryOpenCityStore() (beads.Store, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	return openCityStoreAt(cityPath)
}

func resolveMailAddressForCommand(identifier string, stderr io.Writer, cmdName string) (string, bool) {
	target, ok := resolveMailTargetsForCommand(identifier, stderr, cmdName)
	if !ok {
		return "", false
	}
	return target.display, true
}

func collectMailMessages(fetch func(string) ([]mail.Message, error), recipients []string) ([]mail.Message, error) {
	seen := map[string]mail.Message{}
	order := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		messages, err := fetch(recipient)
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			if _, ok := seen[message.ID]; !ok {
				order = append(order, message.ID)
			}
			seen[message.ID] = message
		}
	}
	result := make([]mail.Message, 0, len(order))
	for _, id := range order {
		result = append(result, seen[id])
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func collectMailCounts(count func(string) (int, int, error), recipients []string) (int, int, error) {
	total := 0
	unread := 0
	for _, recipient := range recipients {
		recipientTotal, recipientUnread, err := count(recipient)
		if err != nil {
			return 0, 0, err
		}
		total += recipientTotal
		unread += recipientUnread
	}
	return total, unread, nil
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
		Short: "Send a message to a session alias or human",
		Long: `Send a message to a session alias or human.

Creates a message bead addressed to the recipient. The sender defaults
to $GC_ALIAS or $GC_SESSION_ID (in sessions) or "human". Use --notify to nudge
the recipient after sending. Use --from to override the sender identity.
Use --to as an alternative to the positional <to> argument.
Use -s/--subject for the summary line and -m/--message for the body text.
Use --all to broadcast to all live sessions (excluding sender and "human").`,
		Example: `  gc mail send mayor "Build is green"
  gc mail send mayor -s "Build is green"
  gc mail send myrig/witness -s "Need investigation" -m "Attach logs from the last failed run"
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
	cmd.Flags().BoolVar(&all, "all", false, "broadcast to all live sessions (excludes sender and human)")
	cmd.Flags().StringVar(&from, "from", "", "sender identity (default: $GC_ALIAS, $GC_SESSION_ID, or \"human\")")
	cmd.Flags().StringVar(&to, "to", "", "recipient address (alternative to positional argument)")
	cmd.Flags().StringVarP(&subject, "subject", "s", "", "message subject line")
	cmd.Flags().StringVarP(&message, "message", "m", "", "message body text")
	cmd.MarkFlagsMutuallyExclusive("to", "all")
	return cmd
}

func newMailInboxCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "inbox [session]",
		Short: "List unread messages (defaults to your inbox)",
		Long: `List all unread messages for a session alias or human.

Shows message ID, sender, subject, and body in a table. The recipient defaults
to $GC_ALIAS, $GC_SESSION_ID, or "human". Pass a session alias to view another inbox.`,
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
		Use:   "count [session]",
		Short: "Show total/unread message count",
		Long: `Show total and unread message counts for a session alias or human.
The recipient defaults to $GC_ALIAS, $GC_SESSION_ID, or "human".`,
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
// resolves session mailbox identities, and delegates to doMailSend.
// The to parameter is the --to flag value (empty if not set).
func cmdMailSend(args []string, notify bool, all bool, from string, to string, subject string, message string, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail send")
	if mp == nil {
		return code
	}

	var (
		store           beads.Store
		validRecipients map[string]bool
		cfg             *config.City
	)
	cityPath, err := resolveCity()
	if err == nil {
		cfg, _ = loadCityConfig(cityPath)
		store, err = openCityStoreAt(cityPath)
	}
	if err != nil && !strings.HasPrefix(mailProviderName(), "exec:") {
		fmt.Fprintf(stderr, "gc mail send: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if store != nil {
		validRecipients, err = listLiveSessionMailboxes(store)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send: listing live sessions: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	sender := from
	if sender == "" {
		sender = defaultMailIdentity()
	}
	if sender != "human" && store != nil {
		sender, err = resolveMailIdentityWithConfig(cityPath, cfg, store, sender)
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send: invalid sender %q: %v\n", sender, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	var nf nudgeFunc
	if notify && store != nil {
		nf = newMailNudgeFunc(sender)
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
	if !all && len(args) > 0 && store != nil {
		canonicalTo, err := resolveMailRecipientIdentity(cityPath, cfg, store, args[0])
		if err != nil {
			fmt.Fprintf(stderr, "gc mail send: unknown recipient %q: %v\n", args[0], err) //nolint:errcheck // best-effort stderr
			return 1
		}
		args[0] = canonicalTo
		if validRecipients != nil {
			validRecipients[canonicalTo] = true
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

// doMailSendAll broadcasts a message to all live session mailboxes (excluding the
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
		fmt.Fprintln(stderr, "gc mail send --all: no recipients (all live sessions excluded)") //nolint:errcheck // best-effort stderr
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

	recipient := defaultMailIdentity()
	if len(args) > 0 {
		recipient = args[0]
	}
	target, ok := resolveMailTargetsForCommand(recipient, stderr, "gc mail inbox")
	if !ok {
		return 1
	}

	return doMailInboxTarget(mp, target, stdout, stderr)
}

// doMailInbox lists unread messages for a recipient.
func doMailInbox(mp mail.Provider, recipient string, stdout, stderr io.Writer) int {
	return doMailInboxTarget(mp, resolvedMailTarget{display: recipient, recipients: []string{recipient}}, stdout, stderr)
}

func doMailInboxTarget(mp mail.Provider, target resolvedMailTarget, stdout, stderr io.Writer) int {
	messages, err := collectMailMessages(mp.Inbox, target.recipients)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail inbox: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(messages) == 0 {
		fmt.Fprintf(stdout, "No unread messages for %s\n", target.display) //nolint:errcheck // best-effort stdout
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

	sender := defaultMailIdentity()
	var hasStore bool
	if sender != "human" {
		v := mailProviderName()
		if !strings.HasPrefix(v, "exec:") && v != "fake" && v != "fail" {
			hasStore = true
			store, storeCode := openCityStore(stderr, "gc mail reply")
			if store == nil {
				return storeCode
			}
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc mail reply: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			cfg, _ := loadCityConfig(cityPath)
			resolved, err := resolveMailIdentityWithConfig(cityPath, cfg, store, sender)
			if err != nil {
				fmt.Fprintf(stderr, "gc mail reply: invalid sender %q: %v\n", sender, err) //nolint:errcheck // best-effort stderr
				return 1
			}
			sender = resolved
		}
	}

	// Determine body from remaining args if -m not set.
	body := message
	if body == "" && len(args) > 1 {
		body = strings.Join(args[1:], " ")
	}

	var nf nudgeFunc
	if notify && hasStore {
		nf = newMailNudgeFunc(sender)
	}

	return doMailReply(mp, rec, args[0], sender, subject, body, nf, stdout, stderr)
}

// doMailReply creates a reply to an existing message.
func doMailReply(mp mail.Provider, rec events.Recorder, id, sender, subject, body string, nudgeFn nudgeFunc, stdout, stderr io.Writer) int {
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

	if nudgeFn != nil && reply.To != "human" {
		if err := nudgeFn(reply.To); err != nil {
			fmt.Fprintf(stderr, "gc mail reply: nudge failed: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
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

	recipient := defaultMailIdentity()
	if len(args) > 0 {
		recipient = args[0]
	}
	target, ok := resolveMailTargetsForCommand(recipient, stderr, "gc mail count")
	if !ok {
		return 1
	}

	return doMailCountTarget(mp, target, stdout, stderr)
}

// doMailCount displays total/unread message counts.
func doMailCount(mp mail.Provider, recipient string, stdout, stderr io.Writer) int {
	return doMailCountTarget(mp, resolvedMailTarget{display: recipient, recipients: []string{recipient}}, stdout, stderr)
}

func doMailCountTarget(mp mail.Provider, target resolvedMailTarget, stdout, stderr io.Writer) int {
	total, unread, err := collectMailCounts(mp.Count, target.recipients)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail count: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "%d total, %d unread for %s\n", total, unread, target.display) //nolint:errcheck // best-effort stdout
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
// (e.g. dashboard clients) can route updates to the correct rig.
// For sent/replied events, pass the full message; for state changes pass nil.
func mailEventPayload(msg *mail.Message) json.RawMessage {
	m := map[string]any{"rig": mailEventRig()}
	if msg != nil {
		m["message"] = msg
	}
	b, _ := json.Marshal(m)
	return b
}
