package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"

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
	store := s.state.CityBeadStore()
	return func(ref extmsg.ConversationRef) string {
		agent := cfg.ExtMsgDefaultRouteAgent(ref.Provider, ref.AccountID)
		if agent == "" {
			return ""
		}
		spec, ok, err := s.findNamedSessionSpecForTarget(store, agent)
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
	store := s.state.CityBeadStore()
	if store == nil {
		return nil
	}
	return func(ctx context.Context, selector string) (string, error) {
		return s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
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
	store := s.state.CityBeadStore()
	if store == nil {
		return extmsgHandleLabel(selector)
	}
	resolvedID, err := session.ResolveSessionIDAllowClosed(store, selector)
	if err != nil {
		return extmsgHandleLabel(selector)
	}
	return s.extmsgSessionHandleForResolvedID(resolvedID, selector)
}

func (s *Server) extmsgSessionHandleForResolvedID(resolvedID, fallback string) string {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return extmsgHandleLabel(fallback)
	}
	source, ok := session.NewStore(store).ExtmsgHandleSource(resolvedID)
	if !ok || source == "" {
		return extmsgHandleLabel(fallback)
	}
	return extmsgHandleLabel(source)
}

// extmsgNotifyBroadcast carries one conversation event through the member
// fan-out in extmsgNotifyMembers.
//
// ExplicitTarget, when non-empty, carries the address-by-handle target so
// peer members can self-silence on off-target messages (see #2484). Outbound
// reply broadcasts and self-update notifications pass "" because they are
// not addressed to a specific agent.
//
// ProviderMessageID and ReplyToMessageID surface the provider-side message
// id and its thread id so the injected reminder can tell agents where a
// threaded reply must go. On the inbound path they come from the inbound
// message; on the outbound broadcast path from the publish receipt and the
// publish request's reply target.
type extmsgNotifyBroadcast struct {
	Conversation      extmsg.ConversationRef
	ActorDisplay      string
	ActorKind         string
	Text              string
	ExcludeSelector   string
	ExplicitTarget    string
	ProviderMessageID string
	ReplyToMessageID  string
}

// extmsgNotifyMembers sends a peer-publication reminder to transcript members
// via the session message API. This treats membership as the routing truth and
// lets session resolution materialize or wake named sessions on first receive.
func (s *Server) extmsgNotifyMembers(ctx context.Context, b extmsgNotifyBroadcast) {
	svc := s.state.ExtMsgServices()
	store := s.state.CityBeadStore()
	if svc == nil || store == nil {
		return
	}
	conv := b.Conversation
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "extmsg-notify"}
	explicitTargetSessionID := extmsgNotifyExplicitTargetSessionID(ctx, svc, conv, b.ExplicitTarget)
	replyInstructions := s.extmsgReplyInstructionsForConversation(conv)
	members, err := svc.Transcript.ListMemberships(ctx, caller, conv)
	if err != nil {
		log.Printf("extmsg: ListMemberships failed for %s/%s: %v", conv.Provider, conv.ConversationID, err)
		return
	}

	excludedResolvedID := ""
	excludedSelector := apiNormalizeSessionTarget(b.ExcludeSelector)
	if selector := strings.TrimSpace(b.ExcludeSelector); selector != "" {
		resolvedID, err := s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
		if err != nil {
			log.Printf("extmsg: resolve sender %s failed: %v", selector, err)
		} else {
			excludedResolvedID = resolvedID
		}
	}

	notifyResolved := func(sessionSelector, resolvedID string) {
		handle := s.extmsgSessionHandleForResolvedID(resolvedID, sessionSelector)
		nudge := formatExtmsgNotifyReminder(extmsgNotifyReminder{
			Provider:                conv.Provider,
			ConversationID:          conv.ConversationID,
			ActorDisplay:            b.ActorDisplay,
			ActorKind:               b.ActorKind,
			Text:                    b.Text,
			RecipientSelector:       sessionSelector,
			RecipientSessionID:      resolvedID,
			Handle:                  handle,
			ExplicitTarget:          b.ExplicitTarget,
			ExplicitTargetSessionID: explicitTargetSessionID,
			ProviderMessageID:       b.ProviderMessageID,
			ReplyToMessageID:        b.ReplyToMessageID,
			ReplyInstructions:       replyInstructions,
		})
		if err := s.sendBackgroundMessageToSession(ctx, store, resolvedID, nudge); err != nil {
			log.Printf("extmsg: notify %s failed: %v", sessionSelector, err)
		}
	}

	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(sessionSelector string) {
			defer wg.Done()
			if excludedSelector != "" && apiNormalizeSessionTarget(sessionSelector) == excludedSelector {
				return
			}
			preexistingID, preErr := s.resolveSessionTargetIDWithContext(ctx, store, sessionSelector, apiSessionResolveOptions{})
			if preErr == nil && preexistingID != "" {
				if excludedResolvedID != "" && preexistingID == excludedResolvedID {
					return
				}
				notifyResolved(sessionSelector, preexistingID)
				return
			}
			resolvedID, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, sessionSelector)
			if err != nil {
				log.Printf("extmsg: resolve session %s failed: %v", sessionSelector, err)
				return
			}
			if preErr != nil {
				log.Printf("extmsg: materialized session %s as %s for conversation %s/%s", sessionSelector, resolvedID, conv.Provider, conv.ConversationID)
			}
			if excludedResolvedID != "" && resolvedID == excludedResolvedID {
				return
			}
			notifyResolved(sessionSelector, resolvedID)
		}(m.SessionID)
	}
	wg.Wait()
}

// extmsgReplyInstructionsForConversation returns the reply-instruction
// template the conversation's adapter registered, or "" when the registry
// or adapter is missing or the adapter does not provide one — the reminder
// then falls back to the generic reply-current text.
func (s *Server) extmsgReplyInstructionsForConversation(conv extmsg.ConversationRef) string {
	reg := s.state.AdapterRegistry()
	if reg == nil {
		return ""
	}
	adapter := reg.LookupByConversation(conv)
	if adapter == nil {
		return ""
	}
	provider, ok := adapter.(extmsg.ReplyInstructionsProvider)
	if !ok {
		return ""
	}
	return provider.ReplyInstructions()
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

func (s *Server) extmsgNotifyInboundMembers(ctx context.Context, msg extmsg.ExternalInboundMessage) {
	actorKind := "agent"
	if !msg.Actor.IsBot {
		actorKind = "human"
	}
	s.extmsgNotifyMembers(ctx, extmsgNotifyBroadcast{
		Conversation:      msg.Conversation,
		ActorDisplay:      msg.Actor.DisplayName,
		ActorKind:         actorKind,
		Text:              msg.Text,
		ExplicitTarget:    msg.ExplicitTarget,
		ProviderMessageID: msg.ProviderMessageID,
		ReplyToMessageID:  msg.ReplyToMessageID,
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
// gastownhall/gascity#2484.
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
	// ProviderMessageID/ReplyToMessageID carry the provider-side message
	// id and thread id (empty when the notifying path has none, e.g. an
	// outbound broadcast with no receipt). They surface threading context
	// in the reminder and feed the {message_ts}/{thread_ts} placeholders.
	ProviderMessageID string
	ReplyToMessageID  string
	// ReplyInstructions is the adapter-registered reply-instruction
	// template (see extmsg.ReplyInstructionsProvider); empty selects the
	// generic reply-current fallback text.
	ReplyInstructions string
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
	if messageID := strings.TrimSpace(r.ProviderMessageID); messageID != "" {
		fmt.Fprintf(&b, "Message id: %s", extmsg.SanitizeForSystemReminder(messageID))
		if threadID := strings.TrimSpace(r.ReplyToMessageID); threadID != "" {
			fmt.Fprintf(&b, " (in thread %s)", extmsg.SanitizeForSystemReminder(threadID))
		}
		b.WriteString("\n\n")
	}
	if target := strings.TrimSpace(r.ExplicitTarget); target != "" && !extmsgNotifyReminderTargetsRecipient(r, target) {
		safeTarget := extmsg.SanitizeForSystemReminder(target)
		fmt.Fprintf(&b,
			"Addressed to: @%s — if that is not you, do not reply.\n\n",
			safeTarget,
		)
	}
	if instructions := renderExtmsgReplyInstructions(r); instructions != "" {
		// Defense-in-depth: the adapter-registered template text is trusted
		// control-plane content, but strip any literal <system-reminder>
		// breakout sequences before embedding it so a compromised or
		// mistaken registration cannot break out of the reminder block. No
		// legitimate template contains these tags, so this is safe.
		instructions = extmsg.SanitizeForSystemReminder(instructions)
		b.WriteString(instructions)
		if !strings.HasSuffix(instructions, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("</system-reminder>")
		return b.String()
	}
	fmt.Fprintf(&b,
		"To reply in %s, write your response to a file and run:\n"+
			"  gc %s reply-current --conversation-id %s --body-file <path>\n"+
			"</system-reminder>",
		providerDisplay,
		providerCLI, r.ConversationID,
	)
	return b.String()
}

var (
	// extmsgOptionalSegmentRE matches [bracketed segments] in an
	// adapter-registered reply-instruction template; a segment is dropped
	// when any placeholder inside it resolves empty.
	extmsgOptionalSegmentRE = regexp.MustCompile(`\[[^\[\]]*\]`)
	// extmsgPlaceholderRE matches {placeholder} tokens.
	extmsgPlaceholderRE = regexp.MustCompile(`\{[a-z_]+\}`)
)

// renderExtmsgReplyInstructions renders the adapter-registered
// reply-instruction template with the reminder's values. The effective
// {thread_ts} is the inbound message's thread id, falling back to the
// message's own id — replying to a top-level message starts its thread.
// Substituted values are attacker-influenced (provider-supplied ids), so
// they are sanitized like every other interpolated reminder field; the
// template itself comes from the local adapter registration.
func renderExtmsgReplyInstructions(r extmsgNotifyReminder) string {
	template := strings.TrimSpace(r.ReplyInstructions)
	if template == "" {
		return ""
	}
	threadTS := strings.TrimSpace(r.ReplyToMessageID)
	if threadTS == "" {
		threadTS = strings.TrimSpace(r.ProviderMessageID)
	}
	values := map[string]string{
		"conversation_id": strings.TrimSpace(r.ConversationID),
		"message_ts":      strings.TrimSpace(r.ProviderMessageID),
		"thread_ts":       threadTS,
		"handle":          strings.TrimSpace(r.Handle),
	}
	rendered := extmsgOptionalSegmentRE.ReplaceAllStringFunc(template, func(segment string) string {
		expanded, allSet := substituteExtmsgPlaceholders(segment[1:len(segment)-1], values)
		if !allSet {
			return ""
		}
		return expanded
	})
	rendered, _ = substituteExtmsgPlaceholders(rendered, values)
	return rendered
}

// substituteExtmsgPlaceholders replaces known {placeholder} tokens with
// their sanitized values, leaving unknown tokens literal. The bool reports
// whether every known placeholder in s resolved non-empty.
func substituteExtmsgPlaceholders(s string, values map[string]string) (string, bool) {
	allSet := true
	out := extmsgPlaceholderRE.ReplaceAllStringFunc(s, func(token string) string {
		value, known := values[token[1:len(token)-1]]
		if !known {
			return token
		}
		if value == "" {
			allSet = false
			return ""
		}
		return extmsg.SanitizeForSystemReminder(value)
	})
	return out, allSet
}

func extmsgNotifyReminderTargetsRecipient(r extmsgNotifyReminder, target string) bool {
	if targetSessionID := strings.TrimSpace(r.ExplicitTargetSessionID); targetSessionID != "" {
		return strings.TrimSpace(r.RecipientSessionID) == targetSessionID ||
			apiNormalizeSessionTarget(r.RecipientSelector) == apiNormalizeSessionTarget(targetSessionID)
	}
	return strings.EqualFold(target, strings.TrimSpace(r.Handle))
}
