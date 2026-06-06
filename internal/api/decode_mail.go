package api

import (
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/mail"
)

// MailCountView is the CLI-facing shape for `gc mail count`. It mirrors the
// partial/total/unread surface of MailCountOutputBody so cmd/gc/ never imports
// genclient directly.
type MailCountView struct {
	Total         int
	Unread        int
	Partial       bool
	PartialErrors []string
}

// MailListView is the CLI-facing shape for `gc mail check`. It mirrors the
// mail-list response body so callers can distinguish authoritative empty
// mailboxes from partial aggregate reads.
type MailListView struct {
	Items         []mail.Message
	Total         int
	Partial       bool
	PartialErrors []string
}

// mailMessageFromGen translates one genclient.Message into mail.Message.
// Optional pointer fields are dereferenced safely.
func mailMessageFromGen(g genclient.Message) mail.Message {
	out := mail.Message{
		ID:        g.Id,
		From:      g.From,
		To:        g.To,
		Subject:   g.Subject,
		Body:      g.Body,
		CreatedAt: g.CreatedAt,
		Read:      g.Read,
	}
	if g.ThreadId != nil {
		out.ThreadID = *g.ThreadId
	}
	if g.ReplyTo != nil {
		out.ReplyTo = *g.ReplyTo
	}
	if g.Priority != nil {
		out.Priority = int(*g.Priority)
	}
	if g.Cc != nil {
		out.CC = append([]string(nil), *g.Cc...)
	}
	if g.Rig != nil {
		out.Rig = *g.Rig
	}
	return out
}

// mailMessagesFromGenList translates the genclient list body into
// []mail.Message. Returns an empty slice (never nil) when the body is missing
// or holds no items so callers can uniformly format the empty case.
func mailMessagesFromGenList(body *genclient.MailListBody) []mail.Message {
	return mailListFromGen(body).Items
}

// mailListFromGen translates the genclient list body into a MailListView.
// A nil body decodes to an empty, non-nil Items slice.
func mailListFromGen(body *genclient.MailListBody) MailListView {
	out := MailListView{Items: []mail.Message{}}
	if body == nil {
		return out
	}
	out.Total = int(body.Total)
	if body.Partial != nil {
		out.Partial = *body.Partial
	}
	if body.PartialErrors != nil {
		out.PartialErrors = append([]string(nil), *body.PartialErrors...)
	}
	if body.Items == nil {
		return out
	}
	items := *body.Items
	out.Items = make([]mail.Message, 0, len(items))
	for _, item := range items {
		out.Items = append(out.Items, mailMessageFromGen(item))
	}
	return out
}

// mailCountFromGen translates the genclient count body into a MailCountView.
// A nil body decodes to the zero value (all counts 0, partial=false).
func mailCountFromGen(body *genclient.MailCountOutputBody) MailCountView {
	if body == nil {
		return MailCountView{}
	}
	out := MailCountView{
		Total:  int(body.Total),
		Unread: int(body.Unread),
	}
	if body.Partial != nil {
		out.Partial = *body.Partial
	}
	if body.PartialErrors != nil {
		out.PartialErrors = append([]string(nil), *body.PartialErrors...)
	}
	return out
}
