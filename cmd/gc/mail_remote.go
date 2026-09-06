package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/mail"
)

// Remote mail: `gc --context <peer> mail send|reply|inbox` operate a REMOTE
// city over the control plane. The remote server resolves the recipient and
// stores the message; the sender identity is asserted by this client (the
// city-write grant proves which city sent it, the from string says who inside
// that city). Local-only modes are refused with a clear message rather than
// silently degraded: --all needs the local live-session enumeration and
// --notify needs local nudge delivery, neither of which the mail API models.
//
// Sender identity: a remote city cannot resolve this session's alias, so the
// default sender is city-qualified — "<local city>/<identity>" (e.g.
// citadel/mayor) — which is what the far side sees in `gc mail inbox` and
// what tells its operator to answer with `gc --context <city> mail send …`.
// --from overrides it verbatim. gc has no cross-city addressing, so a plain
// `gc mail reply` on the far side lands in THAT city's store, addressed to
// the qualified sender; `gc --context <peer> mail inbox` (no argument) reads
// exactly that mailbox back.

// remoteMailIdentity returns the identity part of the default remote sender:
// the first non-empty of GC_ALIAS, GC_AGENT, GC_SESSION_ID, else "human". The
// alias/agent names lead because they are meaningful to a peer city; a bare
// session id (the local default, see defaultMailIdentityCandidates) is opaque
// outside the city that minted it.
func remoteMailIdentity() string {
	for _, k := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "human"
}

// localCityNameForRemoteMail returns the effective name of the LOCAL city the
// caller is operating from (explicit city env, then cwd discovery), or "" when
// no local city is discoverable. It never consults the remote target.
func localCityNameForRemoteMail() string {
	var cityPath string
	if ctx, handled, err := resolveContextFromCityEnv(); handled && err == nil {
		cityPath = ctx.CityPath
	} else if ctx, err := resolveContextFromDir(); err == nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		return ""
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	return strings.TrimSpace(loadedCityName(cfg, cityPath))
}

// remoteMailSender resolves the sender for a remote mail mutation: an explicit
// --from (whitespace-trimmed, otherwise as given), else
// "<local city>/<identity>" (identity alone when no local city is
// discoverable). The peer stores an unknown sender literally; if the peer
// happens to have a session whose alias equals this string (a rig named after
// the sending city), its beadmail binds the message to that session — so a
// peer must not name a rig after a city that mails it (see the runbook).
func remoteMailSender(from string) string {
	if from = strings.TrimSpace(from); from != "" {
		return from
	}
	id := remoteMailIdentity()
	if city := localCityNameForRemoteMail(); city != "" {
		return city + "/" + id
	}
	return id
}

// remoteMailSubject mirrors the beadmail provider's title derivation for a
// send without -s: the API requires a non-empty subject (minLength 1), so
// derive it from the first non-blank body line (truncated). It returns "" only
// when neither the subject nor the body carries any non-blank text — the
// caller refuses that locally instead of round-tripping to a 422.
func remoteMailSubject(subject, body string) string {
	if subject = strings.TrimSpace(subject); subject != "" {
		return subject
	}
	title := strings.TrimSpace(strings.SplitN(strings.TrimLeft(body, " \t\r\n"), "\n", 2)[0])
	if len(title) > 80 {
		title = title[:77] + "..."
	}
	return title
}

// cmdMailSendRemote is the remote arm of `gc mail send`. args are the
// positional [to, body...] (after --to has been folded in by the caller).
func cmdMailSendRemote(c *api.Client, target *remoteTarget, args []string, notify, all bool, from, to, subject, message string, jsonOut bool, stdout, stderr io.Writer) int {
	fail := func(code, msg string) int {
		if jsonOut {
			return writeJSONError(stdout, stderr, code, msg, 1)
		}
		fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	if all {
		return fail("unsupported_remote", "gc mail send: --all is not supported for a remote city (live-session enumeration is local); address one recipient")
	}
	if notify {
		return fail("unsupported_remote", "gc mail send: --notify delivery for a remote city lands separately; send without --notify")
	}
	if to != "" {
		args = append([]string{to}, args...)
	}
	if len(args) < 1 {
		return fail("invalid_arguments", "gc mail send: missing recipient")
	}
	recipient := args[0]
	body := message
	if body == "" && len(args) > 1 {
		body = strings.Join(args[1:], " ")
	}
	derivedSubject := remoteMailSubject(subject, body)
	if derivedSubject == "" {
		return fail("invalid_arguments", "gc mail send: usage: gc mail send <to> <body>  OR  gc mail send <to> -s <subject> [-m <body>] (a non-blank subject or body is required)")
	}
	sender := remoteMailSender(from)
	if !jsonOut {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}
	m, err := c.SendMail(api.MailSendRequest{
		From:    sender,
		To:      recipient,
		Subject: derivedSubject,
		Body:    body,
	})
	if err != nil {
		return fail("mail_send_failed", "gc mail send: "+err.Error())
	}
	if jsonOut {
		summary := summarizeMailMessage(m)
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail send", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.send", Action: "send", ID: m.ID, Message: &summary, Messages: []mailMessageSummary{summary}, Count: intRef(1)})
	}
	fmt.Fprintf(stdout, "Sent message %s to %s (as %s)\n", m.ID, m.To, m.From) //nolint:errcheck // best-effort stdout
	return 0
}

// cmdMailReplyRemote is the remote arm of `gc mail reply <id> [body...]`.
func cmdMailReplyRemote(c *api.Client, target *remoteTarget, args []string, subject, message string, notify, jsonOut bool, stdout, stderr io.Writer) int {
	fail := func(code, msg string) int {
		if jsonOut {
			return writeJSONError(stdout, stderr, code, msg, 1)
		}
		fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(args) < 1 {
		return fail("invalid_arguments", "gc mail reply: missing message ID")
	}
	if notify {
		return fail("unsupported_remote", "gc mail reply: --notify delivery for a remote city lands separately; reply without --notify")
	}
	id := args[0]
	body := message
	if body == "" && len(args) > 1 {
		body = strings.Join(args[1:], " ")
	}
	sender := remoteMailSender("")
	if !jsonOut {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}
	reply, err := c.ReplyMail(id, api.MailReplyRequest{From: sender, Subject: subject, Body: body})
	if err != nil {
		return fail("mail_reply_failed", "gc mail reply: "+err.Error())
	}
	if jsonOut {
		summary := summarizeMailMessage(reply)
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail reply", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.reply", Action: "reply", ID: reply.ID, Message: &summary, Messages: []mailMessageSummary{summary}, Count: intRef(1)})
	}
	fmt.Fprintf(stdout, "Replied to %s — sent message %s to %s (as %s)\n", id, reply.ID, reply.To, reply.From) //nolint:errcheck // best-effort stdout
	return 0
}

// remoteInboxReader adapts the remote client's inbox list to the
// mailInboxReader seam so the remote inbox renders exactly like a local one:
// it pages through the server's keyset pagination until the mailbox is
// exhausted (a local inbox lists every unread message, so a single capped page
// would silently truncate), and a partial aggregate read — one rig's provider
// failed server-side — is an error, not an authoritative short list (remote
// reads are non-fallbackable, gate G1).
type remoteInboxReader struct{ c *api.Client }

// remoteInboxMaxPages bounds the paging loop so a server that keeps returning
// a cursor cannot spin the client forever; at the server's default page size
// this is far beyond any real unread mailbox.
const remoteInboxMaxPages = 200

func (r remoteInboxReader) Inbox(recipient string) ([]mail.Message, error) {
	var out []mail.Message
	cursor := ""
	for page := 0; page < remoteInboxMaxPages; page++ {
		cr, err := r.c.ListMailInboxPage(recipient, "", cursor, 0)
		if err != nil {
			return nil, err
		}
		if cr.Body.Partial {
			return nil, fmt.Errorf("remote inbox read is partial (%s); not rendering an incomplete mailbox", strings.Join(cr.Body.PartialErrors, "; "))
		}
		out = append(out, cr.Body.Items...)
		if cr.Body.NextCursor == "" || len(cr.Body.Items) == 0 {
			return out, nil
		}
		cursor = cr.Body.NextCursor
	}
	return nil, fmt.Errorf("remote inbox read exceeded %d pages; refusing to render a possibly incomplete mailbox", remoteInboxMaxPages)
}

// cmdMailInboxRemote is the remote arm of `gc mail inbox [recipient]`. With no
// recipient it reads the mailbox of this client's own remote sender identity
// ("<local city>/<identity>") — where a far-side `gc mail reply` lands.
func cmdMailInboxRemote(c *api.Client, target *remoteTarget, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	recipient := ""
	if len(args) > 0 {
		recipient = strings.TrimSpace(args[0])
	}
	if recipient == "" {
		recipient = remoteMailSender("")
	}
	if !jsonOut {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}
	return doMailInboxTargetWithJSON(remoteInboxReader{c}, resolvedMailTarget{display: recipient, recipients: []string{recipient}}, jsonOut, stdout, stderr)
}
