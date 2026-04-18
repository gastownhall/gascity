package api

import (
	"github.com/gastownhall/gascity/internal/mail"
)

// Event-bus payload types emitted by API handlers. Every API emitter
// takes one of these typed structs rather than map[string]any so the
// wire-facing shape of each event type is visible in Go source and
// checkable by the compiler (Principle 7). The event bus itself stores
// payloads as []byte for domain-agnostic transport (Principle 4 edge
// case); the marshal step that converts the typed struct to []byte
// lives inside the emitter, not in the call site.
//
// extmsg event payloads (inbound/outbound plus Bound/Unbound/
// GroupCreated/Adapter*) live in internal/extmsg because they flow
// through a callback shared by the API and the extmsg package; the
// sealed extmsg.EventPayload interface gates that callback.

// MailEventPayload is the shape of every mail.* event payload
// (MailSent, MailMarkedRead, MailMarkedUnread, MailArchived, MailReplied,
// MailDeleted). Message is nil for mark/archive/delete events; present
// for send/reply events.
type MailEventPayload struct {
	Rig     string        `json:"rig"`
	Message *mail.Message `json:"message,omitempty"`
}
