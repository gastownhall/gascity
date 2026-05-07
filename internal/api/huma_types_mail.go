package api

import (
	"github.com/gastownhall/gascity/internal/mail"
)

// Per-domain Huma input/output types for the mail handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_mail.go.

// MailListBody is the response body for mail list and thread endpoints.
// Matches the JSON shape of ListBody[mail.Message] so the wire is
// unchanged; the dedicated Go type gives the spec a mail-specific schema
// name.
//
// Partial/PartialErrors signal that the aggregation swept over multiple
// rig providers and at least one of them failed. Callers then know the
// list is not authoritative without needing to re-issue the request. This
// mirrors ListBody's semantics so the all-rigs sweep fails open (partial
// list + errors) rather than fails closed (503 for any single provider).
type MailListBody struct {
	Items         []mail.Message `json:"items" doc:"The list of messages."`
	Total         int            `json:"total" doc:"Total number of messages matching the query."`
	NextCursor    string         `json:"next_cursor,omitempty" doc:"Cursor for the next page of results."`
	Partial       bool           `json:"partial,omitempty" doc:"True when one or more rig providers failed and the list is not authoritative."`
	PartialErrors []string       `json:"partial_errors,omitempty" doc:"Per-provider errors when partial is true."`
}

// MailListOutput is the response envelope for mail list and thread endpoints.
type MailListOutput struct {
	Index uint64 `header:"X-GC-Index" doc:"Latest event sequence number."`
	Body  MailListBody
}

// --- Mail types ---

// MailListInput is the Huma input for GET /v0/city/{cityName}/mail.
type MailListInput struct {
	CityScope
	BlockingParam
	PaginationParam
	Agent  string `query:"agent" required:"false" doc:"Filter by agent name."`
	Status string `query:"status" required:"false" doc:"Filter by status (unread, all)."`
	Rig    string `query:"rig" required:"false" doc:"Filter by rig name."`
}

// MailGetInput is the Huma input for GET /v0/city/{cityName}/mail/{id}.
type MailGetInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint for O(1) lookup."`
}

// MailSendInput is the Huma input for POST /v0/city/{cityName}/mail.
type MailSendInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
		Rig     string `json:"rig,omitempty" doc:"Rig name."`
		From    string `json:"from,omitempty" doc:"Sender name."`
		To      string `json:"to" doc:"Recipient name." minLength:"1"`
		Subject string `json:"subject" doc:"Message subject." minLength:"1"`
		Body    string `json:"body,omitempty" doc:"Message body."`
	}
}

// MailReadInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/read.
type MailReadInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailMarkUnreadInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/mark-unread.
type MailMarkUnreadInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailArchiveInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/archive.
type MailArchiveInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailReplyInput is the Huma input for POST /v0/city/{cityName}/mail/{id}/reply.
type MailReplyInput struct {
	CityScope
	ID   string `path:"id" doc:"Message ID."`
	Rig  string `query:"rig" required:"false" doc:"Rig hint."`
	Body struct {
		From    string `json:"from,omitempty" doc:"Sender name."`
		Subject string `json:"subject,omitempty" doc:"Reply subject."`
		Body    string `json:"body,omitempty" doc:"Reply body."`
	}
}

// MailDeleteInput is the Huma input for DELETE /v0/city/{cityName}/mail/{id}.
type MailDeleteInput struct {
	CityScope
	ID  string `path:"id" doc:"Message ID."`
	Rig string `query:"rig" required:"false" doc:"Rig hint."`
}

// MailThreadInput is the Huma input for GET /v0/city/{cityName}/mail/thread/{id}.
type MailThreadInput struct {
	CityScope
	ID  string `path:"id" doc:"Thread ID, or any message ID in the thread."`
	Rig string `query:"rig" required:"false" doc:"Filter by rig."`
}

// MailCountInput is the Huma input for GET /v0/city/{cityName}/mail/count.
type MailCountInput struct {
	CityScope
	Agent string `query:"agent" required:"false" doc:"Filter by agent name."`
	Rig   string `query:"rig" required:"false" doc:"Filter by rig name."`
}

// MailCountOutput is the response body for GET /v0/mail/count.
// Partial/PartialErrors mirror MailListBody: when one rig provider
// fails but others succeed, we return the partial counts and flag
// the shortfall rather than returning 500 and losing the count
// entirely.
type MailCountOutput struct {
	Body struct {
		Total         int      `json:"total" doc:"Total message count."`
		Unread        int      `json:"unread" doc:"Unread message count."`
		Partial       bool     `json:"partial,omitempty" doc:"True when one or more rig providers failed and the counts are not authoritative."`
		PartialErrors []string `json:"partial_errors,omitempty" doc:"Per-provider errors when partial is true."`
	}
}
