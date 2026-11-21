package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// extmsgEmitEvent builds an event emitter closure for extmsg handlers.
// The payload parameter is the events.Payload sealed interface so only
// types registered in the central event-payload registry are accepted
// — ad-hoc map[string]any emissions are a compile-time error
// (Principle 7). The json.Marshal below is the internal bus
// serialization permitted by the Principle 4 edge case; the SSE
// projection decodes these bytes back into the typed Go variant via
// events.DecodePayload before emitting on the wire.
func (s *Server) extmsgEmitEvent() func(string, string, events.Payload) {
	ep := s.state.EventProvider()
	if ep == nil {
		return func(string, string, events.Payload) {}
	}
	return func(eventType, subject string, payload events.Payload) {
		b, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "extmsg: marshal event payload: %v\n", err)
			return
		}
		ep.Record(events.Event{
			Type:    eventType,
			Subject: subject,
			Payload: b,
		})
	}
}

// extmsgDefaultAgentForConversation builds the InboundDeps default-route
// resolver from [[extmsg.default_route]] config: it maps an unrouted
// inbound conversation to the qualified identity of the configured agent.
// A route naming an agent that does not resolve to a configured named
// session is logged and skipped — the message stays unrouted rather than
// failing the inbound on a config error.
func (s *Server) extmsgDefaultAgentForConversation() func(extmsg.ConversationRef) string {
	cfg := s.state.Config()
	if cfg == nil || len(cfg.ExtMsg.DefaultRoutes) == 0 {
		return nil
	}
	return func(ref extmsg.ConversationRef) string {
		agent := cfg.ExtMsgDefaultRouteAgent(ref.Provider, ref.AccountID)
		if agent == "" {
			return ""
		}
		spec, ok, err := s.findNamedSessionSpecForTarget(agent)
		if err != nil || !ok {
			log.Printf("extmsg: default-route agent %q for %s/%s does not resolve to a configured named session (err=%v)", agent, ref.Provider, ref.AccountID, err)
			return ""
		}
		return spec.Identity
	}
}

// extmsgResolveSessionSelector builds the OutboundDeps session-selector
// resolver: it maps a selector — a configured agent identity, session name,
// alias, or concrete session bead ID — to the concrete ID of a live session,
// without materializing one. HandleOutbound uses it to authorize publishes on
// agent-bound conversations.
func (s *Server) extmsgResolveSessionSelector() func(ctx context.Context, selector string) (string, error) {
	store := s.state.Stores().Sessions()
	if store == (beads.SessionStore{}) {
		return nil
	}
	return func(ctx context.Context, selector string) (string, error) {
		return s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
	}
}

// extmsgResolveAddressedSession builds the InboundDeps addressed-session
// resolver: it resolves a selector as extmsgResolveSessionSelector does,
// without materializing a session. A selector that names no session, or names
// a closed one (an exact session ID still names its session once closed),
// addresses no one. One that names several sessions, or conflicts with a
// configured named session, wraps extmsg.ErrInvalidInput: a retry of the
// inbound cannot change that answer, so the adapter must drop the message
// rather than hold its stream behind it.
func (s *Server) extmsgResolveAddressedSession() func(ctx context.Context, selector string) (string, error) {
	store := s.state.Stores().Sessions()
	if store == (beads.SessionStore{}) {
		return nil
	}
	sessions := session.NewStore(store)
	return func(ctx context.Context, selector string) (string, error) {
		sessionID, err := s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
		switch {
		case errors.Is(err, session.ErrSessionNotFound):
			return "", nil
		case errors.Is(err, session.ErrAmbiguous), errors.Is(err, errConfiguredNamedSessionConflict):
			return "", fmt.Errorf("%w: %w", extmsg.ErrInvalidInput, err)
		case err != nil:
			return "", err
		}
		_, closed, err := sessions.GetState(sessionID)
		switch {
		case errors.Is(err, session.ErrSessionNotFound), errors.Is(err, beads.ErrNotFound), err == nil && closed:
			return "", nil
		case err != nil:
			return "", fmt.Errorf("reading addressed session %s: %w", sessionID, err)
		}
		return sessionID, nil
	}
}

func extmsgHandleLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if idx := strings.LastIndex(value, "/"); idx >= 0 && idx+1 < len(value) {
		return value[idx+1:]
	}
	return value
}

func (s *Server) extmsgSessionHandleForSelector(selector string) string {
	sessions := s.state.Stores().Sessions()
	if sessions == (beads.SessionStore{}) {
		return extmsgHandleLabel(selector)
	}
	resolvedID, err := session.ResolveSessionIDAllowClosed(sessions, selector)
	if err != nil {
		return extmsgHandleLabel(selector)
	}
	return s.extmsgSessionHandleForResolvedID(resolvedID, selector)
}

func (s *Server) extmsgSessionHandleForResolvedID(resolvedID, fallback string) string {
	store := s.state.Stores().Sessions()
	if store == (beads.SessionStore{}) {
		return extmsgHandleLabel(fallback)
	}
	source, ok := session.NewStore(store).ExtmsgHandleSource(resolvedID)
	if !ok || source == "" {
		return extmsgHandleLabel(fallback)
	}
	return extmsgHandleLabel(source)
}

// extmsgNotification is one message a notice fan-out announces to a
// conversation's readers.
type extmsgNotification struct {
	Conversation extmsg.ConversationRef
	ActorDisplay string
	ActorKind    string
	Text         string
	// ExcludeSelector names the session that published the message, which is
	// never told of its own message.
	ExcludeSelector string
	// ExplicitTarget is the handle an inbound was addressed to
	// (ExternalInboundMessage.ExplicitTarget), which the notices' self-silence
	// line names. Outbound reply broadcasts and self-update notifications leave
	// it empty because they are not addressed to a specific agent.
	ExplicitTarget string
	// AddressedSessionID is the session an inbound's adapter addressed it to
	// (InboundResult.AddressedSessionID). It is notified once, whether or not
	// it is a member, and a membership row that resolves to it gets no second
	// notice.
	AddressedSessionID string
}

// extmsgNotifyMembers sends a peer-publication reminder to transcript members
// via the session message API. Transcript members are the conversation's
// readership; session resolution materializes or wakes a member's named
// session on first receive. The one reader outside the membership is an
// inbound's addressed session (n.AddressedSessionID), which inbound routing
// resolved without materializing anything.
//
// When n.ExplicitTarget is non-empty, every notice except the addressee's
// carries a self-silence line so peers can stay quiet on off-target messages
// (see #2484). The addressee is the addressed session or, in a grouped room,
// the participant whose handle the explicit target names.
func (s *Server) extmsgNotifyMembers(ctx context.Context, n extmsgNotification) {
	svc := s.state.ExtMsgServices()
	// Sessions class, and a WRITE path: the member fan-out below materializes a
	// configured named session that has no live bead yet, so through the work
	// store on a relocated city every cold-wake mints a stranded session bead.
	store := s.state.Stores().Sessions()
	if svc == nil || store == (beads.SessionStore{}) {
		return
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "extmsg-notify"}
	addresseeID := extmsgNotifyAddresseeSessionID(ctx, svc, n)
	members, err := svc.Transcript.ListMemberships(ctx, caller, n.Conversation)
	if err != nil {
		log.Printf("extmsg: ListMemberships failed for %s/%s: %v", n.Conversation.Provider, n.Conversation.ConversationID, err)
		return
	}
	sender := s.extmsgNotifySenderOf(ctx, store, n.ExcludeSelector)

	notifyResolved := func(sessionSelector, resolvedID string) {
		handle := s.extmsgSessionHandleForResolvedID(resolvedID, sessionSelector)
		nudge := s.extmsgNotice(extmsgNotifyReminder{
			Provider:                n.Conversation.Provider,
			ConversationID:          n.Conversation.ConversationID,
			ActorDisplay:            n.ActorDisplay,
			ActorKind:               n.ActorKind,
			Text:                    n.Text,
			RecipientSelector:       sessionSelector,
			RecipientSessionID:      resolvedID,
			Handle:                  handle,
			ExplicitTarget:          n.ExplicitTarget,
			ExplicitTargetSessionID: addresseeID,
		})
		if err := s.sendBackgroundMessageToSession(ctx, store, resolvedID, nudge); err != nil {
			log.Printf("extmsg: notify %s failed: %v", sessionSelector, err)
		}
	}

	var wg sync.WaitGroup
	if addressed := n.AddressedSessionID; addressed != "" && !sender.excludes(addressed, addressed) {
		wg.Go(func() { notifyResolved(addressed, addressed) })
	}
	for _, m := range members {
		wg.Go(func() {
			resolvedID, ok := s.extmsgMemberSessionID(ctx, store, n.Conversation, m.SessionID, sender)
			if ok && resolvedID != n.AddressedSessionID {
				notifyResolved(m.SessionID, resolvedID)
			}
		})
	}
	wg.Wait()
}

// extmsgNotifyAddresseeSessionID returns the session n is addressed to, whose
// notice carries no self-silence line: n's addressed session, else the group
// participant whose handle n.ExplicitTarget names.
func extmsgNotifyAddresseeSessionID(ctx context.Context, svc *extmsg.Services, n extmsgNotification) string {
	if n.AddressedSessionID != "" {
		return n.AddressedSessionID
	}
	return extmsgNotifyExplicitTargetSessionID(ctx, svc, n.Conversation, n.ExplicitTarget)
}

func extmsgNotifyExplicitTargetSessionID(ctx context.Context, svc *extmsg.Services, conv extmsg.ConversationRef, explicitTarget string) string {
	if strings.TrimSpace(explicitTarget) == "" || svc == nil || svc.Groups == nil {
		return ""
	}
	route, err := svc.Groups.ResolveInbound(ctx, extmsg.ExternalInboundMessage{
		Conversation:   conv,
		ExplicitTarget: explicitTarget,
	})
	if err != nil {
		log.Printf("extmsg: resolve explicit target %q for %s/%s failed: %v", explicitTarget, conv.Provider, conv.ConversationID, err)
		return ""
	}
	if route == nil || route.Match != extmsg.GroupRouteExplicitTarget {
		return ""
	}
	return strings.TrimSpace(route.TargetSessionID)
}

// extmsgNotifySender is the session that published the message a notice
// fan-out announces, which is never told of its own message: its selector,
// normalized, and the session that selector resolves to, when it does.
type extmsgNotifySender struct {
	selector  string
	sessionID string
}

// excludes reports whether a recipient, named by selector or resolved to
// resolvedID, is the sender. An empty resolvedID checks the selector alone.
func (snd extmsgNotifySender) excludes(selector, resolvedID string) bool {
	return (snd.selector != "" && apiNormalizeSessionTarget(selector) == snd.selector) ||
		(snd.sessionID != "" && resolvedID == snd.sessionID)
}

// extmsgNotifySenderOf resolves the sender excludeSelector names, without
// materializing a session. A selector that does not resolve still excludes the
// membership rows it names directly.
func (s *Server) extmsgNotifySenderOf(ctx context.Context, store beads.SessionStore, excludeSelector string) extmsgNotifySender {
	sender := extmsgNotifySender{selector: apiNormalizeSessionTarget(excludeSelector)}
	selector := strings.TrimSpace(excludeSelector)
	if selector == "" {
		return sender
	}
	resolvedID, err := s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
	if err != nil {
		log.Printf("extmsg: resolve sender %s failed: %v", selector, err)
		return sender
	}
	sender.sessionID = resolvedID
	return sender
}

// extmsgMemberSessionID resolves a membership row's session selector to the
// session its notice goes to, materializing a configured named session that
// has no live bead yet. It reports false when the row gets no notice: it names
// the sender, or its selector does not resolve.
func (s *Server) extmsgMemberSessionID(ctx context.Context, store beads.SessionStore, conv extmsg.ConversationRef, selector string, sender extmsgNotifySender) (string, bool) {
	if sender.excludes(selector, "") {
		return "", false
	}
	preexistingID, preErr := s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
	if preErr == nil && preexistingID != "" {
		return preexistingID, !sender.excludes(selector, preexistingID)
	}
	resolvedID, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, selector)
	if err != nil {
		log.Printf("extmsg: resolve session %s failed: %v", selector, err)
		return "", false
	}
	if preErr != nil {
		log.Printf("extmsg: materialized session %s as %s for conversation %s/%s", selector, resolvedID, conv.Provider, conv.ConversationID)
	}
	return resolvedID, !sender.excludes(selector, resolvedID)
}

// extmsgNotifyInboundMembers announces an inbound to the conversation's
// members and, when its adapter addressed it to a session, to that session
// (addressedSessionID, the inbound's InboundResult.AddressedSessionID).
func (s *Server) extmsgNotifyInboundMembers(ctx context.Context, msg extmsg.ExternalInboundMessage, addressedSessionID string) {
	actorKind := "agent"
	if !msg.Actor.IsBot {
		actorKind = "human"
	}
	s.extmsgNotifyMembers(ctx, extmsgNotification{
		Conversation:       msg.Conversation,
		ActorDisplay:       msg.Actor.DisplayName,
		ActorKind:          actorKind,
		Text:               msg.Text,
		ExplicitTarget:     msg.ExplicitTarget,
		AddressedSessionID: addressedSessionID,
	})
}

// titleCaseProvider uppercases the first ASCII byte of a provider name.
// Used to avoid a golang.org/x/text/cases dependency just for one
// capitalization in the inbound nudge — provider names are always
// short lowercase ASCII identifiers (slack, discord, ...).
func titleCaseProvider(name string) string {
	if name == "" {
		return ""
	}
	first := name[0]
	if first >= 'a' && first <= 'z' {
		return string(first-'a'+'A') + name[1:]
	}
	return name
}

// extmsgNotifyReminder collects the inputs the inbound-message
// <system-reminder> block is constructed from. Externally-supplied fields
// (ActorDisplay, Text, ExplicitTarget) are sanitized via
// extmsg.SanitizeForSystemReminder inside formatExtmsgNotifyReminder before
// interpolation; callers should not pre-sanitize.
//
// ExplicitTarget carries the provider-resolved address-by-handle target (set
// when an inbound was addressed to a specific agent via @handle: prefix or a
// subteam mention). When non-empty and not routed to the receiving session,
// formatExtmsgNotifyReminder emits a "do not reply" discriminator line so
// peer sessions can self-silence on off-target messages. See
// gastownhall/gascity#2484. ExplicitTargetSessionID is the session the inbound
// is addressed to, when known: its addressed session, or the group participant
// ExplicitTarget names.
type extmsgNotifyReminder struct {
	Provider                string
	ConversationID          string
	ActorDisplay            string
	ActorKind               string
	Text                    string
	RecipientSelector       string
	RecipientSessionID      string
	Handle                  string
	ExplicitTarget          string
	ExplicitTargetSessionID string
}

// formatExtmsgNotifyReminder builds the inbound-message reminder body.
// Attacker-controllable fields (ActorDisplay, Text, ExplicitTarget) are
// stripped of literal <system-reminder> open/close sequences before being
// interpolated into the reminder block. Without this guard, an external
// sender can inject the sequence and break out of the legitimate reminder,
// injecting attacker-controlled instructions into the receiving agent's
// prompt. See gastownhall/gascity#2195.
//
// When ExplicitTarget is non-empty and does not target the receiving session,
// a discriminator line is appended so peer sessions can self-silence on
// messages addressed to a different agent. See gastownhall/gascity#2484.
func formatExtmsgNotifyReminder(r extmsgNotifyReminder) string {
	providerCLI := strings.ToLower(r.Provider)
	providerDisplay := titleCaseProvider(providerCLI)
	safeActor := extmsg.SanitizeForSystemReminder(r.ActorDisplay)
	safeText := extmsg.SanitizeForSystemReminder(r.Text)

	var b strings.Builder
	fmt.Fprintf(&b,
		"<system-reminder>\nNew message in shared conversation %s/%s:\n\n"+
			"- %s (%s): %s\n\n",
		r.Provider, r.ConversationID,
		safeActor, r.ActorKind, safeText,
	)
	if target := strings.TrimSpace(r.ExplicitTarget); target != "" && !extmsgNotifyReminderTargetsRecipient(r, target) {
		safeTarget := extmsg.SanitizeForSystemReminder(target)
		fmt.Fprintf(&b,
			"Addressed to: @%s — if that is not you, do not reply.\n\n",
			safeTarget,
		)
	}
	fmt.Fprintf(&b,
		"To reply in %s, write your response to a file and run:\n"+
			"  gc %s reply-current --conversation-id %s --body-file <path>\n"+
			"Prefix your reply with your agent handle in bold (e.g., **%s:** your message).\n"+
			"</system-reminder>",
		providerDisplay,
		providerCLI, r.ConversationID,
		r.Handle,
	)
	return b.String()
}

// extmsgNotifyReminderTargetsRecipient reports whether the reminder's
// recipient is the session its inbound is addressed to, whose notice carries
// no discriminator line. A known addressee (ExplicitTargetSessionID) decides
// by session. Otherwise target is compared with the recipient's handle, its
// session ID, and the selector it was reached by, so a member named by its own
// ID is not told the message is for someone else.
func extmsgNotifyReminderTargetsRecipient(r extmsgNotifyReminder, target string) bool {
	recipientID := strings.TrimSpace(r.RecipientSessionID)
	if targetSessionID := strings.TrimSpace(r.ExplicitTargetSessionID); targetSessionID != "" {
		return recipientID == targetSessionID ||
			apiNormalizeSessionTarget(r.RecipientSelector) == apiNormalizeSessionTarget(targetSessionID)
	}
	return strings.EqualFold(target, strings.TrimSpace(r.Handle)) ||
		target == recipientID ||
		apiNormalizeSessionTarget(target) == apiNormalizeSessionTarget(r.RecipientSelector)
}
