package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	store := s.state.CityBeadStore()
	if store == nil {
		return extmsgHandleLabel(fallback)
	}
	b, err := store.Get(resolvedID)
	if err != nil {
		return extmsgHandleLabel(fallback)
	}
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
		return extmsgHandleLabel(alias)
	}
	if sessionName := strings.TrimSpace(b.Metadata["session_name"]); sessionName != "" {
		return extmsgHandleLabel(sessionName)
	}
	return extmsgHandleLabel(fallback)
}

// extmsgNotifyMembers sends a peer-publication reminder to transcript members
// via the session message API. This treats membership as the routing truth and
// lets session resolution materialize or wake named sessions on first receive.
func (s *Server) extmsgNotifyMembers(
	ctx context.Context,
	conv extmsg.ConversationRef,
	actorDisplayName string,
	actorKind string,
	text string,
	excludeSelector string,
) {
	svc := s.state.ExtMsgServices()
	store := s.state.CityBeadStore()
	if svc == nil || store == nil {
		return
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "extmsg-notify"}
	members, err := svc.Transcript.ListMemberships(ctx, caller, conv)
	if err != nil {
		log.Printf("extmsg: ListMemberships failed for %s/%s: %v", conv.Provider, conv.ConversationID, err)
		return
	}

	excludedResolvedID := ""
	excludedSelector := apiNormalizeSessionTarget(excludeSelector)
	if selector := strings.TrimSpace(excludeSelector); selector != "" {
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
			Provider:       conv.Provider,
			ConversationID: conv.ConversationID,
			ActorDisplay:   actorDisplayName,
			ActorKind:      actorKind,
			Text:           text,
			Handle:         handle,
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

func (s *Server) extmsgNotifyInboundMembers(ctx context.Context, msg extmsg.ExternalInboundMessage) {
	actorKind := "agent"
	if !msg.Actor.IsBot {
		actorKind = "human"
	}
	s.extmsgNotifyMembers(ctx, msg.Conversation, msg.Actor.DisplayName, actorKind, msg.Text, "")
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
// (ActorDisplay, Text) are sanitized via extmsg.SanitizeForSystemReminder
// inside formatExtmsgNotifyReminder before interpolation; callers should
// not pre-sanitize.
type extmsgNotifyReminder struct {
	Provider       string
	ConversationID string
	ActorDisplay   string
	ActorKind      string
	Text           string
	Handle         string
}

// formatExtmsgNotifyReminder builds the inbound-message reminder body.
// Attacker-controllable fields (ActorDisplay, Text) are stripped of literal
// <system-reminder> open/close sequences before being interpolated into
// the reminder block. Without this guard, an external sender can inject
// the sequence and break out of the legitimate reminder, injecting
// attacker-controlled instructions into the receiving agent's prompt.
// See gastownhall/gascity#2195.
func formatExtmsgNotifyReminder(r extmsgNotifyReminder) string {
	providerCLI := strings.ToLower(r.Provider)
	providerDisplay := titleCaseProvider(providerCLI)
	safeActor := extmsg.SanitizeForSystemReminder(r.ActorDisplay)
	safeText := extmsg.SanitizeForSystemReminder(r.Text)
	return fmt.Sprintf(
		"<system-reminder>\nNew message in shared conversation %s/%s:\n\n"+
			"- %s (%s): %s\n\n"+
			"To reply in %s, write your response to a file and run:\n"+
			"  gc %s reply-current --conversation-id %s --body-file <path>\n"+
			"Prefix your reply with your agent handle in bold (e.g., **%s:** your message).\n"+
			"</system-reminder>",
		r.Provider, r.ConversationID,
		safeActor, r.ActorKind, safeText,
		providerDisplay,
		providerCLI, r.ConversationID,
		r.Handle,
	)
}
