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

// extmsgNotifyMembers sends a "check transcript" message to all transcript
// members via the session message API. This ensures delivery regardless of
// session state: sleeping sessions are woken, idle sessions get a new prompt
// turn that triggers the transcript check hook.
func (s *Server) extmsgNotifyMembers(ctx context.Context, conv extmsg.ConversationRef, inboundMsg extmsg.ExternalInboundMessage) {
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

	actorKind := "agent"
	if !inboundMsg.Actor.IsBot {
		actorKind = "human"
	}

	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()
			// Resolve the member's handle from their session bead alias.
			// Membership stores session names (s-et-xxxx); bead IDs drop the "s-" prefix.
			handle := sessionID
			beadID := strings.TrimPrefix(sessionID, "s-")
			if b, err := store.Get(beadID); err == nil {
				if alias := b.Metadata["alias"]; alias != "" {
					if idx := strings.LastIndex(alias, "/"); idx >= 0 {
						handle = alias[idx+1:]
					} else {
						handle = alias
					}
				}
			}
			nudge := fmt.Sprintf("<system-reminder>\nNew message in shared conversation %s/%s:\n\n"+
				"- %s (%s): %s\n\n"+
				"To reply in Discord, write your response to a file and run:\n"+
				"  gc discord reply-current --conversation-id %s --body-file <path>\n"+
				"Prefix your reply with your agent handle in bold (e.g., **%s:** your message).\n"+
				"Run 'gc transcript read --ack' after responding to mark as read.\n"+
				"</system-reminder>",
				conv.Provider, conv.ConversationID,
				inboundMsg.Actor.DisplayName, actorKind, inboundMsg.Text,
				conv.ConversationID,
				handle,
			)
			// Resolve session identifier to bead ID, then send.
			resolvedID, err := session.ResolveSessionID(store, sessionID)
			if err != nil {
				log.Printf("extmsg: resolve session %s failed: %v", sessionID, err)
				return
			}
			if err := s.sendBackgroundMessageToSession(ctx, store, resolvedID, nudge); err != nil {
				log.Printf("extmsg: notify %s failed: %v", sessionID, err)
			}
		}(m.SessionID)
	}
	wg.Wait()
}
